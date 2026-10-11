package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/metrics"
	"github.com/supavise/supavise/internal/registry"
)

const metricsRef = "abcdefghijklmnopqrst"

var metricsNow = time.Date(2026, 10, 8, 11, 0, 30, 0, time.UTC)

type probeFunc func(context.Context, metrics.Target) (metrics.Probe, error)

func (f probeFunc) Probe(ctx context.Context, t metrics.Target) (metrics.Probe, error) {
	return f(ctx, t)
}

// follower and leader are the memberships of the two nodes of a cluster.
func memberships() (follower, leader cluster.Membership) {
	n1 := registry.Node{ID: "n1", Name: "primary", State: registry.NodeActive}
	n2 := registry.Node{ID: "n2", Name: "second", State: registry.NodeActive}
	snap := func(self registry.Node, role cluster.Role) cluster.Snapshot {
		return cluster.Snapshot{Self: self, Nodes: []registry.Node{n1, n2}, Leader: "n1", Epoch: 1, Role: role}
	}
	return cluster.NewStatic(snap(n2, cluster.RoleFollower)), cluster.NewStatic(snap(n1, cluster.RoleLeader))
}

// newMetricsNode is a collector over a registry with one project homed on node home, whose Postgres
// has a (bare) control group, the way a node's collector sees it.
func newMetricsNode(t *testing.T, home string, local bool) (*metrics.Service, registry.Registry) {
	t.Helper()
	reg := registry.NewMemory()
	if home != registry.FounderNodeID {
		n := &registry.Node{Name: "second", Region: "eu-west-1", PublicHost: "eu.pooler.example.test", State: registry.NodeActive}
		if err := reg.CreateNode(context.Background(), n); err != nil || n.ID != home {
			t.Fatalf("node %q: %v", n.ID, err)
		}
	}
	p := &registry.Project{Ref: metricsRef, Name: "demo", Status: registry.StatusActiveHealthy, NodeID: home,
		Limits: config.Limits{MemoryMax: "1G", CPUQuota: "100%"}}
	if err := reg.CreateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	cg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cg, config.Slice, config.UnitName(config.SvcPostgres, metricsRef)), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	svc := metrics.New(metrics.Options{
		Cfg: cfg, Projects: reg, Cgroup: metrics.Cgroup{Root: cg}, Proc: metrics.Proc{Root: t.TempDir()},
		Local: func(*registry.Project) bool { return local }, Now: func() time.Time { return metricsNow },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Prober: probeFunc(func(context.Context, metrics.Target) (metrics.Probe, error) {
			return metrics.Probe{Backends: 7, MaxConnections: 60, DBSize: 1 << 20}, nil
		}),
	})
	svc.Tick(context.Background())
	return svc, reg
}

// A node that is not the leader tells the leader the newest sample of its projects, with the values
// that could not be measured as null; the leader itself adds nothing.
func TestMetricsContributionOfAFollower(t *testing.T) {
	svc, _ := newMetricsNode(t, "n2", true)
	follower, leader := memberships()
	got := metricsContribution(svc, follower)(context.Background())
	if len(got) != 1 || got[0].Ref != metricsRef || got[0].Name != "demo" || !got[0].At.Equal(metricsNow.Truncate(time.Minute)) || len(got[0].V) != int(metrics.NumFields) {
		t.Fatalf("%+v", got)
	}
	if v := got[0].V[metrics.Backends]; v == nil || *v != 7 {
		t.Fatalf("backends = %v", v)
	}
	if got[0].V[metrics.CPUBusy] != nil {
		t.Fatal("a rate with nothing to subtract from is not null")
	}
	b, err := json.Marshal(peerapi.Report{Node: "n2", Metrics: got})
	if err != nil {
		t.Fatalf("a report with unknown values cannot be encoded: %v", err)
	}
	var back peerapi.Report
	if err := json.Unmarshal(b, &back); err != nil || len(back.Metrics) != 1 {
		t.Fatalf("%v %s", err, b)
	}
	if out := metricsContribution(svc, leader)(context.Background()); out != nil {
		t.Fatalf("the leader reports its own samples to itself: %+v", out)
	}
}

func TestMetricsWireRoundTrip(t *testing.T) {
	s := metrics.NewSample(metricsNow)
	s.Set(metrics.CPUBusy, 12.5)
	s.Set(metrics.MemLimit, 1<<30)
	back := metricsFromWire(metricsToWire(metrics.Reported{Ref: "x", Sample: s}))
	if !back.At.Equal(s.At) {
		t.Fatalf("at %v", back.At)
	}
	for f := metrics.Field(0); f < metrics.NumFields; f++ {
		a, aok := s.Get(f)
		b, bok := back.Get(f)
		if aok != bok || (aok && a != b) {
			t.Errorf("%v: %v %v became %v %v", f, a, aok, b, bok)
		}
	}
	// A sample from a build with more fields than this one has them dropped, with fewer left unknown.
	long := make([]*float64, int(metrics.NumFields)+3)
	for i := range long {
		x := float64(i)
		long[i] = &x
	}
	if got := metricsFromWire(peerapi.ProjectMetrics{At: metricsNow, V: long}); got.V[metrics.NumFields-1] != float64(metrics.NumFields-1) {
		t.Fatalf("a longer sample: %v", got.V)
	}
	short := metricsFromWire(peerapi.ProjectMetrics{At: metricsNow, V: long[:2]})
	if v, ok := short.Get(metrics.CPUUser); !ok || v != 0 {
		t.Fatal("the fields it has are read")
	}
	if _, ok := short.Get(metrics.NumFields - 1); ok {
		t.Fatal("a field a shorter sample lacks is known")
	}
	nan := math.NaN()
	if _, ok := metricsFromWire(peerapi.ProjectMetrics{At: metricsNow, V: []*float64{&nan}}).Get(metrics.CPUUser); ok {
		t.Fatal("NaN is known")
	}
}

// The leader stores what a node reports for the projects the node is the home of, once, and
// nothing for a project homed elsewhere.
func TestMetricsIntakeKeepsWhatTheHomeNodeReports(t *testing.T) {
	follower, _ := newMetricsNode(t, "n2", true)
	svc, reg := newMetricsNode(t, "n2", false) // the leader's collector: it samples nothing itself
	pm := metricsToWire(follower.Own()[0])
	in := newMetricsIntake(svc, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	in.store(context.Background(), peerapi.Report{Node: "n3", Metrics: []peerapi.ProjectMetrics{pm}}) // not its project
	if !svc.Newest(metricsRef).IsZero() {
		t.Fatal("a node reported a project it is not the home of")
	}
	in.store(context.Background(), peerapi.Report{Node: "n2", Metrics: []peerapi.ProjectMetrics{pm, pm}})
	if !svc.Newest(metricsRef).Equal(pm.At) {
		t.Fatalf("newest = %v, want %v", svc.Newest(metricsRef), pm.At)
	}
	res, err := svc.Query(metricsRef, []string{"pg_stat_database_num_backends", "max_db_connections"}, metricsNow.Add(-time.Hour), metricsNow.Add(time.Hour), time.Minute)
	if err != nil || len(res.Rows) != 1 || res.Rows[0].Values["pg_stat_database_num_backends"] != 7 || res.Rows[0].Values["max_db_connections"] != 60 {
		t.Fatalf("%+v %v", res, err)
	}
	// Put never blocks and ignores a report that carries nothing.
	in.Put(peerapi.Report{Node: "n2"})
	in.Put(peerapi.Report{Node: "n2", Metrics: []peerapi.ProjectMetrics{pm}})
	in.Put(peerapi.Report{Node: "n2", Metrics: []peerapi.ProjectMetrics{pm}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { in.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

// A single server has no cluster and nothing to carry: the hook returns without registering anything.
func TestWireMetricsOnASingleServer(t *testing.T) {
	w := testWire(t)
	svc, _ := newMetricsNode(t, "n1", true)
	Provide(w, svc)
	if err := wireMetrics(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if len(w.runners) != 0 {
		t.Fatalf("%d runners", len(w.runners))
	}
}

// The alert checker takes the collector's conditions as they are.
func TestMetricsPressureAdaptsTheConditions(t *testing.T) {
	svc, _ := newMetricsNode(t, "n1", true)
	if got := metricsPressure(svc)(context.Background()); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	// The kinds are the strings of internal/alerts.
	if metrics.KindMemoryLow != "memory_low" || metrics.KindProjectDiskHigh != "project_disk_high" {
		t.Fatal("the alert kinds of the collector changed")
	}
}

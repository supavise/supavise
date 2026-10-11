package app

import (
	"context"
	"log/slog"
	"math"
	"sync"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/metrics"
	"github.com/supavise/supavise/internal/registry"
)

// wireMetrics carries the utilization samples across a cluster (internal/metrics). A project's control
// groups can be read only on the node it is homed on, and the Management API runs on the leader, so a
// node that is not the leader adds the newest sample of each project homed on it to the report it
// sends every 10 seconds, and the leader keeps what it is told as if it had sampled the project
// itself. The collector is built by Serve (newMetrics) and provided before the hooks run; a node with
// no collector (the exec supervisor) or no cluster has nothing to carry.
func wireMetrics(ctx context.Context, w *Wire) error {
	svc, ok := Get[*metrics.Service](w)
	if !ok {
		return nil
	}
	if _, clustered := Get[mesh.Mesh](w); !clustered {
		return nil
	}
	mem, ok := Get[cluster.Membership](w)
	if !ok {
		return nil
	}
	if rep, ok := Get[*cluster.Reporter](w); ok {
		rep.AddMetrics(metricsContribution(svc, mem))
	} else {
		w.Off("metrics reports", "the mesh provided no reporter; the leader's Management API has no utilization for the projects homed on this node")
	}
	if reports, ok := Get[*cluster.Reports](w); ok {
		in := newMetricsIntake(svc, w.Node.Registry, w.Log.With("component", "metrics"))
		reports.Subscribe(in.Put)
		w.Go("metrics report intake", func(ctx context.Context) error { in.Run(ctx); return nil })
	} else {
		w.Off("metrics intake", "the mesh provided no report store; the Management API has no utilization for the projects homed on other nodes")
	}
	return nil
}

// metricsContribution is what this node adds to its report: nothing on the leader, whose collector
// is the one the Management API reads, and the newest sample of each project homed here otherwise.
func metricsContribution(svc *metrics.Service, mem cluster.Membership) cluster.MetricsContributor {
	return func(context.Context) []peerapi.ProjectMetrics {
		if mem.IsLeader() {
			return nil
		}
		own := svc.Own()
		out := make([]peerapi.ProjectMetrics, len(own))
		for i, r := range own {
			out[i] = metricsToWire(r)
		}
		return out
	}
}

func metricsToWire(r metrics.Reported) peerapi.ProjectMetrics {
	v := make([]*float64, metrics.NumFields)
	for f := range v {
		if x, ok := r.Sample.Get(metrics.Field(f)); ok {
			v[f] = &x
		}
	}
	return peerapi.ProjectMetrics{Ref: r.Ref, Name: r.Name, At: r.Sample.At, V: v}
}

func metricsFromWire(pm peerapi.ProjectMetrics) metrics.Sample {
	s := metrics.NewSample(pm.At)
	for f := 0; f < min(len(pm.V), int(metrics.NumFields)); f++ {
		if x := pm.V[f]; x != nil && !math.IsNaN(*x) {
			s.Set(metrics.Field(f), *x)
		}
	}
	return s
}

// metricsIntake hands the samples that arrive in a report to the collector. The report's subscribers
// run on the goroutine of the peer request and must not block, and storing a sample writes a file, so
// the latest report of each node is kept and a goroutine of its own stores it.
type metricsIntake struct {
	svc *metrics.Service
	reg registry.Registry
	log *slog.Logger

	mu      sync.Mutex
	pending map[string]peerapi.Report
	wake    chan struct{}
}

func newMetricsIntake(svc *metrics.Service, reg registry.Registry, log *slog.Logger) *metricsIntake {
	return &metricsIntake{svc: svc, reg: reg, log: log, pending: map[string]peerapi.Report{}, wake: make(chan struct{}, 1)}
}

// Put is the subscriber: it never blocks.
func (in *metricsIntake) Put(rep peerapi.Report) {
	if len(rep.Metrics) == 0 {
		return
	}
	in.mu.Lock()
	in.pending[rep.Node] = rep
	in.mu.Unlock()
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// Run stores the reported samples until ctx ends.
func (in *metricsIntake) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-in.wake:
		}
		in.mu.Lock()
		batch := in.pending
		in.pending = map[string]peerapi.Report{}
		in.mu.Unlock()
		for _, rep := range batch {
			in.store(ctx, rep)
		}
	}
}

// store keeps the samples of a report that are new, for the projects the reporting node is the home
// of: a node speaks for its own projects only, whatever it sends.
func (in *metricsIntake) store(ctx context.Context, rep peerapi.Report) {
	for _, pm := range rep.Metrics {
		if ctx.Err() != nil {
			return
		}
		if !pm.At.After(in.svc.Newest(pm.Ref)) {
			continue // the report repeats a sample that is stored
		}
		p, err := in.reg.GetProject(ctx, pm.Ref)
		if err != nil || p.NodeID != rep.Node {
			continue
		}
		if err := in.svc.Ingest(pm.Ref, p.Name, metricsFromWire(pm)); err != nil {
			in.log.Warn("cannot store a sample a node reported", "ref", pm.Ref, "node", rep.Node, "error", err)
		}
	}
}

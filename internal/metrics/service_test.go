package metrics

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

const testRef = "abcdefghijklmnopqrst"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type lister struct {
	mu sync.Mutex
	ps []registry.Project
}

func (l *lister) ListProjects(context.Context) ([]registry.Project, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]registry.Project(nil), l.ps...), nil
}

type fakeProber struct {
	mu   sync.Mutex
	p    Probe
	err  error
	seen []Target
}

func (f *fakeProber) Probe(_ context.Context, t Target) (Probe, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, t)
	return f.p, f.err
}

type fakeDisk struct {
	mu     sync.Mutex
	use    DiskUse
	err    error
	forgot []string
}

func (f *fakeDisk) Forget(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, ref)
}

func (f *fakeDisk) Usage(context.Context, *registry.Project) (DiskUse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.use, f.err
}

// rig is a service over a fake control group tree, a fake probe and a clock.
type rig struct {
	t      *testing.T
	svc    *Service
	cg     string
	proc   string
	clock  *clock
	lister *lister
	prober *fakeProber
	disk   *fakeDisk
	store  *Store
}

func testProject() registry.Project {
	return registry.Project{
		Ref: testRef, Name: "demo", Seq: 3, Status: registry.StatusActiveHealthy, NodeID: "n1",
		Limits: config.Limits{MemoryMax: "1G", CPUQuota: "100%"},
	}
}

func newRig(t *testing.T, mod func(*Options)) *rig {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	r := &rig{
		t: t, cg: t.TempDir(), proc: t.TempDir(), clock: &clock{t: t0.Add(2 * time.Second)},
		lister: &lister{ps: []registry.Project{testProject()}}, prober: &fakeProber{}, disk: &fakeDisk{},
	}
	r.store = NewStore(filepath.Join(cfg.StateDir, "system", "metrics"))
	r.store.Now = r.clock.Now
	o := Options{
		Cfg: cfg, Projects: r.lister, Store: r.store, Cgroup: Cgroup{Root: r.cg}, Proc: Proc{Root: r.proc},
		Prober: r.prober, Disk: r.disk, CPUs: func() int { return 4 }, Now: r.clock.Now,
	}
	if mod != nil {
		mod(&o)
	}
	r.svc = New(o)
	writeProc(t, r.proc, "meminfo", meminfoFixture)
	return r
}

func (r *rig) units(pg, auth, rest unitFiles) {
	r.t.Helper()
	writeUnit(r.t, r.cg, "supavise-postgres@"+testRef+".service", pg)
	writeUnit(r.t, r.cg, "supavise-gotrue@"+testRef+".service", auth)
	writeUnit(r.t, r.cg, "supavise-postgrest@"+testRef+".service", rest)
}

func (r *rig) tick(at time.Time) {
	r.t.Helper()
	r.clock.Set(at)
	r.svc.Tick(context.Background())
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-3*math.Max(1, math.Abs(b)) }

func (r *rig) latest() Sample {
	r.t.Helper()
	rec, ok := r.svc.Recent(testRef)
	if !ok {
		r.t.Fatal("no sample")
	}
	return rec[len(rec)-1]
}

func want(t *testing.T, s Sample, f Field, v float64) {
	t.Helper()
	got, ok := s.Get(f)
	if !ok || !approx(got, v) {
		t.Fatalf("%v = %v (known %v), want %v", f, got, ok, v)
	}
}

func unknown(t *testing.T, s Sample, f Field) {
	t.Helper()
	if v, ok := s.Get(f); ok {
		t.Fatalf("%v = %v, want unknown", f, v)
	}
}

// Two samples a minute apart: the counters of the first turn the second's CPU into rates, and the
// memory is read straight. CPU is a share of the quota (one core), summed over the three units.
func TestServiceSamplesAProjectFromItsControlGroups(t *testing.T) {
	r := newRig(t, nil)
	r.prober.p = Probe{
		Backends: 12, Postgres: 2, Authenticator: 6, Admin: 1, AuthAdmin: 1, StorageAdmin: 1, Other: 1, Active: 3, Supavisor: 4,
		MaxConnections: 60, DBSize: 500 << 20, WALSize: 64 << 20,
		IO: &IOCounters{Reads: 1000, Writes: 500, ReadBytes: 8_192_000, WriteBytes: 4_096_000, WALBytes: 1_000_000},
	}
	r.disk.use = DiskUse{Used: 2 << 30, Size: 10 << 30, Quota: true}
	r.units(
		unitFiles{user: 20_000_000, system: 4_000_000, throttled: 0, anon: 300 << 20, shmem: 200 << 20, file: 500 << 20, current: 900 << 20, swap: 1 << 20, memMax: "1073741824"},
		unitFiles{user: 1_000_000, anon: 20 << 20, file: 10 << 20, current: 40 << 20, memMax: "1073741824"},
		unitFiles{anon: 10 << 20, current: 12 << 20, memMax: "1073741824"},
	)
	r.tick(t0.Add(2 * time.Second))

	first := r.latest()
	want(t, first, MemUsed, 530<<20)
	want(t, first, MemCache, 310<<20) // file - shmem, over the three units
	want(t, first, MemLimit, 1<<30)
	want(t, first, SwapUsed, 1<<20)
	unknown(t, first, CPUBusy) // nothing to subtract from yet
	unknown(t, first, OOMKills)
	unknown(t, first, IOReadOps)
	want(t, first, Backends, 12)
	want(t, first, DiskQuota, 1)

	// A minute later: Postgres used 30 s of user and 6 s of system CPU, GoTrue 0.6 s, one OOM kill.
	r.units(
		unitFiles{user: 50_000_000, system: 10_000_000, throttled: 3_000_000, anon: 300 << 20, shmem: 200 << 20, file: 500 << 20, current: 900 << 20, oomKill: 1, memMax: "1073741824"},
		unitFiles{user: 1_600_000, anon: 20 << 20, file: 10 << 20, current: 40 << 20, memMax: "1073741824"},
		unitFiles{anon: 10 << 20, current: 12 << 20, memMax: "1073741824"},
	)
	r.prober.p.IO = &IOCounters{Reads: 1600, Writes: 800, ReadBytes: 8_192_000 + 4_915_200, WriteBytes: 4_096_000 + 2_457_600, WALBytes: 1_600_000}
	r.tick(t0.Add(62 * time.Second))

	s := r.latest()
	want(t, s, CPUUser, 51)
	want(t, s, CPUSystem, 10)
	want(t, s, CPUBusy, 61)
	want(t, s, CPUThrottled, 5) // 3 s of 60 s
	want(t, s, OOMKills, 1)
	want(t, s, IOReadOps, 10)
	want(t, s, IOWriteOps, 5)
	want(t, s, IOReadBytes, 81920)
	want(t, s, IOWriteBytes, (2_457_600+600_000)/60.0)
	want(t, s, ConnSupavisor, 4)
	want(t, s, MaxConnections, 60)
	want(t, s, DBSize, 500<<20)

	// The probe went to the project's private socket, on the port of its sequence number.
	tgt := r.prober.seen[0]
	cfg := r.svc.o.Cfg
	if tgt.Port != cfg.PortsFor(testRef, 3).Postgres || tgt.Sock != filepath.Join(cfg.StateDir, "projects", testRef, "postgres", "sock") {
		t.Fatalf("probe target = %+v", tgt)
	}

	// Both samples are in the store, and the query turns them into the attributes of the report.
	res, err := r.svc.Query(testRef, []string{"cpu_usage_busy_user", "cpu_usage_busy_idle", "ram_usage_used", "ram_usage_free", "ram_usage_total", "ram_usage", "pg_stat_database_num_backends", "disk_fs_used_system", "disk_fs_avail"},
		t0, t0.Add(5*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("%d rows, want 2: %+v", len(res.Rows), res.Rows)
	}
	if _, has := res.Rows[0].Values["cpu_usage_busy_user"]; has {
		t.Fatalf("the first minute has a CPU value: %v", res.Rows[0].Values)
	}
	v := res.Rows[1].Values
	for name, w := range map[string]float64{
		"cpu_usage_busy_user": 51, "cpu_usage_busy_idle": 39, "ram_usage_used": 530 << 20, "ram_usage_total": 1 << 30,
		"ram_usage_free": 1<<30 - 530<<20 - 310<<20, "ram_usage": 100 * 530.0 / 1024, "pg_stat_database_num_backends": 12,
		"disk_fs_avail": 8 << 30, "disk_fs_used_system": 2<<30 - 500<<20 - 64<<20,
	} {
		if !approx(v[name], w) {
			t.Errorf("%s = %v, want %v", name, v[name], w)
		}
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors for measured attributes: %v", res.Errors)
	}
}

// A counter that goes backward belongs to a unit that restarted: it started from zero, so what it
// holds is the growth.
func TestServiceReadsARestartedUnitAsGrowthFromZero(t *testing.T) {
	r := newRig(t, nil)
	r.units(unitFiles{user: 100_000_000, memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	r.units(unitFiles{user: 12_000_000, memMax: "max"}, unitFiles{}, unitFiles{}) // restarted: 12 s since
	r.tick(t0.Add(62 * time.Second))
	want(t, r.latest(), CPUUser, 20)
}

// A unit that systemd stopped after an OOM kill comes back in a new control group with the counter
// at zero. The slice keeps the kernel's count: a restart in a minute when it grew is a kill; a
// restart when it did not, or a growth with no restart, is not one more than the unit's counter says.
func TestServiceCountsAnOOMKillThatRestartedTheUnit(t *testing.T) {
	steady := unitFiles{user: 100_000_000, memMax: "max", oomKill: 0}
	restarted := unitFiles{user: 2_000_000, memMax: "max", oomKill: 0} // a new group: its counters start over
	for name, c := range map[string]struct {
		sliceBefore, sliceAfter uint64
		after                   unitFiles
		want                    float64
	}{
		"restarted, the slice counted a kill":  {4, 5, restarted, 1},
		"restarted, the slice did not":         {4, 4, restarted, 0},
		"not restarted, the slice grew":        {4, 5, unitFiles{user: 160_000_000, memMax: "max"}, 0},
		"restarted, the slice count went back": {4, 1, restarted, 0},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			writeSliceKills(t, r.cg, c.sliceBefore)
			r.units(steady, unitFiles{}, unitFiles{})
			r.tick(t0.Add(2 * time.Second))
			writeSliceKills(t, r.cg, c.sliceAfter)
			r.units(c.after, unitFiles{}, unitFiles{})
			r.tick(t0.Add(62 * time.Second))
			want(t, r.latest(), OOMKills, c.want)
		})
	}

	// The first tick has no earlier count to compare with: a restart then is not a kill.
	r := newRig(t, nil)
	r.units(steady, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	writeSliceKills(t, r.cg, 9)
	r.units(restarted, unitFiles{}, unitFiles{})
	r.tick(t0.Add(62 * time.Second))
	want(t, r.latest(), OOMKills, 0)

	// A kill the unit's own counter shows (the unit kept its group) is counted once, not twice.
	r = newRig(t, nil)
	writeSliceKills(t, r.cg, 1)
	r.units(steady, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	writeSliceKills(t, r.cg, 2)
	r.units(unitFiles{user: 160_000_000, memMax: "max", oomKill: 1}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(62 * time.Second))
	want(t, r.latest(), OOMKills, 1)
}

// A project without a CPU quota is measured against the node's cores.
func TestServiceMeasuresAProjectWithoutAQuotaAgainstTheNode(t *testing.T) {
	r := newRig(t, nil)
	p := testProject()
	p.Limits.CPUQuota = ""
	r.lister.ps = []registry.Project{p}
	r.units(unitFiles{user: 0, memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	r.units(unitFiles{user: 120_000_000, memMax: "max"}, unitFiles{}, unitFiles{}) // 2 cores of the 4
	r.tick(t0.Add(62 * time.Second))
	want(t, r.latest(), CPUUser, 50)
}

// The units of one project each have the quota to themselves, so together they can use more than
// it: the shares are scaled so that they never exceed 100.
func TestServiceCapsBusyAtTheQuota(t *testing.T) {
	r := newRig(t, nil)
	r.units(unitFiles{memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	r.units(unitFiles{user: 45_000_000, system: 30_000_000, memMax: "max"}, unitFiles{user: 45_000_000, memMax: "max"}, unitFiles{})
	r.tick(t0.Add(62 * time.Second))
	s := r.latest()
	want(t, s, CPUBusy, 100)
	want(t, s, CPUUser, 100*90.0/120)
	want(t, s, CPUSystem, 100*30.0/120)
}

// The memory limit is the project's MemoryMax, whatever the control group says.
func TestServiceMemoryLimitSources(t *testing.T) {
	r := newRig(t, nil)
	p := testProject()
	p.Limits.MemoryMax = ""
	r.lister.ps = []registry.Project{p}
	r.units(unitFiles{memMax: "2147483648"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	want(t, r.latest(), MemLimit, 2<<30) // the control group's
	r.units(unitFiles{memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(62 * time.Second))
	want(t, r.latest(), MemLimit, 8000000*1024) // the node's memory
}

// A failed probe loses the SQL values and keeps the rest.
func TestServiceKeepsTheControlGroupValuesWhenTheProbeFails(t *testing.T) {
	r := newRig(t, nil)
	r.prober.err = errors.New("the cluster is starting")
	r.units(unitFiles{anon: 1 << 20, memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	s := r.latest()
	want(t, s, MemUsed, 1<<20)
	unknown(t, s, Backends)
	unknown(t, s, DBSize)
}

func TestServiceSkipsProjectsThatDoNotRunHere(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Local = func(p *registry.Project) bool { return p.NodeID == "n1" } })
	paused, elsewhere, down := testProject(), testProject(), testProject()
	paused.Ref, paused.Status = "pausedpausedpausedpa", registry.StatusInactive
	elsewhere.Ref, elsewhere.NodeID = "elsewhereelsewhereel", "n2"
	down.Ref = "downdowndowndowndown" // running by its record, but its units have no control group
	r.lister.ps = []registry.Project{paused, elsewhere, down, {Ref: config.SystemRef, Status: registry.StatusActiveHealthy, NodeID: "n1"}}
	for _, ref := range []string{paused.Ref, elsewhere.Ref, config.SystemRef, down.Ref} {
		writeUnit(t, r.cg, "supavise-postgres@"+ref+".service", unitFiles{anon: 1, memMax: "max"})
	}
	// down's cgroup is removed again: its Postgres is not running.
	if err := removeUnit(r.cg, "supavise-postgres@"+down.Ref+".service"); err != nil {
		t.Fatal(err)
	}
	r.tick(t0.Add(2 * time.Second))
	for _, ref := range []string{paused.Ref, elsewhere.Ref, config.SystemRef, down.Ref} {
		if _, ok := r.svc.Recent(ref); ok {
			t.Errorf("%s was sampled", ref)
		}
		if us, _ := r.store.Range(ref, t0, t0.Add(time.Hour)); len(us) != 0 {
			t.Errorf("%s has history", ref)
		}
	}
}

// What a follower reports is stored as if it had been sampled here, once, and the project then
// answers a query and the warnings.
func TestServiceIngestsTheSamplesOfAnotherNode(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Local = func(*registry.Project) bool { return false } })
	smp := sampleAt(t0.Add(time.Minute), 70)
	smp.Set(MemLimit, 1<<30)
	smp.Set(MemUsed, 1<<29)
	if err := r.svc.Ingest(testRef, "demo", smp); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Ingest(testRef, "demo", smp); err != nil { // the next report repeats it
		t.Fatal(err)
	}
	r.tick(t0.Add(2 * time.Minute)) // a tick must not drop what was reported
	res, err := r.svc.Query(testRef, []string{"cpu_usage", "ram_usage"}, t0, t0.Add(10*time.Minute), time.Minute)
	if err != nil || len(res.Rows) != 1 || res.Rows[0].Values["cpu_usage"] != 70 || res.Rows[0].Values["ram_usage"] != 50 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, ok := r.svc.Warnings(testRef); !ok {
		t.Fatal("no warnings for a project that reported")
	}
}

func TestServiceSweepsTheHistoryOfDeletedProjects(t *testing.T) {
	r := newRig(t, nil)
	_ = r.store.Append("deleteddeleteddeleted", sampleAt(t0, 1))
	_ = r.store.Append(testRef, sampleAt(t0, 1))
	r.units(unitFiles{memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	if us, _ := r.store.Range("deleteddeleteddeleted", t0, t0.Add(time.Hour)); len(us) != 0 {
		t.Fatal("the history of a project that is not in the registry was kept")
	}
	if us, _ := r.store.Range(testRef, t0, t0.Add(time.Hour)); len(us) == 0 {
		t.Fatal("the history of a live project was removed")
	}
}

// A project this node sampled and that is homed on another node now is not this node's to judge: the
// flag that says so is cleared by the tick that finds it homed elsewhere, and by the first sample the
// new home reports. A project that is only paused stays this node's.
func TestServiceStopsJudgingAProjectThatMovedAway(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Local = func(p *registry.Project) bool { return p.NodeID == "n1" } })
	r.units(unitFiles{anon: 1 << 20, memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	if got := r.svc.Own(); len(got) != 1 || got[0].Ref != testRef {
		t.Fatalf("Own after a sample = %+v", got)
	}

	r.lister.ps[0].Status = registry.StatusInactive // paused here
	r.tick(t0.Add(time.Minute + 2*time.Second))
	if got := r.svc.Own(); len(got) != 1 {
		t.Fatalf("a paused project left Own: %+v", got)
	}

	r.lister.ps[0].Status = registry.StatusActiveHealthy
	r.lister.ps[0].NodeID = "n2" // moved
	r.tick(t0.Add(2*time.Minute + 2*time.Second))
	if got := r.svc.Own(); len(got) != 0 {
		t.Fatalf("a project homed elsewhere stays in Own: %+v", got)
	}
}

func TestServiceIngestEndsTheOwnershipOfAProject(t *testing.T) {
	r := newRig(t, nil)
	const gb = float64(1 << 30)
	r.lister.ps = nil
	ingestOwn(t, r.svc, testRef, diskSample(t0.Add(time.Minute), 9.5*gb, 10*gb, true))
	r.clock.Set(t0.Add(2 * time.Minute))
	if len(r.svc.Pressure()) != 2 || len(r.svc.Own()) != 1 {
		t.Fatalf("setup: %s, own %d", keysOf(r.svc.Pressure()), len(r.svc.Own()))
	}
	// The project moved: its new home reports a sample, and this node stops judging it.
	if err := r.svc.Ingest(testRef, "demo", diskSample(t0.Add(2*time.Minute), 9.5*gb, 10*gb, true)); err != nil {
		t.Fatal(err)
	}
	if got := keysOf(r.svc.Pressure()); got != "" {
		t.Errorf("this node still raises %s for a project another node reports", got)
	}
	if got := r.svc.Own(); len(got) != 0 {
		t.Errorf("Own still lists the project: %+v", got)
	}
	if _, ok := r.svc.Warnings(testRef); !ok {
		t.Error("the reported project lost its warnings")
	}
}

// A deleted project leaves nothing behind: Forget (the Management API's delete) at once, and a tick
// for a delete made elsewhere, such as the CLI's.
func TestServiceForgetsADeletedProject(t *testing.T) {
	var gone []string
	r := newRig(t, func(o *Options) { o.Gone = func(ref string) { gone = append(gone, ref) } })
	r.units(unitFiles{anon: 1 << 20, memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	if us, _ := r.store.Range(testRef, t0, t0.Add(time.Hour)); len(us) != 1 {
		t.Fatalf("setup: %d units stored", len(us))
	}

	r.svc.Forget(testRef)
	if _, ok := r.svc.Recent(testRef); ok {
		t.Error("the samples in memory were kept")
	}
	if us, _ := r.store.Range(testRef, t0, t0.Add(time.Hour)); len(us) != 0 {
		t.Errorf("the history on disk was kept: %d units", len(us))
	}
	if len(r.disk.forgot) != 1 || r.disk.forgot[0] != testRef {
		t.Errorf("the disk source was told to forget %v", r.disk.forgot)
	}
	if len(gone) != 0 {
		t.Errorf("Gone was called for a delete the service did itself: %v", gone)
	}

	// Another process deletes it: the tick finds the ref gone from the registry.
	r.tick(t0.Add(time.Minute + 2*time.Second))
	if _, ok := r.svc.Recent(testRef); !ok {
		t.Fatal("setup: no sample")
	}
	r.lister.ps = nil
	r.tick(t0.Add(2*time.Minute + 2*time.Second))
	if _, ok := r.svc.Recent(testRef); ok {
		t.Error("the samples of a project gone from the registry were kept")
	}
	if us, _ := r.store.Range(testRef, t0, t0.Add(time.Hour)); len(us) != 0 {
		t.Errorf("the history of a project gone from the registry was kept: %d units", len(us))
	}
	if len(gone) != 1 || gone[0] != testRef {
		t.Errorf("Gone = %v", gone)
	}
}

// A ref used again (supavise projects create --ref) starts with no history, even when the delete and
// the create fall between two ticks.
func TestServiceStartsAProjectCreatedAgainFromNothing(t *testing.T) {
	var gone []string
	r := newRig(t, func(o *Options) { o.Gone = func(ref string) { gone = append(gone, ref) } })
	r.lister.ps[0].CreatedAt = t0.Add(-time.Hour)
	r.units(unitFiles{anon: 1 << 20, memMax: "max"}, unitFiles{}, unitFiles{})
	r.tick(t0.Add(2 * time.Second))
	r.tick(t0.Add(time.Minute + 2*time.Second))
	if us, _ := r.store.Range(testRef, t0, t0.Add(time.Hour)); len(us) != 2 {
		t.Fatalf("setup: %d units stored", len(us))
	}

	r.lister.ps[0].CreatedAt = t0.Add(90 * time.Second) // another project, same ref
	r.tick(t0.Add(2*time.Minute + 2*time.Second))
	us, _ := r.store.Range(testRef, t0, t0.Add(time.Hour))
	if len(us) != 1 || !us[0].At.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("the new project shows %d units, the first at %v: it kept the old one's history", len(us), func() any {
			if len(us) > 0 {
				return us[0].At
			}
			return nil
		}())
	}
	if rec, _ := r.svc.Recent(testRef); len(rec) != 1 {
		t.Errorf("the new project holds %d samples in memory", len(rec))
	}
	if len(gone) != 1 || gone[0] != testRef {
		t.Errorf("Gone = %v", gone)
	}

	// The same project over the following ticks is not taken for a new one.
	r.tick(t0.Add(3*time.Minute + 2*time.Second))
	if us, _ := r.store.Range(testRef, t0, t0.Add(time.Hour)); len(us) != 2 || len(gone) != 1 {
		t.Errorf("a project that did not change lost its history: %d units, Gone = %v", len(us), gone)
	}
}

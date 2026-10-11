package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

const (
	// DefaultInterval is how often a project is sampled: Studio's finest bucket is a minute.
	DefaultInterval = time.Minute
	// DefaultConcurrency is how many projects are sampled at once. A sample is a few small files and
	// one short SQL session, so four keep a node of 35 projects well inside its minute.
	DefaultConcurrency = 4
	// recentKeep is how many samples of each project stay in memory: what the resource warnings
	// average over, and the latest values.
	recentKeep = 30
	// sweepEvery is how often the files of deleted projects are removed.
	sweepEvery = 24 * time.Hour
	// tickDelay is how long after a minute boundary the sample is taken, so that it belongs to the
	// minute it is stored in.
	tickDelay = 2 * time.Second
)

// ProjectLister is the registry's list of projects.
type ProjectLister interface {
	ListProjects(ctx context.Context) ([]registry.Project, error)
}

// Options are what a Service is built from. Only Cfg, Projects and Store are required.
type Options struct {
	Cfg      *config.Config
	Projects ProjectLister
	// Local says whether a project is homed on this node, whose control groups are the only ones
	// this process can read. Nil counts every project as local (a single server).
	Local func(p *registry.Project) bool
	Store *Store
	// Cgroup, Proc, Prober and Disk default to the real ones.
	Cgroup Cgroup
	Proc   Proc
	Prober Prober
	Disk   DiskSource
	// CPUs is the node's core count, which a project without a CPU quota is measured against; nil
	// means runtime.NumCPU.
	CPUs        func() int
	Interval    time.Duration
	Concurrency int
	Log         *slog.Logger
	Now         func() time.Time
}

// Service samples the projects homed on this node once a minute and answers questions about what it
// saw: the series Studio's Database report draws, the resource warnings, and the conditions the
// alert checker raises. Run starts the sampling.
type Service struct {
	o Options

	mu     sync.Mutex
	proj   map[string]*projState
	node   NodeSample
	raised map[string]bool
	swept  time.Time
	// warned is when each kind of failure was last logged: a full disk would otherwise write a line
	// for every project every minute.
	warned map[string]time.Time
}

// projState is what the service remembers of one project between two samples.
type projState struct {
	name string
	// own is true for a project this node samples itself, false for one whose samples were reported
	// by the node it is homed on (Ingest).
	own bool
	// prev are the counters of the previous reading, to turn counters into rates.
	prevAt    time.Time
	prevUnits map[string]UnitStats
	prevIO    *IOCounters
	// recent are the latest samples, oldest first.
	recent []Sample
}

// NodeSample is the node's memory at the last sample.
type NodeSample struct {
	At       time.Time
	Mem      Meminfo
	Pressure Pressure
	OK       bool
}

// New returns a service. It does not start sampling.
func New(o Options) *Service {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Prober == nil {
		o.Prober = SQLProber{}
	}
	if o.CPUs == nil {
		o.CPUs = runtime.NumCPU
	}
	if o.Store == nil && o.Cfg != nil {
		o.Store = NewStore(filepath.Join(o.Cfg.StateDir, "system", "metrics"))
	}
	return &Service{o: o, proj: map[string]*projState{}, raised: map[string]bool{}, warned: map[string]time.Time{}}
}

func (s *Service) now() time.Time {
	if s.o.Now != nil {
		return s.o.Now()
	}
	return time.Now()
}

// warnEvery is how often a failure of one kind is logged again.
const warnEvery = 15 * time.Minute

// warn logs a failure at most once per warnEvery for each key.
func (s *Service) warn(key, msg string, args ...any) {
	now := s.now()
	s.mu.Lock()
	last, seen := s.warned[key]
	if seen && now.Sub(last) < warnEvery {
		s.mu.Unlock()
		return
	}
	s.warned[key] = now
	s.mu.Unlock()
	s.o.Log.Warn(msg, args...)
}

// Run samples until ctx ends: once at the start (the first reading of a counter has nothing to
// subtract from, so a sample that is a minute later is the first to carry rates) and then a couple of
// seconds after every minute boundary.
func (s *Service) Run(ctx context.Context) error {
	s.Tick(ctx)
	for {
		now := s.now()
		next := now.Truncate(s.o.Interval).Add(s.o.Interval + tickDelay)
		t := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		s.Tick(ctx)
	}
}

// Tick takes one round of samples: the node's memory, then every project that runs here.
func (s *Service) Tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	now := s.now()
	s.sampleNode(now)

	ps, err := s.o.Projects.ListProjects(ctx)
	if err != nil {
		s.o.Log.Warn("metrics: cannot list the projects", "error", err)
		return
	}
	live := map[string]bool{}
	var todo []registry.Project
	for _, p := range ps {
		live[p.Ref] = true
		if p.Ref == config.SystemRef || !p.Status.Running() || (s.o.Local != nil && !s.o.Local(&p)) {
			continue
		}
		todo = append(todo, p)
	}
	s.forgetStale(live, todo)
	if len(todo) > 0 && !s.o.Cgroup.SliceExists() {
		s.warn("cgroup-slice", "metrics: there is no control group for supavise.slice, so no project can be measured (the host needs the unified cgroup v2 hierarchy under /sys/fs/cgroup)")
	}

	sem := make(chan struct{}, s.o.Concurrency)
	var wg sync.WaitGroup
	for _, p := range todo {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.sampleProject(ctx, p, now)
		}()
	}
	wg.Wait()

	s.mu.Lock()
	due := s.swept.IsZero() || now.Sub(s.swept) >= sweepEvery
	if due {
		s.swept = now
	}
	s.mu.Unlock()
	if due && s.o.Store != nil {
		if n, err := s.o.Store.Sweep(func(ref string) bool { return live[ref] }); err != nil {
			s.o.Log.Warn("metrics: cannot remove the history of deleted projects", "error", err)
		} else if n > 0 {
			s.o.Log.Info("metrics: removed the history of deleted projects", "projects", n)
		}
	}
}

// forgetStale drops the in-memory state of projects that are gone, paused or homed elsewhere now,
// and keeps the names of those that run here.
func (s *Service) forgetStale(live map[string]bool, running []registry.Project) {
	here := map[string]string{}
	for _, p := range running {
		here[p.Ref] = p.Name
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ref, st := range s.proj {
		if name, ok := here[ref]; ok {
			st.name = name
			continue
		}
		if !live[ref] {
			delete(s.proj, ref)
			continue
		}
		// Paused or moved: its counters are meaningless when it comes back, but what was sampled
		// stays for the warnings until it ages out.
		st.prevAt, st.prevUnits, st.prevIO = time.Time{}, nil, nil
	}
	for k := range s.raised {
		if ref, ok := raisedRef(k); ok && !live[ref] {
			delete(s.raised, k)
		}
	}
}

func (s *Service) sampleNode(now time.Time) {
	mem, err := s.o.Proc.Meminfo()
	if err != nil {
		s.o.Log.Debug("metrics: cannot read the node's memory", "error", err)
		return
	}
	psi, _ := s.o.Proc.MemoryPressure()
	s.mu.Lock()
	s.node = NodeSample{At: now, Mem: mem, Pressure: psi, OK: true}
	s.mu.Unlock()
}

// Node returns the node's memory at the last sample.
func (s *Service) Node() NodeSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.node
}

// counterDelta is how much a counter grew. One that went backward started again from zero (the
// unit restarted), so what it holds now is what it grew by.
func counterDelta(cur, prev uint64) float64 {
	if cur >= prev {
		return float64(cur - prev)
	}
	return float64(cur)
}

func floatDelta(cur, prev float64) (float64, bool) {
	if cur < prev {
		return 0, false
	}
	return cur - prev, true
}

// maxGap is the longest time between two readings that still gives a rate for one minute: after a
// longer pause (the daemon was stopped) the mean over the pause would be written as a minute's.
const maxGap = 3 * time.Minute

// sampleProject takes one project's sample and stores it.
func (s *Service) sampleProject(ctx context.Context, p registry.Project, now time.Time) {
	st := s.stateOf(p.Ref, p.Name)
	smp, next, ok := s.read(ctx, p, now, st)
	if !ok {
		return
	}
	if s.o.Store != nil {
		if err := s.o.Store.Append(p.Ref, smp); err != nil {
			s.warn("store/"+p.Ref, "metrics: cannot store a sample (logged once in a while)", "ref", p.Ref, "error", err)
		}
	}
	s.mu.Lock()
	st.own = true
	st.prevAt, st.prevUnits, st.prevIO = now, next.units, next.io
	st.push(smp)
	s.mu.Unlock()
}

func (s *Service) stateOf(ref, name string) *projState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.proj[ref]
	if st == nil {
		st = &projState{name: name}
		s.proj[ref] = st
	}
	return st
}

func (st *projState) push(smp Sample) {
	if n := len(st.recent); n > 0 && !smp.At.After(st.recent[n-1].At) {
		if smp.At.Equal(st.recent[n-1].At) {
			st.recent[n-1] = smp
		}
		return
	}
	st.recent = append(st.recent, smp)
	if len(st.recent) > recentKeep {
		st.recent = slices.Delete(st.recent, 0, len(st.recent)-recentKeep)
	}
}

type readings struct {
	units map[string]UnitStats
	io    *IOCounters
}

// read builds the sample of p from its control groups, one SQL session and its disk use. ok is
// false when the project's Postgres has no control group (it is not running).
func (s *Service) read(ctx context.Context, p registry.Project, now time.Time, st *projState) (smp Sample, next readings, ok bool) {
	s.mu.Lock()
	prevAt, prevUnits, prevIO := st.prevAt, st.prevUnits, st.prevIO
	s.mu.Unlock()

	next.units = map[string]UnitStats{}
	for svc, unit := range ProjectUnits(p.Ref) {
		u, err := s.o.Cgroup.ReadUnit(unit)
		switch {
		case err == nil:
			next.units[svc] = u
		case errors.Is(err, ErrNoUnit):
		default:
			s.warn("cgroup/"+unit, "metrics: cannot read a control group (logged once in a while)", "ref", p.Ref, "unit", unit, "error", err)
		}
	}
	pg, up := next.units[config.SvcPostgres]
	if !up {
		return smp, next, false
	}
	smp = NewSample(now)

	// Memory: what the project's units hold, against the project's limit.
	var anon, shmem, file, current, swap uint64
	for _, u := range next.units {
		anon += u.Anon
		shmem += u.Shmem
		file += u.File
		current += u.MemCurrent
		swap += u.Swap
	}
	smp.Set(MemUsed, float64(anon+shmem))
	smp.Set(MemCache, float64(file-min(file, shmem)))
	smp.Set(MemCurrent, float64(current))
	smp.Set(SwapUsed, float64(swap))
	smp.Set(MemLimit, float64(s.memoryLimit(p, pg)))

	// Rates need the previous reading.
	dt := now.Sub(prevAt)
	if !prevAt.IsZero() && dt > 0 && dt <= maxGap {
		cores := s.cores(p)
		wallUs := dt.Seconds() * 1e6 * cores
		var user, system, throttled, oom float64
		for svc, u := range next.units {
			pu := prevUnits[svc] // zero when the unit is new: its whole usage is the delta
			user += counterDelta(u.CPUUserUsec, pu.CPUUserUsec)
			system += counterDelta(u.CPUSystemUsec, pu.CPUSystemUsec)
			throttled += counterDelta(u.ThrottledUsec, pu.ThrottledUsec)
			oom += counterDelta(u.OOMKill, pu.OOMKill)
		}
		userPct, systemPct := 100*user/wallUs, 100*system/wallUs
		if sum := userPct + systemPct; sum > 100 { // each unit has the quota to itself, so the units can add up past it
			userPct, systemPct = userPct*100/sum, systemPct*100/sum
		}
		smp.Set(CPUUser, userPct)
		smp.Set(CPUSystem, systemPct)
		smp.Set(CPUBusy, userPct+systemPct)
		smp.Set(CPUThrottled, math.Min(100*throttled/wallUs, 100))
		smp.Set(OOMKills, oom)
	}

	// SQL: connections, sizes and block counters.
	tgt := Target{
		Ref:  p.Ref,
		Sock: filepath.Join(s.o.Cfg.Paths().ProjectService(p.Ref, config.SvcPostgres), "sock"),
		Port: s.o.Cfg.PortsFor(p.Ref, p.Seq).Postgres,
	}
	if pr, err := s.o.Prober.Probe(ctx, tgt); err != nil {
		s.o.Log.Debug("metrics: the probe failed", "ref", p.Ref, "error", err)
	} else {
		smp.Set(Backends, float64(pr.Backends))
		smp.Set(ConnPostgres, float64(pr.Postgres))
		smp.Set(ConnAuthenticator, float64(pr.Authenticator))
		smp.Set(ConnAdmin, float64(pr.Admin))
		smp.Set(ConnAuthAdmin, float64(pr.AuthAdmin))
		smp.Set(ConnStorageAdmin, float64(pr.StorageAdmin))
		smp.Set(ConnOther, float64(pr.Other))
		smp.Set(ConnActive, float64(pr.Active))
		smp.Set(ConnSupavisor, float64(pr.Supavisor))
		smp.Set(MaxConnections, float64(pr.MaxConnections))
		smp.Set(DBSize, float64(pr.DBSize))
		smp.Set(WALSize, float64(pr.WALSize))
		if pr.IO != nil {
			next.io = pr.IO
			if prevIO != nil && dt > 0 && dt <= maxGap {
				secs := dt.Seconds()
				rate := func(f Field, cur, prev float64) {
					if d, ok := floatDelta(cur, prev); ok {
						smp.Set(f, d/secs)
					}
				}
				rate(IOReadOps, pr.IO.Reads, prevIO.Reads)
				rate(IOWriteOps, pr.IO.Writes, prevIO.Writes)
				rate(IOReadBytes, pr.IO.ReadBytes, prevIO.ReadBytes)
				// Bytes written are the relation blocks and the WAL.
				if w, ok := floatDelta(pr.IO.WriteBytes, prevIO.WriteBytes); ok {
					if wal, ok := floatDelta(pr.IO.WALBytes, prevIO.WALBytes); ok {
						smp.Set(IOWriteBytes, (w+wal)/secs)
					}
				}
			}
		}
	}

	// Disk.
	if s.o.Disk != nil {
		if du, err := s.o.Disk.Usage(ctx, &p); err != nil {
			s.o.Log.Debug("metrics: cannot read the disk use", "ref", p.Ref, "error", err)
		} else {
			smp.Set(DiskUsed, float64(du.Used))
			smp.Set(DiskSize, float64(du.Size))
			q := 0.0
			if du.Quota {
				q = 1
			}
			smp.Set(DiskQuota, q)
		}
	}
	return smp, next, true
}

// memoryLimit is the project's MemoryMax: the record's, else the control group's, else the node's
// memory.
func (s *Service) memoryLimit(p registry.Project, pg UnitStats) uint64 {
	if n, err := units.ParseBytes(p.Limits.MemoryMax); err == nil && n > 0 && n != ^uint64(0) {
		return n
	}
	if pg.MemMax > 0 {
		return pg.MemMax
	}
	if m := s.Node().Mem.Total; m > 0 {
		return m
	}
	if m, err := s.o.Proc.Meminfo(); err == nil {
		return m.Total
	}
	return 0
}

// cores is the CPU a project's quota stands for, in cores: CPUQuota=100% is one. A project without
// a quota is measured against the node's cores.
func (s *Service) cores(p registry.Project) float64 {
	if q, err := units.ParseCPUQuota(p.Limits.CPUQuota); err == nil && q != ^uint64(0) && q > 0 {
		return float64(q) / 1e6
	}
	return float64(max(s.o.CPUs(), 1))
}

// Ingest records a sample taken on another node (a follower's report to the leader) as if this node
// had taken it, so that the leader's Management API can answer for the projects homed elsewhere.
// A sample that is not newer than the last one of the project is ignored, so a report that repeats
// the latest sample is harmless.
func (s *Service) Ingest(ref, name string, smp Sample) error {
	smp.At = smp.At.UTC().Truncate(time.Minute)
	s.mu.Lock()
	st := s.proj[ref]
	if st == nil {
		st = &projState{name: name}
		s.proj[ref] = st
	}
	if n := len(st.recent); n > 0 && !smp.At.After(st.recent[n-1].At) {
		s.mu.Unlock()
		return nil
	}
	st.push(smp)
	s.mu.Unlock()
	if s.o.Store == nil {
		return nil
	}
	return s.o.Store.Append(ref, smp)
}

// Reported is the newest sample of a project this node sampled itself.
type Reported struct {
	Ref, Name string
	Sample    Sample
}

// Own returns the newest sample of each project this node samples itself (not those it was told of),
// in the order of the refs. A node that is not the leader reports these to the leader, whose
// Management API answers for every project of the cluster. A sample that has not been refreshed
// for staleAfter (the project was paused or moved) is left out.
func (s *Service) Own() []Reported {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Reported
	for ref, st := range s.proj {
		if !st.own || len(st.recent) == 0 {
			continue
		}
		last := st.recent[len(st.recent)-1]
		if now.Sub(last.At) > staleAfter {
			continue
		}
		out = append(out, Reported{Ref: ref, Name: st.name, Sample: last})
	}
	slices.SortFunc(out, func(a, b Reported) int { return strings.Compare(a.Ref, b.Ref) })
	return out
}

// Newest is the time of the newest sample the service holds for a project, zero when it has none.
func (s *Service) Newest(ref string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.proj[ref]; st != nil && len(st.recent) > 0 {
		return st.recent[len(st.recent)-1].At
	}
	return time.Time{}
}

// Recent returns the newest samples the service holds for a project, oldest first: the ones the
// warnings average over. ok is false when it has none.
func (s *Service) Recent(ref string) (samples []Sample, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.proj[ref]
	if st == nil || len(st.recent) == 0 {
		return nil, false
	}
	return slices.Clone(st.recent), true
}

// Query answers a request for the series of one project: attrs bucketed by step over [from, to).
// The first bucket starts at the step boundary at or before from, so that it holds a whole bucket
// of data. A project with no history has no rows, and a Meta for every attribute.
func (s *Service) Query(ref string, attrs []string, from, to time.Time, step time.Duration) (Result, error) {
	if step <= 0 {
		step = time.Hour
	}
	var rows []Row
	if s.o.Store != nil {
		stepS := int64(step / time.Second)
		start := floorDiv(from.Unix(), stepS) * stepS
		us, err := s.o.Store.Range(ref, time.Unix(start, 0), to)
		if err != nil {
			return Result{}, err
		}
		rows = bucketRows(us, attrs, stepS)
	}
	return Assemble(attrs, rows, ""), nil
}

// bucketRows groups units into buckets of stepS seconds and computes the attributes of each.
func bucketRows(us []Unit, attrs []string, stepS int64) []Row {
	buckets := map[int64]*agg{}
	var keys []int64
	for _, u := range us {
		k := floorDiv(u.At.Unix(), stepS) * stepS
		a := buckets[k]
		if a == nil {
			a = newAgg()
			buckets[k] = a
			keys = append(keys, k)
		}
		a.add(u)
	}
	slices.Sort(keys)
	rows := make([]Row, 0, len(keys))
	for _, k := range keys {
		r := Row{At: time.Unix(k, 0).UTC(), Values: map[string]float64{}}
		for _, name := range attrs {
			if a, ok := attributes[name]; ok && a.val != nil {
				if v, ok := a.val(buckets[k]); ok {
					r.Values[name] = v
				}
			}
		}
		rows = append(rows, r)
	}
	return rows
}

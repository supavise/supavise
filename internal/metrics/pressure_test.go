package metrics

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

func meminfo(availKB int) string {
	return fmt.Sprintf("MemTotal: 8000000 kB\nMemAvailable: %d kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n", availKB)
}

func psi(some, full float64) string {
	return fmt.Sprintf("some avg10=0.00 avg60=%.2f avg300=0.00 total=1\nfull avg10=0.00 avg60=%.2f avg300=0.00 total=1\n", some, full)
}

func keysOf(cs []Condition) string {
	var ks []string
	for _, c := range cs {
		ks = append(ks, c.Key+":"+c.Severity)
	}
	return strings.Join(ks, " ")
}

// The node's memory: low available memory is a warning, under half of that critical, and an alert
// that was raised ends only when the memory is well clear of the line.
func TestPressureNodeMemoryHasHysteresis(t *testing.T) {
	r := newRig(t, nil)
	r.lister.ps = nil
	at := t0.Add(2 * time.Second)
	step := func(availKB int, pressure string) string {
		writeProc(t, r.proc, "meminfo", meminfo(availKB))
		if pressure != "" {
			writeProc(t, r.proc, "pressure/memory", pressure)
		}
		at = at.Add(time.Minute)
		r.tick(at)
		return keysOf(r.svc.Pressure())
	}
	for _, c := range []struct {
		availKB  int
		pressure string
		want     string
	}{
		{2_000_000, psi(0, 0), ""},                                                   // 25% available
		{900_000, "", ""},                                                            // 11%: not low yet
		{700_000, "", "memory_low/:warning"},                                         // 8.75% < 10%
		{500_000, "", "memory_low/:warning"},                                         // 6.25%: not under half yet
		{300_000, "", "memory_low/:warning memory_low/critical:critical"},            // 3.75% < 5%
		{450_000, "", "memory_low/:warning memory_low/critical:critical"},            // 5.6%: critical ends at the low line
		{790_000, "", "memory_low/:warning memory_low/critical:critical"},            // 9.9%: still under the line
		{1_000_000, "", "memory_low/:warning"},                                       // 12.5%: critical over, warning stays
		{1_100_000, "", "memory_low/:warning"},                                       // 13.75%: inside the band
		{1_300_000, "", ""},                                                          // 16.25%: over 1.5 times the line
		{2_000_000, psi(30, 0), "memory_low/:warning"},                               // plenty of memory, tasks wait for it
		{2_000_000, psi(14, 0), "memory_low/:warning"},                               // inside the band
		{1_400_000, psi(30, 12), "memory_low/:warning memory_low/critical:critical"}, // 17.5% available and every task waits
	} {
		if got := step(c.availKB, c.pressure); got != c.want {
			t.Fatalf("available %d kB, pressure %q: conditions = %q, want %q", c.availKB, c.pressure, got, c.want)
		}
	}
}

// The stall time is the whole node's and includes a project that reclaims within its own limit. With
// plenty of memory available that is the project's matter, not the node's.
func TestPressureIgnoresStallsWhileMemoryIsPlentiful(t *testing.T) {
	r := newRig(t, nil)
	r.lister.ps = nil
	writeProc(t, r.proc, "meminfo", meminfo(4_800_000)) // 60% available
	writeProc(t, r.proc, "pressure/memory", psi(80, 40))
	r.tick(t0.Add(2 * time.Second))
	if got := keysOf(r.svc.Pressure()); got != "" {
		t.Fatalf("conditions with 60%% available: %s", got)
	}
	writeProc(t, r.proc, "meminfo", meminfo(1_400_000)) // 17.5%: critical needs under 20%, the warning under 30%
	r.tick(t0.Add(62 * time.Second))
	if got := keysOf(r.svc.Pressure()); got != "memory_low/:warning memory_low/critical:critical" {
		t.Fatalf("conditions with 17.5%% available and stalls: %s", got)
	}
}

func TestPressureMemoryDetailNamesTheCauseAndTheWayOut(t *testing.T) {
	r := newRig(t, nil)
	r.units(unitFiles{anon: 1 << 20, memMax: "max"}, unitFiles{}, unitFiles{})
	writeProc(t, r.proc, "meminfo", meminfo(400_000))
	writeProc(t, r.proc, "pressure/memory", psi(2, 0))
	r.tick(t0.Add(2 * time.Second))
	cs := r.svc.Pressure()
	if len(cs) == 0 {
		t.Fatal("no condition")
	}
	d := cs[0].Detail
	for _, w := range []string{"5% of the node's memory is available", "waited for memory", "memory limits", "[compute] overcommit"} {
		if !strings.Contains(d, w) {
			t.Errorf("detail lacks %q: %s", w, d)
		}
	}
	if cs[0].Kind != KindMemoryLow || cs[0].Ref != "" || cs[0].Title == "" {
		t.Fatalf("%+v", cs[0])
	}
}

// A memory reading that is old says nothing about the node now.
func TestPressureIgnoresAStaleNodeReading(t *testing.T) {
	r := newRig(t, nil)
	r.lister.ps = nil
	writeProc(t, r.proc, "meminfo", meminfo(100_000))
	r.tick(t0.Add(2 * time.Second))
	if len(r.svc.Pressure()) == 0 {
		t.Fatal("no condition for 1% available")
	}
	r.clock.Set(t0.Add(time.Hour))
	if got := keysOf(r.svc.Pressure()); got != "" {
		t.Fatalf("conditions from an hour-old reading: %s", got)
	}
}

func diskSample(at time.Time, used, size float64, quota bool) Sample {
	s := NewSample(at)
	s.Set(DiskUsed, used)
	s.Set(DiskSize, size)
	if quota {
		s.Set(DiskQuota, 1)
	} else {
		s.Set(DiskQuota, 0)
	}
	return s
}

// ingestOwn stores a sample the way the service does for a project it samples itself: Ingest keeps
// what another node reported, which this node does not raise alerts for.
func ingestOwn(t *testing.T, svc *Service, ref string, smp Sample) {
	t.Helper()
	if err := svc.Ingest(ref, "demo", smp); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	svc.proj[ref].own = true
	svc.mu.Unlock()
}

// A project's disk quota: warning from 80%, critical from 90%, and each ends five points lower.
func TestPressureProjectDiskQuota(t *testing.T) {
	r := newRig(t, nil)
	r.lister.ps = nil
	const gb = float64(1 << 30)
	at := t0
	step := func(usedGB float64, quota bool) string {
		at = at.Add(time.Minute)
		r.clock.Set(at)
		ingestOwn(t, r.svc, testRef, diskSample(at, usedGB*gb, 10*gb, quota))
		return keysOf(r.svc.Pressure())
	}
	w, c := "project_disk_high/"+testRef+":warning", "project_disk_high/"+testRef+"/critical:critical"
	for _, tc := range []struct {
		usedGB float64
		quota  bool
		want   string
	}{
		{7.9, true, ""},
		{8.0, true, w},
		{7.6, true, w},  // inside the band: 76% is still above the 75% that ends it
		{7.4, true, ""}, // ended
		{8.2, true, w},  // raised again
		{9.1, true, w + " " + c},
		{8.6, true, w + " " + c}, // 86%: the critical one ends at 85
		{8.4, true, w},
		{7.4, true, ""},
		{9.9, false, ""}, // no quota in force: the node's disk_low alert is the check
	} {
		if got := step(tc.usedGB, tc.quota); got != tc.want {
			t.Fatalf("used %.1f GB (quota %v): %q, want %q", tc.usedGB, tc.quota, got, tc.want)
		}
	}
	// The text says which project and what to do, and the numbers stay out of the title.
	step(9.5, true)
	cs := r.svc.Pressure()
	if len(cs) != 2 || cs[0].Ref != testRef || !strings.Contains(cs[0].Title, "demo ("+testRef+")") || strings.Contains(cs[0].Title, "95") ||
		!strings.Contains(cs[1].Title, "over 90% full") || !strings.Contains(cs[1].Detail, "9.5 GiB of its 10.0 GiB disk (95%)") {
		t.Fatalf("%+v", cs)
	}
}

// A project that stops reporting (paused, or its node is gone) has no condition after a while, so the
// alert ends instead of staying for good.
func TestPressureEndsWhenTheProjectStopsReporting(t *testing.T) {
	r := newRig(t, nil)
	r.lister.ps = nil
	const gb = float64(1 << 30)
	r.clock.Set(t0.Add(time.Minute))
	ingestOwn(t, r.svc, testRef, diskSample(t0.Add(time.Minute), 9.5*gb, 10*gb, true))
	if len(r.svc.Pressure()) != 2 {
		t.Fatal("no conditions for a project at 95%")
	}
	r.clock.Set(t0.Add(time.Hour))
	if got := keysOf(r.svc.Pressure()); got != "" {
		t.Fatalf("conditions from an hour-old sample: %s", got)
	}
}

// A project another server reports is judged by that server, whose alerts and destinations are its
// own: this node raises nothing for it, and does not count its memory limit among the projects that
// may use this node's memory. The project's resource warnings, which Studio shows, still come from the
// report.
func TestPressureLeavesTheProjectsOfOtherServersToThem(t *testing.T) {
	r := newRig(t, nil)
	const gb = float64(1 << 30)
	r.units(unitFiles{anon: 1 << 20, memMax: "max"}, unitFiles{}, unitFiles{})
	writeProc(t, r.proc, "meminfo", meminfo(400_000))
	r.tick(t0.Add(2 * time.Second)) // the project of this node: a 1 GiB limit
	far := diskSample(t0.Add(time.Second), 9.5*gb, 10*gb, true)
	far.Set(MemLimit, 4*gb)
	if err := r.svc.Ingest("farprojectfarprojectx", "far", far); err != nil {
		t.Fatal(err)
	}
	cs := r.svc.Pressure()
	if got := keysOf(cs); got != "memory_low/:warning" {
		t.Fatalf("conditions = %q", got)
	}
	if !strings.Contains(cs[0].Detail, "up to 1.0 GiB") {
		t.Errorf("detail counts the other server's project: %s", cs[0].Detail)
	}
	if w, ok := r.svc.Warnings("farprojectfarprojectx"); !ok || w.DiskSpace != LevelCritical {
		t.Errorf("warnings of the reported project = %+v, %v", w, ok)
	}
	// A project that was paused does not count either.
	r.clock.Set(t0.Add(time.Hour))
	r.svc.Tick(context.Background())
	r.lister.ps = nil
	r.svc.Tick(context.Background())
	for _, c := range r.svc.Pressure() {
		if strings.Contains(c.Detail, "may use up to") {
			t.Errorf("detail counts a project that is gone: %s", c.Detail)
		}
	}
}

// A minute in which the disk could not be read says nothing about the quota: the alert stays on what
// was read last, until that is too old to judge by.
func TestPressureHoldsTheLastDiskReading(t *testing.T) {
	r := newRig(t, nil)
	r.lister.ps = nil
	const gb = float64(1 << 30)
	ingestOwn(t, r.svc, testRef, diskSample(t0.Add(time.Minute), 9.5*gb, 10*gb, true))
	unread := NewSample(t0.Add(2 * time.Minute)) // the control groups were read, the disk was not
	unread.Set(MemUsed, 1)
	ingestOwn(t, r.svc, testRef, unread)
	r.clock.Set(t0.Add(3 * time.Minute))
	if len(r.svc.Pressure()) != 2 {
		t.Fatalf("a failed disk read ended the alert: %s", keysOf(r.svc.Pressure()))
	}
	r.clock.Set(t0.Add(time.Hour))
	if got := keysOf(r.svc.Pressure()); got != "" {
		t.Fatalf("conditions from a reading an hour old: %s", got)
	}
}

// Just after the service started, a condition it has not measured yet is not over. It is, once the
// first minutes have passed, or once it has been measured.
func TestUnjudgedIsTrueUntilTheConditionHasBeenMeasured(t *testing.T) {
	r := newRig(t, nil)
	diskKey, memKey := KindProjectDiskHigh+"/"+testRef, KindMemoryLow+"/critical"
	for _, k := range []string{diskKey, diskKey + "/critical", KindMemoryLow + "/", memKey} {
		if !r.svc.Unjudged(k) {
			t.Errorf("%s is judged before anything was measured", k)
		}
	}
	if r.svc.Unjudged("disk_low") || r.svc.Unjudged(KindProjectDiskHigh+"/") {
		t.Error("a key the service does not own is held")
	}
	const gb = float64(1 << 30)
	r.lister.ps = nil
	r.tick(t0.Add(2 * time.Second)) // the node's memory is read
	if r.svc.Unjudged(memKey) || !r.svc.Unjudged(diskKey) {
		t.Errorf("after the node's memory was read: memory %v, disk %v", r.svc.Unjudged(memKey), r.svc.Unjudged(diskKey))
	}
	ingestOwn(t, r.svc, testRef, diskSample(t0.Add(3*time.Second), 5*gb, 10*gb, true))
	if r.svc.Unjudged(diskKey) {
		t.Error("held after the project's disk was read")
	}
	// A project that never shows up is given up on after staleAfter.
	r2 := newRig(t, nil)
	r2.clock.Set(t0.Add(2*time.Second + staleAfter))
	if r2.svc.Unjudged(diskKey) || r2.svc.Unjudged(memKey) {
		t.Error("still held after the grace period")
	}
}

func TestWarnings(t *testing.T) {
	r := newRig(t, nil)
	r.lister.ps = nil
	const gb = float64(1 << 30)
	minute := 0
	feed := func(n int, f func(i int, s *Sample)) {
		for i := 0; i < n; i++ {
			minute++
			at := t0.Add(time.Duration(minute) * time.Minute)
			s := NewSample(at)
			f(i, &s)
			if err := r.svc.Ingest(testRef, "demo", s); err != nil {
				t.Fatal(err)
			}
			r.clock.Set(at)
		}
	}
	if _, ok := r.svc.Warnings(testRef); ok {
		t.Fatal("warnings for a project nobody sampled")
	}

	// 85% of the CPU quota for five minutes is a warning; 95% is critical; two minutes of it are not enough.
	feed(5, func(_ int, s *Sample) { s.Set(CPUBusy, 85) })
	w, ok := r.svc.Warnings(testRef)
	if !ok || w.CPU != LevelWarning || w.Memory != LevelNone || w.DiskSpace != LevelNone {
		t.Fatalf("%+v %v", w, ok)
	}
	feed(5, func(_ int, s *Sample) { s.Set(CPUBusy, 95); s.Set(MemLimit, gb); s.Set(MemUsed, 0.9*gb) })
	w, _ = r.svc.Warnings(testRef)
	if w.CPU != LevelCritical || w.Memory != LevelWarning {
		t.Fatalf("%+v", w)
	}
	// Memory: a process killed for lack of it in the last quarter hour is critical whatever the share.
	feed(1, func(_ int, s *Sample) { s.Set(MemLimit, gb); s.Set(MemUsed, 0.1*gb); s.Set(OOMKills, 1) })
	if w, _ = r.svc.Warnings(testRef); w.Memory != LevelCritical {
		t.Fatalf("after an OOM kill: %+v", w)
	}
	// Disk: only against a quota in force.
	feed(1, func(_ int, s *Sample) { *s = diskSample(s.At, 8.5*gb, 10*gb, true) })
	if w, _ = r.svc.Warnings(testRef); w.DiskSpace != LevelWarning {
		t.Fatalf("disk at 85%%: %+v", w)
	}
	feed(1, func(_ int, s *Sample) { *s = diskSample(s.At, 9.5*gb, 10*gb, false) })
	if w, _ = r.svc.Warnings(testRef); w.DiskSpace != LevelNone {
		t.Fatalf("disk against a shared volume: %+v", w)
	}
}

func TestLatch(t *testing.T) {
	// above: starts at on, ends below off.
	for _, c := range []struct {
		was      bool
		v        float64
		want     bool
		above    bool
		on, off  float64
		describe string
	}{
		{false, 79, false, true, 80, 75, "below the line"},
		{false, 80, true, true, 80, 75, "at the line"},
		{true, 76, true, true, 80, 75, "inside the band, was on"},
		{false, 76, false, true, 80, 75, "inside the band, was off"},
		{true, 74, false, true, 80, 75, "below the band"},
		{false, 9, true, false, 10, 15, "under the line"},
		{true, 14, true, false, 10, 15, "inside the band, was on (below side)"},
		{true, 16, false, false, 10, 15, "over the band (below side)"},
	} {
		if got := latch(c.was, c.v, c.on, c.off, c.above); got != c.want {
			t.Errorf("%s: latch = %v, want %v", c.describe, got, c.want)
		}
	}
}

var _ = registry.StatusActiveHealthy

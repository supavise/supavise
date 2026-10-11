package metrics

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Level is how bad a resource warning is, in the words of the Management API: "warning" or
// "critical", and empty for nothing to say.
type Level string

const (
	LevelNone     Level = ""
	LevelWarning  Level = "warning"
	LevelCritical Level = "critical"
)

// The thresholds of the resource warnings and of the alerts that follow the same data. They are
// fixed, not settings: hosted does not publish its own, and these are the numbers the disk
// quota's 80 percent and 90 percent mark in its documentation.
const (
	// DiskWarnPercent and DiskCriticalPercent are the share of a project's disk quota in use at which
	// the project's disk is a warning and critical. An alert, once raised, ends below the Clear
	// values, so a project that hovers at the line does not flap.
	DiskWarnPercent          = 80
	DiskCriticalPercent      = 90
	DiskClearWarnPercent     = 75
	DiskClearCriticalPercent = 85

	// CPUWarnPercent and CPUCriticalPercent are the mean busy share of the project's CPU quota over
	// the last warnWindow samples.
	CPUWarnPercent     = 80
	CPUCriticalPercent = 90
	// MemWarnPercent and MemCriticalPercent are the mean share of the project's memory limit that
	// its units hold (anon + shmem) over the same window. A process of the project killed for lack of
	// memory in the last oomWindow samples is critical whatever the share.
	MemWarnPercent     = 85
	MemCriticalPercent = 95

	warnWindow = 5
	oomWindow  = 15

	// The node's memory pressure (PSI, /proc/pressure/memory): the share of the last minute in which
	// some task (warning) or every task (critical) waited for memory.
	psiSomeWarn  = 25.0
	psiFullCrit  = 10.0
	psiClearFrac = 0.5
)

// Warnings are the resource warnings of one project that this node can tell.
type Warnings struct {
	DiskSpace, CPU, Memory Level
}

// staleAfter is how old a project's newest sample may be and still judge it: a project that is
// paused, or whose node stopped reporting, has no warnings.
const staleAfter = 10 * time.Minute

// Warnings judges a project by the samples this node holds of it. ok is false when it holds none
// that are recent (the project is not homed here and has not reported, it is paused, or the node
// has just started).
func (s *Service) Warnings(ref string) (w Warnings, ok bool) {
	recent, ok := s.Recent(ref)
	if !ok || s.now().Sub(recent[len(recent)-1].At) > staleAfter {
		return w, false
	}
	last := recent[len(recent)-1]
	if v, ok := last.Get(DiskQuota); ok && v == 1 {
		used, ok1 := last.Get(DiskUsed)
		size, ok2 := last.Get(DiskSize)
		if ok1 && ok2 && size > 0 {
			w.DiskSpace = levelOf(100*used/size, DiskWarnPercent, DiskCriticalPercent)
		}
	}
	tail := recent[max(len(recent)-warnWindow, 0):]
	if m, n := meanOf(tail, func(s Sample) (float64, bool) { return s.Get(CPUBusy) }); n >= 3 {
		w.CPU = levelOf(m, CPUWarnPercent, CPUCriticalPercent)
	}
	if m, n := meanOf(tail, memShare); n >= 3 {
		w.Memory = levelOf(m, MemWarnPercent, MemCriticalPercent)
	}
	for _, smp := range recent[max(len(recent)-oomWindow, 0):] {
		if k, ok := smp.Get(OOMKills); ok && k > 0 {
			w.Memory = LevelCritical
		}
	}
	return w, true
}

// memShare is the percent of its memory limit that a sample's units hold.
func memShare(s Sample) (float64, bool) {
	used, ok1 := s.Get(MemUsed)
	lim, ok2 := s.Get(MemLimit)
	if !ok1 || !ok2 || lim <= 0 {
		return 0, false
	}
	return 100 * used / lim, true
}

func meanOf(ss []Sample, f func(Sample) (float64, bool)) (mean float64, n int) {
	var sum float64
	for _, s := range ss {
		if v, ok := f(s); ok {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

func levelOf(v, warn, crit float64) Level {
	switch {
	case v >= crit:
		return LevelCritical
	case v >= warn:
		return LevelWarning
	}
	return LevelNone
}

// Alert kinds raised from the node's measurements. They are the strings of internal/alerts.
const (
	KindMemoryLow       = "memory_low"
	KindProjectDiskHigh = "project_disk_high"
)

// Condition is something wrong with the node's resources now, in the shape the alert checker takes.
// Key identifies it for de-duplication and recovery; a warning and its critical successor have
// keys of their own, so that going from the first to the second is told as a new problem.
type Condition struct {
	Key, Kind, Severity, Ref, Title, Detail string
}

// maxSampleAge is how old the node's memory reading may be and still judge the node.
const maxSampleAge = 5 * time.Minute

// latch says whether a condition holds, given whether it held at the last look: it starts when v
// crosses on, and ends only when v has gone back across off. above says which side is bad.
func latch(was bool, v, on, off float64, above bool) bool {
	if above {
		if was {
			return v >= off
		}
		return v >= on
	}
	if was {
		return v < off
	}
	return v < on
}

func raisedRef(key string) (ref string, ok bool) {
	rest, ok := strings.CutPrefix(key, KindProjectDiskHigh+"/")
	if !ok {
		return "", false
	}
	ref, _, _ = strings.Cut(rest, "/")
	return ref, ref != ""
}

// Pressure returns the conditions that hold now: the node's memory, and every project whose disk
// quota is nearly spent. The service remembers which it raised, so a value that sits on the line
// does not make an alert come and go.
func (s *Service) Pressure() []Condition {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Condition
	out = append(out, s.memoryConditions(now)...)
	out = append(out, s.diskConditions(now)...)
	return out
}

func gib(b float64) string { return fmt.Sprintf("%.1f GiB", b/(1<<30)) }

// memoryConditions judges the node's memory: MemAvailable (what the kernel could give without
// swapping, page cache included) under [health] memory_low_percent, or tasks waiting for memory.
// The node can run out while every project is under its limit, because the limits add up to more
// than the memory ([compute] overcommit); this is the check that sees it. The caller holds s.mu.
func (s *Service) memoryConditions(now time.Time) []Condition {
	n := s.node
	if !n.OK || n.Mem.Total == 0 || now.Sub(n.At) > maxSampleAge {
		return nil
	}
	low := 10.0
	if s.o.Cfg != nil {
		low = float64(s.o.Cfg.Health.MemoryLow())
	}
	avail := 100 * float64(n.Mem.Available) / float64(n.Mem.Total)
	some, full := 0.0, 0.0
	if n.Pressure.OK {
		some, full = n.Pressure.SomeAvg60, n.Pressure.FullAvg60
	}

	const wk, ck = KindMemoryLow + "/", KindMemoryLow + "/critical"
	warn := latch(s.raised[wk], avail, low, low*1.5, false) || latch(s.raised[wk], some, psiSomeWarn, psiSomeWarn*psiClearFrac, true)
	crit := latch(s.raised[ck], avail, low/2, low, false) || latch(s.raised[ck], full, psiFullCrit, psiFullCrit*psiClearFrac, true)
	warn = warn || crit
	s.raised[wk], s.raised[ck] = warn, crit

	var committed float64
	for _, st := range s.proj {
		if len(st.recent) == 0 {
			continue
		}
		if v, ok := st.recent[len(st.recent)-1].Get(MemLimit); ok {
			committed += v
		}
	}
	detail := fmt.Sprintf("%.0f%% of the node's memory is available (%s of %s).", avail, gib(float64(n.Mem.Available)), gib(float64(n.Mem.Total)))
	if n.Pressure.OK {
		detail += fmt.Sprintf(" Over the last minute some tasks waited for memory %.0f%% of the time, and all of them %.0f%%.", some, full)
	}
	if committed > 0 {
		detail += fmt.Sprintf(" The projects on this node may use up to %s (their memory limits), which is %.1f times the node's memory.", gib(committed), committed/float64(n.Mem.Total))
	}
	detail += " Pause or delete projects you do not need, lower [compute] overcommit so that fewer fit, or add memory. A project whose own limit is reached is stopped by the kernel without warning."

	var out []Condition
	if warn {
		out = append(out, Condition{Key: wk, Kind: KindMemoryLow, Severity: "warning", Title: "Node memory is low", Detail: detail})
	}
	if crit {
		out = append(out, Condition{Key: ck, Kind: KindMemoryLow, Severity: "critical", Title: "Node memory is almost gone", Detail: detail})
	}
	return out
}

// diskConditions judges each project's disk against its quota. Only a quota in force counts: on a
// volume without one the node's disk_low alert is the check. The caller holds s.mu.
func (s *Service) diskConditions(now time.Time) []Condition {
	refs := make([]string, 0, len(s.proj))
	for ref := range s.proj {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	var out []Condition
	for _, ref := range refs {
		st := s.proj[ref]
		wk, ck := KindProjectDiskHigh+"/"+ref, KindProjectDiskHigh+"/"+ref+"/critical"
		var pct, used, size float64
		have := false
		if n := len(st.recent); n > 0 && now.Sub(st.recent[n-1].At) <= staleAfter {
			last := st.recent[n-1]
			q, _ := last.Get(DiskQuota)
			u, ok1 := last.Get(DiskUsed)
			z, ok2 := last.Get(DiskSize)
			if q == 1 && ok1 && ok2 && z > 0 {
				pct, used, size, have = 100*u/z, u, z, true
			}
		}
		if !have {
			// Nothing to judge by: the project is paused, or has no quota. The alert, if one is open,
			// ends (the checker resolves what is no longer a condition).
			delete(s.raised, wk)
			delete(s.raised, ck)
			continue
		}
		crit := latch(s.raised[ck], pct, DiskCriticalPercent, DiskClearCriticalPercent, true)
		warn := latch(s.raised[wk], pct, DiskWarnPercent, DiskClearWarnPercent, true) || crit
		s.raised[wk], s.raised[ck] = warn, crit
		label := ref
		if st.name != "" {
			label = fmt.Sprintf("%s (%s)", st.name, ref)
		}
		detail := fmt.Sprintf("The project uses %s of its %s disk (%.0f%%). Writes fail with \"No space left on device\" for this project alone once the quota is full. Delete data, or raise the disk size under Settings > Compute and Disk in Studio.",
			gib(used), gib(size), math.Floor(pct))
		if warn {
			out = append(out, Condition{Key: wk, Kind: KindProjectDiskHigh, Severity: "warning", Ref: ref,
				Title: fmt.Sprintf("Disk of project %s is over %d%% full", label, DiskWarnPercent), Detail: detail})
		}
		if crit {
			out = append(out, Condition{Key: ck, Kind: KindProjectDiskHigh, Severity: "critical", Ref: ref,
				Title: fmt.Sprintf("Disk of project %s is over %d%% full", label, DiskCriticalPercent), Detail: detail})
		}
	}
	return out
}

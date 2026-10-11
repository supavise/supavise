// Package reqstats counts the requests that reach each project's API, per service and per time
// bucket, in memory. The edge proxy adds one count for every request that matches a route of a
// project's API (rest, graphql, auth, storage, realtime, functions); the Management API reads
// the counts to answer the dashboard's request charts (usage.api-counts, usage.api-requests-count).
//
// The counter is bounded: a project keeps one ring of per-minute counts for the last three hours
// and one ring of per-hour counts for the last eight days, which is enough for the ranges Studio
// asks for (15 minutes to 7 days) and costs about 12 KB per project. Counts are lost when the
// process stops, and they cover only the requests this process served.
package reqstats

import (
	"sync"
	"sync/atomic"
	"time"
)

// Service is the group a request is counted under. The groups follow the route table of a
// project's API (and the Kong routes of self-hosted Supabase): one per path prefix.
type Service uint8

const (
	Rest      Service = iota // /rest/v1
	GraphQL                  // /graphql/v1
	Auth                     // /auth/v1 and the OAuth discovery document
	Storage                  // /storage/v1
	Realtime                 // /realtime/v1
	Functions                // /functions/v1
	numServices
)

// String is the name of the service as it appears in the dashboard's keys: total_<name>_requests.
func (s Service) String() string {
	switch s {
	case Rest:
		return "rest"
	case GraphQL:
		return "graphql"
	case Auth:
		return "auth"
	case Storage:
		return "storage"
	case Realtime:
		return "realtime"
	case Functions:
		return "functions"
	}
	return "unknown"
}

// Counts holds one count per service.
type Counts [numServices]uint64

// Total is the sum over all services.
func (c Counts) Total() uint64 {
	var t uint64
	for _, v := range c {
		t += v
	}
	return t
}

// Of is the count of service s.
func (c Counts) Of(s Service) uint64 {
	if s >= numServices {
		return 0
	}
	return c[s]
}

// Step is the width of a bucket.
type Step int

const (
	Minute Step = iota
	Hour
	Day // a UTC calendar day
)

const (
	minuteSlots = 180 // 3 hours
	hourSlots   = 200 // 8 days and 8 hours, so that the oldest UTC day of a 7-day range is complete
)

// slot is the count of one bucket. epoch is the bucket's number (minutes or hours since the Unix
// epoch, plus one so that the zero value means "never used").
type slot struct {
	epoch  int64
	counts [numServices]uint32
}

// project is the state of one project. mu is per project, so requests to different projects never
// wait for each other, and the critical section is a handful of integer operations.
type project struct {
	mu      sync.Mutex
	minutes [minuteSlots]slot
	hours   [hourSlots]slot
	last    int64 // unix minute of the latest count; Prune drops idle projects
}

// Counter holds the counts of every project. The zero value is not usable; use New.
type Counter struct {
	projects sync.Map // ref -> *project
	pruned   atomic.Int64
}

// New returns an empty Counter.
func New() *Counter { return &Counter{} }

// Add counts one request of service s to project ref at time t. It is safe for concurrent use.
// A time older than the bucket the ring holds for that slot (a clock step back, a delayed call)
// is ignored for that ring rather than overwriting newer counts.
func (c *Counter) Add(ref string, s Service, t time.Time) {
	if s >= numServices {
		return
	}
	v, ok := c.projects.Load(ref)
	created := false
	if !ok {
		var loaded bool
		v, loaded = c.projects.LoadOrStore(ref, &project{})
		created = !loaded
	}
	p := v.(*project)
	min := t.Unix()/60 + 1
	hr := t.Unix()/3600 + 1
	p.mu.Lock()
	p.bump(&p.minutes[min%minuteSlots], min, s)
	p.bump(&p.hours[hr%hourSlots], hr, s)
	if min > p.last {
		p.last = min
	}
	p.mu.Unlock()
	if created {
		c.pruneLater(t) // a project the counter has not seen: a good moment to drop the idle ones
	}
}

// bump adds one to service s in slot sl for bucket number epoch.
func (p *project) bump(sl *slot, epoch int64, s Service) {
	switch {
	case sl.epoch == epoch:
	case sl.epoch < epoch:
		*sl = slot{epoch: epoch}
	default:
		return // the slot already holds a later bucket
	}
	if sl.counts[s] < ^uint32(0) {
		sl.counts[s]++
	}
}

// Bucket is the counts of one bucket, which starts at Start (UTC).
type Bucket struct {
	Start  time.Time
	Counts Counts
}

// Series returns the counts of project ref for every bucket of width step that overlaps
// [from, to], oldest first, with zero counts for the buckets that saw no request. The first and
// last buckets are whole buckets, so their counts include requests outside [from, to]. A range
// that reaches back further than the rings hold returns zero counts for the part they no longer
// hold; a Minute range is limited to three hours and a Hour or Day range to eight days.
func (c *Counter) Series(ref string, from, to time.Time, step Step) []Bucket {
	if to.Before(from) {
		return nil
	}
	var p *project
	if v, ok := c.projects.Load(ref); ok {
		p = v.(*project)
	}
	width := stepWidth(step)
	first := from.UTC().Truncate(width)
	last := to.UTC().Truncate(width)
	n := int(last.Sub(first)/width) + 1
	if n > maxBuckets {
		first = last.Add(-time.Duration(maxBuckets-1) * width)
		n = maxBuckets
	}
	out := make([]Bucket, n)
	for i := range out {
		out[i].Start = first.Add(time.Duration(i) * width)
	}
	if p == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range out {
		start := out[i].Start.Unix()
		switch step {
		case Minute:
			out[i].Counts = p.minute(start/60 + 1)
		case Hour:
			out[i].Counts = p.hour(start/3600 + 1)
		case Day:
			var sum Counts
			for h := int64(0); h < 24; h++ {
				cs := p.hour(start/3600 + 1 + h)
				for k := range sum {
					sum[k] += cs[k]
				}
			}
			out[i].Counts = sum
		}
	}
	return out
}

// maxBuckets bounds the size of an answer: a day range of eight days, an hour range of a
// week, and a minute range of three hours all fit.
const maxBuckets = 240

func stepWidth(s Step) time.Duration {
	switch s {
	case Hour:
		return time.Hour
	case Day:
		return 24 * time.Hour
	}
	return time.Minute
}

func (p *project) minute(epoch int64) Counts {
	sl := &p.minutes[epoch%minuteSlots]
	if sl.epoch != epoch {
		return Counts{}
	}
	return widen(sl.counts)
}

func (p *project) hour(epoch int64) Counts {
	sl := &p.hours[epoch%hourSlots]
	if sl.epoch != epoch {
		return Counts{}
	}
	return widen(sl.counts)
}

func widen(c [numServices]uint32) Counts {
	var out Counts
	for i, v := range c {
		out[i] = uint64(v)
	}
	return out
}

// pruneLater runs Prune at most once an hour, when a new project shows up. A node that creates and
// deletes projects (branches, previews) therefore does not keep the counts of the deleted ones for
// long, and a node whose projects do not change has nothing to drop.
func (c *Counter) pruneLater(now time.Time) {
	last := c.pruned.Load()
	if now.Unix()-last < 3600 || !c.pruned.CompareAndSwap(last, now.Unix()) {
		return
	}
	c.Prune(now)
}

// Prune forgets the projects that have counted nothing for more than eight days. Add calls it when
// a project first appears (at most once an hour); a caller may also call it.
func (c *Counter) Prune(now time.Time) {
	cutoff := now.Unix()/60 + 1 - int64(hourSlots)*60
	c.projects.Range(func(k, v any) bool {
		p := v.(*project)
		p.mu.Lock()
		old := p.last != 0 && p.last < cutoff // last is 0 until the first count lands
		p.mu.Unlock()
		if old {
			c.projects.Delete(k)
		}
		return true
	})
}

// Forget drops the counts of a project, for a project that was deleted.
func (c *Counter) Forget(ref string) { c.projects.Delete(ref) }

// Len is the number of projects with counts.
func (c *Counter) Len() int {
	n := 0
	c.projects.Range(func(_, _ any) bool { n++; return true })
	return n
}

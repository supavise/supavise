package reqstats

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 10, 12, 30, 15, 0, time.UTC)

func TestSeriesCountsPerBucketAndFillsGaps(t *testing.T) {
	c := New()
	c.Add("p", Rest, t0)
	c.Add("p", Rest, t0.Add(10*time.Second))
	c.Add("p", Auth, t0.Add(5*time.Minute))
	c.Add("p", GraphQL, t0.Add(5*time.Minute))
	c.Add("other", Storage, t0)

	got := c.Series("p", t0.Add(-2*time.Minute), t0.Add(6*time.Minute), Minute)
	if len(got) != 9 {
		t.Fatalf("minute buckets = %d, want 9 (12:28 to 12:36)", len(got))
	}
	if !got[0].Start.Equal(time.Date(2026, 10, 10, 12, 28, 0, 0, time.UTC)) {
		t.Errorf("first bucket starts %v, want 12:28:00", got[0].Start)
	}
	want := map[int]Counts{2: {Rest: 2}, 7: {Auth: 1, GraphQL: 1}}
	for i, b := range got {
		if b.Counts != want[i] {
			t.Errorf("bucket %d (%v) = %v, want %v", i, b.Start.Format("15:04"), b.Counts, want[i])
		}
	}

	hours := c.Series("p", t0, t0, Hour)
	if len(hours) != 1 || hours[0].Counts != (Counts{Rest: 2, Auth: 1, GraphQL: 1}) {
		t.Errorf("hour bucket = %v, want rest 2, auth 1, graphql 1", hours)
	}
	if hours[0].Counts.Total() != 4 || hours[0].Counts.Of(Rest) != 2 {
		t.Errorf("Total/Of = %d/%d", hours[0].Counts.Total(), hours[0].Counts.Of(Rest))
	}
	if o := c.Series("none", t0, t0, Hour); len(o) != 1 || o[0].Counts.Total() != 0 {
		t.Errorf("unknown project = %v, want one empty bucket", o)
	}
	if c.Series("p", t0, t0.Add(-time.Second), Hour) != nil {
		t.Error("a range that ends before it starts must be empty")
	}
}

func TestDayBucketsAreUTCDaysSummedFromHours(t *testing.T) {
	c := New()
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	c.Add("p", Rest, day.Add(23*time.Hour+59*time.Minute)) // last minute of Oct 9
	c.Add("p", Rest, day.Add(24*time.Hour))                // first second of Oct 10
	c.Add("p", Auth, day.Add(30*time.Hour))
	got := c.Series("p", day.Add(2*time.Hour), day.Add(40*time.Hour), Day)
	if len(got) != 2 {
		t.Fatalf("day buckets = %d, want 2", len(got))
	}
	if got[0].Counts != (Counts{Rest: 1}) || got[1].Counts != (Counts{Rest: 1, Auth: 1}) {
		t.Errorf("days = %v and %v, want rest 1 then rest 1 + auth 1", got[0].Counts, got[1].Counts)
	}
}

func TestRingsForgetWhatIsOlderThanTheirWindow(t *testing.T) {
	c := New()
	c.Add("p", Rest, t0)
	// 180 minutes later the minute slot of t0 is reused by a newer minute.
	later := t0.Add(180 * time.Minute)
	c.Add("p", Auth, later)
	m := c.Series("p", t0, t0, Minute)
	if m[0].Counts.Total() != 0 {
		t.Errorf("the minute of t0 still counts %v after its slot was reused", m[0].Counts)
	}
	// The hour ring still has it (three hours back is inside its eight days).
	h := c.Series("p", t0, later, Hour)
	if h[0].Counts.Of(Rest) != 1 {
		t.Errorf("hour of t0 = %v, want rest 1", h[0].Counts)
	}
	// A count with an old time does not overwrite the newer bucket in its slot.
	c.Add("p", Storage, t0)
	if got := c.Series("p", later, later, Minute)[0].Counts; got != (Counts{Auth: 1}) {
		t.Errorf("the later minute = %v, want only auth 1", got)
	}
	// 200 hours on, the slot of the first hour is reused in the hour ring too.
	c.Add("p", Realtime, t0.Add(200*time.Hour))
	if got := c.Series("p", t0, t0, Hour)[0].Counts.Total(); got != 0 {
		t.Errorf("an hour 200 hours old still counts %d", got)
	}
}

func TestSeriesIsBoundedAndPruneForgetsIdleProjects(t *testing.T) {
	c := New()
	c.Add("old", Rest, t0)
	c.Add("new", Rest, t0.Add(10*24*time.Hour))
	if n := len(c.Series("new", t0.Add(-365*24*time.Hour), t0.Add(10*24*time.Hour), Minute)); n != maxBuckets {
		t.Errorf("a year of minutes = %d buckets, want the bound %d", n, maxBuckets)
	}
	c.Prune(t0.Add(10 * 24 * time.Hour))
	if c.Len() != 1 {
		t.Errorf("projects after Prune = %d, want 1 (the idle one is dropped)", c.Len())
	}
	c.Forget("new")
	if c.Len() != 0 {
		t.Errorf("projects after Forget = %d, want 0", c.Len())
	}
	c.Add("x", Service(200), t0) // not a service: ignored
	if c.Len() != 0 {
		t.Error("an unknown service created a project")
	}
}

// Many goroutines count into the same and into different projects while another reads: run with
// -race. Nothing is lost.
func TestConcurrentAddsAreExact(t *testing.T) {
	c := New()
	const workers, per = 16, 5000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				c.Add("shared", Service(i%int(numServices)), t0)
				c.Add(fmt.Sprintf("p%d", w), Rest, t0)
			}
		}()
	}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				c.Series("shared", t0.Add(-time.Hour), t0, Minute)
			}
		}
	}()
	wg.Wait()
	close(stop)
	got := c.Series("shared", t0, t0, Minute)[0].Counts
	if got.Total() != workers*per {
		t.Errorf("shared total = %d, want %d", got.Total(), workers*per)
	}
	for w := 0; w < workers; w++ {
		if n := c.Series(fmt.Sprintf("p%d", w), t0, t0, Hour)[0].Counts.Of(Rest); n != per {
			t.Errorf("p%d = %d, want %d", w, n, per)
		}
	}
}

func BenchmarkAddParallel(b *testing.B) {
	c := New()
	refs := make([]string, 35)
	for i := range refs {
		refs[i] = fmt.Sprintf("project%02d", i)
	}
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Add(refs[i%len(refs)], Rest, t0)
			i++
		}
	})
}

// A project that shows up after an hour of quiet prunes the idle ones.
func TestNewProjectPrunesIdleOnes(t *testing.T) {
	c := New()
	c.Add("old", Rest, t0)
	c.Add("busy", Rest, t0.Add(10*24*time.Hour)) // new project, more than an hour after the first prune: drops "old"
	if c.Len() != 1 {
		t.Errorf("projects = %d, want only the busy one", c.Len())
	}
	c.Add("another", Rest, t0.Add(10*24*time.Hour+time.Minute)) // within the hour: no prune
	if c.Len() != 2 {
		t.Errorf("projects = %d, want 2", c.Len())
	}
}

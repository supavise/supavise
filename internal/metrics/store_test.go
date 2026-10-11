package metrics

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func sampleAt(at time.Time, busy float64) Sample {
	s := NewSample(at)
	s.Set(CPUBusy, busy)
	s.Set(MemUsed, 1<<30)
	return s
}

func newTestStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	s := NewStore(filepath.Join(t.TempDir(), "metrics"))
	s.Now = func() time.Time { return now }
	return s
}

func TestStoreRoundTripOfMinuteSamples(t *testing.T) {
	s := newTestStore(t, t0.Add(90*time.Minute))
	for i := 0; i < 90; i++ {
		smp := sampleAt(t0.Add(time.Duration(i)*time.Minute), float64(i))
		if i == 40 {
			smp.Set(CPUBusy, math.NaN()) // a value that could not be measured
		}
		if err := s.Append("abc", smp); err != nil {
			t.Fatal(err)
		}
	}
	us, err := s.Range("abc", t0.Add(30*time.Minute), t0.Add(60*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(us) != 30 {
		t.Fatalf("%d units, want 30", len(us))
	}
	for i, u := range us {
		wantAt := t0.Add(time.Duration(30+i) * time.Minute)
		if !u.At.Equal(wantAt) || u.N != 1 {
			t.Fatalf("unit %d: %v n=%d, want %v", i, u.At, u.N, wantAt)
		}
		if 30+i == 40 {
			if !math.IsNaN(u.Mean[CPUBusy]) {
				t.Fatalf("the unknown value came back as %v", u.Mean[CPUBusy])
			}
			continue
		}
		if u.Mean[CPUBusy] != float64(30+i) || u.Max[CPUBusy] != float64(30+i) || u.Mean[MemUsed] != 1<<30 {
			t.Fatalf("unit %d: %+v", i, u)
		}
		if !math.IsNaN(u.Mean[IOReadOps]) {
			t.Fatalf("a field nobody set reads as %v, want NaN", u.Mean[IOReadOps])
		}
	}
}

// Ranges are half open, and a project with no history has no units.
func TestStoreRangeBoundsAndMissingProject(t *testing.T) {
	s := newTestStore(t, t0.Add(10*time.Minute))
	for i := 0; i < 10; i++ {
		_ = s.Append("abc", sampleAt(t0.Add(time.Duration(i)*time.Minute), 1))
	}
	if us, _ := s.Range("abc", t0, t0.Add(3*time.Minute)); len(us) != 3 {
		t.Fatalf("[0,3) gave %d units, want 3", len(us))
	}
	if us, _ := s.Range("abc", t0.Add(3*time.Minute), t0.Add(3*time.Minute)); len(us) != 0 {
		t.Fatalf("an empty range gave %d units", len(us))
	}
	if us, err := s.Range("nothing", t0, t0.Add(time.Hour)); err != nil || len(us) != 0 {
		t.Fatalf("a project with no file: %d units, %v", len(us), err)
	}
	// A time past the clock is not served: those slots hold nothing yet.
	if us, _ := s.Range("abc", t0.Add(time.Hour), t0.Add(2*time.Hour)); len(us) != 0 {
		t.Fatalf("the future gave %d units", len(us))
	}
}

// After the minute ring has wrapped, the hours that left it are served as hourly rollups (the mean
// and the maximum of the minutes written in them), and the rest as minutes.
func TestStoreHourlyRollupsOutliveTheMinuteRing(t *testing.T) {
	end := t0.Add(97 * time.Hour)
	s := newTestStore(t, end)
	for h := 0; h < 97; h++ {
		for _, m := range []int{0, 10, 20} {
			at := t0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
			if err := s.Append("abc", sampleAt(at, float64(h*100+m))); err != nil {
				t.Fatal(err)
			}
		}
	}
	us, err := s.Range("abc", t0, end)
	if err != nil {
		t.Fatal(err)
	}
	// The minute ring holds 72 hours; with the clock at hour 97 it still has whole hours from 26 on.
	hourly, minutes := 0, 0
	for _, u := range us {
		if u.N == 1 {
			minutes++
			continue
		}
		h := int(u.At.Sub(t0) / time.Hour)
		if hourly != h {
			t.Fatalf("hourly unit %d is for hour %d", hourly, h)
		}
		hourly++
		if u.N != 3 || u.Mean[CPUBusy] != float64(h*100+10) || u.Max[CPUBusy] != float64(h*100+20) {
			t.Fatalf("hour %d: n=%d mean=%v max=%v", h, u.N, u.Mean[CPUBusy], u.Max[CPUBusy])
		}
	}
	if hourly != 26 || minutes != 71*3 {
		t.Fatalf("%d hourly units and %d minute units, want 26 and %d", hourly, minutes, 71*3)
	}
	// A short range far in the past is answered from the hours alone.
	us, _ = s.Range("abc", t0.Add(5*time.Hour), t0.Add(8*time.Hour))
	if len(us) != 3 || us[0].N != 3 {
		t.Fatalf("hours 5 to 8: %d units %+v", len(us), us)
	}
}

// The current hour's rollup is recomputed at every write, so it is right whenever the minutes
// leave the ring, and a missed minute leaves the others alone.
func TestStoreHourRecordCountsOnlyTheMinutesWritten(t *testing.T) {
	s := newTestStore(t, t0.Add(10*time.Hour))
	for _, m := range []int{5, 6, 9} { // minutes 7 and 8 were missed
		_ = s.Append("abc", sampleAt(t0.Add(time.Duration(m)*time.Minute), float64(m)))
	}
	// Age the store until the hour is served from the hour ring: the clock is past the minute ring.
	s.Now = func() time.Time { return t0.Add(5 * 24 * time.Hour) }
	us, err := s.Range("abc", t0, t0.Add(time.Hour))
	if err != nil || len(us) != 1 {
		t.Fatalf("%d units, %v", len(us), err)
	}
	if u := us[0]; u.N != 3 || math.Abs(u.Mean[CPUBusy]-(5+6+9)/3.0) > 1e-5 || u.Max[CPUBusy] != 9 {
		t.Fatalf("%+v", u)
	}
}

// A slot that a newer sample replaced is not served as the old minute: the hour it belonged to is
// served by its rollup, which still has both of its minutes.
func TestStoreRingReplacesTheOldestMinute(t *testing.T) {
	first := t0.Add(5 * time.Minute)
	later := first.Add(RawRetention) // the same slot, three days on
	s := newTestStore(t, later.Add(time.Minute))
	_ = s.Append("abc", sampleAt(first, 1))
	_ = s.Append("abc", sampleAt(first.Add(time.Minute), 3))
	_ = s.Append("abc", sampleAt(later, 2))
	us, _ := s.Range("abc", later, later.Add(time.Minute))
	if len(us) != 1 || us[0].N != 1 || us[0].Mean[CPUBusy] != 2 {
		t.Fatalf("%+v", us)
	}
	us, _ = s.Range("abc", t0, t0.Add(time.Hour))
	if len(us) != 1 || us[0].N != 2 || us[0].Mean[CPUBusy] != 2 || us[0].Max[CPUBusy] != 3 {
		t.Fatalf("the old hour: %+v", us)
	}
}

func TestStoreFilesHaveTheFixedSize(t *testing.T) {
	s := newTestStore(t, t0)
	if err := s.Append("abc", sampleAt(t0, 1)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "abc.ring"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(fileSize) || fi.Mode().Perm() != 0o600 {
		t.Fatalf("size %d mode %v, want %d 0600", fi.Size(), fi.Mode().Perm(), fileSize)
	}
	// About a mebibyte a project.
	if fileSize < 900<<10 || fileSize > 1200<<10 {
		t.Fatalf("a file is %d bytes, the docs say a little over 1 MiB", fileSize)
	}
}

// A file written by a build with another layout is started again, not misread.
func TestStoreRestartsAFileOfAnotherLayout(t *testing.T) {
	s := newTestStore(t, t0.Add(time.Hour))
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "abc.ring"), []byte("not a ring file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if us, err := s.Range("abc", t0, t0.Add(time.Hour)); err != nil || len(us) != 0 {
		t.Fatalf("a foreign file: %d units, %v", len(us), err)
	}
	if err := s.Append("abc", sampleAt(t0, 7)); err != nil {
		t.Fatal(err)
	}
	us, _ := s.Range("abc", t0, t0.Add(time.Minute))
	if len(us) != 1 || us[0].Mean[CPUBusy] != 7 {
		t.Fatalf("%+v", us)
	}
}

func TestStoreRefusesNamesThatAreNotRefs(t *testing.T) {
	s := newTestStore(t, t0)
	for _, ref := range []string{"", "../x", "a/b", ".hidden"} {
		if err := s.Append(ref, sampleAt(t0, 1)); err == nil {
			t.Errorf("Append(%q) was accepted", ref)
		}
	}
}

func TestStoreSweepRemovesDeletedProjectsOnly(t *testing.T) {
	s := newTestStore(t, t0)
	for _, ref := range []string{"keep", "gone"} {
		_ = s.Append(ref, sampleAt(t0, 1))
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := s.Sweep(func(ref string) bool { return ref == "keep" })
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	for name, want := range map[string]bool{"keep.ring": true, "gone.ring": false, "notes.txt": true} {
		if _, err := os.Stat(filepath.Join(s.Dir, name)); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
	if n, err := NewStore(filepath.Join(t.TempDir(), "none")).Sweep(func(string) bool { return false }); n != 0 || err != nil {
		t.Fatalf("a store with no directory: %d, %v", n, err)
	}
}

// Two samples of one minute (the daemon started again within it) are one: the second wins, except
// for a value it could not measure, which the first keeps.
func TestStoreMergesTwoSamplesOfOneMinute(t *testing.T) {
	s := newTestStore(t, t0.Add(time.Hour))
	first := sampleAt(t0, 40)
	first.Set(Backends, 5)
	second := NewSample(t0)
	second.Set(Backends, 9)
	second.Set(MemUsed, 123)
	for _, smp := range []Sample{first, second} {
		if err := s.Append("abc", smp); err != nil {
			t.Fatal(err)
		}
	}
	us, _ := s.Range("abc", t0, t0.Add(time.Minute))
	if len(us) != 1 {
		t.Fatalf("%d units", len(us))
	}
	u := us[0]
	if u.Mean[CPUBusy] != 40 || u.Mean[Backends] != 9 || u.Mean[MemUsed] != 123 {
		t.Fatalf("cpu %v backends %v mem %v, want 40 9 123", u.Mean[CPUBusy], u.Mean[Backends], u.Mean[MemUsed])
	}
}

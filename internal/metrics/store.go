package metrics

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The store keeps each project's samples in one file of fixed size, a pair of rings:
//
//   - the minute ring holds the last RawRetention of one-minute samples, a slot per minute (the
//     slot of Unix minute m is m mod RawSlots), which is where every chart of 12 hours or less
//     reads from, and
//   - the hour ring holds the last HourRetention of hourly rollups (mean and maximum of each
//     field), a slot per hour, which the longer ranges Studio offers read from (1h buckets up to 7
//     days, 1d buckets beyond).
//
// A record carries its own minute (or hour), so a slot that was never written or that a newer
// sample has not replaced yet is recognized and skipped. Nothing grows and nothing needs pruning:
// a write replaces the oldest record. A file is a little over 1 MiB, so 35 projects keep about
// 37 MiB of history on disk, in the state directory and not in the registry (nothing here is
// replicated to a standby or archived with the system cluster's WAL). The hour record of the current
// hour is recomputed from the minute ring at every write, which keeps it right across a restart of
// the daemon and a missed minute.
const (
	// RawRetention is how long one-minute samples are kept. Studio asks for minute buckets only for
	// ranges up to 12 hours (useReportDateRange.getIntervalGranularity).
	RawRetention = 3 * 24 * time.Hour
	// HourRetention is how long hourly rollups are kept: the 90 days Studio's date range limit
	// allows the plan Supavise reports (DATERANGE_LIMITS.enterprise).
	HourRetention = 92 * 24 * time.Hour

	// RawSlots and HourSlots are the ring sizes. RawSlots is a multiple of 60, so an hour's
	// minutes are neighbours in the ring and never straddle its end.
	RawSlots  = int(RawRetention / time.Minute)
	HourSlots = int(HourRetention / time.Hour)

	storeMagic    = "SVMT"
	storeVersion  = 1
	headerSize    = 64
	rawRecSize    = 8 + 4*int(NumFields)
	hourRecSize   = 16 + 8*int(NumFields)
	rawRegionOff  = headerSize
	hourRegionOff = rawRegionOff + RawSlots*rawRecSize
	fileSize      = hourRegionOff + HourSlots*hourRecSize
)

// Store is the set of files under Dir, one per project. It is safe for concurrent use.
type Store struct {
	// Dir is the directory the files live in (<state_dir>/system/metrics).
	Dir string
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu sync.RWMutex
}

// NewStore returns a store in dir. The directory is created when the first sample is written.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) path(ref string) (string, error) {
	if ref == "" || strings.ContainsAny(ref, `/\`) || strings.HasPrefix(ref, ".") {
		return "", fmt.Errorf("metrics: %q is not a project ref", ref)
	}
	return filepath.Join(s.Dir, ref+".ring"), nil
}

func header() []byte {
	h := make([]byte, headerSize)
	copy(h, storeMagic)
	binary.LittleEndian.PutUint32(h[4:], storeVersion)
	binary.LittleEndian.PutUint32(h[8:], uint32(NumFields))
	binary.LittleEndian.PutUint32(h[12:], uint32(RawSlots))
	binary.LittleEndian.PutUint32(h[16:], uint32(HourSlots))
	return h
}

// open opens ref's file for reading and writing. It creates it, or starts it again when the layout
// on disk is not this build's, when create is set; otherwise a missing or foreign file is
// (nil, nil).
func (s *Store) open(ref string, create bool) (*os.File, error) {
	p, err := s.path(ref)
	if err != nil {
		return nil, err
	}
	flag := os.O_RDWR
	if create {
		if err := os.MkdirAll(s.Dir, 0o750); err != nil {
			return nil, err
		}
		flag |= os.O_CREATE
	}
	f, err := os.OpenFile(p, flag, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !create {
			return nil, nil
		}
		return nil, err
	}
	want := header()
	got := make([]byte, headerSize)
	n, rerr := f.ReadAt(got, 0)
	if rerr != nil && rerr != io.EOF {
		f.Close()
		return nil, rerr
	}
	if n == headerSize && bytes.Equal(got, want) {
		if fi, err := f.Stat(); err == nil && fi.Size() == int64(fileSize) {
			return f, nil
		}
	}
	if !create {
		f.Close()
		return nil, nil
	}
	// A new file, or one written by a build with another layout: start it again.
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Truncate(int64(fileSize)); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteAt(want, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func putRaw(b []byte, minute int64, v *[NumFields]float64) {
	binary.LittleEndian.PutUint64(b, uint64(minute))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[8+4*i:], math.Float32bits(float32(x)))
	}
}

func getRaw(b []byte) (minute int64, v [NumFields]float64) {
	minute = int64(binary.LittleEndian.Uint64(b))
	for i := range v {
		v[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[8+4*i:])))
	}
	return minute, v
}

func slotOf(n int64, slots int) int { return int(((n % int64(slots)) + int64(slots)) % int64(slots)) }

// Append records the sample of one project, and brings the rollup of its hour up to date.
func (s *Store) Append(ref string, smp Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.open(ref, true)
	if err != nil {
		return err
	}
	defer f.Close()

	m := minuteOf(smp.At)
	rec := make([]byte, rawRecSize)
	off := int64(rawRegionOff + slotOf(m, RawSlots)*rawRecSize)
	// A second sample of the same minute (the daemon started again within it) keeps the values the
	// first one measured and this one could not, the rates above all.
	old := make([]byte, rawRecSize)
	if n, err := f.ReadAt(old, off); err == nil || (err == io.EOF && n == len(old)) {
		if om, ov := getRaw(old); om == m {
			for i := range smp.V {
				if math.IsNaN(smp.V[i]) && !math.IsNaN(ov[i]) {
					smp.V[i] = ov[i]
				}
			}
		}
	}
	putRaw(rec, m, &smp.V)
	if _, err := f.WriteAt(rec, off); err != nil {
		return err
	}

	// The hour: the valid minutes of it in the ring.
	h := floorDiv(m, 60)
	first := slotOf(h*60, RawSlots)
	buf := make([]byte, 60*rawRecSize)
	if n, err := f.ReadAt(buf, int64(rawRegionOff+first*rawRecSize)); err != nil && err != io.EOF {
		return err
	} else if n < len(buf) {
		clear(buf[n:])
	}
	var sum [NumFields]float64
	var cnt [NumFields]int
	var peak [NumFields]float64
	for i := range peak {
		peak[i] = math.NaN()
	}
	n := 0
	for i := 0; i < 60; i++ {
		minute, v := getRaw(buf[i*rawRecSize:])
		if minute != h*60+int64(i) {
			continue
		}
		n++
		for j, x := range v {
			if math.IsNaN(x) {
				continue
			}
			sum[j] += x
			cnt[j]++
			if math.IsNaN(peak[j]) || x > peak[j] {
				peak[j] = x
			}
		}
	}
	hr := make([]byte, hourRecSize)
	binary.LittleEndian.PutUint64(hr, uint64(h))
	binary.LittleEndian.PutUint32(hr[8:], uint32(n))
	for j := range sum {
		mean := math.NaN()
		if cnt[j] > 0 {
			mean = sum[j] / float64(cnt[j])
		}
		binary.LittleEndian.PutUint32(hr[16+4*j:], math.Float32bits(float32(mean)))
		binary.LittleEndian.PutUint32(hr[16+4*int(NumFields)+4*j:], math.Float32bits(float32(peak[j])))
	}
	_, err = f.WriteAt(hr, int64(hourRegionOff+slotOf(h, HourSlots)*hourRecSize))
	return err
}

// Unit is what the store hands the query: one minute sample (N 1, Mean and Max equal) or one
// hourly rollup (N the minutes it stands for), starting at At.
type Unit struct {
	At   time.Time
	N    int
	Mean [NumFields]float64
	Max  [NumFields]float64
}

// Range returns the units that overlap [from, to), oldest first: minute samples for the part of the
// range the minute ring still holds in whole hours, hourly rollups for the part before it. A ref
// with no file has no units.
func (s *Store) Range(ref string, from, to time.Time) ([]Unit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, err := s.open(ref, false)
	if err != nil || f == nil {
		return nil, err
	}
	defer f.Close()
	if !to.After(from) {
		return nil, nil
	}

	nowMin := minuteOf(s.now())
	// Minutes from cut on are served from the minute ring (its oldest hour may be half gone, so the
	// cut is the first whole hour it still holds); hours before it from the hour ring.
	cut := ceilDiv(nowMin-int64(RawSlots)+1, 60) * 60
	fromMin, toMin := minuteOf(from), ceilDiv(to.Unix(), 60) // [fromMin, toMin)
	var out []Unit

	// Hourly rollups of the hours that end at or before the cut.
	hFrom, hTo := floorDiv(fromMin, 60), min(ceilDiv(toMin, 60), cut/60) // [hFrom, hTo)
	hFrom = max(hFrom, cut/60-int64(HourSlots))
	if hTo > hFrom {
		recs, err := readRing(f, hourRegionOff, hourRecSize, HourSlots, hFrom, int(hTo-hFrom))
		if err != nil {
			return nil, err
		}
		for i, rec := range recs {
			h := int64(binary.LittleEndian.Uint64(rec))
			n := int(binary.LittleEndian.Uint32(rec[8:]))
			if h != hFrom+int64(i) || n == 0 {
				continue
			}
			u := Unit{At: time.Unix(h*3600, 0).UTC(), N: n}
			for j := 0; j < int(NumFields); j++ {
				u.Mean[j] = float64(math.Float32frombits(binary.LittleEndian.Uint32(rec[16+4*j:])))
				u.Max[j] = float64(math.Float32frombits(binary.LittleEndian.Uint32(rec[16+4*int(NumFields)+4*j:])))
			}
			out = append(out, u)
		}
	}

	// Minute samples from the cut on.
	mFrom, mTo := max(fromMin, cut), min(toMin, nowMin+1)
	if mTo > mFrom {
		recs, err := readRing(f, rawRegionOff, rawRecSize, RawSlots, mFrom, int(mTo-mFrom))
		if err != nil {
			return nil, err
		}
		for i, rec := range recs {
			minute, v := getRaw(rec)
			if minute != mFrom+int64(i) {
				continue
			}
			out = append(out, Unit{At: time.Unix(minute*60, 0).UTC(), N: 1, Mean: v, Max: v})
		}
	}
	return out, nil
}

// readRing reads count consecutive records of a ring, the first being number first (a minute or an
// hour), as one slice per record. A record beyond the end of the file reads as zeros.
func readRing(f *os.File, regionOff, recSize, slots int, first int64, count int) ([][]byte, error) {
	count = min(count, slots)
	buf := make([]byte, count*recSize)
	start := slotOf(first, slots)
	n1 := min(count, slots-start)
	if err := readFull(f, buf[:n1*recSize], int64(regionOff+start*recSize)); err != nil {
		return nil, err
	}
	if n1 < count {
		if err := readFull(f, buf[n1*recSize:], int64(regionOff)); err != nil {
			return nil, err
		}
	}
	out := make([][]byte, count)
	for i := range out {
		out[i] = buf[i*recSize : (i+1)*recSize]
	}
	return out, nil
}

func readFull(f *os.File, b []byte, off int64) error {
	n, err := f.ReadAt(b, off)
	if err != nil && err != io.EOF {
		return err
	}
	clear(b[n:])
	return nil
}

func ceilDiv(a, b int64) int64 { return -floorDiv(-a, b) }

// Forget deletes ref's history.
func (s *Store) Forget(ref string) error {
	p, err := s.path(ref)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Sweep deletes the files of projects keep does not name (deleted projects) and returns how many.
// A file that is not a ring file of this store is left alone.
func (s *Store) Sweep(keep func(ref string) bool) (int, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range ents {
		ref, ok := strings.CutSuffix(e.Name(), ".ring")
		if !ok || e.IsDir() || keep(ref) {
			continue
		}
		if err := s.Forget(ref); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

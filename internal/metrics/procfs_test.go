package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProc(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const meminfoFixture = `MemTotal:        8000000 kB
MemFree:          500000 kB
MemAvailable:    2000000 kB
Buffers:          100000 kB
SwapTotal:       1000000 kB
SwapFree:         900000 kB
Committed_AS:   12000000 kB
`

func TestProcMeminfo(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "meminfo", meminfoFixture)
	m, err := Proc{Root: root}.Meminfo()
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 8000000*1024 || m.Available != 2000000*1024 || m.SwapTotal != 1000000*1024 || m.SwapFree != 900000*1024 {
		t.Fatalf("%+v", m)
	}
	if got := m.UsedPercent(); got != 75 {
		t.Fatalf("used percent = %v, want 75", got)
	}
}

func TestProcMeminfoWithoutMemAvailableIsAnError(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "meminfo", "MemTotal: 8000000 kB\nMemFree: 1 kB\n")
	if _, err := (Proc{Root: root}).Meminfo(); err == nil {
		t.Fatal("a meminfo with no MemAvailable was accepted")
	}
}

func TestProcMemoryPressure(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "pressure/memory", "some avg10=1.50 avg60=12.25 avg300=3.00 total=123456\nfull avg10=0.50 avg60=4.00 avg300=1.00 total=654\n")
	p, err := Proc{Root: root}.MemoryPressure()
	if err != nil || !p.OK {
		t.Fatalf("%+v %v", p, err)
	}
	if p.SomeAvg10 != 1.5 || p.SomeAvg60 != 12.25 || p.SomeAvg300 != 3 || p.FullAvg10 != 0.5 || p.FullAvg60 != 4 || p.FullAvg300 != 1 {
		t.Fatalf("%+v", p)
	}
}

// A kernel without pressure stall information has no such file: no error, and OK is false.
func TestProcMemoryPressureOnAKernelWithoutPSI(t *testing.T) {
	p, err := Proc{Root: t.TempDir()}.MemoryPressure()
	if err != nil || p.OK {
		t.Fatalf("%+v %v", p, err)
	}
}

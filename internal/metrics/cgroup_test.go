package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

// unitFiles are the contents of a fake control group.
type unitFiles struct {
	user, system, throttled uint64 // microseconds
	anon, shmem, file       uint64
	current, swap           uint64
	oomKill                 uint64
	memMax                  string // "max" or a number; empty writes no file
	noSwapFile              bool
}

// writeUnit lays out the control group of unit under a fake cgroup root, the way the kernel
// shows it: flat key files and single-number files.
func writeUnit(t *testing.T, root, unit string, f unitFiles) {
	t.Helper()
	dir := filepath.Join(root, config.Slice, unit)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("cpu.stat", fmt.Sprintf("usage_usec %d\nuser_usec %d\nsystem_usec %d\nnice_usec 0\nnr_periods 100\nnr_throttled 3\nthrottled_usec %d\nnr_bursts 0\nburst_usec 0\n",
		f.user+f.system, f.user, f.system, f.throttled))
	put("memory.stat", fmt.Sprintf("anon %d\nfile %d\nkernel_stack 16384\nshmem %d\nfile_mapped 4096\nslab 8192\n", f.anon, f.file, f.shmem))
	put("memory.current", fmt.Sprintf("%d\n", f.current))
	put("memory.events", fmt.Sprintf("low 0\nhigh 0\nmax 2\noom 1\noom_kill %d\noom_group_kill 0\n", f.oomKill))
	if !f.noSwapFile {
		put("memory.swap.current", fmt.Sprintf("%d\n", f.swap))
	}
	if f.memMax != "" {
		put("memory.max", f.memMax+"\n")
	}
}

func TestCgroupReadsAUnit(t *testing.T) {
	root := t.TempDir()
	writeUnit(t, root, "supavise-postgres@abc.service", unitFiles{
		user: 30_000_000, system: 6_000_000, throttled: 1_500_000,
		anon: 300 << 20, shmem: 200 << 20, file: 500 << 20, current: 900 << 20, swap: 4 << 20, oomKill: 2, memMax: "1073741824",
	})
	u, err := Cgroup{Root: root}.ReadUnit("supavise-postgres@abc.service")
	if err != nil {
		t.Fatal(err)
	}
	want := UnitStats{
		CPUUsageUsec: 36_000_000, CPUUserUsec: 30_000_000, CPUSystemUsec: 6_000_000, ThrottledUsec: 1_500_000,
		MemCurrent: 900 << 20, Anon: 300 << 20, Shmem: 200 << 20, File: 500 << 20, Swap: 4 << 20, OOMKill: 2, MemMax: 1 << 30,
	}
	if u != want {
		t.Fatalf("got %+v\nwant %+v", u, want)
	}
}

// memory.max says "max" for a group with no limit, and a kernel without swap accounting has no
// memory.swap.current: both read as zero.
func TestCgroupToleratesWhatTheKernelOmits(t *testing.T) {
	root := t.TempDir()
	writeUnit(t, root, "supavise-gotrue@abc.service", unitFiles{anon: 1, memMax: "max", noSwapFile: true})
	u, err := Cgroup{Root: root}.ReadUnit("supavise-gotrue@abc.service")
	if err != nil || u.MemMax != 0 || u.Swap != 0 || u.Anon != 1 {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestCgroupOfAUnitThatIsNotRunning(t *testing.T) {
	if _, err := (Cgroup{Root: t.TempDir()}).ReadUnit("supavise-postgres@abc.service"); !errors.Is(err, ErrNoUnit) {
		t.Fatalf("err = %v, want ErrNoUnit", err)
	}
}

// A group whose cpu controller is off has no cpu.stat: the unit is running, and its CPU reads zero.
func TestCgroupWithoutTheCPUController(t *testing.T) {
	root := t.TempDir()
	writeUnit(t, root, "supavise-postgrest@abc.service", unitFiles{anon: 5})
	if err := os.Remove(filepath.Join(root, config.Slice, "supavise-postgrest@abc.service", "cpu.stat")); err != nil {
		t.Fatal(err)
	}
	u, err := Cgroup{Root: root}.ReadUnit("supavise-postgrest@abc.service")
	if err != nil || u.CPUUsageUsec != 0 || u.Anon != 5 {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestProjectUnitsAreTheTemplateInstances(t *testing.T) {
	got := ProjectUnits("abcdefghijklmnopqrst")
	if got[config.SvcPostgres] != "supavise-postgres@abcdefghijklmnopqrst.service" || len(got) != 3 ||
		got[config.SvcGoTrue] != "supavise-gotrue@abcdefghijklmnopqrst.service" || got[config.SvcPostgREST] != "supavise-postgrest@abcdefghijklmnopqrst.service" {
		t.Fatalf("units = %v", got)
	}
}

// removeUnit deletes a fake unit's control group, as the kernel does when the unit stops.
func removeUnit(root, unit string) error {
	return os.RemoveAll(filepath.Join(root, config.Slice, unit))
}

package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/supavise/supavise/internal/config"
)

// DefaultCgroupRoot is where the unified (v2) cgroup hierarchy is mounted. Tests read a tree of
// their own through Cgroup.Root.
const DefaultCgroupRoot = "/sys/fs/cgroup"

// Cgroup reads the control group files of the units that run in supavise.slice. The files are
// world-readable, so the daemon needs no privilege for them.
type Cgroup struct {
	// Root is the mount point of the hierarchy; empty means DefaultCgroupRoot.
	Root string
}

// UnitStats are the counters and gauges of one unit's control group. Memory values are bytes and
// CPU values microseconds, as the kernel reports them; the counters only grow while the unit runs
// and start again from zero when it restarts.
type UnitStats struct {
	CPUUsageUsec, CPUUserUsec, CPUSystemUsec uint64
	// ThrottledUsec is how long the group's threads waited for the CPU quota.
	ThrottledUsec uint64
	// MemCurrent is memory.current; Anon, Shmem and File are the like-named lines of memory.stat
	// (File includes Shmem); Swap is memory.swap.current.
	MemCurrent, Anon, Shmem, File, Swap uint64
	// OOMKill is the oom_kill line of memory.events.
	OOMKill uint64
	// MemMax is memory.max; 0 means "max" (no limit).
	MemMax uint64
	// ID names this control group: the inode of its directory, which a restarted unit gets anew
	// because systemd removes the group when the unit stops. 0 when the filesystem has no inode.
	ID uint64
}

// Restarted reports whether u, read after prev, is a different run of the unit: another control
// group, or counters that went backward, which only a group that started over from zero shows.
func (u UnitStats) Restarted(prev UnitStats) bool {
	if u.ID != 0 && prev.ID != 0 && u.ID != prev.ID {
		return true
	}
	return u.CPUUsageUsec < prev.CPUUsageUsec || u.OOMKill < prev.OOMKill
}

// ErrNoUnit says the unit has no control group: it is not running.
var ErrNoUnit = errors.New("metrics: the unit has no control group")

func (c Cgroup) dir(unit string) string {
	root := c.Root
	if root == "" {
		root = DefaultCgroupRoot
	}
	return filepath.Join(root, config.Slice, unit)
}

// ReadUnit reads the control group of unit (a unit name such as supavise-postgres@<ref>.service).
// A file the kernel does not provide (swap accounting is off, a controller is not enabled) reads
// as zero; a group that does not exist is ErrNoUnit.
func (c Cgroup) ReadUnit(unit string) (UnitStats, error) {
	dir := c.dir(unit)
	var u UnitStats
	if fi, err := os.Stat(dir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			u.ID = uint64(st.Ino)
		}
	}
	cpu, err := readFlat(filepath.Join(dir, "cpu.stat"))
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(dir); serr != nil {
			return u, fmt.Errorf("%w: %s", ErrNoUnit, unit)
		}
		err = nil // the group exists and the cpu controller is off: CPU reads as zero
	}
	if err != nil {
		return u, err
	}
	u.CPUUsageUsec, u.CPUUserUsec, u.CPUSystemUsec = cpu["usage_usec"], cpu["user_usec"], cpu["system_usec"]
	u.ThrottledUsec = cpu["throttled_usec"]

	stat, err := readFlat(filepath.Join(dir, "memory.stat"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return u, err
	}
	u.Anon, u.Shmem, u.File = stat["anon"], stat["shmem"], stat["file"]

	events, err := readFlat(filepath.Join(dir, "memory.events"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return u, err
	}
	u.OOMKill = events["oom_kill"]

	for file, dst := range map[string]*uint64{"memory.current": &u.MemCurrent, "memory.swap.current": &u.Swap} {
		v, err := readUint(filepath.Join(dir, file))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return u, err
		}
		*dst = v
	}
	if b, err := os.ReadFile(filepath.Join(dir, "memory.max")); err == nil {
		if s := strings.TrimSpace(string(b)); s != "max" {
			if v, perr := strconv.ParseUint(s, 10, 64); perr == nil {
				u.MemMax = v
			}
		}
	}
	return u, nil
}

// SliceOOMKills is the oom_kill line of supavise.slice's memory.events: the kernel counts the
// kills in every group below the slice there, and keeps them when a group is removed, which the
// count of a unit that restarted does not. ok is false when the file cannot be read.
func (c Cgroup) SliceOOMKills() (n uint64, ok bool) {
	root := c.Root
	if root == "" {
		root = DefaultCgroupRoot
	}
	events, err := readFlat(filepath.Join(root, config.Slice, "memory.events"))
	if err != nil {
		return 0, false
	}
	n, ok = events["oom_kill"]
	return n, ok
}

// SliceExists reports whether the control group of supavise.slice is there. It is not on a host
// without the unified (v2) hierarchy, which no chart can be drawn from.
func (c Cgroup) SliceExists() bool {
	root := c.Root
	if root == "" {
		root = DefaultCgroupRoot
	}
	fi, err := os.Stat(filepath.Join(root, config.Slice))
	return err == nil && fi.IsDir()
}

// ProjectUnits are the units of a project that count toward its utilization, by service.
func ProjectUnits(ref string) map[string]string {
	m := make(map[string]string, len(config.ProjectServices))
	for _, svc := range config.ProjectServices {
		m[svc] = config.UnitName(svc, ref)
	}
	return m
}

// readFlat reads a file of "key value" lines (cpu.stat, memory.stat, memory.events). A line that
// is not a key and a number is skipped.
func readFlat(path string) (map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out, sc.Err()
}

// readUint reads a file that holds one number.
func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

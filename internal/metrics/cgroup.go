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

package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultProcRoot is where procfs is mounted. Tests read a tree of their own through Proc.Root.
const DefaultProcRoot = "/proc"

// Proc reads the node-wide files of procfs.
type Proc struct {
	// Root is the mount point; empty means DefaultProcRoot.
	Root string
}

func (p Proc) path(rel string) string {
	root := p.Root
	if root == "" {
		root = DefaultProcRoot
	}
	return filepath.Join(root, rel)
}

// Meminfo is the part of /proc/meminfo the node's memory judgement uses, in bytes.
type Meminfo struct {
	Total, Available, SwapTotal, SwapFree uint64
}

// UsedPercent is the share of the memory that is not available, in percent.
func (m Meminfo) UsedPercent() float64 {
	if m.Total == 0 {
		return 0
	}
	return 100 * float64(m.Total-min(m.Available, m.Total)) / float64(m.Total)
}

// Meminfo reads /proc/meminfo. MemAvailable is the kernel's estimate of what can be given to a
// program without swapping: free memory plus the page cache and slab it can drop.
func (p Proc) Meminfo() (Meminfo, error) {
	f, err := os.Open(p.path("meminfo"))
	if err != nil {
		return Meminfo{}, err
	}
	defer f.Close()
	var m Meminfo
	var have int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch k {
		case "MemTotal":
			dst = &m.Total
		case "MemAvailable":
			dst = &m.Available
		case "SwapTotal":
			dst = &m.SwapTotal
		case "SwapFree":
			dst = &m.SwapFree
		default:
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			n *= 1024
		}
		*dst = n
		have++
	}
	if err := sc.Err(); err != nil {
		return Meminfo{}, err
	}
	if m.Total == 0 || have < 2 {
		return Meminfo{}, errors.New("metrics: /proc/meminfo has no MemTotal and MemAvailable")
	}
	return m, nil
}

// Pressure is the memory pressure stall information of /proc/pressure/memory: the share of the
// last 10, 60 and 300 seconds in which some task (Some) or every runnable task (Full) waited for
// memory, in percent. OK is false where the kernel has no PSI (it is off, or the kernel is older
// than 4.20).
type Pressure struct {
	SomeAvg10, SomeAvg60, SomeAvg300 float64
	FullAvg10, FullAvg60, FullAvg300 float64
	OK                               bool
}

// MemoryPressure reads /proc/pressure/memory. A node without PSI answers with OK false and no error.
func (p Proc) MemoryPressure() (Pressure, error) {
	b, err := os.ReadFile(p.path("pressure/memory"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return Pressure{}, nil
		}
		return Pressure{}, err
	}
	var out Pressure
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var a10, a60, a300 *float64
		switch fields[0] {
		case "some":
			a10, a60, a300 = &out.SomeAvg10, &out.SomeAvg60, &out.SomeAvg300
		case "full":
			a10, a60, a300 = &out.FullAvg10, &out.FullAvg60, &out.FullAvg300
		default:
			continue
		}
		for _, kv := range fields[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return Pressure{}, fmt.Errorf("metrics: bad pressure line %q", line)
			}
			switch k {
			case "avg10":
				*a10 = n
			case "avg60":
				*a60 = n
			case "avg300":
				*a300 = n
			}
		}
		out.OK = true
	}
	return out, nil
}

package metrics

import (
	"context"
	"sync"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/diskquota"
	"github.com/supavise/supavise/internal/registry"
)

// DiskUse is a project's disk use against what it may use.
type DiskUse struct {
	Used, Size int64
	// Quota is true when Size is an XFS project quota in force: a write past it fails with ENOSPC
	// for that project alone. Otherwise Size is the volume's size, which the projects share.
	Quota bool
}

// DiskSource reads a project's disk use.
type DiskSource interface {
	Usage(ctx context.Context, p *registry.Project) (DiskUse, error)
}

// DiskLimits is the part of diskquota.Manager that NodeDisk needs.
type DiskLimits interface {
	Volume() (diskquota.Volume, error)
	Limit(ref string) (gb int, ok bool)
	Used(ref string) int64
}

// NodeDisk reads the disk use of the projects of one node.
//
// Where the project's XFS quota is in force, the kernel reports it through statfs on the project's
// directory: the filesystem's size and free space are replaced by the quota's limit and what is left
// of it (a directory with the project-inherit flag, which `xfs_quota project -s` sets, on a
// filesystem that enforces project quotas). That is block-accurate, costs one system call and needs
// no privilege, so it is read at every sample. It is trusted only when the size it reports is the
// limit the daemon set; otherwise (a quota that is not applied yet, a filesystem that behaves
// differently) the project's files are summed, at most every WalkEvery. Without a quota in force
// the files are summed too, against the volume's size.
type NodeDisk struct {
	Cfg    *config.Config
	Limits DiskLimits
	// Statfs reads the filesystem numbers of a path: blocks, free blocks and the block size. Nil
	// means syscall.Statfs; tests replace it.
	Statfs func(path string) (blocks, free, bsize int64, err error)
	// WalkEvery is how often a project's files are summed; zero means 5 minutes.
	WalkEvery time.Duration
	Now       func() time.Time

	mu    sync.Mutex
	walks map[string]walk
	vol   diskquota.Volume
	volAt time.Time
	volOK bool
}

type walk struct {
	at    time.Time
	bytes int64
}

func (n *NodeDisk) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *NodeDisk) statfs(path string) (int64, int64, int64, error) {
	if n.Statfs != nil {
		return n.Statfs(path)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	return int64(st.Blocks), int64(st.Bfree), int64(st.Bsize), nil
}

// volume is the data volume, inspected at most every five minutes (it reads /proc/self/mountinfo).
func (n *NodeDisk) volume() (diskquota.Volume, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.volOK && n.now().Sub(n.volAt) < 5*time.Minute {
		return n.vol, nil
	}
	v, err := n.Limits.Volume()
	if err != nil {
		return diskquota.Volume{}, err
	}
	n.vol, n.volAt, n.volOK = v, n.now(), true
	return v, nil
}

// files is the bytes in the project's files, summed at most every WalkEvery.
func (n *NodeDisk) files(ref string) int64 {
	every := n.WalkEvery
	if every <= 0 {
		every = 5 * time.Minute
	}
	n.mu.Lock()
	w, ok := n.walks[ref]
	n.mu.Unlock()
	if ok && n.now().Sub(w.at) < every {
		return w.bytes
	}
	b := n.Limits.Used(ref)
	n.mu.Lock()
	if n.walks == nil {
		n.walks = map[string]walk{}
	}
	n.walks[ref] = walk{at: n.now(), bytes: b}
	n.mu.Unlock()
	return b
}

// Usage implements DiskSource.
func (n *NodeDisk) Usage(_ context.Context, p *registry.Project) (DiskUse, error) {
	vol, err := n.volume()
	if err != nil {
		return DiskUse{}, err
	}
	if gb, ok := n.Limits.Limit(p.Ref); ok && vol.Enforceable() {
		size := int64(gb) << 30
		if blocks, free, bsize, err := n.statfs(n.Cfg.Paths().Project(p.Ref)); err == nil && bsize > 0 &&
			abs64(blocks*bsize-size) <= bsize {
			return DiskUse{Used: max(blocks-free, 0) * bsize, Size: size, Quota: true}, nil
		}
		return DiskUse{Used: n.files(p.Ref), Size: size, Quota: true}, nil
	}
	return DiskUse{Used: n.files(p.Ref), Size: vol.TotalBytes}, nil
}

// Forget drops what is cached for a project that is gone.
func (n *NodeDisk) Forget(ref string) {
	n.mu.Lock()
	delete(n.walks, ref)
	n.mu.Unlock()
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

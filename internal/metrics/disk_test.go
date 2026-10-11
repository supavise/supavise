package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/diskquota"
	"github.com/supavise/supavise/internal/registry"
)

type fakeLimits struct {
	vol    diskquota.Volume
	limits map[string]int
	files  int64
	walks  int
}

func (f *fakeLimits) Volume() (diskquota.Volume, error) { return f.vol, nil }
func (f *fakeLimits) Limit(ref string) (int, bool)      { gb, ok := f.limits[ref]; return gb, ok }
func (f *fakeLimits) Used(string) int64                 { f.walks++; return f.files }

var xfsVolume = diskquota.Volume{Mount: "/var/lib/supavise", FSType: "xfs", Options: []string{"rw", "prjquota"}, TotalBytes: 100 << 30, FreeBytes: 60 << 30}

func newDisk(f *fakeLimits, statfs func(string) (int64, int64, int64, error)) (*NodeDisk, *time.Time) {
	now := t0
	cfg := config.Default()
	cfg.StateDir = "/var/lib/supavise"
	return &NodeDisk{Cfg: cfg, Limits: f, Statfs: statfs, Now: func() time.Time { return now }}, &now
}

// Where the quota is in force the kernel reports it through statfs on the project's directory: the
// size is the limit and the free space what is left of it.
func TestNodeDiskReadsTheQuotaThroughStatfs(t *testing.T) {
	f := &fakeLimits{vol: xfsVolume, limits: map[string]int{"p": 10}, files: 1}
	var asked string
	d, _ := newDisk(f, func(path string) (int64, int64, int64, error) {
		asked = path
		return (10 << 30) / 4096, ((10 << 30) - (3 << 30)) / 4096, 4096, nil // 3 GiB of 10 used
	})
	use, err := d.Usage(context.Background(), &registry.Project{Ref: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if use != (DiskUse{Used: 3 << 30, Size: 10 << 30, Quota: true}) || asked != "/var/lib/supavise/projects/p" || f.walks != 0 {
		t.Fatalf("%+v asked %q walks %d", use, asked, f.walks)
	}
}

// A statfs that reports the whole filesystem (the quota is not applied to the directory yet) is not
// the quota's: the files are summed against the limit instead.
func TestNodeDiskDistrustsStatfsThatIsNotTheQuota(t *testing.T) {
	f := &fakeLimits{vol: xfsVolume, limits: map[string]int{"p": 10}, files: 2 << 30}
	d, _ := newDisk(f, func(string) (int64, int64, int64, error) { return (100 << 30) / 4096, (60 << 30) / 4096, 4096, nil })
	use, _ := d.Usage(context.Background(), &registry.Project{Ref: "p"})
	if use != (DiskUse{Used: 2 << 30, Size: 10 << 30, Quota: true}) || f.walks != 1 {
		t.Fatalf("%+v walks %d", use, f.walks)
	}
	d, _ = newDisk(f, func(string) (int64, int64, int64, error) { return 0, 0, 0, errors.New("no such directory") })
	if use, _ = d.Usage(context.Background(), &registry.Project{Ref: "p"}); !use.Quota || use.Used != 2<<30 {
		t.Fatalf("after a statfs error: %+v", use)
	}
}

// Without a quota in force the project is measured against the volume, which it shares.
func TestNodeDiskWithoutAQuota(t *testing.T) {
	ext4 := diskquota.Volume{Mount: "/", FSType: "ext4", TotalBytes: 50 << 30, FreeBytes: 20 << 30}
	f := &fakeLimits{vol: ext4, limits: map[string]int{"p": 10}, files: 5 << 30}
	d, _ := newDisk(f, func(string) (int64, int64, int64, error) {
		t.Fatal("statfs is for a quota in force")
		return 0, 0, 0, nil
	})
	use, _ := d.Usage(context.Background(), &registry.Project{Ref: "p"})
	if use != (DiskUse{Used: 5 << 30, Size: 50 << 30}) {
		t.Fatalf("%+v", use)
	}
	// A project with no limit set on an XFS volume is in the same position.
	f = &fakeLimits{vol: xfsVolume, files: 1}
	d, _ = newDisk(f, nil)
	if use, _ = d.Usage(context.Background(), &registry.Project{Ref: "q"}); use.Quota || use.Size != 100<<30 {
		t.Fatalf("%+v", use)
	}
}

// The files are summed at most every five minutes, whatever the number of samples.
func TestNodeDiskSumsFilesRarely(t *testing.T) {
	f := &fakeLimits{vol: diskquota.Volume{FSType: "ext4", TotalBytes: 1 << 30}, files: 7}
	d, now := newDisk(f, nil)
	for i := 0; i < 4; i++ {
		*now = now.Add(time.Minute)
		_, _ = d.Usage(context.Background(), &registry.Project{Ref: "p"})
	}
	if f.walks != 1 {
		t.Fatalf("%d walks in 4 minutes, want 1", f.walks)
	}
	*now = now.Add(5 * time.Minute)
	f.files = 9
	if use, _ := d.Usage(context.Background(), &registry.Project{Ref: "p"}); use.Used != 9 || f.walks != 2 {
		t.Fatalf("%+v walks %d", use, f.walks)
	}
	d.Forget("p")
	_, _ = d.Usage(context.Background(), &registry.Project{Ref: "p"})
	if f.walks != 3 {
		t.Fatalf("a forgotten project is not walked again: %d", f.walks)
	}
}

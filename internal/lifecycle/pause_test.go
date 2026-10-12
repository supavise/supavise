package lifecycle

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

func pauseOf(t *testing.T, h *harness, ref string) *registry.Pause {
	t.Helper()
	p, err := h.reg.GetPause(context.Background(), ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A pause records who made it before the project is INACTIVE, a resume forgets it, and only the
// node's own pause says "idle".
func TestPauseRecordsWhyAndResumeForgetsIt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.create(t)

	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	got := pauseOf(t, h, p.Ref)
	if got == nil || got.Reason != "" || got.PausedAt.IsZero() {
		t.Fatalf("the owner's pause = %+v", got)
	}
	if cur, _ := h.reg.GetProject(ctx, p.Ref); cur.Status != registry.StatusInactive {
		t.Fatalf("status = %s", cur.Status)
	}
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if pauseOf(t, h, p.Ref) != nil {
		t.Fatal("a resumed project still has a pause record")
	}

	if err := h.e.PauseWith(ctx, p.Ref, PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	if got := pauseOf(t, h, p.Ref); got == nil || got.Reason != registry.PauseIdle {
		t.Fatalf("the idle pause = %+v", got)
	}
	// The owner pauses it again by hand (it cannot be paused twice, so wake it first, then pause).
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if got := pauseOf(t, h, p.Ref); got == nil || got.Reason != "" {
		t.Fatalf("a pause after an idle pause = %+v", got)
	}
}

// A pause whose units would not stop leaves the project running and leaves no record behind.
func TestFailedPauseLeavesNoRecord(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.create(t)
	h.plane.failOn["Stop"] = errors.New("postgres would not stop")
	if err := h.e.PauseWith(ctx, p.Ref, PauseOptions{Reason: registry.PauseIdle}); err == nil {
		t.Fatal("expected the stop failure")
	}
	if pauseOf(t, h, p.Ref) != nil {
		t.Fatal("a failed pause left a record that says idle")
	}
	if cur, _ := h.reg.GetProject(ctx, p.Ref); cur.Status.Running() == false {
		t.Fatalf("status = %s, want it running again", cur.Status)
	}
}

// A failed resume leaves the project paused for the same reason, so that the next request tries again.
func TestFailedResumeKeepsTheIdleRecord(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.create(t)
	if err := h.e.PauseWith(ctx, p.Ref, PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	h.plane.failOn["Start"] = errors.New("no postgres")
	if err := h.e.Resume(ctx, p.Ref); err == nil {
		t.Fatal("expected the start failure")
	}
	if got := pauseOf(t, h, p.Ref); got == nil || got.Reason != registry.PauseIdle {
		t.Fatalf("after a failed resume: %+v", got)
	}
	if cur, _ := h.reg.GetProject(ctx, p.Ref); cur.Status != registry.StatusInactive {
		t.Fatalf("status = %s", cur.Status)
	}
}

// A registry that cannot record the reason refuses an idle pause (the project would never wake), and
// does not refuse the owner's.
type noLifecycleRegistry struct{ registry.Registry }

func TestIdlePauseNeedsAStoreThatCanRecordIt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.create(t)
	h.e.reg = noLifecycleRegistry{h.reg}
	if err := h.e.PauseWith(ctx, p.Ref, PauseOptions{Reason: registry.PauseIdle}); err == nil {
		t.Fatal("an idle pause was made without a way to record it")
	}
	if cur, _ := h.reg.GetProject(ctx, p.Ref); !cur.Status.Running() {
		t.Fatalf("status = %s after the refusal", cur.Status)
	}
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatalf("the owner's pause: %v", err)
	}
}

// A branch is homed on its parent's node, not on the leader.
func TestBranchIsHomedOnItsParent(t *testing.T) {
	ctx := context.Background()
	c := newClusterHarness(t, "n1")
	// home2 lives on the second node; the leader is the first.
	branch, err := c.e.Create(ctx, CreateRequest{Name: "feature", Branch: &registry.BranchInfo{
		ID: "7c0c9c4e-0000-4000-8000-000000000001", ParentRef: c.home2.Ref, Name: "feature",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if branch.NodeID != c.home2.NodeID || branch.NodeID == c.home1.NodeID {
		t.Fatalf("the branch is homed on %q, its parent on %q", branch.NodeID, c.home2.NodeID)
	}
	// A project that is not a branch still starts on the leader.
	plain, err := c.e.Create(ctx, CreateRequest{Name: "plain"})
	if err != nil || plain.NodeID != c.home1.NodeID {
		t.Fatalf("plain project node = %q, %v", plain.NodeID, err)
	}
}

type fakeObjects struct {
	mu   sync.Mutex
	refs []string
	err  error
}

func (f *fakeObjects) RemoveObjects(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, ref)
	return f.err
}

// The S3 backend's objects go with the project, and with a branch reset (PurgeObjects) but not with
// the delete of a restore, which brings its own copy back.
func TestDeleteAndResetRemoveTheBucketObjects(t *testing.T) {
	ctx := context.Background()
	newCase := func(t *testing.T, backend string) (*harness, *fakeObjects, *registry.Project, string) {
		h := newHarness(t)
		h.cfg.StateDir = t.TempDir()
		h.cfg.Fleet.StorageBackend = backend
		fo := &fakeObjects{}
		h.e.SetObjectRemover(fo)
		p := h.create(t)
		return h, fo, p, storageObject(t, h, p.Ref)
	}

	t.Run("a delete removes the prefix", func(t *testing.T) {
		h, fo, p, _ := newCase(t, "s3")
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
		if len(fo.refs) != 1 || fo.refs[0] != p.Ref {
			t.Fatalf("removed %v", fo.refs)
		}
	})
	t.Run("the file backend has no bucket to empty", func(t *testing.T) {
		h, fo, p, _ := newCase(t, "")
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
		if len(fo.refs) != 0 {
			t.Fatalf("removed %v from a bucket that is not used", fo.refs)
		}
	})
	t.Run("a reset removes the files and the prefix, a restore keeps them", func(t *testing.T) {
		h, fo, p, obj := newCase(t, "s3")
		if err := h.e.DeleteWith(ctx, p.Ref, DeleteOptions{KeepRecord: true}); err != nil {
			t.Fatal(err)
		}
		if len(fo.refs) != 0 || !exists(obj) {
			t.Fatalf("a KeepRecord delete removed objects: %v, file present %v", fo.refs, exists(obj))
		}
		h, fo, p, obj = newCase(t, "s3")
		if err := h.e.DeleteWith(ctx, p.Ref, DeleteOptions{KeepRecord: true, PurgeObjects: true}); err != nil {
			t.Fatal(err)
		}
		if len(fo.refs) != 1 || exists(obj) {
			t.Fatalf("a reset kept objects: %v, file present %v", fo.refs, exists(obj))
		}
		if _, err := os.Stat(h.cfg.Paths().StorageObjects(p.Ref)); err == nil {
			t.Fatal("the directory is still there")
		}
	})
	t.Run("a bucket that cannot be emptied does not keep the project", func(t *testing.T) {
		h, fo, p, _ := newCase(t, "s3")
		fo.err = errors.New("403 from the bucket")
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatalf("the delete failed on the bucket: %v", err)
		}
		if _, err := h.reg.GetProject(ctx, p.Ref); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("the project stayed: %v", err)
		}
		if len(fo.refs) != 1 {
			t.Fatalf("the bucket was asked %d times", len(fo.refs))
		}
	})
}

// A branch asleep for idleness has a listener on its database port; a resume waits for it to go before
// it starts Postgres, so the two never fight for the port. A port nothing listens on costs nothing.
func TestResumeWaitsForTheWakeGateToLetGoOfThePort(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.create(t)
	if err := h.e.PauseWith(ctx, p.Ref, PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	cur, _ := h.reg.GetProject(ctx, p.Ref)
	port := h.cfg.PortsFor(p.Ref, cur.Seq).Postgres

	start := time.Now()
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("a resume with nothing on the port took %v", d)
	}

	if err := h.e.PauseWith(ctx, p.Ref, PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("port %d is in use on this machine: %v", port, err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln.Close()
	}()
	start = time.Now()
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 2500*time.Millisecond {
		t.Fatalf("the resume took %v with a listener that goes after 300 ms", d)
	}
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("the resume returned while the port was still held")
	}
}

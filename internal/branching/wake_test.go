package branching

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// wakeHarness is the idle harness with the schema-reading database and the function carrier of the
// content tests, so that diff, push, merge and reset run over a branch the node paused for idleness.
type wakeHarness struct {
	*idleHarness
	sdb *fakeSchemaDB
	fns *fakeCarrier
}

func newWakeHarness(t *testing.T) *wakeHarness {
	t.Helper()
	ih := newIdleHarness(t, nil)
	w := &wakeHarness{idleHarness: ih, sdb: newFakeSchemaDB(ih.db), fns: newFakeCarrier()}
	ih.svc.db = w.sdb
	ih.svc.fns = w.fns
	ih.eng.onCreate = func(req lifecycle.CreateRequest) { w.sdb.note("engine.create %s", req.Ref) }
	return w
}

// idlePause lets the node pause b for idleness, through the sweep.
func (w *wakeHarness) idlePause(b *Branch) {
	w.t.Helper()
	w.sweep()
	w.advance(31 * time.Minute)
	if res := w.sweep(); !has(res.Paused, b.Ref) {
		w.t.Fatalf("the sweep did not pause the branch: %+v", res)
	}
	if reason, ok := w.pauseReason(b.Ref); !ok || reason != registry.PauseIdle {
		w.t.Fatalf("pause record = %q, %v", reason, ok)
	}
}

// pause stops b the way the lifecycle engine does, with the reason.
func (w *wakeHarness) pause(ref, reason string) {
	w.t.Helper()
	if err := w.eng.PauseWith(context.Background(), ref, lifecycle.PauseOptions{Reason: reason}); err != nil {
		w.t.Fatal(err)
	}
}

// applied is how many times the Edge Functions runtime was told to serve ref's functions.
func (w *wakeHarness) applied(ref string) int {
	w.fns.mu.Lock()
	defer w.fns.mu.Unlock()
	n := 0
	for _, r := range w.fns.applied {
		if r == ref {
			n++
		}
	}
	return n
}

func (w *wakeHarness) schedule(ref string, at time.Time) {
	w.t.Helper()
	p, err := w.reg.GetProject(context.Background(), ref)
	if err != nil {
		w.t.Fatal(err)
	}
	nb := *p.Branch
	nb.DeletionScheduledAt = &at
	if err := w.reg.UpdateBranch(context.Background(), ref, &nb); err != nil {
		w.t.Fatal(err)
	}
}

// operations are the calls that need a branch's database and used to refuse an idle-paused one.
func operations() map[string]func(w *wakeHarness, ref string) error {
	ctx := context.Background()
	return map[string]func(w *wakeHarness, ref string) error{
		"diff":  func(w *wakeHarness, ref string) error { _, err := w.svc.Diff(ctx, ref); return err },
		"push":  func(w *wakeHarness, ref string) error { _, err := w.svc.Push(ctx, ref, ActionInput{}); return err },
		"merge": func(w *wakeHarness, ref string) error { _, err := w.svc.Merge(ctx, ref, ActionInput{}); return err },
		"reset": func(w *wakeHarness, ref string) error { _, err := w.svc.Reset(ctx, ref, ActionInput{}); return err },
	}
}

// A diff, a push, a merge and a reset start a branch the node paused for idleness instead of failing with
// "must be running", tell the Edge Functions runtime to serve its functions, and count as use.
func TestOperationsStartAnIdlePausedBranch(t *testing.T) {
	for name, op := range operations() {
		t.Run(name, func(t *testing.T) {
			w := newWakeHarness(t)
			b := w.create("a", nil)
			w.idlePause(b)
			before := w.applied(b.Ref)
			if err := op(w, b.Ref); err != nil {
				t.Fatalf("%s of an idle-paused branch: %v", name, err)
			}
			w.wait(b.Ref)
			if w.eng.resumed[b.Ref] != 1 || w.status(b.Ref) != registry.StatusActiveHealthy {
				t.Fatalf("resumes %d, status %s", w.eng.resumed[b.Ref], w.status(b.Ref))
			}
			if w.applied(b.Ref) <= before {
				t.Fatalf("the Edge Functions runtime was not told to serve the woken branch (%d calls)", w.applied(b.Ref))
			}
			evs, _ := w.reg.ListEvents(context.Background(), b.Ref, 200)
			by := ""
			for _, e := range evs {
				if e.Kind == "branch.idle_woken" {
					by = string(e.Payload)
				}
			}
			if !strings.Contains(by, `"by":"`+name+`"`) {
				t.Fatalf("branch.idle_woken payload = %q", by)
			}
			// It was just used: the next sweep leaves it up.
			if res := w.sweep(); len(res.Paused) != 0 {
				t.Fatalf("paused again at once: %+v", res)
			}
		})
	}
}

// An operation never starts what a person paused, what the node stopped because the branch carries the parent's
// credentials, a pause with no record, or a branch scheduled for deletion: it is refused and says what to do.
func TestOperationsDoNotStartABranchThatIsNotIdlePaused(t *testing.T) {
	cases := map[string]struct {
		setup func(w *wakeHarness, b *Branch)
		want  string
	}{
		"the owner paused it": {func(w *wakeHarness, b *Branch) { w.pause(b.Ref, "") }, "Resume it first"},
		"a pause with no record": {func(w *wakeHarness, b *Branch) {
			if err := w.reg.SetProjectStatus(context.Background(), b.Ref, registry.StatusInactive); err != nil {
				t.Fatal(err)
			}
		}, "Resume it first"},
		"a quarantine": {func(w *wakeHarness, b *Branch) { w.pause(b.Ref, registry.PauseQuarantine) }, "reset or delete it"},
		"idle pause, deletion queued": {func(w *wakeHarness, b *Branch) {
			w.pause(b.Ref, registry.PauseIdle)
			w.schedule(b.Ref, w.clock().Add(time.Hour))
		}, "must be running"},
	}
	for name, c := range cases {
		for _, opName := range []string{"diff", "push", "merge"} {
			t.Run(name+"/"+opName, func(t *testing.T) {
				w := newWakeHarness(t)
				b := w.create("a", nil)
				c.setup(w, b)
				err := operations()[opName](w, b.Ref)
				if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("%s: %v", opName, err)
				}
				if w.eng.resumed[b.Ref] != 0 || w.status(b.Ref) != registry.StatusInactive {
					t.Fatalf("resumes %d, status %s", w.eng.resumed[b.Ref], w.status(b.Ref))
				}
			})
		}
	}
}

// Wake, which the proxy and the database gate call, never starts a branch scheduled for deletion, even
// when its pause still says "idle" (the change to the owner's pause did not go through).
func TestWakeLeavesABranchScheduledForDeletion(t *testing.T) {
	w := newWakeHarness(t)
	b := w.create("a", nil)
	w.idlePause(b)
	w.schedule(b.Ref, w.clock().Add(time.Hour))
	if err := w.svc.Wake(context.Background(), b.Ref); !errors.Is(err, ErrNotIdle) || w.eng.resumed[b.Ref] != 0 {
		t.Fatalf("wake of a branch scheduled for deletion: %v, resumes %d", err, w.eng.resumed[b.Ref])
	}
}

// Whoever wakes a branch for a request or a connection, the Edge Functions runtime is told, so that the first
// call to a function finds its files; a Restore tells it too.
func TestWakeAndRestoreTellTheFunctionsRuntime(t *testing.T) {
	w := newWakeHarness(t)
	w.fns.put(parentRef, "hello", "h1", 1)
	b := w.create("a", nil)
	base := w.applied(b.Ref)
	w.idlePause(b)
	if err := w.svc.Wake(context.Background(), b.Ref); err != nil {
		t.Fatal(err)
	}
	if got := w.applied(b.Ref); got != base+1 {
		t.Fatalf("applied after a wake = %d, want %d", got, base+1)
	}
	// A restore of a branch whose deletion is scheduled.
	if _, err := w.svc.Delete(context.Background(), b.Ref, DeleteOptions{Schedule: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Restore(context.Background(), b.Ref); err != nil {
		t.Fatal(err)
	}
	if got := w.applied(b.Ref); got != base+2 {
		t.Fatalf("applied after a restore = %d, want %d", got, base+2)
	}
}

// WakeForDrive, which the Git integration calls for a commit, starts a branch the node paused for idleness and
// nothing else: a commit is not the owner's decision to run a branch again.
func TestWakeForDriveStartsOnlyAnIdlePause(t *testing.T) {
	ctx := context.Background()
	refused := map[string]struct {
		setup func(w *wakeHarness, b *Branch)
		want  string
	}{
		"paused by its owner": {func(w *wakeHarness, b *Branch) { w.pause(b.Ref, "") }, "Resume it first"},
		"no record of the pause": {func(w *wakeHarness, b *Branch) {
			if err := w.reg.SetProjectStatus(ctx, b.Ref, registry.StatusInactive); err != nil {
				t.Fatal(err)
			}
		}, "Resume it first"},
		"a quarantine": {func(w *wakeHarness, b *Branch) { w.pause(b.Ref, registry.PauseQuarantine) }, "reset or delete it"},
		"idle pause, deletion queued": {func(w *wakeHarness, b *Branch) {
			w.pause(b.Ref, registry.PauseIdle)
			w.schedule(b.Ref, w.clock().Add(time.Hour))
		}, "scheduled for deletion"},
		"owner pause, deletion queued": {func(w *wakeHarness, b *Branch) { w.pause(b.Ref, ""); w.schedule(b.Ref, w.clock().Add(time.Hour)) }, "scheduled for deletion"},
		"running, deletion queued":     {func(w *wakeHarness, b *Branch) { w.schedule(b.Ref, w.clock().Add(time.Hour)) }, "scheduled for deletion"},
	}
	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			w := newWakeHarness(t)
			b := w.create("a", nil)
			c.setup(w, b)
			running := w.status(b.Ref).Running()
			got, err := w.svc.WakeForDrive(ctx, b.Ref)
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), c.want) || got != nil {
				t.Fatalf("WakeForDrive = %+v, %v", got, err)
			}
			if w.eng.resumed[b.Ref] != 0 || w.status(b.Ref).Running() != running {
				t.Fatalf("resumes %d, status %s", w.eng.resumed[b.Ref], w.status(b.Ref))
			}
			// Drive does not take a branch that waits for its deletion either.
			if running {
				if _, err := w.svc.Drive(ctx, b.Ref, "push", func(context.Context, *Run) (string, error) { return "", nil }); !errors.Is(err, ErrConflict) {
					t.Fatalf("drive of a branch scheduled for deletion: %v", err)
				}
			}
		})
	}
	t.Run("idle pause", func(t *testing.T) {
		w := newWakeHarness(t)
		b := w.create("a", nil)
		w.idlePause(b)
		got, err := w.svc.WakeForDrive(ctx, b.Ref)
		if err != nil || got.ProjectStatus != registry.StatusActiveHealthy || w.eng.resumed[b.Ref] != 1 {
			t.Fatalf("WakeForDrive = %+v, %v, resumes %v", got, err, w.eng.resumed)
		}
		if w.applied(b.Ref) == 0 {
			t.Fatal("the Edge Functions runtime was not told to serve the woken branch")
		}
		if _, err := w.svc.Drive(ctx, b.Ref, "push", func(context.Context, *Run) (string, error) { return "ok", nil }); err != nil {
			t.Fatalf("drive after the wake: %v", err)
		}
		w.wait(b.Ref)
	})
}

// A branch stopped because it still carries the parent's credentials is recorded as a quarantine, which no
// automatic start undoes: not a request, not a connection, not a commit of the repository, not an operation.
func TestQuarantinedBranchIsRecordedAndNeverWokenByItself(t *testing.T) {
	ctx := context.Background()
	h := cloneHarness(t)
	h.isolateErr = errors.New("cannot reach the cluster")
	b := h.create("leaky", func(in *CreateInput) { in.WithData = true })
	h.mustState(b, registry.BranchMigrationsFailed)
	if p, err := h.reg.GetPause(ctx, b.Ref); err != nil || p.Reason != registry.PauseQuarantine {
		t.Fatalf("pause record = %+v, %v", p, err)
	}
	if got := h.svc.PauseReason(ctx, b.Ref); got != registry.PauseQuarantine {
		t.Fatalf("PauseReason = %q", got)
	}
	if err := h.svc.Wake(ctx, b.Ref); !errors.Is(err, ErrNotIdle) {
		t.Fatalf("Wake of a quarantined branch: %v", err)
	}
	if _, err := h.svc.WakeForDrive(ctx, b.Ref); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "reset or delete it") {
		t.Fatalf("WakeForDrive of a quarantined branch: %v", err)
	}
	// A deletion scheduled for it keeps the quarantine; the node still does not start it.
	if _, err := h.svc.Delete(ctx, b.Ref, DeleteOptions{Schedule: true}); err != nil {
		t.Fatal(err)
	}
	if got := h.svc.PauseReason(ctx, b.Ref); got != registry.PauseQuarantine {
		t.Fatalf("PauseReason after a scheduled deletion = %q", got)
	}
	if h.eng.resumed[b.Ref] != 0 {
		t.Fatalf("resumes %d", h.eng.resumed[b.Ref])
	}
}

// noQuarantineEngine cannot record the quarantine reason: the registry would not take the record.
type noQuarantineEngine struct{ *fakeEngine }

func (e noQuarantineEngine) PauseWith(ctx context.Context, ref string, o lifecycle.PauseOptions) error {
	if o.Reason == registry.PauseQuarantine {
		return errors.New("the pause cannot be recorded")
	}
	return e.fakeEngine.PauseWith(ctx, ref, o)
}

// When the reason cannot be recorded the branch is still stopped, as an owner's pause, which nothing starts by
// itself either.
func TestQuarantineStopsTheBranchEvenWhenTheReasonCannotBeRecorded(t *testing.T) {
	ctx := context.Background()
	h := cloneHarness(t)
	h.svc.eng = noQuarantineEngine{h.eng}
	h.isolateErr = errors.New("cannot reach the cluster")
	b := h.create("leaky", func(in *CreateInput) { in.WithData = true })
	h.mustState(b, registry.BranchMigrationsFailed)
	p, err := h.reg.GetProject(ctx, b.Ref)
	if err != nil || p.Status != registry.StatusInactive {
		t.Fatalf("project = %+v, %v", p, err)
	}
	if got := h.svc.PauseReason(ctx, b.Ref); got != "owner" {
		t.Fatalf("PauseReason = %q", got)
	}
	if _, err := h.svc.WakeForDrive(ctx, b.Ref); !errors.Is(err, ErrConflict) || h.eng.resumed[b.Ref] != 0 {
		t.Fatalf("WakeForDrive: %v, resumes %d", err, h.eng.resumed[b.Ref])
	}
}

// What the service does on a branch itself is use, which no request and no database session shows: a branch
// that was busy for longer than the period is not paused the moment it is free, and a diff pushes the pause back.
func TestOperationsCountAsUseForTheIdlePause(t *testing.T) {
	ctx := context.Background()
	t.Run("a run that outlasts the period", func(t *testing.T) {
		w := newWakeHarness(t)
		b := w.create("a", nil)
		w.sweep()
		started, release := make(chan struct{}), make(chan struct{})
		if _, err := w.svc.Drive(ctx, b.Ref, "push", func(context.Context, *Run) (string, error) {
			close(started)
			<-release
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
		<-started
		w.advance(40 * time.Minute)
		if res := w.sweep(); len(res.Paused) != 0 {
			t.Fatalf("paused while a run was in progress: %+v", res)
		}
		close(release)
		w.wait(b.Ref)
		if res := w.sweep(); len(res.Paused) != 0 {
			t.Fatalf("paused the moment the run ended: %+v", res)
		}
		w.advance(29 * time.Minute)
		if res := w.sweep(); len(res.Paused) != 0 {
			t.Fatalf("paused 29 minutes after the run ended: %+v", res)
		}
		w.advance(2 * time.Minute)
		if res := w.sweep(); !has(res.Paused, b.Ref) {
			t.Fatalf("not paused 31 minutes after the run ended: %+v", res)
		}
	})
	t.Run("a diff", func(t *testing.T) {
		w := newWakeHarness(t)
		b := w.create("a", nil)
		w.sweep()
		w.advance(29 * time.Minute)
		if _, err := w.svc.Diff(ctx, b.Ref); err != nil {
			t.Fatal(err)
		}
		w.advance(29 * time.Minute)
		if res := w.sweep(); len(res.Paused) != 0 {
			t.Fatalf("paused 29 minutes after a diff: %+v", res)
		}
		w.advance(2 * time.Minute)
		if res := w.sweep(); !has(res.Paused, b.Ref) {
			t.Fatalf("not paused 31 minutes after a diff: %+v", res)
		}
	})
}

// The sweep reads a branch again before it deletes it: a Restore that came after the list was read, while an
// earlier deletion of the pass was still running, keeps the branch.
func TestSweepReadsABranchAgainBeforeDeletingIt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	a, b := h.create("a", nil), h.create("b", nil)
	for _, br := range []*Branch{a, b} {
		if _, err := h.svc.Delete(ctx, br.Ref, DeleteOptions{Schedule: true}); err != nil {
			t.Fatal(err)
		}
	}
	h.advance(2 * time.Hour)
	var restored string
	h.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) {
		if restored != "" {
			return
		}
		restored = a.Ref
		if ref == a.Ref {
			restored = b.Ref
		}
		if _, err := h.svc.Restore(ctx, restored); err != nil {
			t.Errorf("restore of %s during the sweep: %v", restored, err)
		}
	}
	res, err := h.svc.Sweep(ctx, false)
	if err != nil || len(res.Failed) != 0 || len(res.Expired) != 1 || res.Expired[0] == restored {
		t.Fatalf("sweep = %+v, %v (restored %s)", res, err, restored)
	}
	got, err := h.svc.Resolve(ctx, restored)
	if err != nil || got.DeletionScheduledAt != nil || got.ProjectStatus != registry.StatusActiveHealthy {
		t.Fatalf("the restored branch = %+v, %v", got, err)
	}
	// The restored branch is not due again.
	if res, err = h.svc.Sweep(ctx, false); err != nil || len(res.Failed) != 0 || len(res.Expired) != 0 {
		t.Fatalf("second sweep = %+v, %v", res, err)
	}
}

// When a second server joins, the branches the node paused for idleness start again and their functions are
// served; one that waits for its deletion stays down.
func TestServerJoinStartsIdleBranchesExceptOnesScheduledForDeletion(t *testing.T) {
	ctx := context.Background()
	w := newWakeHarness(t)
	keep, gone := w.create("keep", nil), w.create("gone", nil)
	w.sweep()
	w.advance(31 * time.Minute)
	if res := w.sweep(); !has(res.Paused, keep.Ref) || !has(res.Paused, gone.Ref) {
		t.Fatalf("not paused: %+v", res)
	}
	w.schedule(gone.Ref, w.clock().Add(time.Hour))
	if err := w.reg.CreateNode(ctx, &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	before := w.applied(keep.Ref)
	res := w.sweep()
	if !has(res.Resumed, keep.Ref) || has(res.Resumed, gone.Ref) || w.status(gone.Ref) != registry.StatusInactive {
		t.Fatalf("sweep = %+v, status of the branch scheduled for deletion %s", res, w.status(gone.Ref))
	}
	if w.applied(keep.Ref) != before+1 {
		t.Fatalf("applied = %d, want %d", w.applied(keep.Ref), before+1)
	}
}

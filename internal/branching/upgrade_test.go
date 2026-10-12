package branching

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// asEarlierRelease rewrites the row of a branch the way v0.3.x leaves one that finished: the state
// MIGRATIONS_PASSED, no detail, and the lifetime it was given (nil: none).
func (h *harness) asEarlierRelease(ref string, expires *time.Time) {
	h.t.Helper()
	p, err := h.reg.GetProject(context.Background(), ref)
	if err != nil || p.Branch == nil {
		h.t.Fatalf("branch %s: %v", ref, err)
	}
	b := *p.Branch
	b.State, b.Detail, b.ExpiresAt = registry.BranchMigrationsPassed, "", expires
	if err := h.reg.UpdateBranch(context.Background(), ref, &b); err != nil {
		h.t.Fatal(err)
	}
}

// TestABranchOfTheEarlierReleaseKeepsItsLifetimeAndIsAtRest: a branch that v0.3.x made ends at
// MIGRATIONS_PASSED with an expiry (7 days by default). Nothing in this release treats that state as a
// run in progress, and no operation, setting change or sweep moves the expiry the branch was given.
func TestABranchOfTheEarlierReleaseKeepsItsLifetimeAndIsAtRest(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("old", func(in *CreateInput) { in.TTL = 72 * time.Hour })
	if b.ExpiresAt == nil {
		t.Fatal("the branch has no expiry")
	}
	life := *b.ExpiresAt

	h.asEarlierRelease(b.Ref, &life)
	got, err := h.svc.Get(ctx, parentRef, "old")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != registry.BranchMigrationsPassed || got.ExpiresAt == nil || !got.ExpiresAt.Equal(life) {
		t.Fatalf("branch = %s, expires %v", got.State, got.ExpiresAt)
	}
	if h.svc.inFlight(ctx, got) {
		t.Fatal("MIGRATIONS_PASSED with no run going on reads as an operation in progress")
	}
	if idle, err := h.svc.WaitIdle(ctx, b.Ref); err != nil || idle.State != registry.BranchMigrationsPassed {
		t.Fatalf("WaitIdle = %+v, %v", idle, err)
	}

	for name, op := range map[string]func() error{
		"push":  func() error { _, err := h.svc.Push(ctx, b.Ref, ActionInput{}); return err },
		"merge": func() error { _, err := h.svc.Merge(ctx, b.Ref, ActionInput{}); return err },
		"reset": func() error { _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); return err },
		"update": func() error {
			name := "renamed-" + b.Name
			_, err := h.svc.Update(ctx, b.Ref, UpdateInput{NotifyURL: new(string), Name: &name})
			return err
		},
	} {
		h.asEarlierRelease(b.Ref, &life)
		if err := op(); err != nil {
			t.Fatalf("%s on a branch at MIGRATIONS_PASSED: %v", name, err)
		}
		after := h.wait(b.Ref)
		if after.ExpiresAt == nil || !after.ExpiresAt.Equal(life) {
			t.Errorf("%s moved the expiry from %v to %v", name, life, after.ExpiresAt)
		}
		if name != "update" && after.State != registry.BranchFunctionsDeployed {
			t.Errorf("%s ended at %s (%s)", name, after.State, after.Detail)
		}
	}

	// The sweeper honors the recorded expiry: kept before it, deleted after it.
	h.asEarlierRelease(b.Ref, &life)
	res, err := h.svc.Sweep(ctx, true)
	if err != nil || slices.Contains(res.Expired, b.Ref) {
		t.Fatalf("sweep before the expiry = %+v, %v", res, err)
	}
	h.advance(73 * time.Hour)
	if res, err = h.svc.Sweep(ctx, true); err != nil || !slices.Contains(res.Expired, b.Ref) {
		t.Fatalf("sweep after the expiry = %+v, %v", res, err)
	}
}

// TestResetKeepsABranchThatNeverExpires: a node that runs with default_ttl off (v0.4.0's default, and a
// setting v0.3.x honored) has branches with no expiry. A reset raises a short expiry to the floor, but
// gives none to a branch that has none: the sweeper would delete it a quarter of an hour later.
func TestResetKeepsABranchThatNeverExpires(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("forever", func(in *CreateInput) { in.TTL = -1 })
	if b.ExpiresAt != nil {
		t.Fatalf("a branch made with no lifetime expires at %v", b.ExpiresAt)
	}
	if _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	if got := h.wait(b.Ref); got.ExpiresAt != nil {
		t.Fatalf("a reset gave a branch that never expires an expiry: %v", got.ExpiresAt)
	}

	// A lifetime shorter than the floor is raised to it; a longer one stays.
	short := h.clock().Add(time.Minute)
	h.asEarlierRelease(b.Ref, &short)
	if _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	if want := h.clock().Add(resetMinLife); got.ExpiresAt == nil || got.ExpiresAt.Before(want.Add(-time.Second)) {
		t.Fatalf("a reset left %v of lifetime, want at least %v", got.ExpiresAt, resetMinLife)
	}
}

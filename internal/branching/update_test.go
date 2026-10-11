package branching

import (
	"context"
	"errors"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

func ptr[T any](v T) *T { return &v }

func TestRequestReviewTrueThenFalseThenMergeClears(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	got, err := h.svc.Update(ctx, b.Ref, UpdateInput{RequestReview: ptr(true)})
	if err != nil || got.ReviewRequestedAt == nil {
		t.Fatalf("request review: %+v %v", got, err)
	}
	// false clears it (Studio's "Close merge request").
	if got, err = h.svc.Update(ctx, b.Ref, UpdateInput{RequestReview: ptr(false)}); err != nil || got.ReviewRequestedAt != nil {
		t.Fatalf("close the merge request: %+v %v", got, err)
	}
	// A successful merge clears it too, without the client asking.
	if _, err = h.svc.Update(ctx, b.Ref, UpdateInput{RequestReview: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	if _, err := h.svc.Merge(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got = h.wait(b.Ref)
	h.mustState(got, registry.BranchFunctionsDeployed)
	if got.ReviewRequestedAt != nil {
		t.Fatalf("review mark after a merge: %v", got.ReviewRequestedAt)
	}
	// A failed merge keeps it.
	if _, err = h.svc.Update(ctx, b.Ref, UpdateInput{RequestReview: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260111000000", "more", "create table g (id int)"))
	h.db.applyErr[parentRef] = errors.New("boom")
	if _, err := h.svc.Merge(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	if got = h.wait(b.Ref); got.State != registry.BranchMigrationsFailed || got.ReviewRequestedAt == nil {
		t.Fatalf("after a failed merge: %s review=%v", got.State, got.ReviewRequestedAt)
	}
}

func TestUpdateStatusOverride(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	for _, st := range []registry.BranchState{
		registry.BranchCreatingProject, registry.BranchRunningMigration, registry.BranchMigrationsPassed,
		registry.BranchMigrationsFailed, registry.BranchFunctionsDeployed, registry.BranchFunctionsFailed,
	} {
		got, err := h.svc.Update(ctx, b.Ref, UpdateInput{State: &st})
		if err != nil || got.State != st {
			t.Fatalf("status %s: %+v %v", st, got, err)
		}
	}
	bad := registry.BranchState("WOBBLY")
	if _, err := h.svc.Update(ctx, b.Ref, UpdateInput{State: &bad}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown status: %v", err)
	}
	// An operation running here owns the status.
	r, err := h.svc.beginRun(ctx, b.Ref, "push", b.Ref, parentRef)
	if err != nil {
		t.Fatal(err)
	}
	st := registry.BranchFunctionsDeployed
	if _, err := h.svc.Update(ctx, b.Ref, UpdateInput{State: &st}); !errors.Is(err, ErrConflict) {
		t.Fatalf("override during an operation: %v", err)
	}
	h.svc.end(b.Ref, r)
}

func TestDefaultBranchTracksAGitBranch(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	// is_default returns the default branch and creates nothing, again and again.
	for range 2 {
		got, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "main", GitBranch: "production", IsDefault: true})
		if err != nil || !got.IsDefault || got.Ref != parentRef || got.GitBranch != "production" || got.Name != DefaultBranchName {
			t.Fatalf("create is_default: %+v %v", got, err)
		}
	}
	if list, _ := h.svc.List(ctx, parentRef); len(list) != 1 || list[0].GitBranch != "production" {
		t.Fatalf("list = %+v", list)
	}
	if len(h.eng.creates) != 0 {
		t.Fatalf("is_default created %d project(s)", len(h.eng.creates))
	}
	// PATCH stores the git branch, with or without the name Studio sends along.
	for _, in := range []UpdateInput{
		{GitBranch: ptr("develop")},
		{GitBranch: ptr("develop"), Name: ptr("develop")}, // Studio's Production branch form
		{GitBranch: ptr("")},
	} {
		got, err := h.svc.Update(ctx, parentRef, in)
		if err != nil || got.Name != DefaultBranchName || got.GitBranch != *in.GitBranch {
			t.Fatalf("patch %+v: %+v %v", in, got, err)
		}
	}
	// What the default branch cannot do is refused with the reason.
	for name, in := range map[string]UpdateInput{
		"rename":         {Name: ptr("trunk")},
		"not persistent": {Persistent: ptr(false)},
		"notify_url":     {NotifyURL: ptr("https://example.com/hook")},
		"review":         {RequestReview: ptr(true)},
		"status":         {State: ptr(registry.BranchFunctionsFailed)},
	} {
		if _, err := h.svc.Update(ctx, parentRef, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Harmless repeats of what is true already pass.
	if _, err := h.svc.Update(ctx, parentRef, UpdateInput{Persistent: ptr(true), Name: ptr("main"), RequestReview: ptr(false), NotifyURL: ptr("")}); err != nil {
		t.Fatalf("no-op patch: %v", err)
	}
	// A branch's own ref names the same default branch through the family.
	b := h.create("feat", nil)
	got, err := h.svc.Create(ctx, b.Ref, CreateInput{Name: "main", IsDefault: true})
	if err != nil || got.Ref != parentRef {
		t.Fatalf("is_default through a branch ref: %+v %v", got, err)
	}
	if _, err := h.svc.Create(ctx, "zzzzzzzzzzzzzzzzzzzz", CreateInput{Name: "main", IsDefault: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

// Studio patches request_review the moment a merge run reads SUCCESS, which can be before the
// branch's status moves on. The patch writes the settings, not the status it read.
func TestUpdateKeepsTheStatusAnOperationSetMeanwhile(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	h.svc.setState(ctx, b.Ref, registry.BranchRunningMigration, "merging", nil)
	h.svc.reg = &hookRegistry{Registry: h.reg, beforeUpdate: func() {
		// The operation ends between the read and the write of the PATCH.
		h.svc.setState(ctx, b.Ref, registry.BranchFunctionsDeployed, "done", nil)
	}}
	got, err := h.svc.Update(ctx, b.Ref, UpdateInput{RequestReview: ptr(false), Persistent: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != registry.BranchFunctionsDeployed || got.Detail != "done" || !got.Persistent || got.ReviewRequestedAt != nil {
		t.Fatalf("branch after the patch = %s %q persistent=%v review=%v", got.State, got.Detail, got.Persistent, got.ReviewRequestedAt)
	}
}

package api

import (
	"context"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

func makeBranch(t *testing.T, f *fixture, name string) (ref, id string) {
	t.Helper()
	f.bdb.migs[testRef] = nil
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": name})
	if rec.Code != 201 && rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	ref, id = jsonField(t, rec, "project_ref").(string), jsonField(t, rec, "id").(string)
	f.waitBranch(t, name, "FUNCTIONS_DEPLOYED")
	return ref, id
}

func hasEventKind(f *fixture, ref, kind string) bool {
	evs, _ := f.reg.ListEvents(context.Background(), ref, 50)
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// A branch is not a project of the v1 API: hosted answers 404 for its ref, so that the CLI keeps the
// parent as the linked project. Studio reads the branch through the platform route, and the other v1
// routes (functions, secrets, config) still take a branch ref.
func TestV1ProjectOfABranchRefIs404(t *testing.T) {
	f := newFixture(t)
	ref, _ := makeBranch(t, f, "feat")

	if rec := f.do("GET", "/v1/projects/"+ref, nil); rec.Code != 404 {
		t.Fatalf("GET /v1/projects/{branchRef}: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/"+ref+"/branches", nil); rec.Code != 404 {
		t.Fatalf("GET /v1/projects/{branchRef}/branches: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef, nil); rec.Code != 200 {
		t.Fatalf("the parent: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); rec.Code != 200 {
		t.Fatalf("the parent's branches: %d", rec.Code)
	}
	if rec := f.do("GET", "/platform/projects/"+ref, nil); rec.Code != 200 {
		t.Fatalf("Studio reads a branch through the platform route: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/projects/"+ref+"/health", nil); rec.Code == 404 {
		t.Fatalf("a v1 route other than the two refused answered 404 for a branch ref: %s", rec.Body)
	}
	if rec := f.do("GET", "/v1/branches/"+ref, nil); rec.Code != 200 {
		t.Fatalf("GET /v1/branches/{ref}: %d", rec.Code)
	}
}

// Deleting a branch as a project goes through the branch service: the parent records branch.deleted
// and the branch is gone, whichever of the three routes asked.
func TestDeleteOfABranchAsAProjectGoesThroughTheBranchService(t *testing.T) {
	for _, route := range []string{"/v1/projects/", "/platform/projects/"} {
		t.Run(route, func(t *testing.T) {
			f := newFixture(t)
			ref, _ := makeBranch(t, f, "feat")
			if rec := f.do("DELETE", route+ref, nil); rec.Code != 200 {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body)
			}
			if _, err := f.reg.GetProject(context.Background(), ref); err == nil {
				t.Fatal("the branch is still there")
			}
			if !hasEventKind(f, testRef, "branch.deleted") {
				t.Fatal("the parent has no branch.deleted event: the branch was not deleted through the branch service")
			}
		})
	}
}

// Resuming, from the paused page, a branch that waits to be deleted cancels the deletion.
func TestResumeOfAScheduledBranchCancelsItsDeletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ref, id := makeBranch(t, f, "feat")
	if rec := f.do("DELETE", "/v1/branches/"+id+"?force=false", nil); rec.Code != 200 {
		t.Fatalf("schedule: %d %s", rec.Code, rec.Body)
	}
	p, _ := f.reg.GetProject(ctx, ref)
	if p.Branch.DeletionScheduledAt == nil || p.Status != registry.StatusInactive {
		t.Fatalf("after scheduling: %+v status %s", p.Branch, p.Status)
	}
	if rec := f.do("POST", "/platform/projects/"+ref+"/restore", nil); rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	p, _ = f.reg.GetProject(ctx, ref)
	if p.Branch.DeletionScheduledAt != nil || !p.Status.Running() {
		t.Fatalf("after the resume: scheduled %v, status %s", p.Branch.DeletionScheduledAt, p.Status)
	}
}

// A call that runs SQL on a branch the node paused for idleness wakes it, so that an agent that left a
// branch alone does not have to resume it first; a branch the owner paused answers 409 as before.
func TestSQLCallWakesAnIdleBranchButNotAnOwnersPause(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ref, _ := makeBranch(t, f, "feat")
	pause := func(reason string) {
		t.Helper()
		if err := f.reg.SetPause(ctx, ref, reason, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := f.reg.SetProjectStatus(ctx, ref, registry.StatusInactive); err != nil {
			t.Fatal(err)
		}
	}
	q := func() int {
		return f.do("POST", "/v1/projects/"+ref+"/database/query", map[string]any{"query": "select 1"}).Code
	}
	pause("")
	if code := q(); code != 409 {
		t.Fatalf("a query on a branch the owner paused: %d", code)
	}
	if p, _ := f.reg.GetProject(ctx, ref); p.Status != registry.StatusInactive {
		t.Fatalf("the owner's pause was undone: %s", p.Status)
	}
	pause(registry.PauseIdle)
	if code := q(); code != 201 && code != 200 {
		t.Fatalf("a query on an idle-paused branch: %d", code)
	}
	if p, _ := f.reg.GetProject(ctx, ref); !p.Status.Running() {
		t.Fatalf("the idle branch was not woken: %s", p.Status)
	}
	// The parent is not a branch: a pause is a pause.
	if err := f.reg.SetProjectStatus(ctx, testRef, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	if code := f.do("POST", "/v1/projects/"+testRef+"/database/query", map[string]any{"query": "select 1"}).Code; code != 409 {
		t.Fatalf("a query on a paused production project: %d", code)
	}
}

package failover

import (
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

const refBranch = "bbbbbbbbbbbbbbbbbbbc"

// addBranch registers a branch of refA on the same node as its parent.
func addBranch(t *testing.T, w *world) {
	t.Helper()
	pa, err := w.reg.GetProject(w.ctx, refA)
	must(t, err)
	must(t, w.reg.CreateProject(w.ctx, &registry.Project{
		Ref: refBranch, OrgID: pa.OrgID, Name: "feature", Status: registry.StatusActiveHealthy, NodeID: pa.NodeID,
		Branch: &registry.BranchInfo{ID: "7c0c9c4e-0000-4000-8000-000000000001", ParentRef: refA, Name: "feature"},
	}))
}

// A branch has no replica, so a server move leaves it on the old server; the plan says so, says what it
// means for the branch, and does not block the move.
func TestServerPlanSaysWhatHappensToBranches(t *testing.T) {
	w := serverWorld(t)
	addBranch(t, w)
	pl, err := w.orch().PlanServer(w.ctx, ServerOptions{})
	must(t, err)
	var note string
	for _, n := range pl.Notes {
		if strings.Contains(n, refBranch) {
			note = n
		}
	}
	if note == "" {
		t.Fatalf("no note about the branch in %q", pl.Notes)
	}
	for _, want := range []string{"1 branch project(s)", "do not move", "unavailable until", "disposable"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
	if len(pl.Projects) != 2 {
		t.Errorf("the plan moves %d projects (the branch must not be one of them): %+v", len(pl.Projects), pl.Projects)
	}
	for _, p := range pl.Projects {
		if p.Ref == refBranch {
			t.Errorf("the branch is in the plan: %+v", p)
		}
	}
}

// Moving a parent to the server that holds its replica leaves its branches behind, and the plan names them.
func TestProjectPlanNamesTheBranchesThatStayBehind(t *testing.T) {
	w := newWorld(t)
	addBranch(t, w)
	pl, err := w.orch().PlanProject(w.ctx, ProjectOptions{Ref: refA})
	must(t, err)
	var note string
	for _, n := range pl.Notes {
		if strings.Contains(n, refBranch) {
			note = n
		}
	}
	if note == "" {
		t.Fatalf("no note about the branch in %q", pl.Notes)
	}
	for _, want := range []string{"1 branch(es) of " + refA, "homed on its parent's server", "created or reset"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
	// A project with no branch has no such note.
	pl, err = w.orch().PlanProject(w.ctx, ProjectOptions{Ref: refB})
	must(t, err)
	for _, n := range pl.Notes {
		if strings.Contains(n, "branch") {
			t.Errorf("a project with no branches got the note %q", n)
		}
	}
	// And the branch itself cannot be moved.
	pl, err = w.orch().PlanProject(w.ctx, ProjectOptions{Ref: refBranch})
	must(t, err)
	if len(pl.Refused(false)) == 0 {
		t.Fatalf("a branch can be moved: %+v", pl)
	}
}

package registry

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMemoryBranchRuns(t *testing.T) { testBranchRuns(t, NewMemory()) }

// TestPostgresBranchRuns runs the same checks against a real database (see TestPostgres).
func TestPostgresBranchRuns(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Pool().Exec(ctx, `truncate supavise.branch_runs, supavise.projects, supavise.organizations cascade`); err != nil {
		t.Fatal(err)
	}
	testBranchRuns(t, r)
}

func newRun(id, owner, op string) *BranchRun {
	r := &BranchRun{ID: id, OwnerRef: owner, BranchRef: "branchaaaaaaaaaaaaaaa", ParentRef: owner, Op: op, Status: RunRunning,
		RunnerHost: "host", RunnerPID: 42, RunnerInstance: "inst"}
	// Steps in a shuffled order: the store returns them in BranchRunSteps order.
	for _, n := range []string{"deploy", "pull", "seed", "clone", "migrate", "health", "configure"} {
		st := StepCreated
		if n == "clone" || n == "configure" {
			st = StepPaused
		}
		r.Steps = append(r.Steps, BranchRunStep{Name: n, Status: st})
	}
	return r
}

// testBranchRuns is the shared check of the BranchRunStore of a registry.
func testBranchRuns(t *testing.T, reg Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := BranchRunsOf(reg)
	if st == nil {
		t.Fatal("registry has no BranchRunStore")
	}
	org, err := reg.CreateOrganization(ctx, "runs-org", "Runs")
	if err != nil {
		t.Fatal(err)
	}
	const a, b = "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"
	for _, ref := range []string{a, b} {
		if err := reg.CreateProject(ctx, &Project{Ref: ref, OrgID: org.ID, Name: ref}); err != nil {
			t.Fatal(err)
		}
	}
	const r1, r2, r3 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"

	// Create stores the run with all its steps in order, and refuses a second run with the same id.
	run := newRun(r1, a, "merge")
	if err := st.CreateBranchRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if run.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not set")
	}
	if err := st.CreateBranchRun(ctx, newRun(r1, a, "merge")); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate id: %v, want ErrConflict", err)
	}
	got, err := st.GetBranchRun(ctx, r1)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got.Steps {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != strings.Join(BranchRunSteps, ",") {
		t.Fatalf("steps = %v, want %v", names, BranchRunSteps)
	}
	if got.Status != RunRunning || got.OwnerRef != a || got.Op != "merge" || got.RunnerPID != 42 || got.CheckRunID != 0 {
		t.Fatalf("run = %+v", got)
	}
	if _, err := st.GetBranchRun(ctx, r2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}

	// Step changes apply together; OnlyFrom spares the steps that are not in the listed status.
	if err := st.SetBranchRunSteps(ctx, r1, []BranchRunStepChange{
		{Name: "pull", Status: StepRunning, Log: "reading\n"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBranchRunSteps(ctx, r1, []BranchRunStepChange{
		{Name: "pull", Status: StepDead, Log: "boom\n"},
		{Name: "migrate", Status: StepPaused, OnlyFrom: []string{StepCreated}},
		{Name: "clone", Status: StepPaused, OnlyFrom: []string{StepCreated}}, // already paused: stays, no log
		{Name: "health", Log: "note\n", OnlyFrom: []string{StepExited}},      // not exited: untouched
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetBranchRun(ctx, r1)
	byName := map[string]BranchRunStep{}
	for _, s := range got.Steps {
		byName[s.Name] = s
	}
	if s := byName["pull"]; s.Status != StepDead || s.Log != "reading\nboom\n" {
		t.Fatalf("pull = %+v", s)
	}
	if byName["migrate"].Status != StepPaused || byName["health"].Status != StepCreated || byName["health"].Log != "" {
		t.Fatalf("migrate = %+v, health = %+v", byName["migrate"], byName["health"])
	}
	if err := st.SetBranchRunSteps(ctx, r1, []BranchRunStepChange{{Name: "nope", Status: StepExited}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown step: %v", err)
	}
	if err := st.SetBranchRunSteps(ctx, r2, []BranchRunStepChange{{Name: "pull", Status: StepExited}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}

	// Finish, check run, running list.
	running, err := st.RunningBranchRuns(ctx)
	if err != nil || len(running) != 1 || running[0].ID != r1 {
		t.Fatalf("running = %+v, %v", running, err)
	}
	if err := st.FinishBranchRun(ctx, r1, RunFailed, "pull: boom"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBranchRunCheck(ctx, r1, 987654321012); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetBranchRun(ctx, r1)
	if got.Status != RunFailed || got.Failure != "pull: boom" || got.CheckRunID != 987654321012 {
		t.Fatalf("run = %+v", got)
	}
	if running, _ = st.RunningBranchRuns(ctx); len(running) != 0 {
		t.Fatalf("running = %+v", running)
	}
	if err := st.FinishBranchRun(ctx, r2, RunSuccess, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finish unknown: %v", err)
	}

	// Listing: by owner, newest first, paged, with the total.
	time.Sleep(5 * time.Millisecond) // created_at orders the runs
	if err := st.CreateBranchRun(ctx, newRun(r2, a, "push")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := st.CreateBranchRun(ctx, newRun(r3, b, "create")); err != nil {
		t.Fatal(err)
	}
	list, total, err := st.ListBranchRuns(ctx, a, 0, 10)
	if err != nil || total != 2 || len(list) != 2 || list[0].ID != r2 || list[1].ID != r1 {
		t.Fatalf("list = %+v total=%d err=%v", list, total, err)
	}
	if len(list[0].Steps) != len(BranchRunSteps) || list[0].Steps[0].Name != "clone" {
		t.Fatalf("listed steps = %+v", list[0].Steps)
	}
	page, total, _ := st.ListBranchRuns(ctx, a, 1, 1)
	if total != 2 || len(page) != 1 || page[0].ID != r1 {
		t.Fatalf("page = %+v total=%d", page, total)
	}
	if page, total, _ = st.ListBranchRuns(ctx, a, 5, 10); total != 2 || len(page) != 0 {
		t.Fatalf("past the end = %+v total=%d", page, total)
	}
	if other, total, _ := st.ListBranchRuns(ctx, b, 0, 10); total != 1 || other[0].ID != r3 {
		t.Fatalf("owner b = %+v", other)
	}
	if none, total, _ := st.ListBranchRuns(ctx, "cccccccccccccccccccc", 0, 10); total != 0 || len(none) != 0 {
		t.Fatalf("no owner = %+v", none)
	}

	// Pruning: old runs go, and so do the runs of a project that is gone.
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	if n, err := st.PruneBranchRuns(ctx, past, past); err != nil || n != 0 {
		t.Fatalf("prune nothing: %d, %v", n, err)
	}
	if err := reg.DeleteProject(ctx, b); err != nil {
		t.Fatal(err)
	}
	// A young run of a missing project may be a create that has not written its project yet.
	if n, err := st.PruneBranchRuns(ctx, past, past); err != nil || n != 0 {
		t.Fatalf("prune a young orphan: %d, %v", n, err)
	}
	if n, err := st.PruneBranchRuns(ctx, past, future); err != nil || n != 1 {
		t.Fatalf("prune orphan: %d, %v", n, err)
	}
	if _, err := st.GetBranchRun(ctx, r3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned run: %v", err)
	}
	if n, err := st.PruneBranchRuns(ctx, future, past); err != nil || n != 2 {
		t.Fatalf("prune old: %d, %v", n, err)
	}

	// The Git branch of a default branch.
	if g, err := st.GetDefaultBranchGit(ctx, a); err != nil || g != "" {
		t.Fatalf("default git = %q, %v", g, err)
	}
	if err := st.SetDefaultBranchGit(ctx, a, "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultBranchGit(ctx, a, "release"); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.GetDefaultBranchGit(ctx, a); g != "release" {
		t.Fatalf("default git = %q", g)
	}
	if err := st.SetDefaultBranchGit(ctx, "dddddddddddddddddddd", "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set for an unknown project: %v", err)
	}
}

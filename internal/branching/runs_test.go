package branching

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// stepStates returns the status of every step of the run, in the order of the DAG.
func stepStates(t *testing.T, h *harness, runID string) map[string]string {
	t.Helper()
	rec, err := h.reg.GetBranchRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("run %s: %v", runID, err)
	}
	if len(rec.Steps) != len(registry.BranchRunSteps) {
		t.Fatalf("run has %d steps, want %d: %+v", len(rec.Steps), len(registry.BranchRunSteps), rec.Steps)
	}
	out := map[string]string{}
	for i, s := range rec.Steps {
		if s.Name != registry.BranchRunSteps[i] {
			t.Fatalf("step %d is %s, want %s", i, s.Name, registry.BranchRunSteps[i])
		}
		out[s.Name] = s.Status
	}
	return out
}

func wantSteps(t *testing.T, got map[string]string, want string) {
	t.Helper()
	// want lists the statuses of clone,pull,health,configure,migrate,seed,deploy.
	var have []string
	for _, n := range registry.BranchRunSteps {
		have = append(have, got[n])
	}
	if strings.Join(have, ",") != want {
		t.Fatalf("steps = %s\nwant    %s", strings.Join(have, ","), want)
	}
}

func latestRun(t *testing.T, h *harness, owner string) *registry.BranchRun {
	t.Helper()
	rec, err := h.svc.LatestRun(context.Background(), owner)
	if err != nil || rec == nil {
		t.Fatalf("no run listed under %s: %v", owner, err)
	}
	return rec
}

func TestCreateRecordsARunWithSevenSteps(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.svc.SetSeed(ctx, parentRef, "insert into t values (1)")
	// Hold the engine inside the creation: the run exists, with all its steps, before it is done.
	h.eng.hold = make(chan struct{})
	b, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "feat"})
	if err != nil {
		t.Fatal(err)
	}
	run := latestRun(t, h, b.Ref)
	if run.Status != registry.RunRunning || run.Op != "create" || run.OwnerRef != b.Ref || run.ParentRef != parentRef {
		t.Fatalf("run = %+v", run)
	}
	wantSteps(t, stepStates(t, h, run.ID), "PAUSED,EXITED,RUNNING,PAUSED,CREATED,CREATED,CREATED")
	if got := DeriveRunStatus(run.Steps); got != registry.RunRunning {
		t.Fatalf("derived status = %s", got)
	}
	close(h.eng.hold)
	done := h.wait(b.Ref)
	h.mustState(done, registry.BranchFunctionsDeployed)

	run = latestRun(t, h, b.Ref)
	wantSteps(t, stepStates(t, h, run.ID), "PAUSED,EXITED,EXITED,PAUSED,EXITED,EXITED,EXITED")
	if run.Status != registry.RunSuccess || run.Failure != "" || DeriveRunStatus(run.Steps) != registry.RunSuccess {
		t.Fatalf("run = %+v", run)
	}
	logs, err := h.svc.RunLogs(ctx, b.Ref, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[migrate] ", "replaying 3 migration(s)", "[seed] ", "applying the seed", "[deploy] ", "schema only: 3 migration(s) replayed"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

func TestCreateWithoutASeedSkipsTheSeedStep(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", nil)
	wantSteps(t, stepStates(t, h, latestRun(t, h, b.Ref).ID), "PAUSED,EXITED,EXITED,PAUSED,EXITED,PAUSED,EXITED")
}

func TestFailingMigrateKillsTheRun(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", nil)
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	h.db.applyErr[parentRef] = errors.New("relation f already exists")
	runID, err := h.svc.Merge(context.Background(), b.Ref, ActionInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "already exists") {
		t.Fatalf("detail = %q", got.Detail)
	}
	rec, err := h.reg.GetBranchRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != runID || rec.OwnerRef != parentRef {
		t.Fatalf("run = %+v", rec)
	}
	// The failed step is DEAD and everything after it is PAUSED; the run is failed by Studio's rule too.
	wantSteps(t, stepStates(t, h, runID), "PAUSED,EXITED,PAUSED,PAUSED,DEAD,PAUSED,PAUSED")
	if rec.Status != registry.RunFailed || !strings.HasPrefix(rec.Failure, "migrate: ") || !strings.Contains(rec.Failure, "already exists") {
		t.Fatalf("run = %+v", rec)
	}
	if DeriveRunStatus(rec.Steps) != registry.RunFailed {
		t.Fatalf("derived = %s", DeriveRunStatus(rec.Steps))
	}
	logs, _ := h.svc.RunLogs(context.Background(), parentRef, runID)
	if !strings.Contains(logs, "[migrate]") || !strings.Contains(logs, "FAILED: ") {
		t.Fatalf("log = %q", logs)
	}
	// The parent's own status shows the failed merge, and the next successful merge clears it.
	def, _ := h.svc.Resolve(context.Background(), parentRef)
	if def.State != registry.BranchMigrationsFailed || !strings.Contains(def.Detail, "already exists") {
		t.Fatalf("default branch = %s %q", def.State, def.Detail)
	}
	delete(h.db.applyErr, parentRef)
	if _, err := h.svc.Merge(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchFunctionsDeployed)
	if def, _ = h.svc.Resolve(context.Background(), parentRef); def.State != registry.BranchFunctionsDeployed {
		t.Fatalf("default branch = %s", def.State)
	}
}

func TestDivergenceIsADeadMigrateStepWithTheReason(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", nil)
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	h.db.migs[parentRef] = append(h.db.migs[parentRef], mig("20260105000000", "hotfix", "create table hf (id int)"))
	runID, err := h.svc.Merge(context.Background(), b.Ref, ActionInput{})
	if err != nil {
		t.Fatalf("the call itself succeeds (201): %v", err)
	}
	h.wait(b.Ref)
	wantSteps(t, stepStates(t, h, runID), "PAUSED,EXITED,PAUSED,PAUSED,DEAD,PAUSED,PAUSED")
	logs, _ := h.svc.RunLogs(context.Background(), parentRef, runID)
	if !strings.Contains(logs, "histories diverged") {
		t.Fatalf("log = %q", logs)
	}
}

func TestPushAndResetRecordRunsOwnedByTheBranch(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	h.db.migs[parentRef] = append(h.db.migs[parentRef], mig("20260105000000", "hotfix", "select 1"))
	push, err := h.svc.Push(ctx, b.Ref, ActionInput{})
	if err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchFunctionsDeployed)
	wantSteps(t, stepStates(t, h, push), "PAUSED,EXITED,PAUSED,PAUSED,EXITED,PAUSED,EXITED")

	reset, err := h.svc.Reset(ctx, b.Ref, ActionInput{})
	if err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchFunctionsDeployed)
	wantSteps(t, stepStates(t, h, reset), "PAUSED,EXITED,EXITED,PAUSED,EXITED,PAUSED,EXITED")
	rec, _ := h.reg.GetBranchRun(ctx, reset)
	if rec.Op != "reset" || rec.OwnerRef != b.Ref || rec.Status != registry.RunSuccess {
		t.Fatalf("reset run = %+v", rec)
	}
	// Newest first under the branch: reset, push, create.
	runs, total, err := h.svc.ListRuns(ctx, b.Ref, 0, 10)
	if err != nil || total != 3 || runs[0].ID != reset || runs[1].ID != push || runs[2].Op != "create" {
		t.Fatalf("runs = %v total=%d err=%v", runs, total, err)
	}
}

func TestRunOwnerRefMatrix(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	merge, err := h.svc.Merge(ctx, b.Ref, ActionInput{})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(b.Ref)
	create := latestRunOf(t, h, b.Ref, "create")
	cases := []struct {
		name, run, ref string
		visible        bool
	}{
		{"merge under the parent", merge, parentRef, true},
		{"merge under the branch", merge, b.Ref, false},
		{"create under the branch", create, b.Ref, true},
		{"create under the parent", create, parentRef, false},
		{"an unknown ref", merge, "zzzzzzzzzzzzzzzzzzzz", false},
		{"a run id that is not a uuid", "nope", parentRef, false},
	}
	for _, c := range cases {
		_, err := h.svc.GetRun(ctx, c.ref, c.run)
		if c.visible != (err == nil) || (!c.visible && !errors.Is(err, ErrNotFound)) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		_, err = h.svc.RunLogs(ctx, c.ref, c.run)
		if c.visible != (err == nil) {
			t.Errorf("%s: logs err = %v", c.name, err)
		}
	}
	// The listing of the parent has the merge and not the create.
	runs, total, _ := h.svc.ListRuns(ctx, parentRef, 0, 10)
	if total != 1 || runs[0].ID != merge {
		t.Fatalf("parent's runs = %+v", runs)
	}
}

func latestRunOf(t *testing.T, h *harness, owner, op string) string {
	t.Helper()
	runs, _, _ := h.svc.ListRuns(context.Background(), owner, 0, 50)
	for _, r := range runs {
		if r.Op == op {
			return r.ID
		}
	}
	t.Fatalf("no %s run under %s", op, owner)
	return ""
}

func TestRecoveryMarksRunsOfAGoneProcessDead(t *testing.T) {
	deadPID := func() int {
		cmd := exec.Command("true")
		if err := cmd.Run(); err != nil {
			t.Skipf("cannot start a process to get a dead pid: %v", err)
		}
		return cmd.Process.Pid
	}()
	h := newHarness(t, nil)
	ctx := context.Background()
	host, _ := os.Hostname()
	b := h.create("feat", nil)
	st := registry.BranchRunsOf(h.reg)
	mk := func(id, h2 string, pid int, inst string) {
		r := &registry.BranchRun{ID: id, OwnerRef: b.Ref, BranchRef: b.Ref, ParentRef: parentRef, Op: "push", Status: registry.RunRunning,
			RunnerHost: h2, RunnerPID: pid, RunnerInstance: inst}
		for _, n := range registry.BranchRunSteps {
			r.Steps = append(r.Steps, registry.BranchRunStep{Name: n, Status: registry.StepCreated})
		}
		r.Steps[1].Status = registry.StepRunning // pull is open
		if err := st.CreateBranchRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	const gone, alive, elsewhere = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	mk(gone, host, deadPID, "old")
	mk(alive, host, os.Getppid(), "other")
	mk(elsewhere, host+"-other", deadPID, "old")
	// The branch is left busy by the interrupted push.
	h.svc.setState(ctx, b.Ref, registry.BranchRunningMigration, "applying the parent's migrations", nil)

	if got := h.svc.recoverRuns(ctx, true); len(got) != 1 || got[0] != gone {
		t.Fatalf("a dry run found %v", got)
	}
	if rec, _ := st.GetBranchRun(ctx, gone); rec.Status != registry.RunRunning {
		t.Fatal("a dry run changed a run")
	}
	if _, err := h.svc.Sweep(ctx, false); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetBranchRun(ctx, gone)
	if rec.Status != registry.RunFailed || !strings.Contains(rec.Failure, "interrupted by restart") {
		t.Fatalf("run = %+v", rec)
	}
	wantSteps(t, stepStates(t, h, gone), "CREATED,DEAD,PAUSED,PAUSED,PAUSED,PAUSED,PAUSED")
	if DeriveRunStatus(rec.Steps) != registry.RunFailed {
		t.Fatalf("derived = %s", DeriveRunStatus(rec.Steps))
	}
	got, _ := h.svc.Resolve(ctx, b.Ref)
	h.mustState(got, registry.BranchMigrationsFailed)
	// A process that is alive keeps its run, and so does one on another host until it goes stale.
	for _, id := range []string{alive, elsewhere} {
		if r, _ := st.GetBranchRun(ctx, id); r.Status != registry.RunRunning {
			t.Fatalf("run %s = %s", id, r.Status)
		}
	}
	h.advance(time.Hour)
	h.reg.Now = h.clock // the registry stamps updated_at with the test clock from here on
	h.advance(time.Hour)
	if _, err := h.svc.Sweep(ctx, false); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.GetBranchRun(ctx, elsewhere); r.Status != registry.RunFailed || !strings.Contains(r.Failure, "no process was working") {
		t.Fatalf("stale run = %+v", r)
	}
}

func TestRunRecorderStateMachine(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	// A Git-driven run: the caller starts clone and configure itself, and finishes the run.
	runID, err := h.svc.Push(ctx, b.Ref, ActionInput{})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(b.Ref)
	rec, err := h.svc.Recorder(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Recorder(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	var nilRun *Run
	nilRun.Phase(StepSeed) // a missing run is a no-op, not a panic
	nilRun.Log("x")
	nilRun.Step(StepPull).Start()

	// Fresh run by hand.
	r := &registry.BranchRun{ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", OwnerRef: b.Ref, BranchRef: b.Ref, ParentRef: parentRef, Op: "create", Status: registry.RunRunning}
	for _, n := range registry.BranchRunSteps {
		r.Steps = append(r.Steps, registry.BranchRunStep{Name: n, Status: registry.StepCreated})
	}
	if err := h.reg.CreateBranchRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	rec, _ = h.svc.Recorder(ctx, r.ID)
	rec.Step(StepClone).Start()
	rec.Log("fetching supabase/")
	rec.Step(StepClone).Finish()
	rec.Phase(StepPull)
	rec.Phase(StepHealth)
	rec.Step(StepConfigure).Skip()
	rec.Phase(StepMigrate) // skips configure (already paused)
	rec.Phase(StepPull)    // going back does nothing
	rec.Phase(StepDeploy)  // seed is skipped
	wantSteps(t, stepStates(t, h, r.ID), "EXITED,EXITED,EXITED,PAUSED,EXITED,PAUSED,RUNNING")
	rec.Step(StepDeploy).Fail(errors.New("bundle failed"))
	wantSteps(t, stepStates(t, h, r.ID), "EXITED,EXITED,EXITED,PAUSED,EXITED,PAUSED,DEAD")
	if got, _ := h.reg.GetBranchRun(ctx, r.ID); got.Status != registry.RunFailed || got.Failure != "deploy: bundle failed" {
		t.Fatalf("run = %+v", got)
	}
	// A second failure or a finish after one changes nothing.
	if step := rec.Finish(nil); step != StepDeploy {
		t.Fatalf("finish after a failure = %q", step)
	}
	logs, _ := h.svc.RunLogs(ctx, b.Ref, r.ID)
	if !strings.Contains(logs, "[clone] ") || !strings.Contains(logs, "fetching supabase/") || !strings.Contains(logs, "[deploy] ") {
		t.Fatalf("log = %q", logs)
	}
}

func TestDeriveRunStatus(t *testing.T) {
	steps := func(sts ...string) []registry.BranchRunStep {
		var out []registry.BranchRunStep
		for _, s := range sts {
			out = append(out, registry.BranchRunStep{Status: s})
		}
		return out
	}
	for name, c := range map[string]struct {
		in   []registry.BranchRunStep
		want string
	}{
		"no steps is running, never success": {nil, registry.RunRunning},
		"all exited or paused":               {steps("EXITED", "PAUSED", "EXITED"), registry.RunSuccess},
		"one created":                        {steps("EXITED", "CREATED"), registry.RunRunning},
		"one running":                        {steps("EXITED", "RUNNING", "CREATED"), registry.RunRunning},
		"dead with paused dependents":        {steps("EXITED", "DEAD", "PAUSED"), registry.RunFailed},
		"dead beats running":                 {steps("DEAD", "RUNNING"), registry.RunFailed},
	} {
		if got := DeriveRunStatus(c.in); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// A failure in the deploy step ends FUNCTIONS_FAILED; the migrations having passed shows as
// MIGRATIONS_PASSED while the functions deploy.
func TestDeployStepStatuses(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	for _, tc := range []struct {
		fail      bool
		wantState registry.BranchState
	}{{false, registry.BranchFunctionsDeployed}, {true, registry.BranchFunctionsFailed}} {
		r, err := h.svc.beginRun(ctx, b.Ref, "push", b.Ref, parentRef)
		if err != nil {
			t.Fatal(err)
		}
		h.svc.setState(ctx, b.Ref, registry.BranchRunningMigration, "starting", nil)
		var during registry.BranchState
		h.svc.spawn(r, b.Ref, parentRef, func(ctx context.Context) (string, error) {
			h.svc.phase(ctx, StepPull)
			h.svc.phase(ctx, StepMigrate)
			h.svc.phase(ctx, StepDeploy)
			cur, _ := h.svc.Resolve(ctx, b.Ref)
			during = cur.State
			if tc.fail {
				return "", errors.New("function bundle failed")
			}
			return "ok", nil
		})
		got := h.wait(b.Ref)
		if during != registry.BranchMigrationsPassed {
			t.Errorf("state while the functions deploy = %s", during)
		}
		if got.State != tc.wantState {
			t.Errorf("fail=%v: state = %s (%s)", tc.fail, got.State, got.Detail)
		}
		want := "PAUSED,EXITED,PAUSED,PAUSED,EXITED,PAUSED,EXITED"
		if tc.fail {
			want = "PAUSED,EXITED,PAUSED,PAUSED,EXITED,PAUSED,DEAD"
		}
		wantSteps(t, stepStates(t, h, r.id), want)
	}
}

// Two operations on one branch do not run at once; the run of the refused one is not recorded.
func TestOnlyOneRunPerBranchAtATime(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	before, _, _ := h.svc.ListRuns(ctx, b.Ref, 0, 50)
	release := make(chan struct{})
	r, err := h.svc.beginRun(ctx, b.Ref, "merge", parentRef, parentRef)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.spawn(r, b.Ref, parentRef, func(ctx context.Context) (string, error) {
		h.svc.phase(ctx, StepPull)
		<-release
		return "merged", nil
	})
	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "busy with merge") {
		t.Fatalf("push during merge: %v", err)
	}
	if _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reset during merge: %v", err)
	}
	// A client cannot patch the steps of a run this node is working on.
	if _, err := h.svc.SetRunSteps(ctx, parentRef, r.id, map[string]string{"pull": "EXITED"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("patch of a live run: %v", err)
	}
	close(release)
	h.wait(b.Ref)
	after, _, _ := h.svc.ListRuns(ctx, b.Ref, 0, 50)
	if len(after) != len(before) {
		t.Fatalf("the refused operations left runs under the branch: %d -> %d", len(before), len(after))
	}
	if mine, _, _ := h.svc.ListRuns(ctx, parentRef, 0, 50); len(mine) != 1 || mine[0].Status != registry.RunSuccess {
		t.Fatalf("parent's runs = %+v", mine)
	}
}

func TestSetRunStepsFollowsTheSteps(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	// A run an outside job drives.
	st := registry.BranchRunsOf(h.reg)
	r := &registry.BranchRun{ID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", OwnerRef: b.Ref, BranchRef: b.Ref, ParentRef: parentRef, Op: "push", Status: registry.RunRunning}
	for _, n := range registry.BranchRunSteps {
		r.Steps = append(r.Steps, registry.BranchRunStep{Name: n, Status: registry.StepCreated})
	}
	if err := st.CreateBranchRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	rec, err := h.svc.SetRunSteps(ctx, b.Ref, r.ID, map[string]string{"clone": "RUNNING", "pull": "RUNNING"})
	if err != nil || rec.Status != registry.RunRunning {
		t.Fatalf("running: %+v %v", rec, err)
	}
	all := map[string]string{}
	for _, n := range registry.BranchRunSteps {
		all[n] = "EXITED"
	}
	if rec, err = h.svc.SetRunSteps(ctx, b.Ref, r.ID, all); err != nil || rec.Status != registry.RunSuccess {
		t.Fatalf("done: %+v %v", rec, err)
	}
	if rec, err = h.svc.SetRunSteps(ctx, b.Ref, r.ID, map[string]string{"migrate": "DEAD", "deploy": "PAUSED"}); err != nil ||
		rec.Status != registry.RunFailed || !strings.HasPrefix(rec.Failure, "migrate: ") {
		t.Fatalf("failed: %+v %v", rec, err)
	}
	if _, err := h.svc.SetRunSteps(ctx, b.Ref, r.ID, map[string]string{"pull": "ASLEEP"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status: %v", err)
	}
	if _, err := h.svc.SetRunSteps(ctx, b.Ref, r.ID, map[string]string{"teleport": "EXITED"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad step: %v", err)
	}
	if _, err := h.svc.SetRunSteps(ctx, parentRef, r.ID, all); !errors.Is(err, ErrNotFound) {
		t.Fatalf("under the wrong ref: %v", err)
	}
}

func TestLongLogsAreCapped(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	rec, err := h.svc.Recorder(ctx, latestRun(t, h, b.Ref).ID)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 1000)
	for range 400 {
		rec.Log(line)
	}
	logs, _ := h.svc.RunLogs(ctx, b.Ref, rec.ID())
	if len(logs) > maxStepLog+1024 || !strings.Contains(logs, "log truncated") {
		t.Fatalf("log is %d bytes, truncated=%v", len(logs), strings.Contains(logs, "log truncated"))
	}
}

func TestOldRunsAndRunsOfGoneProjectsArePruned(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	if n := len(h.svc.mustRuns(t, b.Ref)); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	// A year on, the next operation prunes the create run.
	h.advance(365 * 24 * time.Hour)
	h.reg.Now = h.clock
	h.svc.lastPrune = time.Time{}
	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.wait(b.Ref)
	if runs := h.svc.mustRuns(t, b.Ref); len(runs) != 1 || runs[0].Op != "push" {
		t.Fatalf("runs = %+v", runs)
	}
}

func (s *Service) mustRuns(t *testing.T, owner string) []registry.BranchRun {
	t.Helper()
	runs, _, err := s.ListRuns(context.Background(), owner, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestBranchRunsGathersTheMergesToo(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	a, b := h.create("a", nil), h.create("b", nil)
	h.db.migs[a.Ref] = append(h.db.migs[a.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	merge, err := h.svc.Merge(ctx, a.Ref, ActionInput{})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(a.Ref)
	if _, err := h.svc.Push(ctx, a.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.wait(a.Ref)
	got, err := h.svc.BranchRuns(ctx, a, 10)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, r := range got {
		ops = append(ops, r.Op)
	}
	// Newest first: the push and the create are listed under the branch, the merge under its parent.
	if strings.Join(ops, ",") != "push,merge,create" || got[1].ID != merge {
		t.Fatalf("runs of a = %v", ops)
	}
	// Another branch's runs do not show up.
	if other, _ := h.svc.BranchRuns(ctx, b, 10); len(other) != 1 || other[0].Op != "create" {
		t.Fatalf("runs of b = %+v", other)
	}
	if limited, _ := h.svc.BranchRuns(ctx, a, 2); len(limited) != 2 {
		t.Fatalf("limit: %d", len(limited))
	}
}

func TestCreateReturnsTheRunItStarted(t *testing.T) {
	h := newHarness(t, nil)
	b, err := h.svc.Create(context.Background(), parentRef, CreateInput{Name: "feat"})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(b.Ref)
	if !IsUUID(b.RunID) || b.RunID != latestRun(t, h, b.Ref).ID {
		t.Fatalf("RunID = %q, newest run %q", b.RunID, latestRun(t, h, b.Ref).ID)
	}
	if got, _ := h.svc.Resolve(context.Background(), b.Ref); got.RunID != "" {
		t.Fatalf("a branch read later has a RunID: %q", got.RunID)
	}
	// A create that fails before the project exists returns the error and no branch.
	h.eng.failBeforeRow = errors.New("engine unavailable")
	if got, err := h.svc.Create(context.Background(), parentRef, CreateInput{Name: "bad"}); err == nil || got != nil {
		t.Fatalf("failed create = %+v, %v", got, err)
	}
}

func TestStartRunIsDrivenByTheCaller(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	rec, err := h.svc.StartRun(ctx, "create", b.Ref, b.Ref, parentRef)
	if err != nil {
		t.Fatal(err)
	}
	wantSteps(t, stepStates(t, h, rec.ID()), "CREATED,CREATED,CREATED,CREATED,CREATED,CREATED,CREATED")
	for _, n := range registry.BranchRunSteps {
		rec.Phase(n)
	}
	rec.Finish(nil)
	wantSteps(t, stepStates(t, h, rec.ID()), "EXITED,EXITED,EXITED,EXITED,EXITED,EXITED,EXITED")
	got, _ := h.reg.GetBranchRun(ctx, rec.ID())
	if got.Status != registry.RunSuccess || got.OwnerRef != b.Ref {
		t.Fatalf("run = %+v", got)
	}
	if after, _ := h.svc.Resolve(ctx, b.Ref); after.State != registry.BranchFunctionsDeployed {
		t.Fatalf("the service changed the branch's status: %s", after.State)
	}
}

// The store takes text: a step log cut in the middle of a character, invalid bytes or a NUL would
// make Postgres refuse the chunk, and the log would lose it and the truncation marker.
func TestLogsStayValidUTF8(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	rec, err := h.svc.Recorder(ctx, latestRun(t, h, b.Ref).ID)
	if err != nil {
		t.Fatal(err)
	}
	rec.Log("bad \xff\xfe bytes\x00here")
	logs, _ := h.svc.RunLogs(ctx, b.Ref, rec.ID())
	if !utf8.ValidString(logs) || strings.Contains(logs, "\x00") || !strings.Contains(logs, "bad \uFFFD byteshere") {
		t.Fatalf("log = %q", logs)
	}
	// Each step starts its first line with a different length, so that the cap falls on each of the
	// three bytes of a character in turn.
	for i, step := range []string{StepPull, StepHealth, StepMigrate} {
		rec.mu.Lock()
		rec.logTo(step, strings.Repeat("a", i))
		rec.logTo(step, strings.Repeat("\u65e5", maxStepLog/3))
		rec.mu.Unlock()
	}
	for _, step := range []string{StepPull, StepHealth, StepMigrate} {
		got, err := h.reg.GetBranchRun(ctx, rec.ID())
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range got.Steps {
			if st.Name != step {
				continue
			}
			if !utf8.ValidString(st.Log) || !strings.Contains(st.Log, "log truncated") || len(st.Log) > maxStepLog+1024 {
				t.Errorf("step %s: valid=%v truncated=%v bytes=%d", step, utf8.ValidString(st.Log), strings.Contains(st.Log, "log truncated"), len(st.Log))
			}
		}
	}
}

// A limit of zero or less is "the default", not "no runs": the CLI passes --limit as given.
func TestBranchRunsLimitZeroMeansTheDefault(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	for _, limit := range []int{0, -1} {
		got, err := h.svc.BranchRuns(ctx, b, limit)
		if err != nil || len(got) != 1 || got[0].Op != "create" {
			t.Fatalf("limit %d: %+v %v", limit, got, err)
		}
	}
}

// The default branch reads the failed merge of a branch only while that branch is still failed:
// deleting it, or a later success, clears the status, and the status falls back to the merge before.
func TestDefaultBranchFailureEndsWithTheBranch(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	a, b := h.create("a", nil), h.create("b", nil)
	h.db.migs[a.Ref] = append(h.db.migs[a.Ref], mig("20260110000000", "fa", "create table fa (id int)"))
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260111000000", "fb", "create table fb (id int)"))
	if _, err := h.svc.Merge(ctx, a.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(a.Ref), registry.BranchFunctionsDeployed)
	// b was made before a's merge: its history diverges from the parent's now.
	if _, err := h.svc.Merge(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchMigrationsFailed)
	defState := func() registry.BranchState {
		t.Helper()
		def, err := h.svc.Resolve(ctx, parentRef)
		if err != nil {
			t.Fatal(err)
		}
		return def.State
	}
	if got := defState(); got != registry.BranchMigrationsFailed {
		t.Fatalf("default branch after the failed merge = %s", got)
	}
	// The branch works again: nothing is left to fix.
	if _, err := h.svc.Update(ctx, b.Ref, UpdateInput{State: ptr(registry.BranchFunctionsDeployed)}); err != nil {
		t.Fatal(err)
	}
	if got := defState(); got != registry.BranchFunctionsDeployed {
		t.Fatalf("default branch once the branch works = %s (the merge of a before it succeeded)", got)
	}
	// Failed again, then the branch is deleted.
	if _, err := h.svc.Update(ctx, b.Ref, UpdateInput{State: ptr(registry.BranchMigrationsFailed)}); err != nil {
		t.Fatal(err)
	}
	if got := defState(); got != registry.BranchMigrationsFailed {
		t.Fatalf("default branch with the branch failed again = %s", got)
	}
	if _, err := h.svc.Delete(ctx, b.Ref, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := defState(); got != registry.BranchFunctionsDeployed {
		t.Fatalf("default branch after the failed branch was deleted = %s", got)
	}
}

// While a run deploys the functions the branch reads MIGRATIONS_PASSED, which alone is also how a
// branch at rest looks: another process (the CLI, the sweeper of a second daemon) sees the run.
func TestADeployingBranchIsBusyForOtherProcesses(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	h.svc.setState(ctx, b.Ref, registry.BranchMigrationsPassed, "", nil)
	cur, _ := h.svc.Resolve(ctx, b.Ref)
	if h.svc.inFlight(ctx, cur) || h.svc.checkIdle(ctx, cur) != nil {
		t.Fatal("a branch at MIGRATIONS_PASSED with no run is at rest")
	}
	// A run of another process, still going, and a lapsed expiry that the sweeper would act on.
	host, _ := os.Hostname()
	run := &registry.BranchRun{ID: secrets.NewUUID(), OwnerRef: b.Ref, BranchRef: b.Ref, ParentRef: parentRef, Op: "push",
		Status: registry.RunRunning, RunnerHost: host, RunnerPID: os.Getppid(), RunnerInstance: "other"}
	if err := h.reg.CreateBranchRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	h.svc.event(ctx, b.Ref, "branch.push.started", map[string]any{"operation": "push", "owner": map[string]any{"host": host, "pid": os.Getppid(), "instance": "other"}})
	p, _ := h.reg.GetProject(ctx, b.Ref)
	past := h.clock().Add(-time.Hour)
	p.Branch.ExpiresAt = &past
	if err := h.reg.UpdateBranchSettings(ctx, b.Ref, p.Branch); err != nil {
		t.Fatal(err)
	}
	cur, _ = h.svc.Resolve(ctx, b.Ref)
	if !h.svc.inFlight(ctx, cur) {
		t.Fatal("a deploy step of another process is in flight")
	}
	if err := h.svc.checkIdle(ctx, cur); !errors.Is(err, ErrConflict) {
		t.Fatalf("an operation on a deploying branch: %v", err)
	}
	res, err := h.svc.Sweep(ctx, false)
	if err != nil || len(res.Expired) != 0 || len(res.Stale) != 0 {
		t.Fatalf("sweep during the deploy: %+v %v", res, err)
	}
	if _, err := h.svc.Resolve(ctx, b.Ref); err != nil {
		t.Fatalf("the sweeper deleted a deploying branch: %v", err)
	}
	// The run ends: the branch is free again (and now lapsed).
	if err := h.reg.FinishBranchRun(ctx, run.ID, registry.RunSuccess, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.checkIdle(ctx, cur); err != nil {
		t.Fatalf("after the run ended: %v", err)
	}
	if res, err = h.svc.Sweep(ctx, false); err != nil || len(res.Expired) != 1 {
		t.Fatalf("sweep after the deploy: %+v %v", res, err)
	}
}

func TestWakeResumesAPausedBranchAndLeavesARunningOneAlone(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("feat", nil)
	if got, err := h.svc.Wake(ctx, b.Ref); err != nil || got.Ref != b.Ref || len(h.eng.resumed) != 0 {
		t.Fatalf("wake of a running branch = %+v, %v, resumes %v", got, err, h.eng.resumed)
	}
	// Drive refuses a paused branch; Wake brings it back and Drive then takes it.
	if err := h.reg.SetProjectStatus(ctx, b.Ref, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Drive(ctx, b.Ref, "push", func(context.Context, *Run) (string, error) { return "", nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("drive of a paused branch: %v", err)
	}
	got, err := h.svc.Wake(ctx, b.Ref)
	if err != nil || got.ProjectStatus != registry.StatusActiveHealthy || len(h.eng.resumed) != 1 || h.eng.resumed[0] != b.Ref {
		t.Fatalf("wake of a paused branch = %+v, %v, resumes %v", got, err, h.eng.resumed)
	}
	if _, err := h.svc.Drive(ctx, b.Ref, "push", func(context.Context, *Run) (string, error) { return "ok", nil }); err != nil {
		t.Fatalf("drive after the wake: %v", err)
	}
	h.wait(b.Ref)
	// The default branch is not a branch to wake.
	if _, err := h.svc.Wake(ctx, parentRef); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wake of the default branch: %v", err)
	}
}

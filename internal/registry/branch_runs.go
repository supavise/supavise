package registry

import (
	"context"
	"slices"
	"time"
)

// BranchRunSteps are the steps of an action run, in the order of the DAG the Management API
// documents (clone and configure exist only in the Git flow).
var BranchRunSteps = []string{"clone", "pull", "health", "configure", "migrate", "seed", "deploy"}

// Status of a step, in the Management API's vocabulary (ActionRunResponse.run_steps[].status).
// A step is DEAD when it failed and PAUSED when it was skipped or cannot run because a step before
// it failed.
const (
	StepCreated = "CREATED"
	StepRunning = "RUNNING"
	StepExited  = "EXITED"
	StepPaused  = "PAUSED"
	StepDead    = "DEAD"
)

// Status of a run as a whole. The Management API has none: Studio derives it from the steps, and
// BranchRun.Status is the same answer stored.
const (
	RunRunning = "RUNNING"
	RunSuccess = "SUCCESS"
	RunFailed  = "FAILED"
)

// BranchRunStep is one step of a run with its log.
type BranchRunStep struct {
	Name      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	Log       string
}

// BranchRun is the record of one create, push, merge or reset of a branch (migration 1500).
type BranchRun struct {
	ID string // UUID; the workflow_run_id of the Management API
	// OwnerRef is the project the run is listed under: the parent for a merge, the branch for a
	// create, push and reset. The run answers 404 under any other ref.
	OwnerRef  string
	BranchRef string
	ParentRef string
	Op        string // create, merge, push, reset or deploy (a deployment of the production branch, listed under the project itself)
	Status    string // RunRunning, RunSuccess or RunFailed
	// CheckRunID is the GitHub check run that reports the run, 0 when there is none.
	CheckRunID int64
	// Workdir is the repository directory a Git-driven run reads (supabase/ by default), "" otherwise.
	Workdir string
	// Failure is why the run failed, "" otherwise.
	Failure string
	// RunnerHost, RunnerPID and RunnerInstance say which service works on the run; a restart
	// uses them to tell an interrupted run from one that another process still drives.
	RunnerHost     string
	RunnerPID      int
	RunnerInstance string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Steps are in BranchRunSteps order.
	Steps []BranchRunStep
}

// BranchRunStepChange is one edit of a step.
type BranchRunStepChange struct {
	Name string
	// Status, when not empty, is the step's new status.
	Status string
	// Log is appended to the step's log, as given.
	Log string
	// OnlyFrom, when not empty, applies the change only to a step whose status is one of these:
	// a failed step pauses the steps after it that have not started, and leaves the rest alone.
	OnlyFrom []string
}

// BranchRunStore keeps the action runs of branches. Both registries implement it; the branching
// service reaches it through BranchRunsOf.
type BranchRunStore interface {
	// CreateBranchRun stores r with its Steps (all of them, in any order; ID is set by the caller).
	// CreatedAt and UpdatedAt are set by the store.
	CreateBranchRun(ctx context.Context, r *BranchRun) error
	// GetBranchRun returns the run with its steps and their logs, or ErrNotFound. It is the only
	// read that returns logs: a step log may be 256 KiB, and a list page holds many runs.
	GetBranchRun(ctx context.Context, id string) (*BranchRun, error)
	// ListBranchRuns returns the runs owned by ownerRef, newest first, from offset on and at most
	// limit of them, with the total number the owner has. Their steps carry no Log.
	ListBranchRuns(ctx context.Context, ownerRef string, offset, limit int) ([]BranchRun, int, error)
	// SetBranchRunSteps applies the changes in one step, then touches the run. ErrNotFound for an
	// unknown run or step.
	SetBranchRunSteps(ctx context.Context, id string, changes []BranchRunStepChange) error
	// FinishBranchRun sets the status of the run (RunSuccess or RunFailed) and the reason of a failure.
	FinishBranchRun(ctx context.Context, id, status, failure string) error
	// SetBranchRunCheck records the GitHub check run that reports the run.
	SetBranchRunCheck(ctx context.Context, id string, checkRunID int64) error
	// RunningBranchRuns returns the runs with status RunRunning, oldest first. Their steps carry no Log.
	RunningBranchRuns(ctx context.Context) ([]BranchRun, error)
	// PruneBranchRuns removes the runs created before `before`, and the runs created before
	// `orphans` whose owner is not a project any more (a create writes its run before the branch's
	// project exists, so a young run of a missing project may be one that is starting). It
	// returns how many it removed.
	PruneBranchRuns(ctx context.Context, before, orphans time.Time) (int, error)
	// GetDefaultBranchGit and SetDefaultBranchGit keep the Git branch a project's default branch
	// tracks (migration 1501). "" when none was set. SetDefaultBranchGit is ErrNotFound for an
	// unknown project.
	GetDefaultBranchGit(ctx context.Context, ref string) (string, error)
	SetDefaultBranchGit(ctx context.Context, ref, gitBranch string) error
}

// BranchRunsOf returns reg's BranchRunStore, or nil when it has none.
func BranchRunsOf(reg Registry) BranchRunStore {
	s, _ := reg.(BranchRunStore)
	return s
}

// StepIndex is the position of a step name in BranchRunSteps, or len(BranchRunSteps) for a name
// that is not one of them.
func StepIndex(name string) int {
	if i := slices.Index(BranchRunSteps, name); i >= 0 {
		return i
	}
	return len(BranchRunSteps)
}

// SortBranchRunSteps puts steps in BranchRunSteps order.
func SortBranchRunSteps(steps []BranchRunStep) {
	slices.SortStableFunc(steps, func(a, b BranchRunStep) int { return StepIndex(a.Name) - StepIndex(b.Name) })
}

var (
	_ BranchRunStore = (*Memory)(nil)
	_ BranchRunStore = (*Postgres)(nil)
)

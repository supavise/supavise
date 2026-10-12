package branching

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/supavise/supavise/internal/procutil"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// Action runs. Every create, push, merge and reset of a branch is a run with the seven steps of
// the Management API (clone, pull, health, configure, migrate, seed, deploy), a log per step and a
// status that Studio, the CLI, the MCP server and notify_url receivers read. The run is stored
// (internal/registry/branch_runs.go) with all its steps before the operation's HTTP call
// returns: Studio treats a run with no steps as running forever.
//
// An operation reports its progress with Phase: each call closes the step that was open and opens
// the next, and steps skipped in between are marked PAUSED. spawn finishes the run when the
// operation ends. Code outside the package that drives a run (the GitHub runner) gets a Run from
// Recorder and uses Step, Phase, Log and Finish.

// Step names, in the order of the DAG. clone and configure belong to the Git flow: a run starts
// with them PAUSED (skipped) and the Git flow starts them.
const (
	StepClone     = "clone"
	StepPull      = "pull"
	StepHealth    = "health"
	StepConfigure = "configure"
	StepMigrate   = "migrate"
	StepSeed      = "seed"
	StepDeploy    = "deploy"
)

// maxStepLog bounds the log of one step: a loop that logs must not fill the registry.
const maxStepLog = 256 << 10

// DefaultRunLimit is how many runs BranchRuns returns when the caller names no limit.
const DefaultRunLimit = 20

// runRetention is how long a finished run is kept.
const runRetention = 30 * 24 * time.Hour

// Run records the progress of one action run. A nil *Run does nothing, so that code can report to
// the run in its context without checking that there is one.
type Run struct {
	s   *Service
	id  string
	op  string
	ref string // the branch the run works on

	mu     sync.Mutex
	cur    string         // the step Phase opened and nothing has closed
	logged map[string]int // bytes logged per step
	failed string         // the step that failed
}

type runCtxKey struct{}

// runFrom returns the run an operation reports to, nil when ctx has none.
func runFrom(ctx context.Context) *Run {
	r, _ := ctx.Value(runCtxKey{}).(*Run)
	return r
}

// phase opens the named step of the run in ctx and closes the one before it. It is how the
// operations report progress; without a run in ctx it does nothing.
func (s *Service) phase(ctx context.Context, step string) {
	r := runFrom(ctx)
	r.Phase(step)
	if r != nil && step == StepDeploy {
		// The migrations are done: the branch says so while its functions are being deployed.
		if p, err := s.reg.GetProject(ctx, r.ref); err == nil && p.Branch != nil && p.Branch.State == registry.BranchRunningMigration {
			s.setState(ctx, r.ref, registry.BranchMigrationsPassed, "migrations applied; deploying Edge Functions", nil)
		}
	}
}

// logRun adds a line to the log of the run in ctx.
func (s *Service) logRun(ctx context.Context, format string, a ...any) {
	runFrom(ctx).Logf(format, a...)
}

func (s *Service) runStore() registry.BranchRunStore { return registry.BranchRunsOf(s.reg) }

// beginRun is begin for the operations that are action runs (create, merge, push, reset): it
// registers the operation on ref and stores the run with its seven steps. ownerRef is the project
// the run is listed under, the parent for a merge and the branch otherwise.
func (s *Service) beginRun(ctx context.Context, ref, op, ownerRef, parentRef string) (*run, error) {
	s.maintainRuns(ctx)
	r, err := s.begin(ref, op)
	if err != nil {
		return nil, err
	}
	st := s.runStore()
	if st == nil {
		return r, nil
	}
	rec := &registry.BranchRun{
		ID: r.id, OwnerRef: ownerRef, BranchRef: ref, ParentRef: parentRef, Op: op, Status: registry.RunRunning,
		RunnerHost: s.host, RunnerPID: s.pid, RunnerInstance: s.instance,
	}
	for _, n := range registry.BranchRunSteps {
		status := registry.StepCreated
		if n == StepClone || n == StepConfigure {
			status = registry.StepPaused
		}
		rec.Steps = append(rec.Steps, registry.BranchRunStep{Name: n, Status: status})
	}
	// The request context may end with the response; the run row must exist before it does.
	if err := st.CreateBranchRun(ctx, rec); err != nil {
		s.end(ref, r)
		return nil, fmt.Errorf("record the %s run: %w", op, err)
	}
	r.rec = s.newRun(r.id, op, ref)
	return r, nil
}

// ErrRunSuperseded is the error a caller ends a run with when newer work replaced it before it began:
// the run is closed, and the default branch does not read it as a failure.
var ErrRunSuperseded = errors.New("replaced by a newer deployment before it started")

// StartRun records a run that the caller drives itself, step by step with Run.Step and Run.Phase,
// and ends with Run.Finish: the Git flow of a pull request, which starts clone and configure too.
// All seven steps start CREATED. The service does not touch the branch's status or tell its
// notify_url for such a run; the caller does. ownerRef is the project the run is listed under (a
// merge: the parent; otherwise the branch).
func (s *Service) StartRun(ctx context.Context, op, branchRef, ownerRef, parentRef string) (*Run, error) {
	st := s.runStore()
	if st == nil {
		return nil, errors.New("branching: this registry keeps no action runs")
	}
	s.maintainRuns(ctx)
	rec := &registry.BranchRun{
		ID: secrets.NewUUID(), OwnerRef: ownerRef, BranchRef: branchRef, ParentRef: parentRef, Op: op, Status: registry.RunRunning,
		RunnerHost: s.host, RunnerPID: s.pid, RunnerInstance: s.instance,
	}
	for _, n := range registry.BranchRunSteps {
		rec.Steps = append(rec.Steps, registry.BranchRunStep{Name: n, Status: registry.StepCreated})
	}
	if err := st.CreateBranchRun(ctx, rec); err != nil {
		return nil, fmt.Errorf("record the %s run: %w", op, err)
	}
	return s.newRun(rec.ID, op, branchRef), nil
}

func (s *Service) newRun(id, op, ref string) *Run {
	return &Run{s: s, id: id, op: op, ref: ref, logged: map[string]int{}}
}

// Recorder returns the recorder of the run with this id, for code that drives a run from outside
// the package. ErrNotFound when the run does not exist (or this registry keeps no runs).
func (s *Service) Recorder(ctx context.Context, id string) (*Run, error) {
	st := s.runStore()
	if st == nil || !IsUUID(id) {
		return nil, fmt.Errorf("%w: action run %s", ErrNotFound, id)
	}
	rec, err := st.GetBranchRun(ctx, id)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, fmt.Errorf("%w: action run %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return s.newRun(rec.ID, rec.Op, rec.BranchRef), nil
}

// LatestRun returns the newest run listed under ref (the run a create, push or reset of the
// branch ref started, or a merge into the project ref), or nil when there is none.
func (s *Service) LatestRun(ctx context.Context, ref string) (*registry.BranchRun, error) {
	st := s.runStore()
	if st == nil {
		return nil, nil
	}
	runs, _, err := st.ListBranchRuns(ctx, ref, 0, 1)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// ID is the run's id, the workflow_run_id of the Management API.
func (r *Run) ID() string {
	if r == nil {
		return ""
	}
	return r.id
}

// apply writes step changes; a failure to record progress is logged and never fails the operation.
func (r *Run) apply(changes ...registry.BranchRunStepChange) {
	st := r.s.runStore()
	if st == nil || len(changes) == 0 {
		return
	}
	// Recording survives a cancelled operation: the failure of a cancelled run is what is recorded.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.SetBranchRunSteps(ctx, r.id, changes); err != nil {
		r.s.log.Warn("could not record a run step", "run", r.id, "err", err)
	}
}

// Phase opens the named step and closes the step that was open (EXITED). The steps between them
// that have not started are skipped (PAUSED). Going back to an earlier step does nothing.
func (r *Run) Phase(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == name {
		return
	}
	ni := registry.StepIndex(name)
	if r.cur != "" && registry.StepIndex(r.cur) >= ni {
		return
	}
	var ch []registry.BranchRunStepChange
	from := 0
	if r.cur != "" {
		ch = append(ch, registry.BranchRunStepChange{Name: r.cur, Status: registry.StepExited, OnlyFrom: []string{registry.StepRunning}})
		from = registry.StepIndex(r.cur) + 1
	}
	for _, between := range registry.BranchRunSteps[min(from, ni):ni] {
		ch = append(ch, registry.BranchRunStepChange{Name: between, Status: registry.StepPaused, OnlyFrom: []string{registry.StepCreated}})
	}
	ch = append(ch, registry.BranchRunStepChange{Name: name, Status: registry.StepRunning, OnlyFrom: []string{registry.StepCreated, registry.StepPaused}})
	r.apply(ch...)
	r.cur = name
}

// Log appends a line to the log of the step that is open (pull when none is).
func (r *Run) Log(line string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logTo(r.stepForLog(), line)
}

// Logf is Log with a format.
func (r *Run) Logf(format string, a ...any) {
	if r == nil {
		return
	}
	r.Log(fmt.Sprintf(format, a...))
}

func (r *Run) stepForLog() string {
	if r.cur != "" {
		return r.cur
	}
	return StepPull
}

// logTo appends the lines of text to the step's log, each with the time and the step.
func (r *Run) logTo(step, text string) {
	// The registry stores text: invalid UTF-8 and NUL bytes (an error that quotes binary data) are
	// refused by Postgres, and the chunk would be lost with the rest of the log.
	text = strings.TrimRight(strings.ReplaceAll(strings.ToValidUTF8(text, "\uFFFD"), "\x00", ""), "\r\n")
	if text == "" {
		return
	}
	stamp := r.s.now().UTC().Format("2006-01-02T15:04:05Z")
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(&b, "%s [%s] %s\n", stamp, step, strings.TrimRight(line, "\r"))
	}
	out := b.String()
	if used := r.logged[step]; used >= maxStepLog {
		return
	} else if used+len(out) > maxStepLog {
		// Cut at a rune boundary: half a rune is invalid UTF-8.
		cut := max(0, maxStepLog-used)
		for cut > 0 && cut < len(out) && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + fmt.Sprintf("%s [%s] ... log truncated\n", stamp, step)
		r.logged[step] = maxStepLog
	} else {
		r.logged[step] = used + len(out)
	}
	r.apply(registry.BranchRunStepChange{Name: step, Log: out})
}

// Step is one step of a run, for the code that drives steps one by one.
type Step struct {
	r    *Run
	name string
}

// Step returns the named step (one of the Step* constants).
func (r *Run) Step(name string) Step { return Step{r: r, name: name} }

// Start opens the step (RUNNING), whatever it was before: the Git flow starts clone and configure,
// which a run begins with PAUSED.
func (st Step) Start() {
	if st.r == nil {
		return
	}
	st.r.mu.Lock()
	defer st.r.mu.Unlock()
	st.r.cur = st.name // log lines go to the step that was started last
	st.r.apply(registry.BranchRunStepChange{Name: st.name, Status: registry.StepRunning})
}

// Finish ends the step successfully (EXITED).
func (st Step) Finish() { st.end(registry.StepExited) }

// Skip marks the step as not run (PAUSED).
func (st Step) Skip() { st.end(registry.StepPaused) }

func (st Step) end(status string) {
	if st.r == nil {
		return
	}
	st.r.mu.Lock()
	defer st.r.mu.Unlock()
	if st.r.cur == st.name {
		st.r.cur = ""
	}
	st.r.apply(registry.BranchRunStepChange{Name: st.name, Status: status})
}

// Fail ends the step as DEAD with err in its log, pauses the steps after it that have not started
// and marks the run failed.
func (st Step) Fail(err error) {
	if st.r == nil {
		return
	}
	st.r.mu.Lock()
	defer st.r.mu.Unlock()
	st.r.fail(st.name, err)
}

// fail is Fail with the lock held.
func (r *Run) fail(step string, err error) {
	if r.failed != "" {
		return
	}
	r.failed = step
	reason := "failed"
	if err != nil {
		reason = err.Error()
	}
	r.logTo(step, "FAILED: "+reason)
	ch := []registry.BranchRunStepChange{{Name: step, Status: registry.StepDead}}
	if i := registry.StepIndex(step); i < len(registry.BranchRunSteps) {
		for _, after := range registry.BranchRunSteps[i+1:] {
			ch = append(ch, registry.BranchRunStepChange{Name: after, Status: registry.StepPaused, OnlyFrom: []string{registry.StepCreated}})
		}
	}
	r.apply(ch...)
	r.finishRow(registry.RunFailed, step+": "+reason)
}

func (r *Run) finishRow(status, failure string) {
	st := r.s.runStore()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.FinishBranchRun(ctx, r.id, status, failure); err != nil {
		r.s.log.Warn("could not record the end of a run", "run", r.id, "err", err)
	}
}

// Finish ends the run. With err it fails the step that was open (pull when none was); without,
// it closes that step, skips what never started and marks the run a success. It reports the step
// that failed, "" after a success.
func (r *Run) Finish(err error) (failedStep string) {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != "" {
		return r.failed
	}
	if err != nil {
		step := r.cur
		if step == "" {
			step = StepPull
		}
		r.fail(step, err)
		return step
	}
	var ch []registry.BranchRunStepChange
	for _, n := range registry.BranchRunSteps {
		ch = append(ch,
			registry.BranchRunStepChange{Name: n, Status: registry.StepExited, OnlyFrom: []string{registry.StepRunning}},
			registry.BranchRunStepChange{Name: n, Status: registry.StepPaused, OnlyFrom: []string{registry.StepCreated}})
	}
	r.apply(ch...)
	r.finishRow(registry.RunSuccess, "")
	return ""
}

// DeriveRunStatus is Studio's rule for the status of a run (data/actions/action-detail-query.ts):
// no steps or a step that has not finished is RUNNING, all steps EXITED or PAUSED is SUCCESS, a
// DEAD step is FAILED.
func DeriveRunStatus(steps []registry.BranchRunStep) string {
	if len(steps) == 0 {
		return registry.RunRunning
	}
	success, failed := true, false
	for _, s := range steps {
		if s.Status != registry.StepExited && s.Status != registry.StepPaused {
			success = false
		}
		if s.Status == registry.StepDead {
			failed = true
		}
	}
	switch {
	case success:
		return registry.RunSuccess
	case failed:
		return registry.RunFailed
	}
	return registry.RunRunning
}

// ---- reading and patching runs, for the Management API ----------------------------------------

func (s *Service) runsOrErr() (registry.BranchRunStore, error) {
	st := s.runStore()
	if st == nil {
		return nil, errors.New("branching: this registry keeps no action runs")
	}
	return st, nil
}

// ListRuns returns the runs listed under ownerRef, newest first, and how many there are in all.
func (s *Service) ListRuns(ctx context.Context, ownerRef string, offset, limit int) ([]registry.BranchRun, int, error) {
	st, err := s.runsOrErr()
	if err != nil {
		return nil, 0, err
	}
	return st.ListBranchRuns(ctx, ownerRef, offset, limit)
}

// BranchRuns returns the runs of a branch, newest first: the ones listed under the branch (its
// create, push and reset) and the merges of it that are listed under its parent. limit bounds
// how many it returns; zero or less means DefaultRunLimit (the stores read a limit of zero as "no
// rows").
func (s *Service) BranchRuns(ctx context.Context, b *Branch, limit int) ([]registry.BranchRun, error) {
	st, err := s.runsOrErr()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultRunLimit
	}
	own, _, err := st.ListBranchRuns(ctx, b.Ref, 0, limit)
	if err != nil {
		return nil, err
	}
	out := own
	if !b.IsDefault && b.ParentRef != b.Ref {
		// The parent owns the merges of all its branches: take this branch's from the newest.
		merges, _, err := st.ListBranchRuns(ctx, b.ParentRef, 0, 200)
		if err != nil {
			return nil, err
		}
		for _, m := range merges {
			if m.BranchRef == b.Ref {
				out = append(out, m)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetRun returns the run with this id if it is listed under ownerRef: a run answers under the ref
// that owns it and is not found under any other.
func (s *Service) GetRun(ctx context.Context, ownerRef, id string) (*registry.BranchRun, error) {
	st, err := s.runsOrErr()
	if err != nil {
		return nil, err
	}
	if !IsUUID(id) {
		return nil, fmt.Errorf("%w: action run %s", ErrNotFound, id)
	}
	rec, err := st.GetBranchRun(ctx, id)
	if errors.Is(err, registry.ErrNotFound) || (err == nil && rec.OwnerRef != ownerRef) {
		return nil, fmt.Errorf("%w: action run %s", ErrNotFound, id)
	}
	return rec, err
}

// RunLogs is the text of a run's log: the logs of its steps in the order of the DAG.
func (s *Service) RunLogs(ctx context.Context, ownerRef, id string) (string, error) {
	rec, err := s.GetRun(ctx, ownerRef, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, st := range rec.Steps {
		b.WriteString(st.Log)
	}
	return b.String(), nil
}

// SetRunSteps sets the status of steps of a run from outside (PATCH /actions/{id}/status: a CI job
// that runs a step reports it). A run that this service is working on refuses it. The run's status
// follows the steps (DeriveRunStatus).
func (s *Service) SetRunSteps(ctx context.Context, ownerRef, id string, steps map[string]string) (*registry.BranchRun, error) {
	rec, err := s.GetRun(ctx, ownerRef, id)
	if err != nil {
		return nil, err
	}
	if s.runLive(id) {
		return nil, conflict("action run %s is in progress on this node; its steps follow the operation", id)
	}
	valid := map[string]bool{}
	for _, v := range []string{registry.StepCreated, registry.StepRunning, registry.StepExited, registry.StepPaused, registry.StepDead} {
		valid[v] = true
	}
	var ch []registry.BranchRunStepChange
	for _, n := range registry.BranchRunSteps { // in DAG order, whatever the map order
		status, ok := steps[n]
		if !ok {
			continue
		}
		if !valid[status] {
			return nil, invalid("step %s: unknown status %q", n, status)
		}
		ch = append(ch, registry.BranchRunStepChange{Name: n, Status: status})
	}
	for n := range steps {
		if registry.StepIndex(n) == len(registry.BranchRunSteps) {
			return nil, invalid("unknown step %q", n)
		}
	}
	st := s.runStore()
	if len(ch) > 0 {
		if err := st.SetBranchRunSteps(ctx, id, ch); err != nil {
			return nil, err
		}
	}
	if rec, err = st.GetBranchRun(ctx, id); err != nil {
		return nil, err
	}
	switch derived := DeriveRunStatus(rec.Steps); {
	case derived != rec.Status:
		failure := rec.Failure
		if derived == registry.RunFailed && failure == "" {
			for _, stp := range rec.Steps {
				if stp.Status == registry.StepDead {
					failure = stp.Name + ": reported failed"
					break
				}
			}
		} else if derived != registry.RunFailed {
			failure = ""
		}
		if err := st.FinishBranchRun(ctx, id, derived, failure); err != nil {
			return nil, err
		}
		rec.Status, rec.Failure = derived, failure
	}
	return rec, nil
}

// runLive reports whether this process is working on the run.
func (s *Service) runLive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.id == id {
			return true
		}
	}
	return false
}

// ---- upkeep -------------------------------------------------------------------------------

// maintainRuns removes old runs, and those of projects that are gone, at most once an hour.
func (s *Service) maintainRuns(ctx context.Context) {
	st := s.runStore()
	if st == nil {
		return
	}
	now := s.now()
	s.mu.Lock()
	due := s.lastPrune.IsZero() || now.Sub(s.lastPrune) >= time.Hour
	if due {
		s.lastPrune = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	if _, err := st.PruneBranchRuns(context.WithoutCancel(ctx), now.Add(-runRetention), now.Add(-time.Hour)); err != nil {
		s.log.Warn("could not prune old action runs", "err", err)
	}
}

// runnerGone reports whether the service that recorded host, pid and instance as the one working
// on an operation is known to be gone: it ran on this host with a pid that no longer exists, or it
// was an earlier service of this very process. Another host or a missing record is "not known".
func (s *Service) runnerGone(host string, pid int, instance string) bool {
	if pid <= 0 || host != s.host {
		return false
	}
	if pid == s.pid {
		return instance != s.instance
	}
	return !procutil.Alive(pid)
}

// recoverRuns fails the runs that no process works on any more: a daemon that stopped in the
// middle of an operation leaves its runs RUNNING. A run whose runner is known to be gone is
// recovered at once ("interrupted by restart"), one whose runner is not known after staleAfter
// without a change. Its open step is DEAD, the steps after it PAUSED, and a branch the run left
// busy is MIGRATIONS_FAILED (FUNCTIONS_FAILED when only the deploy was left). dryRun only counts.
func (s *Service) recoverRuns(ctx context.Context, dryRun bool) []string {
	st := s.runStore()
	if st == nil {
		return nil
	}
	runs, err := st.RunningBranchRuns(ctx)
	if err != nil {
		s.log.Warn("could not list the running action runs", "err", err)
		return nil
	}
	var recovered []string
	for i := range runs {
		rec := &runs[i]
		if s.runLive(rec.ID) {
			continue
		}
		reason := ""
		switch {
		case s.runnerGone(rec.RunnerHost, rec.RunnerPID, rec.RunnerInstance):
			reason = "interrupted by restart"
		case s.now().Sub(rec.UpdatedAt) >= s.staleAfter:
			reason = "interrupted: no process was working on this run (the daemon stopped?)"
		default:
			continue
		}
		recovered = append(recovered, rec.ID)
		if dryRun {
			continue
		}
		step := StepPull
		for _, want := range []string{registry.StepRunning, registry.StepCreated} {
			found := false
			for _, stp := range rec.Steps {
				if stp.Status == want {
					step, found = stp.Name, true
					break
				}
			}
			if found {
				break
			}
		}
		r := s.newRun(rec.ID, rec.Op, rec.BranchRef)
		r.mu.Lock()
		r.fail(step, errors.New(reason))
		r.mu.Unlock()
		s.failBranchOf(ctx, rec, step, reason)
	}
	return recovered
}

// failBranchOf records the end of an interrupted run on the branch it was working on, unless
// something else is working on it now.
func (s *Service) failBranchOf(ctx context.Context, rec *registry.BranchRun, step, reason string) {
	if _, running := s.running(rec.BranchRef); running {
		return
	}
	p, err := s.reg.GetProject(ctx, rec.BranchRef)
	if err != nil || p.Branch == nil {
		return
	}
	switch {
	case busy(p.Branch.State):
		s.setState(ctx, rec.BranchRef, registry.BranchMigrationsFailed, reason, nil)
	case p.Branch.State == registry.BranchMigrationsPassed && step == StepDeploy:
		s.setState(ctx, rec.BranchRef, registry.BranchFunctionsFailed, reason, nil)
	}
}

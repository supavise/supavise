package branching

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// ActionInput is the body of merge, reset and push.
type ActionInput struct {
	// MigrationVersion limits a merge to branch migrations up to and including it, and a
	// reset of a schema-only branch to the parent's migrations up to and including it.
	MigrationVersion string
	// Force merges or pushes despite divergence: migrations whose versions differ in content
	// are skipped, the rest are applied.
	Force bool
}

// run is an operation in progress in this process.
type run struct {
	id, op string
	done   chan struct{}
	once   sync.Once
	// rec records the action run of the operation (runs.go); nil for operations that are not one
	// (delete), or when the registry keeps no runs.
	rec *Run
}

// ---- operation tracking ----------------------------------------------------------------

// begin registers an operation on ref; it fails when another runs here.
func (s *Service) begin(ref, op string) (*run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[ref]; ok {
		return nil, conflict("branch %s is busy with %s", ref, r.op)
	}
	r := &run{id: secrets.NewUUID(), op: op, done: make(chan struct{})}
	s.runs[ref] = r
	return r, nil
}

func (s *Service) end(ref string, r *run) {
	s.mu.Lock()
	if s.runs[ref] == r {
		delete(s.runs, ref)
	}
	s.mu.Unlock()
	r.once.Do(func() { close(r.done) })
}

// running reports the operation in progress on ref in this process.
func (s *Service) running(ref string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[ref]; ok {
		return r.op, true
	}
	return "", false
}

// checkIdle refuses an operation on a branch that is busy, here or (by its registry
// state) in another process whose operation is not known to be abandoned.
func (s *Service) checkIdle(ctx context.Context, b *Branch) error {
	if op, ok := s.running(b.Ref); ok {
		return conflict("branch %s is busy with %s", b.Name, op)
	}
	if busy(b.State) && !s.abandoned(ctx, b) {
		return conflict("branch %s is busy (%s)", b.Name, b.State)
	}
	return nil
}

// abandoned reports whether a branch whose registry state is busy has no process working on
// it: no operation runs here, and either its state has not changed for staleAfter, or the
// process that started the operation (recorded in its started event) is gone.
func (s *Service) abandoned(ctx context.Context, b *Branch) bool {
	if !busy(b.State) {
		return false
	}
	if _, ok := s.running(b.Ref); ok {
		return false
	}
	return s.now().Sub(b.UpdatedAt) >= s.staleAfter || s.ownerGone(ctx, b.Ref)
}

// ownerGone reports whether the process that started the newest operation on ref is known to
// be gone: it ran on this host with a pid that no longer exists, or it was an earlier service
// of this very process. A missing record (an older version) or another host is "not known".
func (s *Service) ownerGone(ctx context.Context, ref string) bool {
	evs, err := s.reg.ListEvents(ctx, ref, 50) // newest first
	if err != nil {
		return false
	}
	for _, e := range evs {
		if !strings.HasPrefix(e.Kind, "branch.") {
			continue
		}
		if strings.HasSuffix(e.Kind, ".succeeded") || strings.HasSuffix(e.Kind, ".failed") {
			return false // the newest operation finished; the busy state is not its doing
		}
		if !strings.HasSuffix(e.Kind, ".started") {
			continue
		}
		var pl struct {
			Owner *struct {
				Host     string `json:"host"`
				PID      int    `json:"pid"`
				Instance string `json:"instance"`
			} `json:"owner"`
		}
		if json.Unmarshal(e.Payload, &pl) != nil || pl.Owner == nil || pl.Owner.PID <= 0 {
			return false
		}
		return s.runnerGone(pl.Owner.Host, pl.Owner.PID, pl.Owner.Instance)
	}
	return false
}

// WaitIdle blocks until no operation runs on ref (here, or by registry state elsewhere).
func (s *Service) WaitIdle(ctx context.Context, ref string) (*Branch, error) {
	for {
		s.mu.Lock()
		r := s.runs[ref]
		s.mu.Unlock()
		if r != nil {
			select {
			case <-r.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		b, err := s.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !busy(b.State) || s.now().Sub(b.UpdatedAt) >= s.staleAfter {
			return b, nil
		}
		if r == nil {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
}

// setState records the state and detail of the branch project ref. A branch that is gone
// (deleted while the operation ran) is not an error. While an operation is running its progress
// text also goes to the log of its run.
func (s *Service) setState(ctx context.Context, ref string, st registry.BranchState, detail string, mutate func(*registry.BranchInfo)) {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil || p.Branch == nil {
		return
	}
	b := *p.Branch
	b.State, b.Detail = st, detail
	if mutate != nil {
		mutate(&b)
	}
	if err := s.reg.UpdateBranch(ctx, ref, &b); err != nil && !errors.Is(err, registry.ErrNotFound) {
		s.log.Warn("could not record the branch state", "ref", ref, "state", st, "err", err)
		return
	}
	switch st {
	case registry.BranchMigrationsFailed, registry.BranchFunctionsFailed, registry.BranchFunctionsDeployed:
		// The end of an operation: the run records it itself.
	default:
		if detail != "" {
			s.logRun(ctx, "%s", detail)
		}
	}
}

func (s *Service) event(ctx context.Context, ref, kind string, payload any) {
	if err := s.reg.AppendEvent(ctx, ref, kind, payload); err != nil {
		s.log.Warn("could not record an event", "ref", ref, "kind", kind, "err", err)
	}
}

// spawn runs fn detached from the caller. It records started/succeeded/failed events on the
// branch and its parent, finishes the action run (runs.go), sets the branch's State and Detail
// from fn's outcome, and tells notify_url. The run id is r.id, the API's workflow_run_id. fn
// reports its progress to the run with s.phase. A caller that already holds the run (Create)
// passes it in.
func (s *Service) spawn(r *run, ref, parentRef string, fn func(ctx context.Context) (string, error)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.end(ref, r) // a panic must not leave the branch busy forever
		ctx, cancel := context.WithTimeout(s.base, s.opTimeout)
		defer cancel()
		if r.rec != nil {
			ctx = context.WithValue(ctx, runCtxKey{}, r.rec)
		}
		started := s.now()
		payload := map[string]any{"run_id": r.id, "operation": r.op, "branch": ref, "parent": parentRef,
			// Who works on the operation, so that another process can tell it from an abandoned one (ownerGone).
			"owner": map[string]any{"host": s.host, "pid": s.pid, "instance": s.instance}}
		s.event(ctx, ref, "branch."+r.op+".started", payload)
		detail, err := fn(ctx)
		// Recording the outcome must survive a cancelled operation context.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rcancel()
		payload["ms"] = s.now().Sub(started).Milliseconds()
		// The run ends before the branch's state changes: a client that sees the state change
		// finds the run finished.
		if err == nil {
			r.rec.Log(detail)
		}
		failedStep := r.rec.Finish(err)
		state := registry.BranchFunctionsDeployed
		if err != nil {
			state, detail = registry.BranchMigrationsFailed, err.Error()
			if failedStep == StepDeploy {
				state = registry.BranchFunctionsFailed
			}
			payload["error"] = detail
			s.log.Error("branch operation failed", "op", r.op, "ref", ref, "run", r.id, "err", err)
		} else {
			payload["detail"] = detail
		}
		kind := "branch." + r.op + map[bool]string{true: ".failed", false: ".succeeded"}[err != nil]
		s.event(rctx, ref, kind, payload)
		s.event(rctx, parentRef, kind, payload)
		// The state is the last thing a client can see change, so it comes after the events,
		// and the branch stops being busy the moment it shows: a client that sees the new
		// state may start the next operation at once.
		var after func(*registry.BranchInfo)
		if r.op == "merge" && err == nil {
			// A merged branch is no longer waiting for review (Studio clears the mark itself when
			// it sees the run finish; an MCP or CLI merge does not).
			after = func(bi *registry.BranchInfo) { bi.ReviewRequestedAt = nil }
		}
		s.setState(rctx, ref, state, detail, after)
		s.end(ref, r)
		s.notify(rctx, ref, r, state, detail)
	}()
}

package branching

import (
	"context"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// Drive starts an operation on an existing branch whose work is done by fn: the action run, the
// events, the branch's status and its notify_url follow the operation like those of a push or a
// reset, and the call returns the run's id as soon as the operation is registered. The Git
// integration applies the commit of a pull request this way. The branch must be at rest and
// running; a second operation on it is refused while this one lasts.
func (s *Service) Drive(ctx context.Context, idOrRef, op string, fn Driver) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, op)
	if err != nil {
		return "", err
	}
	if err := s.checkIdle(ctx, b); err != nil {
		return "", err
	}
	if err := s.activeBranch(ctx, b); err != nil {
		return "", err
	}
	r, err := s.beginRun(ctx, b.Ref, op, b.Ref, b.ParentRef)
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchRunningMigration, "applying changes from the repository", nil)
	s.spawn(r, b.Ref, b.ParentRef, func(ctx context.Context) (string, error) { return fn(ctx, runFrom(ctx)) })
	return r.id, nil
}

// Database returns what the service runs SQL in project databases with: the Database it was given,
// or pgx through the engine. The Git integration applies a repository's migrations through it.
func (s *Service) Database() Database { return s.db }

// wakeTimeout is how long Wake waits for a branch that is coming up or going down.
const wakeTimeout = 3 * time.Minute

// Wake returns a branch that runs: one that was paused, by hand or by the idle pause of preview
// branches, is resumed and waited for. The Git integration wakes a branch before it applies a
// commit, which Drive refuses on a branch that is not running.
func (s *Service) Wake(ctx context.Context, idOrRef string) (*Branch, error) {
	deadline := time.NewTimer(wakeTimeout)
	defer deadline.Stop()
	resumed := false
	for {
		b, err := s.resolveBranch(ctx, idOrRef, "wake")
		if err != nil {
			return nil, err
		}
		switch b.ProjectStatus {
		case registry.StatusActiveHealthy, registry.StatusActiveUnhealthy:
			return b, nil
		case registry.StatusInactive:
			if !resumed {
				resumed = true
				if err := s.eng.Resume(ctx, b.Ref); err != nil {
					// Somebody else may have resumed it between the read and the call.
					again, rerr := s.Resolve(ctx, b.Ref)
					if rerr != nil || again.ProjectStatus == registry.StatusInactive {
						return nil, err
					}
				}
				continue
			}
		case registry.StatusComingUp, registry.StatusPausing:
			// Wait: the next read shows where it went.
		default:
			return nil, conflict("branch %s is %s; it cannot be woken", b.Name, b.ProjectStatus)
		}
		select {
		case <-time.After(time.Second):
		case <-deadline.C:
			return nil, conflict("branch %s is still %s after %s", b.Name, b.ProjectStatus, wakeTimeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

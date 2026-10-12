package branching

import (
	"context"
	"errors"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// Drive starts an operation on an existing branch whose work is done by fn: the action run, the
// events, the branch's status and its notify_url follow the operation like those of a push or a
// reset, and the call returns the run's id as soon as the operation is registered. The Git
// integration applies the commit of a pull request this way. The branch must be at rest and
// running, and not scheduled for deletion; a second operation on it is refused while this one lasts.
func (s *Service) Drive(ctx context.Context, idOrRef, op string, fn Driver) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, op)
	if err != nil {
		return "", err
	}
	if err := scheduledForDeletion(b); err != nil {
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

// wakeTimeout is how long WakeForDrive waits for a branch that is coming up or going down.
const wakeTimeout = 3 * time.Minute

// scheduledForDeletion refuses a branch that waits for its deletion: a commit of the repository does not
// cancel it (only Restore does), and the work would go to a branch that is about to be removed.
func scheduledForDeletion(b *Branch) error {
	if b.DeletionScheduledAt == nil {
		return nil
	}
	return conflict("branch %s is scheduled for deletion; restore it in the dashboard to deploy to it", b.Name)
}

// WakeForDrive returns a branch that runs: one the node paused for idleness (the preview branches that
// nothing used) is started and waited for. The Git integration wakes a branch before it applies a commit,
// which Drive refuses on a branch that is not running.
//
// It starts nothing else. A branch its owner paused stays paused, a branch stopped as a quarantine
// (registry.PauseQuarantine) stays stopped, and a branch scheduled for deletion is not woken: a commit of
// the repository, which anyone with push access can make, is not the owner's decision to run a branch again
// (all three answer ErrConflict, and the pull request's check says why). A pause that has no record counts
// as the owner's.
func (s *Service) WakeForDrive(ctx context.Context, idOrRef string) (*Branch, error) {
	deadline := time.NewTimer(wakeTimeout)
	defer deadline.Stop()
	woke := false
	for {
		b, err := s.resolveBranch(ctx, idOrRef, "wake")
		if err != nil {
			return nil, err
		}
		if err := scheduledForDeletion(b); err != nil {
			return nil, err
		}
		switch b.ProjectStatus {
		case registry.StatusActiveHealthy, registry.StatusActiveUnhealthy:
			s.touchIdle(b.Ref)
			return b, nil
		case registry.StatusInactive:
			if !woke {
				woke = true
				if !registry.WakesByItself(s.pauseRecord(ctx, b.Ref)) {
					return nil, s.notRunning(ctx, b)
				}
				wctx, cancel := context.WithTimeout(ctx, wakeTimeout)
				err := s.wake(wctx, b.Ref, "git")
				cancel()
				switch {
				case errors.Is(err, ErrNotIdle):
					// The pause changed hands between the read and the start.
					return nil, s.notRunning(ctx, b)
				case err != nil:
					return nil, err
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

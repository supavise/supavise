package branching

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// purgeArchive removes everything the backup store holds for ref (WAL and base backups).
func (s *Service) purgeArchive(ctx context.Context, ref string) error {
	if s.bk == nil || s.bk.Store() == nil {
		return nil
	}
	if !secrets.ValidRef(ref) { // the prefix must never be empty or reach another project
		return fmt.Errorf("branching: refusing to purge the archive of %q", ref)
	}
	objs, err := s.bk.Store().List(ctx, ref+"/")
	if err != nil {
		return err
	}
	const batch = 500
	for len(objs) > 0 {
		n := min(batch, len(objs))
		keys := make([]string, n)
		for i := range keys {
			keys[i] = objs[i].Key
		}
		if err := s.bk.Store().Delete(ctx, keys...); err != nil {
			return err
		}
		objs = objs[n:]
	}
	return nil
}

// SweepResult lists what a sweep did.
type SweepResult struct {
	Expired []string          // refs of branches deleted because their expiry or scheduled deletion lapsed
	Stale   []string          // refs whose interrupted operation was marked failed
	Warned  []string          // refs of branches with a hostname that were warned about their expiry
	Failed  map[string]string // ref -> error, for branches that could not be deleted
}

// lapsedAt reports whether b is due for deletion at now: a branch that is not persistent is past its expiry,
// or any branch is past its scheduled deletion.
func lapsedAt(b *Branch, now time.Time) bool {
	return (!b.Persistent && b.ExpiresAt != nil && !b.ExpiresAt.After(now)) ||
		(b.DeletionScheduledAt != nil && !b.DeletionScheduledAt.After(now))
}

// Sweep deletes non-persistent branches past expires_at and branches whose scheduled
// deletion has come, without a final backup unless the branch is persistent, and marks
// operations that no process works on as failed: the process that started one is gone (same
// host, pid no longer exists), or the branch has been busy for [StaleAfter] with no change.
// dryRun only reports.
func (s *Service) Sweep(ctx context.Context, dryRun bool) (*SweepResult, error) {
	// Runs that no process works on are failed first, which also recovers the branches they left
	// busy with the reason in the run log (runs.go).
	s.recoverRuns(ctx, dryRun)
	all, err := s.reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	res := &SweepResult{Failed: map[string]string{}}
	now := s.now()
	// The warnings are posted after the deletions: a notify_url that does not answer must not hold them up.
	var notices []expiryNotice
	for i := range all {
		p := &all[i]
		if p.Branch == nil {
			continue
		}
		b := branchOf(p)
		if _, running := s.running(p.Ref); running {
			continue
		}
		if s.inFlight(ctx, b) {
			if s.unattended(ctx, b) {
				res.Stale = append(res.Stale, p.Ref)
				if !dryRun {
					s.setState(ctx, p.Ref, registry.BranchMigrationsFailed, "interrupted: no process was working on this operation (the daemon stopped?)", nil)
				}
			}
			continue // otherwise another process (the CLI) is working on it
		}
		if !lapsedAt(b, now) {
			if !dryRun && s.warnExpiring(ctx, b, now, &notices) {
				res.Warned = append(res.Warned, p.Ref)
			}
			continue
		}
		if dryRun {
			res.Expired = append(res.Expired, p.Ref)
			continue
		}
		// The list was read before the deletions of this pass, which can take long (units stop, a persistent
		// branch takes its final backup): the branch is read again, with the operation held, and deleted only
		// if it is still due. A Restore, an Update that makes it persistent or extends it, or a deletion by
		// hand since the list was read is respected.
		err := s.deleteWith(ctx, b, "sweeper", false, func(cur *Branch) bool {
			return !s.inFlight(ctx, cur) && lapsedAt(cur, s.now())
		})
		if errors.Is(err, errNoLongerDue) {
			s.log.Info("left a branch that is no longer due for deletion", "ref", p.Ref, "name", b.Name)
			continue
		}
		if err != nil {
			s.log.Error("could not delete a lapsed branch", "ref", p.Ref, "name", b.Name, "err", err)
			res.Failed[p.Ref] = err.Error()
			continue
		}
		s.log.Info("deleted a lapsed branch", "ref", p.Ref, "name", b.Name, "parent", b.ParentRef)
		res.Expired = append(res.Expired, p.Ref)
	}
	s.sendExpiryNotices(ctx, notices)
	return res, ctx.Err()
}

// Run sweeps every [branching] sweep_interval_seconds until ctx ends. The daemon starts it
// in a goroutine next to the API server:
//
//	go svc.Run(ctx)
//
// It sweeps once at start, which also fails operations that a crash interrupted.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Branching.SweepInterval())
	defer t.Stop()
	for {
		if _, err := s.Sweep(ctx, false); err != nil && ctx.Err() == nil {
			s.log.Error("branch sweep failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

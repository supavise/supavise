package branching

import (
	"context"
	"errors"
	"fmt"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// DeleteOptions tune Delete.
type DeleteOptions struct {
	// Schedule is the soft delete of DELETE /v1/branches/{id}?force=false: the branch is paused, so
	// that the dashboard offers Restore, and removed after [branching] soft_delete_grace_minutes
	// unless Restore cancels it.
	Schedule bool
	// SkipFinalBackup deletes a persistent branch without the final base backup (the data is gone).
	// A branch that is not persistent never gets one.
	SkipFinalBackup bool
}

// Delete removes a branch: its fleet tenants, units, data and registry row. A branch that
// is not persistent gets no final backup and its WAL archive is removed; a persistent one
// keeps the final base backup the lifecycle takes (when a backup backend is configured).
// With Schedule it only marks the branch for deletion, pauses it and returns it.
func (s *Service) Delete(ctx context.Context, idOrRef string, o DeleteOptions) (*Branch, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "delete")
	if err != nil {
		return nil, err
	}
	// Only an operation running here blocks a delete. A busy state in the registry may be a
	// crash's leftover, and an operation in another process holds the lifecycle's advisory
	// lock, which the delete waits for.
	if op, ok := s.running(b.Ref); ok {
		return nil, conflict("branch %s is busy with %s", b.Name, op)
	}
	if o.Schedule {
		return s.scheduleDelete(ctx, b)
	}
	// A request that gives up must not leave a half-deleted branch.
	return b, s.deleteWith(context.WithoutCancel(ctx), b, "api", o.SkipFinalBackup)
}

// scheduleDelete marks b for deletion after the grace period and pauses it. The pause is what makes
// preview_project_status INACTIVE, the one state in which Studio enables Restore; it is the owner's
// pause, so a request does not wake the branch while it waits to go. A branch that is already paused
// stays paused, and one the node paused for idleness becomes the owner's.
func (s *Service) scheduleDelete(ctx context.Context, b *Branch) (*Branch, error) {
	p, err := s.project(ctx, b.Ref)
	if err != nil {
		return nil, err
	}
	// An idle pause wakes on a request; this one must not.
	if st := registry.BranchLifecycleOf(s.reg); st != nil && p.Status == registry.StatusInactive {
		if pz, err := st.GetPause(ctx, b.Ref); err == nil && pz.Reason != "" {
			if err := st.SetPause(ctx, b.Ref, "", s.now().UTC()); err != nil {
				s.log.Warn("could not make the pause of a branch scheduled for deletion the owner's", "ref", b.Ref, "err", err)
			}
		}
	}
	nb := *p.Branch
	at := s.now().Add(s.cfg.Branching.SoftDeleteGrace()).UTC()
	nb.DeletionScheduledAt = &at
	if err := s.reg.UpdateBranch(ctx, b.Ref, &nb); err != nil {
		return nil, err
	}
	s.event(ctx, b.Ref, "branch.deletion_scheduled", map[string]any{"at": at})
	if p.Status.Running() {
		// The deletion stands whether or not the pause works: the sweeper removes the branch at the
		// deadline either way, and a branch that keeps running until then is only less convenient.
		if err := s.eng.Pause(context.WithoutCancel(ctx), b.Ref); err != nil {
			s.log.Warn("could not pause a branch scheduled for deletion; it runs until it is deleted", "ref", b.Ref, "err", err)
			s.event(ctx, b.Ref, "branch.deletion_pause_failed", map[string]any{"error": err.Error()})
		}
	}
	return s.Resolve(ctx, b.Ref)
}

func (s *Service) deleteNow(ctx context.Context, b *Branch, by string) error {
	return s.deleteWith(ctx, b, by, false)
}

func (s *Service) deleteWith(ctx context.Context, b *Branch, by string, skipBackup bool) error {
	r, err := s.begin(b.Ref, "delete")
	if err != nil {
		return err
	}
	defer s.end(b.Ref, r)
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	if err := s.removeBranch(ctx, b, removeOptions{final: true, skipFinalBackup: skipBackup}); err != nil {
		return err
	}
	payload := map[string]any{"branch": b.Ref, "name": b.Name, "by": by}
	s.event(ctx, b.ParentRef, "branch.deleted", payload)
	return nil
}

type removeOptions struct {
	// final lets a persistent branch keep its final base backup and its archive (a delete);
	// a reset removes the archive whatever the branch is.
	final bool
	// skipFinalBackup deletes a persistent branch without the final base backup, and keeps its archive: the
	// older backups and the WAL stay available for a restore to a new project.
	skipFinalBackup bool
	// keepRecord keeps the registry row (lifecycle.DeleteOptions.KeepRecord).
	keepRecord bool
}

// removeBranch removes the branch's fleet tenants, units and data, and its registry row
// unless keepRecord is set. An ephemeral branch takes no final backup and leaves nothing in
// the archive.
func (s *Service) removeBranch(ctx context.Context, b *Branch, o removeOptions) error {
	keepBackup := b.Persistent && o.final
	// A reset (the only caller that keeps the record) starts the branch with no objects, so the
	// files of the old cluster, in the bucket or on disk, go with it.
	if err := s.eng.DeleteWith(ctx, b.Ref, lifecycle.DeleteOptions{SkipFinalBackup: !keepBackup || o.skipFinalBackup, KeepRecord: o.keepRecord, PurgeObjects: o.keepRecord}); err != nil {
		return err
	}
	if !keepBackup {
		// An ephemeral branch's WAL and base backups are worthless and cost storage, and a
		// reset reuses the ref, so its WAL would collide with the old one.
		if err := s.purgeArchive(ctx, b.Ref); err != nil {
			s.log.Warn("could not remove the branch's archive", "ref", b.Ref, "err", err)
		}
	}
	return nil
}

// Restore cancels a scheduled deletion and starts the branch again if it is paused, which is what
// the dashboard's Restore button asks for. A branch that is neither scheduled nor paused has nothing
// to restore (409).
func (s *Service) Restore(ctx context.Context, idOrRef string) (*Branch, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "restore")
	if err != nil {
		return nil, err
	}
	if b.DeletionScheduledAt == nil && b.ProjectStatus != registry.StatusInactive {
		return nil, conflict("branch %s is not scheduled for deletion or paused", b.Name)
	}
	if b.DeletionScheduledAt != nil {
		p, err := s.project(ctx, b.Ref)
		if err != nil {
			return nil, err
		}
		nb := *p.Branch
		nb.DeletionScheduledAt = nil
		if err := s.reg.UpdateBranch(ctx, b.Ref, &nb); err != nil {
			return nil, err
		}
		s.event(ctx, b.Ref, "branch.deletion_cancelled", nil)
	}
	if b.ProjectStatus == registry.StatusInactive {
		if err := s.eng.Resume(context.WithoutCancel(ctx), b.Ref); err != nil {
			return nil, fmt.Errorf("the deletion is cancelled, but branch %s did not start again (resume it with `supavise projects resume %s`): %w", b.Name, b.Ref, err)
		}
	}
	return s.Resolve(ctx, b.Ref)
}

// DisableBranching deletes every branch of the project (DELETE /v1/projects/{ref}/branches).
func (s *Service) DisableBranching(ctx context.Context, ref string) error {
	parent, err := s.Parent(ctx, ref)
	if err != nil {
		return err
	}
	kids, err := s.children(ctx, parent.Ref)
	if err != nil {
		return err
	}
	var errs []error
	for i := range kids {
		b := branchOf(&kids[i])
		if err := s.deleteNow(context.WithoutCancel(ctx), b, "disable"); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.Name, err))
		}
	}
	return errors.Join(errs...)
}

package branching

import (
	"context"
	"errors"
	"fmt"

	"github.com/supavise/supavise/internal/lifecycle"
)

// DeleteOptions tune Delete.
type DeleteOptions struct {
	// Schedule is the soft delete of DELETE /v1/branches/{id}?force=false: the branch is
	// removed after [branching] soft_delete_grace_minutes, and Restore cancels it.
	Schedule bool
}

// Delete removes a branch: its fleet tenants, units, data and registry row. A branch that
// is not persistent gets no final backup and its WAL archive is removed; a persistent one
// keeps the final base backup the lifecycle takes (when a backup backend is configured).
// With Schedule it only marks the branch for deletion and returns it.
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
		p, err := s.project(ctx, b.Ref)
		if err != nil {
			return nil, err
		}
		nb := *p.Branch
		at := s.now().Add(s.cfg.Branching.SoftDeleteGrace()).UTC()
		nb.DeletionScheduledAt = &at
		if err := s.reg.UpdateBranch(ctx, b.Ref, &nb); err != nil {
			return nil, err
		}
		s.event(ctx, b.Ref, "branch.deletion_scheduled", map[string]any{"at": at})
		return s.Resolve(ctx, b.Ref)
	}
	// A request that gives up must not leave a half-deleted branch.
	return b, s.deleteNow(context.WithoutCancel(ctx), b, "api")
}

func (s *Service) deleteNow(ctx context.Context, b *Branch, by string) error {
	r, err := s.begin(b.Ref, "delete")
	if err != nil {
		return err
	}
	defer s.end(b.Ref, r)
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	if err := s.removeBranch(ctx, b, removeOptions{final: true}); err != nil {
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
	// keepRecord keeps the registry row (lifecycle.DeleteOptions.KeepRecord).
	keepRecord bool
}

// removeBranch removes the branch's fleet tenants, units and data, and its registry row
// unless keepRecord is set. An ephemeral branch takes no final backup and leaves nothing in
// the archive.
func (s *Service) removeBranch(ctx context.Context, b *Branch, o removeOptions) error {
	keepBackup := b.Persistent && o.final
	if err := s.eng.DeleteWith(ctx, b.Ref, lifecycle.DeleteOptions{SkipFinalBackup: !keepBackup, KeepRecord: o.keepRecord}); err != nil {
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

// Restore cancels a scheduled deletion.
func (s *Service) Restore(ctx context.Context, idOrRef string) (*Branch, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "restore")
	if err != nil {
		return nil, err
	}
	if b.DeletionScheduledAt == nil {
		return nil, conflict("branch %s is not scheduled for deletion", b.Name)
	}
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

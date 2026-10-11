package branching

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// resetMinLife is the shortest remaining lifetime of a non-persistent branch after a reset.
const resetMinLife = 15 * time.Minute

// Reset recreates the branch from its parent: a schema-only branch gets the parent's
// migrations (up to MigrationVersion when set) and seed, a with_data branch a fresh copy of
// the parent's data. The branch keeps its id, name, ref and settings. A schema-only branch
// also keeps its credentials; a with_data branch gets new ones, as at creation. Anything
// written to the branch is gone, and so is the archive of a persistent branch (its WAL and
// base backups: the recreated cluster reuses the ref, and its WAL would collide with the old).
//
// Everything that can fail without touching the branch is checked first (the parent is
// running, a with_data branch has a way to get its data). The old cluster is then removed but
// the branch's registry row is kept, so a failure later leaves the branch registered as
// MIGRATIONS_FAILED: it can be reset again or deleted.
func (s *Service) Reset(ctx context.Context, idOrRef string, in ActionInput) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "reset")
	if err != nil {
		return "", err
	}
	if err := s.checkIdle(ctx, b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	if in.MigrationVersion != "" && b.WithData {
		return "", invalid("migration_version only applies to a schema-only branch")
	}
	old, err := s.reg.GetProject(ctx, b.Ref)
	if err != nil {
		return "", err
	}
	org := ""
	if old.OrgID != 0 {
		o, err := s.reg.GetOrganizationByID(ctx, old.OrgID)
		if err != nil {
			return "", err
		}
		org = o.Slug
	}
	if err := s.preflightReset(ctx, b, parent, old); err != nil {
		return "", err
	}
	var keys *secrets.ProjectKeys
	if !b.WithData {
		// A branch whose creation failed may have no credentials worth keeping.
		if keys, err = s.eng.Keys(ctx, b.Ref); err != nil {
			keys = nil
		}
	}
	r, err := s.beginRun(ctx, b.Ref, "reset", b.Ref, parent.Ref)
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchCreatingProject, "resetting from "+parent.Ref, nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) {
		s.phase(ctx, StepPull)
		if err := s.removeBranch(ctx, b, removeOptions{keepRecord: true}); err != nil {
			return "", fmt.Errorf("remove the old cluster: %w", err)
		}
		info := *old.Branch
		info.State, info.Detail, info.CloneMethod = registry.BranchCreatingProject, "recreating the project", ""
		if !info.Persistent {
			// The branch keeps the lifetime it was given, but a reset never leaves it less than
			// resetMinLife: it would otherwise expire while the agent starts using it again.
			if floor := s.now().Add(resetMinLife).UTC(); info.ExpiresAt == nil || info.ExpiresAt.Before(floor) {
				info.ExpiresAt = &floor
			}
		}
		info.DeletionScheduledAt = nil
		// With data, the new cluster gets the same outbound policy, applied again after its first
		// start (set below); an opt-out stays an opt-out, and what could not be enforced is tried again.
		// The row is still there: record the lifetime and the cleared method on it now. The policy
		// has its own writer (UpdateBranch never writes it), a compare-and-set from the value the
		// row has right now, so that nothing racing with the reset puts a stale policy back.
		s.setState(ctx, b.Ref, info.State, info.Detail, func(bi *registry.BranchInfo) {
			bi.CloneMethod, bi.ExpiresAt, bi.DeletionScheduledAt = info.CloneMethod, info.ExpiresAt, nil
		})
		if info.WithData {
			cur, err := s.reg.GetProject(ctx, b.Ref)
			if err != nil || cur.Branch == nil {
				return "", fmt.Errorf("read the branch before its policy is set: %v", err)
			}
			info.Egress = s.egressPolicy(cur.Branch.Egress == registry.EgressAllowed)
			if err := s.reg.SetBranchEgress(ctx, b.Ref, cur.Branch.Egress, info.Egress); err != nil {
				return "", fmt.Errorf("record the branch's egress policy: %w", err)
			}
		}
		j := &createJob{
			parent: parent, ref: b.Ref, info: &info, class: old.Class, org: org, recreate: true,
			in: CreateInput{Name: b.Name, GitBranch: b.GitBranch, Persistent: b.Persistent, WithData: b.WithData, NotifyURL: b.NotifyURL},
		}
		if keys != nil {
			j.keys = keys
		}
		j.upTo = in.MigrationVersion
		j.keepExpiry = true
		return s.doCreate(ctx, j)
	})
	return r.id, nil
}

// preflightReset checks what a reset needs before the old cluster is removed: the same way
// of getting data that creation would use, and for the base-backup way a base backup to
// restore.
func (s *Service) preflightReset(ctx context.Context, b *Branch, parent, old *registry.Project) error {
	if !b.WithData {
		return nil
	}
	// A base-backup branch holds a full private copy that the reset removes before it clones
	// again, so that space counts; a copy-on-write branch shares blocks with its parent and
	// gives back little, so nothing is assumed.
	var credit int64
	if old.Branch != nil && old.Branch.CloneMethod == MethodBackup {
		credit = dataSize(filepath.Join(s.cfg.Paths().ProjectService(b.Ref, config.SvcPostgres), "data"))
	}
	if err := s.checkDisk(parent.Ref, credit); err != nil {
		return err
	}
	method, _, reason := s.planData(parent.Ref, b.Ref)
	switch method {
	case MethodSchema:
		return invalid("with_data is not possible: %s", reason)
	case MethodBackup:
		if _, err := s.bk.PlanRestoreWith(ctx, parent.Ref, s.now(), backup.RestoreOptions{Latest: true}); err != nil {
			return conflict("cannot reset %s from the base backup of %s: %v", b.Name, parent.Ref, err)
		}
	default:
		if _, err := s.eng.Keys(ctx, parent.Ref); err != nil {
			return fmt.Errorf("read the parent's credentials: %w", err)
		}
	}
	return nil
}

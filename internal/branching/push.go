package branching

import (
	"context"
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/registry"
)

// Push applies the parent's migrations the branch lacks to the branch (a rebase onto the
// parent). Migrations the branch added itself stay. It refuses when a version exists on
// both sides with different content, unless Force is set (that version is skipped).
func (s *Service) Push(ctx context.Context, idOrRef string, in ActionInput) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "push")
	if err != nil {
		return "", err
	}
	if err := s.checkIdle(ctx, b); err != nil {
		return "", err
	}
	if err := s.activeBranch(ctx, b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	r, err := s.begin(b.Ref, "push")
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchRunningMigration, "applying the parent's migrations", nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) {
		parentMigs, err := s.db.Migrations(ctx, parent.Ref)
		if err != nil {
			return "", fmt.Errorf("read the parent's migrations: %w", err)
		}
		branchMigs, err := s.db.Migrations(ctx, b.Ref)
		if err != nil {
			return "", fmt.Errorf("read the branch's migrations: %w", err)
		}
		plan := Compare(parentMigs, branchMigs)
		if len(plan.Conflicts) > 0 && !in.Force {
			var v []string
			for _, c := range plan.Conflicts {
				v = append(v, c.Version)
			}
			return "", fmt.Errorf("%w: versions with different content on the branch and the parent: %s (use force to skip them)", ErrDiverged, strings.Join(v, ", "))
		}
		apply, err := upTo(plan.Apply, in.MigrationVersion)
		if err != nil {
			return "", err
		}
		if len(apply) == 0 {
			return "nothing to push: the branch already has every migration of the parent", nil
		}
		res, err := s.db.Apply(ctx, b.Ref, apply, ApplyOptions{Atomic: true})
		if err != nil {
			return "", fmt.Errorf("apply to the branch: %w", err)
		}
		return fmt.Sprintf("applied %d migration(s) from %s: %s", res.Applied, parent.Ref, versions(apply)), nil
	})
	return r.id, nil
}

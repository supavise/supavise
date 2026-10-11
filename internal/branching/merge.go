package branching

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// mergeLockTimeout bounds how long a merge waits for a lock on the parent: DDL that queues
// behind traffic would block the traffic behind it.
const mergeLockTimeout = 30 * time.Second

// Merge applies the migrations the branch has and its parent lacks to the parent, in one
// transaction where the SQL allows it. It refuses when the histories diverged (the parent
// has migrations the branch lacks, a version differs in content, or a branch migration is
// older than the parent's latest) unless Force is set. It runs in the background and
// returns the run id.
func (s *Service) Merge(ctx context.Context, idOrRef string, in ActionInput) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "merge")
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
	r, err := s.begin(b.Ref, "merge")
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchRunningMigration, "merging into "+parent.Ref, nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) {
		branchMigs, err := s.db.Migrations(ctx, b.Ref)
		if err != nil {
			return "", fmt.Errorf("read the branch's migrations: %w", err)
		}
		parentMigs, err := s.db.Migrations(ctx, parent.Ref)
		if err != nil {
			return "", fmt.Errorf("read the parent's migrations: %w", err)
		}
		plan := Compare(branchMigs, parentMigs)
		if d := plan.Summary(); d != "" && !in.Force {
			return "", fmt.Errorf("%w: %s (use force to merge anyway)", ErrDiverged, d)
		}
		apply, err := upTo(plan.Apply, in.MigrationVersion)
		if err != nil {
			return "", err
		}
		if len(apply) == 0 {
			return "nothing to merge: the parent already has every migration of the branch" + s.functionsNote(ctx, b.Ref, parent.Ref), nil
		}
		// The plan was made before Apply took the parent's apply lock. A merge from another
		// branch may have landed since: judge the history the parent has under the lock, so
		// that a serial merge and a concurrent one are refused alike.
		verify := func(current []Migration) error {
			if d := Compare(branchMigs, current).Summary(); d != "" && !in.Force {
				return fmt.Errorf("%w: the parent changed while this merge waited: %s (use force to merge anyway)", ErrDiverged, d)
			}
			return nil
		}
		res, err := s.db.Apply(ctx, parent.Ref, apply, ApplyOptions{Atomic: true, LockTimeout: mergeLockTimeout, Verify: verify})
		if err != nil {
			return "", fmt.Errorf("apply to the parent (rolled back where the SQL allows): %w", err)
		}
		detail := fmt.Sprintf("merged %d migration(s) into %s: %s", res.Applied, parent.Ref, versions(apply))
		if res.NonAtomic {
			detail += " (not atomic: a statement cannot run inside a transaction)"
		}
		if in.Force && len(plan.Conflicts) > 0 {
			detail += fmt.Sprintf("; skipped %d version(s) with different content", len(plan.Conflicts))
		}
		return detail + s.functionsNote(ctx, b.Ref, parent.Ref), nil
	})
	return r.id, nil
}

// functionsNote is the sentence a merge adds when the branch has Edge Functions that the
// parent lacks or has in another version: a merge carries migrations only, and a client that
// reads "merged" must not take the functions to be on the parent. Empty when there is nothing
// to say.
func (s *Service) functionsNote(ctx context.Context, branchRef, parentRef string) string {
	if s.fns == nil {
		return ""
	}
	bf, err := s.fns.Digests(ctx, branchRef)
	if err == nil {
		var pf map[string]string
		if pf, err = s.fns.Digests(ctx, parentRef); err == nil {
			var differ []string
			for slug, d := range bf {
				if pd, ok := pf[slug]; !ok || pd != d {
					differ = append(differ, slug)
				}
			}
			if len(differ) == 0 {
				return ""
			}
			sort.Strings(differ)
			return fmt.Sprintf("; Edge Functions are NOT merged: %d function(s) of the branch are missing from the parent or differ from it (%s); deploy them to the parent (supabase functions deploy --project-ref %s)",
				len(differ), strings.Join(differ, ", "), parentRef)
		}
	}
	s.log.Warn("merge: could not compare the Edge Functions of the branch and the parent", "branch", branchRef, "err", err)
	return "; Edge Functions are not merged (a merge carries migrations only) and could not be compared"
}

// Diff returns, as SQL text, what Merge would apply: the branch's migrations the parent lacks.
func (s *Service) Diff(ctx context.Context, idOrRef string) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "diff")
	if err != nil {
		return "", err
	}
	if err := s.activeBranch(ctx, b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	branchMigs, err := s.db.Migrations(ctx, b.Ref)
	if err != nil {
		return "", err
	}
	parentMigs, err := s.db.Migrations(ctx, parent.Ref)
	if err != nil {
		return "", err
	}
	plan := Compare(branchMigs, parentMigs)
	var sb strings.Builder
	fmt.Fprintf(&sb, "-- migrations of branch %s that %s lacks\n", b.Name, parent.Ref)
	if d := plan.Summary(); d != "" {
		fmt.Fprintf(&sb, "-- divergence: %s\n", d)
	}
	for _, m := range plan.Apply {
		fmt.Fprintf(&sb, "\n-- migration %s %s\n", m.Version, m.Name)
		for _, st := range m.Statements {
			sb.WriteString(strings.TrimRight(st, "; \n\t") + ";\n")
		}
	}
	if len(plan.Apply) == 0 {
		sb.WriteString("-- nothing to merge\n")
	}
	return sb.String(), nil
}

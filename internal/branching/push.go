package branching

import (
	"context"
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/registry"
)

// Push brings the parent into the branch (a rebase onto the parent). It applies the parent's
// migrations the branch lacks and, for the schema changes the parent got outside migrations (the
// SQL editor, the Table Editor), the statements that make the branch have them too, in one
// transaction. The parent's Edge Functions that are new or changed are copied over the branch's
// (functions only the branch has stay, nothing is deleted; a function with no recorded base, as every
// function of a branch an earlier release made, stays as the branch has it unless Force is set).
// Migrations the branch added itself stay,
// and so do its own schema changes. It refuses when a version exists on both sides with different
// content, or when the parent and the branch changed the same schema object differently, unless Force
// is set (the parent's version then wins and the differing version is skipped). Data is never
// touched and the seed is not run again.
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
	r, err := s.beginRun(ctx, b.Ref, "push", b.Ref, parent.Ref)
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchRunningMigration, "applying the parent's changes", nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) { return s.doPush(ctx, b, parent, in) })
	return r.id, nil
}

func (s *Service) doPush(ctx context.Context, b *Branch, parent *registry.Project, in ActionInput) (string, error) {
	s.phase(ctx, StepPull)
	parentMigs, err := s.db.Migrations(ctx, parent.Ref)
	if err != nil {
		return "", fmt.Errorf("read the parent's migrations: %w", err)
	}
	branchMigs, err := s.db.Migrations(ctx, b.Ref)
	if err != nil {
		return "", fmt.Errorf("read the branch's migrations: %w", err)
	}
	s.phase(ctx, StepMigrate)
	s.logRun(ctx, "the parent has %d migration(s) and the branch %d", len(parentMigs), len(branchMigs))
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
	// A parent migration that another migration of the parent covers runs in full when that one is not
	// pushed (cut by the version) and the branch does not have it.
	apply = settleCovers(apply, branchMigs)
	var parts []string
	if len(apply) > 0 {
		if _, err := s.applyHistory(ctx, b.Ref, apply, ApplyOptions{Atomic: true}); err != nil {
			return "", fmt.Errorf("apply to the branch: %w", err)
		}
		parts = append(parts, fmt.Sprintf("applied %d migration(s) from %s: %s", len(apply), parent.Ref, versions(apply)))
	}
	// A push that stops at a version leaves the rest of the parent's schema alone, but the objects its
	// migrations made are the branch's and the parent's alike now: the base moves to where the two sides
	// agree, so that the diff of the branch does not list them as the branch's own changes.
	if in.MigrationVersion == "" {
		note, err := s.pushSchema(ctx, b, parent, in)
		if err != nil {
			return "", err
		}
		if note != "" {
			parts = append(parts, note)
		}
	} else if len(apply) > 0 {
		if _, ok, err := s.getBase(ctx, b.Ref); err == nil && ok {
			s.syncBase(ctx, b, parent, "push")
		}
	}
	s.phase(ctx, StepDeploy)
	fnNote, err := s.pushFunctions(ctx, b, parent, in.Force)
	if err != nil {
		return "", err
	}
	if fnNote != "" {
		parts = append(parts, fnNote)
	}
	if len(parts) == 0 {
		return "nothing to push: the branch already has every migration, schema change and Edge Function of the parent", nil
	}
	return strings.Join(parts, "; "), nil
}

// pushSchema takes the parent's schema changes since the base into the branch: everything the parent
// added, changed or removed that the branch did not change itself. The branch's changes stay. One
// transaction runs them and records them as a migration of the branch only (pushMigrationPrefix), so
// that a reset of the branch replays them and a merge does not run them on the parent, which has
// them. The base then moves to where the two agree.
func (s *Service) pushSchema(ctx context.Context, b *Branch, parent *registry.Project, in ActionInput) (string, error) {
	sd := s.schemaDB()
	if sd == nil {
		return "", nil
	}
	base, ok, err := s.getBase(ctx, b.Ref)
	if err != nil {
		return "", err
	}
	if !ok {
		// A branch made before bases existed: its schema changes cannot be told from the parent's.
		s.logRun(ctx, "the branch has no schema base (it was made by an earlier release); only the parent's migrations were pushed")
		return "", nil
	}
	branchSnap, err := sd.Snapshot(ctx, b.Ref, schemadiff.ReadOptions{})
	if err != nil {
		return "", fmt.Errorf("read the schema of the branch: %w", err)
	}
	parentSnap, err := sd.Snapshot(ctx, parent.Ref, schemadiff.ReadOptions{})
	if err != nil {
		return "", fmt.Errorf("read the schema of the parent: %w", err)
	}
	merged, conflicts := schemadiff.Merge3(base, branchSnap, parentSnap, in.Force)
	if len(conflicts) > 0 && !in.Force {
		return "", fmt.Errorf("%w: the parent and the branch both changed %s (use force to take the parent's version)", ErrDiverged, strings.Join(conflicts, "; "))
	}
	res := schemadiff.Diff(branchSnap, merged, schemadiff.Options{})
	var note string
	if !res.Empty() {
		if schemadiff.HasUnsupported(res.SQL()) {
			return "", fmt.Errorf("the parent's schema changes include some this node cannot express as SQL (%s); apply them to the branch by hand",
				strings.Join(unsupportedLines(res.SQL()), "; "))
		}
		m := Migration{Version: s.now().UTC().Format("20060102150405"), Name: pushMigrationPrefix + s.now().UTC().Format("20060102150405"), Statements: []string{res.SQL()}}
		if _, err := s.db.Apply(ctx, b.Ref, []Migration{m}, ApplyOptions{Atomic: true}); err != nil {
			return "", fmt.Errorf("apply the parent's schema changes to the branch (rolled back where the SQL allows): %w", err)
		}
		note = fmt.Sprintf("applied the parent's schema changes that were not migrations (%d statement(s), recorded as %s)", len(res.Statements), m.Name)
		if len(conflicts) > 0 {
			note += fmt.Sprintf("; took the parent's version of %s", strings.Join(conflicts, "; "))
		}
	}
	// Where the two sides agree now becomes the base, whether or not anything had to run.
	after, err := sd.Snapshot(ctx, b.Ref, schemadiff.ReadOptions{})
	if err != nil {
		s.logRun(ctx, "could not read the branch again to advance its schema base: %v", err)
		return note, nil
	}
	if err := s.putBase(ctx, b.Ref, schemadiff.Sync(base, after, parentSnap), "push"); err != nil {
		s.logRun(ctx, "could not advance the schema base of the branch: %v", err)
	}
	return note, nil
}

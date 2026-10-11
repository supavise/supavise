package branching

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/registry"
)

// mergeLockTimeout bounds how long a merge waits for a lock on the parent: DDL that queues
// behind traffic would block the traffic behind it.
const mergeLockTimeout = 30 * time.Second

// Merge brings what the branch added into its parent. It runs in the background and returns the
// run id.
//
// Two flows end here, as on hosted. The CLI and the MCP server record the branch's changes as
// migrations (`db push`, apply_migration), and a merge applies the ones the parent lacks, in one
// transaction under the parent's apply lock, refusing when the histories diverged (the parent has
// migrations the branch lacks, a version differs in content, or a branch migration is older than the
// parent's latest) unless Force is set. Studio's merge page records the branch's schema diff as one
// migration without running it (PUT .../database/migrations, named branch_merge_*): when the branch
// has such a migration the parent lacks, the merge checks that the diff is still what was recorded
// (it fails with "refresh" when the branch or production moved), runs only that migration on the
// parent and records the branch's other migrations there without running them, so that the
// histories end up equal and no statement runs twice.
//
// Either way the Edge Functions the branch created or changed since it was made are deployed to
// the parent (never deleted from it); a function both sides changed makes the merge fail before
// anything runs, unless Force is set. The branch is kept.
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
	r, err := s.beginRun(ctx, b.Ref, "merge", parent.Ref, parent.Ref)
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchRunningMigration, "merging into "+parent.Ref, nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) { return s.doMerge(ctx, b, parent, in) })
	return r.id, nil
}

func (s *Service) doMerge(ctx context.Context, b *Branch, parent *registry.Project, in ActionInput) (string, error) {
	s.phase(ctx, StepPull)
	branchMigs, err := s.db.Migrations(ctx, b.Ref)
	if err != nil {
		return "", fmt.Errorf("read the branch's migrations: %w", err)
	}
	parentMigs, err := s.db.Migrations(ctx, parent.Ref)
	if err != nil {
		return "", fmt.Errorf("read the parent's migrations: %w", err)
	}
	fm, err := s.planFunctionMerge(ctx, b, parent)
	if err != nil {
		return "", err
	}
	if len(fm.conflicts) > 0 && !in.Force {
		return "", fmt.Errorf("%w: the branch and the parent both changed the Edge Function(s) %s; update the branch (push) or use force to deploy the branch's version",
			ErrDiverged, strings.Join(fm.conflicts, ", "))
	}
	s.phase(ctx, StepMigrate)
	own := withoutPushed(branchMigs)
	s.logRun(ctx, "the branch has %d migration(s) and the parent %d", len(own), len(parentMigs))
	plan := Compare(own, parentMigs)

	var detail string
	if rec := recordedDiff(plan.Apply); rec != nil && s.schemaDB() != nil && (in.MigrationVersion == "" || rec.Version <= in.MigrationVersion) {
		detail, err = s.mergeRecorded(ctx, b, parent, in, own, plan, rec)
	} else {
		detail, err = s.mergeHistory(ctx, b, parent, in, own, plan)
	}
	if err != nil {
		return "", err
	}
	s.phase(ctx, StepDeploy)
	fnNote, err := s.deployFunctions(ctx, b, parent, fm, in.Force)
	if err != nil {
		return "", err
	}
	if fnNote != "" {
		if strings.HasPrefix(detail, "nothing to merge") {
			detail = "no migrations to merge: the parent already has every migration of the branch"
		}
		detail += "; " + fnNote
	}
	return detail, nil
}

// withoutPushed drops the migrations a push recorded on the branch for the parent's own schema
// changes (pushMigrationPrefix): the parent has those changes, and a merge must not run them there.
func withoutPushed(ms []Migration) []Migration {
	out := make([]Migration, 0, len(ms))
	for _, m := range ms {
		if !strings.HasPrefix(m.Name, pushMigrationPrefix) {
			out = append(out, m)
		}
	}
	return out
}

// recordedDiff returns the newest migration of the plan that Studio's merge page recorded (named
// branch_merge_*), nil when there is none.
func recordedDiff(apply []Migration) *Migration {
	var rec *Migration
	for i := range apply {
		if strings.HasPrefix(apply[i].Name, mergeMigrationPrefix) && (rec == nil || apply[i].Version > rec.Version) {
			rec = &apply[i]
		}
	}
	return rec
}

// mergeHistory is the merge of the CLI and the MCP server: run the branch's migrations that the
// parent lacks.
func (s *Service) mergeHistory(ctx context.Context, b *Branch, parent *registry.Project, in ActionInput, own []Migration, plan Plan) (string, error) {
	if d := plan.Summary(); d != "" && !in.Force {
		return "", fmt.Errorf("%w: %s (use force to merge anyway)", ErrDiverged, d)
	}
	apply, err := upTo(plan.Apply, in.MigrationVersion)
	if err != nil {
		return "", err
	}
	if len(apply) == 0 {
		return "nothing to merge: the parent already has every migration of the branch", nil
	}
	// The plan was made before Apply took the parent's apply lock. A merge from another
	// branch may have landed since: judge the history the parent has under the lock, so
	// that a serial merge and a concurrent one are refused alike.
	verify := func(current []Migration) error {
		if d := Compare(own, current).Summary(); d != "" && !in.Force {
			return fmt.Errorf("%w: the parent changed while this merge waited: %s (use force to merge anyway)", ErrDiverged, d)
		}
		return nil
	}
	res, err := s.applyHistory(ctx, parent.Ref, apply, ApplyOptions{Atomic: true, LockTimeout: mergeLockTimeout, Verify: verify})
	if err != nil {
		return "", fmt.Errorf("apply to the parent (rolled back where the SQL allows): %w", err)
	}
	detail := fmt.Sprintf("merged %d migration(s) into %s: %s", len(apply), parent.Ref, versions(apply))
	if res.NonAtomic {
		detail += " (not atomic: a statement cannot run inside a transaction)"
	}
	if in.Force && len(plan.Conflicts) > 0 {
		detail += fmt.Sprintf("; skipped %d version(s) with different content", len(plan.Conflicts))
	}
	s.syncBase(ctx, b, parent, "merge")
	return detail, nil
}

// mergeRecorded is the merge of Studio's merge page, which recorded the branch's schema diff as one
// migration (rec) without running it.
func (s *Service) mergeRecorded(ctx context.Context, b *Branch, parent *registry.Project, in ActionInput, own []Migration, plan Plan, rec *Migration) (string, error) {
	sd := s.schemaDB()
	// The parent having migrations the branch lacks, or a version differing, means the branch is not
	// up to date; the order of the branch's own migrations does not matter here, they are not run.
	div := plan
	div.OutOfOrder = nil
	if d := div.Summary(); d != "" && !in.Force {
		return "", fmt.Errorf("%w: %s (update the branch, or use force to merge anyway)", ErrDiverged, d)
	}
	if schemadiff.HasUnsupported(rec.sql()) {
		return "", fmt.Errorf("the recorded schema diff %s holds changes this node cannot express as SQL (%s); apply them to the parent by hand and record the rest",
			rec.Version, strings.Join(unsupportedLines(rec.sql()), "; "))
	}
	// The diff must still be what the page showed and the user approved.
	res, branchSnap, err := s.schemaChanges(ctx, b, parent, schemadiff.ReadOptions{})
	if err != nil {
		return "", err
	}
	if scriptHash(res.SQL()) != scriptHash(rec.sql()) && !in.Force {
		return "", fmt.Errorf("%w: production or branch changed since the diff was recorded; refresh", ErrDiverged)
	}
	if base, ok, err := s.getBase(ctx, b.Ref); err != nil {
		return "", err
	} else if ok {
		parentSnap, err := sd.Snapshot(ctx, parent.Ref, schemadiff.ReadOptions{})
		if err != nil {
			return "", fmt.Errorf("read the schema of the parent: %w", err)
		}
		if _, conflicts := schemadiff.Merge3(base, parentSnap, branchSnap, false); len(conflicts) > 0 && !in.Force {
			return "", fmt.Errorf("%w: production and the branch both changed %s (use force to merge anyway)", ErrDiverged, strings.Join(conflicts, "; "))
		}
	}
	s.logRun(ctx, "the recorded schema diff %s is current; running it on the parent", rec.Version)
	verify := func(current []Migration) error {
		c := Compare(own, current)
		c.OutOfOrder = nil
		if d := c.Summary(); d != "" && !in.Force {
			return fmt.Errorf("%w: the parent changed while this merge waited: %s (use force to merge anyway)", ErrDiverged, d)
		}
		return nil
	}
	applied, err := s.db.Apply(ctx, parent.Ref, []Migration{*rec}, ApplyOptions{Atomic: true, LockTimeout: mergeLockTimeout, Verify: verify})
	if err != nil {
		return "", fmt.Errorf("apply to the parent (rolled back where the SQL allows): %w", err)
	}
	// The branch's other migrations are part of what the diff ran. They join the parent's history as
	// covered by the diff (kept, not run again: a replay of the history would otherwise create their
	// tables twice), and the branch's own rows are marked the same way, so that both histories hold the
	// same migrations and either replays to the same schema.
	var others []Migration
	for _, m := range plan.Apply {
		if m.Version != rec.Version {
			others = append(others, m)
		}
	}
	if len(others) > 0 {
		covered := withCovered(others, rec.Version)
		if _, err := sd.Record(ctx, parent.Ref, covered); err != nil {
			return "", fmt.Errorf("record the branch's other migrations in the parent's history: %w", err)
		}
		if _, err := sd.Record(ctx, b.Ref, covered); err != nil {
			return "", fmt.Errorf("mark the branch's other migrations as covered by the merged diff: %w", err)
		}
	}
	s.syncBase(ctx, b, parent, "merge")
	detail := fmt.Sprintf("merged the recorded schema diff %s into %s", rec.Version, parent.Ref)
	if applied.NonAtomic {
		detail += " (not atomic: a statement cannot run inside a transaction)"
	}
	if len(others) > 0 {
		detail += fmt.Sprintf("; recorded %d other migration(s) of the branch in the parent's history without running them (the diff holds their changes): %s", len(others), versions(others))
	}
	return detail, nil
}

// unsupportedLines returns the unsupported-change markers of a script, without the marker.
func unsupportedLines(script string) []string {
	var out []string
	for _, l := range strings.Split(script, "\n") {
		if rest, ok := strings.CutPrefix(l, schemadiff.UnsupportedMarker); ok {
			out = append(out, strings.TrimSpace(rest))
		}
	}
	return out
}

// syncBase advances the schema base of the branch after the two sides exchanged changes: what the
// branch and its parent now have in the same form becomes the base. A failure only costs the
// precision of the next diff, and is logged.
func (s *Service) syncBase(ctx context.Context, b *Branch, parent *registry.Project, source string) {
	sd := s.schemaDB()
	if sd == nil || registry.BranchContentOf(s.reg) == nil {
		return
	}
	err := func() error {
		branchSnap, err := sd.Snapshot(ctx, b.Ref, schemadiff.ReadOptions{})
		if err != nil {
			return err
		}
		parentSnap, err := sd.Snapshot(ctx, parent.Ref, schemadiff.ReadOptions{})
		if err != nil {
			return err
		}
		base, _, err := s.getBase(ctx, b.Ref)
		if err != nil {
			return err
		}
		return s.putBase(ctx, b.Ref, schemadiff.Sync(base, branchSnap, parentSnap), source)
	}()
	if err != nil {
		s.log.Warn("could not advance the schema base of a branch", "ref", b.Ref, "err", err)
		s.logRun(ctx, "could not advance the schema base of the branch: %v", err)
	}
}

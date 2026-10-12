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
// migration without running it (PUT .../database/migrations, named branch_merge_*): when the newest
// migration of the branch is such a recording and the parent lacks it, the merge checks that the diff is
// still what was recorded (it fails with "refresh" when the branch or production moved, whatever Force
// says), runs that migration on the parent, records the branch's other migrations there as covered by it
// and runs, after it and in the same transaction, the statements of those migrations that a schema diff
// does not hold (data, publications, cron jobs, roles), so that the histories end up equal and no
// statement runs twice. A recorded diff that is not the newest migration of the branch is out of date
// (the branch moved on after it was recorded): the merge ignores it and takes the history flow.
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
	if b, err = s.wakeIfIdle(ctx, b, "merge"); err != nil {
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
	s.healCovers(ctx, b, own, parentMigs)
	plan := Compare(own, parentMigs)

	var detail string
	rec, stale := s.pickRecordedDiff(own, plan, in)
	if rec != nil {
		detail, err = s.mergeRecorded(ctx, b, parent, in, own, plan, rec)
	} else {
		if len(stale) > 0 {
			// The branch changed after Studio recorded these: the migrations of the branch are what the
			// merge runs, and a recorded copy of their changes would run them twice.
			s.logRun(ctx, "ignoring the schema diff(s) recorded by the merge page that the branch has moved past: %s", versions(stale))
			own = without(own, stale)
			plan = Compare(own, parentMigs)
		}
		detail, err = s.mergeHistory(ctx, b, parent, in, own, parentMigs, plan, stale)
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

// without returns ms without the migrations at the versions of drop.
func without(ms, drop []Migration) []Migration {
	gone := make(map[string]bool, len(drop))
	for _, m := range drop {
		gone[m.Version] = true
	}
	out := make([]Migration, 0, len(ms))
	for _, m := range ms {
		if !gone[m.Version] {
			out = append(out, m)
		}
	}
	return out
}

// pickRecordedDiff decides whether the merge takes the flow of Studio's merge page. It does when the
// newest migration of the plan that the page recorded (branch_merge_*) is also the newest migration of
// the branch, and the merge is not cut at an older version: the page records the diff and calls merge,
// so nothing is newer. A recording with a newer migration after it is out of date, the branch having
// moved on (a failed or abandoned attempt, then an apply_migration or a db push): it is returned as
// stale, with the older ones, for the history flow to leave out.
func (s *Service) pickRecordedDiff(own []Migration, plan Plan, in ActionInput) (rec *Migration, stale []Migration) {
	if s.schemaDB() == nil {
		return nil, nil
	}
	for _, m := range plan.Apply {
		if strings.HasPrefix(m.Name, mergeMigrationPrefix) {
			stale = append(stale, m)
		}
	}
	rec = recordedDiff(plan.Apply)
	if rec == nil {
		return nil, nil
	}
	current := in.MigrationVersion == "" || rec.Version <= in.MigrationVersion
	for _, m := range own {
		if m.Version > rec.Version {
			current = false
		}
	}
	if current {
		return rec, nil
	}
	return nil, stale
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
// parent lacks. stale are the recordings of Studio's merge page that were left out of own and plan.
func (s *Service) mergeHistory(ctx context.Context, b *Branch, parent *registry.Project, in ActionInput, own, parentMigs []Migration, plan Plan, stale []Migration) (string, error) {
	if d := plan.Summary(); d != "" && !in.Force {
		return "", fmt.Errorf("%w: %s (use force to merge anyway)", ErrDiverged, d)
	}
	apply, err := upTo(plan.Apply, in.MigrationVersion)
	if err != nil {
		return "", err
	}
	// A migration covered by a recording that was left out runs in full.
	apply = settleCovers(apply, parentMigs)
	if len(apply) == 0 {
		msg := "nothing to merge: the parent already has every migration of the branch"
		if len(stale) > 0 {
			msg += fmt.Sprintf(" (the schema diff the merge page recorded on the branch, %s, was ignored because the branch changed after it; open the merge page again to merge schema changes made outside migrations)", versions(stale))
		}
		return msg, nil
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
		detail += " (not atomic: a statement cannot run inside a transaction block)"
	}
	if in.Force && len(plan.Conflicts) > 0 {
		detail += fmt.Sprintf("; skipped %d version(s) with different content", len(plan.Conflicts))
	}
	if len(stale) > 0 {
		detail += fmt.Sprintf("; ignored the schema diff(s) the merge page recorded on the branch (%s), because the branch changed after them", versions(stale))
	}
	s.syncBase(ctx, b, parent, "merge")
	return detail, nil
}

// mergeRecorded is the merge of Studio's merge page, which recorded the branch's schema diff as one
// migration (rec) without running it.
//
// Everything lands in one transaction on the parent: rec; the branch's other migrations the parent
// lacks, recorded as covered by it (a schema diff holds their schema changes); and the statements of
// those that a schema diff does not hold (an INSERT, a cron.schedule, an ALTER PUBLICATION, a role), which
// run after rec. A failure rolls the whole of it back, so that a retry starts from the same state.
func (s *Service) mergeRecorded(ctx context.Context, b *Branch, parent *registry.Project, in ActionInput, own []Migration, plan Plan, rec *Migration) (string, error) {
	sd := s.schemaDB()
	// The parent having migrations the branch lacks, or a version differing, means the branch is not
	// up to date; the order of the branch's own migrations does not matter here, they are not run.
	div := plan
	div.OutOfOrder = nil
	if d := div.Summary(); d != "" && !in.Force {
		return "", fmt.Errorf("%w: %s (update the branch, or use force to merge anyway)", ErrDiverged, d)
	}
	unsupported := unsupportedLines(rec.sql())
	if len(unsupported) > 0 && !in.Force {
		return "", fmt.Errorf("the recorded schema diff %s holds changes this node cannot express as SQL (%s); apply them to the parent by hand and record the rest, or use force to merge without them",
			rec.Version, strings.Join(unsupported, "; "))
	}
	// The diff must still be what the page showed and the user approved. Force does not merge a diff
	// that is out of date: what would run is not what was approved.
	res, branchSnap, err := s.schemaChanges(ctx, b, parent, schemadiff.ReadOptions{})
	if err != nil {
		return "", err
	}
	if scriptHash(res.SQL()) != scriptHash(rec.sql()) {
		return "", fmt.Errorf("%w: production or the branch changed since the diff was recorded; refresh the merge page (or record a newer migration on the branch, which makes the merge run the branch's migrations instead)", ErrDiverged)
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
	// The branch's other migrations are part of what the diff ran. They join the parent's history as
	// covered by the diff (their schema changes are not run again: a replay of the history would
	// otherwise create their tables twice), and the branch's own rows are marked the same way, so that
	// both histories hold the same migrations and either replays to the same schema. What a diff does
	// not hold of them runs after it.
	var others []Migration
	for _, m := range plan.Apply {
		if m.Version != rec.Version {
			others = append(others, m)
		}
	}
	covered := withCovered(others, rec.Version)
	var carriedFrom []string
	nCarried := 0
	for _, m := range covered {
		if c := m.carried(); len(c) > 0 {
			nCarried += len(c)
			carriedFrom = append(carriedFrom, m.Version)
		}
	}
	s.logRun(ctx, "the recorded schema diff %s is current; running it on the parent", rec.Version)
	if nCarried > 0 {
		s.logRun(ctx, "running %d statement(s) of the branch's other migrations that a schema diff does not hold, after the diff: %s", nCarried, strings.Join(carriedFrom, ", "))
	}
	verify := func(current []Migration) error {
		c := Compare(own, current)
		c.OutOfOrder = nil
		if d := c.Summary(); d != "" && !in.Force {
			return fmt.Errorf("%w: the parent changed while this merge waited: %s (use force to merge anyway)", ErrDiverged, d)
		}
		return nil
	}
	applied, err := s.db.Apply(ctx, parent.Ref, append([]Migration{*rec}, covered...), ApplyOptions{Atomic: true, LockTimeout: mergeLockTimeout, Verify: verify})
	if err != nil {
		return "", fmt.Errorf("apply to the parent (rolled back where the SQL allows): %w", err)
	}
	detail := fmt.Sprintf("merged the recorded schema diff %s into %s", rec.Version, parent.Ref)
	if applied.NonAtomic {
		detail += " (not atomic: a statement cannot run inside a transaction block)"
	}
	if len(others) > 0 {
		detail += fmt.Sprintf("; recorded %d other migration(s) of the branch in the parent's history without running their schema changes (the diff holds them): %s", len(others), versions(others))
		if nCarried > 0 {
			detail += fmt.Sprintf("; ran %d statement(s) of them that a schema diff does not hold (%s)", nCarried, strings.Join(carriedFrom, ", "))
		}
	}
	if len(unsupported) > 0 {
		detail += fmt.Sprintf("; not merged, because this node cannot express them as SQL (force): %s", strings.Join(unsupported, "; "))
	}
	// The parent is merged for good by now. Marking the branch's rows is what lets a reset of the branch
	// skip their schema changes, and its failure must not turn a merge that worked into a failed run.
	if len(covered) > 0 {
		if _, err := sd.Record(ctx, b.Ref, covered); err != nil {
			s.log.Warn("could not mark the migrations of a merged branch as covered", "ref", b.Ref, "err", err)
			s.logRun(ctx, "the merge worked, but the branch's other migrations could not be marked as covered by the merged diff (%v); a reset of the branch would run their schema changes twice until a later merge marks them", err)
			detail += "; could not mark the branch's other migrations as covered (see the log)"
		}
	}
	s.syncBase(ctx, b, parent, "merge")
	return detail, nil
}

// healCovers marks, on the branch, the migrations that the parent records as covered by a recorded diff
// both sides have, when the branch does not yet: the mark is written after the parent was merged, and its
// failure is only logged, so a later merge puts it right. It changes no schema.
func (s *Service) healCovers(ctx context.Context, b *Branch, own, parentMigs []Migration) {
	sd := s.schemaDB()
	if sd == nil {
		return
	}
	ownHas := make(map[string]bool, len(own))
	for _, m := range own {
		ownHas[m.Version] = true
	}
	theirs := make(map[string]Migration, len(parentMigs))
	for _, m := range parentMigs {
		theirs[m.Version] = m
	}
	var fix []Migration
	for _, m := range own {
		if p, ok := theirs[m.Version]; ok && m.Covered == "" && p.Covered != "" && ownHas[p.Covered] && sameMigration(m, p) {
			m.Covered = p.Covered
			fix = append(fix, m)
		}
	}
	if len(fix) == 0 {
		return
	}
	if _, err := sd.Record(ctx, b.Ref, fix); err != nil {
		s.logRun(ctx, "could not mark the migrations %s of the branch as covered, as the parent has them: %v", versions(fix), err)
		return
	}
	s.logRun(ctx, "marked the migrations %s of the branch as covered by the diff that was merged, as the parent has them", versions(fix))
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

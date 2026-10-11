package branching

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/registry"
)

// baselineName is the name of the migration that holds the schema of a parent that has no history
// (the name `supabase db pull` gives the first pull of a remote database).
const baselineName = "remote_schema"

// pushMigrationPrefix names the migrations a push records on a branch for the parent's schema changes
// that were never migrations. They exist only on the branch: the parent has the changes already, so
// a merge leaves them out.
const pushMigrationPrefix = "branch_push_"

// mergeMigrationPrefix names the migration Studio's merge page records (PUT .../database/migrations)
// for the schema diff it shows: the branch's changes in one script.
const mergeMigrationPrefix = "branch_merge_"

// hasUserSchema reports whether a snapshot holds anything a user made that a baseline can carry. The
// extensions, the grants and the default privileges of a new project are not that: a parent with only
// those has no schema to give a branch. The objects the engine does not model (Snapshot.Unmodeled) do
// not count either: they cannot be written into a baseline, and the stock ones of a new project (the
// event triggers of PostgREST and pg_graphql, the Realtime publication) are not listed at all.
func hasUserSchema(s *schemadiff.Snapshot) bool {
	for i := range s.Objects {
		switch s.Objects[i].Kind {
		case schemadiff.KindSchema, schemadiff.KindExtension, schemadiff.KindACL, schemadiff.KindDefACL, schemadiff.KindComment:
			if s.Objects[i].Kind == schemadiff.KindSchema && !strings.HasSuffix(s.Objects[i].Key, "|public") {
				return true // a schema of the user's own
			}
		default:
			return true
		}
	}
	return false
}

// baselineFor gives a new branch the schema of a parent whose migration history is empty (everything
// was built in the SQL editor, the Table Editor or execute_sql). It takes a schema-only dump of the
// parent and applies it to the branch, and only when that worked records it as the first migration of
// the parent's history, without running it there (so that a merge has a common ancestor, as hosted's
// first branch initializes the history). The apply lock of the parent is held from the look at the
// history to the record, so that a second branch made at the same time waits and then replays the
// history the first one recorded. A baseline that fails to run on the branch leaves the parent's history
// empty. Order matters for the Data API default privileges: the branch applied the parent's choice when
// it was created; the baseline's own ALTER DEFAULT PRIVILEGES statements run after it and win.
//
// It dumps only an empty history. If another branch recorded one while this was being made, that
// history is what the branch replays. Custom roles are not captured: grants, default privileges and
// policies that name a role the branch lacks are left out, and the run log says so.
//
// applied reports that the returned migration is already applied to the branch.
func (s *Service) baselineFor(ctx context.Context, j *createJob) (hist []Migration, applied bool, err error) {
	sd := s.schemaDB()
	if sd == nil {
		return nil, false, nil
	}
	snap, err := sd.Snapshot(ctx, j.parent.Ref, schemadiff.ReadOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("read the parent's schema for the baseline: %w", err)
	}
	if !hasUserSchema(snap) {
		for _, u := range snap.Unmodeled {
			s.logRun(ctx, "not captured in the baseline: %s (create it by hand)", u)
		}
		s.logRun(ctx, "the parent has no migration history and no schema of its own: the branch starts empty")
		return nil, false, nil
	}
	roles, err := sd.Roles(ctx, j.ref)
	if err != nil {
		return nil, false, fmt.Errorf("read the roles of the branch: %w", err)
	}
	// A policy or trigger on a table of a managed schema needs that table on the branch.
	have, err := s.managedTables(ctx, sd, j.ref, snap)
	if err != nil {
		return nil, false, err
	}
	plan := schemadiff.Diff(nil, snap, schemadiff.Options{
		RoleExists:  func(n string) bool { return roles[n] },
		TableExists: func(key string) bool { return have[key] },
	})
	if len(plan.Statements) == 0 {
		// Never record an empty baseline as the parent's history.
		if len(plan.Unsupported) > 0 {
			return nil, false, fmt.Errorf("the parent's schema could not be turned into a baseline: %s", strings.Join(plan.Unsupported, "; "))
		}
		for _, n := range plan.Notes {
			s.logRun(ctx, "%s", n)
		}
		s.logRun(ctx, "nothing of the parent's schema can be carried to the branch: it starts empty")
		return nil, false, nil
	}
	for _, u := range plan.Unsupported {
		s.logRun(ctx, "not captured in the baseline: %s", u)
	}
	for _, n := range plan.Notes {
		s.logRun(ctx, "%s", n)
	}
	m := Migration{Version: s.now().UTC().Format("20060102150405"), Name: baselineName, Statements: plan.Statements}
	recorded, err := sd.InitHistory(ctx, j.parent.Ref, m, func(ctx context.Context) error {
		s.setState(ctx, j.ref, registry.BranchRunningMigration, "applying the parent's schema as a baseline", nil)
		_, err := s.applyHistory(ctx, j.ref, []Migration{m}, ApplyOptions{})
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("apply the parent's schema to the branch as a baseline (nothing was recorded in the parent's migration history): %w", err)
	}
	if !recorded {
		// Another branch got there first; its history is now the parent's.
		s.logRun(ctx, "the parent's migration history got a baseline while this one was made; replaying that")
		hist, err = s.db.Migrations(ctx, j.parent.Ref)
		return hist, false, err
	}
	s.logRun(ctx, "the parent had no migration history: recorded its schema as migration %s %s (%d statement(s), nothing was run on the parent)", m.Version, m.Name, len(m.Statements))
	s.event(ctx, j.parent.Ref, "branch.baseline_recorded", map[string]any{
		"branch": j.ref, "version": m.Version, "name": m.Name, "statements": len(m.Statements), "objects": len(snap.Objects), "unsupported": plan.Unsupported,
	})
	return []Migration{m}, true, nil
}

// ---- the schema base --------------------------------------------------------------------

// getBase returns the schema base of a branch (the schema it and its parent last agreed on). ok is
// false for a branch that has none (made before the base existed), or whose base another format wrote.
func (s *Service) getBase(ctx context.Context, branchRef string) (snap *schemadiff.Snapshot, ok bool, err error) {
	st := registry.BranchContentOf(s.reg)
	if st == nil {
		return nil, false, nil
	}
	b, err := st.GetBranchSchemaBase(ctx, branchRef)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(b.Snapshot))
	if err != nil {
		return nil, false, nil
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, false, nil
	}
	snap, err = schemadiff.Unmarshal(raw)
	var verr *schemadiff.VersionError
	if errors.As(err, &verr) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return snap, true, nil
}

// putBase stores snap as the schema base of a branch. source says what took it.
func (s *Service) putBase(ctx context.Context, branchRef string, snap *schemadiff.Snapshot, source string) error {
	st := registry.BranchContentOf(s.reg)
	if st == nil {
		return nil
	}
	raw, err := snap.Marshal()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return st.PutBranchSchemaBase(ctx, registry.BranchSchemaBase{
		BranchRef: branchRef, Engine: schemadiff.FormatVersion, Fingerprint: snap.Fingerprint(), Snapshot: buf.Bytes(), Source: source,
	})
}

// recordBase takes the schema of the branch as it is and stores it as the base. A failure is
// logged in the run and does not fail the operation: without a base the diff falls back to
// comparing the branch with the parent as it is.
func (s *Service) recordBase(ctx context.Context, branchRef, source string) {
	sd := s.schemaDB()
	if sd == nil || registry.BranchContentOf(s.reg) == nil {
		return
	}
	snap, err := sd.Snapshot(ctx, branchRef, schemadiff.ReadOptions{})
	if err == nil {
		err = s.putBase(ctx, branchRef, snap, source)
	}
	if err != nil {
		s.log.Warn("could not record the schema base of a branch", "ref", branchRef, "err", err)
		s.logRun(ctx, "could not record the schema base of the branch (its diff will compare it with the parent as it is): %v", err)
		return
	}
	s.logRun(ctx, "recorded the schema base of the branch (%d objects)", len(snap.Objects))
}

// managedTables returns the keys of the tables and views, in the managed schemas the snapshot hangs
// policies or triggers on, that exist on the project ref.
func (s *Service) managedTables(ctx context.Context, sd SchemaDatabase, ref string, snap *schemadiff.Snapshot) (map[string]bool, error) {
	idx := map[string]bool{}
	for i := range snap.Objects {
		idx[snap.Objects[i].Key] = true
	}
	schemas := map[string]bool{}
	for i := range snap.Objects {
		o := &snap.Objects[i]
		if (o.Kind == schemadiff.KindPolicy || o.Kind == schemadiff.KindTrigger) && o.Parent != "" && !idx[o.Parent] {
			if parts := strings.SplitN(o.Parent, "|", 3); len(parts) == 3 {
				schemas[parts[1]] = true
			}
		}
	}
	have := map[string]bool{}
	if len(schemas) == 0 {
		return have, nil
	}
	list := make([]string, 0, len(schemas))
	for n := range schemas {
		list = append(list, n)
	}
	sort.Strings(list)
	managed, err := sd.Snapshot(ctx, ref, schemadiff.ReadOptions{Schemas: list})
	if err != nil {
		return nil, fmt.Errorf("read the managed schemas of the branch: %w", err)
	}
	for i := range managed.Objects {
		have[managed.Objects[i].Key] = true
	}
	return have, nil
}

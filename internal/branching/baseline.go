package branching

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
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

// hasUserSchema reports whether a snapshot holds anything a user made. The extensions, the grants
// and the default privileges of a new project are not that: a parent with only those has no schema
// to give a branch.
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
	return len(s.Unmodeled) > 0
}

// baselineFor gives a new branch the schema of a parent whose migration history is empty (everything
// was built in the SQL editor, the Table Editor or execute_sql). It takes a schema-only dump of the
// parent, records it as the first migration of the parent's history, without running it there (so
// that a merge has a common ancestor, as hosted's first branch initializes the history), and returns
// it for the branch to run. Order matters for the Data API default privileges: the branch applied
// the parent's choice when it was created; the baseline's own ALTER DEFAULT PRIVILEGES statements run
// after it and win.
//
// It dumps only an empty history. If another branch recorded one while this was being made, that
// history is what the branch replays. Custom roles are not captured: grants, default privileges and
// policies that name a role the branch lacks are left out, and the run log says so.
func (s *Service) baselineFor(ctx context.Context, j *createJob) ([]Migration, error) {
	sd := s.schemaDB()
	if sd == nil {
		return nil, nil
	}
	snap, err := sd.Snapshot(ctx, j.parent.Ref, schemadiff.ReadOptions{})
	if err != nil {
		return nil, fmt.Errorf("read the parent's schema for the baseline: %w", err)
	}
	if !hasUserSchema(snap) {
		s.logRun(ctx, "the parent has no migration history and no schema of its own: the branch starts empty")
		return nil, nil
	}
	roles, err := sd.Roles(ctx, j.ref)
	if err != nil {
		return nil, fmt.Errorf("read the roles of the branch: %w", err)
	}
	plan := schemadiff.Diff(nil, snap, schemadiff.Options{RoleExists: func(n string) bool { return roles[n] }})
	for _, u := range plan.Unsupported {
		s.logRun(ctx, "not captured in the baseline: %s", u)
	}
	for _, n := range plan.Notes {
		s.logRun(ctx, "%s", n)
	}
	m := Migration{Version: s.now().UTC().Format("20060102150405"), Name: baselineName, Statements: plan.Statements}
	recorded, err := sd.InitHistory(ctx, j.parent.Ref, m)
	if err != nil {
		return nil, fmt.Errorf("record the baseline in the parent's migration history: %w", err)
	}
	if !recorded {
		// Another branch got there first; its history is now the parent's.
		s.logRun(ctx, "the parent's migration history got a baseline while this one was made; replaying that")
		return s.db.Migrations(ctx, j.parent.Ref)
	}
	s.logRun(ctx, "the parent had no migration history: recorded its schema as migration %s %s (%d statement(s), nothing was run on the parent)", m.Version, m.Name, len(m.Statements))
	s.event(ctx, j.parent.Ref, "branch.baseline_recorded", map[string]any{
		"branch": j.ref, "version": m.Version, "name": m.Name, "statements": len(m.Statements), "objects": len(snap.Objects), "unsupported": plan.Unsupported,
	})
	return []Migration{m}, nil
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

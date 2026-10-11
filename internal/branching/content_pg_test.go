package branching

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// The tests in this file run the branching operations over real databases: the parent and each
// branch are databases on the Postgres of SUPAVISE_TEST_DATABASE_URL (a superuser), made and dropped
// by the test. The engine, the registry and the Edge Function store are fakes. Linux-only behavior
// (systemd, the clone) is not part of it.

var pgSeq atomic.Int64

// pgManaged stands in for what the services create in a Supabase project.
const pgManaged = `
create schema if not exists extensions;
create schema if not exists auth;
create schema if not exists storage;
create table auth.users (id uuid primary key default gen_random_uuid(), email text);
create table storage.buckets (id text primary key, name text);
create table storage.objects (id uuid primary key default gen_random_uuid(), bucket_id text references storage.buckets (id), name text);
alter table storage.objects enable row level security;
grant usage on schema public, auth, storage to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant all on tables to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant all on sequences to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant all on functions to anon, authenticated, service_role;
`

type pgHarness struct {
	*harness
	mu    sync.Mutex
	admin string
	dbs   map[string]string // project ref -> database
}

func newPGHarness(t *testing.T) *pgHarness {
	t.Helper()
	admin := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL is not set")
	}
	h := &pgHarness{harness: newHarness(t, nil), admin: admin, dbs: map[string]string{}}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"anon", "authenticated", "service_role"} {
		_, _ = c.Exec(ctx, "create role "+r+" nologin")
	}
	c.Close(ctx)
	h.makeDB(parentRef)
	h.svc.db = &pgDatabase{dsn: func(_ context.Context, ref string) (string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		name, ok := h.dbs[ref]
		if !ok {
			return "", fmt.Errorf("no database for %s", ref)
		}
		return h.dsn(name), nil
	}}
	h.eng.onCreate = func(req lifecycle.CreateRequest) {
		if req.Ref != parentRef {
			h.makeDB(req.Ref)
		}
	}
	h.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { h.dropDB(ref) }
	t.Cleanup(func() {
		for ref := range h.dbs {
			h.dropDB(ref)
		}
	})
	return h
}

func (h *pgHarness) dsn(db string) string {
	u, _ := url.Parse(h.admin)
	u.Path = "/" + db
	return u.String()
}

func (h *pgHarness) makeDB(ref string) {
	h.t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("bh_%d_%d", time.Now().UnixNano()%1_000_000_000, pgSeq.Add(1))
	c, err := pgx.Connect(ctx, h.admin)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "create database "+name); err != nil {
		h.t.Fatal(err)
	}
	c.Close(ctx)
	h.mu.Lock()
	h.dbs[ref] = name
	h.mu.Unlock()
	h.exec(ref, pgManaged)
}

func (h *pgHarness) dropDB(ref string) {
	h.mu.Lock()
	name, ok := h.dbs[ref]
	delete(h.dbs, ref)
	h.mu.Unlock()
	if !ok {
		return
	}
	ctx := context.Background()
	if c, err := pgx.Connect(ctx, h.admin); err == nil {
		_, _ = c.Exec(ctx, "drop database if exists "+name+" with (force)")
		c.Close(ctx)
	}
}

// exec runs SQL in the database of ref.
func (h *pgHarness) exec(ref, sql string) {
	h.t.Helper()
	if err := h.tryExec(ref, sql); err != nil {
		h.t.Fatalf("exec on %s: %v\n%s", ref, err, sql)
	}
}

func (h *pgHarness) tryExec(ref, sql string) error {
	h.mu.Lock()
	name := h.dbs[ref]
	h.mu.Unlock()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, h.dsn(name))
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	_, err = c.Exec(ctx, sql)
	return err
}

// has reports whether the table exists in the public schema of ref.
func (h *pgHarness) has(ref, table string) bool {
	h.t.Helper()
	h.mu.Lock()
	name := h.dbs[ref]
	h.mu.Unlock()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, h.dsn(name))
	if err != nil {
		h.t.Fatal(err)
	}
	defer c.Close(ctx)
	var ok bool
	if err := c.QueryRow(ctx, `select to_regclass($1) is not null`, "public."+table).Scan(&ok); err != nil {
		h.t.Fatal(err)
	}
	return ok
}

func (h *pgHarness) migrations(ref string) []Migration {
	h.t.Helper()
	ms, err := h.svc.db.Migrations(context.Background(), ref)
	if err != nil {
		h.t.Fatal(err)
	}
	return ms
}

func (h *pgHarness) snapshot(ref string) *schemadiff.Snapshot {
	h.t.Helper()
	s, err := h.svc.schemaDB().Snapshot(context.Background(), ref, schemadiff.ReadOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// apply runs a migration on ref and records it, as `supabase db push` does.
func (h *pgHarness) apply(ref string, m Migration) {
	h.t.Helper()
	if _, err := h.svc.db.Apply(context.Background(), ref, []Migration{m}, ApplyOptions{Atomic: true}); err != nil {
		h.t.Fatal(err)
	}
}

// same fails unless the user schemas of two projects are equal.
func (h *pgHarness) same(a, b string) {
	h.t.Helper()
	if d := schemadiff.Diff(h.snapshot(a), h.snapshot(b), schemadiff.Options{}); !d.Empty() {
		h.t.Fatalf("%s and %s differ:\n%s", a, b, d.SQL())
	}
}

// histories fails unless two projects have recorded the same migrations.
func (h *pgHarness) histories(a, b string) {
	h.t.Helper()
	p := Compare(h.migrations(a), h.migrations(b))
	if p.Summary() != "" || len(p.Apply) > 0 || len(p.TargetOnly) > 0 {
		h.t.Fatalf("histories of %s and %s differ: apply %v, only on %s: %v, %s", a, b, versions(p.Apply), b, versions(p.TargetOnly), p.Summary())
	}
}

func (h *pgHarness) merge(ref string, in ActionInput) *Branch {
	h.t.Helper()
	if _, err := h.svc.Merge(context.Background(), ref, in); err != nil {
		h.t.Fatal(err)
	}
	return h.wait(ref)
}

const userSchema = `
create extension if not exists pgcrypto with schema extensions;
create table public.todos (id bigserial primary key, title text not null, owner uuid default gen_random_uuid());
alter table public.todos enable row level security;
create policy todos_read on public.todos for select to authenticated using (true);
create function public.touch() returns trigger language plpgsql as $$ begin return new; end $$;
create trigger todos_touch before update on public.todos for each row execute function public.touch();
create index todos_title on public.todos (title);
create policy "bucket read" on storage.objects for select to anon using (bucket_id = 'public');
create function public.on_signup() returns trigger language plpgsql security definer as $$ begin return new; end $$;
create trigger on_auth_user_created after insert on auth.users for each row execute function public.on_signup();
`

func TestBaselineOfAParentWithoutHistory(t *testing.T) {
	h := newPGHarness(t)
	h.exec(parentRef, userSchema)
	if len(h.migrations(parentRef)) != 0 {
		t.Fatal("the parent has a history before the branch")
	}
	b := h.create("feat", nil)
	h.mustState(b, registry.BranchFunctionsDeployed)

	// The branch has the table, the policy on storage.objects, the trigger on auth.users, the extension.
	h.same(parentRef, b.Ref)
	for _, k := range []string{"policy|storage|objects|bucket read", "trigger|auth|users|on_auth_user_created", "extension|pgcrypto", "table|public|todos"} {
		found := false
		for _, o := range h.snapshot(b.Ref).Objects {
			found = found || o.Key == k
		}
		if !found {
			t.Errorf("the branch lacks %s", k)
		}
	}
	// The parent's history was initialized with the baseline, which was not run there.
	pm := h.migrations(parentRef)
	if len(pm) != 1 || pm[0].Name != baselineName {
		t.Fatalf("parent history = %+v", pm)
	}
	h.histories(parentRef, b.Ref)
	// A second branch reuses it: dumped only on an empty history.
	b2 := h.create("feat2", nil)
	if pm2 := h.migrations(parentRef); len(pm2) != 1 || pm2[0].Version != pm[0].Version {
		t.Fatalf("the second branch dumped again: %+v", pm2)
	}
	h.same(parentRef, b2.Ref)
	// The run log says it, and the parent's event log records it.
	if evs, _ := h.reg.ListEvents(context.Background(), parentRef, 50); !hasEvent(evs, "branch.baseline_recorded") {
		t.Errorf("no branch.baseline_recorded event on the parent")
	}
	// The branch has a base: nothing to merge yet.
	if d, err := h.svc.Diff(context.Background(), b.Ref); err != nil || d != "" {
		t.Fatalf("diff of a fresh branch = %q, %v", d, err)
	}
}

func hasEvent(evs []registry.Event, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestNoBaselineWhenTheParentHasAHistory(t *testing.T) {
	h := newPGHarness(t)
	h.apply(parentRef, mig("20260101000000", "init", "create table public.t (id int primary key)"))
	h.exec(parentRef, "create table public.untracked (id int)") // not in the history: not in the branch
	b := h.create("feat", nil)
	if pm := h.migrations(parentRef); len(pm) != 1 || pm[0].Name != "init" {
		t.Fatalf("parent history = %+v", pm)
	}
	if !h.has(b.Ref, "t") || h.has(b.Ref, "untracked") {
		t.Fatalf("the branch has t=%v untracked=%v, want true false", h.has(b.Ref, "t"), h.has(b.Ref, "untracked"))
	}
	// The parent's untracked table is drift: the diff of the branch does not list it, as a DROP or otherwise.
	if d, _ := h.svc.Diff(context.Background(), b.Ref); d != "" {
		t.Fatalf("diff of a fresh branch = %q", d)
	}
}

func TestResetReplaysTheBranchsOwnHistory(t *testing.T) {
	h := newPGHarness(t)
	h.apply(parentRef, mig("20260101000000", "init", "create table public.t (id int primary key)"))
	b := h.create("feat", nil)
	h.apply(b.Ref, mig("20260102000000", "tracked", "create table public.tracked_only (id int)"))
	h.exec(b.Ref, "create table public.scratch (id int); insert into public.t values (1)")

	// An unknown version is refused before anything is removed.
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: "29990101000000"}); !errorsIs(err, ErrInvalid) {
		t.Fatalf("reset to an unknown version: %v", err)
	}
	if !h.has(b.Ref, "scratch") {
		t.Fatal("the refused reset removed the branch")
	}
	// Reset to the latest keeps the tracked migration, drops the untracked table and the rows.
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchFunctionsDeployed)
	if !h.has(b.Ref, "tracked_only") || !h.has(b.Ref, "t") || h.has(b.Ref, "scratch") {
		t.Fatalf("after reset: tracked_only=%v t=%v scratch=%v", h.has(b.Ref, "tracked_only"), h.has(b.Ref, "t"), h.has(b.Ref, "scratch"))
	}
	// A branch-only version is accepted: back to the first migration, then to nothing.
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: "20260101000000"}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchFunctionsDeployed)
	if h.has(b.Ref, "tracked_only") || !h.has(b.Ref, "t") {
		t.Fatalf("reset to the first version: tracked_only=%v t=%v", h.has(b.Ref, "tracked_only"), h.has(b.Ref, "t"))
	}
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: "0"}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchFunctionsDeployed)
	if h.has(b.Ref, "t") || len(h.migrations(b.Ref)) != 0 {
		t.Fatalf("reset to 0: t=%v migrations=%d", h.has(b.Ref, "t"), len(h.migrations(b.Ref)))
	}
}

// The dashboard flow records the branch's diff and merges it. A branch with a tracked migration and
// an untracked table must reach the parent once: no statement runs twice, the histories converge, and
// repeating the record and the merge changes nothing.
func TestDashboardMergeNeverAppliesTwice(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	h.apply(b.Ref, mig("20260102000000", "tracked", "create table public.tracked (id int primary key)"))
	h.exec(b.Ref, "create table public.untracked (id int primary key, tracked_id int references public.tracked (id)); alter table public.untracked enable row level security")
	// production moved on meanwhile, outside migrations: the merge must not undo it
	h.exec(parentRef, "create table public.parent_only (id int)")

	diff, err := h.svc.Diff(ctx, b.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, `"public"."tracked"`) || !strings.Contains(diff, `"public"."untracked"`) || strings.Contains(diff, "parent_only") || strings.Contains(diff, "DROP") {
		t.Fatalf("diff:\n%s", diff)
	}
	rec := Migration{Version: "20260103000000", Name: "branch_merge_a", Statements: []string{diff}}
	record := func() {
		t.Helper()
		if _, err := h.svc.schemaDB().Record(ctx, b.Ref, []Migration{rec}); err != nil {
			t.Fatal(err)
		}
	}
	record()
	record() // PUT again: recorded once
	if n := len(h.migrations(b.Ref)); n != 3 {
		t.Fatalf("branch history has %d rows, want 3", n)
	}

	got := h.merge(b.Ref, ActionInput{})
	h.mustState(got, registry.BranchFunctionsDeployed)
	if !strings.Contains(got.Detail, "recorded schema diff") {
		t.Fatalf("detail = %q", got.Detail)
	}
	for _, tbl := range []string{"base_t", "tracked", "untracked", "parent_only"} {
		if !h.has(parentRef, tbl) {
			t.Errorf("the parent lacks %s after the merge", tbl)
		}
	}
	// Both histories agree: init, tracked, branch_merge_a.
	if pm := h.migrations(parentRef); len(pm) != 3 {
		t.Fatalf("parent history = %+v", pm)
	}
	h.histories(parentRef, b.Ref)
	// The branch's diff is empty now, and the parent's drift is still drift.
	if d, _ := h.svc.Diff(ctx, b.Ref); d != "" {
		t.Fatalf("diff after the merge:\n%s", d)
	}

	// The tracked migration is in both histories as covered by the diff, which holds its changes: the
	// parent's history replays to the same schema, for a new branch and for a reset of this one.
	for _, ref := range []string{parentRef, b.Ref} {
		for _, m := range h.migrations(ref) {
			if m.Name == "tracked" && m.Covered != rec.Version {
				t.Fatalf("%s: the tracked migration is not marked as covered by the diff: %+v", ref, m)
			}
			if m.Name == "branch_merge_a" && m.Covered != "" {
				t.Fatalf("%s: the diff itself is marked as covered", ref)
			}
		}
	}
	fresh := h.create("replayed", nil)
	h.mustState(fresh, registry.BranchFunctionsDeployed)
	for _, tbl := range []string{"base_t", "tracked", "untracked"} {
		if !h.has(fresh.Ref, tbl) {
			t.Fatalf("a branch made from the merged parent lacks %s", tbl)
		}
	}
	h.histories(parentRef, fresh.Ref)
	if _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchFunctionsDeployed)
	if !h.has(b.Ref, "tracked") || !h.has(b.Ref, "untracked") {
		t.Fatal("a reset of the merged branch lost a table")
	}
	h.histories(parentRef, b.Ref)

	// Again: the same record, the same merge. Nothing runs, nothing breaks.
	record()
	again := h.merge(b.Ref, ActionInput{})
	h.mustState(again, registry.BranchFunctionsDeployed)
	if !strings.Contains(again.Detail, "nothing to merge") {
		t.Fatalf("second merge detail = %q", again.Detail)
	}
	if n := len(h.migrations(parentRef)); n != 3 {
		t.Fatalf("a second merge changed the parent's history to %d rows", n)
	}
}

func TestDashboardMergeFailsWhenTheBranchMovedSinceTheDiffWasRecorded(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	h.exec(b.Ref, "create table public.one (id int)")
	diff, _ := h.svc.Diff(ctx, b.Ref)
	if _, err := h.svc.schemaDB().Record(ctx, b.Ref, []Migration{{Version: "20260103000000", Name: "branch_merge_a", Statements: []string{diff}}}); err != nil {
		t.Fatal(err)
	}
	h.exec(b.Ref, "create table public.two (id int)") // after the diff was recorded
	got := h.merge(b.Ref, ActionInput{})
	h.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "changed since the diff was recorded; refresh") {
		t.Fatalf("detail = %q", got.Detail)
	}
	if h.has(parentRef, "one") {
		t.Fatal("the failed merge ran statements on the parent")
	}
}

func TestHistoryMergeStillWorksAndAdvancesTheBase(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	h.apply(b.Ref, mig("20260102000000", "tracked", "create table public.tracked (id int primary key)"))
	h.exec(b.Ref, "create table public.untracked (id int)")
	got := h.merge(b.Ref, ActionInput{})
	h.mustState(got, registry.BranchFunctionsDeployed)
	if !h.has(parentRef, "tracked") || h.has(parentRef, "untracked") {
		t.Fatalf("history merge: tracked=%v untracked=%v on the parent", h.has(parentRef, "tracked"), h.has(parentRef, "untracked"))
	}
	// What was merged is no longer a change of the branch; the untracked table still is.
	d, err := h.svc.Diff(ctx, b.Ref)
	if err != nil || !strings.Contains(d, `"public"."untracked"`) || strings.Contains(d, `"public"."tracked"`) {
		t.Fatalf("diff after a history merge:\n%s (%v)", d, err)
	}
}

func TestPushTakesInTheParentsUntrackedChanges(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	h.exec(b.Ref, "create table public.branch_only (id int)") // the branch's own change
	h.apply(parentRef, mig("20260102000000", "later", "create table public.later (id int)"))
	h.exec(parentRef, "create table public.p_untracked (id int primary key); alter table public.base_t add column extra text")

	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchFunctionsDeployed)
	for _, tbl := range []string{"later", "p_untracked", "branch_only"} {
		if !h.has(b.Ref, tbl) {
			t.Errorf("the branch lacks %s after the push (%s)", tbl, got.Detail)
		}
	}
	if !strings.Contains(got.Detail, "applied 1 migration") || !strings.Contains(got.Detail, "schema changes that were not migrations") {
		t.Fatalf("detail = %q", got.Detail)
	}
	// What the branch holds of the parent is not a change of the branch; its own table is.
	d, _ := h.svc.Diff(ctx, b.Ref)
	if !strings.Contains(d, "branch_only") || strings.Contains(d, "p_untracked") || strings.Contains(d, "later") || strings.Contains(d, "extra") {
		t.Fatalf("diff after the push:\n%s", d)
	}
	// A merge of the branch does not run the push's own migration on the parent.
	rec := Migration{Version: "20260105000000", Name: "branch_merge_x", Statements: []string{d}}
	if _, err := h.svc.schemaDB().Record(ctx, b.Ref, []Migration{rec}); err != nil {
		t.Fatal(err)
	}
	m := h.merge(b.Ref, ActionInput{})
	h.mustState(m, registry.BranchFunctionsDeployed)
	if !h.has(parentRef, "branch_only") {
		t.Fatalf("the parent lacks branch_only: %s", m.Detail)
	}
	for _, mg := range h.migrations(parentRef) {
		if strings.HasPrefix(mg.Name, pushMigrationPrefix) {
			t.Fatalf("the parent's history holds the branch's push migration %s", mg.Name)
		}
	}
	// A second push has nothing to bring.
	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	if got = h.wait(b.Ref); !strings.Contains(got.Detail, "nothing to push") {
		t.Fatalf("second push detail = %q", got.Detail)
	}
}

func TestPushRefusesAChangeBothSidesMadeUnlessForced(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key, v text)"))
	b := h.create("feat", nil)
	h.exec(b.Ref, "alter table public.base_t alter column v type varchar(10)")
	h.exec(parentRef, "alter table public.base_t alter column v type varchar(20)")
	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "both changed") {
		t.Fatalf("detail = %q", got.Detail)
	}
	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{Force: true}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchFunctionsDeployed)
	h.same(parentRef, b.Ref)
}

func TestDiffOfABranchWithoutABaseListsAdditionsOnly(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	// A branch made by v0.3.x has no base.
	if err := registry.BranchContentOf(h.reg).PutBranchSchemaBase(ctx, registry.BranchSchemaBase{BranchRef: b.Ref, Snapshot: []byte("not a snapshot")}); err != nil {
		t.Fatal(err)
	}
	h.exec(b.Ref, "create table public.mine (id int)")
	h.exec(parentRef, "create table public.theirs (id int)")
	d, err := h.svc.Diff(ctx, b.Ref)
	if err != nil || !strings.Contains(d, "mine") || strings.Contains(d, "theirs") || strings.Contains(d, "DROP") {
		t.Fatalf("diff without a base:\n%s (%v)", d, err)
	}
}

func TestDiffHonorsIncludedSchemas(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create schema other; create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	h.exec(b.Ref, "create table public.in_public (id int); create table other.in_other (id int)")
	all, _ := h.svc.Diff(ctx, b.Ref)
	only, err := h.svc.DiffWith(ctx, b.Ref, DiffOptions{IncludedSchemas: "other", PGDelta: true})
	if err != nil || !strings.Contains(all, "in_public") || !strings.Contains(all, "in_other") || strings.Contains(only, "in_public") || !strings.Contains(only, "in_other") {
		t.Fatalf("all:\n%s\nonly other:\n%s (%v)", all, only, err)
	}
}

func errorsIs(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

// A diff recorded and not merged yet holds the branch's tracked migrations too: a reset must not
// create their tables twice.
func TestResetWithARecordedDiffDoesNotReplayTheMigrationsItHolds(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	h.apply(b.Ref, mig("20260102000000", "tracked", "create table public.tracked (id int primary key)"))
	h.exec(b.Ref, "create table public.untracked (id int primary key)")
	diff, err := h.svc.Diff(ctx, b.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.schemaDB().Record(ctx, b.Ref, []Migration{{Version: "20260103000000", Name: "branch_merge_a", Statements: []string{diff}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchFunctionsDeployed)
	if !h.has(b.Ref, "tracked") || !h.has(b.Ref, "untracked") || !h.has(b.Ref, "base_t") {
		t.Fatalf("after the reset: tracked=%v untracked=%v base_t=%v (%s)", h.has(b.Ref, "tracked"), h.has(b.Ref, "untracked"), h.has(b.Ref, "base_t"), got.Detail)
	}
	// And the merge afterwards still works, once.
	m := h.merge(b.Ref, ActionInput{})
	h.mustState(m, registry.BranchFunctionsDeployed)
	if !h.has(parentRef, "tracked") || !h.has(parentRef, "untracked") {
		t.Fatalf("the merge after the reset: %s", m.Detail)
	}
}

// Push takes in the parent's history, covered rows included, without running them.
func TestPushRecordsCoveredMigrationsWithoutRunningThem(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	old := h.create("old", nil) // made before the merge below
	b := h.create("feat", nil)
	h.apply(b.Ref, mig("20260102000000", "tracked", "create table public.tracked (id int primary key)"))
	diff, _ := h.svc.Diff(ctx, b.Ref)
	if _, err := h.svc.schemaDB().Record(ctx, b.Ref, []Migration{{Version: "20260103000000", Name: "branch_merge_a", Statements: []string{diff}}}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.merge(b.Ref, ActionInput{}), registry.BranchFunctionsDeployed)
	if _, err := h.svc.Push(ctx, old.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(old.Ref)
	h.mustState(got, registry.BranchFunctionsDeployed)
	if !h.has(old.Ref, "tracked") {
		t.Fatalf("the push did not bring the merged table: %s", got.Detail)
	}
	h.histories(parentRef, old.Ref)
}

// scalar returns the first column of the first row of a query on ref, as text.
func (h *pgHarness) scalar(ref, sql string) string {
	h.t.Helper()
	h.mu.Lock()
	name := h.dbs[ref]
	h.mu.Unlock()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, h.dsn(name))
	if err != nil {
		h.t.Fatal(err)
	}
	defer c.Close(ctx)
	var out string
	if err := c.QueryRow(ctx, "select ("+sql+")::text").Scan(&out); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// A schema diff holds the DDL of a branch's migrations and not their data. The dashboard merge records
// the migrations as covered by the diff, and must still run the rest: a lookup table's rows, a bucket.
func TestDashboardMergeRunsWhatTheDiffDoesNotHold(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	h.apply(b.Ref, mig("20260102000000", "lookup",
		"create table public.lookup (id int primary key, name text)",
		"insert into public.lookup values (1, 'one'), (2, 'two')",
		"insert into storage.buckets (id, name) values ('avatars', 'avatars')"))
	diff, err := h.svc.Diff(ctx, b.Ref)
	if err != nil || strings.Contains(diff, "insert") || strings.Contains(diff, "avatars") {
		t.Fatalf("diff = %q, %v", diff, err)
	}
	rec := Migration{Version: "20260103000000", Name: "branch_merge_a", Statements: []string{diff}}
	if _, err := h.svc.schemaDB().Record(ctx, b.Ref, []Migration{rec}); err != nil {
		t.Fatal(err)
	}

	got := h.merge(b.Ref, ActionInput{})
	h.mustState(got, registry.BranchFunctionsDeployed)
	if n := h.scalar(parentRef, "select count(*) from public.lookup"); n != "2" {
		t.Fatalf("the parent has %s rows of the lookup table, want 2 (%s)", n, got.Detail)
	}
	if n := h.scalar(parentRef, "select count(*) from storage.buckets where id = 'avatars'"); n != "1" {
		t.Fatalf("the parent has %s avatars bucket(s), want 1", n)
	}
	if !strings.Contains(got.Detail, "ran 2 statement(s)") {
		t.Fatalf("detail does not say what ran after the diff: %q", got.Detail)
	}
	h.histories(parentRef, b.Ref)

	// What the parent's history replays to: the diff first, then the covered migration's data.
	fresh := h.create("replayed", nil)
	h.mustState(fresh, registry.BranchFunctionsDeployed)
	if n := h.scalar(fresh.Ref, "select count(*) from public.lookup"); n != "2" {
		t.Fatalf("a branch made from the merged parent has %s rows, want 2", n)
	}
	if n := h.scalar(fresh.Ref, "select count(*) from storage.buckets where id = 'avatars'"); n != "1" {
		t.Fatalf("a branch made from the merged parent has %s avatars bucket(s), want 1", n)
	}
	// And so does a reset of the merged branch.
	if _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchFunctionsDeployed)
	if n := h.scalar(b.Ref, "select count(*) from public.lookup"); n != "2" {
		t.Fatalf("the reset branch has %s rows, want 2", n)
	}
}

// A recording the branch moved past does not decide a merge, and does not block one.
func TestStaleRecordedDiffDoesNotBlockAHistoryMerge(t *testing.T) {
	h := newPGHarness(t)
	ctx := context.Background()
	h.apply(parentRef, mig("20260101000000", "init", "create table public.base_t (id int primary key)"))
	b := h.create("feat", nil)
	h.apply(b.Ref, mig("20260102000000", "one", "create table public.one (id int primary key)"))
	h.exec(b.Ref, "create table public.sql_editor (id int)")
	diff, _ := h.svc.Diff(ctx, b.Ref)
	if _, err := h.svc.schemaDB().Record(ctx, b.Ref, []Migration{{Version: "20260103000000", Name: "branch_merge_a", Statements: []string{diff}}}); err != nil {
		t.Fatal(err)
	}
	// The user does not finish the merge in Studio; an agent adds a migration and merges.
	h.apply(b.Ref, mig("20260104000000", "two", "create table public.two (id int primary key)"))
	got := h.merge(b.Ref, ActionInput{Force: true})
	h.mustState(got, registry.BranchFunctionsDeployed)
	if !h.has(parentRef, "one") || !h.has(parentRef, "two") || h.has(parentRef, "sql_editor") {
		t.Fatalf("the parent has one=%v two=%v sql_editor=%v (%s)", h.has(parentRef, "one"), h.has(parentRef, "two"), h.has(parentRef, "sql_editor"), got.Detail)
	}
	for _, m := range h.migrations(parentRef) {
		if m.Name == "branch_merge_a" {
			t.Fatal("the stale recording reached the parent's history")
		}
	}
	// The change made outside migrations is still the branch's, and a new recording merges it.
	diff, _ = h.svc.Diff(ctx, b.Ref)
	if !strings.Contains(diff, "sql_editor") || strings.Contains(diff, `"public"."one"`) {
		t.Fatalf("diff after the merge:\n%s", diff)
	}
	if _, err := h.svc.schemaDB().Record(ctx, b.Ref, []Migration{{Version: "20260105000000", Name: "branch_merge_b", Statements: []string{diff}}}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.merge(b.Ref, ActionInput{}), registry.BranchFunctionsDeployed)
	if !h.has(parentRef, "sql_editor") {
		t.Fatal("the new recording did not merge the change made outside migrations")
	}
	h.histories(parentRef, b.Ref)
}

// A baseline that does not run on the branch must not be left in the parent's history, where every later
// branch would replay it.
func TestBaselineThatCannotRunLeavesTheParentsHistoryEmpty(t *testing.T) {
	h := newPGHarness(t)
	h.exec(parentRef, userSchema)
	contaminate := true
	h.eng.onCreate = func(req lifecycle.CreateRequest) {
		if req.Ref == parentRef {
			return
		}
		h.makeDB(req.Ref)
		if contaminate {
			h.exec(req.Ref, "create table public.todos (id int)") // the baseline's CREATE TABLE fails
		}
	}
	b := h.create("feat", nil)
	h.mustState(b, registry.BranchMigrationsFailed)
	if pm := h.migrations(parentRef); len(pm) != 0 {
		t.Fatalf("the parent's history after a baseline that failed: %+v (%s)", pm, b.Detail)
	}
	contaminate = false
	b2 := h.create("feat2", nil)
	h.mustState(b2, registry.BranchFunctionsDeployed)
	if pm := h.migrations(parentRef); len(pm) != 1 || pm[0].Name != baselineName {
		t.Fatalf("parent history = %+v", pm)
	}
	h.same(parentRef, b2.Ref)
}

// The event triggers and the publication of a new project are not a schema of the user's: no baseline.
func TestStockEventTriggersAndPublicationsAreNotAUserSchema(t *testing.T) {
	h := newPGHarness(t)
	h.exec(parentRef, `
		create function extensions.noop() returns event_trigger language plpgsql as $$ begin end $$;
		create event trigger pgrst_ddl_watch on ddl_command_end execute function extensions.noop();
		create event trigger issue_pg_net_access on ddl_command_end execute function extensions.noop();
		create publication supabase_realtime;`)
	if un := h.snapshot(parentRef).Unmodeled; len(un) != 0 {
		t.Fatalf("stock objects are listed as unmodeled: %v", un)
	}
	b := h.create("feat", nil)
	h.mustState(b, registry.BranchFunctionsDeployed)
	if pm := h.migrations(parentRef); len(pm) != 0 {
		t.Fatalf("a baseline was recorded for a project with nothing of its own: %+v", pm)
	}
	// An event trigger of the user's own is named, as before.
	h.exec(parentRef, `create event trigger audit_ddl on ddl_command_end execute function extensions.noop()`)
	if un := h.snapshot(parentRef).Unmodeled; len(un) != 1 || un[0] != "event trigger audit_ddl" {
		t.Fatalf("unmodeled = %v", un)
	}
}

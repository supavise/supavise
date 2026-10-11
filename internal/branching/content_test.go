package branching

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// ---- fakes ---------------------------------------------------------------------------------

// fakeSchemaDB is the fake database with a schema to read per project and a trace of what the
// operations asked of it, in order.
type fakeSchemaDB struct {
	*fakeDB
	tm    sync.Mutex
	snaps map[string]*schemadiff.Snapshot
	roles map[string]bool
	trace []string
	// initCalls counts InitHistory calls that recorded.
	initCalls int
}

func newFakeSchemaDB(db *fakeDB) *fakeSchemaDB {
	return &fakeSchemaDB{fakeDB: db, snaps: map[string]*schemadiff.Snapshot{}, roles: map[string]bool{"anon": true, "authenticated": true, "service_role": true}}
}

func (d *fakeSchemaDB) note(format string, a ...any) {
	d.tm.Lock()
	d.trace = append(d.trace, fmt.Sprintf(format, a...))
	d.tm.Unlock()
}

func (d *fakeSchemaDB) Apply(ctx context.Context, ref string, ms []Migration, o ApplyOptions) (ApplyResult, error) {
	res, err := d.fakeDB.Apply(ctx, ref, ms, o)
	if err == nil {
		d.note("apply %s %s", ref, versions(ms))
	}
	return res, err
}

func (d *fakeSchemaDB) Snapshot(_ context.Context, ref string, _ schemadiff.ReadOptions) (*schemadiff.Snapshot, error) {
	d.note("snapshot %s", ref)
	d.tm.Lock()
	defer d.tm.Unlock()
	if s := d.snaps[ref]; s != nil {
		c := *s
		c.Objects = append([]schemadiff.Object(nil), s.Objects...)
		return &c, nil
	}
	return &schemadiff.Snapshot{Version: schemadiff.FormatVersion}, nil
}

func (d *fakeSchemaDB) Roles(context.Context, string) (map[string]bool, error) { return d.roles, nil }

func (d *fakeSchemaDB) Record(_ context.Context, ref string, ms []Migration) ([]string, error) {
	d.fakeDB.mu.Lock()
	defer d.fakeDB.mu.Unlock()
	var done []string
	for _, m := range ms {
		have := false
		for _, x := range d.migs[ref] {
			have = have || x.Version == m.Version
		}
		if !have {
			d.migs[ref] = append(d.migs[ref], m)
			done = append(done, m.Version)
		}
	}
	d.note("record %s %s", ref, strings.Join(done, ","))
	return done, nil
}

func (d *fakeSchemaDB) InitHistory(_ context.Context, ref string, m Migration) (bool, error) {
	d.fakeDB.mu.Lock()
	defer d.fakeDB.mu.Unlock()
	if len(d.migs[ref]) > 0 {
		return false, nil
	}
	d.migs[ref] = append(d.migs[ref], m)
	d.initCalls++
	d.note("init %s %s", ref, m.Version)
	return true, nil
}

// table is the snapshot objects of a table with the given columns.
func table(schema, name string, cols ...string) []schemadiff.Object {
	tk := "table|" + schema + "|" + name
	objs := []schemadiff.Object{{Kind: schemadiff.KindTable, Key: tk, Name: fmt.Sprintf("%q.%q", schema, name), Attr: map[string]string{"rls": "false", "force": "false"}}}
	for i, c := range cols {
		objs = append(objs, schemadiff.Object{Kind: schemadiff.KindColumn, Key: tk[len("table"):] + "|" + c, Name: fmt.Sprintf("%q", c), Parent: tk,
			Attr: map[string]string{"type": "integer", "notnull": "false", "pos": fmt.Sprint(i + 1)}})
		objs[len(objs)-1].Key = "column" + objs[len(objs)-1].Key
	}
	return objs
}

func snap(objs ...[]schemadiff.Object) *schemadiff.Snapshot {
	s := &schemadiff.Snapshot{Version: schemadiff.FormatVersion, Schemas: []string{"public"}}
	for _, o := range objs {
		s.Objects = append(s.Objects, o...)
	}
	s.Sort()
	return s
}

type fakeFn struct {
	id      string
	version int
	updated time.Time
	digest  string
}

// fakeCarrier is the Edge Function store of the fake node.
type fakeCarrier struct {
	mu      sync.Mutex
	fns     map[string]map[string]fakeFn
	secrets map[string]map[string]string
	applied []string
	next    int
}

func newFakeCarrier() *fakeCarrier {
	return &fakeCarrier{fns: map[string]map[string]fakeFn{}, secrets: map[string]map[string]string{}}
}

func (c *fakeCarrier) put(ref, slug, digest string, version int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fns[ref] == nil {
		c.fns[ref] = map[string]fakeFn{}
	}
	c.next++
	c.fns[ref][slug] = fakeFn{id: fmt.Sprintf("id-%d", c.next), version: version, updated: time.Unix(1_700_000_000+int64(c.next), 0), digest: digest}
}

func (c *fakeCarrier) Digests(_ context.Context, ref string) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for s, f := range c.fns[ref] {
		out[s] = f.digest
	}
	return out, nil
}

func (c *fakeCarrier) Copy(_ context.Context, from, to string, slugs []string) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	want := map[string]bool{}
	for _, s := range slugs {
		want[s] = true
	}
	out := map[string]string{}
	for slug, f := range c.fns[from] {
		if slugs != nil && !want[slug] {
			continue
		}
		if c.fns[to] == nil {
			c.fns[to] = map[string]fakeFn{}
		}
		if cur, ok := c.fns[to][slug]; ok {
			f.id, f.version = cur.id, cur.version+1
		} else {
			c.next++
			f.id = fmt.Sprintf("id-%d", c.next)
		}
		c.fns[to][slug] = f
		out[slug] = f.digest
	}
	return out, nil
}

func (c *fakeCarrier) PutSecrets(_ context.Context, ref string, s map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets[ref] == nil {
		c.secrets[ref] = map[string]string{}
	}
	for k, v := range s {
		c.secrets[ref][k] = v
	}
	return nil
}

func (c *fakeCarrier) Applied(_ context.Context, ref string) error {
	c.mu.Lock()
	c.applied = append(c.applied, ref)
	c.mu.Unlock()
	return nil
}

func (c *fakeCarrier) slugs(ref string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for s := range c.fns[ref] {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// contentHarness is the harness with the schema-reading database and the function carrier.
type contentHarness struct {
	*harness
	sdb *fakeSchemaDB
	fns *fakeCarrier
}

func newContentHarness(t *testing.T, mut func(*contentHarness)) *contentHarness {
	t.Helper()
	h := newHarness(t, nil)
	c := &contentHarness{harness: h, sdb: newFakeSchemaDB(h.db), fns: newFakeCarrier()}
	h.svc.db = c.sdb
	h.svc.fns = c.fns
	h.eng.onCreate = func(req lifecycle.CreateRequest) { c.sdb.note("engine.create %s", req.Ref) }
	if mut != nil {
		mut(c)
	}
	return c
}

// ---- baseline ------------------------------------------------------------------------------

func TestBaselineIsTakenOnlyForAnEmptyHistoryAndRecordedOnce(t *testing.T) {
	c := newContentHarness(t, func(c *contentHarness) {
		c.db.migs[parentRef] = nil // everything was built in the SQL editor
		c.sdb.snaps[parentRef] = snap(table("public", "todos", "id", "title"))
	})
	b := c.create("one", nil)
	c.mustState(b, registry.BranchFunctionsDeployed)
	pm := c.db.migs[parentRef]
	if len(pm) != 1 || pm[0].Name != baselineName || c.sdb.initCalls != 1 {
		t.Fatalf("parent history = %+v, init calls %d", pm, c.sdb.initCalls)
	}
	if !strings.Contains(strings.Join(pm[0].Statements, "\n"), `CREATE TABLE "public"."todos"`) {
		t.Fatalf("baseline statements = %q", pm[0].Statements)
	}
	// The branch ran the same migration, and the engine's create (the Data API choice) came first.
	bm := c.db.migs[b.Ref]
	if len(bm) != 1 || bm[0].Version != pm[0].Version || !reflect.DeepEqual(bm[0].Statements, pm[0].Statements) {
		t.Fatalf("branch history = %+v", bm)
	}
	var order []string
	for _, e := range c.sdb.trace {
		if strings.HasPrefix(e, "engine.create") || strings.HasPrefix(e, "init") || strings.HasPrefix(e, "apply "+b.Ref) {
			order = append(order, strings.Fields(e)[0])
		}
	}
	if want := []string{"engine.create", "init", "apply"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v (the Data API choice, the history, the baseline)", order, want)
	}
	// A second branch replays what is recorded: nothing is dumped or recorded again.
	b2 := c.create("two", nil)
	if c.sdb.initCalls != 1 || len(c.db.migs[parentRef]) != 1 || len(c.db.migs[b2.Ref]) != 1 {
		t.Fatalf("second branch: init calls %d, parent %d, branch %d", c.sdb.initCalls, len(c.db.migs[parentRef]), len(c.db.migs[b2.Ref]))
	}
}

func TestNoBaselineWhenThereIsAHistoryOrNothingToDump(t *testing.T) {
	// A history: replayed, nothing dumped.
	c := newContentHarness(t, func(c *contentHarness) { c.sdb.snaps[parentRef] = snap(table("public", "todos", "id")) })
	b := c.create("one", nil)
	if c.sdb.initCalls != 0 || len(c.db.migs[b.Ref]) != 3 {
		t.Fatalf("with a history: init calls %d, replayed %d", c.sdb.initCalls, len(c.db.migs[b.Ref]))
	}
	// An empty history and an empty schema: the branch starts empty and the parent's history stays empty.
	c = newContentHarness(t, func(c *contentHarness) { c.db.migs[parentRef] = nil })
	b = c.create("two", nil)
	c.mustState(b, registry.BranchFunctionsDeployed)
	if c.sdb.initCalls != 0 || len(c.db.migs[parentRef]) != 0 || len(c.db.migs[b.Ref]) != 0 {
		t.Fatalf("nothing to dump: init calls %d, parent %d, branch %d", c.sdb.initCalls, len(c.db.migs[parentRef]), len(c.db.migs[b.Ref]))
	}
}

func TestBaselineLeavesOutCustomRolesAndSaysSo(t *testing.T) {
	c := newContentHarness(t, func(c *contentHarness) {
		c.db.migs[parentRef] = nil
		s := snap(table("public", "todos", "id"))
		s.Objects = append(s.Objects, schemadiff.Object{Kind: schemadiff.KindACL, Key: "acl|table|public|todos|reporting", Name: "reporting", Parent: "table|public|todos",
			List: []string{"SELECT"}, Attr: map[string]string{"objtype": "TABLE", "target": `"public"."todos"`}})
		c.sdb.snaps[parentRef] = s
	})
	b := c.create("one", nil)
	c.mustState(b, registry.BranchFunctionsDeployed)
	if strings.Contains(strings.Join(c.db.migs[b.Ref][0].Statements, "\n"), "reporting") {
		t.Fatalf("a grant to a role the branch lacks is in the baseline: %q", c.db.migs[b.Ref][0].Statements)
	}
	run := c.runLog(b.Ref)
	if !strings.Contains(run, "reporting") || !strings.Contains(run, "custom roles are not copied") {
		t.Fatalf("run log does not say what was left out:\n%s", run)
	}
}

// runLog returns the whole log of the newest run of ref.
func (h *harness) runLog(ref string) string {
	h.t.Helper()
	run, err := h.svc.LatestRun(context.Background(), ref)
	if err != nil || run == nil {
		h.t.Fatalf("latest run of %s: %v", ref, err)
	}
	full, err := registry.BranchRunsOf(h.reg).GetBranchRun(context.Background(), run.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	var b strings.Builder
	for _, st := range full.Steps {
		b.WriteString(st.Log)
	}
	return b.String()
}

// ---- diff ----------------------------------------------------------------------------------

func TestDiffIsEmptyWhenNothingChangedAndSaysWhatChangedOtherwise(t *testing.T) {
	c := newContentHarness(t, func(c *contentHarness) {
		c.sdb.snaps[parentRef] = snap(table("public", "todos", "id"))
	})
	b := c.create("one", nil)
	// The branch is what it started as: its base is the parent's schema at creation.
	c.sdb.snaps[b.Ref] = snap(table("public", "todos", "id"))
	// (the base was taken from the branch when it was created: empty then)
	c.setBase(b.Ref, snap(table("public", "todos", "id")))
	if out, err := c.svc.Diff(context.Background(), b.Ref); err != nil || out != "" {
		t.Fatalf("diff of an unchanged branch = %q, %v", out, err)
	}
	c.sdb.snaps[b.Ref] = snap(table("public", "todos", "id", "title"), table("public", "notes", "id"))
	out, err := c.svc.Diff(context.Background(), b.Ref)
	if err != nil || !strings.Contains(out, `ADD COLUMN "title"`) || !strings.Contains(out, `CREATE TABLE "public"."notes"`) || strings.Contains(out, "DROP") {
		t.Fatalf("diff = %q, %v", out, err)
	}
	// Both values of pgdelta give the same answer.
	for _, pg := range []bool{false, true} {
		got, err := c.svc.DiffWith(context.Background(), b.Ref, DiffOptions{PGDelta: pg})
		if err != nil || got != out {
			t.Fatalf("pgdelta=%v: %q, %v", pg, got, err)
		}
	}
}

func (c *contentHarness) setBase(ref string, s *schemadiff.Snapshot) {
	c.t.Helper()
	if err := c.svc.putBase(context.Background(), ref, s, "create"); err != nil {
		c.t.Fatal(err)
	}
}

func TestDiffNamesWhatItCannotExpress(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	c.setBase(b.Ref, snap())
	s := snap()
	s.Unmodeled = []string{"aggregate public.agg(integer)"}
	c.sdb.snaps[b.Ref] = s
	out, _ := c.svc.Diff(context.Background(), b.Ref)
	if !schemadiff.HasUnsupported(out) || !strings.Contains(out, "aggregate public.agg") {
		t.Fatalf("diff = %q", out)
	}
}

// ---- reset ---------------------------------------------------------------------------------

func TestResetReplaysTheBranchsOwnHistoryAndChecksTheVersionFirst(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], mig("20260110000000", "branch_only", "create table b (id int)"))
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.harness.db.forget(ref) } // the cluster is gone

	// An unknown version is refused before anything is removed.
	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: "29990101000000"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reset to an unknown version: %v", err)
	}
	if len(c.eng.deletes) != 0 {
		t.Fatalf("a refused reset removed %v", c.eng.deletes)
	}
	// A version only the branch has is accepted, and the branch's own migration survives the replay.
	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: "20260110000000"}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	got := c.db.migs[b.Ref]
	if len(got) != 4 || got[3].Name != "branch_only" {
		t.Fatalf("history after the reset = %+v", got)
	}
	// 0 is an empty database: nothing replays and the seed does not run.
	if err := c.svc.SetSeed(context.Background(), parentRef, "insert into t values (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: "0"}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	if len(c.db.migs[b.Ref]) != 0 || len(c.db.scripts[b.Ref]) != 0 {
		t.Fatalf("reset to 0: history %v, scripts %v", c.db.migs[b.Ref], c.db.scripts[b.Ref])
	}
}

// forget clears what a removed cluster held.
func (d *fakeDB) forget(ref string) {
	d.mu.Lock()
	delete(d.migs, ref)
	delete(d.scripts, ref)
	d.mu.Unlock()
}

func TestResetKeepsFunctionsAndSettings(t *testing.T) {
	c := newContentHarness(t, nil)
	c.fns.put(parentRef, "hello", "d1", 1)
	b := c.create("one", nil)
	if got := c.fns.slugs(b.Ref); !reflect.DeepEqual(got, []string{"hello"}) {
		t.Fatalf("branch functions = %v", got)
	}
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.harness.db.forget(ref) }
	before := len(c.fns.applied)
	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	if got := c.fns.slugs(b.Ref); !reflect.DeepEqual(got, []string{"hello"}) || len(c.fns.applied) != before {
		t.Fatalf("a reset touched the branch's functions: %v, applied %d -> %d", got, before, len(c.fns.applied))
	}
	// The recreate asks for no settings: the branch keeps its own.
	last := c.eng.creates[len(c.eng.creates)-1]
	if !last.Recreate || last.SettingsFrom != "" {
		t.Fatalf("recreate request = %+v", last)
	}
}

// ---- Edge Functions and secrets --------------------------------------------------------------

func TestCreateCopiesTheParentsFunctionsButNotItsSecrets(t *testing.T) {
	c := newContentHarness(t, nil)
	c.fns.put(parentRef, "hello", "d-hello", 3)
	c.fns.put(parentRef, "world", "d-world", 7)
	c.fns.secrets[parentRef] = map[string]string{"STRIPE_KEY": "sk_live"}
	b := c.create("one", func(in *CreateInput) { in.Secrets = map[string]string{"MY_SECRET": "x"} })
	c.mustState(b, registry.BranchFunctionsDeployed)

	if got := c.fns.slugs(b.Ref); !reflect.DeepEqual(got, []string{"hello", "world"}) {
		t.Fatalf("branch functions = %v", got)
	}
	for _, slug := range []string{"hello", "world"} {
		p, br := c.fns.fns[parentRef][slug], c.fns.fns[b.Ref][slug]
		if br.digest != p.digest || br.version != p.version || !br.updated.Equal(p.updated) || br.id == p.id {
			t.Errorf("%s: branch %+v, parent %+v (same digest, version and update time, a new id)", slug, br, p)
		}
	}
	if got := c.fns.secrets[b.Ref]; !reflect.DeepEqual(got, map[string]string{"MY_SECRET": "x"}) {
		t.Fatalf("branch secrets = %v, want the create body's alone", got)
	}
	if !contains(c.fns.applied, b.Ref) {
		t.Fatalf("the runtime was not told about the branch: %v", c.fns.applied)
	}
	// Their digests are the base a push and a merge compare with.
	base, _ := registry.BranchContentOf(c.reg).GetBranchFunctionBase(context.Background(), b.Ref)
	if !reflect.DeepEqual(base, map[string]string{"hello": "d-hello", "world": "d-world"}) {
		t.Fatalf("function base = %v", base)
	}
	// Without secrets in the body the branch has none.
	b2 := c.create("two", nil)
	if len(c.fns.secrets[b2.Ref]) != 0 {
		t.Fatalf("secrets of a branch made without any = %v", c.fns.secrets[b2.Ref])
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestPushOverwritesSameSlugAndKeepsBranchOnlyFunctions(t *testing.T) {
	c := newContentHarness(t, nil)
	c.fns.put(parentRef, "a", "a1", 1)
	c.fns.put(parentRef, "b", "b1", 1)
	b := c.create("one", nil)
	// The branch changed b and added mine; the parent changed a and added n.
	c.fns.put(b.Ref, "b", "b-branch", 2)
	c.fns.put(b.Ref, "mine", "m1", 1)
	c.fns.put(parentRef, "a", "a2", 2)
	c.fns.put(parentRef, "n", "n1", 1)
	if _, err := c.svc.Push(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := c.wait(b.Ref)
	c.mustState(got, registry.BranchFunctionsDeployed)
	d, _ := c.fns.Digests(context.Background(), b.Ref)
	if want := map[string]string{"a": "a2", "b": "b-branch", "mine": "m1", "n": "n1"}; !reflect.DeepEqual(d, want) {
		t.Fatalf("branch functions after the push = %v, want %v", d, want)
	}
	if !strings.Contains(got.Detail, "added n") || !strings.Contains(got.Detail, "overwrote a") || !strings.Contains(got.Detail, "kept the branch's version of b") {
		t.Fatalf("detail = %q", got.Detail)
	}
	// What the parent changed after the base was overwritten, even when the branch had changed it too.
	c.fns.put(b.Ref, "a", "a-branch", 3)
	c.fns.put(parentRef, "a", "a3", 3)
	c.svc.Push(context.Background(), b.Ref, ActionInput{})
	got = c.wait(b.Ref)
	if d, _ = c.fns.Digests(context.Background(), b.Ref); d["a"] != "a3" || !strings.Contains(got.Detail, "branch's own changes are gone") {
		t.Fatalf("a = %q, detail = %q", d["a"], got.Detail)
	}
}

func TestMergeDeploysNewAndChangedFunctionsAndNeverDeletes(t *testing.T) {
	c := newContentHarness(t, nil)
	c.fns.put(parentRef, "same", "s1", 1)
	c.fns.put(parentRef, "edit", "e1", 1)
	c.fns.put(parentRef, "parent-only", "p1", 1)
	c.fns.put(parentRef, "gone-on-branch", "g1", 1)
	b := c.create("one", nil)
	delete(c.fns.fns[b.Ref], "gone-on-branch") // deleting on the branch is not carried to the parent
	c.fns.put(b.Ref, "edit", "e2", 2)
	c.fns.put(b.Ref, "fresh", "f1", 1)
	if _, err := c.svc.Merge(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := c.wait(b.Ref)
	c.mustState(got, registry.BranchFunctionsDeployed)
	d, _ := c.fns.Digests(context.Background(), parentRef)
	if want := map[string]string{"same": "s1", "edit": "e2", "parent-only": "p1", "gone-on-branch": "g1", "fresh": "f1"}; !reflect.DeepEqual(d, want) {
		t.Fatalf("parent functions after the merge = %v, want %v", d, want)
	}
	if !strings.Contains(got.Detail, "deployed 2 Edge Function(s)") || !strings.Contains(got.Detail, "edit, fresh") {
		t.Fatalf("detail = %q", got.Detail)
	}
	if !strings.HasPrefix(got.Detail, "no migrations to merge") {
		t.Fatalf("detail = %q", got.Detail)
	}
	// Merging again changes nothing.
	c.svc.Merge(context.Background(), b.Ref, ActionInput{})
	if got = c.wait(b.Ref); !strings.Contains(got.Detail, "nothing to merge") || strings.Contains(got.Detail, "deployed") {
		t.Fatalf("second merge detail = %q", got.Detail)
	}
}

func TestMergeFailsBeforeRunningAnythingWhenBothSidesChangedAFunction(t *testing.T) {
	c := newContentHarness(t, nil)
	c.fns.put(parentRef, "shared", "s1", 1)
	b := c.create("one", nil)
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	c.fns.put(b.Ref, "shared", "s-branch", 2)
	c.fns.put(parentRef, "shared", "s-parent", 2)
	if _, err := c.svc.Merge(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := c.wait(b.Ref)
	c.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "both changed the Edge Function(s) shared") {
		t.Fatalf("detail = %q", got.Detail)
	}
	if len(c.db.applied[parentRef]) != 0 {
		t.Fatalf("a refused merge ran %v on the parent", c.db.applied[parentRef])
	}
	// Forced, the branch's version is deployed.
	if _, err := c.svc.Merge(context.Background(), b.Ref, ActionInput{Force: true}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	if d, _ := c.fns.Digests(context.Background(), parentRef); d["shared"] != "s-branch" {
		t.Fatalf("parent shared = %q", d["shared"])
	}
}

// ---- settings and class -----------------------------------------------------------------------

func TestBranchStartsOnTheParentsSettingsAndAReplayedClusterDoesNot(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	if got := c.eng.creates[0].SettingsFrom; got != parentRef {
		t.Fatalf("SettingsFrom = %q, want the parent", got)
	}
	// The restored cluster of a base-backup branch is stamped the same way.
	m := stampManager{Manager: c.eng, info: &registry.BranchInfo{ID: secrets.NewUUID(), ParentRef: parentRef, Name: "r"}, settingsFrom: parentRef}
	if _, err := m.Create(context.Background(), lifecycle.CreateRequest{Ref: "rrrrrrrrrrrrrrrrrrrr", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if got := c.eng.creates[len(c.eng.creates)-1].SettingsFrom; got != parentRef {
		t.Fatalf("restore SettingsFrom = %q", got)
	}
	_ = b
}

func TestWithDataBranchDefaultsToTheParentsClass(t *testing.T) {
	c := newContentHarness(t, nil)
	p, _ := c.reg.GetProject(context.Background(), parentRef)
	p.Class = "large"
	if err := c.reg.UpdateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	c.svc.detect = func(string, string) (string, string, string) { return MethodClonefile, "apfs", "" }
	c.svc.clone = func(_ context.Context, _, m, _ string) (*CloneStats, error) { return &CloneStats{Method: m}, nil }
	c.create("data", func(in *CreateInput) { in.WithData = true })
	c.create("schema", nil)
	c.create("sized", func(in *CreateInput) { in.WithData = true; in.DesiredInstanceSize = "small" })
	got := []string{c.eng.creates[0].Class, c.eng.creates[1].Class, c.eng.creates[2].Class}
	if want := []string{"large", c.cfg.Branching.Class(), "small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("classes = %v, want %v (the parent's for data, the default for schema only, the asked one)", got, want)
	}
}

func TestDropSizingKeepsEveryOtherLine(t *testing.T) {
	dir := t.TempDir()
	conf := "# Do not edit\nshared_buffers = '4GB'\nstatement_timeout = '8s'\nWORK_MEM = '64MB'\nmax_connections = 400\nlog_statement = 'all'\n"
	if err := os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dropSizing(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if want := "# Do not edit\nstatement_timeout = '8s'\nlog_statement = 'all'\n"; string(got) != want {
		t.Fatalf("auto.conf = %q, want %q", got, want)
	}
	// Every sizing setting goes, and a directory with no file is fine.
	var all strings.Builder
	for _, k := range projectconfig.SizingSettings {
		fmt.Fprintf(&all, "%s = '1'\n", k)
	}
	os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte(all.String()), 0o600)
	if err := dropSizing(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf")); len(got) != 0 {
		t.Fatalf("sizing left in %q", got)
	}
	if err := dropSizing(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// lifecycle puts the same settings on the command line.
	a, b := append([]string(nil), projectconfig.SizingSettings...), lifecycle.SizingSettings()
	sort.Strings(a)
	sort.Strings(b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("projectconfig.SizingSettings %v, lifecycle %v", a, b)
	}
}

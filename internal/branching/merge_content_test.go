package branching

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// The tests in this file run the merge, reset, push and baseline flows over the fake database. The
// statements themselves are checked on a real Postgres in content_pg_test.go.

// dashboardBranch makes a branch whose schema gained the table public.notes and holds the tracked
// migrations, then records its diff the way Studio's merge page does (a branch_merge_* migration that
// is not run) as the branch's newest migration.
func (c *contentHarness) dashboardBranch(tracked ...Migration) (*Branch, Migration) {
	c.t.Helper()
	ctx := context.Background()
	c.sdb.snaps[parentRef] = snap(table("public", "todos", "id"))
	b := c.create("one", nil)
	c.sdb.snaps[b.Ref] = snap(table("public", "todos", "id"), table("public", "notes", "id"))
	c.setBase(b.Ref, snap(table("public", "todos", "id")))
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], tracked...)
	diff, err := c.svc.Diff(ctx, b.Ref)
	if err != nil || diff == "" {
		c.t.Fatalf("diff = %q, %v", diff, err)
	}
	rec := mig("20260120000000", "branch_merge_a", diff)
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], rec)
	return b, rec
}

func (c *contentHarness) merge(b *Branch, in ActionInput) *Branch {
	c.t.Helper()
	if _, err := c.svc.Merge(context.Background(), b.Ref, in); err != nil {
		c.t.Fatal(err)
	}
	return c.wait(b.Ref)
}

func (c *contentHarness) row(ref, version string) Migration {
	c.t.Helper()
	for _, m := range c.db.migs[ref] {
		if m.Version == version {
			return m
		}
	}
	c.t.Fatalf("%s has no migration %s: %+v", ref, version, c.db.migs[ref])
	return Migration{}
}

func TestRecordedMergeRunsTheStatementsADiffDoesNotHold(t *testing.T) {
	c := newContentHarness(t, nil)
	tracked := mig("20260110000000", "notes",
		"create table public.notes (id int)",
		"insert into storage.buckets (id, name) values ('avatars', 'avatars')",
		"alter publication supabase_realtime add table public.notes")
	plain := mig("20260111000000", "index", "create index on public.notes (id)")
	b, rec := c.dashboardBranch(tracked, plain)

	got := c.merge(b, ActionInput{})
	c.mustState(got, registry.BranchFunctionsDeployed)
	// Only the diff ran; the covered migrations were recorded, and what the diff does not hold ran after it.
	if want := []string{rec.Version}; !reflect.DeepEqual(c.db.applied[parentRef], want) {
		t.Fatalf("ran on the parent: %v, want %v", c.db.applied[parentRef], want)
	}
	carried := strings.Join(c.db.carried[parentRef], "\n")
	if len(c.db.carried[parentRef]) != 1 || !strings.Contains(carried, "insert into storage.buckets") || !strings.Contains(carried, "alter publication") || strings.Contains(carried, "create table") {
		t.Fatalf("carried statements = %q", c.db.carried[parentRef])
	}
	for _, v := range []string{tracked.Version, plain.Version} {
		if m := c.row(parentRef, v); m.Covered != rec.Version {
			t.Fatalf("parent row %s covered by %q, want %s", v, m.Covered, rec.Version)
		}
		if m := c.row(b.Ref, v); m.Covered != rec.Version {
			t.Fatalf("branch row %s covered by %q, want %s", v, m.Covered, rec.Version)
		}
	}
	if m := c.row(parentRef, rec.Version); m.Covered != "" {
		t.Fatalf("the diff itself is covered by %q", m.Covered)
	}
	if !strings.Contains(got.Detail, "ran 2 statement(s)") || !strings.Contains(got.Detail, tracked.Version) {
		t.Fatalf("detail = %q", got.Detail)
	}
	// Merging again finds nothing.
	got = c.merge(b, ActionInput{})
	if !strings.Contains(got.Detail, "nothing to merge") {
		t.Fatalf("second merge detail = %q", got.Detail)
	}
}

func TestRecordedMergeIsAllOrNothing(t *testing.T) {
	c := newContentHarness(t, nil)
	b, rec := c.dashboardBranch(mig("20260110000000", "notes", "create table public.notes (id int)", "insert into public.notes values (1)"))
	c.db.applyErr[parentRef] = errors.New("boom")
	got := c.merge(b, ActionInput{})
	c.mustState(got, registry.BranchMigrationsFailed)
	if n := len(c.db.migs[parentRef]); n != 3 {
		t.Fatalf("the parent's history has %d rows after a failed merge", n)
	}
	if m := c.row(b.Ref, "20260110000000"); m.Covered != "" {
		t.Fatalf("the branch's row was marked as covered by a diff that did not merge: %q", m.Covered)
	}
	// The retry is the same merge, and it works.
	delete(c.db.applyErr, parentRef)
	got = c.merge(b, ActionInput{})
	c.mustState(got, registry.BranchFunctionsDeployed)
	if !strings.Contains(got.Detail, rec.Version) || len(c.db.carried[parentRef]) != 1 {
		t.Fatalf("detail = %q, carried %v", got.Detail, c.db.carried[parentRef])
	}
}

// A recording the branch has moved past (a failed attempt, then apply_migration) must not decide the
// merge, whoever asks for it and whatever force says.
func TestStaleRecordedDiffIsIgnoredByTheHistoryMerge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		force  bool
		marker bool
		cover  bool
	}{
		{name: "plain"},
		{name: "force", force: true},
		{name: "unsupported marker in the stale diff", marker: true},
		{name: "rows marked as covered by it", cover: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newContentHarness(t, nil)
			older := mig("20260110000000", "notes", "create table public.notes (id int)")
			b, rec := c.dashboardBranch(older)
			if tc.marker {
				rows := c.db.migs[b.Ref]
				rows[len(rows)-1].Statements = []string{rows[len(rows)-1].sql() + "\n" + schemadiff.UnsupportedMarker + " aggregate public.agg(integer) exists only in the source; create it by hand"}
			}
			if tc.cover {
				for i, m := range c.db.migs[b.Ref] {
					if m.Version == older.Version {
						c.db.migs[b.Ref][i].Covered = rec.Version
					}
				}
			}
			newer := mig("20260125000000", "more", "create table public.more (id int)")
			c.db.migs[b.Ref] = append(c.db.migs[b.Ref], newer) // after the recording

			got := c.merge(b, ActionInput{Force: tc.force})
			c.mustState(got, registry.BranchFunctionsDeployed)
			if want := []string{older.Version, newer.Version}; !reflect.DeepEqual(c.db.applied[parentRef], want) {
				t.Fatalf("ran on the parent: %v, want the branch's migrations %v and not the stale recording", c.db.applied[parentRef], want)
			}
			for _, m := range c.db.migs[parentRef] {
				if m.Version == rec.Version {
					t.Fatalf("the stale recording reached the parent's history")
				}
			}
			if !strings.Contains(got.Detail, "ignored the schema diff") || !strings.Contains(got.Detail, rec.Version) {
				t.Fatalf("detail = %q", got.Detail)
			}
			// And it stays out of the way: the next merge has nothing to do.
			again := c.merge(b, ActionInput{})
			if !strings.Contains(again.Detail, "nothing to merge") || !strings.Contains(again.Detail, "ignored") {
				t.Fatalf("second merge detail = %q", again.Detail)
			}
		})
	}
}

func TestRecordedDiffThatIsOutOfDateIsNotMergedEvenForced(t *testing.T) {
	c := newContentHarness(t, nil)
	b, _ := c.dashboardBranch()
	c.sdb.snaps[b.Ref] = snap(table("public", "todos", "id"), table("public", "notes", "id"), table("public", "later", "id")) // after the recording
	for _, force := range []bool{false, true} {
		got := c.merge(b, ActionInput{Force: force})
		c.mustState(got, registry.BranchMigrationsFailed)
		if !strings.Contains(got.Detail, "changed since the diff was recorded; refresh") {
			t.Fatalf("force=%v: detail = %q", force, got.Detail)
		}
		if len(c.db.applied[parentRef]) != 0 || len(c.db.migs[parentRef]) != 3 {
			t.Fatalf("force=%v: the parent was changed: ran %v, history %d", force, c.db.applied[parentRef], len(c.db.migs[parentRef]))
		}
	}
}

func TestRecordedDiffWithChangesThatCannotBeExpressedNeedsForce(t *testing.T) {
	c := newContentHarness(t, nil)
	c.sdb.snaps[parentRef] = snap(table("public", "todos", "id"))
	b := c.create("one", nil)
	s := snap(table("public", "todos", "id"), table("public", "notes", "id"))
	s.Unmodeled = []string{"aggregate public.agg(integer)"}
	c.sdb.snaps[b.Ref] = s
	c.setBase(b.Ref, snap(table("public", "todos", "id")))
	diff, _ := c.svc.Diff(context.Background(), b.Ref)
	if !schemadiff.HasUnsupported(diff) {
		t.Fatalf("diff = %q", diff)
	}
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], mig("20260120000000", "branch_merge_a", diff))

	got := c.merge(b, ActionInput{})
	c.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "cannot express") || !strings.Contains(got.Detail, "use force") {
		t.Fatalf("detail = %q", got.Detail)
	}
	got = c.merge(b, ActionInput{Force: true})
	c.mustState(got, registry.BranchFunctionsDeployed)
	if !strings.Contains(got.Detail, "not merged") || !strings.Contains(got.Detail, "aggregate public.agg") {
		t.Fatalf("forced detail = %q", got.Detail)
	}
}

// The mark on the branch's rows is written after the parent merged; when that failed, a later merge puts it right.
func TestMergeMarksTheBranchRowsThatTheParentRecordsAsCovered(t *testing.T) {
	c := newContentHarness(t, nil)
	c.sdb.snaps[parentRef] = snap(table("public", "todos", "id"))
	b := c.create("one", nil)
	tracked := mig("20260110000000", "notes", "create table public.notes (id int)")
	rec := mig("20260120000000", "branch_merge_a", "create table public.notes (id int)")
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], tracked, rec)
	covered := tracked
	covered.Covered = rec.Version
	c.db.migs[parentRef] = append(c.db.migs[parentRef], covered, rec) // merged, the branch not marked
	got := c.merge(b, ActionInput{})
	c.mustState(got, registry.BranchFunctionsDeployed)
	if m := c.row(b.Ref, tracked.Version); m.Covered != rec.Version {
		t.Fatalf("branch row covered by %q, want %s", m.Covered, rec.Version)
	}
	if log := c.runLog(parentRef); !strings.Contains(log, "marked the migrations "+tracked.Version) { // a merge is listed under the parent
		t.Fatalf("run log: %s", log)
	}
}

// ---- reset --------------------------------------------------------------------------------

func TestResetCoversEveryUnmergedRecordingButTheNewest(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	tracked := mig("20260110000000", "notes", "create table public.notes (id int)", "insert into public.notes values (1)")
	r1 := mig("20260120000000", "branch_merge_a", "create table public.notes (id int)")
	r2 := mig("20260121000000", "branch_merge_b", "create table public.notes (id int)")
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], tracked, r1, r2)
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.db.forget(ref) }
	c.db.applied[b.Ref] = nil

	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	// The three migrations of the parent and the newest recording ran; the rest were recorded, and the
	// data statement of the tracked migration ran after the recording.
	if want := []string{"20260101000000", "20260102000000", "20260103000000", r2.Version}; !reflect.DeepEqual(c.db.applied[b.Ref], want) {
		t.Fatalf("ran on the branch: %v, want %v", c.db.applied[b.Ref], want)
	}
	for _, v := range []string{tracked.Version, r1.Version} {
		if m := c.row(b.Ref, v); m.Covered != r2.Version {
			t.Fatalf("%s is covered by %q, want %s", v, m.Covered, r2.Version)
		}
	}
	if got := c.db.carried[b.Ref]; len(got) != 1 || !strings.Contains(got[0], "insert into public.notes") {
		t.Fatalf("carried = %v", got)
	}
}

func TestResetToAVersionRunsInFullWhatTheCutAwayRecordingCovered(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	tracked := mig("20260110000000", "notes", "create table public.notes (id int)")
	rec := mig("20260120000000", "branch_merge_a", "create table public.notes (id int)")
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], tracked, rec)
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.db.forget(ref) }
	c.db.applied[b.Ref] = nil

	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: tracked.Version}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	if got := c.db.applied[b.Ref]; len(got) != 4 || got[3] != tracked.Version {
		t.Fatalf("ran on the branch: %v, want the migration at the version in full", got)
	}
	if m := c.row(b.Ref, tracked.Version); m.Covered != "" {
		t.Fatalf("the migration is still covered by the recording that was cut away: %q", m.Covered)
	}
}

func TestResetOfABranchThatCannotBeReadKeepsItsMigrations(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	c.db.migs[b.Ref] = append(c.db.migs[b.Ref], mig("20260110000000", "mine", "create table public.mine (id int)"))
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.db.forget(ref) }
	c.db.readErr[b.Ref] = errors.New("connection refused")

	// A branch that works and does not answer: stop, and say why.
	_, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{})
	if err == nil || !strings.Contains(err.Error(), "read the branch's migration history") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("reset of an unreadable branch: %v", err)
	}
	if len(c.eng.deletes) != 0 {
		t.Fatalf("a refused reset removed %v", c.eng.deletes)
	}
	// Forced, it is reset from the parent's history, and the run log says so.
	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{Force: true}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	delete(c.db.readErr, b.Ref)
	if !strings.Contains(c.runLog(b.Ref), "recreated from the parent's migration history") {
		t.Fatalf("run log: %s", c.runLog(b.Ref))
	}
	for _, m := range c.db.migs[b.Ref] {
		if m.Name == "mine" {
			t.Fatal("the forced reset kept a migration it could not read")
		}
	}
}

func TestResetOfABranchThatFailedFallsBackToTheParentsHistory(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	c.svc.setState(context.Background(), b.Ref, registry.BranchMigrationsFailed, "boom", nil)
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.db.forget(ref) }
	c.db.readErr[b.Ref] = errors.New("connection refused")
	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatalf("reset of a failed branch: %v", err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	delete(c.db.readErr, b.Ref)
	if !strings.Contains(c.runLog(b.Ref), "could not be read") {
		t.Fatalf("run log: %s", c.runLog(b.Ref))
	}
}

// ---- push ----------------------------------------------------------------------------------

func TestPushStoppedAtAVersionMovesTheBase(t *testing.T) {
	c := newContentHarness(t, nil)
	c.sdb.snaps[parentRef] = snap(table("public", "todos", "id"))
	b := c.create("one", nil)
	c.setBase(b.Ref, snap(table("public", "todos", "id")))
	later := mig("20260115000000", "later", "create table public.later (id int)")
	rest := mig("20260116000000", "rest", "create table public.rest (id int)")
	c.db.migs[parentRef] = append(c.db.migs[parentRef], later, rest)
	// What the push brings: the table of the first migration is on the branch afterwards, and the
	// parent has both.
	c.sdb.snaps[parentRef] = snap(table("public", "todos", "id"), table("public", "later", "id"), table("public", "rest", "id"))
	c.sdb.snaps[b.Ref] = snap(table("public", "todos", "id"), table("public", "later", "id"))

	if _, err := c.svc.Push(context.Background(), b.Ref, ActionInput{MigrationVersion: later.Version}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	base, ok, err := c.svc.getBase(context.Background(), b.Ref)
	if err != nil || !ok {
		t.Fatalf("base: %v, %v", ok, err)
	}
	keys := map[string]bool{}
	for _, o := range base.Objects {
		keys[o.Key] = true
	}
	if !keys["table|public|later"] || keys["table|public|rest"] {
		t.Fatalf("base after the push = %v: the table of the pushed migration must be in it, the other must not", keys)
	}
	// So the branch's diff does not list what the push brought as the branch's own change.
	if d, _ := c.svc.Diff(context.Background(), b.Ref); d != "" {
		t.Fatalf("diff after the push:\n%s", d)
	}
}

// ---- baseline ------------------------------------------------------------------------------

func TestBaselineThatFailsOnTheBranchIsNotRecordedOnTheParent(t *testing.T) {
	c := newContentHarness(t, func(c *contentHarness) {
		c.db.migs[parentRef] = nil
		c.sdb.snaps[parentRef] = snap(table("public", "todos", "id"))
	})
	c.db.applyErr["*"] = errors.New("type does not exist")
	b := c.create("one", nil)
	c.mustState(b, registry.BranchMigrationsFailed)
	if !strings.Contains(b.Detail, "baseline") || !strings.Contains(b.Detail, "nothing was recorded in the parent's migration history") {
		t.Fatalf("detail = %q", b.Detail)
	}
	if len(c.db.migs[parentRef]) != 0 || c.sdb.initCalls != 0 {
		t.Fatalf("the parent's history = %+v after a baseline that did not run", c.db.migs[parentRef])
	}
	// Once the cause is gone, the next branch records it.
	delete(c.db.applyErr, "*")
	b2 := c.create("two", nil)
	c.mustState(b2, registry.BranchFunctionsDeployed)
	if len(c.db.migs[parentRef]) != 1 || c.sdb.initCalls != 1 {
		t.Fatalf("the parent's history = %+v", c.db.migs[parentRef])
	}
}

func TestOnlyWhatAUserMadeCountsAsASchemaForABaseline(t *testing.T) {
	stock := &schemadiff.Snapshot{Version: schemadiff.FormatVersion, Unmodeled: []string{"collation public.c", "foreign server s"}}
	if hasUserSchema(stock) {
		t.Error("objects the engine does not model make a baseline that cannot hold them")
	}
	if !hasUserSchema(snap(table("public", "todos", "id"))) {
		t.Error("a table is a schema")
	}
	if hasUserSchema(snap()) {
		t.Error("nothing is not a schema")
	}
	// A parent with only unmodeled objects starts the branch empty and says what was left out.
	c := newContentHarness(t, func(c *contentHarness) {
		c.db.migs[parentRef] = nil
		s := snap()
		s.Unmodeled = []string{"collation public.c"}
		c.sdb.snaps[parentRef] = s
	})
	b := c.create("one", nil)
	c.mustState(b, registry.BranchFunctionsDeployed)
	if c.sdb.initCalls != 0 || !strings.Contains(c.runLog(b.Ref), "collation public.c") {
		t.Fatalf("init calls %d, run log: %s", c.sdb.initCalls, c.runLog(b.Ref))
	}
}

// ---- covered markers ------------------------------------------------------------------------

func TestSettleCoversKeepsAMarkerOnlyWhileTheCoveringMigrationIsThere(t *testing.T) {
	cov := func(v, by string) Migration { m := mig(v, "n"+v, "x"); m.Covered = by; return m }
	ms := []Migration{cov("1", "3"), cov("2", "9"), mig("3", "diff", "y"), cov("4", "5")}
	got := settleCovers(ms, []Migration{mig("9", "other", "z")})
	want := map[string]string{"1": "3", "2": "9", "3": "", "4": ""}
	for _, m := range got {
		if m.Covered != want[m.Version] {
			t.Errorf("%s covered by %q, want %q", m.Version, m.Covered, want[m.Version])
		}
	}
	if ms[3].Covered != "5" {
		t.Error("settleCovers changed its input")
	}
}

func TestCoveredLastKeepsTheOrderInsideEachGroup(t *testing.T) {
	cov := func(v string) Migration { m := mig(v, v, "x"); m.Covered = "9"; return m }
	got := versions(coveredLast([]Migration{cov("1"), mig("2", "b", "x"), cov("3"), mig("9", "d", "x")}))
	if got != "2, 9, 1, 3" {
		t.Fatalf("order = %s", got)
	}
}

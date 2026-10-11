package schemadiff

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// These tests need no database: they build snapshots by hand.

func tbl(schema, name string, cols ...string) []Object {
	tk := "table|" + schema + "|" + name
	out := []Object{{Kind: KindTable, Key: tk, Name: qname(schema, name), Parent: "schema|" + schema, Attr: map[string]string{"rls": "false", "force": "false"}}}
	for i, c := range cols {
		out = append(out, Object{Kind: KindColumn, Key: "column|" + schema + "|" + name + "|" + c, Name: QuoteIdent(c), Parent: tk,
			Attr: map[string]string{"type": "integer", "notnull": "false", "pos": string(rune('1' + i))}})
	}
	return out
}

func snap(objs ...[]Object) *Snapshot {
	s := &Snapshot{Version: FormatVersion, Schemas: []string{"public"}}
	for _, o := range objs {
		s.Objects = append(s.Objects, o...)
	}
	s.Sort()
	return s
}

func TestEqualSnapshotsHaveNoDiff(t *testing.T) {
	a, b := snap(tbl("public", "t", "id")), snap(tbl("public", "t", "id"))
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("equal snapshots, different fingerprints")
	}
	if r := Diff(a, b, Options{}); !r.Empty() || r.SQL() != "" {
		t.Fatalf("diff of equal snapshots = %q", r.SQL())
	}
	if Diff(nil, nil, Options{}).SQL() != "" {
		t.Fatal("diff of nothing")
	}
}

func TestCreateAddAndDropStatements(t *testing.T) {
	a := snap(tbl("public", "old", "id"), tbl("public", "keep", "id"))
	b := snap(tbl("public", "keep", "id", "extra"), tbl("public", "new", "id"))
	r := Diff(a, b, Options{})
	got := strings.Join(r.Statements, "\n")
	for _, want := range []string{`DROP TABLE "public"."old"`, `CREATE TABLE "public"."new"`, `ALTER TABLE "public"."keep" ADD COLUMN "extra" integer`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	// Drops come before creates.
	if strings.Index(got, "DROP TABLE") > strings.Index(got, "CREATE TABLE") {
		t.Errorf("a drop after a create:\n%s", got)
	}
	// And the other way round.
	if back := Diff(b, a, Options{}); !strings.Contains(strings.Join(back.Statements, "\n"), `DROP COLUMN "extra"`) {
		t.Errorf("reverse diff: %q", back.SQL())
	}
}

func TestStatementsFollowDependencies(t *testing.T) {
	fn := Object{Kind: KindFunction, Key: "function|public|f()", Name: `"public"."f"()`, Def: "CREATE OR REPLACE FUNCTION public.f() RETURNS integer LANGUAGE sql AS $$ select 1 $$", Attr: map[string]string{"kind": "FUNCTION", "result": "integer"}}
	def := Object{Kind: KindDefault, Key: "default|public|t|id", Name: `"id"`, Parent: "column|public|t|id", Def: "public.f()", Attr: map[string]string{"table": `"public"."t"`}, Deps: []string{"function|public|f()"}}
	r := Diff(snap(), snap(tbl("public", "t", "id"), []Object{def, fn}), Options{})
	var iFn, iTbl, iDef int
	for i, s := range r.Statements {
		switch {
		case strings.Contains(s, "CREATE OR REPLACE FUNCTION"):
			iFn = i
		case strings.HasPrefix(s, "CREATE TABLE"):
			iTbl = i
		case strings.Contains(s, "SET DEFAULT"):
			iDef = i
		}
	}
	if !(iTbl < iDef && iFn < iDef) {
		t.Fatalf("a default must follow its table and the function it calls:\n%s", r.SQL())
	}
	if r.Statements[0] != "SET check_function_bodies = off" {
		t.Fatalf("a script that creates a function turns the body check off first:\n%s", r.SQL())
	}
}

func TestUnsupportedChangesAreMarkedAndKeepRemovedLeavesDropsOut(t *testing.T) {
	a, b := snap(tbl("public", "old", "id")), snap(tbl("public", "new", "id"))
	b.Unmodeled = []string{"aggregate public.agg(integer)"}
	r := Diff(a, b, Options{})
	sql := r.SQL()
	if !HasUnsupported(sql) || !strings.HasPrefix(sql, UnsupportedMarker+"aggregate public.agg") {
		t.Fatalf("sql = %q", sql)
	}
	if got := unsupportedOf(sql); !reflect.DeepEqual(got, []string{"aggregate public.agg(integer) exists only in the source; create it by hand"}) {
		t.Fatalf("unsupported = %v", got)
	}
	kept := Diff(a, b, Options{KeepRemoved: true})
	if strings.Contains(strings.Join(kept.Statements, "\n"), "DROP") {
		t.Fatalf("KeepRemoved dropped:\n%s", kept.SQL())
	}
}

func unsupportedOf(sql string) []string {
	var out []string
	for _, l := range strings.Split(sql, "\n") {
		if rest, ok := strings.CutPrefix(l, UnsupportedMarker); ok {
			out = append(out, rest)
		}
	}
	return out
}

func TestEnumsOnlyGrow(t *testing.T) {
	enum := func(labels ...string) Object {
		return Object{Kind: KindEnum, Key: "enum|public|mood", Name: `"public"."mood"`, List: labels, Def: "CREATE TYPE ..."}
	}
	r := Diff(snap([]Object{enum("a", "c")}), snap([]Object{enum("a", "b", "c", "d")}), Options{})
	got := strings.Join(r.Statements, "\n")
	if !strings.Contains(got, `ADD VALUE IF NOT EXISTS 'b' AFTER 'a'`) || !strings.Contains(got, `ADD VALUE IF NOT EXISTS 'd' AFTER 'c'`) || len(r.Unsupported) != 0 {
		t.Fatalf("diff = %q", r.SQL())
	}
	r = Diff(snap([]Object{enum("a", "b")}), snap([]Object{enum("b")}), Options{})
	if len(r.Unsupported) != 1 {
		t.Fatalf("removing a value must be named: %q", r.SQL())
	}
}

func TestMerge3TakesTheOtherSidesChangesAndNamesConflicts(t *testing.T) {
	retype := func(objs []Object, col, typ string) []Object {
		for i := range objs {
			if objs[i].Kind == KindColumn && strings.HasSuffix(objs[i].Key, "|"+col) {
				a := map[string]string{}
				for k, v := range objs[i].Attr {
					a[k] = v
				}
				a["type"] = typ
				objs[i].Attr = a
			}
		}
		return objs
	}
	base := snap(tbl("public", "a", "id"), tbl("public", "b", "id"), tbl("public", "c", "id"))
	// ours removed b, retyped c.id and added mine; theirs added a column to a and a table, and retyped c.id differently
	ours := snap(tbl("public", "a", "id"), retype(tbl("public", "c", "id"), "id", "bigint"), tbl("public", "mine", "id"))
	theirs := snap(tbl("public", "a", "id", "theirs"), tbl("public", "b", "id"), retype(tbl("public", "c", "id"), "id", "text"), tbl("public", "new", "id"))
	merged, conflicts := Merge3(base, ours, theirs, false)
	has := func(key string) bool { return merged.index()[key] != nil }
	if !has("column|public|a|theirs") || !has("table|public|new") || !has("table|public|mine") || has("table|public|b") {
		t.Errorf("merged = %v", keysOf(merged))
	}
	// c.id: both changed it, differently; ours stays.
	if merged.index()["column|public|c|id"].Attr["type"] != "bigint" || len(conflicts) != 1 || !strings.Contains(conflicts[0], "column public.c.id") {
		t.Errorf("conflicts = %v", conflicts)
	}
	forced, _ := Merge3(base, ours, theirs, true)
	if forced.index()["column|public|c|id"].Attr["type"] != "text" {
		t.Errorf("preferTheirs kept ours")
	}
	// The plan from ours to the merge adds what theirs added and drops nothing of ours.
	r := Diff(ours, merged, Options{})
	if got := strings.Join(r.Statements, "\n"); strings.Contains(got, "mine") || !strings.Contains(got, `"public"."new"`) {
		t.Errorf("push plan:\n%s", got)
	}
}

func keysOf(s *Snapshot) []string {
	var k []string
	for _, o := range s.Objects {
		k = append(k, o.Key)
	}
	return k
}

func TestSyncMovesTheBaseOnlyWhereTheSidesAgree(t *testing.T) {
	base := snap(tbl("public", "a", "id"))
	branch := snap(tbl("public", "a", "id"), tbl("public", "merged", "id"), tbl("public", "unmerged", "id"))
	parent := snap(tbl("public", "a", "id"), tbl("public", "merged", "id"), tbl("public", "drift", "id"))
	got := Sync(base, branch, parent)
	idx := got.index()
	if idx["table|public|merged"] == nil || idx["table|public|unmerged"] != nil || idx["table|public|drift"] != nil || idx["table|public|a"] == nil {
		t.Fatalf("sync = %v", keysOf(got))
	}
	// What is still different from the new base is exactly the branch's unmerged table.
	if r := Diff(got, branch, Options{}); !strings.Contains(r.SQL(), "unmerged") || strings.Contains(r.SQL(), "merged\"\n") {
		t.Fatalf("branch changes after the sync:\n%s", r.SQL())
	}
}

func TestRestrictAndCovers(t *testing.T) {
	s := snap(tbl("public", "a", "id"), tbl("other", "b", "id"))
	s.Schemas = []string{"public", "other"}
	r := s.Restrict([]string{"other"})
	if r.index()["table|public|a"] != nil || r.index()["table|other|b"] == nil || !r.Covers([]string{"other"}) || r.Covers([]string{"public"}) {
		t.Fatalf("restricted = %v", keysOf(r))
	}
}

func TestSnapshotRoundTripAndFormatVersion(t *testing.T) {
	s := snap(tbl("public", "a", "id", "b"))
	b, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Unmarshal(b)
	if err != nil || back.Fingerprint() != s.Fingerprint() {
		t.Fatalf("round trip: %v", err)
	}
	_, err = Unmarshal([]byte(`{"v": 99}`))
	var ve *VersionError
	if !errors.As(err, &ve) || ve.Have != 99 {
		t.Fatalf("other format: %v", err)
	}
}

func TestRolesTheTargetLacksAreLeftOut(t *testing.T) {
	acl := Object{Kind: KindACL, Key: "acl|table|public|t|ghost", Name: "ghost", Parent: "table|public|t", List: []string{"SELECT"}, Attr: map[string]string{"objtype": "TABLE", "target": `"public"."t"`}}
	pol := Object{Kind: KindPolicy, Key: "policy|public|t|p", Name: `"p"`, Parent: "table|public|t", List: []string{"ghost"},
		Attr: map[string]string{"table": `"public"."t"`, "cmd": "r", "permissive": "true", "using": "true"}}
	pol.Def = policyDDL(&pol)
	r := Diff(nil, snap(tbl("public", "t", "id"), []Object{acl, pol}), Options{RoleExists: func(n string) bool { return n != "ghost" }})
	got := strings.Join(r.Statements, "\n")
	if strings.Contains(got, "ghost") || strings.Contains(got, "CREATE POLICY") || len(r.Notes) != 2 {
		t.Fatalf("statements:\n%s\nnotes: %v", got, r.Notes)
	}
	// Without a filter every role is kept (a merge between a project and its branch).
	r = Diff(nil, snap(tbl("public", "t", "id"), []Object{acl, pol}), Options{})
	if !strings.Contains(strings.Join(r.Statements, "\n"), "ghost") {
		t.Fatalf("unfiltered diff lost the role:\n%s", r.SQL())
	}
}

func TestPoliciesOnManagedTablesNeedTheTableOnTheTarget(t *testing.T) {
	pol := Object{Kind: KindPolicy, Key: "policy|storage|objects|p", Name: `"p"`, Parent: "table|storage|objects", List: []string{"public"},
		Attr: map[string]string{"table": `"storage"."objects"`, "cmd": "r", "permissive": "true", "using": "true"}}
	pol.Def = policyDDL(&pol)
	trg := Object{Kind: KindTrigger, Key: "trigger|auth|users|t", Name: "t", Parent: "table|auth|users", Def: "CREATE TRIGGER t AFTER INSERT ON auth.users FOR EACH ROW EXECUTE FUNCTION public.f()",
		Attr: map[string]string{"table": `"auth"."users"`, "enabled": "O"}}
	to := snap([]Object{pol, trg})
	// The target has auth.users and no storage.objects (Storage has not set the branch up).
	r := Diff(nil, to, Options{TableExists: func(k string) bool { return k == "table|auth|users" }})
	got := strings.Join(r.Statements, "\n")
	if strings.Contains(got, "CREATE POLICY") || !strings.Contains(got, "CREATE TRIGGER t") || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "storage") {
		t.Fatalf("statements:\n%s\nnotes: %v", got, r.Notes)
	}
	// Without the check, or with the table there, both are created.
	for _, opt := range []Options{{}, {TableExists: func(string) bool { return true }}} {
		got := strings.Join(Diff(nil, to, opt).Statements, "\n")
		if !strings.Contains(got, "CREATE POLICY") || !strings.Contains(got, "CREATE TRIGGER t") {
			t.Fatalf("statements:\n%s", got)
		}
	}
}

func TestTheNumberOfAColumnIsNotWhatItIs(t *testing.T) {
	a := snap(tbl("public", "t", "id", "x"))
	b := snap(tbl("public", "t", "id", "x"))
	for i := range b.Objects {
		if b.Objects[i].Kind == KindColumn {
			b.Objects[i].Attr = map[string]string{"type": "integer", "notnull": "false", "pos": "7"} // a gap left by dropped columns
		}
	}
	if len(Changed(a, b)) != 0 {
		t.Fatalf("columns that differ only in their number: %v", Changed(a, b))
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/secrets"
)

// The fake database of the API tests reads the schemas the tests give it, so that the diff route
// has something to compare (branching.SchemaDatabase).

func (d *fakeBranchDB) Snapshot(_ context.Context, ref string, _ schemadiff.ReadOptions) (*schemadiff.Snapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.snaps[ref]; s != nil {
		c := *s
		return &c, nil
	}
	return &schemadiff.Snapshot{Version: schemadiff.FormatVersion}, nil
}

func (d *fakeBranchDB) Roles(context.Context, string) (map[string]bool, error) {
	return map[string]bool{"anon": true, "authenticated": true, "service_role": true}, nil
}

func (d *fakeBranchDB) Record(_ context.Context, ref string, ms []branching.Migration) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var done []string
	for _, m := range ms {
		d.migs[ref] = append(d.migs[ref], m)
		done = append(done, m.Version)
	}
	return done, nil
}

func (d *fakeBranchDB) InitHistory(_ context.Context, ref string, m branching.Migration) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.migs[ref]) > 0 {
		return false, nil
	}
	d.migs[ref] = append(d.migs[ref], m)
	return true, nil
}

func tableSnapshot(names ...string) *schemadiff.Snapshot {
	s := &schemadiff.Snapshot{Version: schemadiff.FormatVersion, Schemas: []string{"public", "other"}}
	for _, n := range names {
		schema, name, _ := strings.Cut(n, ".")
		tk := "table|" + schema + "|" + name
		s.Objects = append(s.Objects,
			schemadiff.Object{Kind: schemadiff.KindTable, Key: tk, Name: `"` + schema + `"."` + name + `"`, Attr: map[string]string{"rls": "false", "force": "false"}},
			schemadiff.Object{Kind: schemadiff.KindColumn, Key: "column|" + schema + "|" + name + "|id", Name: `"id"`, Parent: tk, Attr: map[string]string{"type": "integer", "notnull": "false", "pos": "1"}})
	}
	s.Sort()
	return s
}

func TestBranchDiffRouteIsEmptyWhenEqualAndHonorsItsParameters(t *testing.T) {
	f := newFixture(t)
	f.bdb.migs[testRef] = []branching.Migration{{Version: "20260101000000", Name: "init", Statements: []string{"create table t (id int)"}}}
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "feature"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	ref := decodeBody(t, rec).(map[string]any)["project_ref"].(string)
	f.waitBranch(t, "feature", "FUNCTIONS_DEPLOYED")

	diff := func(query string) string {
		t.Helper()
		rec := f.do("GET", "/v1/branches/"+ref+"/diff"+query, nil)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("diff%s: %d %q %s", query, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
		return rec.Body.String()
	}
	if d := diff(""); d != "" {
		t.Fatalf("diff of an unchanged branch = %q, want an empty body", d)
	}
	f.bdb.mu.Lock()
	f.bdb.snaps[ref] = tableSnapshot("public.notes", "other.things")
	f.bdb.mu.Unlock()
	all := diff("")
	if !strings.Contains(all, `CREATE TABLE "public"."notes"`) || !strings.Contains(all, `CREATE TABLE "other"."things"`) {
		t.Fatalf("diff = %q", all)
	}
	// pgdelta true or false: one engine, one answer.
	for _, q := range []string{"?pgdelta=true", "?pgdelta=false", "?pgdelta=1", "?pgdelta=off"} {
		if got := diff(q); got != all {
			t.Errorf("diff%s differs from the plain diff", q)
		}
	}
}

// PUT does not run anything: the statements reach pg-meta only as the text of a row to insert.
func TestUpsertMigrationRecordsTheStatementsWithoutRunningThem(t *testing.T) {
	f := newFixture(t)
	before := len(f.meta.Requests)
	const ddl = "create table public.never_run (id int)"
	rec := f.do("PUT", "/v1/projects/"+testRef+"/database/migrations", map[string]any{"query": ddl, "name": "branch_merge_x"}, "Idempotency-Key", "k1")
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if rec = f.do("PUT", "/v1/projects/"+testRef+"/database/migrations", map[string]any{"query": " "}); rec.Code != 400 {
		t.Fatalf("PUT without a query: %d", rec.Code)
	}
	var got []pgmetaRequest
	f.meta.mu.Lock()
	got = append(got, f.meta.Requests[before:]...)
	f.meta.mu.Unlock()
	var sent string
	for _, r := range got {
		var b struct{ Query string }
		_ = json.Unmarshal([]byte(r.Body), &b)
		if strings.Contains(b.Query, "never_run") {
			sent = b.Query
		}
	}
	if sent == "" {
		t.Fatalf("no query reached pg-meta: %v", got)
	}
	// The DDL is only ever inside a string literal (the row, and the text the guard compares it with),
	// and the idempotency key and the name are in the guard.
	outside := regexp.MustCompile(`'(?:[^']|'')*'`).ReplaceAllString(sent, "''")
	if strings.Contains(outside, "never_run") || !strings.Contains(sent, "array['"+ddl+"']") || !strings.Contains(sent, "'k1'") ||
		!strings.Contains(sent, "branch_merge_x") || !strings.Contains(sent, "having not exists") {
		t.Fatalf("query to pg-meta:\n%s", sent)
	}
}

func TestFunctionResponsesCarryTheDigestStudioComparesAndBranchesCopyThem(t *testing.T) {
	f := newFixture(t)
	st := NewMemoryStore()
	deploy := func(ref, slug, body string, verify bool) {
		t.Helper()
		fn := &Function{Ref: ref, Slug: slug, Name: slug, Status: "ACTIVE", VerifyJWT: verify, EntrypointPath: "index.ts"}
		if err := st.UpsertFunction(context.Background(), fn, []FunctionFile{{Path: "index.ts", Content: []byte(body)}, {Path: "lib/a.ts", Content: []byte("a")}}); err != nil {
			t.Fatal(err)
		}
	}
	deploy("parentparentparentpa", "hello", "export default 1", true)
	deploy("parentparentparentpa", "other", "export default 2", false)

	sec, _ := secrets.New(make([]byte, 32))
	bf := NewBranchFunctions(st, sec)
	copied, err := bf.Copy(context.Background(), "parentparentparentpa", "branchbranchbranchbr", nil)
	if err != nil || len(copied) != 2 {
		t.Fatalf("copy: %v, %v", copied, err)
	}
	pd, _ := bf.Digests(context.Background(), "parentparentparentpa")
	bd, _ := bf.Digests(context.Background(), "branchbranchbranchbr")
	if !reflect.DeepEqual(pd, bd) || !reflect.DeepEqual(copied, bd) {
		t.Fatalf("digests: parent %v, branch %v, copy %v", pd, bd, copied)
	}
	pf, _ := st.GetFunction(context.Background(), "parentparentparentpa", "hello")
	bfn, _ := st.GetFunction(context.Background(), "branchbranchbranchbr", "hello")
	if bfn.ID == pf.ID || bfn.Version != pf.Version || !bfn.UpdatedAt.Equal(pf.UpdatedAt) || !bfn.VerifyJWT || bfn.EntrypointPath != "index.ts" {
		t.Fatalf("copy: %+v, parent %+v", bfn, pf)
	}
	// A different body, or a different verify_jwt, is a different digest.
	deploy("parentparentparentpa", "hello", "export default 3", true)
	d2, _ := bf.Digests(context.Background(), "parentparentparentpa")
	if d2["hello"] == pd["hello"] || d2["other"] != pd["other"] {
		t.Fatalf("digests after a redeploy: %v, before %v", d2, pd)
	}
	// Copying over an existing function keeps the target's id and takes the next version.
	if _, err := bf.Copy(context.Background(), "parentparentparentpa", "branchbranchbranchbr", []string{"hello"}); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetFunction(context.Background(), "branchbranchbranchbr", "hello")
	if after.ID != bfn.ID || after.Version != bfn.Version+1 || !after.CreatedAt.Equal(bfn.CreatedAt) {
		t.Fatalf("overwrite: %+v, before %+v", after, bfn)
	}
	if d3, _ := bf.Digests(context.Background(), "branchbranchbranchbr"); d3["hello"] != d2["hello"] {
		t.Fatalf("the overwritten function has digest %q, want %q", d3["hello"], d2["hello"])
	}
	// The branching service reads digests with the formula of the store's own reader.
	legacy, _ := FunctionDigests(st).Digests(context.Background(), "parentparentparentpa")
	if !reflect.DeepEqual(legacy, d2) {
		t.Fatalf("FunctionDigests %v, BranchFunctions %v", legacy, d2)
	}

	// Secrets are stored sealed, and only for the branch.
	if err := bf.PutSecrets(context.Background(), "branchbranchbranchbr", map[string]string{"A": "1"}); err != nil {
		t.Fatal(err)
	}
	if ss, _ := st.ListFunctionSecrets(context.Background(), "branchbranchbranchbr"); len(ss) != 1 || ss[0].Name != "A" || string(ss[0].Sealed) == "1" {
		t.Fatalf("secrets = %+v", ss)
	}
	if ss, _ := st.ListFunctionSecrets(context.Background(), "parentparentparentpa"); len(ss) != 0 {
		t.Fatalf("parent secrets = %+v", ss)
	}
	_ = f
}

// The function API returns ezbr_sha256, equal for equal deployments.
func TestFunctionListGetAndDeployReturnEzbrSha256(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, slug := range []string{"one", "two"} {
		if err := f.srv.store.UpsertFunction(ctx, &Function{Ref: testRef, Slug: slug, Name: slug, Status: "ACTIVE", VerifyJWT: true, EntrypointPath: "index.ts"},
			[]FunctionFile{{Path: "index.ts", Content: []byte("body " + slug)}}); err != nil {
			t.Fatal(err)
		}
	}
	rec := f.do("GET", "/v1/projects/"+testRef+"/functions", nil)
	var list []map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list) != 2 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	digests := map[string]string{}
	for _, fn := range list {
		d, _ := fn["ezbr_sha256"].(string)
		if len(d) != 64 {
			t.Fatalf("function %v has ezbr_sha256 %q", fn["slug"], d)
		}
		digests[fn["slug"].(string)] = d
	}
	if digests["one"] == digests["two"] {
		t.Fatal("two different functions have one digest")
	}
	rec = f.do("GET", "/v1/projects/"+testRef+"/functions/one", nil)
	if rec.Code != http.StatusOK || jsonField(t, rec, "ezbr_sha256") != digests["one"] {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	// updated_at stays in epoch milliseconds, the form the CLI reads.
	if _, ok := jsonField(t, rec, "updated_at").(float64); !ok {
		t.Fatalf("updated_at = %v", jsonField(t, rec, "updated_at"))
	}
}

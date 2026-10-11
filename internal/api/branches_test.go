package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// branchEngine adds DeleteWith to the fake manager.
type branchEngine struct{ *fakeManager }

func (e branchEngine) DeleteWith(ctx context.Context, ref string, _ lifecycle.DeleteOptions) error {
	return e.Delete(ctx, ref)
}

// fakeBranchDB is the branching.Database of the API tests: an in-memory migration history.
type fakeBranchDB struct {
	mu   sync.Mutex
	migs map[string][]branching.Migration
	// applyErr makes Apply to that project fail.
	applyErr map[string]error
}

func newFakeBranchDB() *fakeBranchDB {
	return &fakeBranchDB{migs: map[string][]branching.Migration{}, applyErr: map[string]error{}}
}

func (d *fakeBranchDB) Migrations(_ context.Context, ref string) ([]branching.Migration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]branching.Migration(nil), d.migs[ref]...), nil
}

func (d *fakeBranchDB) Apply(_ context.Context, ref string, ms []branching.Migration, _ branching.ApplyOptions) (branching.ApplyResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.applyErr[ref]; err != nil {
		return branching.ApplyResult{}, err
	}
	d.migs[ref] = append(d.migs[ref], ms...)
	return branching.ApplyResult{Applied: len(ms)}, nil
}

func (d *fakeBranchDB) RunScript(context.Context, string, string) error { return nil }

func (f *fixture) waitBranch(t *testing.T, name, wantStatus string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := f.do("GET", "/v1/projects/"+testRef+"/branches/"+name, nil)
		// A reset recreates the branch: for a moment it is not there (404), which is not a failure.
		if rec.Code == 404 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if rec.Code != 200 {
			t.Fatalf("get branch %s: %d %s", name, rec.Code, rec.Body)
		}
		b := decodeBody(t, rec).(map[string]any)
		if b["status"] == wantStatus {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("branch %s is %v (%v), want %s", name, b["status"], b, wantStatus)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBranches(t *testing.T) {
	f := newFixture(t)
	f.bdb.migs[testRef] = []branching.Migration{{Version: "20260101000000", Name: "init", Statements: []string{"create table t (id int)"}}}

	// Create: the body of the CLI and of the MCP server's create_branch.
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{
		"branch_name": "feature/login", "is_default": false, "git_branch": "feat/login", "persistent": false, "with_data": false,
		"desired_instance_size": "micro", "notify_url": "https://example.com/hook",
	})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /v1/projects/{ref}/branches", rec.Body.Bytes())
	br := decodeBody(t, rec).(map[string]any)
	ref, id := br["project_ref"].(string), br["id"].(string)
	if br["name"] != "feature/login" || br["parent_project_ref"] != testRef || br["is_default"] != false || br["with_data"] != false || br["git_branch"] != "feat/login" {
		t.Fatalf("branch = %v", br)
	}
	got := f.waitBranch(t, "feature%2Flogin", "FUNCTIONS_DEPLOYED")
	if got["preview_project_status"] != "ACTIVE_HEALTHY" {
		t.Fatalf("branch = %v", got)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/branches/{name}", mustJSON(got))
	if m := f.bdb.migs[ref]; len(m) != 1 || m[0].Version != "20260101000000" {
		t.Fatalf("migrations replayed into the branch: %v", m)
	}

	f.run(t, []step{
		{key: "GET /v1/projects/{ref}/branches", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "0.name") != "main" || jsonField(t, rec, "0.is_default") != true || jsonField(t, rec, "0.project_ref") != testRef ||
				jsonField(t, rec, "1.name") != "feature/login" || jsonField(t, rec, "1.project_ref") != ref {
				t.Errorf("list = %s", rec.Body)
			}
		}},
		{key: "GET /v1/projects/{ref}/branches/{name}", path: "/v1/projects/" + testRef + "/branches/main", check: want("is_default", true)},
		{key: "GET /v1/projects/{ref}/branches/{name}", path: "/v1/projects/" + testRef + "/branches/nope", status: 404},
		{key: "GET /v1/branches/{branch_id_or_ref}", path: "/v1/branches/" + id, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "ref") != ref || jsonField(t, rec, "db_user") != "postgres."+ref || jsonField(t, rec, "status") != "ACTIVE_HEALTHY" ||
				jsonField(t, rec, "db_pass") == "" || jsonField(t, rec, "jwt_secret") == "" || jsonField(t, rec, "postgres_engine") != "17" {
				t.Errorf("detail = %s", rec.Body)
			}
		}},
		{key: "GET /v1/branches/{branch_id_or_ref}", path: "/v1/branches/" + ref, check: want("ref", ref)},
		{key: "GET /v1/branches/{branch_id_or_ref}", path: "/v1/branches/zzzzzzzzzzzzzzzzzzzz", status: 404},
		{key: "PATCH /v1/branches/{branch_id_or_ref}", path: "/v1/branches/" + id, body: map[string]any{"persistent": true, "git_branch": "feat/other"}, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "persistent") != true || jsonField(t, rec, "git_branch") != "feat/other" {
				t.Errorf("patch = %s", rec.Body)
			}
		}},
		{key: "PATCH /v1/branches/{branch_id_or_ref}", path: "/v1/branches/" + id, body: map[string]any{"branch_name": "no good"}, status: 400},
	})

	// The branch is hidden from the project lists, and the parent shows it.
	rec = f.do("GET", "/v1/projects", nil)
	if strings.Contains(rec.Body.String(), ref) {
		t.Fatalf("a branch is listed as a project: %s", rec.Body)
	}
	rec = f.do("GET", "/platform/projects", nil)
	if strings.Contains(rec.Body.String(), `"ref":"`+ref+`"`) || !strings.Contains(rec.Body.String(), `"preview_branch_refs":["`+ref+`"]`) || !strings.Contains(rec.Body.String(), `"is_branch_enabled":true`) {
		t.Fatalf("platform projects: %s", rec.Body)
	}
	rec = f.do("GET", "/platform/projects/"+ref, nil)
	if rec.Code != 200 || jsonField(t, rec, "parent_project_ref") != testRef {
		t.Fatalf("platform project of a branch: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/projects/{ref}", rec.Body.Bytes())

	// merge, push, reset: 201 with a run id; the branch status follows.
	f.bdb.migs[ref] = append(f.bdb.migs[ref], branching.Migration{Version: "20260110000000", Name: "feature", Statements: []string{"create table f (id int)"}})
	for _, op := range []string{"merge", "push", "reset"} {
		key := "POST /v1/branches/{branch_id_or_ref}/" + op
		rec := f.do("POST", "/v1/branches/"+id+"/"+op, map[string]any{})
		if rec.Code != 201 {
			t.Fatalf("%s: %d %s", op, rec.Code, rec.Body)
		}
		validateAgainstSpec(t, key, rec.Body.Bytes())
		if run := jsonField(t, rec, "workflow_run_id"); run == "" || !branching.IsUUID(run.(string)) {
			t.Fatalf("%s run id = %v", op, run)
		}
		f.waitBranch(t, "feature%2Flogin", "FUNCTIONS_DEPLOYED")
	}
	if m := f.bdb.migs[testRef]; len(m) != 2 || m[1].Version != "20260110000000" {
		t.Fatalf("merge did not reach the parent: %v", m)
	}
	// An empty body and an unknown branch.
	if rec := f.do("POST", "/v1/branches/"+id+"/merge", nil); rec.Code != 201 {
		t.Fatalf("merge without a body: %d %s", rec.Code, rec.Body)
	}
	f.waitBranch(t, "feature%2Flogin", "FUNCTIONS_DEPLOYED")
	if rec := f.do("POST", "/v1/branches/"+ref+"x/merge", nil); rec.Code != 404 {
		t.Fatalf("merge unknown: %d", rec.Code)
	}
	if rec := f.do("POST", "/v1/branches/"+testRef+"/merge", nil); rec.Code != 400 {
		t.Fatalf("merge of the default branch: %d %s", rec.Code, rec.Body)
	}

	rec = f.do("GET", "/v1/branches/"+id+"/diff", nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.Contains(rec.Body.String(), "nothing to merge") {
		t.Fatalf("diff: %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}

	// Soft delete and restore, then delete for real.
	rec = f.do("DELETE", "/v1/branches/"+id+"?force=false", nil)
	if rec.Code != 200 {
		t.Fatalf("soft delete: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "DELETE /v1/branches/{branch_id_or_ref}", rec.Body.Bytes())
	if b := f.waitBranch(t, "feature%2Flogin", "FUNCTIONS_DEPLOYED"); b["deletion_scheduled_at"] == nil {
		t.Fatalf("not scheduled: %v", b)
	}
	rec = f.do("POST", "/v1/branches/"+id+"/restore", nil)
	if rec.Code != 201 || jsonField(t, rec, "message") != "Branch restoration initiated" {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /v1/branches/{branch_id_or_ref}/restore", rec.Body.Bytes())
	if rec := f.do("DELETE", "/v1/branches/"+id, nil); rec.Code != 200 || jsonField(t, rec, "message") != "ok" {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/branches/"+id, nil); rec.Code != 404 {
		t.Fatalf("deleted branch: %d", rec.Code)
	}

	// Errors: duplicate names, a branch of a branch, a project that does not exist,
	// disabling branching.
	f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "dup"})
	f.waitBranch(t, "dup", "FUNCTIONS_DEPLOYED")
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "dup"}); rec.Code != 409 {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": ""}); rec.Code != 400 {
		t.Fatalf("empty name: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/branches", map[string]any{"branch_name": "x"}); rec.Code != 404 {
		t.Fatalf("unknown project: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/branches", nil); rec.Code != 404 {
		t.Fatalf("list of an unknown project: %d", rec.Code)
	}
	if rec := f.do("DELETE", "/v1/projects/"+testRef+"/branches", nil); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); len(decodeBody(t, rec).([]any)) != 1 {
		t.Fatalf("branches after disable: %s", rec.Body)
	}
	// The Management API shape of the error envelope.
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "main"}); rec.Code != 400 || jsonField(t, rec, "message") == "" {
		t.Fatalf("reserved name: %d %s", rec.Code, rec.Body)
	}
}

func TestBranchRequestsThatCannotBeHonoredAreRefused(t *testing.T) {
	f := newFixture(t)
	create := func(body map[string]any) *httptest.ResponseRecorder {
		body["branch_name"] = "x"
		return f.do("POST", "/v1/projects/"+testRef+"/branches", body)
	}
	for name, body := range map[string]map[string]any{
		"release channel":          {"release_channel": "preview"},
		"postgres engine":          {"postgres_engine": "15"},
		"a reserved secret":        {"secrets": map[string]string{"SUPABASE_URL": "x"}},
		"a bad secret name":        {"secrets": map[string]string{"1 bad": "x"}},
		"too many secrets":         {"secrets": manySecrets(maxBranchSecrets + 1)},
		"an unknown instance size": {"desired_instance_size": "gigantic"},
	} {
		if rec := create(body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); len(decodeBody(t, rec).([]any)) != 1 {
		t.Fatalf("a refused create left a branch: %s", rec.Body)
	}
	// Secrets, the node's own engine, the ga channel and a region are accepted (the region of the
	// branch is its parent's, and the answer says so).
	rec := create(map[string]any{"secrets": map[string]string{"STRIPE_KEY": "sk_test"}, "release_channel": "ga", "postgres_engine": "17", "region": "us-east-1"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if got := jsonField(t, rec, "supavise_region"); got == nil || got == "" {
		t.Fatalf("the answer does not say where the branch runs: %s", rec.Body)
	}
	br := f.waitBranch(t, "x", "FUNCTIONS_DEPLOYED")
	id := br["id"].(string)
	// The create-body secrets are the branch's own, and the parent's are not copied.
	if sec, err := f.srv.store.ListFunctionSecrets(context.Background(), br["project_ref"].(string)); err != nil || len(sec) != 1 || sec[0].Name != "STRIPE_KEY" {
		t.Fatalf("the branch's secrets = %v, %v", sec, err)
	}
	if rec := f.do("PATCH", "/v1/branches/"+id, map[string]any{"status": "MIGRATIONS_PASSED", "reset_on_push": true, "persistent": true}); rec.Code != 200 {
		t.Errorf("a status and the deprecated reset_on_push are accepted: %d %s", rec.Code, rec.Body)
	}
}

func manySecrets(n int) map[string]string {
	m := map[string]string{}
	for i := range n {
		m["S"+strings.Repeat("A", i%7)+itoa(int64(i))] = "v"
	}
	return m
}

// with_data on a node that cannot clone and has no backup is something the user can fix: 400.
func TestBranchWithDataWithoutAWayIs400(t *testing.T) {
	f := newFixture(t)
	f.cfg.Branching.Clone = "backup"
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "d", "with_data": true})
	if rec.Code != 400 || !strings.Contains(jsonField(t, rec, "message").(string), "with_data is not possible") {
		t.Fatalf("with_data: %d %s", rec.Code, rec.Body)
	}
}

// The detail of the default branch does not hand out the project's secrets, and a branch whose
// credentials are not stored yet answers without them instead of failing.
func TestBranchDetailSecrets(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/v1/branches/"+testRef, nil)
	if rec.Code != 200 || jsonField(t, rec, "ref") != testRef || jsonField(t, rec, "db_pass") != nil || jsonField(t, rec, "jwt_secret") != nil {
		t.Fatalf("default branch detail: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /v1/branches/{branch_id_or_ref}", rec.Body.Bytes())

	const ref = "wwwwwwwwwwwwwwwwwwww"
	if err := f.reg.CreateProject(context.Background(), &registry.Project{Ref: ref, Name: "w", Status: registry.StatusComingUp,
		Branch: &registry.BranchInfo{ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: testRef, Name: "w", State: registry.BranchCreatingProject}}); err != nil {
		t.Fatal(err)
	}
	rec = f.do("GET", "/v1/branches/"+ref, nil)
	if rec.Code != 200 || jsonField(t, rec, "ref") != ref || jsonField(t, rec, "db_pass") != nil {
		t.Fatalf("branch without credentials yet: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /v1/branches/{branch_id_or_ref}", rec.Body.Bytes())
}

func TestBranchesWithoutService(t *testing.T) {
	f := newFixture(t)
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.srv.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	f.ownerOn(srv)
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/branches/"+testRef, nil); rec.Code != 404 {
		t.Fatalf("get: %d", rec.Code)
	}
}

// The digest of a function follows its files and settings, not the project it is deployed to
// or its version counter, so a branch's copy of an unchanged function equals the parent's.
func TestFunctionDigests(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	deploy := func(ref, slug, body string, verify bool) {
		t.Helper()
		f := &Function{Ref: ref, Slug: slug, Name: slug, VerifyJWT: verify, EntrypointPath: "index.ts"}
		if err := st.UpsertFunction(ctx, f, []FunctionFile{{Path: "index.ts", Content: []byte(body)}, {Path: "lib/a.ts", Content: []byte("a")}}); err != nil {
			t.Fatal(err)
		}
	}
	deploy("parentparentparentpa", "hello", "v1", true)
	deploy("branchbranchbranchbr", "hello", "v1", true)
	deploy("branchbranchbranchbr", "other", "v1", true)
	deploy("parentparentparentpa", "edited", "v1", true)
	deploy("branchbranchbranchbr", "edited", "v2", true)
	deploy("parentparentparentpa", "flag", "v1", true)
	deploy("branchbranchbranchbr", "flag", "v1", false)
	src := FunctionDigests(st)
	p, err := src.Digests(ctx, "parentparentparentpa")
	if err != nil {
		t.Fatal(err)
	}
	b, err := src.Digests(ctx, "branchbranchbranchbr")
	if err != nil {
		t.Fatal(err)
	}
	if p["hello"] == "" || p["hello"] != b["hello"] {
		t.Errorf("the same function differs: %q %q", p["hello"], b["hello"])
	}
	if p["edited"] == b["edited"] || p["flag"] == b["flag"] {
		t.Error("changed source or settings must change the digest")
	}
	if _, ok := p["other"]; ok || b["other"] == "" {
		t.Errorf("other: parent %q branch %q", p["other"], b["other"])
	}
}

// A with_data create that the state disk cannot hold is refused before anything exists: 409 in
// the API's message envelope, whatever allow_egress says.
func TestBranchWithDataWithoutFreeDiskIs409(t *testing.T) {
	f := newFixture(t)
	f.cfg.Branching.DiskReserveMB = 1_000_000_000 // a reserve no disk has free
	for _, q := range []string{"", "?allow_egress=true"} {
		rec := f.do("POST", "/v1/projects/"+testRef+"/branches"+q, map[string]any{"branch_name": "d", "with_data": true})
		msg, _ := jsonField(t, rec, "message").(string)
		if rec.Code != 409 || !strings.Contains(msg, "not enough free disk") || !strings.Contains(msg, "disk_reserve_mb") || strings.Contains(msg, "branching:") {
			t.Fatalf("with_data on a full disk%s: %d %s", q, rec.Code, rec.Body)
		}
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); len(decodeBody(t, rec).([]any)) != 1 {
		t.Fatalf("a refused create left a branch: %s", rec.Body)
	}
	// A schema-only branch copies nothing and is not held back.
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "s"}); rec.Code != 201 {
		t.Fatalf("schema-only create: %d %s", rec.Code, rec.Body)
	}
}

// A branch reports its outbound network policy next to the spec's fields, which stay as they are.
func TestBranchJSONCarriesTheEgressPolicy(t *testing.T) {
	b := &branching.Branch{
		ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", Name: "d", Ref: "abcdefghijklmnopqrst", ParentRef: testRef, WithData: true,
		State: registry.BranchMigrationsPassed, ProjectStatus: registry.StatusActiveHealthy, Egress: registry.EgressDenied,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	out, err := branchJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["supavise_egress"] != "denied" || m["with_data"] != true || m["project_ref"] != "abcdefghijklmnopqrst" || m["status"] != "MIGRATIONS_PASSED" {
		t.Fatalf("branch JSON = %s", body)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/branches/{name}", body)
	// Schema-only and default branches have none.
	b.Egress = ""
	body, _ = json.Marshal(mustBranchJSON(t, b))
	if strings.Contains(string(body), "supavise_egress") {
		t.Fatalf("branch without a policy reports one: %s", body)
	}
}

func mustBranchJSON(t *testing.T, b *branching.Branch) *branchOut {
	t.Helper()
	out, err := branchJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A branch is governed by the roles of its parent project, whichever way a route names it (by
// id, by its own ref, or through the parent), and a project-scoped role does not stop at the
// parent's own ref or reach a branch of another project. Read-only members get no branch
// credentials.
func TestBranchRoutesFollowTheParentsRoles(t *testing.T) {
	rf := newRolesFixture(t)
	mk := func(parent, name string) (ref, id string) {
		rec := rf.status(201, "owner", "POST", "/v1/projects/"+parent+"/branches", map[string]any{"branch_name": name})
		b := decodeBody(t, rec).(map[string]any)
		ref, id = b["project_ref"].(string), b["id"].(string)
		deadline := time.Now().Add(5 * time.Second)
		for {
			p, err := rf.reg.GetProject(context.Background(), ref)
			if err == nil && p.Status == registry.StatusActiveHealthy && p.Branch != nil && p.Branch.State == registry.BranchFunctionsDeployed {
				return ref, id
			}
			if time.Now().After(deadline) {
				t.Fatalf("branch %s did not settle: %+v %v", name, p, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	ref, id := mk(testRef, "one")
	ref2, id2 := mk(secondRef, "two")

	for _, tc := range []struct {
		role      string
		see, keys bool // reads the branch of testRef; sees its db_pass
	}{
		{"owner", true, true}, {"admin", true, true}, {"dev", true, true}, {"scoped", true, true},
		{"ro", true, false}, {"stranger", false, false},
	} {
		for _, name := range []string{id, ref} {
			rec := rf.as(tc.role, "GET", "/v1/branches/"+name, nil)
			if !tc.see {
				if rec.Code != 403 {
					t.Errorf("%s reads branch %s: %d, want 403", tc.role, name, rec.Code)
				}
				continue
			}
			if rec.Code != 200 {
				t.Errorf("%s reads branch %s: %d %s", tc.role, name, rec.Code, rec.Body)
				continue
			}
			if has := jsonField(t, rec, "db_pass") != nil || jsonField(t, rec, "jwt_secret") != nil; has != tc.keys {
				t.Errorf("%s: branch credentials in the detail = %v, want %v", tc.role, has, tc.keys)
			}
		}
		if rec := rf.as(tc.role, "GET", "/v1/branches/"+id+"/diff", nil); (rec.Code == 403) == tc.see {
			t.Errorf("%s: diff of the branch: %d", tc.role, rec.Code)
		}
	}
	// The default branch is the project: same answer as the project's own routes.
	rf.status(403, "stranger", "GET", "/v1/branches/"+testRef, nil)
	rf.status(200, "scoped", "GET", "/v1/branches/"+testRef, nil)
	// The scoped Developer has testRef and its branches, not the other project's.
	for _, name := range []string{id2, ref2} {
		rf.status(403, "scoped", "GET", "/v1/branches/"+name, nil)
		rf.status(403, "scoped", "PATCH", "/v1/branches/"+name, map[string]any{"persistent": true})
		rf.status(403, "scoped", "DELETE", "/v1/branches/"+name, nil)
		rf.status(403, "scoped", "POST", "/v1/branches/"+name+"/push", map[string]any{})
	}
	rf.status(403, "scoped", "POST", "/v1/projects/"+secondRef+"/branches", map[string]any{"branch_name": "x"})
	rf.status(403, "scoped", "GET", "/v1/projects/"+ref2+"/api-keys", nil)
	if rec := rf.as("scoped", "GET", "/v1/projects/"+ref+"/api-keys", nil); rec.Code == 403 {
		t.Errorf("a role scoped to the parent is refused on its branch's project routes: %s", rec.Body)
	}
	// Writes need the branch permissions: Read-only and strangers are refused on every verb.
	for _, role := range []string{"ro", "stranger"} {
		rf.status(403, role, "PATCH", "/v1/branches/"+id, map[string]any{"persistent": true})
		rf.status(403, role, "DELETE", "/v1/branches/"+id, nil)
		for _, op := range []string{"push", "merge", "reset", "restore"} {
			rf.status(403, role, "POST", "/v1/branches/"+id+"/"+op, map[string]any{})
		}
	}
	// A branch that does not exist is a 404 for a caller who could use the permission.
	rf.status(404, "owner", "GET", "/v1/branches/zzzzzzzzzzzzzzzzzzzz", nil)
	rf.status(200, "scoped", "PATCH", "/v1/branches/"+id, map[string]any{"persistent": true})
}

func TestBranchCreateV2(t *testing.T) {
	f := newFixture(t)
	body := func(attrs map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"type": "branch", "attributes": attrs}}
	}
	rec := f.do("POST", "/v2/projects/"+testRef+"/branches", body(map[string]any{
		"name": "v2-branch", "git_branch": "feat/v2", "persistent": true, "desired_instance_size": "micro", "region": "eu-west-1",
		"notify_url": "https://example.com/hook",
	}))
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /v2/projects/{ref}/branches", rec.Body.Bytes())
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Fatal("served by the spec stub")
	}
	attrs := jsonField(t, rec, "data.attributes").(map[string]any)
	if jsonField(t, rec, "data.type") != "branch" || attrs["name"] != "v2-branch" || attrs["parent_project_ref"] != testRef || attrs["is_default"] != false ||
		attrs["persistent"] != true || attrs["git_branch"] != "feat/v2" || attrs["notify_url"] != "https://example.com/hook" || attrs["with_data"] != false {
		t.Fatalf("branch = %s", rec.Body)
	}
	// It is a real branch: v1 reads it, and its run is listed under it.
	got := f.waitBranch(t, "v2-branch", "FUNCTIONS_DEPLOYED")
	if got["project_ref"] != attrs["project_ref"] || got["id"] != jsonField(t, rec, "data.id") || got["persistent"] != true {
		t.Fatalf("v1 branch = %v", got)
	}
	runs := f.do("GET", "/v1/projects/"+attrs["project_ref"].(string)+"/actions", nil)
	if n := len(decodeBody(t, runs).([]any)); n != 1 {
		t.Fatalf("runs = %s", runs.Body)
	}
	// What this node cannot honor is refused with the reason; so is a body of another type.
	for name, b := range map[string]map[string]any{
		"a disk size":  body(map[string]any{"name": "d", "desired_disk_size_gb": 50}),
		"a wrong type": {"data": map[string]any{"type": "project", "attributes": map[string]any{"name": "d"}}},
		"no name":      body(map[string]any{"git_branch": "x"}),
		"a bad name":   body(map[string]any{"name": "no good"}),
	} {
		if rec := f.do("POST", "/v2/projects/"+testRef+"/branches", b); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.do("POST", "/v2/projects/"+testRef+"/branches", body(map[string]any{"name": "v2-branch"})); rec.Code != 409 {
		t.Errorf("a name in use: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v2/projects/zzzzzzzzzzzzzzzzzzzz/branches", body(map[string]any{"name": "x"})); rec.Code != 404 {
		t.Errorf("unknown project: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); len(decodeBody(t, rec).([]any)) != 2 {
		t.Fatalf("branches = %s", rec.Body)
	}
}

func TestBranchDefaultBranchRoutes(t *testing.T) {
	f := newFixture(t)
	create := func() *httptest.ResponseRecorder {
		return f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "main", "is_default": true, "git_branch": "production"})
	}
	// Studio's Production branch save: the default branch comes back, again and again, and nothing is created.
	for range 2 {
		rec := create()
		if rec.Code != 201 {
			t.Fatalf("create is_default: %d %s", rec.Code, rec.Body)
		}
		validateAgainstSpec(t, "POST /v1/projects/{ref}/branches", rec.Body.Bytes())
		if jsonField(t, rec, "is_default") != true || jsonField(t, rec, "project_ref") != testRef || jsonField(t, rec, "git_branch") != "production" {
			t.Fatalf("default branch = %s", rec.Body)
		}
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); len(decodeBody(t, rec).([]any)) != 1 {
		t.Fatalf("is_default created a branch: %s", rec.Body)
	}
	// PATCH on the default branch stores the git branch; the name Studio sends with it does not rename.
	rec := f.do("PATCH", "/v1/branches/"+testRef, map[string]any{"git_branch": "develop", "branch_name": "develop"})
	if rec.Code != 200 || jsonField(t, rec, "git_branch") != "develop" || jsonField(t, rec, "name") != "main" || jsonField(t, rec, "is_default") != true {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches/main", nil); jsonField(t, rec, "git_branch") != "develop" {
		t.Fatalf("get by name: %s", rec.Body)
	}
	if rec := f.do("PATCH", "/v1/branches/"+testRef, map[string]any{"git_branch": ""}); rec.Code != 200 || jsonField(t, rec, "git_branch") != nil {
		t.Fatalf("clearing the git branch: %d %s", rec.Code, rec.Body)
	}
	// A rename of its own and the other things a default branch cannot do are refused.
	for name, b := range map[string]map[string]any{
		"rename":         {"branch_name": "trunk"},
		"not persistent": {"persistent": false},
		"notify":         {"notify_url": "https://example.com/hook"},
		"review":         {"request_review": true},
	} {
		if rec := f.do("PATCH", "/v1/branches/"+testRef, b); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestBranchStatusRequestReviewAndFailureFields(t *testing.T) {
	f := newFixture(t)
	f.cfg.Branching.DefaultTTL = "7d" // a branch has no lifetime unless the node gives one ([branching] default_ttl)
	f.bdb.migs[testRef] = []branching.Migration{{Version: "20260101000000", Name: "init", Statements: []string{"create table t (id int)"}}}
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "feat"})
	ref, id := jsonField(t, rec, "project_ref").(string), jsonField(t, rec, "id").(string)
	got := f.waitBranch(t, "feat", "FUNCTIONS_DEPLOYED")
	// A ready branch says when it goes away and where it runs.
	if got["supavise_expires_at"] == nil || got["supavise_region"] == nil || !strings.Contains(got["supavise_detail"].(string), "1 migration(s) replayed") {
		t.Fatalf("branch = %v", got)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/branches/{name}", mustJSON(got))
	// A persistent branch has no expiry to show.
	if rec := f.do("PATCH", "/v1/branches/"+id, map[string]any{"persistent": true}); jsonField(t, rec, "supavise_expires_at") != nil {
		t.Fatalf("a persistent branch shows an expiry: %s", rec.Body)
	}
	f.do("PATCH", "/v1/branches/"+id, map[string]any{"persistent": false})

	// request_review true, then false (Studio's "Close merge request").
	if rec := f.do("PATCH", "/v1/branches/"+id, map[string]any{"request_review": true}); rec.Code != 200 || jsonField(t, rec, "review_requested_at") == nil {
		t.Fatalf("request review: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("PATCH", "/v1/branches/"+id, map[string]any{"request_review": false}); rec.Code != 200 || jsonField(t, rec, "review_requested_at") != nil {
		t.Fatalf("close the merge request: %d %s", rec.Code, rec.Body)
	}
	// A successful merge clears a request for review without being told to.
	f.do("PATCH", "/v1/branches/"+id, map[string]any{"request_review": true})
	f.bdb.migs[ref] = append(f.bdb.migs[ref], branching.Migration{Version: "20260110000000", Name: "feature", Statements: []string{"create table f (id int)"}})
	if rec := f.do("POST", "/v1/branches/"+id+"/merge", nil); rec.Code != 201 {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body)
	}
	if b := f.waitBranch(t, "feat", "FUNCTIONS_DEPLOYED"); b["review_requested_at"] != nil {
		t.Fatalf("the merge left the request for review: %v", b)
	}

	// A failing merge: the 201 is followed by a failed run, and the reason is on the branch.
	f.bdb.migs[ref] = append(f.bdb.migs[ref], branching.Migration{Version: "20260111000000", Name: "more", Statements: []string{"create table g (id int)"}})
	f.bdb.applyErr[testRef] = errBoom
	mrec := f.do("POST", "/v1/branches/"+id+"/merge", nil)
	if mrec.Code != 201 {
		t.Fatalf("merge: %d %s", mrec.Code, mrec.Body)
	}
	runID := jsonField(t, mrec, "workflow_run_id").(string)
	b := f.waitBranch(t, "feat", "MIGRATIONS_FAILED")
	if !strings.Contains(b["supavise_detail"].(string), "boom") {
		t.Fatalf("the reason is not on the branch: %v", b)
	}
	run := decodeBody(t, f.do("GET", "/v1/projects/"+testRef+"/actions/"+runID, nil))
	if got := stepStatuses(t, run); !strings.Contains(got, "migrate=DEAD") || !strings.Contains(got, "seed=PAUSED") || !strings.Contains(got, "deploy=PAUSED") {
		t.Fatalf("steps = %s", got)
	}
	logs := f.do("GET", "/v1/projects/"+testRef+"/actions/"+runID+"/logs", nil).Body.String()
	if !strings.Contains(logs, "FAILED") || !strings.Contains(logs, "boom") {
		t.Fatalf("log = %q", logs)
	}
	// The default branch reads as failed too until the next merge works.
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches/main", nil); jsonField(t, rec, "status") != "MIGRATIONS_FAILED" || !strings.Contains(jsonField(t, rec, "supavise_detail").(string), "boom") {
		t.Fatalf("default branch = %s", rec.Body)
	}

	// The status can be set to any of the six values the spec lists, and nothing else.
	for _, st := range []string{"CREATING_PROJECT", "RUNNING_MIGRATIONS", "MIGRATIONS_PASSED", "MIGRATIONS_FAILED", "FUNCTIONS_DEPLOYED", "FUNCTIONS_FAILED"} {
		if rec := f.do("PATCH", "/v1/branches/"+id, map[string]any{"status": st}); rec.Code != 200 || jsonField(t, rec, "status") != st {
			t.Fatalf("status %s: %d %s", st, rec.Code, rec.Body)
		}
	}
	if rec := f.do("PATCH", "/v1/branches/"+id, map[string]any{"status": "WOBBLY"}); rec.Code != 400 {
		t.Fatalf("an unknown status: %d %s", rec.Code, rec.Body)
	}
}

func TestBranchDeleteTellsTheFunctionsHook(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "gone"})
	ref, id := jsonField(t, rec, "project_ref").(string), jsonField(t, rec, "id").(string)
	f.waitBranch(t, "gone", "FUNCTIONS_DEPLOYED")
	hook := &recordingHook{}
	f.srv.fnHook = hook
	if rec := f.do("DELETE", "/v1/branches/"+id, nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if hook.count() != 1 || hook.calls[0] != ref {
		t.Fatalf("hook calls = %v, want [%s]", hook.calls, ref)
	}
	// Disabling branching removes every branch the same way.
	for _, n := range []string{"a", "b"} {
		f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": n})
		f.waitBranch(t, n, "FUNCTIONS_DEPLOYED")
	}
	refs, _ := f.srv.branches.Children(context.Background(), testRef)
	if len(refs) != 2 {
		t.Fatalf("children = %v", refs)
	}
	if rec := f.do("DELETE", "/v1/projects/"+testRef+"/branches", nil); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if hook.count() != 3 {
		t.Fatalf("hook calls after disabling branching = %v", hook.calls)
	}
}

var errBoom = errors.New("boom: relation g already exists")

// The failure reason, the expiry and the region sit next to the spec's fields, which stay as they are.
func TestBranchJSONCarriesDetailExpiryAndRegion(t *testing.T) {
	exp := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	b := &branching.Branch{
		ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", Name: "d", Ref: "abcdefghijklmnopqrst", ParentRef: testRef, Region: "eu-west-1",
		State: registry.BranchMigrationsFailed, ProjectStatus: registry.StatusActiveHealthy, Detail: "apply to the parent: relation f already exists",
		ExpiresAt: &exp, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	body, err := json.Marshal(mustBranchJSON(t, b))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["supavise_detail"] != "apply to the parent: relation f already exists" || m["supavise_expires_at"] != "2026-11-01T12:00:00Z" ||
		m["supavise_region"] != "eu-west-1" || m["status"] != "MIGRATIONS_FAILED" {
		t.Fatalf("branch JSON = %s", body)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/branches/{name}", body)
	// Nothing to say: the fields are absent. A persistent branch never shows an expiry.
	b.Detail, b.Region, b.State = "", "", registry.BranchFunctionsDeployed
	b.Persistent = true
	body, _ = json.Marshal(mustBranchJSON(t, b))
	for _, k := range []string{"supavise_detail", "supavise_expires_at", "supavise_region"} {
		if strings.Contains(string(body), k) {
			t.Errorf("%s in %s", k, body)
		}
	}
}

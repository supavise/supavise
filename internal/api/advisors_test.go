package api

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

// The SQL lints the fake database returns: a table without RLS (security, error), an unused
// index (performance, info) and a bucket whose metadata Studio links on.
const fakeSQLLints = `[
 {"name":"rls_disabled_in_public","title":"RLS Disabled in Public","level":"ERROR","facing":"EXTERNAL","categories":["SECURITY"],
  "description":"Detects tables in the public schema without row level security.","detail":"Table public.notes is public, but RLS has not been enabled.",
  "remediation":"https://supabase.com/docs/guides/database/database-linter?lint=0013_rls_disabled_in_public",
  "metadata":{"schema":"public","name":"notes","type":"table"},"cache_key":"rls_disabled_in_public_public_notes"},
 {"name":"unused_index","title":"Unused Index","level":"INFO","facing":"EXTERNAL","categories":["PERFORMANCE"],
  "description":"Detects indexes never used.","detail":"Index notes_idx on table public.notes has not been used",
  "remediation":"https://supabase.com/docs/guides/database/database-linter?lint=0005_unused_index",
  "metadata":{"schema":"public","name":"notes","type":"table"},"cache_key":"unused_index_public_notes_notes_idx"},
 {"name":"public_bucket_allows_listing","title":"Public Bucket Allows Listing","level":"WARN","facing":"EXTERNAL","categories":["SECURITY"],
  "description":"d","detail":"x","remediation":"https://supabase.com/docs/guides/database/database-linter?lint=0025_public_bucket_allows_listing",
  "metadata":{"bucket_id":"avatars","name":"avatars","schema":"storage","type":"table"},"cache_key":"public_bucket_allows_listing_avatars"}
]`

const lintMarker = "-- user: self host"

// advisorsFixture is a fixture whose database answers the lint SQL with fakeSQLLints.
func advisorsFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.meta.Rules = []pgmetaRule{{Contains: lintMarker, Status: 200, Body: fakeSQLLints}}
	return f
}

func (f *fixture) lintRuns() int {
	f.meta.mu.Lock()
	defer f.meta.mu.Unlock()
	n := 0
	for _, r := range f.meta.Requests {
		if strings.Contains(r.Body, lintMarker) {
			n++
		}
	}
	return n
}

func (f *fixture) lintSQL(i int) string {
	f.meta.mu.Lock()
	defer f.meta.mu.Unlock()
	n := 0
	for _, r := range f.meta.Requests {
		if strings.Contains(r.Body, lintMarker) {
			if n == i {
				var in struct{ Query string }
				_ = json.Unmarshal([]byte(r.Body), &in)
				return in.Query
			}
			n++
		}
	}
	return ""
}

type lintObj = map[string]any

func lintList(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) []lintObj {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status %d, want %d: %s", rec.Code, wantStatus, truncate(rec.Body.String(), 400))
	}
	var raw any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rec.Body)
	}
	var list []any
	switch x := raw.(type) {
	case []any:
		list = x
	case map[string]any:
		if d, ok := x["data"].(map[string]any); ok { // v2
			list = d["attributes"].(map[string]any)["lints"].([]any)
		} else {
			list = x["lints"].([]any)
		}
	}
	out := make([]lintObj, len(list))
	for i, l := range list {
		out[i] = l.(map[string]any)
	}
	return out
}

func lintNames(ls []lintObj) []string {
	n := make([]string, len(ls))
	for i, l := range ls {
		n[i], _ = l["name"].(string)
	}
	return n
}

func find(ls []lintObj, name string) lintObj {
	for _, l := range ls {
		if l["name"] == name {
			return l
		}
	}
	return nil
}

func TestRunLintsReturnsTheSQLLintsThenTheConfigurationLints(t *testing.T) {
	f := advisorsFixture(t)
	rec := f.do("GET", "/platform/projects/"+testRef+"/run-lints", nil)
	validateAgainstSpec(t, "GET /platform/projects/{ref}/run-lints", rec.Body.Bytes())
	ls := lintList(t, rec, 200)
	want := []string{"rls_disabled_in_public", "unused_index", "public_bucket_allows_listing",
		"auth_leaked_password_protection", "auth_insufficient_mfa_options", "auth_otp_long_expiry", "auth_db_connections_absolute"}
	if got := lintNames(ls); !slices.Equal(got, want) {
		t.Fatalf("lints %v, want %v", got, want)
	}
	// The row is passed on as the database made it: Studio links the bucket by metadata.bucket_id.
	if md := ls[2]["metadata"].(map[string]any); md["bucket_id"] != "avatars" {
		t.Errorf("metadata.bucket_id lost: %v", md)
	}
	if ls[0]["cache_key"] != "rls_disabled_in_public_public_notes" || ls[0]["level"] != "ERROR" {
		t.Errorf("first lint: %v", ls[0])
	}
}

func TestRunLintsAsksTheDatabaseTheWayStudioDoes(t *testing.T) {
	f := advisorsFixture(t)
	f.do("GET", "/platform/projects/"+testRef+"/run-lints", nil)
	if f.lintRuns() != 1 {
		t.Fatalf("%d lint runs", f.lintRuns())
	}
	q := f.lintSQL(0)
	for _, want := range []string{
		"set local pg_stat_statements.track = none;",
		"set local statement_timeout = '30s';",
		"set local pgrst.db_schemas = 'public,graphql_public';", // the project's PostgREST default
		"-- source: dashboard", "set local search_path = '';", "rls_disabled_in_public",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("the SQL sent lacks %q", want)
		}
	}
	// The exposed schemas follow the project's PostgREST settings.
	if rec := f.do("PATCH", "/v1/projects/"+testRef+"/postgrest", map[string]any{"db_schema": "public,api,graphql_public"}); rec.Code != 200 {
		t.Fatalf("patch postgrest: %d %s", rec.Code, rec.Body)
	}
	f.srv.advisorRunner.Invalidate(testRef)
	f.do("GET", "/platform/projects/"+testRef+"/run-lints", nil)
	if q := f.lintSQL(1); !strings.Contains(q, "set local pgrst.db_schemas = 'public,api,graphql_public';") {
		t.Errorf("exposed schemas not taken from PostgREST's settings: %.400s", q)
	}
}

func TestAdvisorsShareOneRunAndStartAgainAfterAChange(t *testing.T) {
	f := advisorsFixture(t)
	p := "/platform/projects/" + testRef + "/run-lints"
	for i := 0; i < 3; i++ {
		f.do("GET", p, nil)
		f.do("GET", "/v1/projects/"+testRef+"/advisors/security", nil)
		f.do("GET", "/v1/projects/"+testRef+"/advisors/performance", nil)
	}
	if n := f.lintRuns(); n != 1 {
		t.Fatalf("%d lint runs for nine requests: the surfaces must share one", n)
	}
	// Changing the schema through the API makes the next call look again: a statement of the
	// Management API, a migration, and a change through pg-meta's own routes.
	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"database/query", "POST", "/v1/projects/" + testRef + "/database/query", map[string]any{"query": "alter table notes enable row level security"}},
		{"migration", "POST", "/v1/projects/" + testRef + "/database/migrations", map[string]any{"query": "create table t(id int)", "name": "t"}},
		{"pg-meta query", "POST", "/platform/pg-meta/" + testRef + "/query", map[string]any{"query": "ALTER TABLE public.notes ENABLE ROW LEVEL SECURITY"}},
	} {
		before := f.lintRuns()
		if rec := f.do(c.method, c.path, c.body); rec.Code >= 300 {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		f.do("GET", p, nil)
		if f.lintRuns() != before+1 {
			t.Errorf("%s: the advisors did not look again", c.name)
		}
	}
	// A read through the SQL editor's route, a read-only statement and a write of rows change no schema.
	before := f.lintRuns()
	f.do("POST", "/platform/pg-meta/"+testRef+"/query", map[string]any{"query": "select 1"})
	f.do("POST", "/v1/projects/"+testRef+"/database/query/read-only", map[string]any{"query": "select 1"})
	f.do("POST", "/v1/projects/"+testRef+"/database/query", map[string]any{"query": "insert into notes (id) values (1)"})
	f.do("GET", p, nil)
	if f.lintRuns() != before {
		t.Error("a read emptied the advisors' cache")
	}
}

func TestAdvisorsOfACategory(t *testing.T) {
	f := advisorsFixture(t)
	sec := lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/security", nil), 200)
	if got, want := lintNames(sec), []string{"rls_disabled_in_public", "public_bucket_allows_listing", "auth_leaked_password_protection", "auth_insufficient_mfa_options", "auth_otp_long_expiry"}; !slices.Equal(got, want) {
		t.Errorf("security: %v, want %v", got, want)
	}
	perf := lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/performance", nil), 200)
	if got, want := lintNames(perf), []string{"unused_index", "auth_db_connections_absolute"}; !slices.Equal(got, want) {
		t.Errorf("performance: %v, want %v", got, want)
	}
	// lint_type=sql leaves out what is computed from the project's configuration.
	sql := lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/security?lint_type=sql", nil), 200)
	if got, want := lintNames(sql), []string{"rls_disabled_in_public", "public_bucket_allows_listing"}; !slices.Equal(got, want) {
		t.Errorf("security, sql: %v, want %v", got, want)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/advisors/security?lint_type=other", nil); rec.Code != 400 {
		t.Errorf("lint_type=other: %d", rec.Code)
	}
	// Both endpoints answer an object with a list, empty when nothing is wrong.
	f2 := newFixture(t) // the fake database finds nothing
	f2.meta.Rules = []pgmetaRule{{Contains: lintMarker, Status: 200, Body: `[]`}}
	f2.do("PATCH", "/v1/projects/"+testRef+"/config/auth", map[string]any{"password_hibp_enabled": true, "mfa_phone_enroll_enabled": true, "mfa_phone_verify_enabled": true,
		"mailer_otp_exp": 3600, "db_max_pool_size": 20, "db_max_pool_size_unit": "percent"})
	for _, p := range []string{"security", "performance"} {
		rec := f2.do("GET", "/v1/projects/"+testRef+"/advisors/"+p, nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"lints":[]}` {
			t.Errorf("%s of a clean project: %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/advisors/security", nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}

func TestConfigurationLintsFollowTheAuthSettings(t *testing.T) {
	f := advisorsFixture(t)
	sec := func() []string {
		return lintNames(lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/security?lint_type=", nil), 200))
	}
	if got := sec(); !slices.Contains(got, "auth_leaked_password_protection") || !slices.Contains(got, "auth_otp_long_expiry") || !slices.Contains(got, "auth_insufficient_mfa_options") {
		t.Fatalf("defaults: %v", got)
	}
	patch := func(m map[string]any) {
		t.Helper()
		if rec := f.do("PATCH", "/v1/projects/"+testRef+"/config/auth", m); rec.Code != 200 {
			t.Fatalf("patch %v: %d %s", m, rec.Code, rec.Body)
		}
	}
	patch(map[string]any{"password_hibp_enabled": true})
	if slices.Contains(sec(), "auth_leaked_password_protection") {
		t.Error("leaked password protection is on but still reported")
	}
	patch(map[string]any{"mailer_otp_exp": 3600})
	if slices.Contains(sec(), "auth_otp_long_expiry") {
		t.Error("an hour is the limit, not past it")
	}
	patch(map[string]any{"mailer_otp_exp": 3601})
	if !slices.Contains(sec(), "auth_otp_long_expiry") {
		t.Error("an expiry past an hour is not reported")
	}
	patch(map[string]any{"mfa_phone_enroll_enabled": true, "mfa_phone_verify_enabled": true})
	if slices.Contains(sec(), "auth_insufficient_mfa_options") {
		t.Error("two MFA methods are on but the lint is reported")
	}
	// Enrolling without verifying is not an option users can use.
	patch(map[string]any{"mfa_phone_verify_enabled": false})
	if !slices.Contains(sec(), "auth_insufficient_mfa_options") {
		t.Error("a method that cannot verify counts as an option")
	}
	patch(map[string]any{"db_max_pool_size": 7})
	perf := lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/performance", nil), 200)
	if l := find(perf, "auth_db_connections_absolute"); l == nil || !strings.Contains(l["detail"].(string), "at most 7 connections") {
		t.Errorf("auth_db_connections_absolute: %v", l)
	}
}

func TestAdvisorErrors(t *testing.T) {
	get := func(f *fixture) *httptest.ResponseRecorder {
		return f.do("GET", "/platform/projects/"+testRef+"/run-lints", nil)
	}
	t.Run("a SQL error is the server's, not the caller's", func(t *testing.T) {
		f := newFixture(t)
		f.meta.Rules = []pgmetaRule{{Contains: lintMarker, Status: 400, Body: `{"error":"permission denied for table x"}`}}
		rec := get(f)
		if rec.Code != 500 || !strings.Contains(rec.Body.String(), "permission denied for table x") {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("the statement timeout is a 504", func(t *testing.T) {
		f := newFixture(t)
		f.meta.Rules = []pgmetaRule{{Contains: lintMarker, Status: 400, Body: `{"error":"canceling statement due to statement timeout"}`}}
		rec := get(f)
		if rec.Code != 504 || !strings.Contains(rec.Body.String(), "30 seconds") {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("pg-meta failing is a gateway error", func(t *testing.T) {
		f := newFixture(t)
		f.meta.Rules = []pgmetaRule{{Contains: lintMarker, Status: 500, Body: `{"error":"boom"}`}}
		if rec := get(f); rec.Code != 502 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("rows that are not lints", func(t *testing.T) {
		f := newFixture(t)
		f.meta.Rules = []pgmetaRule{{Contains: lintMarker, Status: 200, Body: `[{"?column?":1}]`}}
		if rec := get(f); rec.Code != 500 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a failure is not remembered", func(t *testing.T) {
		f := newFixture(t)
		f.meta.Rules = []pgmetaRule{{Contains: lintMarker, Status: 500, Body: `{"error":"boom"}`, Skip: 0}}
		get(f)
		f.meta.Rules = []pgmetaRule{{Contains: lintMarker, Status: 200, Body: fakeSQLLints}}
		if rec := get(f); rec.Code != 200 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a project that is not running", func(t *testing.T) {
		f := advisorsFixture(t)
		const paused = "bbbbbbbbbbbbbbbbbbbb"
		f.mgr.addProject(t, paused, "Paused", f.org.ID, registry.StatusInactive)
		for _, p := range []string{"/platform/projects/" + paused + "/run-lints", "/v1/projects/" + paused + "/advisors/security"} {
			if rec := f.do("GET", p, nil); rec.Code != 409 {
				t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
			}
		}
	})
	t.Run("an unknown project", func(t *testing.T) {
		f := advisorsFixture(t)
		for _, p := range []string{"/platform/projects/zzzzzzzzzzzzzzzzzzzz/run-lints", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/advisors/performance"} {
			if rec := f.do("GET", p, nil); rec.Code != 404 {
				t.Errorf("%s: %d", p, rec.Code)
			}
		}
	})
}

func v2Body(names ...string) map[string]any {
	ls := make([]map[string]string, len(names))
	for i, n := range names {
		ls[i] = map[string]string{"name": n}
	}
	return map[string]any{"data": map[string]any{"type": "project_advisors", "attributes": map[string]any{"lints": ls}}}
}

func TestAdvisorsRunV2(t *testing.T) {
	f := advisorsFixture(t)
	p := "/v2/projects/" + testRef + "/advisors/run"

	// SQL lints and configuration lints are found by name, whatever else exists.
	rec := f.do("POST", p, v2Body("rls_disabled_in_public", "auth_leaked_password_protection", "unused_index"))
	validateAgainstSpec(t, "POST /v2/projects/{ref}/advisors/run", rec.Body.Bytes())
	ls := lintList(t, rec, 200)
	if got, want := lintNames(ls), []string{"rls_disabled_in_public", "unused_index", "auth_leaked_password_protection"}; !slices.Equal(got, want) {
		t.Errorf("by name: %v, want %v", got, want)
	}
	var env struct{ Data struct{ Type string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Data.Type != "project_advisors" {
		t.Errorf("data.type %q", env.Data.Type)
	}

	// A name with no finding gives nothing, without running SQL that cannot help.
	before := f.lintRuns()
	rec = f.do("POST", p, v2Body("auth_leaked_password_protection", "ssl_not_enforced"))
	if got := lintNames(lintList(t, rec, 200)); !slices.Equal(got, []string{"auth_leaked_password_protection"}) {
		t.Errorf("config only: %v", got)
	}
	if f.lintRuns() != before {
		t.Error("a request for configuration lints ran the lint SQL")
	}

	// Health checks Supavise cannot run say so, and the lint the clients drop.
	ls = lintList(t, f.do("POST", p, v2Body("log_data_api_error_rate_high", "log_auth_error_rate_high", "instance_alert_firing")), 200)
	if len(ls) != 3 {
		t.Fatalf("health: %v", lintNames(ls))
	}
	for _, l := range ls {
		if l["name"] != "advisor_check_unavailable" || l["level"] != "INFO" || l["categories"].([]any)[0] != "HEALTH" {
			t.Errorf("health answer %v", l)
		}
	}
	if ls[0]["cache_key"] != "advisor_check_unavailable:logs_unavailable" || ls[2]["cache_key"] != "advisor_check_unavailable:metrics_unavailable" {
		t.Errorf("cache keys %v %v", ls[0]["cache_key"], ls[2]["cache_key"])
	}
	// The same name twice is asked once.
	if got := lintNames(lintList(t, f.do("POST", p, v2Body("unused_index", "unused_index")), 200)); len(got) != 1 {
		t.Errorf("duplicate names: %v", got)
	}

	// A project that is not running answers project_not_active.
	const paused = "bbbbbbbbbbbbbbbbbbbb"
	f.mgr.addProject(t, paused, "Paused", f.org.ID, registry.StatusInactive)
	rec = f.do("POST", "/v2/projects/"+paused+"/advisors/run", v2Body("unused_index"))
	validateAgainstSpec(t, "POST /v2/projects/{ref}/advisors/run", rec.Body.Bytes())
	if got := lintNames(lintList(t, rec, 200)); !slices.Equal(got, []string{"project_not_active"}) {
		t.Errorf("paused: %v", got)
	}

	// The body is checked as the spec says: 1 to 10 lints, well-formed names.
	for name, body := range map[string]any{
		"none":       v2Body(),
		"eleven":     v2Body("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"),
		"wrong type": map[string]any{"data": map[string]any{"type": "x", "attributes": map[string]any{"lints": []map[string]string{{"name": "unused_index"}}}}},
		"bad name":   v2Body("Robert'); drop table"),
	} {
		if rec := f.do("POST", p, body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.do("POST", "/v2/projects/zzzzzzzzzzzzzzzzzzzz/advisors/run", v2Body("unused_index")); rec.Code != 404 {
		t.Errorf("unknown project: %d", rec.Code)
	}
}

func TestAdvisorRulesCRUD(t *testing.T) {
	f := advisorsFixture(t)
	base := "/platform/projects/" + testRef + "/notifications/advisor/exceptions"
	rules := func() []lintObj {
		rec := f.do("GET", base, nil)
		validateAgainstSpec(t, "GET /platform/projects/{ref}/notifications/advisor/exceptions", rec.Body.Bytes())
		var out struct{ Exceptions []lintObj }
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		return out.Exceptions
	}
	if got := rules(); got == nil || len(got) != 0 {
		t.Fatalf("a new project has rules: %v", got)
	}
	rec := f.do("POST", base, map[string]any{"exceptions": []map[string]any{{"lint_name": "rls_disabled_in_public", "is_disabled": true}}})
	validateAgainstSpec(t, "POST /platform/projects/{ref}/notifications/advisor/exceptions", rec.Body.Bytes())
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	got := rules()
	if len(got) != 1 {
		t.Fatalf("rules %v", got)
	}
	r := got[0]
	if r["lint_name"] != "rls_disabled_in_public" || r["is_disabled"] != true || r["project_ref"] != testRef || r["last_updated_by"] != f.userID ||
		r["assigned_to"] != nil || r["lint_category"] != nil || r["note"] != nil || r["lint_metadata"] != nil {
		t.Errorf("rule %v", r)
	}
	id := r["id"].(string)

	// Studio saves a rule and then reads the lints: the rule has taken effect.
	sec := lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/security", nil), 200)
	if find(sec, "rls_disabled_in_public") != nil {
		t.Error("a lint a rule disables is still reported")
	}
	if find(sec, "public_bucket_allows_listing") == nil {
		t.Error("a rule for one lint hid another")
	}
	all := lintList(t, f.do("GET", "/platform/projects/"+testRef+"/run-lints", nil), 200)
	if find(all, "rls_disabled_in_public") != nil {
		t.Error("run-lints still reports it")
	}

	// Enabling the rule again (the page's Enable button) brings the lint back.
	if rec := f.do("PATCH", base+"/"+id, map[string]any{"is_disabled": false, "note": "back"}); rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if r := rules()[0]; r["is_disabled"] != false || r["note"] != "back" || r["lint_name"] != "rls_disabled_in_public" {
		t.Errorf("patched rule %v", r)
	}
	if find(lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/security", nil), 200), "rls_disabled_in_public") == nil {
		t.Error("an enabled rule still hides the lint")
	}

	// A rule for one member hides the lint from that member only: the caller here.
	other := "99999999-2222-4333-8444-555555555555"
	f.do("POST", base, map[string]any{"exceptions": []map[string]any{{"lint_name": "unused_index", "assigned_to": other, "is_disabled": true}}})
	if find(lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/performance", nil), 200), "unused_index") == nil {
		t.Error("a rule for another member hid the lint from the caller")
	}
	f.do("POST", base, map[string]any{"exceptions": []map[string]any{{"lint_name": "unused_index", "assigned_to": f.userID, "is_disabled": true}}})
	if find(lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/performance", nil), 200), "unused_index") != nil {
		t.Error("a rule for the caller did not hide the lint")
	}
	// A rule by category and metadata.
	f.do("POST", base, map[string]any{"exceptions": []map[string]any{{"lint_category": "SECURITY", "lint_metadata": map[string]any{"name": "avatars"}, "is_disabled": true}}})
	if find(lintList(t, f.do("GET", "/v1/projects/"+testRef+"/advisors/security", nil), 200), "public_bucket_allows_listing") != nil {
		t.Error("a rule by metadata did not hide the lint")
	}

	// Delete by ids, as Studio sends them.
	list := rules()
	if len(list) != 4 {
		t.Fatalf("rules %d", len(list))
	}
	ids := list[0]["id"].(string) + "," + list[1]["id"].(string)
	if rec := f.do("DELETE", base+"?ids="+ids, nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if left := rules(); len(left) != 2 || left[0]["id"] != list[2]["id"] {
		t.Errorf("after delete: %v", left)
	}

	// What is refused.
	for name, c := range map[string]struct {
		method, path string
		body         any
		want         int
	}{
		"no rules":            {"POST", base, map[string]any{"exceptions": []any{}}, 400},
		"no is_disabled":      {"POST", base, map[string]any{"exceptions": []map[string]any{{"lint_name": "unused_index"}}}, 400},
		"bad category":        {"POST", base, map[string]any{"exceptions": []map[string]any{{"lint_category": "HEALTH", "is_disabled": true}}}, 400},
		"bad assignee":        {"POST", base, map[string]any{"exceptions": []map[string]any{{"assigned_to": "me", "is_disabled": true}}}, 400},
		"bad lint name":       {"POST", base, map[string]any{"exceptions": []map[string]any{{"lint_name": "x y", "is_disabled": true}}}, 400},
		"long note":           {"POST", base, map[string]any{"exceptions": []map[string]any{{"note": strings.Repeat("x", 2001), "is_disabled": true}}}, 400},
		"delete without ids":  {"DELETE", base, nil, 400},
		"delete with bad ids": {"DELETE", base + "?ids=1,2", nil, 400},
		"patch bad id":        {"PATCH", base + "/nope", map[string]any{"is_disabled": true}, 400},
		"patch missing":       {"PATCH", base + "/aaaaaaaa-2222-4333-8444-555555555555", map[string]any{"is_disabled": true}, 404},
		"patch bad category":  {"PATCH", base + "/" + id, map[string]any{"lint_category": "x"}, 400},
		"unknown project":     {"GET", "/platform/projects/zzzzzzzzzzzzzzzzzzzz/notifications/advisor/exceptions", nil, 404},
		"create for unknown":  {"POST", "/platform/projects/zzzzzzzzzzzzzzzzzzzz/notifications/advisor/exceptions", map[string]any{"exceptions": []map[string]any{{"is_disabled": true}}}, 404},
	} {
		if rec := f.do(c.method, c.path, c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, c.want)
		}
	}
}

func TestAdvisorRulesStayWithTheirProject(t *testing.T) {
	f := advisorsFixture(t)
	const other = "bbbbbbbbbbbbbbbbbbbb"
	f.mgr.addProject(t, other, "Other", f.org.ID, registry.StatusActiveHealthy)
	f.do("POST", "/platform/projects/"+testRef+"/notifications/advisor/exceptions", map[string]any{"exceptions": []map[string]any{{"lint_name": "rls_disabled_in_public", "is_disabled": true}}})
	if find(lintList(t, f.do("GET", "/platform/projects/"+other+"/run-lints", nil), 200), "rls_disabled_in_public") == nil {
		t.Error("a rule of one project hid a lint of another")
	}
	var out struct{ Exceptions []lintObj }
	rec := f.do("GET", "/platform/projects/"+other+"/notifications/advisor/exceptions", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Exceptions) != 0 {
		t.Errorf("rules of another project: %v", out.Exceptions)
	}
}

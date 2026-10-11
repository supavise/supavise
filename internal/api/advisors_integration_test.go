package api

// Integration test of the Security and Performance Advisors against a real Postgres and a real
// supavise-pgmeta (see integration_test.go for the variables): the vendored lint SQL must run
// through pg-meta, return the lints of a table without RLS, and follow the project's changes.

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// supabaseLikeSetup gives the empty database what the lints expect of a Supabase project: the API
// roles and the schemas Auth and Storage keep their tables in.
const supabaseLikeSetup = `
create role anon nologin;
create role authenticated nologin;
create role service_role nologin bypassrls;
create schema if not exists extensions;
create schema if not exists auth;
create table auth.users (id uuid primary key, email text);
create function auth.uid() returns uuid language sql stable as $$ select nullif(current_setting('request.jwt.claim.sub', true), '')::uuid $$;
create function auth.jwt() returns jsonb language sql stable as $$ select '{}'::jsonb $$;
create schema if not exists storage;
create table storage.buckets (id text primary key, name text not null, public boolean default false);
create table storage.objects (id uuid primary key default gen_random_uuid(), bucket_id text references storage.buckets (id), name text);
grant usage on schema public to anon, authenticated, service_role;
-- what a Supabase project grants on the tables its owner creates in public
alter default privileges for role postgres in schema public grant all on tables to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant all on functions to anon, authenticated, service_role;
`

func TestIntegrationAdvisors(t *testing.T) {
	s := startStack(t)
	p := "/v1/projects/" + s.Ref
	pp := "/platform/projects/" + s.Ref
	sql := func(q string) {
		t.Helper()
		if code, b := s.call("POST", p+"/database/query", map[string]any{"query": q}, false); code != 201 {
			t.Fatalf("%s: %d %s", q, code, b)
		}
	}
	lints := func(path string, jwt bool) []lintObj {
		t.Helper()
		code, b := s.call("GET", path, nil, jwt)
		rec := httptest.NewRecorder()
		rec.WriteHeader(code)
		_, _ = rec.Write(b)
		return lintList(t, rec, 200)
	}
	byName := func(ls []lintObj, name string) []lintObj {
		var out []lintObj
		for _, l := range ls {
			if l["name"] == name {
				out = append(out, l)
			}
		}
		return out
	}

	sql(supabaseLikeSetup)
	sql(`
create table public.notes (id bigint primary key, author uuid references auth.users (id), body text);
create table public.log (message text);
create index notes_body_a on public.notes (body);
create index notes_body_b on public.notes (body);
create view public.notes_view as select * from public.notes;
create function public.touch() returns void language sql security definer as 'select 1';
create schema private;
create table private.secrets (id bigint primary key, v text);
`)

	// Security: the table without RLS is an error, found through the Management API.
	sec := lints(p+"/advisors/security", false)
	rls := byName(sec, "rls_disabled_in_public")
	if len(rls) != 2 { // public.notes and public.log; the view, storage and auth tables are not findings of this lint
		t.Fatalf("rls_disabled_in_public: %d findings, want 2: %v", len(rls), lintNames(sec))
	}
	var notes lintObj
	for _, l := range rls {
		if l["metadata"].(map[string]any)["name"] == "notes" {
			notes = l
		}
	}
	if notes == nil {
		t.Fatalf("no finding for public.notes: %v", rls)
	}
	md := notes["metadata"].(map[string]any)
	if notes["level"] != "ERROR" || notes["facing"] != "EXTERNAL" || md["schema"] != "public" || md["type"] != "table" ||
		notes["cache_key"] != "rls_disabled_in_public_public_notes" || notes["categories"].([]any)[0] != "SECURITY" ||
		!strings.HasPrefix(notes["remediation"].(string), "https://supabase.com/docs/guides/database/database-linter?lint=0013") {
		t.Errorf("finding: %v", notes)
	}
	if len(byName(sec, "security_definer_view")) != 1 || len(byName(sec, "function_search_path_mutable")) != 1 {
		t.Errorf("security findings: %v", lintNames(sec))
	}
	// The schema PostgREST does not expose is left alone.
	for _, l := range byName(sec, "rls_disabled_in_public") {
		if l["metadata"].(map[string]any)["schema"] == "private" {
			t.Errorf("a schema the Data API does not expose was reported: %v", l)
		}
	}
	// The lints computed from the Auth settings come with them.
	if len(byName(sec, "auth_otp_long_expiry")) != 1 {
		t.Errorf("no configuration lint: %v", lintNames(sec))
	}

	// Performance.
	perf := lints(p+"/advisors/performance", false)
	for _, want := range []string{"unindexed_foreign_keys", "no_primary_key", "duplicate_index", "auth_db_connections_absolute"} {
		if len(byName(perf, want)) == 0 {
			t.Errorf("performance lacks %s: %v", want, lintNames(perf))
		}
	}
	for _, l := range perf {
		if !slices.Contains(toStrings(l["categories"]), "PERFORMANCE") {
			t.Errorf("a lint of another category in the performance advisor: %v", l)
		}
	}

	// Studio's call returns every lint of both kinds.
	all := lints(pp+"/run-lints", true)
	if len(all) != len(sec)+len(perf) {
		t.Errorf("run-lints has %d lints, security %d + performance %d", len(all), len(sec), len(perf))
	}

	// Every row satisfies the spec's lint object.
	code, b := s.call("GET", pp+"/run-lints", nil, true)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	validateAgainstSpec(t, "GET /platform/projects/{ref}/run-lints", b)

	// A change made through the API shows at once: enable RLS, and the error turns into the notice
	// that the table has no policy.
	sql(`alter table public.notes enable row level security`)
	sec = lints(p+"/advisors/security", false)
	for _, l := range byName(sec, "rls_disabled_in_public") {
		if l["metadata"].(map[string]any)["name"] == "notes" {
			t.Errorf("public.notes is still reported after RLS was enabled")
		}
	}
	if got := byName(sec, "rls_enabled_no_policy"); len(got) != 1 || got[0]["level"] != "INFO" {
		t.Errorf("rls_enabled_no_policy: %v", got)
	}

	// Policies: one that calls auth.uid() for every row, a second permissive one for the same role
	// and action, and one that trusts the user's own metadata.
	sql(`
create policy by_author on public.notes for select to authenticated using (author = auth.uid());
create policy by_author_select on public.notes for select to authenticated using (author = (select auth.uid()));
create policy admins on public.notes for update to authenticated using ((auth.jwt() -> 'user_metadata' ->> 'role') = 'admin');
`)
	sec = lints(p+"/advisors/security", false)
	perf = lints(p+"/advisors/performance", false)
	// by_author and admins call auth functions per row; by_author_select wraps the call in a select.
	hasPolicy := func(ls []lintObj, policy string) bool {
		for _, l := range ls {
			if strings.Contains(l["detail"].(string), "`"+policy+"`") {
				return true
			}
		}
		return false
	}
	initplan := byName(perf, "auth_rls_initplan")
	if !hasPolicy(initplan, "by_author") || !hasPolicy(initplan, "admins") || hasPolicy(initplan, "by_author_select") {
		t.Errorf("auth_rls_initplan: %v", initplan)
	}
	for _, l := range initplan {
		if l["level"] != "WARN" || l["metadata"].(map[string]any)["name"] != "notes" {
			t.Errorf("auth_rls_initplan finding: %v", l)
		}
	}
	if got := byName(perf, "multiple_permissive_policies"); len(got) == 0 {
		t.Errorf("multiple_permissive_policies: %v", lintNames(perf))
	}
	if got := byName(sec, "rls_references_user_metadata"); len(got) != 1 || got[0]["level"] != "ERROR" {
		t.Errorf("rls_references_user_metadata: %v", lintNames(sec))
	}
	if got := byName(sec, "rls_enabled_no_policy"); len(got) != 0 {
		t.Errorf("rls_enabled_no_policy is reported for a table with policies: %v", got)
	}

	// The schemas PostgREST exposes decide what is reported.
	sql(`grant usage on schema private to anon, authenticated; grant select on all tables in schema private to anon, authenticated`)
	if code, b := s.call("PATCH", p+"/postgrest", map[string]any{"db_schema": "public,private"}, false); code != 200 {
		t.Fatalf("patch postgrest: %d %s", code, b)
	}
	sec = lints(p+"/advisors/security", false)
	found := false
	for _, l := range byName(sec, "rls_disabled_in_public") {
		if l["metadata"].(map[string]any)["schema"] == "private" {
			found = true
		}
	}
	if !found {
		t.Errorf("private.secrets is exposed now but not reported: %v", byName(sec, "rls_disabled_in_public"))
	}

	// The v2 route finds a lint by name.
	code, b = s.call("POST", "/v2/projects/"+s.Ref+"/advisors/run", v2Body("rls_disabled_in_public", "log_auth_error_rate_high"), false)
	if code != 200 {
		t.Fatalf("v2 run: %d %s", code, b)
	}
	if !strings.Contains(string(b), `"rls_disabled_in_public"`) || !strings.Contains(string(b), `"advisor_check_unavailable"`) {
		t.Errorf("v2 run: %s", b)
	}

	// The rules of the Advisor Rules page persist in the registry and take effect.
	exc := pp + "/notifications/advisor/exceptions"
	code, b = s.call("POST", exc, map[string]any{"exceptions": []map[string]any{{"lint_name": "rls_disabled_in_public", "is_disabled": true, "note": "intended"}}}, true)
	if code != 201 {
		t.Fatalf("create rule: %d %s", code, b)
	}
	var made struct{ Exceptions []map[string]any }
	_ = json.Unmarshal(b, &made)
	if len(made.Exceptions) != 1 || made.Exceptions[0]["note"] != "intended" || made.Exceptions[0]["last_updated_by"] != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("created rule: %s", b)
	}
	if got := byName(lints(p+"/advisors/security", false), "rls_disabled_in_public"); len(got) != 0 {
		t.Errorf("a disabled lint is still reported: %v", got)
	}
	id := made.Exceptions[0]["id"].(string)
	if code, b := s.call("PATCH", exc+"/"+id, map[string]any{"is_disabled": false}, true); code != 200 {
		t.Fatalf("patch rule: %d %s", code, b)
	}
	if got := byName(lints(p+"/advisors/security", false), "rls_disabled_in_public"); len(got) == 0 {
		t.Error("an enabled rule still hides the lint")
	}
	if code, b := s.call("DELETE", exc+"?ids="+id, nil, true); code != 200 {
		t.Fatalf("delete rule: %d %s", code, b)
	}
	code, b = s.call("GET", exc, nil, true)
	if code != 200 || strings.TrimSpace(string(b)) != `{"exceptions":[]}` {
		t.Errorf("rules after delete: %d %s", code, b)
	}

	// A Read-only member reads the advisors and cannot change the rules.
	req := func(role, method, path string, body any) int {
		t.Helper()
		saved := s.JWT
		s.JWT = s.JWTs[role]
		defer func() { s.JWT = saved }()
		code, _ := s.call(method, path, body, true)
		return code
	}
	if code := req("ro", "GET", pp+"/run-lints", nil); code != 200 {
		t.Errorf("read-only member reading the advisors: %d", code)
	}
	if code := req("ro", "POST", exc, map[string]any{"exceptions": []map[string]any{{"lint_name": "unused_index", "is_disabled": true}}}); code != 403 {
		t.Errorf("read-only member creating a rule: %d", code)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

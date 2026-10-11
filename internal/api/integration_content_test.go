package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// callWithHeader is call with extra request headers (the Idempotency-Key of a migration). A header
// with an empty value is not sent. It returns the status.
func (s *stack) callWithHeader(method, path string, body any, h map[string]string) int {
	s.t.Helper()
	b := mustJSON2(body)
	req, _ := http.NewRequest(method, s.APIURL+path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+s.PAT)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// PUT /v1/projects/{ref}/database/migrations records a migration without running it, once: by
// Idempotency-Key and by name plus statements. It runs against the stack of integration_test.go.
func TestIntegrationUpsertMigration(t *testing.T) {
	s := startStack(t)
	p := "/v1/projects/" + s.Ref
	put := func(body map[string]any, key string) int {
		t.Helper()
		// call has no header parameter: use the key in the body of a raw request below
		return s.callWithHeader("PUT", p+"/database/migrations", body, map[string]string{"Idempotency-Key": key})
	}
	count := func() int { return mustCount(t, s, p, "supabase_migrations.schema_migrations") }

	q := "create table public.recorded (id int primary key);\n"
	if code := put(map[string]any{"query": q, "name": "branch_merge_a"}, ""); code != 200 {
		t.Fatalf("first PUT: %d", code)
	}
	if n := count(); n != 1 {
		t.Fatalf("migrations after the first PUT: %d", n)
	}
	// The statements were recorded and not run.
	if n := mustCount(t, s, p, "pg_class where relname = 'recorded' and relnamespace = 'public'::regnamespace"); n != 0 {
		t.Fatalf("PUT ran the statements (%d table)", n)
	}
	// The same name and statements, even spaced differently, are recorded once.
	for _, again := range []string{q, "  create table public.recorded (id int primary key)  ;", "create table public.recorded\n(id int primary key)"} {
		if code := put(map[string]any{"query": again, "name": "branch_merge_a"}, ""); code != 200 {
			t.Fatalf("repeated PUT %q: %d", again, code)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("repeating the PUT recorded %d rows", n)
	}
	// A different name or other statements are another migration.
	if code := put(map[string]any{"query": q, "name": "branch_merge_b"}, ""); code != 200 {
		t.Fatalf("PUT under another name: %d", code)
	}
	if code := put(map[string]any{"query": "create table public.other (id int)", "name": "branch_merge_a"}, ""); code != 200 {
		t.Fatalf("PUT of other statements: %d", code)
	}
	if n := count(); n != 3 {
		t.Fatalf("rows after two different migrations: %d", n)
	}
	// An Idempotency-Key records one row however often it is sent, and whatever else changes.
	for i := 0; i < 3; i++ {
		if code := put(map[string]any{"query": "select " + strings.Repeat("1", i+1), "name": "keyed"}, "key-1"); code != 200 {
			t.Fatalf("keyed PUT %d: %d", i, code)
		}
	}
	if n := count(); n != 4 {
		t.Fatalf("rows after three PUTs with one key: %d", n)
	}
	// The versions are distinct and increasing, and the list shows them.
	if n := mustCount(t, s, p, "(select distinct version from supabase_migrations.schema_migrations) v"); n != 4 {
		t.Fatalf("%d distinct versions of 4 rows", n)
	}
	// An empty query is refused.
	if code, _ := s.call("PUT", p+"/database/migrations", map[string]any{"query": " ", "name": "x"}, false); code != 400 {
		t.Fatalf("empty query: %d", code)
	}
	// Reading the recorded one back through the CLI's list shows its name.
	code, b := s.call("GET", p+"/database/migrations", nil, false)
	if code != 200 || !strings.Contains(string(b), "branch_merge_a") {
		t.Fatalf("list: %d %s", code, b)
	}
}

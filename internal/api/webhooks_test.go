package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// roleRecorder records the database role each connection string was requested for.
type roleRecorder struct {
	*fakeManager
	roles *[]string
}

func (r roleRecorder) ConnString(ctx context.Context, ref, role string) (string, error) {
	*r.roles = append(*r.roles, role)
	return r.fakeManager.ConnString(ctx, ref, role)
}

var _ lifecycle.Manager = roleRecorder{}

// Enabling webhooks runs the embedded script once per call, as supabase_admin, through pg-meta,
// from either route, and answers 201 with no body (the specs' response).
func TestEnableWebhooksRunsTheScriptAsAdmin(t *testing.T) {
	f := newFixture(t)
	var roles []string
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.mgr.sec, Manager: roleRecorder{f.mgr, &roles}, Config: f.cfg, PGMetaURL: f.meta.URL})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	f.ownerOn(srv)
	for _, path := range []string{
		"/platform/database/" + testRef + "/hook-enable",
		"/v1/projects/" + testRef + "/database/webhooks/enable",
	} {
		before := len(f.meta.Requests)
		rec := f.do("POST", path, nil)
		if rec.Code != http.StatusCreated || rec.Body.Len() != 0 {
			t.Fatalf("POST %s: %d %q, want 201 and no body", path, rec.Code, rec.Body.String())
		}
		if len(f.meta.Requests) != before+1 {
			t.Fatalf("POST %s sent %d pg-meta requests, want 1", path, len(f.meta.Requests)-before)
		}
		req := f.meta.last()
		if req.Path != "/query" || !strings.Contains(req.Body, "supabase_functions.http_request") || !strings.Contains(req.Body, "pg_net") {
			t.Errorf("POST %s sent %s %q, want the webhooks script to /query", path, req.Path, truncate(req.Body, 80))
		}
	}
	if len(roles) != 2 || roles[0] != "supabase_admin" || roles[1] != "supabase_admin" {
		t.Errorf("roles requested = %v, want supabase_admin twice", roles)
	}
}

// A failing script reaches the caller with pg-meta's message and status, so the dashboard's toast
// says why; a paused project, a missing one and a read-only member never reach the database.
func TestEnableWebhooksErrors(t *testing.T) {
	f := newFixture(t)
	path := "/platform/database/" + testRef + "/hook-enable"

	f.meta.Rules = []pgmetaRule{{Contains: "supabase_functions", Status: 400, Body: `{"error":"extension \"pg_net\" is not available"}`}}
	rec := f.do("POST", path, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `extension \"pg_net\" is not available`) {
		t.Errorf("failing script: %d %s, want 400 with pg-meta's message", rec.Code, rec.Body.String())
	}

	f.meta.Rules = nil
	n := len(f.meta.Requests)
	if rec := f.do("POST", "/platform/database/bbbbbbbbbbbbbbbbbbbb/hook-enable", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: %d, want 404", rec.Code)
	}
	f.project.Status = registry.StatusInactive
	if err := f.reg.SetProjectStatus(context.Background(), testRef, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	if rec := f.do("POST", path, nil); rec.Code != http.StatusConflict {
		t.Errorf("paused project: %d, want 409", rec.Code)
	}
	if len(f.meta.Requests) != n {
		t.Errorf("pg-meta got %d requests for a missing and a paused project, want none", len(f.meta.Requests)-n)
	}
}

// The embedded script stays safe to run twice and keeps the artifact's pg_net grants: a
// re-vendored copy of upstream's file would fail these. (The schema and the migrations table are
// created inside checks, which the next test holds in place.)
func TestWebhooksScriptIsRerunnable(t *testing.T) {
	sql := webhooksSQL
	for _, c := range []struct{ name, re string }{
		{"hooks table", `(?i)create table supabase_functions\.hooks`},
		{"request id index", `(?i)create index supabase_functions_hooks`},
		{"function", `(?i)create function supabase_functions\.http_request`},
	} {
		if regexp.MustCompile(c.re).MatchString(sql) {
			t.Errorf("%s: the script creates it without IF NOT EXISTS, OR REPLACE or a check for an existing object", c.name)
		}
	}
	// The artifact's migrations own these (postgres artifact, migration 20250220051611_pg_net_perms_fix).
	if regexp.MustCompile(`(?i)create (or replace )?function extensions\.grant_pg_net_access`).MatchString(sql) ||
		regexp.MustCompile(`(?i)create event trigger`).MatchString(sql) {
		t.Error("the script redefines the artifact's pg_net event trigger")
	}
	// SECURITY DEFINER on net.http_* belongs to pg_net 0.11.0 and older only.
	for _, stmt := range regexp.MustCompile(`(?is)alter function net\.http_(get|post)[^;]*security definer;`).FindAllStringIndex(sql, -1) {
		guard := sql[:stmt[0]]
		if i := strings.LastIndex(guard, "extversion IN"); i < 0 || !strings.Contains(sql[i:stmt[0]], "'0.11.0'") {
			t.Errorf("an ALTER FUNCTION net.http_* ... SECURITY DEFINER at offset %d is not limited to the old pg_net versions", stmt[0])
		}
	}
	if !strings.Contains(sql, "Apache-2.0") || !strings.Contains(sql, "8617532ae1a41571d5d557cd24c6f5f2565d9230") {
		t.Error("the script lost its provenance header")
	}
}

// The script runs as a superuser on a database whose project member (the postgres role) can create
// a schema, a table with a trigger, a column default or a role before Enable is called, and, once
// the first run has granted privileges on the schema's tables, can add a trigger to them. These
// checks hold the rule that nothing the script runs executes code the project wrote:
//   - it refuses a supabase_functions schema that supabase_admin does not own, before it changes it;
//   - it inserts rows only into the migrations table that the same run has just created;
//   - it never reassigns or drops what a role owns (upstream's cleanup of supabase_pg_net_admin
//     would hand the superuser a SECURITY DEFINER function the project made).
//
// The behavior (a trigger on supabase_functions.migrations that stays silent, a refused schema) is
// checked against a real Postgres in tests/linux/roles-smoke.sh.
func TestWebhooksScriptRunsNoCodeAProjectWrote(t *testing.T) {
	sql := webhooksSQL
	body := sql[strings.Index(sql, "BEGIN;"):] // the header's prose is not SQL
	at := func(re string) int {
		t.Helper()
		loc := regexp.MustCompile(re).FindStringIndex(body)
		if loc == nil {
			t.Fatalf("the script has no %s", re)
		}
		return loc[0]
	}

	// 1. The schema check comes before anything touches the schema, and the CREATE inside it is plain.
	owner := at(`schema_owner <> 'supabase_admin'`)
	raise := at(`RAISE EXCEPTION 'The schema supabase_functions already exists`)
	create := at(`CREATE SCHEMA supabase_functions AUTHORIZATION supabase_admin;`)
	if create > owner {
		t.Error("the owner check comes before the schema is created: a schema made in between would be taken over")
	}
	for _, re := range []string{`GRANT\s+USAGE\s+ON\s+SCHEMA\s+supabase_functions`, `ALTER\s+DEFAULT\s+PRIVILEGES\s+IN\s+SCHEMA\s+supabase_functions`, `CREATE\s+TABLE`, `GRANT\s+ALL\s+PRIVILEGES`} {
		if first := at(re); first < raise {
			t.Errorf("%s runs before the check of the schema's owner", re)
		}
	}

	// 2. Every INSERT into a table of the schema is either the migrations rows inside the branch that
	// creates the table, or the body of http_request(), which runs as supabase_functions_admin for a
	// trigger the project created.
	inserts := regexp.MustCompile(`(?i)\binsert\s+into\s+([a-z_.]+)`).FindAllStringSubmatchIndex(body, -1)
	if len(inserts) != 2 {
		t.Fatalf("the script has %d INSERT statements, want 2 (the migrations rows and the hook record in http_request)", len(inserts))
	}
	for _, m := range inserts {
		target, pos := body[m[2]:m[3]], m[0]
		switch target {
		case "supabase_functions.migrations":
			guard := strings.LastIndex(body[:pos], "IF to_regclass('supabase_functions.migrations') IS NULL THEN")
			created := strings.LastIndex(body[:pos], "CREATE TABLE supabase_functions.migrations")
			if guard < 0 || created < guard || strings.Contains(body[created:pos], "END IF") {
				t.Error("the migrations rows are not inserted inside the branch that creates the table")
			}
			if tail := body[pos:]; !strings.HasPrefix(tail[strings.Index(tail, ";")+1:], "\n    END IF;") {
				t.Error("the migrations insert is followed by more statements before END IF")
			}
		case "supabase_functions.hooks":
			fn, end := strings.Index(body, "CREATE OR REPLACE FUNCTION supabase_functions.http_request()"), strings.Index(body, "$function$;")
			if pos < fn || pos > end {
				t.Error("an INSERT into supabase_functions.hooks outside of http_request()")
			}
		default:
			t.Errorf("INSERT INTO %s", target)
		}
	}

	// 3. No statement that rewrites what other roles own, deletes, or runs a project's code on purpose.
	for _, re := range []string{
		`(?i)\breassign\s+owned\b`, `(?i)\bdrop\s+owned\b`, `(?i)\bdrop\s+(role|user|schema|table|function|trigger)\b`,
		`(?i)\b(update|delete)\s+(from\s+)?supabase_functions`, `(?i)\btruncate\b`, `(?i)\bcopy\b`, `(?i)\bperform\b`, `(?i)\bexecute\b\s+(format|'|\$)`,
	} {
		if m := regexp.MustCompile(re).FindString(body); m != "" {
			t.Errorf("the script has %q", m)
		}
	}
}

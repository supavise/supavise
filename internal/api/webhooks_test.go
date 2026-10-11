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
// re-vendored copy of upstream's file would fail these.
func TestWebhooksScriptIsRerunnable(t *testing.T) {
	sql := webhooksSQL
	for _, c := range []struct{ name, re string }{
		{"schema", `(?i)create schema supabase_functions`},
		{"migrations table", `(?i)create table supabase_functions\.migrations`},
		{"hooks table", `(?i)create table supabase_functions\.hooks`},
		{"request id index", `(?i)create index supabase_functions_hooks`},
		{"function", `(?i)create function supabase_functions\.http_request`},
		{"migration row", `(?i)insert into supabase_functions\.migrations \(version\) values \('[^']+'\);`},
	} {
		if regexp.MustCompile(c.re).MatchString(sql) {
			t.Errorf("%s: the script creates it without IF NOT EXISTS, OR REPLACE or ON CONFLICT", c.name)
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

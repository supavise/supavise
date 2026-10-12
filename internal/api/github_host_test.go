package api

import (
	"context"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/projectconfig"
)

// The GitHub integration saves a branch's PostgREST settings through ghHost.SaveSettings. Like the
// route, it refuses a list that names a schema the database lacks before it restarts PostgREST:
// without that the restart would wait out PostgREST's readiness timeout and roll the save back, and the
// run would fail with a message that does not name the schema.
func TestTheGitHubHostRefusesAMissingSchemaBeforeItRestartsPostgREST(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var asked [][]string
	f.srv.missingSchemas = func(_ context.Context, _ string, names []string) ([]string, error) {
		asked = append(asked, names)
		var missing []string
		for _, n := range names {
			if n == "nope" {
				missing = append(missing, n)
			}
		}
		return missing, nil
	}
	h := ghHost{f.srv}
	applied := func() int {
		f.mgr.mu.Lock()
		defer f.mgr.mu.Unlock()
		return len(f.mgr.applied)
	}
	before := applied()

	_, err := h.SaveSettings(ctx, testRef, projectconfig.PostgREST, map[string]any{"db_schema": "public, nope", "max_rows": 7})
	if err == nil || !strings.Contains(err.Error(), "does not exist in the database: nope") {
		t.Fatalf("err = %v", err)
	}
	if len(asked) != 1 || strings.Join(asked[0], ",") != "public,nope" {
		t.Fatalf("the database was asked about %v", asked)
	}
	if applied() != before {
		t.Fatal("PostgREST was reconfigured for a list that names a missing schema")
	}
	if st, _ := f.srv.settings.Get(ctx, testRef, projectconfig.PostgREST); st.Set["db_schema"] != nil || st.Set["max_rows"] != nil {
		t.Fatalf("a refused save left settings behind: %v", st.Set)
	}

	// A list whose schemas exist is saved and applied, and so is a patch that does not touch the list.
	changed, err := h.SaveSettings(ctx, testRef, projectconfig.PostgREST, map[string]any{"db_schema": "public,api"})
	if err != nil || strings.Join(changed, ",") != "db_schema" || applied() != before+1 {
		t.Fatalf("changed %v, err %v, applies %d", changed, err, applied()-before)
	}
	asked = nil
	if _, err := h.SaveSettings(ctx, testRef, projectconfig.PostgREST, map[string]any{"max_rows": 7}); err != nil || len(asked) != 0 {
		t.Fatalf("err %v, asked %v", err, asked)
	}
	// Another service's settings are not checked against the schemas.
	if _, err := h.SaveSettings(ctx, testRef, projectconfig.Auth, map[string]any{"site_url": "https://app.example.com"}); err != nil || len(asked) != 0 {
		t.Fatalf("err %v, asked %v", err, asked)
	}
}

// SettingSaved is what the integration asks before it switches an OAuth provider on: a secret that
// was saved counts, a cleared or unsaved one does not, and the value is never returned.
func TestTheGitHubHostSaysWhichSettingsAreSaved(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := ghHost{f.srv}
	saved := func(key string) bool {
		t.Helper()
		ok, err := h.SettingSaved(ctx, testRef, projectconfig.Auth, key)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if saved("external_github_secret") || saved("site_url") {
		t.Fatal("nothing is saved yet")
	}
	if _, err := h.SaveSettings(ctx, testRef, projectconfig.Auth, map[string]any{
		"external_github_enabled": true, "external_github_client_id": "id", "external_github_secret": "s3cret", "site_url": "https://app.example.com"}); err != nil {
		t.Fatal(err)
	}
	if !saved("external_github_secret") || !saved("site_url") || !saved("external_github_enabled") || saved("external_google_secret") {
		t.Fatal("the saved settings are not reported")
	}
	if _, err := h.SaveSettings(ctx, testRef, projectconfig.Auth, map[string]any{"external_github_enabled": false, "external_github_secret": nil}); err != nil {
		t.Fatal(err)
	}
	if saved("external_github_secret") {
		t.Fatal("a cleared secret is reported as saved")
	}
}

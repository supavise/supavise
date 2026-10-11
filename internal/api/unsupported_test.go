package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Each hosted-only feature answers what is true instead of a success that did nothing. The statuses are
// 4xx, which Studio shows as the reason and does not retry.
func TestHostedOnlyFeaturesAreRefused(t *testing.T) {
	f := newFixture(t)
	const p = "/v1/projects/" + testRef
	const pp = "/platform/projects/" + testRef
	cases := []struct {
		method, path string
		status       int
		want         string
	}{
		// Studio matches this text to render a disabled switch instead of an error.
		{"GET", p + "/ssl-enforcement", 400, "not allowed to configure SSL enforcements"},
		{"PUT", p + "/ssl-enforcement", 400, "not allowed to configure SSL enforcements"},
		{"GET", p + "/config/auth/signing-keys/11111111-2222-4333-8444-555555555555", 404, "Signing key not found"},
		{"POST", p + "/config/auth/third-party-auth", 400, "third-party auth"},
		{"GET", p + "/config/auth/third-party-auth/11111111-2222-4333-8444-555555555555", 404, "not found"},
		{"DELETE", p + "/config/auth/third-party-auth/11111111-2222-4333-8444-555555555555", 404, "not found"},
		{"POST", "/platform/feedback/send", 400, "open an issue"},
		{"GET", pp + "/analytics/endpoints/auth.metrics", 400, "supavise logs " + testRef + " --service auth"},
		// The paused project's Download backups menu: a stub made Studio poll for ever behind a loading toast.
		{"GET", "/platform/storage/" + testRef + "/archive", 400, "does not build storage archives"},
		{"POST", "/platform/storage/" + testRef + "/archive", 400, "does not build storage archives"},
		{"POST", "/platform/database/" + testRef + "/backups/download", 400, "supavise backups list"},
		{"PUT", "/platform/update-email", 400, "supavise users invite"},
		// Add-ons other than compute: a stub said "Successfully updated" and nothing changed.
		{"POST", pp + "/billing/addons", 400, "Point-in-time recovery is included"},
		{"DELETE", pp + "/billing/addons/pitr_7", 400, "Point-in-time recovery is included"},
		{"DELETE", p + "/billing/addons/pitr_14", 400, "Point-in-time recovery is included"},
		{"DELETE", pp + "/billing/addons/cd_default", 400, "Custom domains are included"},
		{"DELETE", pp + "/billing/addons/something_else", 400, "only the compute size"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			var body any
			if c.method == "PUT" || c.method == "PATCH" || c.method == "POST" {
				body = map[string]any{}
				if strings.HasSuffix(c.path, "/billing/addons") {
					body = map[string]any{"addon_type": "pitr", "addon_variant": "pitr_7"}
				}
			}
			rec := f.do(c.method, c.path, body)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			var out struct{ Message string }
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !strings.Contains(out.Message, c.want) {
				t.Errorf("message %q (%v), want it to contain %q", out.Message, err, c.want)
			}
			if rec.Header().Get("X-Supavise-Stub") != "" {
				t.Error("served by the stub")
			}
		})
	}
	// The same routes on a project that does not exist.
	for _, path := range []string{p + "/ssl-enforcement", pp + "/config/secrets/update-status", pp + "/analytics/endpoints/auth.metrics"} {
		path = strings.ReplaceAll(path, testRef, "bbbbbbbbbbbbbbbbbbbb")
		if rec := f.do("GET", path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, rec.Code)
		}
	}
}

// The JWT secret page polls this: a stub says status 0, which is "updating", so the legacy API keys tab showed
// "Updating JWT secret..." in place of the keys. A project whose secret never changed has no status (jwt_secret.go).
func TestNoJWTSecretUpdateRuns(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/platform/projects/"+testRef+"/config/secrets/update-status", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/projects/{ref}/config/secrets/update-status", rec.Body.Bytes())
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if v, ok := out["update_status"]; !ok || v != nil {
		t.Errorf("update_status = %v (present %v), want null", v, ok)
	}
}

// The profile hides the pages of features a node cannot offer.
func TestProfileHidesWhatCannotWork(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/platform/profile", nil)
	var prof struct {
		DisabledFeatures []string `json:"disabled_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &prof); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, d := range prof.DisabledFeatures {
		have[d] = true
	}
	// Hosted's own documents, the deletion request to Supabase's support desk and the project transfer have
	// nothing behind them on a node (unsupported.go, identity.go).
	for _, want := range []string{"storage:analytics", "storage:vectors", "organization:show_legal_documents", "profile:show_account_deletion", "projects:transfer"} {
		if !have[want] {
			t.Errorf("disabled_features lacks %s", want)
		}
	}
	// Logs and Reports stay: they say what is missing. Third-party Auth stays too: services could accept
	// a JWKS (PostgREST, Storage and Realtime take one), hosted offers it to every project, and the page
	// is not unbuildable, so its "Add provider" answers with the reason (unsupported.go) instead of vanishing.
	// The organization's SSO page works against sso_routes.go (one provider per organization, as Studio's page
	// assumes), so it is not hidden.
	for _, keep := range []string{"logs:all", "reports:all", "authentication:third_party_auth", "organization:show_sso_settings"} {
		if have[keep] {
			t.Errorf("disabled_features hides %s", keep)
		}
	}
}

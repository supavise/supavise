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
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			var body any
			if c.method == "PUT" || c.method == "PATCH" || c.method == "POST" {
				body = map[string]any{}
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

// Settings > General > Custom domains works for projects and branches, so the profile shows it, unless
// the node's certificates come from a DNS-01 wildcard, which cannot serve a hostname of its own.
func TestProfileShowsCustomDomainsUnlessTheNodeIssuesOnlyAWildcard(t *testing.T) {
	disabled := func(t *testing.T, mode string) map[string]bool {
		t.Helper()
		f := newFixture(t)
		f.cfg.TLS.Mode = mode
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
		return have
	}
	for _, mode := range []string{"", "auto", "http01", "off"} {
		if disabled(t, mode)["project_settings:custom_domains"] {
			t.Errorf("tls.mode %q hides the Custom domains page", mode)
		}
	}
	if !disabled(t, "dns01")["project_settings:custom_domains"] {
		t.Error("tls.mode dns01 shows a page whose claims the node refuses")
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
	for _, want := range []string{"storage:analytics", "storage:vectors"} {
		if !have[want] {
			t.Errorf("disabled_features lacks %s", want)
		}
	}
	// Logs and Reports stay: they say what is missing. Third-party Auth stays too: services could accept
	// a JWKS (PostgREST, Storage and Realtime take one), hosted offers it to every project, and the page
	// is not unbuildable, so its "Add provider" answers with the reason (unsupported.go) instead of vanishing.
	for _, keep := range []string{"logs:all", "reports:all", "authentication:third_party_auth", "project_settings:custom_domains"} {
		if have[keep] {
			t.Errorf("disabled_features hides %s", keep)
		}
	}
}

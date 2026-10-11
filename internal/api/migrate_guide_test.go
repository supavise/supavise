package api

import (
	"os"
	"regexp"
	"testing"

	"github.com/supavise/supavise/internal/projectconfig"
)

// guideSecretsFilter reads the pattern of settings that docs/guide.md ("Copy the settings") removes
// before it sends a hosted project's settings to the node.
func guideSecretsFilter(t *testing.T) *regexp.Regexp {
	t.Helper()
	b, err := os.ReadFile("../../docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^secrets='([^']+)'$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("docs/guide.md has no secrets='...' line in its settings block")
	}
	re, err := regexp.Compile(string(m[1]))
	if err != nil {
		t.Fatalf("the guide's filter is not a regular expression: %v", err)
	}
	return re
}

// The guide removes the secrets of the Auth settings by name, because hosted reports a secret as
// a hash that the node would take as a new value. A secret setting the filter misses would be
// replaced by a hash; a setting it catches that is not a secret would not move.
func TestGuideSettingsFilterMatchesExactlyTheSecrets(t *testing.T) {
	re := guideSecretsFilter(t)
	for _, f := range projectconfig.AuthSchema.Fields {
		if f.Secret && !re.MatchString(f.Name) {
			t.Errorf("the guide's filter lets the secret %s through", f.Name)
		}
		if !f.Secret && re.MatchString(f.Name) {
			t.Errorf("the guide's filter removes %s, which is not a secret", f.Name)
		}
	}
	if !re.MatchString("jwt_secret") {
		t.Error("the guide's filter must remove jwt_secret, which GET .../postgrest returns")
	}
}

// The settings loop of the guide reads each service's settings with GET and sends them with
// PATCH, minus the secrets. The node must take every one of its own answers back.
func TestGuideSettingsLoopRoundTrips(t *testing.T) {
	f := newFixture(t)
	re := guideSecretsFilter(t)
	const p = "/v1/projects/" + testRef
	f.mustDo("PATCH", authV1, map[string]any{
		"site_url": "https://app.example.test", "uri_allow_list": "https://app.example.test/**",
		"external_github_enabled": true, "external_github_client_id": "cid", "external_github_secret": "github-secret-value",
		"smtp_host": "smtp.example.com", "smtp_port": "2525", "smtp_user": "mailer", "smtp_pass": "smtp-password",
		"smtp_admin_email": "no-reply@example.com", "smtp_sender_name": "Acme",
	}, 200)
	f.mustDo("PATCH", p+"/postgrest", map[string]any{"max_rows": 777}, 200)
	for _, path := range []string{p + "/config/auth", p + "/postgrest", p + "/config/storage", p + "/config/realtime"} {
		got := bodyMap(t, f.mustDo("GET", path, nil, 200))
		out := map[string]any{}
		for k, v := range got {
			if !re.MatchString(k) {
				out[k] = v
			}
		}
		if len(out) == 0 {
			t.Fatalf("%s: nothing left to send", path)
		}
		want := 200
		if path == p+"/config/realtime" {
			want = 204 // the spec answers the Realtime PATCH without a body
		}
		f.mustDo("PATCH", path, out, want)
	}
	// What was set is still set, and the secrets were not replaced by the hashes GET reported.
	after := bodyMap(t, f.mustDo("GET", authV1, nil, 200))
	if after["site_url"] != "https://app.example.test" || after["smtp_host"] != "smtp.example.com" {
		t.Fatalf("settings changed: site_url=%v smtp_host=%v", after["site_url"], after["smtp_host"])
	}
	if after["external_github_secret"] != projectconfig.Redact("github-secret-value") || after["smtp_pass"] != projectconfig.Redact("smtp-password") {
		t.Fatalf("a secret was replaced: %v %v", after["external_github_secret"], after["smtp_pass"])
	}
	if got := bodyMap(t, f.mustDo("GET", p+"/postgrest", nil, 200)); got["max_rows"] != float64(777) {
		t.Fatalf("max_rows = %v", got["max_rows"])
	}
}

package github

import (
	"fmt"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/projectconfig"
)

func plan(t *testing.T, toml, ref string, persistent bool, env map[string]string) *ConfigPlan {
	t.Helper()
	doc, err := ParseConfig([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	p, err := PlanConfig(PlanInput{Doc: doc, Ref: ref, Persistent: persistent,
		Env: func(name string) (string, bool, error) { v, ok := env[name]; return v, ok, nil },
		Template: func(rel string) (string, bool) {
			if rel == "templates/invite.html" {
				return "<h1>Hi</h1>", true
			}
			return "", false
		}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func hasWarning(p *ConfigPlan, sub string) bool {
	for _, w := range p.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestConfigTomlMapsTheSectionsOntoProjectSettings(t *testing.T) {
	p := plan(t, `
project_id = "app"
[api]
enabled = true
port = 54321
schemas = ["public", "graphql_public"]
extra_search_path = ["public", "extensions"]
max_rows = 500
[db]
port = 54322
major_version = 17
[db.settings]
max_connections = 120
work_mem = "8MB"
[auth]
site_url = "https://app.example.com"
additional_redirect_urls = ["https://a.example.com", "https://b.example.com"]
jwt_expiry = 3600
enable_refresh_token_rotation = true
refresh_token_reuse_interval = 10
enable_signup = false
enable_anonymous_sign_ins = true
minimum_password_length = 8
password_requirements = "lower_upper_letters_digits"
[auth.rate_limit]
email_sent = 7
sign_in_sign_ups = 40
token_verifications = 50
[auth.email]
enable_signup = true
enable_confirmations = true
double_confirm_changes = true
max_frequency = "1m0s"
otp_length = 8
[auth.email.smtp]
host = "smtp.example.com"
port = 587
user = "mailer"
pass = "env(SMTP_PASS)"
admin_email = "ops@example.com"
sender_name = "App"
[auth.email.template.invite]
subject = "You are invited"
content_path = "./templates/invite.html"
[auth.mfa]
max_enrolled_factors = 5
[auth.mfa.totp]
enroll_enabled = true
verify_enabled = true
[auth.external.github]
enabled = true
client_id = "gh-id"
secret = "env(GITHUB_SECRET)"
redirect_uri = "http://localhost:54321/auth/v1/callback"
[auth.hook.custom_access_token]
enabled = true
uri = "pg-functions://postgres/public/hook"
[auth.sms.twilio]
enabled = true
account_sid = "AC1"
message_service_sid = "MG1"
auth_token = "env(TWILIO_TOKEN)"
[auth.sms.test_otp]
"4152127777" = "123456"
[storage]
file_size_limit = "50MiB"
[storage.image_transformation]
enabled = true
[storage.buckets.avatars]
public = true
file_size_limit = "5MiB"
allowed_mime_types = ["image/png", "image/jpeg"]
[functions.hello]
verify_jwt = false
[db.seed]
enabled = true
sql_paths = ["./seed.sql", "./seeds/*.sql"]
`, "refrefrefrefrefrefref", false, map[string]string{"SMTP_PASS": "s3cret", "GITHUB_SECRET": "gh-secret", "TWILIO_TOKEN": "tw"})

	a := p.Patches[projectconfig.Auth]
	want := map[string]any{
		"site_url": "https://app.example.com", "uri_allow_list": "https://a.example.com,https://b.example.com", "jwt_exp": int64(3600),
		"refresh_token_rotation_enabled": true, "security_refresh_token_reuse_interval": int64(10), "disable_signup": true,
		"external_anonymous_users_enabled": true, "password_min_length": int64(8),
		"password_required_characters": "abcdefghijklmnopqrstuvwxyz:ABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456789",
		"rate_limit_email_sent":        int64(7), "rate_limit_otp": int64(40), "rate_limit_verify": int64(50),
		"external_email_enabled": true, "mailer_autoconfirm": false, "mailer_secure_email_change_enabled": true, "smtp_max_frequency": int64(60), "mailer_otp_length": int64(8),
		"smtp_host": "smtp.example.com", "smtp_port": "587", "smtp_user": "mailer", "smtp_pass": "s3cret", "smtp_admin_email": "ops@example.com", "smtp_sender_name": "App",
		"mailer_subjects_invite": "You are invited", "mailer_templates_invite_content": "<h1>Hi</h1>",
		"mfa_max_enrolled_factors": int64(5), "mfa_totp_enroll_enabled": true, "mfa_totp_verify_enabled": true,
		"external_github_enabled": true, "external_github_client_id": "gh-id", "external_github_secret": "gh-secret",
		"hook_custom_access_token_enabled": true, "hook_custom_access_token_uri": "pg-functions://postgres/public/hook",
		"sms_provider": "twilio", "sms_twilio_account_sid": "AC1", "sms_twilio_message_service_sid": "MG1", "sms_twilio_auth_token": "tw",
		"sms_test_otp": "4152127777=123456",
	}
	for k, v := range want {
		if fmt.Sprint(a[k]) != fmt.Sprint(v) {
			t.Errorf("auth %s = %v (%T), want %v", k, a[k], a[k], v)
		}
	}
	// Every key that was mapped is one the schema knows, so the save cannot reject its name.
	for svc, patch := range p.Patches {
		sch := map[projectconfig.Service]*projectconfig.Schema{projectconfig.Auth: projectconfig.AuthSchema, projectconfig.PostgREST: projectconfig.PostgRESTSchema,
			projectconfig.Storage: projectconfig.StorageSchema, projectconfig.Postgres: projectconfig.PostgresSchema}[svc]
		for k := range patch {
			if _, ok := sch.Field(k); !ok {
				t.Errorf("%s: %q is not a setting", svc, k)
			}
		}
	}
	pr := p.Patches[projectconfig.PostgREST]
	if pr["db_schema"] != "public,graphql_public" || pr["db_extra_search_path"] != "public,extensions" || pr["max_rows"] != int64(500) {
		t.Errorf("postgrest = %v", pr)
	}
	if pg := p.Patches[projectconfig.Postgres]; pg["max_connections"] != int64(120) || pg["work_mem"] != "8MB" {
		t.Errorf("postgres = %v", pg)
	}
	st := p.Patches[projectconfig.Storage]
	if st["fileSizeLimit"] != int64(50<<20) {
		t.Errorf("storage = %v", st)
	}
	if f, _ := st["features"].(map[string]any); f["imageTransformation"].(map[string]any)["enabled"] != true {
		t.Errorf("features = %v", st["features"])
	}
	if len(p.Buckets) != 1 || p.Buckets[0].Name != "avatars" || !p.Buckets[0].Public || p.Buckets[0].FileSizeLimit != 5<<20 || len(p.Buckets[0].AllowedMIMETypes) != 2 {
		t.Errorf("buckets = %+v", p.Buckets)
	}
	if fc := p.Functions["hello"]; fc.VerifyJWT == nil || *fc.VerifyJWT {
		t.Errorf("functions = %+v", p.Functions)
	}
	if p.Seed.Enabled == nil || !*p.Seed.Enabled || len(p.Seed.SQLPaths) != 2 {
		t.Errorf("seed = %+v", p.Seed)
	}
	// The local-development keys are named once as such; the redirect_uri of a provider says why it is not applied.
	if !hasWarning(p, "[api.port]") || !hasWarning(p, "redirect_uri") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	for _, w := range p.Warnings {
		for _, secret := range []string{"s3cret", "gh-secret", "tw"} {
			if w == secret {
				t.Errorf("a warning is a secret")
			}
		}
	}
	for _, a := range p.Applied {
		if strings.Contains(a, "s3cret") {
			t.Errorf("applied lists a value: %q", a)
		}
	}
}

func TestConfigTomlNeverDropsASettingSilently(t *testing.T) {
	p := plan(t, `
[auth.third_party.firebase]
enabled = true
project_id = "x"
[auth.oauth_server]
enabled = true
[auth.external.github]
enabled = true
url = "https://ghe.example.com"
[auth.hook.send_sms]
enabled = true
nonsense = 1
[storage.analytics]
enabled = true
[db.settings]
not_a_setting = 1
[db.network_restrictions]
enabled = true
[functions.hello]
deno_version = 2
[storage.buckets.b]
unknown = 1
[studio]
port = 54323
[inbucket]
port = 54324
[analytics]
enabled = true
`, "r", false, nil)
	for _, want := range []string{
		"auth.third_party.firebase.enabled", "auth.third_party.firebase.project_id", "auth.oauth_server.enabled", "auth.external.github.url",
		"auth.hook.send_sms.nonsense", "storage.analytics.enabled", "db.settings.not_a_setting", "db.network_restrictions.enabled",
		"functions.hello.deno_version", "storage.buckets.b.unknown",
	} {
		if !hasWarning(p, want) {
			t.Errorf("no warning for %s:\n%s", want, strings.Join(p.Warnings, "\n"))
		}
	}
	// The local development sections are one line, not a line per key.
	local := 0
	for _, w := range p.Warnings {
		if strings.Contains(w, "local development") {
			local++
			for _, sec := range []string{"[studio]", "[inbucket]", "[analytics]"} {
				if !strings.Contains(w, sec) {
					t.Errorf("the local line lacks %s: %s", sec, w)
				}
			}
		}
	}
	if local != 1 {
		t.Errorf("local development warnings = %d", local)
	}
	// What is supported in the same file is applied; what is not is in no patch.
	if a := p.Patches[projectconfig.Auth]; a["external_github_enabled"] != true || a["hook_send_sms_enabled"] != true || len(a) != 2 {
		t.Errorf("auth patch = %v", a)
	}
	if len(p.Patches[projectconfig.Postgres]) != 0 || len(p.Patches[projectconfig.Storage]) != 0 {
		t.Errorf("patches = %v", p.Patches)
	}
}

func TestEnvThatCannotBeResolvedIsSkippedWithAWarning(t *testing.T) {
	p := plan(t, `
[auth.external.github]
enabled = true
client_id = "id"
secret = "env(MISSING_SECRET)"
[auth.email.smtp]
host = "smtp.example.com"
pass = "encrypted:abcdef"
`, "r", false, map[string]string{})
	a := p.Patches[projectconfig.Auth]
	if _, set := a["external_github_secret"]; set {
		t.Error("an unresolved env() was applied")
	}
	if a["external_github_client_id"] != "id" || a["smtp_host"] != "smtp.example.com" {
		t.Errorf("the rest was dropped too: %v", a)
	}
	if !hasWarning(p, "MISSING_SECRET") || !hasWarning(p, "encrypted:") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	if _, set := a["smtp_pass"]; set {
		t.Error("an encrypted value was applied")
	}
}

func TestRemoteBlockWithTheBranchRefOverridesTheTopLevel(t *testing.T) {
	const toml = `
project_id = "app"
[auth]
site_url = "https://base.example.com"
enable_signup = true
[api]
max_rows = 100

[remotes.staging]
project_id = "stagingstagingstagin"
[remotes.staging.auth]
site_url = "https://staging.example.com"
[remotes.staging.api]
max_rows = 7

[remotes.production]
project_id = "prodprodprodprodprodp"
[remotes.production.auth]
site_url = "https://example.com"
`
	p := plan(t, toml, "stagingstagingstagin", true, nil)
	if a := p.Patches[projectconfig.Auth]; a["site_url"] != "https://staging.example.com" || a["disable_signup"] != false {
		t.Errorf("staging auth = %v", a)
	}
	if pr := p.Patches[projectconfig.PostgREST]; pr["max_rows"] != int64(7) {
		t.Errorf("staging postgrest = %v", pr)
	}
	if p.Skipped != "" {
		t.Errorf("skipped = %q", p.Skipped)
	}
	// Another branch matches the other block.
	if a := plan(t, toml, "prodprodprodprodprodp", true, nil).Patches[projectconfig.Auth]; a["site_url"] != "https://example.com" {
		t.Errorf("production auth = %v", a)
	}
	// A persistent branch no block names is not configured at all (hosted skips the step) ...
	none := plan(t, toml, "unknownunknownunknow", true, nil)
	if none.Skipped == "" || !strings.Contains(none.Skipped, "unknownunknownunknow") || len(none.Patches) != 0 {
		t.Errorf("a persistent branch with no remote: %+v", none)
	}
	// ... while a preview branch takes the top-level values.
	prev := plan(t, toml, "unknownunknownunknow", false, nil)
	if prev.Skipped != "" || prev.Patches[projectconfig.Auth]["site_url"] != "https://base.example.com" || prev.Patches[projectconfig.PostgREST]["max_rows"] != int64(100) {
		t.Errorf("a preview branch: %+v", prev)
	}
	// The remotes themselves are not reported as unknown keys.
	for _, w := range prev.Warnings {
		if strings.Contains(w, "remotes") {
			t.Errorf("warning about remotes: %s", w)
		}
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[any]int64{"50MiB": 50 << 20, "5GB": 5e9, "1024": 1024, "10 KB": 10000, int64(7): 7, "2.5MiB": int64(2.5 * (1 << 20)), "1GiB": 1 << 30} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%v) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []any{"fifty", "5 parsecs", true, 1.5} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%v) accepted", bad)
		}
	}
}

func TestBucketsSQLQuotesEveryValue(t *testing.T) {
	sql := bucketsSQL([]BucketSpec{{Name: "o'brien; drop table x; --", Public: true, FileSizeLimit: 5, AllowedMIMETypes: []string{"image/png", "a'b"}}, {Name: "plain"}})
	if strings.Count(sql, "insert into storage.buckets") != 2 || !strings.Contains(sql, `'o''brien; drop table x; --'`) || !strings.Contains(sql, `'a''b'`) {
		t.Fatalf("sql = %s", sql)
	}
	if !strings.Contains(sql, "values ('plain', 'plain', false, null, null)") {
		t.Fatalf("sql = %s", sql)
	}
}

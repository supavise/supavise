package github

import (
	"fmt"
	"slices"
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
	// This node runs no imgproxy: `enabled = true` is not applied, and the run log says why. Studio and the
	// CLI get a refusal for the same setting from the API.
	if f, _ := st["features"].(map[string]any); f["imageTransformation"] != nil {
		t.Errorf("image transformation was switched on: %v", st["features"])
	}
	if !hasWarning(p, "storage.image_transformation.enabled = true is not applied") || !hasWarning(p, "imgproxy") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	if slices.Contains(p.Applied, "storage.image_transformation.enabled") {
		t.Errorf("applied lists image transformation: %v", p.Applied)
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

func TestABranchDoesNotSendItsSecretsToAHostTheRepositoryNames(t *testing.T) {
	cfg := `
[auth.email.smtp]
host = "mail.attacker.example"
user = "u"
[auth.external.gitlab]
enabled = true
client_id = "id"
url = "https://gitlab.attacker.example"
[auth.external.keycloak]
enabled = true
client_id = "id"
secret = "env(KEYCLOAK_SECRET)"
url = "https://sso.example.com"
`
	doc, err := ParseConfig([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	env := func(name string) (string, bool, error) { return "s3cret", name == "KEYCLOAK_SECRET", nil }
	branch, err := PlanConfig(PlanInput{Doc: doc, Ref: "bbbbbbbbbbbbbbbbbbbb", Branch: true, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	auth := branch.Patches[projectconfig.Auth]
	for _, k := range []string{"smtp_pass", "external_gitlab_secret"} {
		if v, present := auth[k]; !present || v != nil {
			t.Errorf("%s = %v (present %v), want it cleared (nil)", k, v, present)
		}
	}
	// A secret config.toml gives the branch is applied as it says.
	if auth["external_keycloak_secret"] != "s3cret" {
		t.Errorf("external_keycloak_secret = %v", auth["external_keycloak_secret"])
	}
	if !hasWarning(branch, "smtp_pass") || !hasWarning(branch, "external_gitlab_secret") || hasWarning(branch, "external_keycloak_secret") {
		t.Errorf("warnings = %v", branch.Warnings)
	}
	// The project itself is configured as the repository says: nothing is cleared there.
	prod, err := PlanConfig(PlanInput{Doc: doc, Ref: "pppppppppppppppppppp", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := prod.Patches[projectconfig.Auth]["smtp_pass"]; present {
		t.Errorf("production had its SMTP password cleared: %v", prod.Patches[projectconfig.Auth])
	}
	// A config.toml that does not name a destination leaves the branch's secrets alone.
	doc2, _ := ParseConfig([]byte("[auth.email.smtp]\nsender_name = \"App\"\n"))
	same, _ := PlanConfig(PlanInput{Doc: doc2, Ref: "bbbbbbbbbbbbbbbbbbbb", Branch: true})
	if _, present := same.Patches[projectconfig.Auth]["smtp_pass"]; present {
		t.Errorf("an SMTP sender name cleared the password: %v", same.Patches[projectconfig.Auth])
	}
}

// `enabled = false` still applies, so a project that has image transformation on can be switched off
// from the repository; `true` is left out with a warning and the rest of [storage] still applies.
func TestConfigTomlAppliesImageTransformationOffAndSkipsOn(t *testing.T) {
	off := plan(t, "[storage]\nfile_size_limit = \"10MiB\"\n[storage.image_transformation]\nenabled = false\n", "refrefrefrefrefrefref", false, nil)
	f, _ := off.Patches[projectconfig.Storage]["features"].(map[string]any)
	if it, _ := f["imageTransformation"].(map[string]any); it["enabled"] != false || !slices.Contains(off.Applied, "storage.image_transformation.enabled") {
		t.Errorf("enabled = false: features %v, applied %v", off.Patches[projectconfig.Storage], off.Applied)
	}
	on := plan(t, "[storage]\nfile_size_limit = \"10MiB\"\n[storage.image_transformation]\nenabled = true\n", "refrefrefrefrefrefref", false, nil)
	if _, has := on.Patches[projectconfig.Storage]["features"]; has || on.Patches[projectconfig.Storage]["fileSizeLimit"] != int64(10<<20) {
		t.Errorf("enabled = true: storage patch %v", on.Patches[projectconfig.Storage])
	}
	if !hasWarning(on, "set it to false") {
		t.Errorf("warnings = %v", on.Warnings)
	}
}

// A new preview branch has no parent credentials (projectconfig.BranchWithheld) and no Edge Function
// secret, so env(NAME) cannot be read on its first run. The node refuses a provider, a captcha or an
// HTTP hook that is switched on without its secret, and that refusal would fail the configure step and
// the run with it. The switch is left out with a warning and the rest of the file applies.
const switchesNeedingSecrets = `
[auth]
enable_signup = false
[auth.external.github]
enabled = true
client_id = "gh-id"
secret = "env(GITHUB_SECRET)"
[auth.external.google]
enabled = true
client_id = "g-id"
secret = "env(GOOGLE_SECRET)"
[auth.external.apple]
enabled = true
client_id = "apple-id"
[auth.external.discord]
enabled = false
client_id = "d-id"
[auth.captcha]
enabled = true
provider = "hcaptcha"
secret = "env(CAPTCHA_SECRET)"
[auth.hook.send_email]
enabled = true
uri = "https://hooks.example.com/mail"
secrets = "env(HOOK_SECRETS)"
[auth.hook.custom_access_token]
enabled = true
uri = "pg-functions://postgres/public/add_claims"
`

func planOfBranch(t *testing.T, cfg string, env map[string]string, held map[string]bool) (*ConfigPlan, error) {
	t.Helper()
	doc, err := ParseConfig([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return PlanConfig(PlanInput{Doc: doc, Ref: "bbbbbbbbbbbbbbbbbbbb", Branch: true,
		Env: func(name string) (string, bool, error) { v, ok := env[name]; return v, ok, nil },
		Held: func(svc projectconfig.Service, key string) (bool, error) {
			if key == "boom" {
				return false, fmt.Errorf("boom")
			}
			return svc == projectconfig.Auth && held[key], nil
		}})
}

func TestAFreshBranchHoldsBackTheSwitchesThatNeedASecretItDoesNotHave(t *testing.T) {
	p, err := planOfBranch(t, switchesNeedingSecrets, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := p.Patches[projectconfig.Auth]
	for _, k := range []string{"external_github_enabled", "external_google_enabled", "security_captcha_enabled", "hook_send_email_enabled",
		"external_github_secret", "external_google_secret", "security_captcha_secret", "hook_send_email_secrets"} {
		if _, in := a[k]; in {
			t.Errorf("%s = %v is in the patch of a branch with no secret", k, a[k])
		}
	}
	// What needs no secret, and everything else in the file, still applies.
	for k, want := range map[string]any{
		"external_github_client_id": "gh-id", "external_google_client_id": "g-id", "security_captcha_provider": "hcaptcha",
		"hook_send_email_uri": "https://hooks.example.com/mail", "disable_signup": true,
		"external_apple_enabled": true, "external_discord_enabled": false,
		"hook_custom_access_token_enabled": true, "hook_custom_access_token_uri": "pg-functions://postgres/public/add_claims",
	} {
		if a[k] != want {
			t.Errorf("%s = %v, want %v", k, a[k], want)
		}
	}
	for _, path := range []string{"auth.external.github.enabled", "auth.external.google.enabled", "auth.captcha.enabled", "auth.hook.send_email.enabled"} {
		if slices.Contains(p.Applied, path) {
			t.Errorf("%s is reported as applied", path)
		}
		if !hasWarning(p, "["+strings.TrimSuffix(path, ".enabled")+"]") && !hasWarning(p, path+" is not applied") {
			t.Errorf("no warning names %s: %v", path, p.Warnings)
		}
	}
	for _, path := range []string{"auth.external.apple.enabled", "auth.external.discord.enabled", "auth.hook.custom_access_token.enabled", "auth.external.github.client_id"} {
		if !slices.Contains(p.Applied, path) {
			t.Errorf("%s is not reported as applied: %v", path, p.Applied)
		}
	}
	if !hasWarning(p, "external_github_secret yet") || !hasWarning(p, "GITHUB_SECRET") {
		t.Errorf("the warnings do not say what to do: %v", p.Warnings)
	}
}

// The switch goes on as soon as the branch has the secret: from config.toml's env(NAME), or one saved on
// the branch itself.
func TestABranchSwitchesOnWhatHasItsSecret(t *testing.T) {
	p, err := planOfBranch(t, switchesNeedingSecrets,
		map[string]string{"GITHUB_SECRET": "gh-s", "CAPTCHA_SECRET": "cap-s", "HOOK_SECRETS": "v1,whsec_x"},
		map[string]bool{"external_google_secret": true})
	if err != nil {
		t.Fatal(err)
	}
	a := p.Patches[projectconfig.Auth]
	for k, want := range map[string]any{
		"external_github_enabled": true, "external_github_secret": "gh-s", "security_captcha_enabled": true, "security_captcha_secret": "cap-s",
		"hook_send_email_enabled": true, "hook_send_email_secrets": "v1,whsec_x",
		"external_google_enabled": true, // the secret is saved on the branch; config.toml's env(GOOGLE_SECRET) could not be read
	} {
		if a[k] != want {
			t.Errorf("%s = %v, want %v", k, a[k], want)
		}
	}
	if _, in := a["external_google_secret"]; in {
		t.Errorf("the saved secret was overwritten: %v", a["external_google_secret"])
	}
	for _, w := range p.Warnings {
		if strings.Contains(w, "is not applied: the branch has no") {
			t.Errorf("a switch with its secret was held back: %s", w)
		}
	}
}

// A secret that config.toml's url cleared (dropInheritedSecrets) is not one the branch has.
func TestABranchDoesNotSwitchOnAProviderWhoseSecretWasCleared(t *testing.T) {
	p, err := planOfBranch(t, "[auth.external.gitlab]\nenabled = true\nclient_id = \"id\"\nurl = \"https://gitlab.attacker.example\"\n", nil,
		map[string]bool{"external_gitlab_secret": true})
	if err != nil {
		t.Fatal(err)
	}
	a := p.Patches[projectconfig.Auth]
	if v, in := a["external_gitlab_secret"]; !in || v != nil {
		t.Errorf("the secret is not cleared: %v", a)
	}
	if _, in := a["external_gitlab_enabled"]; in {
		t.Errorf("a provider with a cleared secret was switched on: %v", a)
	}
}

// The project itself is configured as the repository says: only a branch is held back, and a failure to
// ask the node is an error, not a guess.
func TestOnlyABranchHoldsBackSwitchesAndAnErrorFailsThePlan(t *testing.T) {
	doc, _ := ParseConfig([]byte(switchesNeedingSecrets))
	prod, err := PlanConfig(PlanInput{Doc: doc, Ref: "pppppppppppppppppppp"})
	if err != nil {
		t.Fatal(err)
	}
	a := prod.Patches[projectconfig.Auth]
	if a["external_github_enabled"] != true || a["security_captcha_enabled"] != true || a["hook_send_email_enabled"] != true {
		t.Errorf("the project's switches were held back: %v", a)
	}
	_, err = PlanConfig(PlanInput{Doc: doc, Ref: "bbbbbbbbbbbbbbbbbbbb", Branch: true,
		Held: func(projectconfig.Service, string) (bool, error) { return false, fmt.Errorf("registry down") }})
	if err == nil || !strings.Contains(err.Error(), "registry down") {
		t.Errorf("err = %v", err)
	}
}

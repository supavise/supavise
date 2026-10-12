package projectconfig

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const branchRef = "bbbbbbbbbbbbbbbbbbbb"

func patchRef(t *testing.T, m *Manager, r string, svc Service, body map[string]any) {
	t.Helper()
	if _, err := m.Patch(context.Background(), r, svc, body, CrossContext{}); err != nil {
		t.Fatalf("patch %s %v: %v", svc, body, err)
	}
}

// A branch takes the parent's saved settings and none of the ones that identify, address or size the
// parent, and no credential. This is the golden list of what is left out for being host-bound or sizing.
func TestCopyForBranchSkipsWhatIdentifiesAddressesOrSizesTheParent(t *testing.T) {
	m, st := newManager(t)
	ctx := context.Background()
	patchRef(t, m, ref, Auth, map[string]any{
		"site_url": "https://app.example.com", "uri_allow_list": "https://app.example.com/**",
		"saml_external_url": "https://saml.example.com", "webauthn_rp_id": "example.com", "webauthn_rp_origins": "https://app.example.com",
		"webauthn_rp_display_name": "Example", "passkey_enabled": true, "mfa_web_authn_enroll_enabled": true,
		"db_max_pool_size": 40, "db_max_pool_size_unit": "connections",
		"external_github_enabled": true, "external_github_client_id": "gh-id", "external_github_secret": "gh-secret",
		"smtp_host": "smtp.example.com", "smtp_port": "587", "smtp_user": "mailer", "smtp_pass": "smtp-pass", "smtp_admin_email": "ops@example.com",
		"mfa_totp_enroll_enabled": false, "mfa_max_enrolled_factors": 3, "disable_signup": true,
	})
	patchRef(t, m, ref, PostgREST, map[string]any{"db_schema": "public,api", "max_rows": 250, "db_pool": 30})
	patchRef(t, m, ref, Realtime, map[string]any{"max_concurrent_users": 500, "connection_pool": 9, "suspend": true})
	patchRef(t, m, ref, Storage, map[string]any{"fileSizeLimit": 1048576})
	patchRef(t, m, ref, Postgres, map[string]any{"statement_timeout": "30s", "log_connections": true, "shared_buffers": "512MB", "work_mem": "32MB", "max_connections": 120})
	patchRef(t, m, ref, Pooler, map[string]any{"default_pool_size": 20})

	res, err := m.CopyForBranch(ctx, ref, branchRef, BranchCopyOptions{Seeded: true})
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}

	set := func(svc Service) Values {
		t.Helper()
		s, err := m.Get(ctx, branchRef, svc)
		if err != nil {
			t.Fatal(err)
		}
		return s.Set
	}
	auth := set(Auth)
	for _, name := range BranchExcluded(Auth) {
		if _, ok := auth[name]; ok {
			t.Errorf("auth: %s was copied", name)
		}
	}
	// What a user set up for sign-in comes along: the provider's client id, the sender's address, MFA
	// and sign-up policy. The credentials and the connections they belong to do not (the next test).
	for name, want := range map[string]any{
		"external_github_client_id": "gh-id", "smtp_admin_email": "ops@example.com",
		"mfa_totp_enroll_enabled": false, "mfa_max_enrolled_factors": int64(3), "disable_signup": true, "webauthn_rp_display_name": "Example",
	} {
		if got := auth[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("auth %s = %#v, want %#v", name, got, want)
		}
	}
	for _, name := range []string{"external_github_enabled", "external_github_secret", "smtp_host", "smtp_user", "smtp_pass"} {
		if _, ok := auth[name]; ok {
			t.Errorf("auth %s was copied", name)
		}
	}
	// No sealed value reaches the branch's record.
	rec, _ := st.Get(ctx, branchRef, Auth)
	if len(rec.Sealed) != 0 {
		t.Errorf("sealed secrets of the branch: %v", keys(rec.Sealed))
	}
	if got := set(PostgREST); got["db_schema"] != "public,api" || got["max_rows"] != int64(250) {
		t.Errorf("postgrest = %v", got)
	} else if _, ok := got["db_pool"]; ok {
		t.Errorf("postgrest db_pool was copied")
	}
	if got := set(Realtime); got["max_concurrent_users"] != int64(500) {
		t.Errorf("realtime = %v", got)
	} else if _, ok := got["suspend"]; ok || got["connection_pool"] != nil {
		t.Errorf("realtime tenant state or pool was copied: %v", got)
	}
	if got := set(Storage); got["fileSizeLimit"] != int64(1048576) {
		t.Errorf("storage = %v", got)
	}
	pg := set(Postgres)
	if pg["statement_timeout"] != "30s" || pg["log_connections"] != true {
		t.Errorf("postgres = %v", pg)
	}
	for _, k := range SizingSettings {
		if _, ok := pg[k]; ok {
			t.Errorf("postgres sizing %s was copied", k)
		}
	}
	if got := set(Pooler); len(got) != 0 {
		t.Errorf("the pooler's settings were copied: %v", got)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The exclusion list names real settings, and every setting that looks like an address is either on
// it or has been looked at and kept. A new setting of that kind makes this test fail until someone
// decides which.
func TestEverySettingThatLooksLikeAnAddressIsDecided(t *testing.T) {
	excluded := map[Service]map[string]bool{}
	for _, svc := range []Service{Auth, PostgREST, Realtime, Storage, Postgres} {
		excluded[svc] = map[string]bool{}
		sch, err := schemaOf(svc)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range BranchExcluded(svc) {
			excluded[svc][n] = true
			if _, ok := sch.Field(n); !ok {
				t.Errorf("%s: %q is excluded but is not a setting", svc, n)
			}
		}
	}
	// Looked at and kept: they point at something that is not the parent's host (a provider's tenant), or
	// are not addresses at all. The SMTP host and the hooks' URIs are withheld (BranchWithheldNames): they
	// reach the outside with the parent's credentials.
	kept := map[string]bool{
		"external_azure_url": true, "external_gitlab_url": true, "external_keycloak_url": true, "external_workos_url": true,
		"oauth_server_authorization_path": true,
	}
	for _, n := range BranchWithheldNames(Auth) {
		if kept[n] {
			t.Errorf("auth %s is both kept and withheld", n)
		}
		kept[n] = true
	}
	words := map[string]bool{"url": true, "uri": true, "origin": true, "origins": true, "host": true, "hostname": true, "domain": true, "redirect": true, "callback": true}
	for _, f := range AuthSchema.Fields {
		for _, tok := range strings.Split(strings.ToLower(f.Name), "_") {
			if words[tok] && !excluded[Auth][f.Name] && !kept[f.Name] {
				t.Errorf("auth setting %s looks host-bound: copy it (kept) or exclude it", f.Name)
			}
		}
	}
	// Nothing named like the keys a branch must have of its own is a setting.
	for _, svc := range Services {
		sch, _ := schemaOf(svc)
		for _, f := range sch.Fields {
			n := strings.ToLower(f.Name)
			if n == "jwt_secret" || strings.Contains(n, "service_role") || strings.Contains(n, "anon_key") || strings.Contains(n, "db_password") {
				t.Errorf("%s: %s is a setting; a branch must never copy it", svc, f.Name)
			}
		}
	}
}

// Passkeys and WebAuthn MFA cannot run without the relying party that was left out, so they stay off.
func TestTheBranchCopyNeverLeavesGoTrueUnableToStart(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	patchRef(t, m, ref, Auth, map[string]any{"webauthn_rp_id": "example.com", "webauthn_rp_origins": "https://app.example.com", "webauthn_rp_display_name": "Example",
		"passkey_enabled": true, "mfa_web_authn_verify_enabled": true})
	res, err := m.CopyForBranch(ctx, ref, branchRef, BranchCopyOptions{Seeded: true})
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	if err := m.CheckSaved(ctx, branchRef, Auth, CrossContext{}); err != nil {
		t.Fatalf("the branch's settings do not pass the checks GoTrue's start needs: %v", err)
	}
}

func TestCopyForBranchWithNothingSavedWritesNothing(t *testing.T) {
	m, st := newManager(t)
	res, err := m.CopyForBranch(context.Background(), ref, branchRef, BranchCopyOptions{})
	if err != nil || len(res.Copied) != 0 || len(res.Withheld) != 0 || len(res.Deferred) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	if rec, _ := st.Get(context.Background(), branchRef, Auth); rec.Version != 0 {
		t.Fatalf("a record was written: %+v", rec)
	}
}

// hookSecret is a secret in the form GoTrue accepts for a hook.
const hookSecret = "v1,whsec_aGVsbG8gd29ybGQgdGhpcyBpcyBhIHRlc3Qgc2VjcmV0IQ=="

// A parent with every kind of outbound provider set up: the branch gets the settings that are not
// credentials, none of the credentials, and no connection that would be left half set up, and what it
// gets passes the checks GoTrue's start needs.
func TestCopyForBranchWithholdsCredentialsAndTheConnectionsTheyBelongTo(t *testing.T) {
	m, st := newManager(t)
	ctx := context.Background()
	patchRef(t, m, ref, Auth, map[string]any{
		"smtp_host": "smtp.example.com", "smtp_port": "587", "smtp_user": "mailer", "smtp_pass": "smtp-pass",
		"smtp_admin_email": "ops@example.com", "smtp_sender_name": "Example", "smtp_max_frequency": 30,
		"sms_provider": "twilio", "sms_twilio_account_sid": "AC123", "sms_twilio_auth_token": "tok", "sms_twilio_message_service_sid": "MG1",
		"sms_vonage_api_key": "vk", "sms_vonage_api_secret": "vs", "sms_messagebird_access_key": "mb", "sms_textlocal_api_key": "tl",
		"sms_twilio_verify_account_sid": "AC9", "sms_twilio_verify_auth_token": "vtok",
		"sms_autoconfirm": false, "sms_otp_length": 8, "sms_test_otp": "15551234567=123456", "sms_template": "Code {{ .Code }}",
		"external_github_enabled": true, "external_github_client_id": "gh-id", "external_github_secret": "gh-secret", "external_github_email_optional": true,
		"external_apple_enabled": true, "external_apple_client_id": "apple-id", "external_apple_additional_client_ids": "a,b",
		"external_gitlab_enabled": true, "external_gitlab_client_id": "gl", "external_gitlab_secret": "gls", "external_gitlab_url": "https://gitlab.example.com",
		"security_captcha_enabled": true, "security_captcha_provider": "hcaptcha", "security_captcha_secret": "cap-secret",
		"hook_send_email_enabled": true, "hook_send_email_uri": "https://hooks.example.com/mail", "hook_send_email_secrets": hookSecret,
		"hook_custom_access_token_enabled": true, "hook_custom_access_token_uri": "pg-functions://postgres/public/add_claims",
		"hook_send_sms_uri": "https://hooks.example.com/sms", "hook_send_sms_secrets": hookSecret,
		"mailer_autoconfirm": false, "external_phone_enabled": true, "mfa_phone_enroll_enabled": true,
	})

	res, err := m.CopyForBranch(ctx, ref, branchRef, BranchCopyOptions{Seeded: true})
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	got, err := m.Get(ctx, branchRef, Auth)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get(ctx, branchRef, Auth)
	if len(rec.Sealed) != 0 {
		t.Errorf("sealed secrets reached the branch: %v", keys(rec.Sealed))
	}

	// The golden list of what the parent had and the branch did not get.
	wantWithheld := []string{
		"external_apple_enabled", "external_github_enabled", "external_github_secret", "external_gitlab_enabled", "external_gitlab_secret",
		"hook_send_email_enabled", "hook_send_email_secrets", "hook_send_email_uri", "hook_send_sms_secrets", "hook_send_sms_uri",
		"security_captcha_enabled", "security_captcha_secret",
		"sms_messagebird_access_key", "sms_provider", "sms_textlocal_api_key", "sms_twilio_account_sid", "sms_twilio_auth_token",
		"sms_twilio_message_service_sid", "sms_twilio_verify_account_sid", "sms_twilio_verify_auth_token", "sms_vonage_api_key", "sms_vonage_api_secret",
		"smtp_host", "smtp_pass", "smtp_port", "smtp_user",
	}
	if !reflect.DeepEqual(res.Withheld[Auth], wantWithheld) {
		t.Errorf("withheld = %v\nwant     %v", res.Withheld[Auth], wantWithheld)
	}
	for _, name := range wantWithheld {
		if _, ok := got.Set[name]; ok {
			t.Errorf("auth %s was copied", name)
		}
	}

	// What stays: the provider's other settings, the identity of the sender, the policies, a hook that runs
	// in the branch's own database, and the switches that are not providers.
	for name, want := range map[string]any{
		"external_github_client_id": "gh-id", "external_github_email_optional": true, "external_apple_client_id": "apple-id", "external_apple_additional_client_ids": "a,b",
		"external_gitlab_client_id": "gl", "external_gitlab_url": "https://gitlab.example.com",
		"smtp_admin_email": "ops@example.com", "smtp_sender_name": "Example", "smtp_max_frequency": int64(30),
		"sms_autoconfirm": false, "sms_otp_length": int64(8), "sms_test_otp": "15551234567=123456", "sms_template": "Code {{ .Code }}",
		"security_captcha_provider":        "hcaptcha",
		"hook_custom_access_token_enabled": true, "hook_custom_access_token_uri": "pg-functions://postgres/public/add_claims",
		"mailer_autoconfirm": false, "external_phone_enabled": true, "mfa_phone_enroll_enabled": true,
	} {
		if v := got.Set[name]; !reflect.DeepEqual(v, want) {
			t.Errorf("auth %s = %#v, want %#v", name, v, want)
		}
	}
	// Nothing is half set up: GoTrue's start checks pass, and no provider is on without its secret.
	if err := m.CheckSaved(ctx, branchRef, Auth, CrossContext{}); err != nil {
		t.Fatalf("the branch's settings do not pass the checks GoTrue's start needs: %v", err)
	}
	env, err := m.AuthEnv(ctx, branchRef, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"GOTRUE_SMTP_HOST", "GOTRUE_SMTP_PASS", "GOTRUE_EXTERNAL_GITHUB_SECRET", "GOTRUE_SMS_PROVIDER", "GOTRUE_SMS_TWILIO_AUTH_TOKEN",
		"GOTRUE_SECURITY_CAPTCHA_SECRET", "GOTRUE_HOOK_SEND_EMAIL_URI", "GOTRUE_HOOK_SEND_EMAIL_SECRETS"} {
		if env[k] != "" {
			t.Errorf("the branch's GoTrue is given %s=%q", k, env[k])
		}
	}
	for _, k := range []string{"GOTRUE_EXTERNAL_GITHUB_ENABLED", "GOTRUE_EXTERNAL_APPLE_ENABLED", "GOTRUE_SECURITY_CAPTCHA_ENABLED", "GOTRUE_HOOK_SEND_EMAIL_ENABLED"} {
		if env[k] == "true" {
			t.Errorf("the branch's GoTrue is given %s=true", k)
		}
	}
	if env["GOTRUE_HOOK_CUSTOM_ACCESS_TOKEN_ENABLED"] != "true" {
		t.Errorf("the hook that runs in the branch's database was not rendered: %v", env["GOTRUE_HOOK_CUSTOM_ACCESS_TOKEN_ENABLED"])
	}
}

// The parent's settings are not changed by the copy, and a parent that never saved a credential has
// nothing listed as withheld.
func TestCopyForBranchListsOnlyWhatTheParentHad(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	patchRef(t, m, ref, Auth, map[string]any{"disable_signup": true, "smtp_admin_email": "ops@example.com"})
	res, err := m.CopyForBranch(ctx, ref, branchRef, BranchCopyOptions{Seeded: true})
	if err != nil || len(res.Failed) != 0 || len(res.Withheld) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	patchRef(t, m, ref, Auth, map[string]any{"smtp_host": "smtp.example.com", "smtp_pass": "p"})
	st, _ := m.Get(ctx, ref, Auth)
	if st.Set["smtp_pass"] != "p" || st.Set["smtp_host"] != "smtp.example.com" {
		t.Fatalf("the parent's own settings changed: %v", st.Set)
	}
	if got := BranchWithheld(Auth, st.Set); !reflect.DeepEqual(got, []string{"smtp_host", "smtp_pass"}) {
		t.Errorf("withheld = %v", got)
	}
}

// A hook that runs a function in the branch's own database is kept; one that calls out is not, and
// neither is the hook secret of either.
func TestBranchWithheldKeepsOnlyHooksThatRunInTheBranchsDatabase(t *testing.T) {
	set := Values{
		"hook_custom_access_token_uri": "pg-functions://postgres/public/f", "hook_custom_access_token_enabled": true, "hook_custom_access_token_secrets": hookSecret,
		"hook_send_email_uri": "https://example.com/h", "hook_send_email_enabled": true,
		"hook_send_sms_uri": "http://localhost:9999/h",
	}
	want := []string{"hook_custom_access_token_secrets", "hook_send_email_enabled", "hook_send_email_uri", "hook_send_sms_uri"}
	if got := BranchWithheld(Auth, set); !reflect.DeepEqual(got, want) {
		t.Fatalf("withheld = %v, want %v", got, want)
	}
	kept := BranchSettings(Auth, set)
	if kept["hook_custom_access_token_uri"] == nil || kept["hook_custom_access_token_enabled"] != true || len(kept) != 2 {
		t.Fatalf("kept = %v", kept)
	}
}

// Every name the copy withholds is a setting, every sealed secret of every service is withheld, and
// the settings that connect a provider are listed whole. A new secret or provider setting makes this
// fail until someone decides what a branch does with it.
func TestBranchWithheldNamesCoverEverySecretAndProviderConnection(t *testing.T) {
	for _, svc := range Services {
		sch, _ := schemaOf(svc)
		names := map[string]bool{}
		for _, n := range BranchWithheldNames(svc) {
			names[n] = true
			if _, ok := sch.Field(n); !ok {
				t.Errorf("%s: %q is withheld but is not a setting", svc, n)
			}
		}
		for _, f := range sch.Fields {
			if f.Secret && !names[f.Name] {
				t.Errorf("%s: secret %s is not withheld", svc, f.Name)
			}
		}
		if svc != Auth && len(names) != 0 {
			t.Errorf("%s withholds %v; only Auth has credentials", svc, names)
		}
	}
	// The settings of a provider that are not sealed but name an account, a host or a switch.
	want := []string{
		"hook_after_user_created_enabled", "hook_after_user_created_uri", "hook_before_user_created_enabled", "hook_before_user_created_uri",
		"hook_custom_access_token_enabled", "hook_custom_access_token_uri", "hook_mfa_verification_attempt_enabled", "hook_mfa_verification_attempt_uri",
		"hook_password_verification_attempt_enabled", "hook_password_verification_attempt_uri", "hook_send_email_enabled", "hook_send_email_uri",
		"hook_send_sms_enabled", "hook_send_sms_uri",
		"mailer_allow_unverified_email_sign_ins", "security_captcha_enabled", "smtp_host", "smtp_port", "smtp_user",
		"sms_messagebird_originator", "sms_provider", "sms_textlocal_sender", "sms_twilio_account_sid", "sms_twilio_content_sid", "sms_twilio_message_service_sid",
		"sms_twilio_verify_account_sid", "sms_twilio_verify_message_service_sid", "sms_vonage_api_key", "sms_vonage_from",
	}
	for _, p := range authProviders {
		want = append(want, "external_"+p.name+"_enabled")
	}
	sch, _ := schemaOf(Auth)
	var got []string
	for _, n := range BranchWithheldNames(Auth) {
		if f, _ := sch.Field(n); !f.Secret {
			got = append(got, n)
		}
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("withheld names that are not secrets:\n got %v\nwant %v", got, want)
	}
}

// A parent that never saved mailer_autoconfirm has confirmation on because it has SMTP, and
// mailer_allow_unverified_email_sign_ins is valid there. The branch has no SMTP host, so it runs with
// auto-confirm on, and GoTrue refuses the combination: the copy would be refused whole and the branch
// would lose its disable_signup, MFA and expiry settings. The setting is withheld instead.
func TestCopyForBranchWithholdsUnverifiedSignInsWhenTheParentGetsConfirmationFromSMTP(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	patchRef(t, m, ref, Auth, map[string]any{
		"smtp_host": "smtp.example.com", "smtp_admin_email": "ops@example.com",
		"mailer_allow_unverified_email_sign_ins": true, "disable_signup": true, "jwt_exp": 1800, "mfa_max_enrolled_factors": 3,
	})
	if err := m.CheckSaved(ctx, ref, Auth, CrossContext{}); err != nil {
		t.Fatalf("the parent's own settings are valid: %v", err)
	}
	res, err := m.CopyForBranch(ctx, ref, branchRef, BranchCopyOptions{Seeded: true})
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	want := []string{"mailer_allow_unverified_email_sign_ins", "smtp_host"}
	if !reflect.DeepEqual(res.Withheld[Auth], want) {
		t.Errorf("withheld = %v, want %v", res.Withheld[Auth], want)
	}
	got, _ := m.Get(ctx, branchRef, Auth)
	if got.Set["disable_signup"] != true || got.Set["jwt_exp"] != int64(1800) || got.Set["mfa_max_enrolled_factors"] != int64(3) || got.Set["smtp_admin_email"] != "ops@example.com" {
		t.Errorf("the rest of the Auth copy did not arrive: %v", got.Set)
	}
	if _, ok := got.Set["mailer_allow_unverified_email_sign_ins"]; ok {
		t.Error("unverified sign-ins were copied next to auto-confirm")
	}
	if !got.AuthAutoconfirm() {
		t.Error("a branch with no SMTP host should run with auto-confirm on")
	}
	if err := m.CheckSaved(ctx, branchRef, Auth, CrossContext{}); err != nil {
		t.Fatalf("the branch's settings do not pass the checks GoTrue's start needs: %v", err)
	}

	// A parent that saved mailer_autoconfirm = false has said what it wants; the branch gets exactly that.
	const other = "cccccccccccccccccccc"
	const parent2 = "dddddddddddddddddddd"
	patchRef(t, m, parent2, Auth, map[string]any{
		"smtp_host": "smtp.example.com", "smtp_admin_email": "ops@example.com", "mailer_autoconfirm": false, "mailer_allow_unverified_email_sign_ins": true,
	})
	res, err = m.CopyForBranch(ctx, parent2, other, BranchCopyOptions{Seeded: true})
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	if got, _ := m.Get(ctx, other, Auth); got.Set["mailer_allow_unverified_email_sign_ins"] != true || got.Set["mailer_autoconfirm"] != false {
		t.Errorf("an explicit confirmation setting was not copied: %v", got.Set)
	}
}

// PostgREST does not start on a schema that does not exist, so a branch that starts with an empty
// database does not take the parent's list yet; one that starts from the parent's data does.
func TestCopyForBranchDefersTheExposedSchemasOfAnEmptyBranch(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	patchRef(t, m, ref, PostgREST, map[string]any{"db_schema": "public,api", "max_rows": 250})

	res, err := m.CopyForBranch(ctx, ref, branchRef, BranchCopyOptions{})
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	st, _ := m.Get(ctx, branchRef, PostgREST)
	if _, ok := st.Set["db_schema"]; ok || st.Set["max_rows"] != int64(250) {
		t.Errorf("an empty branch's PostgREST settings = %v, want max_rows only", st.Set)
	}
	if !reflect.DeepEqual(res.Deferred[PostgREST], []string{"db_schema"}) || !reflect.DeepEqual(res.Copied[PostgREST], []string{"max_rows"}) {
		t.Errorf("deferred %v, copied %v", res.Deferred, res.Copied)
	}

	const other = "cccccccccccccccccccc"
	res, err = m.CopyForBranch(ctx, ref, other, BranchCopyOptions{Seeded: true})
	if err != nil || len(res.Failed) != 0 || len(res.Deferred) != 0 {
		t.Fatalf("seeded copy: %+v, %v", res, err)
	}
	if st, _ := m.Get(ctx, other, PostgREST); st.Set["db_schema"] != "public,api" {
		t.Errorf("a branch with data's PostgREST settings = %v", st.Set)
	}

	// A parent that saved no list defers nothing.
	const third = "dddddddddddddddddddd"
	patchRef(t, m, "eeeeeeeeeeeeeeeeeeee", PostgREST, map[string]any{"max_rows": 5})
	if res, err = m.CopyForBranch(ctx, "eeeeeeeeeeeeeeeeeeee", third, BranchCopyOptions{}); err != nil || len(res.Deferred) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
}

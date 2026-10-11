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

// A branch takes the parent's saved settings, secrets included, and none of the ones that identify,
// address or size the parent. This is the golden list of what is left out.
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

	res, err := m.CopyForBranch(ctx, ref, branchRef)
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
	// What a user set up for sign-in comes along, secrets in the clear after opening.
	for name, want := range map[string]any{
		"external_github_enabled": true, "external_github_client_id": "gh-id", "external_github_secret": "gh-secret",
		"smtp_host": "smtp.example.com", "smtp_pass": "smtp-pass", "smtp_admin_email": "ops@example.com", "smtp_user": "mailer",
		"mfa_totp_enroll_enabled": false, "mfa_max_enrolled_factors": int64(3), "disable_signup": true, "webauthn_rp_display_name": "Example",
	} {
		if got := auth[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("auth %s = %#v, want %#v", name, got, want)
		}
	}
	// The secrets are sealed in the branch's record as they are in the parent's.
	rec, _ := st.Get(ctx, branchRef, Auth)
	if len(rec.Sealed["smtp_pass"]) == 0 || len(rec.Sealed["external_github_secret"]) == 0 {
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
	// Looked at and kept: they point at something that is not the parent's host (a provider, an SMTP
	// server, a hook that runs in the database or calls out), or are not addresses at all.
	kept := map[string]bool{
		"external_azure_url": true, "external_gitlab_url": true, "external_keycloak_url": true, "external_workos_url": true,
		"smtp_host": true, "oauth_server_authorization_path": true, "sms_messagebird_originator": true,
	}
	for _, h := range authHooks {
		kept["hook_"+h+"_uri"] = true
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
	res, err := m.CopyForBranch(ctx, ref, branchRef)
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	if err := m.CheckSaved(ctx, branchRef, Auth, CrossContext{}); err != nil {
		t.Fatalf("the branch's settings do not pass the checks GoTrue's start needs: %v", err)
	}
}

func TestCopyForBranchWithNothingSavedWritesNothing(t *testing.T) {
	m, st := newManager(t)
	res, err := m.CopyForBranch(context.Background(), ref, branchRef)
	if err != nil || len(res.Copied) != 0 {
		t.Fatalf("copy: %+v, %v", res, err)
	}
	if rec, _ := st.Get(context.Background(), branchRef, Auth); rec.Version != 0 {
		t.Fatalf("a record was written: %+v", rec)
	}
}

package projectconfig

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// SizingSettings are the Postgres settings a project's compute class decides (they sit on the
// postmaster's command line, ahead of anything saved or inherited). A branch has its own class, so
// it never takes them from its parent. lifecycle's command-line list is the same
// (a test in internal/lifecycle compares them).
var SizingSettings = []string{
	"shared_buffers", "effective_cache_size", "work_mem", "maintenance_work_mem", "max_wal_size",
	"max_connections", "max_wal_senders", "max_replication_slots", "max_worker_processes",
}

// branchExcluded lists, per service, the settings a branch does not copy from its parent. The JWT
// secret, the API keys and the database password are not settings at all: a branch always has its
// own (internal/secrets), so they cannot be copied by this.
//
//   - Host-bound: they name the parent's address, and the branch has its own host. site_url and
//     uri_allow_list (the redirect allow list), saml_external_url, and the WebAuthn relying party
//     (id and origins), which passkeys and WebAuthn MFA cannot do without, so those switches stay
//     off until the branch is set up.
//   - Sizing: pool sizes and the Postgres settings of SizingSettings; the Supavisor tenant of the
//     branch (the whole Pooler service) is sized for the branch's class.
//   - State of the tenant: suspend.
//
// The custom hostname and the vanity subdomain are not settings either (internal/domains); they are
// never copied.
var branchExcluded = map[Service][]string{
	Auth: {
		"site_url", "uri_allow_list", "saml_external_url", "webauthn_rp_id", "webauthn_rp_origins",
		"passkey_enabled", "mfa_web_authn_enroll_enabled", "mfa_web_authn_verify_enabled",
		"db_max_pool_size", "db_max_pool_size_unit",
	},
	PostgREST: {"db_pool", "db_pool_acquisition_timeout"},
	Realtime:  {"connection_pool", "postgres_changes_pool", "suspend"},
	Postgres:  SizingSettings,
}

// branchServices are the services whose settings a branch starts with.
var branchServices = []Service{Auth, PostgREST, Realtime, Storage, Postgres}

// BranchExcluded returns the names of the settings of svc that a branch does not copy, sorted.
func BranchExcluded(svc Service) []string {
	out := append([]string{}, branchExcluded[svc]...)
	if svc == Pooler {
		return []string{"*"}
	}
	sort.Strings(out)
	return out
}

// smsProviders are the SMS providers GoTrue can send through: the settings of one are named
// sms_<provider>_*.
var smsProviders = []string{"messagebird", "textlocal", "twilio", "vonage"}

// branchWithheld names the settings of svc that a branch never copies because they hold, or reach
// the outside with, a credential of the parent. They are not host-bound or sizing (branchExcluded);
// the branch's owner sets its own, as hosted keeps the secrets of each branch apart. A branch does
// not get:
//
//   - every sealed secret (a setting of Secret kind): the SMTP password, the SMS providers' tokens
//     and keys, the OAuth clients' secrets, the captcha secret, the hook secrets;
//   - the connection a secret belongs to, so that no provider is left half set up: the SMTP host,
//     port and user; the SMS provider choice and every sms_<provider>_* setting, identifiers
//     included; the HTTP(S) hooks (uri and switch; a hook that runs a function in the branch's own
//     database is kept); the switch of every external OAuth provider (the client id and the other
//     provider settings stay); the captcha switch (the provider choice stays);
//   - mailer_allow_unverified_email_sign_ins, when the parent gets its confirmation setting from
//     having SMTP: with no mailer_autoconfirm saved, auto-confirm is on exactly while smtp_host is
//     empty, and GoTrue refuses unverified sign-ins next to auto-confirm. The branch has no SMTP host,
//     so it runs with auto-confirm on and the setting would mean nothing there.
//
// Names that validation ties together go together: GoTrue refuses an enabled provider without its
// secret, a captcha without its secret, an HTTP hook without its secrets and unverified sign-ins
// next to auto-confirm, and a refused patch would lose the branch's whole Auth copy.
//
// The names are those of the schema, whether the parent saved them or not; only the hooks and the
// confirmation setting look at set: the hooks for the scheme of the hook's uri, the confirmation
// setting for whether the parent saved mailer_autoconfirm and an SMTP host.
func branchWithheld(svc Service, set Values) map[string]bool {
	out := map[string]bool{}
	sch, err := schemaOf(svc)
	if err != nil {
		return out
	}
	for _, f := range sch.Fields {
		if f.Secret {
			out[f.Name] = true
		}
	}
	if svc != Auth {
		return out
	}
	for _, n := range []string{"smtp_host", "smtp_port", "smtp_user", "sms_provider", "security_captcha_enabled"} {
		out[n] = true
	}
	for _, f := range sch.Fields {
		for _, p := range smsProviders {
			if strings.HasPrefix(f.Name, "sms_"+p+"_") {
				out[f.Name] = true
			}
		}
	}
	for _, p := range authProviders {
		out["external_"+p.name+"_enabled"] = true
	}
	for _, h := range authHooks {
		if uri, _ := set["hook_"+h+"_uri"].(string); uri != "" && !strings.HasPrefix(strings.ToLower(uri), "pg-functions:") {
			out["hook_"+h+"_uri"], out["hook_"+h+"_enabled"] = true, true
		}
	}
	if _, saved := set["mailer_autoconfirm"]; !saved && set.Str("smtp_host") != "" {
		out["mailer_allow_unverified_email_sign_ins"] = true
	}
	return out
}

// BranchWithheld returns the names, sorted, of the settings in set that a branch does not copy from
// its parent because they are credentials or reach the outside with one (branchWithheld).
func BranchWithheld(svc Service, set Values) []string {
	var out []string
	for name := range branchWithheld(svc, set) {
		if _, ok := set[name]; ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// BranchWithheldNames returns every name of svc a branch can be denied, sorted: what BranchWithheld
// gives for a parent that has saved all of them, every hook pointing at an HTTPS URL and an SMTP host
// with no mailer_autoconfirm. The docs list it, and a test keeps the list in step with the schema.
func BranchWithheldNames(svc Service) []string {
	every := Values{"smtp_host": "smtp.example.com"}
	for _, h := range authHooks {
		every["hook_"+h+"_uri"] = "https://example.com/hook"
	}
	var out []string
	for name := range branchWithheld(svc, every) {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// BranchSettings returns the part of a service's saved settings (plain values, secrets in the clear)
// that a branch starts with: everything but BranchExcluded and BranchWithheld. The Pooler service is
// not copied at all.
func BranchSettings(svc Service, set Values) map[string]any {
	skip := map[string]bool{}
	for _, n := range branchExcluded[svc] {
		skip[n] = true
	}
	for n := range branchWithheld(svc, set) {
		skip[n] = true
	}
	out := map[string]any{}
	for name, v := range set {
		if !skip[name] {
			out[name] = v
		}
	}
	return out
}

// BranchCopy reports what CopyForBranch did, by service.
type BranchCopy struct {
	// Copied are the names of the settings written to the branch.
	Copied map[Service][]string
	// Withheld are the names of the settings the parent had saved and the branch did not get because
	// they are credentials or reach the outside with one (BranchWithheld). The branch's owner sets its
	// own. The values are never listed.
	Withheld map[Service][]string
	// Deferred are the names of the settings the branch does not get yet: PostgREST's db_schema for a
	// branch whose database starts empty (BranchCopyOptions.Seeded false). The caller applies them once
	// the schemas exist.
	Deferred map[Service][]string
	// Failed are services whose settings the branch refused (the branch keeps its defaults for them).
	Failed map[Service]string
}

// BranchCopyOptions say what the branch being made starts from.
type BranchCopyOptions struct {
	// Seeded is true when the branch's database is a copy of the parent's data (a clone or a base
	// backup): the schemas the parent exposes through PostgREST exist when PostgREST first starts. When
	// it is false the database starts empty, PostgREST would not start on a schema that does not exist
	// yet, and db_schema is deferred (BranchCopy.Deferred).
	Seeded bool
}

// CopyForBranch gives the project `to` (a branch that has just been registered) the saved settings
// of the project `from` (its parent), except the ones a branch never copies: what identifies,
// addresses or sizes the parent (BranchExcluded) and the credentials and the connections that carry
// them (BranchWithheld: the SMTP password, the SMS and OAuth secrets, the hook secrets, the captcha
// secret). A branch has egress to the outside in its GoTrue even when its database has none, and a
// sealed value binds to no project, so a copied credential would let the branch send mail or SMS or
// call a hook as the parent. The values go through the same validation as a save, so a combination
// GoTrue would refuse at start is not written; a service whose copy is refused keeps its defaults and
// is named in Failed.
func (m *Manager) CopyForBranch(ctx context.Context, from, to string, o BranchCopyOptions) (*BranchCopy, error) {
	res := &BranchCopy{Copied: map[Service][]string{}, Withheld: map[Service][]string{}, Deferred: map[Service][]string{}, Failed: map[Service]string{}}
	for _, svc := range branchServices {
		st, err := m.Get(ctx, from, svc)
		if err != nil {
			return res, err
		}
		if w := BranchWithheld(svc, st.Set); len(w) > 0 {
			res.Withheld[svc] = w
		}
		patch := BranchSettings(svc, st.Set)
		if _, ok := patch[PostgRESTExposedSchemas]; svc == PostgREST && ok && !o.Seeded {
			delete(patch, PostgRESTExposedSchemas)
			res.Deferred[svc] = []string{PostgRESTExposedSchemas}
		}
		if len(patch) == 0 {
			continue
		}
		ch, err := m.Patch(ctx, to, svc, patch, CrossContext{})
		if err != nil {
			var verr *ValidationError
			if errors.As(err, &verr) {
				res.Failed[svc] = err.Error()
				continue
			}
			return res, err
		}
		names := append([]string{}, ch.Changed...)
		sort.Strings(names)
		res.Copied[svc] = names
	}
	return res, nil
}

// PostgRESTExposedSchemas is the name of PostgREST's setting for the schemas the Data API exposes.
const PostgRESTExposedSchemas = "db_schema"

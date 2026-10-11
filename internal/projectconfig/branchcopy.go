package projectconfig

import (
	"context"
	"errors"
	"sort"
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

// BranchSettings returns the part of a service's saved settings (plain values, secrets in the clear)
// that a branch starts with: everything but BranchExcluded. The Pooler service is not copied at all.
func BranchSettings(svc Service, set Values) map[string]any {
	skip := map[string]bool{}
	for _, n := range branchExcluded[svc] {
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
	// Failed are services whose settings the branch refused (the branch keeps its defaults for them).
	Failed map[Service]string
}

// CopyForBranch gives the project `to` (a branch that has just been registered) the saved settings
// of the project `from` (its parent), except the ones a branch never copies (BranchExcluded).
// Secrets are copied too (SMTP password, OAuth client secrets, hook secrets, the captcha secret):
// sealing does not bind a value to a project, and a branch of a project with data cannot reach the
// outside to use them (its egress is denied). The values go through the same validation as a save, so
// a combination GoTrue would refuse at start is not written; a service whose copy is refused keeps
// its defaults and is named in Failed.
func (m *Manager) CopyForBranch(ctx context.Context, from, to string) (*BranchCopy, error) {
	res := &BranchCopy{Copied: map[Service][]string{}, Failed: map[Service]string{}}
	for _, svc := range branchServices {
		st, err := m.Get(ctx, from, svc)
		if err != nil {
			return res, err
		}
		patch := BranchSettings(svc, st.Set)
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

package api

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	v1 "github.com/supavise/supavise/internal/api/gen/v1"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
)

// disabledFeatures are the dashboard features hidden through profile.disabled_features:
// billing, and the hosted-only infrastructure and integrations supavise does not offer.
// Keys are Studio's enabled-features keys (packages/common/enabled-features).
var disabledFeatures = []string{
	"billing:all", "database:replication", featureRestoreToNewProject, "database:network_restrictions",
	featureReadReplicas, "integrations:vercel", "integrations:aws_private_link", "integrations:partners",
	"project_addons:dedicated_ipv4_address", "project_addons:show_compute_price",
	"project_creation:show_high_availability", "project_settings:custom_domains", "project_settings:log_drains",
	// Supabase Inc.'s own legal and compliance documents (DPA, SOC 2, ISO 27001, HIPAA), the request to
	// delete an account with Supabase's support desk, and the transfer of a project to another
	// organization (a project's branches, replicas, domains and roles would have to follow it).
	"organization:show_legal_documents", "profile:show_account_deletion", "projects:transfer",
	// Iceberg analytics buckets and vector buckets: Storage runs here without the catalog and vector
	// services behind them (icebergCatalog and vectorBuckets stay off, internal/fleet/storage.go), so
	// the pages would offer buckets that cannot work. Logs and Reports stay: they say what is missing.
	"storage:analytics", "storage:vectors",
	// Not listed, on purpose: organization:show_sso_settings. The organization's SSO page is shown. Its four
	// calls match sso_routes.go and the page's source at the pinned commit, but no browser run has loaded
	// it: Studio renders "unknown interface" for a page whose key is disabled, so only a run with the key
	// enabled can settle it. If the crawl finds a dead end, put the key back here and in the hide list of
	// unsupported_test.go.
}

// featureCustomDomains is the key that hides Settings > General > Custom domains, the page that claims,
// verifies and activates a project's or branch's own hostname. It stays in disabledFeatures only for a
// node that cannot serve one (customDomainsOffered).
const featureCustomDomains = "project_settings:custom_domains"

// customDomainsOffered reports whether the Custom domains page works on this node. A custom hostname
// needs a certificate ordered on demand for that name, and tls.mode "dns01" issues only the wildcard,
// so the domains service refuses a claim there (internal/domains README, "Certificates"). Every other
// mode serves the page, for projects and branches alike.
func (s *Server) customDomainsOffered() bool { return s.cfg.TLS.Mode != "dns01" }

// featureRestoreToNewProject is the key that hides Database > Backups > Restore to new project. It stays in
// disabledFeatures on a node whose backup service cannot restore as a new project (profileOf).
const featureRestoreToNewProject = "database:restore_to_new_project"

// featureReadReplicas is the key that hides Studio's read replica screens (the Infrastructure
// page, the database selector, the replica rows of the reports). It stays in disabledFeatures
// until a second server has joined (replicasOffered).
const featureReadReplicas = "infrastructure:read_replicas"

func (s *Server) routesProfile(add func(string, handlerFunc)) {
	add("GET /v1/profile", s.v1Profile)
	add("GET /platform/profile", s.platformProfile)
	add("POST /platform/profile", s.platformProfile)
	add("PATCH /platform/profile", s.platformUpdateProfile)
}

// currentUser returns the stored user behind the request's credentials.
func (s *Server) currentUser(r *http.Request) (*User, error) {
	p := principalFrom(r.Context())
	if p == nil {
		return nil, errUnauthorized
	}
	u, err := s.store.GetUser(r.Context(), p.UserID)
	if err == ErrNotFound {
		email := p.UserEmail()
		return s.store.UpsertUser(r.Context(), User{UserID: p.UserID, Email: email, Username: strings.SplitN(email, "@", 2)[0]})
	}
	return u, err
}

func (s *Server) v1Profile(w http.ResponseWriter, r *http.Request) error {
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &v1.V1ProfileResponseOutput{GotrueId: u.UserID, PrimaryEmail: u.Email, Username: u.Username})
	return nil
}

func (s *Server) platformProfile(w http.ResponseWriter, r *http.Request) error {
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, s.profileOf(r.Context(), u))
	return nil
}

func (s *Server) profileOf(ctx context.Context, u *User) *plat.ProfileResponseOutput {
	opt := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	disabled := append([]string(nil), disabledFeatures...)
	if s.replicasOffered(ctx) {
		disabled = slices.DeleteFunc(disabled, func(f string) bool { return f == featureReadReplicas })
	}
	if s.customDomainsOffered() {
		disabled = slices.DeleteFunc(disabled, func(f string) bool { return f == featureCustomDomains })
	}
	// Restore to a new project needs the backup service (clone.go); a node without one hides the page.
	if s.cloner() != nil {
		disabled = slices.DeleteFunc(disabled, func(f string) bool { return f == featureRestoreToNewProject })
	}
	if !s.githubOffered(ctx) {
		// No GitHub App on this server: the GitHub card, the Preferences row and "Sync with a GitHub
		// branch" would lead to nothing, or to the App of another party.
		disabled = append(disabled, featureGitHub)
	}
	return &plat.ProfileResponseOutput{
		Auth0Id: u.UserID, DisabledFeatures: &disabled, FirstName: opt(u.FirstName), LastName: opt(u.LastName),
		GotrueId: u.UserID, Id: float32(u.ID), PrimaryEmail: u.Email, Username: u.Username,
	}
}

func (s *Server) platformUpdateProfile(w http.ResponseWriter, r *http.Request) error {
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	var in struct {
		FirstName *string `json:"first_name"`
		LastName  *string `json:"last_name"`
		Username  *string `json:"username"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.FirstName != nil {
		u.FirstName = *in.FirstName
	}
	if in.LastName != nil {
		u.LastName = *in.LastName
	}
	if in.Username != nil && *in.Username != "" {
		u.Username = *in.Username
	}
	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		return mapErr(err)
	}
	writeJSON(w, http.StatusOK, s.profileOf(r.Context(), u))
	return nil
}

// allOrgs returns the organizations the caller belongs to. A node that has none yet gets the
// default organization, owned by the caller (the first dashboard user).
func (s *Server) allOrgs(r *http.Request) ([]registry.Organization, error) {
	existing, err := s.reg.ListOrganizations(r.Context())
	if err != nil {
		return nil, err
	}
	o, err := s.defaultOrg(r.Context())
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		if p := principalFrom(r.Context()); p != nil {
			if err := s.members.EnsureOwner(r.Context(), orgRef(o), p.UserID); err != nil {
				return nil, err
			}
		}
	}
	return s.memberOrgs(r)
}

func (s *Server) routesOrganizations(add func(string, handlerFunc)) {
	add("GET /v1/organizations", s.v1Orgs)
	add("GET /v1/organizations/{slug}", s.v1Org)
	add("GET /v1/organizations/{slug}/entitlements", s.entitlements("GET /v1/organizations/{slug}/entitlements"))

	add("GET /platform/organizations", s.platformOrgs)
	add("POST /platform/organizations", s.platformCreateOrg)
	add("GET /platform/organizations/{slug}", s.platformOrg)
	add("PATCH /platform/organizations/{slug}", s.platformUpdateOrg)
	add("DELETE /platform/organizations/{slug}", s.platformDeleteOrg)
	add("GET /platform/organizations/{slug}/entitlements", s.entitlements("GET /platform/organizations/{slug}/entitlements"))
	add("GET /platform/organizations/{slug}/billing/subscription", s.subscription)
	add("GET /platform/organizations/{slug}/usage", s.orgUsage)
	add("GET /platform/organizations/{slug}/usage/daily", s.orgUsageDaily)
}

func (s *Server) v1Orgs(w http.ResponseWriter, r *http.Request) error {
	orgs, err := s.allOrgs(r)
	if err != nil {
		return err
	}
	out := make([]v1.OrganizationResponseV1Output, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, v1.OrganizationResponseV1Output{Id: o.Slug, Name: o.Name, Slug: o.Slug})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) v1Org(w http.ResponseWriter, r *http.Request) error {
	o, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	resp := base("GET /v1/organizations/{slug}")
	setAll(resp, map[string]any{"id": o.Slug, "name": o.Name, "opt_in_tags": optInTags(o), "allowed_release_channels": []string{"ga"}})
	set(resp, "plan", "enterprise")
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// orgEntry is one organization of the platform list.
func (s *Server) orgEntry(o *registry.Organization, isOwner bool) map[string]any {
	row := elem("GET /platform/organizations", "")
	return setAll(row, map[string]any{
		"id": o.ID, "slug": o.Slug, "name": o.Name, "is_owner": isOwner, "opt_in_tags": optInTags(o),
		"plan": map[string]any{"id": "enterprise", "name": "Self-hosted"}, "usage_billing_enabled": false,
		"billing_email": nil, "billing_partner": nil, "integration_source": nil, "stripe_customer_id": nil,
		"subscription_id": nil, "restriction_data": nil, "restriction_status": nil,
		"organization_missing_address": false, "organization_missing_tax_id": false,
		"organization_requires_mfa": false, "requires_indirect_tax_declaration": false,
	})
}

func (s *Server) platformOrgs(w http.ResponseWriter, r *http.Request) error {
	orgs, err := s.allOrgs(r)
	if err != nil {
		return err
	}
	a, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(orgs))
	for i := range orgs {
		e := s.orgEntry(&orgs[i], a.OrgRole(orgs[i].ID) == members.RoleOwner)
		if on, err := s.members.MFAEnforced(r.Context(), orgs[i].ID); err == nil {
			e["organization_requires_mfa"] = on
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) platformOrg(w http.ResponseWriter, r *http.Request) error {
	o, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	resp := base("GET /platform/organizations/{slug}")
	setAll(resp, map[string]any{
		"id": o.ID, "slug": o.Slug, "name": o.Name, "created_at": ts(o.CreatedAt), "has_oriole_project": false,
		"opt_in_tags": optInTags(o), "plan": map[string]any{"id": "enterprise", "name": "Self-hosted"},
		"usage_billing_enabled": false, "billing_email": nil, "billing_partner": nil, "integration_source": nil,
		"restriction_data": nil, "restriction_status": nil,
	})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "org"
	}
	return s
}

func (s *Server) platformCreateOrg(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Name) == "" {
		return errf(http.StatusBadRequest, "name is required")
	}
	// Organizations are made by Owners (or by anyone on a node that has none yet), and the
	// creator owns the new one, as on hosted.
	a, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if existing, err := s.reg.ListOrganizations(r.Context()); err != nil {
		return err
	} else if len(existing) > 0 && !a.IsOwnerAnywhere() {
		return errf(http.StatusForbidden, "Only an Owner can create an organization")
	}
	o, err := s.reg.CreateOrganization(r.Context(), slugify(in.Name), strings.TrimSpace(in.Name))
	if err != nil {
		return mapErr(err)
	}
	if err := s.members.EnsureOwner(r.Context(), orgRef(o), principalFrom(r.Context()).UserID); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.orgEntry(o, true))
	return nil
}

// optInTags is the organization's AI opt-in tags as the responses list them: never null.
func optInTags(o *registry.Organization) []string {
	if len(o.OptInTags) == 0 {
		return []string{}
	}
	return slices.Clone(o.OptInTags)
}

// aiOptInTags are the opt-in tags Studio's AI settings use (the enum of the spec's
// UpdateOrganizationBody): schema metadata, logs and database data may go to the model provider.
var aiOptInTags = []plat.UpdateOrganizationBodyOptInTags{"AI_SQL_GENERATOR_OPT_IN", "AI_DATA_GENERATOR_OPT_IN", "AI_LOG_GENERATOR_OPT_IN"}

// parseOptInTags checks the tags of a PATCH against the spec's enum and drops repeats, keeping the
// order they came in.
func parseOptInTags(in []plat.UpdateOrganizationBodyOptInTags) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, t := range in {
		if !slices.Contains(aiOptInTags, t) {
			return nil, errf(http.StatusBadRequest, "opt_in_tags: %q is not one of %s", string(t), joinTags(aiOptInTags))
		}
		if !slices.Contains(out, string(t)) {
			out = append(out, string(t))
		}
	}
	return out, nil
}

func joinTags(tags []plat.UpdateOrganizationBodyOptInTags) string {
	s := make([]string, len(tags))
	for i, t := range tags {
		s[i] = string(t)
	}
	return strings.Join(s, ", ")
}

// platformUpdateOrg renames the organization and saves its AI opt-in tags. The route needs the
// organization update permission, which only an Owner has (authz.go), as on hosted; the tags decide
// what Studio may send to the node's model provider (docs/guide.md, "Use the AI Assistant").
func (s *Server) platformUpdateOrg(w http.ResponseWriter, r *http.Request) error {
	o, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in plat.UpdateOrganizationBody
	if err := decode(r, &in); err != nil {
		return err
	}
	// upd is what the registry gets. Its tags stay nil unless the body names them: the registry then
	// leaves the saved tags alone, so a rename cannot write back the tags this handler read before
	// another Owner changed them.
	upd := *o
	upd.OptInTags = nil
	changed := false
	if in.Name != nil {
		if n := strings.TrimSpace(*in.Name); n != "" {
			upd.Name, changed = n, true
		}
	}
	if in.OptInTags != nil {
		tags, err := parseOptInTags(*in.OptInTags)
		if err != nil {
			return err
		}
		upd.OptInTags, changed = tags, true
	}
	if changed {
		if err := s.reg.UpdateOrganization(r.Context(), &upd); err != nil {
			return mapErr(err)
		}
		o.Name = upd.Name
		if upd.OptInTags != nil {
			o.OptInTags = upd.OptInTags
		} else if fresh, err := s.reg.GetOrganizationByID(r.Context(), o.ID); err == nil {
			o.OptInTags = fresh.OptInTags // answer with what is saved, not with what was read first
		}
	}
	resp := base("PATCH /platform/organizations/{slug}")
	setAll(resp, map[string]any{"id": o.ID, "name": o.Name, "slug": o.Slug, "opt_in_tags": optInTags(o)})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// numericEntitlement and setEntitlement classify the entitlement keys of the spec
// enum. The spec lists keys and three kinds (boolean, numeric, set) but not which key
// has which kind, so the kind follows the key's name.
func entitlementKind(key string) string {
	switch {
	case strings.Contains(key, "available_"):
		return "set"
	case strings.Contains(key, "max_") && !strings.HasSuffix(key, ".configurable"),
		strings.HasSuffix(key, "_days"), strings.HasSuffix(key, "_limit"), strings.HasSuffix(key, "_mb"):
		return "numeric"
	}
	return "boolean"
}

// entitlements grants every feature of the spec's key enum: supavise has no plans.
func (s *Server) entitlements(key string) handlerFunc {
	op := operationByKey(key)
	keys := op.Response.Value.Properties["entitlements"].Value.Items.Value.Properties["feature"].Value.Properties["key"].Value.Enum
	list := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		name, _ := k.(string)
		kind := entitlementKind(name)
		e := map[string]any{"feature": map[string]any{"key": name, "type": kind}, "type": kind, "hasAccess": true}
		switch kind {
		case "numeric":
			e["config"] = map[string]any{"enabled": true, "value": 0, "unlimited": true, "unit": "count"}
		case "set":
			e["config"] = map[string]any{"enabled": true, "set": []string{}}
		default:
			e["config"] = map[string]any{"enabled": true}
		}
		list = append(list, e)
	}
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.orgBySlug(r.Context(), r.PathValue("slug")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"entitlements": list})
		return nil
	}
}

func (s *Server) subscription(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.orgBySlug(r.Context(), r.PathValue("slug")); err != nil {
		return err
	}
	resp := base("GET /platform/organizations/{slug}/billing/subscription")
	set(resp, "plan", map[string]any{"id": "enterprise", "name": "Self-hosted"})
	// The Usage page prints this period and takes it as its default range; the stub's zero printed
	// "01 Jan 1970 - 01 Jan 1970". A node has no billing cycle, so it is the calendar month (UTC).
	start, end := billingPeriod(s.now())
	set(resp, "current_period_start", start.Unix())
	set(resp, "current_period_end", end.Unix())
	writeJSON(w, http.StatusOK, resp)
	return nil
}

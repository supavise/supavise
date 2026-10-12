package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	v2 "github.com/supavise/supavise/internal/api/gen/v2"
	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/github"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
)

// The GitHub integration of the dashboard: Settings > Integrations > GitHub, the "Sync with a
// GitHub branch" block of the branch dialogs and Account > Preferences > Connections. The work is
// internal/github (docs/github-integration.md); this file is the Management API in front of it.
//
// Without a GitHub App on the server (Deps.GitHub nil, or the service holds none) the answers are
// truthful: the user has no authorization (404), the connection list is empty, nothing can be
// connected, and the profile hides the dashboard's GitHub surfaces (integrations:github in
// disabled_features). No route sends anyone to another party's GitHub App.

// resGitHubConnections is the resource Studio checks for a project's GitHub connection
// (integrations.github_connections): Owners and Administrators create, change and delete it,
// every role that sees the project reads it.
const resGitHubConnections = "integrations.github_connections"

// featureGitHub is the key that hides the dashboard's GitHub surfaces (Studio patch 0007).
const featureGitHub = "integrations:github"

// The ids GitHub issues (repositories, installations) do not fit the float32 the generated types
// give every number, so these bodies use int64. TestGitHubBodiesKeepTheSpecsFields pins their
// fields to the generated types.

type ghAuthorizationOut struct {
	ID       int64 `json:"id"`
	SenderID int64 `json:"sender_id"`
	UserID   int64 `json:"user_id"`
}

type ghConnectionBody struct {
	ProjectRef          string   `json:"project_ref"`
	InstallationID      int64    `json:"installation_id"`
	RepositoryID        int64    `json:"repository_id"`
	Workdir             *string  `json:"workdir"`
	SupabaseChangesOnly *bool    `json:"supabase_changes_only"`
	BranchLimit         *float64 `json:"branch_limit"`
	NewBranchPerPR      *bool    `json:"new_branch_per_pr"`
}

type ghConnectionCreated struct {
	ID                  int64   `json:"id"`
	InstallationID      int64   `json:"installation_id"`
	Workdir             string  `json:"workdir"`
	SupabaseChangesOnly bool    `json:"supabase_changes_only"`
	BranchLimit         float64 `json:"branch_limit"`
	NewBranchPerPR      bool    `json:"new_branch_per_pr"`
	InsertedAt          string  `json:"inserted_at"`
	UpdatedAt           string  `json:"updated_at"`
}

type ghConnectionOut struct {
	ghConnectionCreated
	Project struct {
		ID   int64  `json:"id"`
		Ref  string `json:"ref"`
		Name string `json:"name"`
	} `json:"project"`
	Repository struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"repository"`
	User *ghUserOut `json:"user"`
}

type ghUserOut struct {
	ID           int64   `json:"id"`
	Username     string  `json:"username"`
	PrimaryEmail *string `json:"primary_email"`
}

type ghRepositoryOut struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	InstallationID int64  `json:"installation_id"`
	DefaultBranch  string `json:"default_branch"`
}

func (s *Server) routesGitHub(add func(string, handlerFunc)) {
	add("GET /platform/integrations/github/authorization", s.ghGetAuthorization)
	add("POST /platform/integrations/github/authorization", s.ghCreateAuthorization)
	add("DELETE /platform/integrations/github/authorization", s.ghDeleteAuthorization)
	add("GET /platform/integrations/github/connections", s.ghListConnections)
	add("POST /platform/integrations/github/connections", s.ghCreateConnection)
	add("PATCH /platform/integrations/github/connections/{connection_id}", s.ghUpdateConnection)
	add("DELETE /platform/integrations/github/connections/{connection_id}", s.ghDeleteConnection)
	add("GET /platform/integrations/github/connections/{connection_id}/config", s.ghConnectionConfig)
	add("GET /platform/integrations/github/repositories", s.ghListRepositories)
	add("GET /platform/integrations/github/repositories/{repository_id}/branches", s.ghListRepoBranches)
	add("GET /platform/integrations/github/repositories/{repository_id}/branches/{branch_name}", s.ghCheckBranch)
	add("GET /v2/organizations/{slug}/integrations/github/connections", s.ghListConnectionsV2)

	// The operator's side: the server's own GitHub App. Only reading it is an HTTP route: the App is
	// one object for the whole node, so creating, replacing and removing it (`supavise github app
	// create|remove`) stay with the operator at the node and are not open to an organization's Owner.
	add("GET /supavise/v1/github/app", s.ghAppStatus)
	// These carry no credentials: the manifest page and GitHub's redirect are visited by a browser
	// that holds only the one-time state, and GitHub posts the webhooks.
	add("GET "+github.ManifestPath, s.ghManifestPage)
	add("GET "+github.ManifestCallbackPath, s.ghManifestCallback)
	add("POST /hooks/github", s.ghWebhook)
}

// githubPublicRoutes are the routes of routesGitHub that need no credentials.
var githubPublicRoutes = []string{"GET " + github.ManifestPath, "GET " + github.ManifestCallbackPath, "POST /hooks/github"}

// githubOffered reports whether the dashboard shows its GitHub surfaces: the server has a GitHub App.
func (s *Server) githubOffered(ctx context.Context) bool {
	return s.gh != nil && s.gh.Configured(ctx)
}

// ghService returns the integration, or the answer of a server that has none.
func (s *Server) ghService(r *http.Request) (*github.Service, error) {
	if s.gh == nil || !s.gh.Configured(r.Context()) {
		return nil, s.ghNotConfigured(r)
	}
	return s.gh, nil
}

const ghNotConfiguredMsg = "The GitHub integration is not set up on this Supavise server. An operator creates the GitHub App with `supavise github app create` (docs/github-integration.md)."

// ghNotConfigured is the refusal of a server without a GitHub App: 404 for what reads or removes
// something that cannot exist, 400 for what asks to make something.
func (s *Server) ghNotConfigured(r *http.Request) error {
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		return errf(http.StatusBadRequest, "%s", ghNotConfiguredMsg)
	}
	return errf(http.StatusNotFound, "%s", ghNotConfiguredMsg)
}

// ghErr turns the integration's errors into the API's.
func (s *Server) ghErr(r *http.Request, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, github.ErrNotConfigured):
		return s.ghNotConfigured(r)
	case errors.Is(err, github.ErrNotAuthorized):
		return errf(http.StatusForbidden, "Authorize the GitHub App first: Account > Preferences > Connections, or Project Settings > Integrations > GitHub.")
	case errors.Is(err, github.ErrNotFound):
		return errf(http.StatusNotFound, "%s", trimGH(err))
	case errors.Is(err, github.ErrInvalid):
		return errf(http.StatusBadRequest, "%s", trimGH(err))
	case errors.Is(err, github.ErrConflict):
		return errf(http.StatusConflict, "%s", trimGH(err))
	}
	var ae *github.APIError
	if errors.As(err, &ae) {
		// GitHub's own refusal, with the status it gave; the message never carries a token.
		switch ae.Status {
		case http.StatusForbidden:
			if ae.SSO {
				return errf(http.StatusForbidden, "GitHub requires you to authorize the App for your organization's SAML single sign-on first.")
			}
			return errf(http.StatusForbidden, "GitHub refused the request: %s", ae.Message)
		case http.StatusNotFound:
			return errf(http.StatusNotFound, "GitHub could not find it: %s", ae.Message)
		}
		s.log.Warn("GitHub refused a request", "status", ae.Status, "path", ae.Path, "message", ae.Message)
		return errf(http.StatusBadGateway, "GitHub answered %d: %s", ae.Status, ae.Message)
	}
	return mapErr(err)
}

func trimGH(err error) string {
	msg := err.Error()
	for _, e := range []error{github.ErrNotFound, github.ErrInvalid, github.ErrConflict} {
		if p := e.Error() + ": "; strings.HasPrefix(msg, p) {
			return msg[len(p):]
		}
	}
	return msg
}

// ---- the user's authorization -----------------------------------------------------------------

func (s *Server) ghGetAuthorization(w http.ResponseWriter, r *http.Request) error {
	gh, err := s.ghService(r)
	if err != nil {
		return err
	}
	p := principalFrom(r.Context())
	a, err := gh.Authorization(r.Context(), p.UserID)
	if errors.Is(err, github.ErrNotAuthorized) {
		return errf(http.StatusNotFound, "You have not authorized the GitHub App.")
	}
	if err != nil {
		return s.ghErr(r, err)
	}
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ghAuthorizationOut{ID: a.ID, SenderID: a.GitHubUserID, UserID: u.ID})
	return nil
}

func (s *Server) ghCreateAuthorization(w http.ResponseWriter, r *http.Request) error {
	gh, err := s.ghService(r)
	if err != nil {
		return err
	}
	// Studio's authorize page compares the state with the one it kept in the browser before it posts;
	// the body that arrives here is only the code.
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if _, err := gh.Authorize(r.Context(), principalFrom(r.Context()).UserID, in.Code); err != nil {
		return s.ghErr(r, err)
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (s *Server) ghDeleteAuthorization(w http.ResponseWriter, r *http.Request) error {
	gh, err := s.ghService(r)
	if err != nil {
		return err
	}
	had, err := gh.Deauthorize(r.Context(), principalFrom(r.Context()).UserID)
	if err != nil {
		return s.ghErr(r, err)
	}
	if !had {
		return errf(http.StatusNotFound, "You have not authorized the GitHub App.")
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// ---- connections ----------------------------------------------------------------------------------

// connectionJSON describes a connection with its project and the user who made it.
func (s *Server) connectionJSON(ctx context.Context, c *registry.GitHubConnection) *ghConnectionOut {
	out := &ghConnectionOut{ghConnectionCreated: ghConnectionOf(c)}
	out.Repository.ID, out.Repository.Name = c.RepositoryID, c.RepositoryName
	out.Project.Ref = c.ProjectRef
	if p, err := s.reg.GetProject(ctx, c.ProjectRef); err == nil {
		out.Project.ID, out.Project.Name = int64(p.Seq), p.Name
	}
	if c.CreatedBy != "" {
		if u, err := s.store.GetUser(ctx, c.CreatedBy); err == nil {
			out.User = &ghUserOut{ID: u.ID, Username: u.Username}
			if u.Email != "" {
				e := u.Email
				out.User.PrimaryEmail = &e
			}
		}
	}
	return out
}

func ghConnectionOf(c *registry.GitHubConnection) ghConnectionCreated {
	return ghConnectionCreated{ID: c.ID, InstallationID: c.InstallationID, Workdir: c.Workdir, SupabaseChangesOnly: c.SupabaseChangesOnly,
		BranchLimit: float64(c.BranchLimit), NewBranchPerPR: c.NewBranchPerPR,
		InsertedAt: c.InsertedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

// ghOrgConnections lists the connections of an organization the caller may read.
func (s *Server) ghOrgConnections(r *http.Request, org *registry.Organization) ([]*ghConnectionOut, error) {
	access, err := s.callerAccess(r)
	if err != nil {
		return nil, err
	}
	ref := members.OrgRef{ID: org.ID, Slug: org.Slug}
	if !access.IsMember(org.ID) {
		return nil, forbidden(members.ActRead, resGitHubConnections)
	}
	out := []*ghConnectionOut{}
	if s.gh == nil || !s.gh.Configured(r.Context()) {
		return out, nil
	}
	conns, err := s.gh.Connections(r.Context(), org.ID)
	if err != nil {
		return nil, s.ghErr(r, err)
	}
	for i := range conns {
		// A role scoped to some projects sees the connections of those.
		if !access.Can(ref, conns[i].ProjectRef, members.ActRead, resGitHubConnections, nil) {
			continue
		}
		out = append(out, s.connectionJSON(r.Context(), &conns[i]))
	}
	return out, nil
}

func (s *Server) ghListConnections(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.URL.Query().Get("organization_id"), 10, 64)
	if err != nil || id <= 0 {
		return errf(http.StatusBadRequest, "organization_id is required")
	}
	org, err := s.reg.GetOrganizationByID(r.Context(), id)
	if errors.Is(err, registry.ErrNotFound) {
		return errf(http.StatusNotFound, "Organization not found")
	}
	if err != nil {
		return err
	}
	list, err := s.ghOrgConnections(r, org)
	if err != nil {
		return err
	}
	if ref := r.URL.Query().Get("project_ref"); ref != "" {
		kept := list[:0]
		for _, c := range list {
			if c.Project.Ref == ref {
				kept = append(kept, c)
			}
		}
		list = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": list})
	return nil
}

func (s *Server) ghListConnectionsV2(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	list, err := s.ghOrgConnections(r, org)
	if err != nil {
		return err
	}
	// The JSON:API shape of the v2 spec; the ids of GitHub fit float32 only by luck, so the rows are
	// written as maps of the spec's attributes.
	data := make([]map[string]any, 0, len(list))
	for _, c := range list {
		data = append(data, map[string]any{"id": strconv.FormatInt(c.ID, 10), "type": string(v2.GithubConnection), "attributes": map[string]any{
			"branch_limit": c.BranchLimit, "inserted_at": c.InsertedAt, "installation_id": c.InstallationID, "new_branch_per_pr": c.NewBranchPerPR,
			"project": c.Project, "repository": c.Repository, "supabase_changes_only": c.SupabaseChangesOnly, "updated_at": c.UpdatedAt,
			"user": c.User, "workdir": c.Workdir}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "links": map[string]any{"next": nil, "prev": nil}})
	return nil
}

// wholeNumber reads a JSON number that must be a whole number in [1, max].
func wholeNumber(f float64, name string, max int) (int, error) {
	n := int(f)
	if float64(n) != f || n < 1 || n > max {
		return 0, errf(http.StatusBadRequest, "%s must be a whole number between 1 and %d", name, max)
	}
	return n, nil
}

func (s *Server) ghCreateConnection(w http.ResponseWriter, r *http.Request) error {
	var in ghConnectionBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.ProjectRef == "" || in.InstallationID <= 0 || in.RepositoryID <= 0 {
		return errf(http.StatusBadRequest, "project_ref, installation_id and repository_id are required")
	}
	p, err := s.loadProject(r.Context(), in.ProjectRef)
	if err != nil {
		return err
	}
	if p.Branch != nil {
		return errf(http.StatusBadRequest, "Connect the project itself, not one of its branches (%s is a branch of %s).", p.Ref, p.Branch.ParentRef)
	}
	org, err := s.orgOf(r.Context(), p)
	if err != nil {
		return err
	}
	if err := s.require(r, org, scopeRef(p), members.ActCreate, resGitHubConnections); err != nil {
		return err
	}
	gh, err := s.ghService(r)
	if err != nil {
		return err
	}
	ci := github.ConnectInput{UserID: principalFrom(r.Context()).UserID, OrgID: org.ID, ProjectRef: p.Ref, InstallationID: in.InstallationID,
		RepositoryID: in.RepositoryID, SupabaseChangesOnly: in.SupabaseChangesOnly, NewBranchPerPR: in.NewBranchPerPR}
	if in.Workdir != nil {
		ci.Workdir = *in.Workdir
	}
	if in.BranchLimit != nil {
		n, err := wholeNumber(*in.BranchLimit, "branch_limit", github.MaxBranchLimit)
		if err != nil {
			return err
		}
		ci.BranchLimit = &n
	}
	c, err := gh.Connect(r.Context(), ci)
	if err != nil {
		return s.ghErr(r, err)
	}
	writeJSON(w, http.StatusCreated, ghConnectionOf(c))
	return nil
}

// ghConnectionFor loads the connection of the path and checks the caller's right on its project.
func (s *Server) ghConnectionFor(r *http.Request, action string) (*github.Service, *registry.GitHubConnection, error) {
	id, err := strconv.ParseInt(r.PathValue("connection_id"), 10, 64)
	if err != nil {
		return nil, nil, errf(http.StatusNotFound, "Connection not found")
	}
	gh, err := s.ghService(r)
	if err != nil {
		return nil, nil, err
	}
	c, err := gh.Connection(r.Context(), id)
	if err != nil {
		return nil, nil, s.ghErr(r, err)
	}
	p, err := s.reg.GetProject(r.Context(), c.ProjectRef)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, nil, errf(http.StatusNotFound, "Connection not found")
	}
	if err != nil {
		return nil, nil, err
	}
	org, err := s.orgOf(r.Context(), p)
	if err != nil {
		return nil, nil, err
	}
	// A caller who may not see the organization learns nothing of the connection.
	if ok, aerr := s.can(r, org, scopeRef(p), members.ActRead, resGitHubConnections); aerr != nil {
		return nil, nil, aerr
	} else if !ok {
		return nil, nil, errf(http.StatusNotFound, "Connection not found")
	}
	if action != members.ActRead {
		if err := s.require(r, org, scopeRef(p), action, resGitHubConnections); err != nil {
			return nil, nil, err
		}
	}
	return gh, c, nil
}

func (s *Server) ghUpdateConnection(w http.ResponseWriter, r *http.Request) error {
	gh, c, err := s.ghConnectionFor(r, members.ActUpdate)
	if err != nil {
		return err
	}
	var in struct {
		Workdir             *string  `json:"workdir"`
		SupabaseChangesOnly *bool    `json:"supabase_changes_only"`
		BranchLimit         *float64 `json:"branch_limit"`
		NewBranchPerPR      *bool    `json:"new_branch_per_pr"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	ui := github.UpdateInput{Workdir: in.Workdir, SupabaseChangesOnly: in.SupabaseChangesOnly, NewBranchPerPR: in.NewBranchPerPR}
	if in.BranchLimit != nil {
		n, err := wholeNumber(*in.BranchLimit, "branch_limit", github.MaxBranchLimit)
		if err != nil {
			return err
		}
		ui.BranchLimit = &n
	}
	if _, err := gh.Update(r.Context(), c.ID, ui); err != nil {
		return s.ghErr(r, err)
	}
	writeNoContent(w)
	return nil
}

func (s *Server) ghDeleteConnection(w http.ResponseWriter, r *http.Request) error {
	gh, c, err := s.ghConnectionFor(r, members.ActDelete)
	if err != nil {
		return err
	}
	if err := gh.Disconnect(r.Context(), c.ID); err != nil {
		return s.ghErr(r, err)
	}
	writeNoContent(w)
	return nil
}

func (s *Server) ghConnectionConfig(w http.ResponseWriter, r *http.Request) error {
	gh, c, err := s.ghConnectionFor(r, members.ActRead)
	if err != nil {
		return err
	}
	doc, err := gh.ConnectionConfig(r.Context(), c, r.URL.Query().Get("ref"))
	if err != nil {
		if errors.Is(err, github.ErrInvalid) {
			return errf(http.StatusUnprocessableEntity, "%s", trimGH(err))
		}
		return s.ghErr(r, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": doc.Path, "ref": doc.Ref, "sha": doc.SHA, "config": doc.Config})
	return nil
}

// ---- repositories ---------------------------------------------------------------------------------

func (s *Server) ghListRepositories(w http.ResponseWriter, r *http.Request) error {
	gh, err := s.ghService(r)
	if err != nil {
		return err
	}
	repos, partial, err := gh.Repositories(r.Context(), principalFrom(r.Context()).UserID)
	if err != nil {
		return s.ghErr(r, err)
	}
	out := make([]ghRepositoryOut, 0, len(repos))
	for _, rp := range repos {
		out = append(out, ghRepositoryOut{ID: rp.ID, Name: rp.Name, InstallationID: rp.InstallationID, DefaultBranch: rp.DefaultBranch})
	}
	writeJSON(w, http.StatusOK, map[string]any{"partial_response_due_to_sso": partial, "repositories": out})
	return nil
}

func ghRepoID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("repository_id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errf(http.StatusBadRequest, "repository_id must be a number")
	}
	return id, nil
}

func (s *Server) ghListRepoBranches(w http.ResponseWriter, r *http.Request) error {
	gh, err := s.ghService(r)
	if err != nil {
		return err
	}
	id, err := ghRepoID(r)
	if err != nil {
		return err
	}
	names, err := gh.Branches(r.Context(), principalFrom(r.Context()).UserID, id)
	if err != nil {
		return s.ghErr(r, err)
	}
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": out})
	return nil
}

func (s *Server) ghCheckBranch(w http.ResponseWriter, r *http.Request) error {
	gh, err := s.ghService(r)
	if err != nil {
		return err
	}
	id, err := ghRepoID(r)
	if err != nil {
		return err
	}
	name, err := gh.Branch(r.Context(), principalFrom(r.Context()).UserID, id, r.PathValue("branch_name"))
	if err != nil {
		return s.ghErr(r, err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
	return nil
}

// ---- branches ------------------------------------------------------------------------------------

// annotatePRs fills pr_number of the branches that follow a pull request.
func (s *Server) annotatePRs(ctx context.Context, outs ...*branchOut) {
	if s.gh == nil || len(outs) == 0 {
		return
	}
	refs := make([]string, 0, len(outs))
	for _, o := range outs {
		refs = append(refs, o.ProjectRef)
	}
	nums, err := s.gh.PRNumbers(ctx, refs)
	if err != nil {
		s.log.Warn("could not read the pull requests of branches", "err", err)
		return
	}
	for _, o := range outs {
		if n, ok := nums[o.ProjectRef]; ok {
			v := int32(n)
			o.PrNumber = &v
		}
	}
}

// writeBranchPR is writeBranch with the number of the pull request the branch follows. The routes
// that answer one branch use it; writeBranch itself keeps its signature.
func (s *Server) writeBranchPR(w http.ResponseWriter, r *http.Request, status int, b *branching.Branch) error {
	out, err := branchJSON(b)
	if err != nil {
		return err
	}
	s.annotatePRs(r.Context(), out)
	writeJSON(w, status, out)
	return nil
}

// pushBranch is the push of POST /v1/branches/{id}/push. On the production row of a project with
// "Deploy to production" on, it deploys the production branch of the repository again (Resync);
// elsewhere it is the service's push.
func (s *Server) pushBranch(ctx context.Context, id string, ai branching.ActionInput) (string, error) {
	if s.gh != nil && s.gh.Configured(ctx) {
		if b, err := s.branches.Resolve(ctx, id); err == nil && b.IsDefault && s.gh.ProductionBranch(ctx, b.Ref) != "" {
			run, derr := s.gh.DeployProduction(ctx, b.Ref)
			if derr != nil {
				return "", s.ghErr(&http.Request{Method: http.MethodPost}, derr)
			}
			return run, nil
		}
	}
	return s.branches.Push(ctx, id, ai)
}

package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/registry"
)

// Repository is a repository a user may connect, as Studio's picker lists it.
type Repository struct {
	ID             int64
	Name           string // owner/name
	InstallationID int64
	DefaultBranch  string
}

type apiRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
}

const (
	maxInstallations = 50
	maxPages         = 20
)

// installations lists the installations of this server's App that the user can see.
func (s *Service) installations(ctx context.Context, app *App, token string) ([]int64, error) {
	var ids []int64
	err := s.cl.getAll(ctx, token, "/user/installations?per_page=100", "installations", maxPages, func(raw json.RawMessage) error {
		var page []struct {
			ID    int64 `json:"id"`
			AppID int64 `json:"app_id"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, in := range page {
			// The user's token sees only the installations of its own App; the check is for the day it does not.
			if in.AppID == 0 || in.AppID == app.ID {
				ids = append(ids, in.ID)
			}
		}
		return nil
	})
	if len(ids) > maxInstallations {
		ids = ids[:maxInstallations]
	}
	return ids, err
}

// installationRepos lists the repositories of an installation that the user can see.
func (s *Service) installationRepos(ctx context.Context, token string, installationID int64, each func(apiRepo) (stop bool)) error {
	stop := errors.New("stop")
	err := s.cl.getAll(ctx, token, fmt.Sprintf("/user/installations/%d/repositories?per_page=100", installationID), "repositories", maxPages, func(raw json.RawMessage) error {
		var page []apiRepo
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, r := range page {
			if each(r) {
				return stop
			}
		}
		return nil
	})
	if errors.Is(err, stop) {
		return nil
	}
	return err
}

// Repositories lists the repositories the user may connect: those of the App's installations that
// the user's own GitHub token can see. partialSSO is true when an organization that requires SAML
// SSO hid its repositories until the user authorizes the token for it.
func (s *Service) Repositories(ctx context.Context, userID string) (repos []Repository, partialSSO bool, err error) {
	app, err := s.App(ctx)
	if err != nil {
		return nil, false, err
	}
	err = s.asUser(ctx, userID, func(token string) error {
		ids, err := s.installations(ctx, app, token)
		if err != nil {
			return err
		}
		for _, id := range ids {
			err := s.installationRepos(ctx, token, id, func(r apiRepo) bool {
				if !r.Archived {
					repos = append(repos, Repository{ID: r.ID, Name: r.FullName, InstallationID: id, DefaultBranch: r.DefaultBranch})
				}
				return false
			})
			var ae *APIError
			if errors.As(err, &ae) && ae.SSO {
				partialSSO = true
				continue
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return repos, partialSSO, err
}

// repoFor finds a repository by id among the ones the user may connect.
func (s *Service) repoFor(ctx context.Context, userID string, repoID int64) (*Repository, error) {
	repos, _, err := s.Repositories(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range repos {
		if repos[i].ID == repoID {
			return &repos[i], nil
		}
	}
	return nil, fmt.Errorf("%w: repository %d is not one the GitHub App can reach for you", ErrNotFound, repoID)
}

// Branches lists the branch names of a repository the user may connect.
func (s *Service) Branches(ctx context.Context, userID string, repoID int64) ([]string, error) {
	r, err := s.repoFor(ctx, userID, repoID)
	if err != nil {
		return nil, err
	}
	tok, err := s.installationToken(ctx, r.InstallationID)
	if err != nil {
		return nil, err
	}
	var names []string
	err = s.cl.getAll(ctx, tok, "/repos/"+escapeSegments(r.Name)+"/branches?per_page=100", "", maxPages, func(raw json.RawMessage) error {
		var page []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, b := range page {
			names = append(names, b.Name)
		}
		return nil
	})
	return names, err
}

// Branch reports whether a repository has the branch, for the production-branch field and the
// branch pickers. ErrNotFound when it has not.
func (s *Service) Branch(ctx context.Context, userID string, repoID int64, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", invalid("the branch name is required")
	}
	r, err := s.repoFor(ctx, userID, repoID)
	if err != nil {
		return "", err
	}
	tok, err := s.installationToken(ctx, r.InstallationID)
	if err != nil {
		return "", err
	}
	var b struct {
		Name string `json:"name"`
	}
	if err := s.cl.do(ctx, tok, http.MethodGet, "/repos/"+escapeSegments(r.Name)+"/branches/"+escapeSegments(name), nil, &b); err != nil {
		if IsStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("%w: branch %q does not exist in %s", ErrNotFound, name, r.Name)
		}
		return "", err
	}
	return b.Name, nil
}

// ---- connections ---------------------------------------------------------------------------

// ConnectInput is the body of a new connection plus who sends it.
type ConnectInput struct {
	UserID         string
	OrgID          int64
	ProjectRef     string
	InstallationID int64
	RepositoryID   int64
	Workdir        string
	// The options are nil when the body leaves them out.
	SupabaseChangesOnly *bool
	BranchLimit         *int
	NewBranchPerPR      *bool
}

// Defaults of a connection whose body leaves an option out.
const (
	DefaultBranchLimit = 50
	MaxBranchLimit     = 500
)

// CleanWorkdir normalizes the folder that holds supabase/: relative to the repository root, no
// "..", no leading slash. "" is the root.
func CleanWorkdir(w string) (string, error) {
	w = strings.TrimSpace(strings.ReplaceAll(w, "\\", "/"))
	if w == "" || w == "." || w == "./" || w == "/" {
		return "", nil
	}
	for _, seg := range strings.Split(w, "/") {
		if seg == ".." {
			return "", invalid("the working directory %q leaves the repository", w)
		}
	}
	c := path.Clean(strings.TrimPrefix(w, "/"))
	if c == "." {
		return "", nil
	}
	if strings.ContainsAny(c, "\x00\r\n") {
		return "", invalid("the working directory has characters a path cannot")
	}
	return c, nil
}

func checkLimit(n int) error {
	if n < 1 || n > MaxBranchLimit {
		return invalid("branch_limit must be between 1 and %d", MaxBranchLimit)
	}
	return nil
}

// Connect connects a project to a repository the user can reach through the App.
func (s *Service) Connect(ctx context.Context, in ConnectInput) (*registry.GitHubConnection, error) {
	if _, err := s.App(ctx); err != nil {
		return nil, err
	}
	workdir, err := CleanWorkdir(in.Workdir)
	if err != nil {
		return nil, err
	}
	c := &registry.GitHubConnection{OrgID: in.OrgID, ProjectRef: in.ProjectRef, InstallationID: in.InstallationID, RepositoryID: in.RepositoryID,
		Workdir: workdir, BranchLimit: DefaultBranchLimit, CreatedBy: in.UserID}
	if in.SupabaseChangesOnly != nil {
		c.SupabaseChangesOnly = *in.SupabaseChangesOnly
	}
	if in.NewBranchPerPR != nil {
		c.NewBranchPerPR = *in.NewBranchPerPR
	}
	if in.BranchLimit != nil {
		if err := checkLimit(*in.BranchLimit); err != nil {
			return nil, err
		}
		c.BranchLimit = *in.BranchLimit
	}
	repo, err := s.repoFor(ctx, in.UserID, in.RepositoryID)
	if err != nil {
		return nil, err
	}
	if repo.InstallationID != in.InstallationID {
		return nil, invalid("repository %d is not part of installation %d", in.RepositoryID, in.InstallationID)
	}
	c.RepositoryName = repo.Name
	if err := s.st.CreateGitHubConnection(ctx, c); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return nil, fmt.Errorf("%w: this project is already connected to a repository", ErrConflict)
		}
		return nil, err
	}
	s.auditEvent(ctx, "github.connected", map[string]any{"project": in.ProjectRef, "repository": repo.Name, "installation_id": in.InstallationID})
	return c, nil
}

// UpdateInput changes the options of a connection; nil leaves an option alone.
type UpdateInput struct {
	Workdir             *string
	SupabaseChangesOnly *bool
	BranchLimit         *int
	NewBranchPerPR      *bool
}

// Update changes a connection's options.
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput) (*registry.GitHubConnection, error) {
	c, err := s.st.GetGitHubConnection(ctx, id)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, fmt.Errorf("%w: connection %d", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if in.Workdir != nil {
		if c.Workdir, err = CleanWorkdir(*in.Workdir); err != nil {
			return nil, err
		}
	}
	if in.SupabaseChangesOnly != nil {
		c.SupabaseChangesOnly = *in.SupabaseChangesOnly
	}
	if in.NewBranchPerPR != nil {
		c.NewBranchPerPR = *in.NewBranchPerPR
	}
	if in.BranchLimit != nil {
		if err := checkLimit(*in.BranchLimit); err != nil {
			return nil, err
		}
		c.BranchLimit = *in.BranchLimit
	}
	if err := s.st.UpdateGitHubConnection(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Disconnect removes a connection. Its preview branches stay: they are the user's to delete.
func (s *Service) Disconnect(ctx context.Context, id int64) error {
	c, err := s.st.GetGitHubConnection(ctx, id)
	if errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("%w: connection %d", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	links, _ := s.st.ListGitHubBranchLinks(ctx, c.ProjectRef)
	if err := s.st.DeleteGitHubConnection(ctx, id); err != nil {
		return err
	}
	for _, l := range links {
		if l.RepositoryID == c.RepositoryID {
			_ = s.st.DeleteGitHubBranchLink(ctx, l.BranchRef)
		}
	}
	s.auditEvent(ctx, "github.disconnected", map[string]any{"project": c.ProjectRef, "repository": c.RepositoryName})
	return nil
}

// ConfigDoc is a repository's supabase/config.toml as the dashboard's drift check reads it.
type ConfigDoc struct {
	Path   string
	Ref    *string
	SHA    string
	Config map[string]any
}

// maxConfigSize bounds a config.toml: a real one is a few kilobytes.
const maxConfigSize = 2 << 20

// ConnectionConfig reads and parses supabase/config.toml of the connection's repository at ref (a
// branch name or commit; empty is the default branch). ErrNotFound when the file is not there and
// ErrInvalid when it is not valid TOML.
func (s *Service) ConnectionConfig(ctx context.Context, c *registry.GitHubConnection, ref string) (*ConfigDoc, error) {
	tok, err := s.installationToken(ctx, c.InstallationID)
	if err != nil {
		return nil, err
	}
	p := path.Join(c.Workdir, "supabase", "config.toml")
	q := "/repos/" + escapeSegments(c.RepositoryName) + "/contents/" + escapeSegments(p)
	if ref != "" {
		q += "?ref=" + url.QueryEscape(ref)
	}
	var f struct {
		SHA      string `json:"sha"`
		Size     int64  `json:"size"`
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := s.cl.do(ctx, tok, http.MethodGet, q, nil, &f); err != nil {
		if IsStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("%w: %s has no %s%s", ErrNotFound, c.RepositoryName, p, onRef(ref))
		}
		return nil, err
	}
	if f.Type != "" && f.Type != "file" {
		return nil, fmt.Errorf("%w: %s is not a file", ErrNotFound, p)
	}
	if f.Size > maxConfigSize {
		return nil, invalid("%s is larger than %d bytes", p, maxConfigSize)
	}
	var body []byte
	if f.Encoding == "base64" {
		if body, err = base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", "")); err != nil {
			return nil, fmt.Errorf("github: decode %s: %w", p, err)
		}
	} else if body, err = s.cl.raw(ctx, tok, "/repos/"+escapeSegments(c.RepositoryName)+"/git/blobs/"+url.PathEscape(f.SHA), maxConfigSize); err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(body, &doc); err != nil {
		return nil, invalid("%s is not valid TOML: %v", p, err)
	}
	out := &ConfigDoc{Path: p, SHA: f.SHA, Config: jsonable(doc).(map[string]any)}
	if ref != "" {
		out.Ref = &ref
	}
	return out, nil
}

func onRef(ref string) string {
	if ref == "" {
		return ""
	}
	return " at " + ref
}

// jsonable turns what the TOML decoder returns into values that JSON can carry: dates become
// strings, and every table a map[string]any.
func jsonable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = jsonable(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonable(e)
		}
		return out
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case toml.LocalDate:
		return x.String()
	case toml.LocalTime:
		return x.String()
	case toml.LocalDateTime:
		return x.String()
	}
	return v
}

// Connections lists an organization's connections, oldest first.
func (s *Service) Connections(ctx context.Context, orgID int64) ([]registry.GitHubConnection, error) {
	return s.st.ListGitHubConnections(ctx, orgID)
}

// Connection returns a connection by id, ErrNotFound when there is none.
func (s *Service) Connection(ctx context.Context, id int64) (*registry.GitHubConnection, error) {
	c, err := s.st.GetGitHubConnection(ctx, id)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, fmt.Errorf("%w: connection %d", ErrNotFound, id)
	}
	return c, err
}

// ConnectionOfProject returns the project's connection, nil when it has none.
func (s *Service) ConnectionOfProject(ctx context.Context, ref string) (*registry.GitHubConnection, error) {
	c, err := s.st.GetGitHubConnectionByProject(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, nil
	}
	return c, err
}

// PRNumbers returns the pull request number of each branch that follows one.
func (s *Service) PRNumbers(ctx context.Context, branchRefs []string) (map[string]int, error) {
	return s.st.GitHubPRNumbers(ctx, branchRefs)
}

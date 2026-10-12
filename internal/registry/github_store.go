package registry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The GitHub integration's state (migration 1540). The package internal/github owns what the rows
// mean; this file stores them. Every secret arrives sealed (internal/secrets) and leaves sealed.

// GitHubApp is the server's GitHub App as the manifest flow created it. An App from [github]
// in config.toml has no row.
type GitHubApp struct {
	AppID      int64
	Slug       string // github.com/apps/<Slug>
	Name       string
	ClientID   string
	OwnerLogin string
	HTMLURL    string

	SealedClientSecret  []byte
	SealedWebhookSecret []byte
	SealedPrivateKey    []byte

	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GitHubAuthorization is a Supavise user's authorization of the App: their GitHub user-to-server
// token. The App's user tokens expire, so a refresh token and both expiries are kept.
type GitHubAuthorization struct {
	ID               int64
	UserID           string // GoTrue user id
	AppID            int64  // the App the token belongs to
	GitHubUserID     int64
	GitHubLogin      string
	SealedAccess     []byte
	SealedRefresh    []byte
	AccessExpiresAt  *time.Time
	RefreshExpiresAt *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// GitHubConnection links a project to a repository.
type GitHubConnection struct {
	ID                  int64
	OrgID               int64
	ProjectRef          string
	InstallationID      int64
	RepositoryID        int64
	RepositoryName      string // owner/name
	Workdir             string
	SupabaseChangesOnly bool
	BranchLimit         int
	NewBranchPerPR      bool
	CreatedBy           string // GoTrue user id
	// ProdFunctions maps a function slug to the hash of the source last deployed to production, so
	// that the next production deployment deploys only what changed.
	ProdFunctions map[string]string
	InsertedAt    time.Time
	UpdatedAt     time.Time
}

// GitHubBranchLink ties a preview branch to the pull request or Git branch it follows.
type GitHubBranchLink struct {
	BranchRef      string
	ProjectRef     string // the parent
	RepositoryID   int64
	RepositoryName string
	InstallationID int64
	PRNumber       int // 0: a Git branch with no pull request
	HeadRef        string
	HeadSHA        string
	CheckRunID     int64
	CommentID      int64
	// Functions maps a function slug to the hash of the source last deployed from the repository,
	// so that a later commit deploys only the functions that changed.
	Functions map[string]string
	// SeededAt is when the repository's seed was applied to the branch (or found not to apply). It
	// stays empty until a run gets past the seed step, so that a first run that failed in a
	// migration, or that a newer commit cancelled, does not leave the branch without its seed.
	SeededAt  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GitHubStore keeps the integration's state. Both registries implement it; internal/github
// reaches it through GitHubOf.
type GitHubStore interface {
	// GetGitHubApp returns the server's App, or ErrNotFound.
	GetGitHubApp(ctx context.Context) (*GitHubApp, error)
	// PutGitHubApp replaces the server's App. The users' authorizations, the manifest states, the
	// connections and the branch links belong to the App they were made for (they name its
	// installations), so they are removed with it; the branches themselves stay.
	PutGitHubApp(ctx context.Context, a *GitHubApp) error
	// DeleteGitHubApp forgets the App, the authorizations, the manifest states, the connections and
	// the branch links. It reports whether there was an App.
	DeleteGitHubApp(ctx context.Context) (bool, error)

	// PutGitHubState records a manifest flow in progress: stateHash is the SHA-256 of the state and
	// params says what the flow was asked for (the owning account, the App's name).
	PutGitHubState(ctx context.Context, stateHash []byte, params, createdBy string, expires time.Time) error
	// PeekGitHubState returns the params of a state without using it; ok is false when it is unknown
	// or has expired.
	PeekGitHubState(ctx context.Context, stateHash []byte, now time.Time) (params string, ok bool, err error)
	// TakeGitHubState removes the state and returns its params; ok is false when it is unknown or
	// has expired. A state is good once.
	TakeGitHubState(ctx context.Context, stateHash []byte, now time.Time) (params string, ok bool, err error)

	GetGitHubAuthorization(ctx context.Context, userID string) (*GitHubAuthorization, error)
	// PutGitHubAuthorization stores the user's authorization, replacing the one they had.
	PutGitHubAuthorization(ctx context.Context, a *GitHubAuthorization) error
	// DeleteGitHubAuthorization removes the user's authorization and reports whether they had one.
	DeleteGitHubAuthorization(ctx context.Context, userID string) (bool, error)

	GetGitHubConnection(ctx context.Context, id int64) (*GitHubConnection, error)
	GetGitHubConnectionByProject(ctx context.Context, projectRef string) (*GitHubConnection, error)
	// ListGitHubConnections returns an organization's connections, oldest first.
	ListGitHubConnections(ctx context.Context, orgID int64) ([]GitHubConnection, error)
	// ListGitHubConnectionsByRepo returns the connections to a repository, any organization.
	ListGitHubConnectionsByRepo(ctx context.Context, repositoryID int64) ([]GitHubConnection, error)
	// CreateGitHubConnection stores c (ID, InsertedAt and UpdatedAt are set); ErrConflict when the
	// project already has a connection.
	CreateGitHubConnection(ctx context.Context, c *GitHubConnection) error
	// UpdateGitHubConnection saves the options of the connection (workdir, supabase_changes_only,
	// branch_limit, new_branch_per_pr) and the repository's current name.
	UpdateGitHubConnection(ctx context.Context, c *GitHubConnection) error
	DeleteGitHubConnection(ctx context.Context, id int64) error
	// SetGitHubProdFunctions records the function hashes of the last production deployment.
	SetGitHubProdFunctions(ctx context.Context, id int64, hashes map[string]string) error

	GetGitHubBranchLink(ctx context.Context, branchRef string) (*GitHubBranchLink, error)
	// FindGitHubBranchLink returns the link of a project's branch that follows the Git branch (or
	// the pull request, when pr is not 0) of a repository, or ErrNotFound.
	FindGitHubBranchLink(ctx context.Context, projectRef string, repositoryID int64, headRef string, pr int) (*GitHubBranchLink, error)
	ListGitHubBranchLinks(ctx context.Context, projectRef string) ([]GitHubBranchLink, error)
	// PutGitHubBranchLink inserts or replaces the link of a branch.
	PutGitHubBranchLink(ctx context.Context, l *GitHubBranchLink) error
	DeleteGitHubBranchLink(ctx context.Context, branchRef string) error
	// GitHubPRNumbers returns the pull request number of each of the branches that follows one.
	GitHubPRNumbers(ctx context.Context, branchRefs []string) (map[string]int, error)

	// RecordGitHubDelivery notes a webhook delivery; fresh is false when the id was seen before.
	RecordGitHubDelivery(ctx context.Context, id, event string, now time.Time) (fresh bool, err error)
	// ForgetGitHubDelivery removes a delivery that could not be handled, so that GitHub can send it again.
	ForgetGitHubDelivery(ctx context.Context, id string) error
	// PruneGitHubDeliveries removes the deliveries received before the time and the manifest states
	// that expired before it.
	PruneGitHubDeliveries(ctx context.Context, before time.Time) error
}

// GitHubOf returns reg's GitHubStore, or nil when it has none.
func GitHubOf(reg Registry) GitHubStore {
	s, _ := reg.(GitHubStore)
	return s
}

var (
	_ GitHubStore = (*Memory)(nil)
	_ GitHubStore = (*Postgres)(nil)
)

// ---- Postgres ---------------------------------------------------------------------------------

// missingTable reports whether err says a table is not there (SQLSTATE 42P01). A server on this
// release can read a registry whose leader has not applied migration 1540 yet, in a cluster that is
// being upgraded one server at a time; for the GitHub state that means "none".
func missingTable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42P01"
}

func (r *Postgres) GetGitHubApp(ctx context.Context) (*GitHubApp, error) {
	var a GitHubApp
	err := r.pool.QueryRow(ctx, `
		select app_id, slug, name, client_id, owner_login, html_url, sealed_client_secret, sealed_webhook_secret,
		       sealed_private_key, created_by, created_at, updated_at
		  from supavise.github_app`).Scan(&a.AppID, &a.Slug, &a.Name, &a.ClientID, &a.OwnerLogin, &a.HTMLURL,
		&a.SealedClientSecret, &a.SealedWebhookSecret, &a.SealedPrivateKey, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	if missingTable(err) {
		return nil, ErrNotFound // the registry has not got migration 1540 yet: no App
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

func (r *Postgres) PutGitHubApp(ctx context.Context, a *GitHubApp) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		// The tokens and states belong to the App that issued them.
		if _, err := tx.Exec(ctx, `delete from supavise.github_authorizations`); err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `delete from supavise.github_manifest_states`); err != nil {
			return mapErr(err)
		}
		if err := dropGitHubInstallations(ctx, tx); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			insert into supavise.github_app (singleton, app_id, slug, name, client_id, owner_login, html_url,
			                                 sealed_client_secret, sealed_webhook_secret, sealed_private_key, created_by)
			values (true, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			on conflict (singleton) do update set app_id = excluded.app_id, slug = excluded.slug, name = excluded.name,
			  client_id = excluded.client_id, owner_login = excluded.owner_login, html_url = excluded.html_url,
			  sealed_client_secret = excluded.sealed_client_secret, sealed_webhook_secret = excluded.sealed_webhook_secret,
			  sealed_private_key = excluded.sealed_private_key, created_by = excluded.created_by,
			  created_at = now(), updated_at = now()
			returning created_at, updated_at`,
			a.AppID, a.Slug, a.Name, a.ClientID, a.OwnerLogin, a.HTMLURL, a.SealedClientSecret, a.SealedWebhookSecret,
			a.SealedPrivateKey, a.CreatedBy).Scan(&a.CreatedAt, &a.UpdatedAt)
		return mapErr(err)
	})
}

func (r *Postgres) DeleteGitHubApp(ctx context.Context) (bool, error) {
	var had bool
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `delete from supavise.github_app`)
		if err != nil {
			return mapErr(err)
		}
		had = tag.RowsAffected() > 0
		if _, err := tx.Exec(ctx, `delete from supavise.github_authorizations`); err != nil {
			return mapErr(err)
		}
		if _, err = tx.Exec(ctx, `delete from supavise.github_manifest_states`); err != nil {
			return mapErr(err)
		}
		return dropGitHubInstallations(ctx, tx)
	})
	return had, err
}

// dropGitHubInstallations removes what names an installation of the App that is going: a
// connection keeps the installation id of its repository, a branch link the one of its pull request.
func dropGitHubInstallations(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `delete from supavise.github_branch_links`); err != nil {
		return mapErr(err)
	}
	_, err := tx.Exec(ctx, `delete from supavise.github_connections`)
	return mapErr(err)
}

func (r *Postgres) PutGitHubState(ctx context.Context, stateHash []byte, params, createdBy string, expires time.Time) error {
	_, err := r.pool.Exec(ctx, `
		insert into supavise.github_manifest_states (state_hash, params, created_by, expires_at) values ($1, $2, $3, $4)`,
		stateHash, params, createdBy, expires)
	return mapErr(err)
}

func (r *Postgres) PeekGitHubState(ctx context.Context, stateHash []byte, now time.Time) (string, bool, error) {
	var params string
	err := r.pool.QueryRow(ctx, `select params from supavise.github_manifest_states where state_hash = $1 and expires_at > $2`,
		stateHash, now).Scan(&params)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapErr(err)
	}
	return params, true, nil
}

func (r *Postgres) TakeGitHubState(ctx context.Context, stateHash []byte, now time.Time) (string, bool, error) {
	var params string
	err := r.pool.QueryRow(ctx, `
		delete from supavise.github_manifest_states where state_hash = $1 and expires_at > $2 returning params`,
		stateHash, now).Scan(&params)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapErr(err)
	}
	return params, true, nil
}

const githubAuthCols = `id, user_id, app_id, github_user_id, github_login, sealed_access, sealed_refresh,
	access_expires_at, refresh_expires_at, created_at, updated_at`

func scanGitHubAuth(row pgx.Row) (*GitHubAuthorization, error) {
	var a GitHubAuthorization
	err := row.Scan(&a.ID, &a.UserID, &a.AppID, &a.GitHubUserID, &a.GitHubLogin, &a.SealedAccess, &a.SealedRefresh,
		&a.AccessExpiresAt, &a.RefreshExpiresAt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

func (r *Postgres) GetGitHubAuthorization(ctx context.Context, userID string) (*GitHubAuthorization, error) {
	return scanGitHubAuth(r.pool.QueryRow(ctx, `select `+githubAuthCols+` from supavise.github_authorizations where user_id = $1`, userID))
}

func (r *Postgres) PutGitHubAuthorization(ctx context.Context, a *GitHubAuthorization) error {
	err := r.pool.QueryRow(ctx, `
		insert into supavise.github_authorizations (user_id, app_id, github_user_id, github_login, sealed_access, sealed_refresh,
		                                            access_expires_at, refresh_expires_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (user_id) do update set app_id = excluded.app_id, github_user_id = excluded.github_user_id,
		  github_login = excluded.github_login, sealed_access = excluded.sealed_access, sealed_refresh = excluded.sealed_refresh,
		  access_expires_at = excluded.access_expires_at, refresh_expires_at = excluded.refresh_expires_at, updated_at = now()
		returning id, created_at, updated_at`,
		a.UserID, a.AppID, a.GitHubUserID, a.GitHubLogin, a.SealedAccess, a.SealedRefresh, a.AccessExpiresAt, a.RefreshExpiresAt,
	).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
	return mapErr(err)
}

func (r *Postgres) DeleteGitHubAuthorization(ctx context.Context, userID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `delete from supavise.github_authorizations where user_id = $1`, userID)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() > 0, nil
}

const githubConnCols = `id, org_id, project_ref, installation_id, repository_id, repository_name, workdir, supabase_changes_only,
	branch_limit, new_branch_per_pr, created_by, prod_functions, inserted_at, updated_at`

func scanGitHubConn(row pgx.Row) (*GitHubConnection, error) {
	var c GitHubConnection
	var fns []byte
	err := row.Scan(&c.ID, &c.OrgID, &c.ProjectRef, &c.InstallationID, &c.RepositoryID, &c.RepositoryName, &c.Workdir,
		&c.SupabaseChangesOnly, &c.BranchLimit, &c.NewBranchPerPR, &c.CreatedBy, &fns, &c.InsertedAt, &c.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if len(fns) > 0 {
		_ = json.Unmarshal(fns, &c.ProdFunctions)
	}
	return &c, nil
}

func (r *Postgres) queryGitHubConns(ctx context.Context, sql string, args ...any) ([]GitHubConnection, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []GitHubConnection
	for rows.Next() {
		c, err := scanGitHubConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, mapErr(rows.Err())
}

func (r *Postgres) GetGitHubConnection(ctx context.Context, id int64) (*GitHubConnection, error) {
	return scanGitHubConn(r.pool.QueryRow(ctx, `select `+githubConnCols+` from supavise.github_connections where id = $1`, id))
}

func (r *Postgres) GetGitHubConnectionByProject(ctx context.Context, projectRef string) (*GitHubConnection, error) {
	return scanGitHubConn(r.pool.QueryRow(ctx, `select `+githubConnCols+` from supavise.github_connections where project_ref = $1`, projectRef))
}

func (r *Postgres) ListGitHubConnections(ctx context.Context, orgID int64) ([]GitHubConnection, error) {
	return r.queryGitHubConns(ctx, `select `+githubConnCols+` from supavise.github_connections where org_id = $1 order by id`, orgID)
}

func (r *Postgres) ListGitHubConnectionsByRepo(ctx context.Context, repositoryID int64) ([]GitHubConnection, error) {
	return r.queryGitHubConns(ctx, `select `+githubConnCols+` from supavise.github_connections where repository_id = $1 order by id`, repositoryID)
}

func (r *Postgres) CreateGitHubConnection(ctx context.Context, c *GitHubConnection) error {
	err := r.pool.QueryRow(ctx, `
		insert into supavise.github_connections (org_id, project_ref, installation_id, repository_id, repository_name, workdir,
		                                         supabase_changes_only, branch_limit, new_branch_per_pr, created_by)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		returning id, inserted_at, updated_at`,
		c.OrgID, c.ProjectRef, c.InstallationID, c.RepositoryID, c.RepositoryName, c.Workdir, c.SupabaseChangesOnly,
		c.BranchLimit, c.NewBranchPerPR, c.CreatedBy).Scan(&c.ID, &c.InsertedAt, &c.UpdatedAt)
	return mapErr(err)
}

func (r *Postgres) UpdateGitHubConnection(ctx context.Context, c *GitHubConnection) error {
	return affected(r.pool.Exec(ctx, `
		update supavise.github_connections set repository_name = $2, workdir = $3, supabase_changes_only = $4,
		  branch_limit = $5, new_branch_per_pr = $6, updated_at = now() where id = $1`,
		c.ID, c.RepositoryName, c.Workdir, c.SupabaseChangesOnly, c.BranchLimit, c.NewBranchPerPR))
}

func (r *Postgres) DeleteGitHubConnection(ctx context.Context, id int64) error {
	return affected(r.pool.Exec(ctx, `delete from supavise.github_connections where id = $1`, id))
}

func (r *Postgres) SetGitHubProdFunctions(ctx context.Context, id int64, hashes map[string]string) error {
	b, err := json.Marshal(nonNilMap(hashes))
	if err != nil {
		return err
	}
	return affected(r.pool.Exec(ctx, `update supavise.github_connections set prod_functions = $2::text::jsonb where id = $1`, id, string(b)))
}

const githubLinkCols = `branch_ref, project_ref, repository_id, repository_name, installation_id, pr_number, head_ref, head_sha,
	check_run_id, comment_id, functions, seeded_at, created_at, updated_at`

func scanGitHubLink(row pgx.Row) (*GitHubBranchLink, error) {
	var l GitHubBranchLink
	var fns []byte
	err := row.Scan(&l.BranchRef, &l.ProjectRef, &l.RepositoryID, &l.RepositoryName, &l.InstallationID, &l.PRNumber, &l.HeadRef,
		&l.HeadSHA, &l.CheckRunID, &l.CommentID, &fns, &l.SeededAt, &l.CreatedAt, &l.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if len(fns) > 0 {
		_ = json.Unmarshal(fns, &l.Functions)
	}
	return &l, nil
}

func (r *Postgres) GetGitHubBranchLink(ctx context.Context, branchRef string) (*GitHubBranchLink, error) {
	return scanGitHubLink(r.pool.QueryRow(ctx, `select `+githubLinkCols+` from supavise.github_branch_links where branch_ref = $1`, branchRef))
}

func (r *Postgres) FindGitHubBranchLink(ctx context.Context, projectRef string, repositoryID int64, headRef string, pr int) (*GitHubBranchLink, error) {
	if pr > 0 {
		return scanGitHubLink(r.pool.QueryRow(ctx, `select `+githubLinkCols+`
			from supavise.github_branch_links where project_ref = $1 and repository_id = $2 and pr_number = $3
			order by created_at desc limit 1`, projectRef, repositoryID, pr))
	}
	return scanGitHubLink(r.pool.QueryRow(ctx, `select `+githubLinkCols+`
		from supavise.github_branch_links where project_ref = $1 and repository_id = $2 and head_ref = $3
		order by pr_number = 0 desc, created_at desc limit 1`, projectRef, repositoryID, headRef))
}

func (r *Postgres) ListGitHubBranchLinks(ctx context.Context, projectRef string) ([]GitHubBranchLink, error) {
	rows, err := r.pool.Query(ctx, `select `+githubLinkCols+` from supavise.github_branch_links where project_ref = $1 order by created_at`, projectRef)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []GitHubBranchLink
	for rows.Next() {
		l, err := scanGitHubLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, mapErr(rows.Err())
}

func (r *Postgres) PutGitHubBranchLink(ctx context.Context, l *GitHubBranchLink) error {
	fns, err := json.Marshal(nonNilMap(l.Functions))
	if err != nil {
		return err
	}
	return mapErr(r.pool.QueryRow(ctx, `
		insert into supavise.github_branch_links (branch_ref, project_ref, repository_id, repository_name, installation_id, pr_number,
		                                          head_ref, head_sha, check_run_id, comment_id, functions, seeded_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text::jsonb, $12)
		on conflict (branch_ref) do update set repository_name = excluded.repository_name, installation_id = excluded.installation_id,
		  pr_number = excluded.pr_number, head_ref = excluded.head_ref, head_sha = excluded.head_sha,
		  check_run_id = excluded.check_run_id, comment_id = excluded.comment_id, functions = excluded.functions,
		  seeded_at = coalesce(supavise.github_branch_links.seeded_at, excluded.seeded_at), updated_at = now()
		returning seeded_at, created_at, updated_at`,
		l.BranchRef, l.ProjectRef, l.RepositoryID, l.RepositoryName, l.InstallationID, l.PRNumber, l.HeadRef, l.HeadSHA,
		l.CheckRunID, l.CommentID, string(fns), l.SeededAt).Scan(&l.SeededAt, &l.CreatedAt, &l.UpdatedAt))
}

func (r *Postgres) DeleteGitHubBranchLink(ctx context.Context, branchRef string) error {
	_, err := r.pool.Exec(ctx, `delete from supavise.github_branch_links where branch_ref = $1`, branchRef)
	return mapErr(err)
}

func (r *Postgres) GitHubPRNumbers(ctx context.Context, branchRefs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(branchRefs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `select branch_ref, pr_number from supavise.github_branch_links where branch_ref = any($1) and pr_number > 0`, branchRefs)
	if missingTable(err) {
		return out, nil // the registry has not got migration 1540 yet: no pull requests
	}
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var n int
		if err := rows.Scan(&ref, &n); err != nil {
			return nil, mapErr(err)
		}
		out[ref] = n
	}
	return out, mapErr(rows.Err())
}

func (r *Postgres) RecordGitHubDelivery(ctx context.Context, id, event string, now time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		insert into supavise.github_deliveries (delivery_id, event, received_at) values ($1, $2, $3)
		on conflict (delivery_id) do nothing`, id, event, now)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r *Postgres) ForgetGitHubDelivery(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `delete from supavise.github_deliveries where delivery_id = $1`, id)
	return mapErr(err)
}

func (r *Postgres) PruneGitHubDeliveries(ctx context.Context, before time.Time) error {
	if _, err := r.pool.Exec(ctx, `delete from supavise.github_deliveries where received_at < $1`, before); err != nil {
		return mapErr(err)
	}
	_, err := r.pool.Exec(ctx, `delete from supavise.github_manifest_states where expires_at < $1`, before)
	return mapErr(err)
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

func TestAuthorizeKeepsTheUsersTokenSealed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Authorization(ctx, "u1"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("before authorizing: %v", err)
	}
	if _, err := h.svc.Authorize(ctx, "u1", "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad code: %v", err)
	}
	if _, err := h.svc.Authorize(ctx, "u1", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no code: %v", err)
	}
	a, err := h.svc.Authorize(ctx, "u1", "good-code")
	if err != nil || a.GitHubLogin != "octo" || a.GitHubUserID != 7 || a.ID == 0 {
		t.Fatalf("authorization = %+v, %v", a, err)
	}
	row, _ := h.reg.GetGitHubAuthorization(ctx, "u1")
	if strings.Contains(string(row.SealedAccess), "ghu_") || strings.Contains(string(row.SealedRefresh), "ghr_") {
		t.Fatal("the user's tokens are stored in the clear")
	}
	if row.AccessExpiresAt == nil || row.RefreshExpiresAt == nil {
		t.Fatalf("expiries = %v %v", row.AccessExpiresAt, row.RefreshExpiresAt)
	}
	// Another user has none.
	if _, err := h.svc.Authorization(ctx, "u2"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("another user: %v", err)
	}
	// The repositories come through the user's own token.
	repos, partial, err := h.svc.Repositories(ctx, "u1")
	if err != nil || partial || len(repos) != 1 || repos[0].ID != h.gh.repoID || repos[0].Name != "acme/app" || repos[0].InstallationID != h.gh.instID {
		t.Fatalf("repositories = %+v %v %v", repos, partial, err)
	}
	if _, _, err := h.svc.Repositories(ctx, "u2"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("repositories of another user: %v", err)
	}
}

func TestAnExpiringTokenIsRefreshedOnceAndAFailedRefreshEndsTheAuthorization(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Authorize(ctx, "u1", "good-code"); err != nil {
		t.Fatal(err)
	}
	// Make the stored token about to expire.
	row, _ := h.reg.GetGitHubAuthorization(ctx, "u1")
	soon := h.svc.now().Add(10 * time.Second)
	row.AccessExpiresAt = &soon
	if err := h.reg.PutGitHubAuthorization(ctx, row); err != nil {
		t.Fatal(err)
	}
	// Two calls at once: one refresh (a refresh token works once).
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, _, err := h.svc.Repositories(ctx, "u1"); errs <- err }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("repositories during a refresh: %v", err)
		}
	}
	if h.gh.refreshed != 1 {
		t.Fatalf("the token was refreshed %d times", h.gh.refreshed)
	}
	// The refreshed token is stored and used.
	row, _ = h.reg.GetGitHubAuthorization(ctx, "u1")
	if row.AccessExpiresAt == nil || row.AccessExpiresAt.Before(h.svc.now().Add(time.Hour)) {
		t.Fatalf("expiry after the refresh = %v", row.AccessExpiresAt)
	}

	// A refresh token GitHub no longer knows ends the authorization.
	h.gh.mu.Lock()
	h.gh.refreshToken = "someone-elses"
	h.gh.mu.Unlock()
	soon = h.svc.now().Add(5 * time.Second)
	row.AccessExpiresAt = &soon
	_ = h.reg.PutGitHubAuthorization(ctx, row)
	if _, _, err := h.svc.Repositories(ctx, "u1"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a refresh GitHub refused: %v", err)
	}
	if _, err := h.reg.GetGitHubAuthorization(ctx, "u1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatal("the authorization outlived its refresh token")
	}
}

func TestATokenGitHubRefusesEndsTheAuthorization(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _ = h.svc.Authorize(ctx, "u1", "good-code")
	h.gh.mu.Lock()
	h.gh.userToken = "ghu_revoked_on_github"
	h.gh.refreshToken = "revoked-too"
	h.gh.mu.Unlock()
	if _, _, err := h.svc.Repositories(ctx, "u1"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a revoked token: %v", err)
	}
	if _, err := h.svc.Authorization(ctx, "u1"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatal("the authorization survived a 401")
	}
}

func TestATokenThatExpiredEarlyIsRefreshedAndTheCallRepeated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _ = h.svc.Authorize(ctx, "u1", "good-code")
	// GitHub forgets the access token before our clock says it expires (a revoked or early-expired token).
	h.gh.mu.Lock()
	h.gh.userToken = "ghu_expired_early"
	h.gh.mu.Unlock()
	repos, _, err := h.svc.Repositories(ctx, "u1")
	if err != nil {
		t.Fatalf("repositories after an early expiry: %v", err)
	}
	if len(repos) != 1 || h.gh.refreshed != 1 {
		t.Fatalf("repos %v, refreshed %d", repos, h.gh.refreshed)
	}
	if _, err := h.svc.Authorization(ctx, "u1"); err != nil {
		t.Fatalf("the authorization was lost: %v", err)
	}
}

func TestDeauthorizeRevokesTheTokenAndForgetsIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if had, err := h.svc.Deauthorize(ctx, "u1"); err != nil || had {
		t.Fatalf("nothing to remove: %v %v", had, err)
	}
	_, _ = h.svc.Authorize(ctx, "u1", "good-code")
	had, err := h.svc.Deauthorize(ctx, "u1")
	if err != nil || !had {
		t.Fatalf("deauthorize: %v %v", had, err)
	}
	if h.gh.callsTo("DELETE /applications/") != 1 {
		t.Fatal("the token was not revoked at GitHub")
	}
	if _, err := h.svc.Authorization(ctx, "u1"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatal("still authorized")
	}
}

func TestAnAuthorizationOfAnotherAppIsNotOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _ = h.svc.Authorize(ctx, "u1", "good-code")
	row, _ := h.reg.GetGitHubAuthorization(ctx, "u1")
	row.AppID++
	_ = h.reg.PutGitHubAuthorization(ctx, row)
	if _, err := h.svc.Authorization(ctx, "u1"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a token of another App: %v", err)
	}
}

func TestConnectChecksTheRepositoryAgainstTheUsersOwnReach(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.reg.DeleteGitHubConnection(ctx, h.conn.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = h.svc.Authorize(ctx, "u1", "good-code")
	in := ConnectInput{UserID: "u1", OrgID: 1, ProjectRef: prodRef, InstallationID: h.gh.instID, RepositoryID: 5}
	if _, err := h.svc.Connect(ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a repository the user cannot reach: %v", err)
	}
	in.RepositoryID, in.InstallationID = h.gh.repoID, 999
	if _, err := h.svc.Connect(ctx, in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a repository of another installation: %v", err)
	}
	in.InstallationID = h.gh.instID
	in.Workdir = "../x"
	if _, err := h.svc.Connect(ctx, in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a working directory outside the repository: %v", err)
	}
	in.Workdir = "./apps/web/"
	lim := 0
	in.BranchLimit = &lim
	if _, err := h.svc.Connect(ctx, in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a branch limit of 0: %v", err)
	}
	in.BranchLimit = nil
	c, err := h.svc.Connect(ctx, in)
	if err != nil || c.Workdir != "apps/web" || c.BranchLimit != DefaultBranchLimit || c.RepositoryName != "acme/app" || c.NewBranchPerPR || c.SupabaseChangesOnly {
		t.Fatalf("connection = %+v, %v", c, err)
	}
	if _, err := h.svc.Connect(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second connection: %v", err)
	}
}

func TestDisconnectForgetsTheLinksOfThatRepositoryButNotTheBranches(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 1, "feature", "sha-1"))
	b := h.br.byName("feature")
	if err := h.svc.Disconnect(ctx, h.conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reg.GetGitHubBranchLink(ctx, b.Ref); err == nil {
		t.Fatal("the link outlived the connection")
	}
	if len(h.br.deleted) != 0 {
		t.Fatal("disconnecting deleted a branch")
	}
	if err := h.svc.Disconnect(ctx, h.conn.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disconnect again: %v", err)
	}
}

func TestCleanWorkdir(t *testing.T) {
	for in, want := range map[string]string{"": "", ".": "", "./": "", "/": "", "apps/web": "apps/web", "./apps/web/": "apps/web", "/apps//web": "apps/web", "a\\b": "a/b"} {
		if got, err := CleanWorkdir(in); err != nil || got != want {
			t.Errorf("CleanWorkdir(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"..", "../x", "a/../../x", "a/..", "x\x00y"} {
		if _, err := CleanWorkdir(bad); err == nil {
			t.Errorf("CleanWorkdir(%q) accepted", bad)
		}
	}
}

func TestBranchesAndBranchCheck(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _ = h.svc.Authorize(ctx, "u1", "good-code")
	names, err := h.svc.Branches(ctx, "u1", h.gh.repoID)
	if err != nil || len(names) != 2 {
		t.Fatalf("branches = %v, %v", names, err)
	}
	if name, err := h.svc.Branch(ctx, "u1", h.gh.repoID, "main"); err != nil || name != "main" {
		t.Fatalf("branch = %q, %v", name, err)
	}
	if _, err := h.svc.Branch(ctx, "u1", h.gh.repoID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing branch: %v", err)
	}
	if _, err := h.svc.Branch(ctx, "u1", h.gh.repoID, " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a blank branch: %v", err)
	}
	if _, err := h.svc.Branches(ctx, "u1", 424242); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a repository the user cannot reach: %v", err)
	}
}

func TestConnectionConfigIsParsedAndPassedOn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.gh.setCommit("head-main", map[string]string{"supabase/config.toml": "project_id = \"app\"\n[auth]\nenable_signup = true\nsite_url = \"https://x.test\"\n[storage.buckets.avatars]\npublic = true\n"})
	doc, err := h.svc.ConnectionConfig(ctx, &h.conn, "")
	if err != nil || doc.Path != "supabase/config.toml" || doc.Ref != nil || doc.SHA == "" {
		t.Fatalf("doc = %+v, %v", doc, err)
	}
	auth := doc.Config["auth"].(map[string]any)
	if auth["site_url"] != "https://x.test" || auth["enable_signup"] != true {
		t.Fatalf("config = %v", doc.Config)
	}
	h.gh.setCommit("feature", map[string]string{"supabase/config.toml": "this = is = broken"})
	if _, err := h.svc.ConnectionConfig(ctx, &h.conn, "feature"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("broken TOML: %v", err)
	}
	if _, err := h.svc.ConnectionConfig(ctx, &h.conn, "missing-ref"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a ref that does not exist: %v", err)
	}
	conn := h.conn
	conn.Workdir = "apps/web"
	h.gh.setCommit("feature", map[string]string{"apps/web/supabase/config.toml": "project_id = \"x\""})
	doc, err = h.svc.ConnectionConfig(ctx, &conn, "feature")
	if err != nil || doc.Path != "apps/web/supabase/config.toml" || *doc.Ref != "feature" {
		t.Fatalf("with a working directory: %+v, %v", doc, err)
	}
}

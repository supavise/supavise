package registry

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestMemoryGitHubStore(t *testing.T) { testGitHubStore(t, NewMemory()) }

// TestPostgresGitHubStore runs the same checks against a real database (see TestPostgres).
func TestPostgresGitHubStore(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Pool().Exec(ctx, `truncate supavise.github_app, supavise.github_manifest_states, supavise.github_authorizations,
		supavise.github_connections, supavise.github_branch_links, supavise.github_deliveries, supavise.projects, supavise.organizations cascade`); err != nil {
		t.Fatal(err)
	}
	testGitHubStore(t, r)
}

func testGitHubStore(t *testing.T, reg Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := GitHubOf(reg)
	if st == nil {
		t.Fatal("registry has no GitHubStore")
	}
	org, err := reg.CreateOrganization(ctx, "gh-org", "GH")
	if err != nil {
		t.Fatal(err)
	}
	const parent, other, branch = "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccc"
	for _, ref := range []string{parent, other, branch} {
		if err := reg.CreateProject(ctx, &Project{Ref: ref, OrgID: org.ID, Name: ref}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)

	// The App: none, then one, and a replacement takes the users' tokens and the states with it.
	if _, err := st.GetGitHubApp(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an App before there is one: %v", err)
	}
	app := &GitHubApp{AppID: 750123456, Slug: "my-app", Name: "My app", ClientID: "Iv1.x", OwnerLogin: "acme", HTMLURL: "https://github.com/apps/my-app",
		SealedClientSecret: []byte{1, 2}, SealedWebhookSecret: []byte{3, 4}, SealedPrivateKey: []byte{5, 6}, CreatedBy: "cli"}
	if err := st.PutGitHubApp(ctx, app); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetGitHubApp(ctx)
	if err != nil || got.AppID != 750123456 || got.Slug != "my-app" || got.OwnerLogin != "acme" || !reflect.DeepEqual(got.SealedPrivateKey, []byte{5, 6}) || got.CreatedAt.IsZero() {
		t.Fatalf("app = %+v, %v", got, err)
	}

	// Manifest states are single use, expire, and can be looked at once without being used.
	hash := []byte("state-hash-0123456789abcdef0123456")
	if err := st.PutGitHubState(ctx, hash, `{"owner":"acme"}`, "cli", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.PutGitHubState(ctx, hash, "again", "cli", now.Add(time.Hour)); !errors.Is(err, ErrConflict) && err == nil {
		t.Fatalf("the same state twice: %v", err)
	}
	if p, ok, err := st.PeekGitHubState(ctx, hash, now); err != nil || !ok || p != `{"owner":"acme"}` {
		t.Fatalf("peek = %q %v %v", p, ok, err)
	}
	if _, ok, _ := st.PeekGitHubState(ctx, hash, now.Add(2*time.Hour)); ok {
		t.Fatal("an expired state can be peeked")
	}
	if p, ok, err := st.TakeGitHubState(ctx, hash, now); err != nil || !ok || p != `{"owner":"acme"}` {
		t.Fatalf("take = %q %v %v", p, ok, err)
	}
	if _, ok, _ := st.TakeGitHubState(ctx, hash, now); ok {
		t.Fatal("a state worked twice")
	}
	expired := []byte("expired-state-hash-0123456789abcd")
	_ = st.PutGitHubState(ctx, expired, "", "", now.Add(-time.Minute))
	if _, ok, _ := st.TakeGitHubState(ctx, expired, now); ok {
		t.Fatal("an expired state worked")
	}

	// Authorizations: one per user, replaced on a new authorization.
	exp := now.Add(8 * time.Hour)
	a := &GitHubAuthorization{UserID: "u1", AppID: 750123456, GitHubUserID: 7, GitHubLogin: "octo", SealedAccess: []byte("a1"), SealedRefresh: []byte("r1"), AccessExpiresAt: &exp}
	if err := st.PutGitHubAuthorization(ctx, a); err != nil || a.ID == 0 {
		t.Fatalf("put authorization: %v %+v", err, a)
	}
	first := a.ID
	a2 := &GitHubAuthorization{UserID: "u1", AppID: 750123456, GitHubUserID: 7, GitHubLogin: "octo2", SealedAccess: []byte("a2")}
	if err := st.PutGitHubAuthorization(ctx, a2); err != nil || a2.ID != first {
		t.Fatalf("a second authorization of the user must keep its id: %v %d %d", err, a2.ID, first)
	}
	g, err := st.GetGitHubAuthorization(ctx, "u1")
	if err != nil || g.GitHubLogin != "octo2" || string(g.SealedAccess) != "a2" || len(g.SealedRefresh) != 0 || g.AccessExpiresAt != nil {
		t.Fatalf("authorization = %+v, %v", g, err)
	}
	if _, err := st.GetGitHubAuthorization(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("authorization of nobody: %v", err)
	}

	// Connections: one per project; listed by organization and by repository.
	c := &GitHubConnection{OrgID: org.ID, ProjectRef: parent, InstallationID: 49123456, RepositoryID: 750123456, RepositoryName: "acme/app", Workdir: "apps/web",
		SupabaseChangesOnly: true, BranchLimit: 7, NewBranchPerPR: true, CreatedBy: "u1"}
	if err := st.CreateGitHubConnection(ctx, c); err != nil || c.ID == 0 || c.InsertedAt.IsZero() {
		t.Fatalf("create connection: %v %+v", err, c)
	}
	if err := st.CreateGitHubConnection(ctx, &GitHubConnection{OrgID: org.ID, ProjectRef: parent, InstallationID: 1, RepositoryID: 2, RepositoryName: "x/y"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second connection of the project: %v", err)
	}
	c2 := &GitHubConnection{OrgID: org.ID, ProjectRef: other, InstallationID: 49123456, RepositoryID: 750123456, RepositoryName: "acme/app", BranchLimit: 50}
	if err := st.CreateGitHubConnection(ctx, c2); err != nil {
		t.Fatal(err)
	}
	byOrg, err := st.ListGitHubConnections(ctx, org.ID)
	if err != nil || len(byOrg) != 2 || byOrg[0].ID != c.ID || byOrg[0].Workdir != "apps/web" || !byOrg[0].SupabaseChangesOnly || byOrg[0].BranchLimit != 7 || byOrg[0].InstallationID != 49123456 {
		t.Fatalf("by org = %+v, %v", byOrg, err)
	}
	byRepo, _ := st.ListGitHubConnectionsByRepo(ctx, 750123456)
	if len(byRepo) != 2 {
		t.Fatalf("by repository = %d", len(byRepo))
	}
	if none, _ := st.ListGitHubConnectionsByRepo(ctx, 1); len(none) != 0 {
		t.Fatalf("by another repository = %d", len(none))
	}
	if by, err := st.GetGitHubConnectionByProject(ctx, parent); err != nil || by.ID != c.ID {
		t.Fatalf("by project = %+v, %v", by, err)
	}
	c.Workdir, c.BranchLimit, c.NewBranchPerPR, c.RepositoryName = "", 9, false, "acme/renamed"
	if err := st.UpdateGitHubConnection(ctx, c); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.GetGitHubConnection(ctx, c.ID)
	if got2.Workdir != "" || got2.BranchLimit != 9 || got2.NewBranchPerPR || got2.RepositoryName != "acme/renamed" || got2.InstallationID != 49123456 || got2.OrgID != org.ID {
		t.Fatalf("updated = %+v", got2)
	}
	if err := st.SetGitHubProdFunctions(ctx, c.ID, map[string]string{"hello": "abc"}); err != nil {
		t.Fatal(err)
	}
	if got2, _ = st.GetGitHubConnection(ctx, c.ID); got2.ProdFunctions["hello"] != "abc" {
		t.Fatalf("production functions = %v", got2.ProdFunctions)
	}
	if err := st.UpdateGitHubConnection(ctx, &GitHubConnection{ID: 99999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of an unknown connection: %v", err)
	}

	// Branch links.
	l := &GitHubBranchLink{BranchRef: branch, ProjectRef: parent, RepositoryID: 750123456, RepositoryName: "acme/app", InstallationID: 49123456,
		PRNumber: 12, HeadRef: "feature", HeadSHA: "abc", CheckRunID: 5, CommentID: 6, Functions: map[string]string{"hello": "h1"}}
	if err := st.PutGitHubBranchLink(ctx, l); err != nil || l.CreatedAt.IsZero() {
		t.Fatalf("put link: %v %+v", err, l)
	}
	l.HeadSHA, l.Functions = "def", map[string]string{"hello": "h2", "other": "o"}
	if err := st.PutGitHubBranchLink(ctx, l); err != nil {
		t.Fatal(err)
	}
	gl, err := st.GetGitHubBranchLink(ctx, branch)
	if err != nil || gl.HeadSHA != "def" || gl.PRNumber != 12 || gl.CheckRunID != 5 || gl.Functions["other"] != "o" || gl.ProjectRef != parent {
		t.Fatalf("link = %+v, %v", gl, err)
	}
	if byPR, err := st.FindGitHubBranchLink(ctx, parent, 750123456, "", 12); err != nil || byPR.BranchRef != branch {
		t.Fatalf("by pull request = %+v, %v", byPR, err)
	}
	if byHead, err := st.FindGitHubBranchLink(ctx, parent, 750123456, "feature", 0); err != nil || byHead.BranchRef != branch {
		t.Fatalf("by head = %+v, %v", byHead, err)
	}
	if _, err := st.FindGitHubBranchLink(ctx, parent, 750123456, "other", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("by another head: %v", err)
	}
	if _, err := st.FindGitHubBranchLink(ctx, other, 750123456, "feature", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("under another project: %v", err)
	}
	if links, _ := st.ListGitHubBranchLinks(ctx, parent); len(links) != 1 {
		t.Fatalf("links = %d", len(links))
	}
	nums, err := st.GitHubPRNumbers(ctx, []string{branch, other})
	if err != nil || nums[branch] != 12 || len(nums) != 1 {
		t.Fatalf("pr numbers = %v, %v", nums, err)
	}
	if empty, err := st.GitHubPRNumbers(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("pr numbers of none = %v, %v", empty, err)
	}
	if err := st.DeleteGitHubBranchLink(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetGitHubBranchLink(ctx, branch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link after delete: %v", err)
	}

	// Deliveries: remembered once, forgotten on request, pruned by age.
	if fresh, err := st.RecordGitHubDelivery(ctx, "d1", "push", now); err != nil || !fresh {
		t.Fatalf("first delivery: %v %v", fresh, err)
	}
	if fresh, _ := st.RecordGitHubDelivery(ctx, "d1", "push", now); fresh {
		t.Fatal("a delivery id was fresh twice")
	}
	if err := st.ForgetGitHubDelivery(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := st.RecordGitHubDelivery(ctx, "d1", "push", now); !fresh {
		t.Fatal("a forgotten delivery was not fresh")
	}
	_, _ = st.RecordGitHubDelivery(ctx, "old", "push", now.Add(-100*time.Hour))
	if err := st.PruneGitHubDeliveries(ctx, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := st.RecordGitHubDelivery(ctx, "old", "push", now); !fresh {
		t.Fatal("an old delivery survived the prune")
	}
	if fresh, _ := st.RecordGitHubDelivery(ctx, "d1", "push", now); fresh {
		t.Fatal("a recent delivery was pruned")
	}

	// Connections go with the connection's project or on request.
	if err := st.DeleteGitHubConnection(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteGitHubConnection(ctx, c2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: %v", err)
	}

	// Replacing the App takes the users' tokens and the states with it; removing it too.
	_ = st.PutGitHubState(ctx, hash, "x", "", now.Add(time.Hour))
	app2 := *app
	app2.AppID = 1
	if err := st.PutGitHubApp(ctx, &app2); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetGitHubAuthorization(ctx, "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an authorization outlived its App: %v", err)
	}
	if _, ok, _ := st.PeekGitHubState(ctx, hash, now); ok {
		t.Fatal("a state outlived the App it was made for")
	}
	_ = st.PutGitHubAuthorization(ctx, a)
	had, err := st.DeleteGitHubApp(ctx)
	if err != nil || !had {
		t.Fatalf("delete App: %v %v", had, err)
	}
	if had, _ = st.DeleteGitHubApp(ctx); had {
		t.Fatal("deleted twice")
	}
	if _, err := st.GetGitHubAuthorization(ctx, "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an authorization outlived the App: %v", err)
	}
	if removed, err := st.DeleteGitHubAuthorization(ctx, "u1"); err != nil || removed {
		t.Fatalf("remove authorization of nobody: %v %v", removed, err)
	}
}

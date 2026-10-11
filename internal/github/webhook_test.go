package github

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

func TestSignatureOK(t *testing.T) {
	body := []byte(`{"a":1}`)
	h := &harness{gh: &fakeGitHub{webhookSecret: "s3"}}
	sig := h.sign(body)
	if !SignatureOK("s3", body, sig) {
		t.Fatal("a good signature is refused")
	}
	for name, bad := range map[string]string{
		"wrong secret": h.sign([]byte("other")),
		"no prefix":    strings.TrimPrefix(sig, "sha256="),
		"sha1":         "sha1=" + strings.TrimPrefix(sig, "sha256="),
		"not hex":      "sha256=zz",
		"empty":        "",
		"truncated":    sig[:len(sig)-2],
	} {
		if SignatureOK("s3", body, bad) {
			t.Errorf("%s: accepted", name)
		}
	}
	if SignatureOK("other", body, sig) {
		t.Error("the wrong secret verified")
	}
}

func TestWebhookRejectsABadSignatureAndAnUnconfiguredServer(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{}`)
	res := h.svc.HandleWebhook(context.Background(), Delivery{Event: "pull_request", ID: "d1", Signature: "sha256=00", Body: body})
	if res.Status != http.StatusUnauthorized {
		t.Fatalf("a bad signature answered %d", res.Status)
	}
	res = h.svc.HandleWebhook(context.Background(), Delivery{Event: "pull_request", ID: "d1", Body: body})
	if res.Status != http.StatusUnauthorized {
		t.Fatalf("a missing signature answered %d", res.Status)
	}
	// Nothing was recorded for a delivery that failed the signature check.
	if fresh, _ := h.reg.RecordGitHubDelivery(context.Background(), "d1", "x", h.svc.now()); !fresh {
		t.Fatal("the unsigned delivery was remembered")
	}
	res = h.svc.HandleWebhook(context.Background(), Delivery{Event: "ping", ID: "d2", Signature: h.sign(body), Body: body})
	if res.Status != http.StatusOK || res.Message != "pong" {
		t.Fatalf("ping: %+v", res)
	}

	none := newHarness(t, func(c *config.Config) { c.GitHub = config.GitHub{} })
	res = none.svc.HandleWebhook(context.Background(), Delivery{Event: "ping", ID: "d3", Signature: "sha256=00", Body: body})
	if res.Status != http.StatusNotFound {
		t.Fatalf("a server with no App answered %d", res.Status)
	}
}

func TestWebhookIgnoresAReplayedDelivery(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	ev := h.prEvent("opened", 7, "feature", "sha-1")
	if res := h.deliver("pull_request", "delivery-1", ev); res.Status != http.StatusAccepted {
		t.Fatalf("first delivery: %+v", res)
	}
	if got := len(h.br.creates); got != 1 {
		t.Fatalf("creates after the first delivery = %d", got)
	}
	res := h.deliver("pull_request", "delivery-1", ev)
	if res.Status != http.StatusOK || !strings.Contains(res.Message, "before") {
		t.Fatalf("replay: %+v", res)
	}
	if got := len(h.br.creates); got != 1 {
		t.Fatalf("a replayed delivery created a branch: %d creates", got)
	}
}

func TestWebhookIsRateLimited(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{}`)
	limited := false
	for i := 0; i < 400; i++ {
		res := h.svc.HandleWebhook(context.Background(), Delivery{Event: "ping", ID: "p", Signature: h.sign(body), Body: body})
		if res.Status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("400 deliveries in a burst were all admitted")
	}
}

func TestPullRequestOpenedCreatesAPreviewBranchBuiltFromTheRepository(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	if res := h.deliver("pull_request", "d1", h.prEvent("opened", 12, "feature/login", "sha-1")); res.Status != http.StatusAccepted {
		t.Fatalf("%+v", res)
	}
	if len(h.br.creates) != 1 {
		t.Fatalf("creates = %d", len(h.br.creates))
	}
	in := h.br.creates[0]
	if in.Name != "feature/login" || in.GitBranch != "feature/login" || in.Persistent || in.WithData {
		t.Fatalf("create input = %+v", in)
	}
	if in.TTL >= 0 {
		t.Fatalf("a pull request branch has a lifetime (TTL %v); it lives until the pull request closes", in.TTL)
	}
	if in.SchemaSource == nil {
		t.Fatal("the branch is not built from the repository")
	}
	b := h.br.byName("feature/login")
	if b == nil || b.State != registry.BranchFunctionsDeployed {
		t.Fatalf("branch = %+v", b)
	}
	if got := h.db.applied(b.Ref); len(got) != 1 || got[0] != "20240101000000" {
		t.Fatalf("migrations applied = %v", got)
	}
	if scripts := h.db.scripts[b.Ref]; len(scripts) != 1 || !strings.Contains(scripts[0], "insert into public.todos") {
		t.Fatalf("seed scripts = %v", scripts)
	}
	if got := h.host.deployed; len(got) != 1 || got[0] != "hello@"+b.Ref {
		t.Fatalf("functions deployed = %v", got)
	}
	if got := h.host.files["hello@"+b.Ref]; !slices.Contains(got, "functions/_shared/cors.ts") || !slices.Contains(got, "functions/hello/index.ts") {
		t.Fatalf("files of hello = %v", got)
	}
	if got := h.host.patches["postgrest@"+b.Ref]["max_rows"]; got != int64(500) {
		t.Fatalf("config.toml max_rows reached the branch as %v", got)
	}
	link, err := h.reg.GetGitHubBranchLink(context.Background(), b.Ref)
	if err != nil || link.PRNumber != 12 || link.HeadRef != "feature/login" || link.HeadSHA != "sha-1" || link.ProjectRef != prodRef {
		t.Fatalf("link = %+v, %v", link, err)
	}
	if n, _ := h.reg.GitHubPRNumbers(context.Background(), []string{b.Ref}); n[b.Ref] != 12 {
		t.Fatalf("pr numbers = %v", n)
	}
}

func TestSynchronizeAppliesOnlyTheMigrationsTheBranchLacks(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 3, "feature", "sha-1"))
	b := h.br.byName("feature")
	if b == nil {
		t.Fatal("no branch")
	}
	h.gh.setCommit("sha-2", repoFiles(map[string]string{"supabase/migrations/20240102000000_done.sql": mig2}))
	h.db.mu.Lock()
	h.db.applyLog = nil
	h.db.mu.Unlock()
	h.deliver("pull_request", "d2", h.prEvent("synchronize", 3, "feature", "sha-2"))
	if len(h.br.creates) != 1 || len(h.br.drives) != 1 {
		t.Fatalf("creates %d, drives %d: a new commit must update the branch, not make another", len(h.br.creates), len(h.br.drives))
	}
	if got := h.db.applyLog; len(got) != 1 || got[0] != b.Ref+"/20240102000000" {
		t.Fatalf("migrations applied by the second commit = %v, want only the new one", got)
	}
	if got := h.db.applied(b.Ref); len(got) != 2 {
		t.Fatalf("history = %v", got)
	}
	// The seed ran once.
	if len(h.db.scripts[b.Ref]) != 1 {
		t.Fatalf("scripts = %d, the seed must run once", len(h.db.scripts[b.Ref]))
	}
	// The unchanged function is not deployed again.
	if len(h.host.deployed) != 1 {
		t.Fatalf("functions deployed = %v, an unchanged function is deployed once", h.host.deployed)
	}
	link, _ := h.reg.GetGitHubBranchLink(context.Background(), b.Ref)
	if link.HeadSHA != "sha-2" {
		t.Fatalf("head sha = %s", link.HeadSHA)
	}
	// A changed function is.
	h.gh.setCommit("sha-3", repoFiles(map[string]string{"supabase/functions/hello/index.ts": "Deno.serve(() => new Response('hello'))"}))
	h.deliver("pull_request", "d3", h.prEvent("synchronize", 3, "feature", "sha-3"))
	if len(h.host.deployed) != 2 {
		t.Fatalf("functions deployed = %v after the function changed", h.host.deployed)
	}
}

func TestClosedDeletesTheBranchAndItsLink(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 5, "fix", "sha-1"))
	b := h.br.byName("fix")
	h.deliver("pull_request", "d2", h.prEvent("closed", 5, "fix", "sha-1"))
	if got := h.br.deleted; len(got) != 1 || got[0] != b.Ref {
		t.Fatalf("deleted = %v", got)
	}
	if _, err := h.reg.GetGitHubBranchLink(context.Background(), b.Ref); err == nil {
		t.Fatal("the link outlived the branch")
	}
	// A pull request that was never built closes quietly.
	h.deliver("pull_request", "d3", h.prEvent("closed", 99, "other", "sha-1"))
	if len(h.br.deleted) != 1 {
		t.Fatalf("deleted = %v", h.br.deleted)
	}
}

func TestClosedKeepsAPersistentBranch(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 5, "staging", "sha-1"))
	b := h.br.byName("staging")
	h.br.mu.Lock()
	h.br.branches[b.Ref].Persistent = true
	h.br.mu.Unlock()
	h.deliver("pull_request", "d2", h.prEvent("closed", 5, "staging", "sha-1"))
	if len(h.br.deleted) != 0 {
		t.Fatalf("a persistent branch was deleted: %v", h.br.deleted)
	}
	link, err := h.reg.GetGitHubBranchLink(context.Background(), b.Ref)
	if err != nil || link.PRNumber != 0 {
		t.Fatalf("the link of a kept branch = %+v, %v; it should stay as a Git branch link", link, err)
	}
}

func TestForkPullRequestsAreIgnoredUnlessAllowed(t *testing.T) {
	fork := func(h *harness) map[string]any {
		ev := h.prEvent("opened", 8, "evil", "sha-1")
		pr := ev["pull_request"].(map[string]any)
		pr["head"].(map[string]any)["repo"] = map[string]any{"id": 555, "full_name": "mallory/app", "fork": true}
		return ev
	}
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	res := h.deliver("pull_request", "d1", fork(h))
	if len(h.br.creates) != 0 || !strings.Contains(res.Message, "fork") {
		t.Fatalf("a fork pull request created a branch (%d creates): %+v", len(h.br.creates), res)
	}
	// A fork whose repository was deleted has no head repo at all.
	ev := h.prEvent("opened", 9, "gone", "sha-1")
	ev["pull_request"].(map[string]any)["head"].(map[string]any)["repo"] = nil
	h.deliver("pull_request", "d2", ev)
	if len(h.br.creates) != 0 {
		t.Fatal("a pull request with no head repository created a branch")
	}

	allowed := newHarness(t, func(c *config.Config) { c.GitHub.AllowForks = true })
	allowed.gh.setCommit("sha-1", repoFiles(nil))
	allowed.deliver("pull_request", "d1", fork(allowed))
	if len(allowed.br.creates) != 1 {
		t.Fatalf("allow_forks: creates = %d", len(allowed.br.creates))
	}
}

func TestBranchLimitIsEnforced(t *testing.T) {
	h := newHarness(t)
	h.conn.BranchLimit = 2
	if _, err := h.svc.Update(context.Background(), h.conn.ID, UpdateInput{BranchLimit: ptr(2)}); err != nil {
		t.Fatal(err)
	}
	h.gh.setCommit("sha-1", repoFiles(nil))
	for i, head := range []string{"a", "b", "c"} {
		h.deliver("pull_request", "d"+head, h.prEvent("opened", i+1, head, "sha-1"))
	}
	if got := h.br.names(); len(got) != 2 {
		t.Fatalf("branches = %v, want 2 (the limit)", got)
	}
	// The pull request over the limit got a neutral check and a comment that says why.
	var neutral bool
	for _, c := range h.gh.checks {
		if c.Conclusion == "neutral" && strings.Contains(c.Summary, "branch limit") {
			neutral = true
		}
	}
	if !neutral {
		t.Fatalf("no neutral check run explains the limit: %+v", h.gh.checks)
	}
}

func TestSupabaseChangesOnlySkipsPullRequestsWithoutSupabaseFiles(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Update(context.Background(), h.conn.ID, UpdateInput{SupabaseChangesOnly: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.gh.prFiles[1] = []string{"README.md", "src/app.ts"}
	h.gh.prFiles[2] = []string{"README.md", "supabase/migrations/20240101000000_init.sql"}
	h.deliver("pull_request", "d1", h.prEvent("opened", 1, "docs", "sha-1"))
	if len(h.br.creates) != 0 {
		t.Fatal("a pull request with no supabase change created a branch")
	}
	h.deliver("pull_request", "d2", h.prEvent("opened", 2, "schema", "sha-1"))
	if len(h.br.creates) != 1 || h.br.creates[0].Name != "schema" {
		t.Fatalf("creates = %+v", h.br.creates)
	}
	// With a working directory the supabase folder is under it.
	if _, err := h.svc.Update(context.Background(), h.conn.ID, UpdateInput{Workdir: ptr("apps/web")}); err != nil {
		t.Fatal(err)
	}
	h.gh.prFiles[3] = []string{"supabase/migrations/x.sql"}
	h.deliver("pull_request", "d3", h.prEvent("opened", 3, "elsewhere", "sha-1"))
	if len(h.br.creates) != 1 {
		t.Fatal("a change outside the working directory counted as a supabase change")
	}
}

func TestAutomaticBranchingOffIgnoresPullRequests(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Update(context.Background(), h.conn.ID, UpdateInput{NewBranchPerPR: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 1, "feature", "sha-1"))
	if len(h.br.creates) != 0 {
		t.Fatal("automatic branching is off and a branch was made")
	}
	if h.gh.callsTo("POST /repos/acme/app/check-runs") != 0 {
		t.Fatal("an ignored pull request got a check run")
	}
}

func TestNoSupabaseDirectoryIsNeutral(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", map[string]string{"README.md": "x"})
	h.deliver("pull_request", "d1", h.prEvent("opened", 1, "feature", "sha-1"))
	if len(h.br.creates) != 0 {
		t.Fatal("a branch was made from a commit with no supabase directory")
	}
	for _, c := range h.gh.checks {
		if c.Conclusion != "neutral" {
			t.Fatalf("check = %+v", c)
		}
		return
	}
	t.Fatal("no check run")
}

func TestAFailedMigrationFailsTheRunAndIsReported(t *testing.T) {
	h := newHarness(t)
	h.db.failAt = "20240101000000"
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 4, "broken", "sha-1"))
	b := h.br.byName("broken")
	if b == nil || b.State == registry.BranchFunctionsDeployed {
		t.Fatalf("branch = %+v", b)
	}
	if !strings.Contains(b.Detail, "20240101000000_init") || !strings.Contains(b.Detail, "syntax error") {
		t.Fatalf("detail = %q", b.Detail)
	}
	var failed bool
	for _, c := range h.gh.checks {
		failed = failed || c.Conclusion == "failure"
	}
	if !failed {
		t.Fatalf("no failing check run: %+v", h.gh.checks)
	}
	// The branch is kept for inspection, and a fixed commit builds on it.
	h.db.failAt = ""
	h.gh.setCommit("sha-2", repoFiles(nil))
	h.deliver("pull_request", "d2", h.prEvent("synchronize", 4, "broken", "sha-2"))
	if got := h.br.byName("broken"); got.State != registry.BranchFunctionsDeployed {
		t.Fatalf("the fixed commit did not deploy: %+v", got)
	}
}

func TestCheckRunAndCommentAreMadeOnceAndUpdated(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.gh.setCommit("sha-2", repoFiles(map[string]string{"supabase/migrations/20240102000000_done.sql": mig2}))
	h.deliver("pull_request", "d1", h.prEvent("opened", 6, "feature", "sha-1"))
	if got := h.gh.callsTo("POST /repos/acme/app/check-runs"); got != 1 {
		t.Fatalf("check runs created = %d", got)
	}
	if got := h.gh.callsTo("POST /repos/acme/app/issues/6/comments"); got != 1 {
		t.Fatalf("comments created = %d", got)
	}
	for _, c := range h.gh.checks {
		if c.Name != "Supabase Preview" || c.Status != "completed" || c.Conclusion != "success" || c.HeadSHA != "sha-1" {
			t.Fatalf("check = %+v", c)
		}
		if !strings.Contains(c.Summary, "| migrate | Success |") {
			t.Fatalf("the summary lacks the step table: %q", c.Summary)
		}
	}
	h.deliver("pull_request", "d2", h.prEvent("synchronize", 6, "feature", "sha-2"))
	// A new commit gets its own check run; the one comment is updated, not repeated.
	if got := h.gh.callsTo("POST /repos/acme/app/check-runs"); got != 2 {
		t.Fatalf("check runs created = %d, want one per commit", got)
	}
	if got := h.gh.callsTo("POST /repos/acme/app/issues/6/comments"); got != 1 {
		t.Fatalf("comments created = %d, want the first one updated", got)
	}
	if got := h.gh.callsTo("PATCH /repos/acme/app/issues/comments/"); got != 1 {
		t.Fatalf("comment updates = %d", got)
	}
	for _, c := range h.gh.comments {
		if c.PR != 6 || !strings.HasPrefix(c.Body, commentMarker) || c.Updates != 1 || !strings.Contains(c.Body, "feature") {
			t.Fatalf("comment = %+v", c)
		}
	}
	// A comment someone deleted is made again.
	h.gh.mu.Lock()
	h.gh.comments = map[int64]*fakeComment{}
	h.gh.mu.Unlock()
	h.gh.setCommit("sha-3", repoFiles(nil))
	h.deliver("pull_request", "d3", h.prEvent("synchronize", 6, "feature", "sha-3"))
	if got := h.gh.callsTo("POST /repos/acme/app/issues/6/comments"); got != 2 {
		t.Fatalf("comments created = %d after a deletion", got)
	}
}

func TestPushToTheProductionBranchDeploysToProduction(t *testing.T) {
	h := newHarness(t)
	if err := h.reg.CreateProject(context.Background(), &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	h.gh.setCommit("sha-main", repoFiles(nil))
	// Deploy to production is off until the project has a production branch.
	if res := h.deliver("push", "d1", h.pushEvent("refs/heads/main", "sha-main")); res.Status != http.StatusAccepted {
		t.Fatalf("%+v", res)
	}
	if len(h.db.history[prodRef]) != 0 {
		t.Fatal("production was deployed with Deploy to production off")
	}
	if err := h.reg.SetDefaultBranchGit(context.Background(), prodRef, "main"); err != nil {
		t.Fatal(err)
	}
	h.deliver("push", "d2", h.pushEvent("refs/heads/main", "sha-main"))
	if got := h.db.applied(prodRef); len(got) != 1 {
		t.Fatalf("production migrations = %v", got)
	}
	if len(h.db.scripts[prodRef]) != 0 {
		t.Fatalf("the seed ran on production: %v", h.db.scripts[prodRef])
	}
	if got := h.host.deployed; len(got) != 1 || got[0] != "hello@"+prodRef {
		t.Fatalf("functions deployed = %v", got)
	}
	// config.toml reaches production only through a [remotes] block for its ref.
	if len(h.host.patches) != 0 {
		t.Fatalf("settings of config.toml reached production without a remote: %v", h.host.patches)
	}
	var prodCheck bool
	for _, c := range h.gh.checks {
		prodCheck = prodCheck || (c.Name == CheckNameProduction && c.Conclusion == "success")
	}
	if !prodCheck {
		t.Fatalf("checks = %+v", h.gh.checks)
	}
	// The same commit again changes nothing: the function is unchanged.
	h.deliver("push", "d3", h.pushEvent("refs/heads/main", "sha-main"))
	if len(h.host.deployed) != 1 {
		t.Fatalf("functions deployed = %v, an unchanged function is deployed once", h.host.deployed)
	}
}

func TestPushWithoutSupabaseFilesSkipsProductionWhenSupabaseChangesOnly(t *testing.T) {
	h := newHarness(t)
	_ = h.reg.CreateProject(context.Background(), &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusActiveHealthy})
	_ = h.reg.SetDefaultBranchGit(context.Background(), prodRef, "main")
	if _, err := h.svc.Update(context.Background(), h.conn.ID, UpdateInput{SupabaseChangesOnly: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	h.gh.setCommit("sha-main", repoFiles(nil))
	h.deliver("push", "d1", h.pushEvent("refs/heads/main", "sha-main", "README.md"))
	if len(h.db.history[prodRef]) != 0 {
		t.Fatal("production was deployed by a push that changed nothing under supabase/")
	}
	h.deliver("push", "d2", h.pushEvent("refs/heads/main", "sha-main", "supabase/migrations/20240101000000_init.sql"))
	if len(h.db.history[prodRef]) != 1 {
		t.Fatal("production was not deployed by a push that changed supabase/")
	}
}

func TestPushToAGitBranchDeploysToTheBranchThatFollowsIt(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	// A branch somebody made by hand for the Git branch "staging" (persistent).
	h.deliver("pull_request", "d0", h.prEvent("opened", 1, "staging", "sha-1"))
	b := h.br.byName("staging")
	h.br.mu.Lock()
	h.br.branches[b.Ref].Persistent = true
	h.br.mu.Unlock()
	h.deliver("pull_request", "d0b", h.prEvent("closed", 1, "staging", "sha-1"))
	h.gh.setCommit("sha-2", repoFiles(map[string]string{"supabase/migrations/20240102000000_done.sql": mig2}))
	h.deliver("push", "d1", h.pushEvent("refs/heads/staging", "sha-2"))
	if got := h.db.applied(b.Ref); len(got) != 2 {
		t.Fatalf("migrations on the branch = %v", got)
	}
	// A push to a branch nothing follows does nothing.
	before := len(h.br.drives)
	h.deliver("push", "d2", h.pushEvent("refs/heads/other", "sha-2"))
	if len(h.br.drives) != before {
		t.Fatal("a push to an unrelated Git branch drove a branch")
	}
}

func TestDeletedGitBranchDeletesItsPreviewBranch(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Update(context.Background(), h.conn.ID, UpdateInput{NewBranchPerPR: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	// A branch made by hand with git_branch.
	b, _ := h.br.Create(context.Background(), prodRef, branching.CreateInput{Name: "handmade", GitBranch: "feature-x"})
	repo := map[string]any{"id": h.gh.repoID, "full_name": h.gh.repo}
	h.deliver("delete", "d1", map[string]any{"ref": "feature-x", "ref_type": "branch", "repository": repo, "installation": map[string]any{"id": h.gh.instID}})
	if got := h.br.deleted; len(got) != 1 || got[0] != b.Ref {
		t.Fatalf("deleted = %v", got)
	}
	// A deleted tag is not a branch.
	h.deliver("delete", "d2", map[string]any{"ref": "v1", "ref_type": "tag", "repository": repo, "installation": map[string]any{"id": h.gh.instID}})
	if len(h.br.deleted) != 1 {
		t.Fatal("a deleted tag deleted a branch")
	}
}

func TestEventsOfAnotherInstallationAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	ev := h.prEvent("opened", 1, "feature", "sha-1")
	ev["installation"] = map[string]any{"id": 999}
	h.deliver("pull_request", "d1", ev)
	if len(h.br.creates) != 0 {
		t.Fatal("an event of another installation built a branch")
	}
}

func TestBranchNames(t *testing.T) {
	for in, want := range map[string]string{
		"feature":        "feature",
		"feature/login":  "feature/login",
		"fix: the thing": "fix-the-thing",
		"release..1":     "release.1",
		"-weird":         "weird",
		"main":           "pr-9",
		"":               "pr-9",
		"///":            "pr-9",
		"ünïcode":        "n-code",
		"trailing/":      "trailing",
		"dots...":        "dots",
	} {
		if got := branchName(in, 9); got != want {
			t.Errorf("branchName(%q) = %q, want %q", in, got, want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

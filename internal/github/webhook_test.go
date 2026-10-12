package github

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestAFloodOfForgeriesDoesNotUseUpTheAllowanceOfRealDeliveries(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{}`)
	refused := 0
	for i := 0; i < 600; i++ {
		res := h.svc.HandleWebhook(context.Background(), Delivery{Event: "ping", ID: "f", Signature: "sha256=00", Body: body})
		if res.Status == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("600 forgeries in a burst were all answered")
	}
	if res := h.svc.HandleWebhook(context.Background(), Delivery{Event: "ping", ID: "real", Signature: h.sign(body), Body: body}); res.Status != http.StatusOK {
		t.Fatalf("a real delivery after the flood: %+v", res)
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
	got := h.br.byName("broken")
	if got.State != registry.BranchFunctionsDeployed {
		t.Fatalf("the fixed commit did not deploy: %+v", got)
	}
	// The first run never reached the seed step, so the fixed commit seeds the branch, once.
	if n := len(h.db.scripts[got.Ref]); n != 1 {
		t.Fatalf("seed scripts after the fixed commit = %d, want 1", n)
	}
	link, err := h.reg.GetGitHubBranchLink(context.Background(), got.Ref)
	if err != nil || link.SeededAt == nil {
		t.Fatalf("link after the seed = %+v, %v", link, err)
	}
	h.gh.setCommit("sha-3", repoFiles(nil))
	h.deliver("pull_request", "d3", h.prEvent("synchronize", 4, "broken", "sha-3"))
	if n := len(h.db.scripts[got.Ref]); n != 1 {
		t.Fatalf("seed scripts after a third commit = %d, want the seed not to repeat", n)
	}
}

func TestAFailedSeedIsTriedAgainByTheNextCommitAndAnAbsentOneIsNot(t *testing.T) {
	h := newHarness(t)
	h.db.failSeed = true
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 5, "seedless", "sha-1"))
	b := h.br.byName("seedless")
	if b == nil || b.State == registry.BranchFunctionsDeployed {
		t.Fatalf("a failing seed did not fail the run: %+v", b)
	}
	h.db.failSeed = false
	h.gh.setCommit("sha-2", repoFiles(nil))
	h.deliver("pull_request", "d2", h.prEvent("synchronize", 5, "seedless", "sha-2"))
	if n := len(h.db.scripts[b.Ref]); n != 1 {
		t.Fatalf("seed scripts after the retry = %d, want 1", n)
	}

	// A first run that found no seed file used its chance: a seed added later is not applied to a branch with data.
	h.gh.setCommit("sha-3", repoFiles(map[string]string{"supabase/seed.sql": ""}))
	h.deliver("pull_request", "d3", h.prEvent("opened", 6, "noseed", "sha-3"))
	nb := h.br.byName("noseed")
	h.gh.setCommit("sha-4", repoFiles(nil))
	h.deliver("pull_request", "d4", h.prEvent("synchronize", 6, "noseed", "sha-4"))
	if n := len(h.db.scripts[nb.Ref]); n != 0 {
		t.Fatalf("a seed added after the first run was applied: %d script(s)", n)
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
		if c.PR != 6 || !strings.HasPrefix(c.Body, commentMarkerPrefix+prodRef+" -->") || c.Updates != 1 || !strings.Contains(c.Body, "feature") {
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
	h.gh.setHead("main", "sha-main")
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
	// (A delivery's body is remembered, so the second push of the commit differs in what GitHub sends
	// with it, as a force-push of the same commit does.)
	again := func(before string) map[string]any {
		ev := h.pushEvent("refs/heads/main", "sha-main")
		ev["before"] = before
		return ev
	}
	h.deliver("push", "d2", again("sha-before-1"))
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
	h.deliver("push", "d3", again("sha-before-2"))
	if len(h.host.deployed) != 1 {
		t.Fatalf("functions deployed = %v, an unchanged function is deployed once", h.host.deployed)
	}
}

// prodHarness is a harness whose project is running and deploys the Git branch main.
func prodHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.reg.CreateProject(ctx, &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetDefaultBranchGit(ctx, prodRef, "main"); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) prodChecks() (n int, headSHAs []string) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	for _, c := range h.gh.checks {
		if c.Name == CheckNameProduction {
			n++
			headSHAs = append(headSHAs, c.HeadSHA)
		}
	}
	return n, headSHAs
}

// Events do not arrive in order, and a delivery that failed is sent again by hand long after: a push
// of a commit that is no longer the head of the branch must not put its older code on production.
func TestAnOlderPushIsNotDeployedOverANewerOne(t *testing.T) {
	h := prodHarness(t)
	fnA := map[string]string{"supabase/functions/hello/index.ts": "Deno.serve(() => new Response('vulnerable'))"}
	fnB := map[string]string{"supabase/functions/hello/index.ts": "Deno.serve(() => new Response('fixed'))"}
	h.gh.setCommit("sha-A", repoFiles(fnA))
	h.gh.setCommit("sha-B", repoFiles(fnB))
	h.gh.setHead("main", "sha-B")
	// B's delivery is handled first.
	if res := h.deliver("push", "dB", h.pushEvent("refs/heads/main", "sha-B")); res.Status != http.StatusAccepted {
		t.Fatalf("%+v", res)
	}
	if n, shas := h.prodChecks(); n != 1 || shas[0] != "sha-B" {
		t.Fatalf("production checks after B = %d %v", n, shas)
	}
	conn, err := h.reg.GetGitHubConnectionByProject(context.Background(), prodRef)
	if err != nil {
		t.Fatal(err)
	}
	hashB := conn.ProdFunctions["hello"]
	deployed := len(h.host.deployed)
	if hashB == "" || deployed != 1 {
		t.Fatalf("after B: hash %q, %d function deployment(s)", hashB, deployed)
	}
	// A arrives late (or is sent again after an outage): it is accepted and does nothing.
	if res := h.deliver("push", "dA", h.pushEvent("refs/heads/main", "sha-A")); res.Status != http.StatusAccepted {
		t.Fatalf("%+v", res)
	}
	if n, _ := h.prodChecks(); n != 1 {
		t.Fatalf("a stale push opened a production check run (%d)", n)
	}
	if len(h.host.deployed) != deployed {
		t.Fatalf("the older function code was deployed: %v", h.host.deployed)
	}
	conn, _ = h.reg.GetGitHubConnectionByProject(context.Background(), prodRef)
	if conn.ProdFunctions["hello"] != hashB {
		t.Fatalf("the recorded function hash went back to the older code's")
	}
}

func TestPushesInTheWrongOrderEndOnTheHead(t *testing.T) {
	h := prodHarness(t)
	for _, sha := range []string{"sha-A", "sha-B"} {
		h.gh.setCommit(sha, repoFiles(map[string]string{"supabase/functions/hello/index.ts": "Deno.serve(() => new Response('" + sha + "'))"}))
	}
	h.gh.setHead("main", "sha-B")
	h.deliver("push", "dA", h.pushEvent("refs/heads/main", "sha-A")) // arrives first, is already stale
	h.deliver("push", "dB", h.pushEvent("refs/heads/main", "sha-B"))
	if n, shas := h.prodChecks(); n != 1 || shas[0] != "sha-B" {
		t.Fatalf("production checks = %d %v, want the head only", n, shas)
	}
	if got := h.host.deployed; len(got) != 1 {
		t.Fatalf("functions deployed = %v", got)
	}
}

// GitHub's API can show the previous head for a moment after the event was sent; the check asks
// again before it calls a push stale.
func TestAPushWhoseHeadTheAPIShowsLateIsStillDeployed(t *testing.T) {
	h := prodHarness(t)
	headRecheck = 5 * time.Millisecond
	h.gh.setCommit("sha-old", repoFiles(nil))
	h.gh.setCommit("sha-new", repoFiles(map[string]string{"supabase/migrations/20240102000000_done.sql": mig2}))
	h.gh.setHead("main", "sha-new")
	h.gh.mu.Lock()
	h.gh.headQueue["main"] = []string{"sha-old"}
	h.gh.mu.Unlock()
	h.deliver("push", "d1", h.pushEvent("refs/heads/main", "sha-new"))
	if got := h.db.applied(prodRef); len(got) != 2 {
		t.Fatalf("production migrations = %v", got)
	}
	if n, shas := h.prodChecks(); n != 1 || shas[0] != "sha-new" {
		t.Fatalf("production checks = %d %v", n, shas)
	}
}

func TestAPushToABranchThatIsGoneIsNotDeployed(t *testing.T) {
	h := prodHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil)) // no head: "main" does not exist any more
	h.deliver("push", "d1", h.pushEvent("refs/heads/main", "sha-1"))
	if n, _ := h.prodChecks(); n != 0 || len(h.db.history[prodRef]) != 0 {
		t.Fatalf("a push to a deleted branch deployed (%d checks)", n)
	}
}

func TestAnOlderPushToAGitBranchIsNotDeployedToTheBranchThatFollowsIt(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d0", h.prEvent("opened", 1, "staging", "sha-1"))
	b := h.br.byName("staging")
	h.br.mu.Lock()
	h.br.branches[b.Ref].Persistent = true
	h.br.mu.Unlock()
	h.deliver("pull_request", "d0b", h.prEvent("closed", 1, "staging", "sha-1"))
	h.gh.setCommit("sha-2", repoFiles(map[string]string{"supabase/migrations/20240102000000_done.sql": mig2}))
	h.gh.setCommit("sha-3", repoFiles(map[string]string{"supabase/migrations/20240102000000_done.sql": mig2, "supabase/migrations/20240103000000_more.sql": mig2}))
	h.gh.setHead("staging", "sha-3")
	before := len(h.br.drives)
	h.deliver("push", "d1", h.pushEvent("refs/heads/staging", "sha-2"))
	if len(h.br.drives) != before {
		t.Fatal("a push of a commit that is not the head of its Git branch drove the branch")
	}
	h.deliver("push", "d2", h.pushEvent("refs/heads/staging", "sha-3"))
	if len(h.br.drives) != before+1 {
		t.Fatalf("drives = %d, want the head's push to drive once", len(h.br.drives)-before)
	}
	// A push that no branch follows costs GitHub no request at all.
	calls := h.gh.callsTo("GET /repos/acme/app/commits/")
	h.deliver("push", "d3", h.pushEvent("refs/heads/unfollowed", "sha-3"))
	if got := h.gh.callsTo("GET /repos/acme/app/commits/"); got != calls {
		t.Fatalf("a push nothing follows read the head from GitHub (%d request(s))", got-calls)
	}
}

// The signature covers the body and not X-GitHub-Delivery: a body that was captured once and is sent
// again under a new id is the same event.
func TestAReplayedBodyUnderANewDeliveryIdIsNotHandledAgain(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.gh.setCommit("sha-2", repoFiles(nil))
	ev := h.prEvent("opened", 7, "feature", "sha-1")
	if res := h.deliver("pull_request", "delivery-1", ev); res.Status != http.StatusAccepted {
		t.Fatalf("first delivery: %+v", res)
	}
	res := h.deliver("pull_request", "delivery-2", ev) // the same signed body, a new id
	if res.Status != http.StatusOK || !strings.Contains(res.Message, "body") {
		t.Fatalf("a replayed body: %+v", res)
	}
	if h.gh.callsTo("POST /repos/acme/app/check-runs") != 1 || len(h.br.creates) != 1 {
		t.Fatalf("the replay was handled: %d check run(s), %d create(s)", h.gh.callsTo("POST /repos/acme/app/check-runs"), len(h.br.creates))
	}
	// Another body is another event, and a delivery that could not be handled is forgotten with its body.
	if res := h.deliver("pull_request", "delivery-3", h.prEvent("synchronize", 7, "feature", "sha-2")); res.Status != http.StatusAccepted {
		t.Fatalf("a new body: %+v", res)
	}
	bad := h.prEvent("opened", 8, "feature-8", "")
	if res := h.deliver("pull_request", "delivery-4", bad); res.Status != http.StatusBadRequest {
		t.Fatalf("an unreadable payload: %+v", res)
	}
	if res := h.deliver("pull_request", "delivery-4", bad); res.Status != http.StatusBadRequest {
		t.Fatalf("an unreadable payload is sent again: %+v", res)
	}
}

// A replay of a captured body has no three-day limit like GitHub's own redelivery: the body is
// remembered for a week.
func TestADeliveryBodyIsRememberedBeyondGitHubsRedeliveryWindow(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	clock := time.Now() // the App's JWTs are checked against the real clock by the fake
	h.svc.now = func() time.Time { return clock }
	ev := h.prEvent("opened", 7, "feature", "sha-1")
	h.deliver("pull_request", "d1", ev)
	drives := len(h.br.drives)
	clock = clock.Add(73 * time.Hour) // past the 72 hours of GitHub's redelivery
	if res := h.deliver("pull_request", "d2", ev); res.Status != http.StatusOK || !strings.Contains(res.Message, "before") {
		t.Fatalf("a replay after three days: %+v", res)
	}
	clock = clock.Add(8 * 24 * time.Hour)
	if res := h.deliver("pull_request", "d3", ev); res.Status != http.StatusAccepted {
		t.Fatalf("after a week the body is forgotten: %+v", res)
	}
	if len(h.br.drives) != drives+1 {
		t.Fatalf("drives = %d, want the forgotten body handled once more", len(h.br.drives)-drives)
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
	h.gh.setHead("main", "sha-main")
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
	h.gh.setHead("staging", "sha-2")
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

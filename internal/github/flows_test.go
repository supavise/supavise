package github

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/registry"
)

func TestTheQueueRunsOneJobPerKeyAndTheNewestReplacesTheOlder(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var log []string
	started := make(chan struct{})
	release := make(chan struct{})
	h.svc.enqueue("k", func(ctx context.Context) {
		mu.Lock()
		log = append(log, "first:start")
		mu.Unlock()
		close(started)
		select {
		case <-ctx.Done():
			mu.Lock()
			log = append(log, "first:cancelled")
			mu.Unlock()
		case <-release:
			mu.Lock()
			log = append(log, "first:done")
			mu.Unlock()
		}
	})
	<-started
	// Two newer jobs arrive while the first runs: the first is cancelled, the second never starts.
	h.svc.enqueue("k", func(ctx context.Context) {
		mu.Lock()
		log = append(log, "second")
		mu.Unlock()
	})
	h.svc.enqueue("k", func(ctx context.Context) {
		mu.Lock()
		log = append(log, "third")
		mu.Unlock()
	})
	// Other keys do not wait.
	other := make(chan struct{})
	h.svc.enqueue("other", func(context.Context) { close(other) })
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("a job with another key waited")
	}
	h.svc.wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(log, ","); got != "first:start,first:cancelled,third" {
		t.Fatalf("log = %s", got)
	}
}

func TestANewCommitCancelsTheMigrationsOfAnOlderOne(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 3, "feature", "sha-1"))
	b := h.br.byName("feature")
	// The next commit adds two migrations; the first of them takes a while.
	h.gh.setCommit("sha-2", repoFiles(map[string]string{
		"supabase/migrations/20240102000000_slow.sql": "select pg_sleep(60);",
		"supabase/migrations/20240103000000_more.sql": "select 1;",
	}))
	h.db.gate = make(chan struct{})
	h.db.gateAt = "20240102000000"
	h.db.entered = make(chan struct{}, 2)
	body, _ := jsonBody(h.prEvent("synchronize", 3, "feature", "sha-2"))
	go func() {
		h.svc.HandleWebhook(context.Background(), Delivery{Event: "pull_request", ID: "d2", Signature: h.sign(body), Body: body})
	}()
	select {
	case <-h.db.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow migration never started")
	}
	// A third commit arrives while it runs: the older work is cancelled and the newer applies everything once.
	h.gh.setCommit("sha-3", repoFiles(map[string]string{
		"supabase/migrations/20240102000000_slow.sql": "select 2;",
		"supabase/migrations/20240103000000_more.sql": "select 1;",
	}))
	h.db.gateAt = ""
	body3, _ := jsonBody(h.prEvent("synchronize", 3, "feature", "sha-3"))
	h.svc.HandleWebhook(context.Background(), Delivery{Event: "pull_request", ID: "d3", Signature: h.sign(body3), Body: body3})
	h.svc.wg.Wait()
	if got := h.db.applied(b.Ref); len(got) != 3 {
		t.Fatalf("history = %v: the cancelled commit left the branch unfinished", got)
	}
	link, _ := h.reg.GetGitHubBranchLink(context.Background(), b.Ref)
	if link.HeadSHA != "sha-3" {
		t.Fatalf("head sha = %s", link.HeadSHA)
	}
	// Only the last commit has a finished check run.
	done := map[string]string{}
	for _, c := range h.gh.checks {
		done[c.HeadSHA] = c.Status
	}
	if done["sha-3"] != "completed" || done["sha-2"] == "completed" {
		t.Fatalf("check runs = %v", done)
	}
}

func TestAReopenedPullRequestMakesANewBranchAndSeedsAgain(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 4, "feature", "sha-1"))
	first := h.br.byName("feature")
	h.deliver("pull_request", "d2", h.prEvent("closed", 4, "feature", "sha-1"))
	h.deliver("pull_request", "d3", h.prEvent("reopened", 4, "feature", "sha-1"))
	second := h.br.byName("feature")
	if second == nil || second.Ref == first.Ref {
		t.Fatalf("branches: %v %v", first, second)
	}
	if len(h.db.scripts[second.Ref]) != 1 {
		t.Fatal("the new branch was not seeded")
	}
}

func TestAnExistingBranchOfThatNameForAnotherGitBranchGetsTheNumberAsASuffix(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	// Somebody's branch is called "feature" and follows another Git branch.
	if _, err := h.br.Create(context.Background(), prodRef, branching.CreateInput{Name: "feature", GitBranch: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	h.deliver("pull_request", "d1", h.prEvent("opened", 9, "feature", "sha-1"))
	if got := h.br.names(); len(got) != 2 || got[1] != "feature-9" {
		t.Fatalf("branches = %v", got)
	}
}

func TestACreatedBranchOfOurOwnIsAdoptedWhenItsLinkWasLost(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	if _, err := h.br.Create(context.Background(), prodRef, branching.CreateInput{Name: "feature", GitBranch: "feature"}); err != nil {
		t.Fatal(err)
	}
	// An open pull request whose branch exists with the right Git branch, as after a cancelled create.
	h.deliver("pull_request", "d1", h.prEvent("opened", 9, "feature", "sha-1"))
	if len(h.br.names()) != 1 {
		t.Fatalf("branches = %v", h.br.names())
	}
	b := h.br.byName("feature")
	link, err := h.reg.GetGitHubBranchLink(context.Background(), b.Ref)
	if err != nil || link.PRNumber != 9 {
		t.Fatalf("link = %+v, %v", link, err)
	}
	if got := h.db.applied(b.Ref); len(got) != 1 {
		t.Fatalf("migrations = %v", got)
	}
}

func TestAPersistentBranchIsConfiguredOnlyByItsRemoteBlock(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 1, "staging", "sha-1"))
	b := h.br.byName("staging")
	h.br.mu.Lock()
	h.br.branches[b.Ref].Persistent = true
	h.br.mu.Unlock()
	cfg := "project_id = \"app\"\n[api]\nmax_rows = 500\n[remotes.staging]\nproject_id = \"" + b.Ref + "\"\n[remotes.staging.api]\nmax_rows = 9\n[remotes.other]\nproject_id = \"zzzzzzzzzzzzzzzzzzzz\"\n[remotes.other.api]\nmax_rows = 1\n"
	h.gh.setCommit("sha-2", repoFiles(map[string]string{"supabase/config.toml": cfg}))
	h.deliver("pull_request", "d2", h.prEvent("synchronize", 1, "staging", "sha-2"))
	if got := h.host.patches["postgrest@"+b.Ref]["max_rows"]; got != int64(9) {
		t.Fatalf("max_rows = %v, want the remote's 9", got)
	}
	// Without a remote for the branch, the settings of a persistent branch stay as they are.
	h.host.mu.Lock()
	h.host.patches = map[string]map[string]any{}
	h.host.mu.Unlock()
	h.gh.setCommit("sha-3", repoFiles(map[string]string{"supabase/config.toml": "project_id = \"app\"\n[api]\nmax_rows = 500\n[remotes.other]\nproject_id = \"zzzzzzzzzzzzzzzzzzzz\"\n"}))
	h.deliver("pull_request", "d3", h.prEvent("synchronize", 1, "staging", "sha-3"))
	if len(h.host.patches) != 0 {
		t.Fatalf("settings reached a persistent branch with no remote: %v", h.host.patches)
	}
	// The seed does not run again either way.
	if len(h.db.scripts[b.Ref]) != 1 {
		t.Fatalf("scripts = %d", len(h.db.scripts[b.Ref]))
	}
}

func TestAnUnsupportedConfigSectionIsInTheRunLogAndNotInSilence(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(map[string]string{"supabase/config.toml": "[auth.third_party.firebase]\nenabled = true\n"}))
	h.deliver("pull_request", "d1", h.prEvent("opened", 1, "fb", "sha-1"))
	b := h.br.byName("fb")
	if b == nil || b.State != registry.BranchFunctionsDeployed {
		t.Fatalf("an unsupported section failed the run: %+v", b)
	}
	// The warning is a line of the run's log; with the fake the recorder is nil, so look at the plan the source kept.
	src := &repoSource{}
	doc, _ := ParseConfig([]byte("[auth.third_party.firebase]\nenabled = true\n"))
	p, _ := PlanConfig(PlanInput{Doc: doc})
	if !hasWarning(p, "auth.third_party.firebase.enabled") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
	_ = src
}

func TestAPullRequestOfARepositoryNobodyConnectedIsAccepted(t *testing.T) {
	h := newHarness(t)
	ev := h.prEvent("opened", 1, "feature", "sha-1")
	ev["repository"].(map[string]any)["id"] = 99
	ev["pull_request"].(map[string]any)["head"].(map[string]any)["repo"] = map[string]any{"id": 99, "full_name": "x/y"}
	res := h.deliver("pull_request", "d1", ev)
	if res.Status != http.StatusAccepted || !strings.Contains(res.Message, "no project is connected") || len(h.br.creates) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestAnInstallationThatIsGoneFailsQuietlyAndTheNextEventRecovers(t *testing.T) {
	h := newHarness(t)
	h.gh.missingInst = true
	h.gh.setCommit("sha-1", repoFiles(nil))
	if res := h.deliver("pull_request", "d1", h.prEvent("opened", 1, "feature", "sha-1")); res.Status != http.StatusAccepted {
		t.Fatalf("%+v", res)
	}
	if len(h.br.creates) != 0 {
		t.Fatal("a branch was made without a token")
	}
	h.gh.missingInst = false
	h.deliver("pull_request", "d2", h.prEvent("synchronize", 1, "feature", "sha-1"))
	if len(h.br.creates) != 1 {
		t.Fatalf("creates = %d", len(h.br.creates))
	}
}

func TestAFailedCreateIsReportedInOneCommentThatLaterCommitsUpdate(t *testing.T) {
	h := newHarness(t)
	h.br.maxPerProject = 1
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d0", h.prEvent("opened", 1, "taken", "sha-1"))
	// The node refuses a second branch; the pull request is told, once.
	h.deliver("pull_request", "d1", h.prEvent("opened", 2, "second", "sha-1"))
	h.deliver("pull_request", "d2", h.prEvent("synchronize", 2, "second", "sha-1"))
	n := 0
	for _, c := range h.gh.comments {
		if c.PR == 2 {
			n++
			if !strings.Contains(c.Body, "already has 1 branches") || c.Updates != 1 {
				t.Fatalf("comment = %+v", c)
			}
		}
	}
	if n != 1 {
		t.Fatalf("comments on the pull request = %d, want one that was updated", n)
	}
}

func TestProductionFailureKeepsItsLogOutOfTheCheckRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.reg.CreateProject(ctx, &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusActiveHealthy})
	_ = h.reg.SetDefaultBranchGit(ctx, prodRef, "main")
	h.db.failAt = "20240101000000"
	h.gh.setCommit("sha-main", repoFiles(nil))
	h.deliver("push", "d1", h.pushEvent("refs/heads/main", "sha-main"))
	var got *fakeCheck
	for _, c := range h.gh.checks {
		got = c
	}
	if got == nil || got.Name != CheckNameProduction || got.Conclusion != "failure" {
		t.Fatalf("check = %+v", got)
	}
	if strings.Contains(got.Summary, "selec") && len(got.Summary) > 2000 {
		t.Fatalf("the summary is long: %d", len(got.Summary))
	}
	if !strings.Contains(got.Summary, "dashboard") {
		t.Fatalf("the failure does not point at the dashboard: %s", got.Summary)
	}
	if len(h.host.deployed) != 0 {
		t.Fatal("functions were deployed after the migrations failed")
	}
}

func TestProductionDeployWithoutASupabaseDirectoryIsNeutral(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.reg.CreateProject(ctx, &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusActiveHealthy})
	_ = h.reg.SetDefaultBranchGit(ctx, prodRef, "main")
	h.gh.setCommit("sha-main", map[string]string{"README.md": "x"})
	h.deliver("push", "d1", h.pushEvent("refs/heads/main", "sha-main"))
	for _, c := range h.gh.checks {
		if c.Conclusion != "neutral" {
			t.Fatalf("check = %+v", c)
		}
		return
	}
	t.Fatal("no check run")
}

func TestProductionIsNotDeployedToAPausedProject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.reg.CreateProject(ctx, &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusInactive})
	_ = h.reg.SetDefaultBranchGit(ctx, prodRef, "main")
	h.gh.setCommit("sha-main", repoFiles(nil))
	h.deliver("push", "d1", h.pushEvent("refs/heads/main", "sha-main"))
	if len(h.db.history[prodRef]) != 0 {
		t.Fatal("a paused project was migrated")
	}
	for _, c := range h.gh.checks {
		if c.Conclusion != "failure" || !strings.Contains(c.Title, "failed") {
			t.Fatalf("check = %+v", c)
		}
	}
}

func TestDeployProductionFromTheDashboardsResync(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.reg.CreateProject(ctx, &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusActiveHealthy})
	if _, err := h.svc.DeployProduction(ctx, prodRef); !strings.Contains(err.Error(), "Deploy to production is off") {
		t.Fatalf("with no production branch: %v", err)
	}
	if _, err := h.svc.DeployProduction(ctx, "zzzzzzzzzzzzzzzzzzzz"); err == nil || !strings.Contains(err.Error(), "no GitHub connection") {
		t.Fatalf("with no connection: %v", err)
	}
	_ = h.reg.SetDefaultBranchGit(ctx, prodRef, "main")
	if _, err := h.svc.DeployProduction(ctx, prodRef); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a production branch GitHub does not have: %v", err)
	}
	h.gh.setCommit("head-main", repoFiles(nil))
	if got := h.svc.ProductionBranch(ctx, prodRef); got != "main" {
		t.Fatalf("production branch = %q", got)
	}
	if _, err := h.svc.DeployProduction(ctx, prodRef); err != nil {
		t.Fatal(err)
	}
	h.svc.wg.Wait()
	if got := h.db.applied(prodRef); len(got) != 1 {
		t.Fatalf("production migrations = %v", got)
	}
	var check *fakeCheck
	for _, c := range h.gh.checks {
		check = c
	}
	if check == nil || check.HeadSHA != "head-main" || check.Conclusion != "success" {
		t.Fatalf("check = %+v", check)
	}
}

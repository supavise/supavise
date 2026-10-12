package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/config"
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
	}, func() {
		mu.Lock()
		log = append(log, "second:dropped")
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
	if got := strings.Join(log, ","); got != "first:start,first:cancelled,second:dropped,third" {
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
	h.gh.setHead("main", "sha-main")
	h.deliver("push", "d1", h.pushEvent("refs/heads/main", "sha-main"))
	var got *fakeCheck
	for _, c := range h.gh.checks {
		got = c
	}
	if got == nil || got.Name != CheckNameProduction || got.Conclusion != "failure" {
		t.Fatalf("check = %+v", got)
	}
	if strings.Contains(got.Summary, "selec") || len(got.Summary) > 2000 {
		t.Fatalf("the summary quotes the error or is long (%d): %s", len(got.Summary), got.Summary)
	}
	if !strings.Contains(got.Summary, "dashboard") {
		t.Fatalf("the failure does not point at the dashboard: %s", got.Summary)
	}
	if got.Text != "" {
		t.Fatalf("the check run of production carries a log: %q", got.Text)
	}
	if len(h.host.deployed) != 0 {
		t.Fatal("functions were deployed after the migrations failed")
	}
}

// The check run and the comment are read by everyone who can read the repository. A database error
// can quote a value of a row, so a branch that may hold data (a copy of production's, or a persistent
// one) and production itself report that they failed and where to look, and nothing the error says.
func TestAFailureOfABranchThatMayHoldDataIsNotQuotedOnGitHub(t *testing.T) {
	const quoted = `ERROR: invalid input syntax for type integer: "jane@example.com" (SQLSTATE 22P02)`
	for name, in := range map[string]branching.CreateInput{
		"with data":  {Name: "staging", GitBranch: "staging", WithData: true},
		"persistent": {Name: "staging", GitBranch: "staging", Persistent: true},
	} {
		for _, via := range []string{"a pull request", "a push"} {
			name, in, via := name, in, via
			t.Run(name+" through "+via, func(t *testing.T) { testQuotedFailure(t, in, via == "a push", quoted) })
		}
	}
}

func testQuotedFailure(t *testing.T, in branching.CreateInput, viaPush bool, quoted string) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.br.Create(ctx, prodRef, in); err != nil {
		t.Fatal(err)
	}
	h.db.failAt, h.db.failMsg = "20240101000000", quoted
	h.gh.setCommit("sha-1", repoFiles(nil))
	if viaPush {
		// A push to the Git branch the branch follows (no pull request owns it).
		h.gh.setHead("staging", "sha-1")
		h.deliver("push", "d2", h.pushEvent("refs/heads/staging", "sha-1"))
	} else {
		h.deliver("pull_request", "d1", h.prEvent("opened", 5, "staging", "sha-1"))
	}
	if len(h.gh.checks) == 0 || (!viaPush && len(h.gh.comments) == 0) {
		t.Fatalf("no report was made: %d check run(s), %d comment(s)", len(h.gh.checks), len(h.gh.comments))
	}
	everything := func() string {
		var all strings.Builder
		for _, c := range h.gh.checks {
			fmt.Fprintf(&all, "%s|%s|%s|%s\n", c.Title, c.Summary, c.Text, c.Conclusion)
		}
		for _, c := range h.gh.comments {
			all.WriteString(c.Body + "\n")
		}
		return all.String()
	}
	got := everything()
	for _, leak := range []string{"jane@example.com", "22P02", "invalid input syntax", "line of migrate"} {
		if strings.Contains(got, leak) {
			t.Errorf("GitHub was told %q:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "dashboard") || !strings.Contains(got, "failure") {
		t.Errorf("the report does not say that it failed and where to look:\n%s", got)
	}
	// The run keeps its log: it is in the dashboard.
	kept := false
	h.br.mu.Lock()
	for _, run := range h.br.runs {
		for _, st := range run.Steps {
			kept = kept || strings.Contains(st.Log, "jane@example.com")
		}
	}
	h.br.mu.Unlock()
	if !kept {
		t.Errorf("the run lost its log")
	}
}

// A branch that holds only what the repository gave it gets the failure, shortened, with the end of
// the failed step's log in the check run, and never a credential.
func TestAFailureOfAPreviewBranchIsShortenedAndHasNoCredential(t *testing.T) {
	h := newHarness(t)
	h.db.failAt = "20240101000000"
	h.db.failMsg = `ERROR: could not connect to postgres://admin:hunter2secret@db.internal:5432/app with password=s3cr3tvalue and Bearer abcdefghijklmnop123 ` + strings.Repeat("padding ", 100)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 6, "feature", "sha-1"))
	var check *fakeCheck
	for _, c := range h.gh.checks {
		check = c
	}
	var comment string
	for _, c := range h.gh.comments {
		comment = c.Body
	}
	if check == nil || check.Conclusion != "failure" || comment == "" {
		t.Fatalf("check = %+v, comment = %q", check, comment)
	}
	for _, text := range []string{check.Title, check.Summary, check.Text, comment} {
		for _, leak := range []string{"hunter2secret", "s3cr3tvalue", "abcdefghijklmnop123"} {
			if strings.Contains(text, leak) {
				t.Errorf("a credential reached GitHub (%q): %.300s", leak, text)
			}
		}
	}
	if strings.Contains(check.Summary, strings.Repeat("padding ", 60)) || len(check.Summary) > 1500 {
		t.Errorf("the failure is not shortened (%d bytes)", len(check.Summary))
	}
	if !strings.Contains(check.Summary, "could not connect") || !strings.Contains(check.Summary, "[redacted]") {
		t.Errorf("the summary lost the start of the failure: %.400s", check.Summary)
	}
	// The check run has the end of the failed step's log, not the log of the run.
	if !strings.Contains(check.Text, "FAILED") || strings.Contains(check.Text, "line of seed") || len(check.Text) > maxPublicLog+200 {
		t.Errorf("the check run's text is not the failed step's excerpt (%d bytes): %.300s", len(check.Text), check.Text)
	}
}

func TestRedactTakesOutWhatLooksLikeACredentialAndTheSecretsTheRunRead(t *testing.T) {
	for in, want := range map[string]string{
		"dial postgres://app:pw1234@host:5432/db failed":     "dial postgres://app:[redacted]@host:5432/db failed",
		"Authorization: Bearer abcdefgh12345678":             "Authorization: Bearer [redacted]",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln ok":   "jwt [redacted token] ok",
		"smtp_pass = hunter2 and api_key: abc123":            "smtp_pass = [redacted] and api_key: [redacted]",
		"password='my pass' next":                            "password=[redacted] next",
		"the host smtp.example.com refused user bob":         "the host smtp.example.com refused user bob",
		"relation \"todos\" does not exist (SQLSTATE 42P01)": "relation \"todos\" does not exist (SQLSTATE 42P01)",
		"value from env: s3cretFromEnv end":                  "value from env: [redacted] end",
	} {
		if got := redact(in, []string{"s3cretFromEnv", "x"}); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clip("héllo wörld", 8); !utf8.ValidString(got) || len(got) > 8 {
		t.Errorf("clip = %q", got)
	}
}

func TestProductionDeployWithoutASupabaseDirectoryIsNeutral(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.reg.CreateProject(ctx, &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusActiveHealthy})
	_ = h.reg.SetDefaultBranchGit(ctx, prodRef, "main")
	h.gh.setCommit("sha-main", map[string]string{"README.md": "x"})
	h.gh.setHead("main", "sha-main")
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
	h.gh.setHead("main", "sha-main")
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

func TestARenamedRepositoryKeepsItsConnectionAndItsEventsWork(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	// The owner renames acme/app: the id stays, GitHub answers the old name with a redirect (here a
	// 404 would do as little good), and the next event carries the new name.
	h.gh.repo = "acme/app2"
	h.deliver("pull_request", "d1", h.prEvent("opened", 3, "feature", "sha-1"))
	conn, err := h.reg.GetGitHubConnection(context.Background(), h.conn.ID)
	if err != nil || conn.RepositoryName != "acme/app2" {
		t.Fatalf("connection after the rename = %+v, %v", conn, err)
	}
	if b := h.br.byName("feature"); b == nil || b.State != registry.BranchFunctionsDeployed {
		t.Fatalf("the pull request of the renamed repository was not built: %+v", b)
	}
	if h.gh.callsTo("POST /repos/acme/app2/check-runs") != 1 || h.gh.callsTo("POST /repos/acme/app2/issues/3/comments") != 1 {
		t.Fatalf("the report did not go to the new name: %v", h.gh.calls)
	}
}

func TestARedirectIsAnErrorAndNotAnAnswerToDecode(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		w.Header().Set("Location", "https://api.github.com/repositories/1001/check-runs")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte(`{"message":"Temporary Redirect","url":"https://api.github.com/repositories/1001/check-runs"}`))
	}))
	defer srv.Close()
	cl := &Client{HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, API: srv.URL, Web: srv.URL}
	var out struct {
		ID int64 `json:"id"`
	}
	err := cl.do(context.Background(), "tok", http.MethodPost, "/repos/acme/old/check-runs", map[string]string{"name": "x"}, &out)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusTemporaryRedirect || out.ID != 0 {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
	if len(asked) != 1 {
		t.Fatalf("requests = %v: a POST must not be repeated", asked)
	}
}

func TestAPausedPreviewBranchIsWokenForTheNextCommit(t *testing.T) {
	h := newHarness(t)
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 8, "idle", "sha-1"))
	b := h.br.byName("idle")
	h.br.mu.Lock()
	h.br.paused = map[string]bool{b.Ref: true}
	h.br.mu.Unlock()
	h.gh.setCommit("sha-2", repoFiles(map[string]string{"supabase/migrations/20240102000000_done.sql": mig2}))
	h.deliver("pull_request", "d2", h.prEvent("synchronize", 8, "idle", "sha-2"))
	if len(h.br.woken) != 1 || h.br.woken[0] != b.Ref {
		t.Fatalf("woken = %v", h.br.woken)
	}
	if got := h.br.byName("idle"); got.State != registry.BranchFunctionsDeployed || len(h.db.applied(b.Ref)) != 2 {
		t.Fatalf("the commit was not applied to the woken branch: %+v, %v", got, h.db.applied(b.Ref))
	}
}

func TestTwoProjectsOfOneRepositoryKeepTheirOwnComments(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repA, err := h.svc.reporterFor(ctx, h.gh.instID, h.gh.repo, "aaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	repB, err := h.svc.reporterFor(ctx, h.gh.instID, h.gh.repo, "bbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	// Both report on pull request 5 with no comment id yet, as two jobs of a new pull request do.
	a, err := repA.comment(ctx, 5, 0, "report of A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := repB.comment(ctx, 5, 0, "report of B")
	if err != nil {
		t.Fatal(err)
	}
	if a == 0 || b == 0 || a == b {
		t.Fatalf("comment ids = %d, %d: the second project adopted the first's comment", a, b)
	}
	// Each finds its own again when its link has lost the id.
	if again, err := repB.comment(ctx, 5, 0, "second report of B"); err != nil || again != b {
		t.Fatalf("B's comment after losing the id = %d, %v, want %d", again, err, b)
	}
	if h.gh.comments[a].Updates != 0 || !strings.Contains(h.gh.comments[a].Body, "report of A") {
		t.Fatalf("A's comment was edited by B: %+v", h.gh.comments[a])
	}
}

func TestACommentAnotherAppWroteIsNotAdoptedAndAForbiddenEditMakesANewOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rep, err := h.svc.reporterFor(ctx, h.gh.instID, h.gh.repo, prodRef)
	if err != nil {
		t.Fatal(err)
	}
	// A comment with our marker that another App wrote (a second server on the repository, or
	// somebody copying the first line) is left alone.
	h.gh.comments[900] = &fakeComment{ID: 900, PR: 7, Body: rep.marker() + "\nold", AppID: h.gh.appID + 1}
	id, err := rep.comment(ctx, 7, 0, "mine")
	if err != nil || id == 900 || h.gh.comments[900].Updates != 0 {
		t.Fatalf("comment = %d, %v, foreign comment %+v", id, err, h.gh.comments[900])
	}
	// A comment this App may no longer edit is replaced.
	h.gh.comments[id].Forbidden = true
	next, err := rep.comment(ctx, 7, id, "again")
	if err != nil || next == id || next == 0 {
		t.Fatalf("comment after a 403 = %d, %v (was %d)", next, err, id)
	}
}

func TestMigrationsAreAppliedUnderTheProjectsApplyLockSoThatAMergeDoesNotInterleave(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.reg.CreateProject(ctx, &registry.Project{Ref: prodRef, Name: "prod", Status: registry.StatusActiveHealthy})
	_ = h.reg.SetDefaultBranchGit(ctx, prodRef, "main")
	h.gh.setCommit("head-main", repoFiles(nil))
	if _, err := h.svc.DeployProduction(ctx, prodRef); err != nil {
		t.Fatal(err)
	}
	h.svc.wg.Wait()
	h.gh.setCommit("sha-1", repoFiles(nil))
	h.deliver("pull_request", "d1", h.prEvent("opened", 2, "feature", "sha-1"))
	if len(h.db.applyLog) < 2 {
		t.Fatalf("migrations applied = %v", h.db.applyLog)
	}
	if len(h.db.unlocked) != 0 {
		t.Fatalf("migrations applied without the apply lock: %v", h.db.unlocked)
	}
}

func TestABurstOfPullRequestsRunsAtMostTheConfiguredNumberOfBuilds(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.GitHub.MaxConcurrentBuilds = 2 })
	h.gh.setCommit("sha-1", repoFiles(nil))
	// Every build applies its first migration; the gate holds each one there.
	h.db.gate = make(chan struct{})
	h.db.gateAt = "20240101000000"
	h.db.entered = make(chan struct{}, 8)
	for n := 1; n <= 5; n++ {
		body, _ := jsonBody(h.prEvent("opened", n, fmt.Sprintf("feature-%d", n), "sha-1"))
		h.svc.HandleWebhook(context.Background(), Delivery{Event: "pull_request", ID: fmt.Sprintf("burst-%d", n), Signature: h.sign(body), Body: body})
	}
	for i := 0; i < 2; i++ {
		select {
		case <-h.db.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("build %d never reached its migration", i+1)
		}
	}
	select {
	case <-h.db.entered:
		t.Fatal("a third build started while two were running")
	case <-time.After(300 * time.Millisecond):
	}
	close(h.db.gate)
	h.svc.wg.Wait()
	if got := len(h.br.names()); got != 5 {
		t.Fatalf("branches after the burst = %v, want all five built in turn", h.br.names())
	}
}

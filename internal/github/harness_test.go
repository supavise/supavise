package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// fakeDB stands in for the branches' databases.
type fakeDB struct {
	mu        sync.Mutex
	history   map[string][]branching.Migration
	scripts   map[string][]string
	applyLog  []string // ref/version, in order
	failAt    string   // a version whose migration fails
	failSeed  bool
	bucketLag int // RunScripts that fail with "Storage schema is not ready" first
}

func newFakeDB() *fakeDB {
	return &fakeDB{history: map[string][]branching.Migration{}, scripts: map[string][]string{}}
}

func (d *fakeDB) Migrations(_ context.Context, ref string) ([]branching.Migration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.history[ref]), nil
}

func (d *fakeDB) Apply(_ context.Context, ref string, ms []branching.Migration, _ branching.ApplyOptions) (branching.ApplyResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range ms {
		if m.Version == d.failAt {
			return branching.ApplyResult{}, fmt.Errorf("ERROR: syntax error at or near \"selec\" (SQLSTATE 42601)")
		}
		d.history[ref] = append(d.history[ref], m)
		d.applyLog = append(d.applyLog, ref+"/"+m.Version)
	}
	return branching.ApplyResult{Applied: len(ms)}, nil
}

func (d *fakeDB) RunScript(_ context.Context, ref, script string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failSeed && !strings.Contains(script, "storage.buckets") {
		return fmt.Errorf("ERROR: relation \"nope\" does not exist")
	}
	if strings.Contains(script, "storage.buckets") && d.bucketLag > 0 {
		d.bucketLag--
		return fmt.Errorf("ERROR: the Storage schema is not ready")
	}
	d.scripts[ref] = append(d.scripts[ref], script)
	return nil
}

func (d *fakeDB) applied(ref string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, m := range d.history[ref] {
		out = append(out, m.Version)
	}
	return out
}

// fakeBranches stands in for the branching service: it runs the SchemaSource and the Driver at once.
type fakeBranches struct {
	db *fakeDB

	mu       sync.Mutex
	branches map[string]*branching.Branch
	creates  []branching.CreateInput
	deleted  []string
	drives   []string
	runs     map[string]*registry.BranchRun
	n        int
	// failFunctions makes the deploy of a create fail.
	maxPerProject int
}

func newFakeBranches(db *fakeDB) *fakeBranches {
	return &fakeBranches{db: db, branches: map[string]*branching.Branch{}, runs: map[string]*registry.BranchRun{}}
}

func (f *fakeBranches) run(ref string) *registry.BranchRun {
	f.n++
	id := fmt.Sprintf("run-%d", f.n)
	r := &registry.BranchRun{ID: id, OwnerRef: ref, BranchRef: ref, Status: registry.RunSuccess}
	for _, s := range registry.BranchRunSteps {
		r.Steps = append(r.Steps, registry.BranchRunStep{Name: s, Status: registry.StepExited, Log: "line of " + s + "\n"})
	}
	f.runs[id] = r
	return r
}

func (f *fakeBranches) Create(ctx context.Context, ref string, in branching.CreateInput) (*branching.Branch, error) {
	f.mu.Lock()
	for _, b := range f.branches {
		if b.ParentRef == ref && b.Name == in.Name {
			f.mu.Unlock()
			return nil, fmt.Errorf("%w: project %s already has a branch named %q", branching.ErrConflict, ref, in.Name)
		}
	}
	if f.maxPerProject > 0 {
		n := 0
		for _, b := range f.branches {
			if b.ParentRef == ref {
				n++
			}
		}
		if n >= f.maxPerProject {
			f.mu.Unlock()
			return nil, fmt.Errorf("%w: project %s already has %d branches", branching.ErrLimit, ref, n)
		}
	}
	f.creates = append(f.creates, in)
	newRef := secrets.NewRef()
	b := &branching.Branch{ID: secrets.NewUUID(), Name: in.Name, Ref: newRef, ParentRef: ref, GitBranch: in.GitBranch, Persistent: in.Persistent,
		State: registry.BranchCreatingProject, ProjectStatus: registry.StatusActiveHealthy, CreatedAt: time.Now()}
	run := f.run(newRef)
	b.RunID = run.ID
	f.branches[newRef] = b
	f.mu.Unlock()

	state, detail := registry.BranchFunctionsDeployed, "schema only"
	var err error
	if in.SchemaSource != nil {
		var d string
		if d, err = in.SchemaSource.Schema(ctx, nil, newRef); err == nil {
			detail = d
			err = in.SchemaSource.Functions(ctx, nil, newRef)
			if err != nil {
				state = registry.BranchFunctionsFailed
			}
		} else {
			state = registry.BranchMigrationsFailed
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		detail = err.Error()
		run.Status = registry.RunFailed
	}
	b.State, b.Detail = state, detail
	c := *b
	return &c, nil
}

func (f *fakeBranches) Delete(_ context.Context, idOrRef string, _ branching.DeleteOptions) (*branching.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.branches[idOrRef]
	if !ok {
		return nil, fmt.Errorf("%w: %s", branching.ErrNotFound, idOrRef)
	}
	delete(f.branches, idOrRef)
	f.deleted = append(f.deleted, idOrRef)
	return b, nil
}

func (f *fakeBranches) Resolve(_ context.Context, idOrRef string) (*branching.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.branches[idOrRef]
	if !ok {
		return nil, fmt.Errorf("%w: %s", branching.ErrNotFound, idOrRef)
	}
	c := *b
	return &c, nil
}

func (f *fakeBranches) List(_ context.Context, ref string) ([]branching.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []branching.Branch{{ID: "default", Name: "main", Ref: ref, ParentRef: ref, IsDefault: true, Persistent: true}}
	var refs []string
	for r := range f.branches {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	for _, r := range refs {
		if b := f.branches[r]; b.ParentRef == ref {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (f *fakeBranches) WaitIdle(ctx context.Context, ref string) (*branching.Branch, error) {
	return f.Resolve(ctx, ref)
}

func (f *fakeBranches) Drive(ctx context.Context, idOrRef, op string, fn branching.Driver) (string, error) {
	f.mu.Lock()
	b, ok := f.branches[idOrRef]
	if !ok {
		f.mu.Unlock()
		return "", fmt.Errorf("%w: %s", branching.ErrNotFound, idOrRef)
	}
	f.drives = append(f.drives, idOrRef)
	run := f.run(idOrRef)
	f.mu.Unlock()
	detail, err := fn(ctx, nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		b.State, b.Detail = registry.BranchMigrationsFailed, err.Error()
		run.Status = registry.RunFailed
	} else {
		b.State, b.Detail = registry.BranchFunctionsDeployed, detail
	}
	return run.ID, nil
}

func (f *fakeBranches) StartRun(_ context.Context, op, branchRef, ownerRef, parentRef string) (*branching.Run, error) {
	return nil, nil
}

func (f *fakeBranches) GetRun(_ context.Context, ownerRef, id string) (*registry.BranchRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, fmt.Errorf("%w: run %s", branching.ErrNotFound, id)
	}
	c := *r
	return &c, nil
}

func (f *fakeBranches) Database() branching.Database { return f.db }

func (f *fakeBranches) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.branches {
		out = append(out, b.Name)
	}
	sort.Strings(out)
	return out
}

func (f *fakeBranches) byName(name string) *branching.Branch {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.branches {
		if b.Name == name {
			c := *b
			return &c
		}
	}
	return nil
}

// fakeHost records what the runner asked the node to do.
type fakeHost struct {
	mu       sync.Mutex
	deployed []string // slug@ref
	files    map[string][]string
	patches  map[string]map[string]any // service@ref -> merged settings
	secrets  map[string]string
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: map[string][]string{}, patches: map[string]map[string]any{}, secrets: map[string]string{}}
}

func (h *fakeHost) DeployFunction(_ context.Context, ref string, f FunctionDeploy) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deployed = append(h.deployed, f.Slug+"@"+ref)
	var paths []string
	for _, fl := range f.Files {
		paths = append(paths, fl.Path)
	}
	h.files[f.Slug+"@"+ref] = paths
	return nil
}

func (h *fakeHost) FunctionSecret(_ context.Context, ref, name string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.secrets[ref+"/"+name]
	return v, ok, nil
}

func (h *fakeHost) SaveSettings(_ context.Context, ref string, svc projectconfig.Service, patch map[string]any) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := string(svc) + "@" + ref
	if h.patches[k] == nil {
		h.patches[k] = map[string]any{}
	}
	var changed []string
	for key, v := range patch {
		if fmt.Sprint(h.patches[k][key]) != fmt.Sprint(v) {
			changed = append(changed, key)
		}
		h.patches[k][key] = v
	}
	sort.Strings(changed)
	return changed, nil
}

// harness is a service over fakes, with the operator's App configured.
type harness struct {
	t    *testing.T
	svc  *Service
	gh   *fakeGitHub
	br   *fakeBranches
	db   *fakeDB
	host *fakeHost
	reg  *registry.Memory
	cfg  *config.Config
	conn registry.GitHubConnection
}

const prodRef = "pppppppppppppppppppp"

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := config.Default()
	cfg.Domain = "example.test"
	cfg.TLS.Mode = "off"
	cfg.GitHub = config.GitHub{AppID: gh.appID, AppSlug: "supavise-test", ClientID: "Iv1.test", APIURL: gh.URL(), WebURL: gh.URL(),
		ClientSecretFile: write("client", gh.clientSecret+"\n"), PrivateKeyFile: write("key.pem", gh.pemKey()), WebhookSecretFile: write("hook", gh.webhookSecret+"\n")}
	for _, m := range mutate {
		m(cfg)
	}
	key := make([]byte, 32)
	sec, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.NewMemory()
	db := newFakeDB()
	br := newFakeBranches(db)
	host := newFakeHost()
	svc, err := New(Options{Cfg: cfg, Registry: reg, Secrets: sec, Branches: br, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Drain(context.Background()) })
	h := &harness{t: t, svc: svc, gh: gh, br: br, db: db, host: host, reg: reg, cfg: cfg}
	h.conn = registry.GitHubConnection{OrgID: 1, ProjectRef: prodRef, InstallationID: gh.instID, RepositoryID: gh.repoID, RepositoryName: gh.repo,
		SupabaseChangesOnly: false, BranchLimit: 50, NewBranchPerPR: true, CreatedBy: "user-1"}
	if err := reg.CreateGitHubConnection(context.Background(), &h.conn); err != nil {
		t.Fatal(err)
	}
	return h
}

// deliver posts a signed event and waits for the work it queued.
func (h *harness) deliver(event, id string, payload any) WebhookResult {
	h.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	res := h.svc.HandleWebhook(context.Background(), Delivery{Event: event, ID: id, Signature: h.sign(body), Body: body})
	h.svc.wg.Wait()
	return res
}

func (h *harness) sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(h.gh.webhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// prPayload builds a pull_request event for the repository.
func (h *harness) prEvent(action string, n int, head, sha string) map[string]any {
	repo := map[string]any{"id": h.gh.repoID, "full_name": h.gh.repo, "default_branch": "main"}
	return map[string]any{
		"action": action, "number": n,
		"pull_request": map[string]any{"number": n, "state": "open", "title": "A change", "merged": false,
			"head": map[string]any{"ref": head, "sha": sha, "repo": repo},
			"base": map[string]any{"ref": "main", "sha": "base-sha", "repo": repo}},
		"repository": repo, "installation": map[string]any{"id": h.gh.instID},
	}
}

func (h *harness) pushEvent(ref, after string, files ...string) map[string]any {
	repo := map[string]any{"id": h.gh.repoID, "full_name": h.gh.repo, "default_branch": "main"}
	ev := map[string]any{"ref": ref, "before": "0000", "after": after, "repository": repo, "installation": map[string]any{"id": h.gh.instID}}
	if len(files) > 0 {
		ev["commits"] = []map[string]any{{"modified": files}}
	}
	return ev
}

const (
	mig1 = "create table public.todos (id bigint primary key);"
	mig2 = "alter table public.todos add column done boolean;"
)

// repoFiles is a repository with two migrations, a seed, a config and a function.
func repoFiles(extra map[string]string) map[string]string {
	f := map[string]string{
		"README.md": "# app",
		"supabase/migrations/20240101000000_init.sql": mig1,
		"supabase/seed.sql":                           "insert into public.todos (id) values (1);",
		"supabase/config.toml":                        "project_id = \"app\"\n[api]\nmax_rows = 500\n",
		"supabase/functions/hello/index.ts":           "Deno.serve(() => new Response('hi'))",
		"supabase/functions/_shared/cors.ts":          "export const cors = {}",
	}
	for k, v := range extra {
		if v == "" {
			delete(f, k)
		} else {
			f[k] = v
		}
	}
	return f
}

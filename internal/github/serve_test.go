package github_test

// TestServeGitHub starts a node on real clusters, a parent project, the Management API with the
// branching service and the GitHub integration, and supavise-pgmeta, and keeps them up so that
// tests/linux/github-smoke.sh can post signed webhooks at it:
//
//	SUPAVISE_GITHUB_SERVE_FILE=$PWD/stack.json SUPAVISE_GITHUB_SERVE_CONFIG=$PWD/github.json \
//	  SUPAVISE_TEST_UNPACKED=~/.cache/sbctl/unpacked go test ./internal/github -run TestServeGitHub -timeout 60m
//
// SUPAVISE_GITHUB_SERVE_CONFIG is a JSON file with the [github] section: app_id, app_slug, client_id,
// the three secret files and api_url and web_url (the script's fake GitHub). The out file gets
// {api_url, pat, parent_ref}; delete it to stop everything. Without the variables the test skips.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/github"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

type dirArts map[string]string

func (d dirArts) Dir(svc string) (string, error) {
	if dir, ok := d[svc]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("no artifact for %s", svc)
}
func (d dirArts) Tag(svc string) (string, error) { return filepath.Base(d[svc]), nil }

func TestServeGitHub(t *testing.T) {
	out, root, ghConf := os.Getenv("SUPAVISE_GITHUB_SERVE_FILE"), os.Getenv("SUPAVISE_TEST_UNPACKED"), os.Getenv("SUPAVISE_GITHUB_SERVE_CONFIG")
	if out == "" || root == "" || ghConf == "" {
		t.Skip("set SUPAVISE_GITHUB_SERVE_FILE, SUPAVISE_GITHUB_SERVE_CONFIG and SUPAVISE_TEST_UNPACKED to serve the stack")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Fatalf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	pgmeta, _ := filepath.Glob(filepath.Join(root, "pgmeta-*", "bin", "pgmeta"))
	if len(pgmeta) == 0 {
		t.Fatal("no pgmeta artifact")
	}
	state, err := os.MkdirTemp("", "sbgh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	bin := filepath.Join(state, "supavise")
	build := exec.Command("go", "build", "-o", bin, "./cmd/supavise")
	_, thisFile, _, _ := runtime.Caller(0)
	build.Dir = filepath.Join(filepath.Dir(thisFile), "..", "..")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}

	base := freeBase(t, 60)
	cfg := config.Default()
	cfg.StateDir = filepath.Join(state, "s")
	cfg.KeyPath = filepath.Join(state, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	cfg.BinPath = bin
	cfg.Backup.Backend = "file://" + filepath.Join(state, "backups")
	cfg.Backup.ArchiveTimeoutSeconds = 30
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = base, base+1, base+2
	cfg.Ports.PGMeta = base + 52 // and +1 for its admin app
	cfg.API.PGMetaCryptoKey = "serve-test-key"
	cfg.Branching.SweepIntervalSeconds = 5
	raw, err := os.ReadFile(ghConf)
	if err != nil {
		t.Fatal(err)
	}
	// The script writes the field names of config.toml.
	var in struct {
		AppID             int64  `json:"app_id"`
		AppSlug           string `json:"app_slug"`
		ClientID          string `json:"client_id"`
		ClientSecretFile  string `json:"client_secret_file"`
		PrivateKeyFile    string `json:"private_key_file"`
		WebhookSecretFile string `json:"webhook_secret_file"`
		APIURL            string `json:"api_url"`
		WebURL            string `json:"web_url"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	cfg.GitHub = config.GitHub{AppID: in.AppID, AppSlug: in.AppSlug, ClientID: in.ClientID, ClientSecretFile: in.ClientSecretFile,
		PrivateKeyFile: in.PrivateKeyFile, WebhookSecretFile: in.WebhookSecretFile, APIURL: in.APIURL, WebURL: in.WebURL}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(state, "config.toml")
	b, _ := toml.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPAVISE_CONFIG", cfgPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oo := lifecycle.OpenOptions{Artifacts: arts, ConfigPath: cfgPath}
	t.Cleanup(func() { _ = lifecycle.StopAll(context.Background(), cfg, oo) })
	n, err := lifecycle.InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	meta := exec.Command(pgmeta[0])
	meta.Env = append(os.Environ(), fmt.Sprintf("PG_META_PORT=%d", cfg.Ports.PGMeta), "PG_META_HOST=127.0.0.1", "CRYPTO_KEY="+cfg.API.PGMetaCryptoKey,
		fmt.Sprintf("PG_META_ADMIN_PORT=%d", cfg.Ports.PGMeta+1), "NODE_OPTIONS=--max-old-space-size=192")
	meta.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := meta.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-meta.Process.Pid, syscall.SIGKILL); _, _ = meta.Process.Wait() })
	for i := 0; ; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Ports.PGMeta))
		if err == nil {
			resp.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("pgmeta did not start")
		}
		time.Sleep(200 * time.Millisecond)
	}

	pg, ok := n.Registry.(*registry.Postgres)
	if !ok {
		t.Fatalf("the registry is %T, want Postgres", n.Registry)
	}
	fnStore := api.NewPGStore(pg.Pool())
	svc, err := branching.New(branching.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Engine: n.Engine, CreateWait: 60 * time.Second,
		Functions: api.NewBranchFunctions(fnStore, n.Secrets)})
	if err != nil {
		t.Fatal(err)
	}
	go svc.Run(ctx) //nolint:errcheck
	defer svc.Drain(context.Background())
	gh, err := github.New(github.Options{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Branches: svc})
	if err != nil {
		t.Fatal(err)
	}
	defer gh.Drain(context.Background())

	parent, err := n.Engine.Create(ctx, lifecycle.CreateRequest{Name: "Parent", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.NewServer(api.Deps{Registry: n.Registry, Secrets: n.Secrets, Manager: n.Engine, Config: cfg, Branching: svc,
		CreateWait: 30 * time.Second, Store: fnStore, GitHub: gh})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+50))
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln) //nolint:errcheck
	defer hs.Close()

	const serveUser = "11111111-2222-4333-8444-555555555555"
	orgs, err := n.Registry.ListOrganizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var orgID int64
	for _, o := range orgs {
		orgID = o.ID
		if err := srv.Members().EnsureOwner(ctx, members.OrgRef{ID: o.ID, Slug: o.Slug}, serveUser); err != nil {
			t.Fatal(err)
		}
	}
	pat := secrets.NewPAT()
	if err := n.Registry.CreateAccessToken(ctx, &registry.AccessToken{UserID: serveUser, Name: "serve", Hash: secrets.HashToken(pat), Prefix: pat[:8]}); err != nil {
		t.Fatal(err)
	}
	// A dashboard session for the /platform routes, which take no personal access token: signed with the
	// system project's JWT secret, like the sessions of supavise-gotrue@system.
	sysKeys, err := n.Engine.Keys(ctx, config.SystemRef)
	if err != nil {
		t.Fatal(err)
	}
	session, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": serveUser, "email": "serve@example.test", "role": "authenticated", "aud": "authenticated",
		"exp": time.Now().Add(55 * time.Minute).Unix(), "iat": time.Now().Unix(),
		"app_metadata": map[string]any{api.AdminClaim: true},
	}).SignedString([]byte(sysKeys.JWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.MarshalIndent(map[string]any{
		"api_url": "http://" + ln.Addr().String(), "pat": pat, "jwt": session, "parent_ref": parent.Ref, "org_id": orgID, "state_dir": cfg.StateDir,
	}, "", "  ")
	if err := os.WriteFile(out, info, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(55 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, err := os.Stat(out); err != nil {
			return
		}
	}
}

func freeBase(t *testing.T, n int) int {
	t.Helper()
	for base := 39000; base+n < 39900; base += n + 3 {
		var ls []net.Listener
		ok := true
		for i := 0; i < n; i++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			ls = append(ls, l)
		}
		for _, l := range ls {
			l.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port range")
	return 0
}

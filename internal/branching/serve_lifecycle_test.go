package branching_test

// TestServeLifecycle is TestServe with the edge proxy in front of the projects and the idle pause on:
// a node on real clusters, a parent project, the Management API, the sweeper, the idle loop with a
// period of a few seconds ($SUPAVISE_BRANCH_SERVE_IDLE, 8s by default), and the proxy on a loopback port.
// tests/linux/branching/lifecycle.sh drives it: a scheduled delete pauses a branch and restore resumes it,
// an idle branch pauses, a request through the proxy and a database connection wake it, a branch the
// owner paused stays paused, a persistent branch never sleeps.
//
//	SUPAVISE_BRANCH_SERVE_FILE=$PWD/stack.json SUPAVISE_TEST_UNPACKED=~/.cache/sbctl/unpacked \
//	  go test ./internal/branching -run TestServeLifecycle -timeout 60m
//
// The file gets {api_url, pat, parent_ref, state_dir, backup_dir, project_base, proxy_url, domain, idle_after, dsn_url};
// delete it
// to stop everything.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/reqstats"
	"github.com/supavise/supavise/internal/secrets"
)

func TestServeLifecycle(t *testing.T) {
	out := os.Getenv("SUPAVISE_BRANCH_SERVE_FILE")
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if out == "" || root == "" {
		t.Skip("set SUPAVISE_BRANCH_SERVE_FILE and SUPAVISE_TEST_UNPACKED to serve the lifecycle stack")
	}
	idle := os.Getenv("SUPAVISE_BRANCH_SERVE_IDLE")
	if idle == "" {
		idle = "8s"
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
	state, err := os.MkdirTemp("/tmp", "sbtl")
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

	base := freeBase(t, 40)
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
	cfg.Ports.PGMeta = base + 36 // and +1 for its admin app
	cfg.API.PGMetaCryptoKey = "serve-test-key"
	cfg.Branching.SweepIntervalSeconds = 5
	cfg.Branching.IdlePauseAfter = idle
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

	store, err := backup.OpenStore(ctx, cfg.Backup)
	if err != nil {
		t.Fatal(err)
	}
	bk, err := backup.New(backup.Options{Config: cfg, Registry: n.Registry, Store: store, Secrets: n.Secrets,
		Access: backup.AccessFromRegistry(cfg, n.Registry, n.Secrets), ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	bk.SetManager(n.Engine)
	requests := reqstats.New()
	svc, err := branching.New(branching.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Engine: n.Engine, Backup: bk,
		CreateWait: 60 * time.Second, Activity: requests})
	if err != nil {
		t.Fatal(err)
	}
	go svc.Run(ctx)     //nolint:errcheck
	go svc.RunIdle(ctx) //nolint:errcheck
	defer svc.Drain(context.Background())

	parent, err := n.Engine.Create(ctx, lifecycle.CreateRequest{Name: "Parent", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	dsn, _ := n.Engine.ConnString(ctx, parent.Ref, "postgres")
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `
		create table public.items (id int primary key, name text);
		insert into public.items select g, 'row ' || g from generate_series(1, 50) g;
		grant select on public.items to anon; -- new tables need a grant for the Data API (v0.3.0 defaults, as hosted)
		create schema supabase_migrations;
		create table supabase_migrations.schema_migrations (version text not null primary key, statements text[], name text);
		insert into supabase_migrations.schema_migrations values
		  ('20260101000000', array['create table if not exists public.items (id int primary key, name text)', 'grant select on public.items to anon'], 'items')`); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)

	srv, err := api.NewServer(api.Deps{Registry: n.Registry, Secrets: n.Secrets, Manager: n.Engine, Config: cfg, Branching: svc, CreateWait: 30 * time.Second, Requests: requests})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+34))
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln) //nolint:errcheck
	defer hs.Close()

	edge, err := proxy.New(proxy.Options{Config: cfg, Registry: n.Registry, Keys: n.Engine, Stats: requests, IdleWaker: svc,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	go edge.Sync(ctx)
	pln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+35))
	if err != nil {
		t.Fatal(err)
	}
	ps := &http.Server{Handler: edge.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go ps.Serve(pln) //nolint:errcheck
	defer ps.Close()

	// The script connects to the databases of branches the way the pooler does: over TCP, with the password.
	mux := http.NewServeMux()
	mux.HandleFunc("/dsn/", func(w http.ResponseWriter, r *http.Request) {
		dsn, err := n.Engine.ConnString(r.Context(), strings.TrimPrefix(r.URL.Path, "/dsn/"), lifecycle.RolePostgres)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, dsn)
	})
	dl, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+38))
	if err != nil {
		t.Fatal(err)
	}
	ds := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go ds.Serve(dl) //nolint:errcheck
	defer ds.Close()

	// The token's user owns every organization, as the claimed first user of a node does (serve_test.go does the
	// same): without a seat the API refuses to create a branch of the parent ("You are not a member of this
	// organization").
	const serveUser = "11111111-2222-4333-8444-555555555555"
	orgs, err := n.Registry.ListOrganizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orgs {
		if err := srv.Members().EnsureOwner(ctx, members.OrgRef{ID: o.ID, Slug: o.Slug}, serveUser); err != nil {
			t.Fatal(err)
		}
	}
	pat := secrets.NewPAT()
	if err := n.Registry.CreateAccessToken(ctx, &registry.AccessToken{UserID: serveUser, Name: "serve", Hash: secrets.HashToken(pat), Prefix: pat[:8]}); err != nil {
		t.Fatal(err)
	}
	info, _ := json.MarshalIndent(map[string]any{
		"api_url": "http://" + ln.Addr().String(), "pat": pat, "parent_ref": parent.Ref, "state_dir": cfg.StateDir, "config": cfgPath,
		"backup_dir": filepath.Join(state, "backups"), "project_base": cfg.Ports.ProjectBase, "proxy_url": "http://" + pln.Addr().String(),
		"domain": cfg.Domain, "idle_after": idle, "dsn_url": "http://" + dl.Addr().String() + "/dsn/",
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

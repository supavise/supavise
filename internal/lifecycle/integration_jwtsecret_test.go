package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/secrets"
)

// TestIntegrationJWTSecret runs the real GoTrue and PostgREST under the exec backend and moves
// a project's JWT secret twice: a project created with the secret of a project moved from
// hosted Supabase accepts the keys that project's app holds, and SetJWTSecret makes the
// services follow the new secret and refuse the old one. It needs SUPAVISE_TEST_UNPACKED (see
// TestIntegrationSystemAndProject) and starts two PostgreSQL clusters.
func TestIntegrationJWTSecret(t *testing.T) {
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if root == "" {
		t.Skip("SUPAVISE_TEST_UNPACKED not set")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1)")
	}
	base := freePortBase(t, 8)
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.KeyPath = filepath.Join(cfg.StateDir, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	cfg.BinPath = truePath
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = base, base+1, base+2

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	oo := OpenOptions{Artifacts: arts}
	t.Cleanup(func() {
		if err := StopAll(context.Background(), cfg, oo); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})
	n, err := InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	// What the app of a project moved from hosted holds: a secret, and keys of another ref.
	const hosted = "hosted-legacy-secret-0123456789-abcdefghij"
	const hostedRef = "zzzzzzzzzzzzzzzzzzzz"
	ref := secrets.NewRef()
	keys, err := secrets.NewProjectKeysWithSecret(ref, hosted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signed := func(secret, role string) string {
		s, err := secrets.NewLegacyKey(secret, hostedRef, role, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	hostedAnon := signed(hosted, secrets.RoleAnon)
	if err := keys.UseLegacyKey(secrets.RoleAnon, hostedAnon); err != nil {
		t.Fatal(err)
	}
	p, err := n.Engine.Create(ctx, CreateRequest{Name: "moved", Class: "micro", Ref: ref, Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	ports := cfg.PortsFor(p.Ref, p.Seq)
	// status of a request with the bearer token to PostgREST's root (the OpenAPI document).
	rest := func(bearer string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", ports.PostgREST), nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := rest(hostedAnon); c != 200 {
		t.Fatalf("a project created with the hosted secret answers the hosted anon key with %d, want 200", c)
	}
	if got, _ := n.Engine.Keys(ctx, p.Ref); got.AnonKey != hostedAnon {
		t.Fatal("the stored anon key is not the one given")
	}
	if c := rest(signed("another-secret-0123456789-abcdefghijklmnop", secrets.RoleAnon)); c != 401 {
		t.Fatalf("a key of another secret = %d, want 401", c)
	}

	// Change the secret: PostgREST follows and refuses what the old secret signed.
	const next = "second-secret-0123456789-abcdefghijklmnopqrstu"
	nk, err := n.Engine.SetJWTSecret(ctx, p.Ref, next)
	if err != nil {
		t.Fatal(err)
	}
	if c := rest(hostedAnon); c != 401 {
		t.Fatalf("the old anon key after the change = %d, want 401", c)
	}
	if c := rest(nk.AnonKey); c != 200 {
		t.Fatalf("the new anon key = %d, want 200", c)
	}
	if c := rest(signed(next, secrets.RoleAnon)); c != 200 {
		t.Fatalf("a key of another ref signed with the new secret = %d, want 200", c)
	}
	// GoTrue restarted too, on the new secret: its admin API takes a service_role key of it.
	admin := func(token string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/admin/users", ports.GoTrue), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := admin(nk.ServiceRoleKey); c != 200 {
		t.Fatalf("GoTrue admin with the new service_role key = %d, want 200", c)
	}
	if c := admin(signed(hosted, secrets.RoleServiceRole)); c == 200 {
		t.Fatal("GoTrue still accepts a service_role key of the old secret")
	}
	// The change survives a pause and a resume (the env files carry it).
	if err := n.Engine.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if err := n.Engine.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if c := rest(nk.AnonKey); c != 200 {
		t.Fatalf("after pause and resume the new anon key = %d, want 200", c)
	}
}

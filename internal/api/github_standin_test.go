package api

// TestServeGitHubStandIn serves the API of the test fixture (fake project manager, in-memory registry,
// the real branching and GitHub services) over HTTP, so that tests/linux/github/drive.mjs and
// fake-github.mjs can be run against it on a workstation, without Postgres or root:
//
//	tests/linux/github/selftest.sh
//
// SUPAVISE_GITHUB_STANDIN_FILE gets {api_url, pat, jwt, parent_ref, org_id}; delete it to stop.
// SUPAVISE_GITHUB_STANDIN_CONFIG is the [github] section as JSON (the keys of config.toml).

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/github"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

func TestServeGitHubStandIn(t *testing.T) {
	out, conf := os.Getenv("SUPAVISE_GITHUB_STANDIN_FILE"), os.Getenv("SUPAVISE_GITHUB_STANDIN_CONFIG")
	if out == "" || conf == "" {
		t.Skip("set SUPAVISE_GITHUB_STANDIN_FILE and SUPAVISE_GITHUB_STANDIN_CONFIG to serve the stand-in")
	}
	raw, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
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
	f := newFixture(t)
	f.cfg.GitHub = config.GitHub{AppID: in.AppID, AppSlug: in.AppSlug, ClientID: in.ClientID, ClientSecretFile: in.ClientSecretFile,
		PrivateKeyFile: in.PrivateKeyFile, WebhookSecretFile: in.WebhookSecretFile, APIURL: in.APIURL, WebURL: in.WebURL}
	svc, err := github.New(github.Options{Cfg: f.cfg, Registry: f.reg, Secrets: f.srv.sec, Branches: f.srv.branches})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Drain(context.Background())
	f.srv.gh = svc
	svc.SetHost(ghHost{f.srv})
	// The fixture's project is the parent; the fake manager has no SQL, so the databases of the stand-in are not checked.
	pat := secrets.NewPAT()
	if err := f.reg.CreateAccessToken(context.Background(), &registry.AccessToken{UserID: f.userID, Name: "standin", Hash: secrets.HashToken(pat), Prefix: pat[:8]}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: f.srv, ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln) //nolint:errcheck
	defer hs.Close()
	info, _ := json.Marshal(map[string]any{"api_url": "http://" + ln.Addr().String(), "pat": pat, "jwt": f.jwt, "parent_ref": testRef, "org_id": f.org.ID})
	if err := os.WriteFile(out, info, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(out); err != nil {
			return
		}
	}
}

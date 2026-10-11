package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	v2 "github.com/supavise/supavise/internal/api/gen/v2"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/github"
	"github.com/supavise/supavise/internal/registry"
)

// ghFake is the few calls of GitHub's API the dashboard's routes make.
type ghFake struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu         sync.Mutex
	conversion int
}

const (
	tRepoID   = int64(750123456) // does not fit a float32
	tInstID   = int64(49123456)
	tAppID    = int64(918273)
	tRepoName = "acme/app"
)

func newGHFake(t *testing.T) *ghFake {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &ghFake{key: key}
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "good-code" || r.Form.Get("client_secret") != "client-secret" {
			js(w, 200, map[string]string{"error": "bad_verification_code"})
			return
		}
		js(w, 200, map[string]any{"access_token": "ghu_user", "refresh_token": "ghr_user", "expires_in": 28800, "refresh_token_expires_in": 15811200})
	})
	user := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer ghu_user" {
				js(w, 401, map[string]string{"message": "Bad credentials"})
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /user", user(func(w http.ResponseWriter, r *http.Request) { js(w, 200, map[string]any{"id": 7, "login": "octo"}) }))
	mux.HandleFunc("GET /user/installations", user(func(w http.ResponseWriter, r *http.Request) {
		js(w, 200, map[string]any{"installations": []map[string]any{{"id": tInstID, "app_id": tAppID}}})
	}))
	mux.HandleFunc("GET /user/installations/{id}/repositories", user(func(w http.ResponseWriter, r *http.Request) {
		js(w, 200, map[string]any{"repositories": []map[string]any{{"id": tRepoID, "full_name": tRepoName, "default_branch": "main"}}})
	}))
	mux.HandleFunc("DELETE /applications/{c}/token", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		js(w, 201, map[string]any{"token": "inst", "expires_at": "2099-01-01T00:00:00Z"})
	})
	inst := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer inst" || r.PathValue("owner")+"/"+r.PathValue("repo") != tRepoName {
				js(w, 404, map[string]string{"message": "Not Found"})
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", inst(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("path") != "supabase/config.toml" {
			js(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		if r.URL.Query().Get("ref") == "broken" {
			js(w, 200, map[string]any{"type": "file", "sha": "bad", "size": 5, "encoding": "none", "content": ""})
			return
		}
		js(w, 200, map[string]any{"type": "file", "sha": "cfgsha", "size": 40, "encoding": "none", "content": ""})
	}))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", inst(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sha") == "bad" {
			_, _ = w.Write([]byte("this is = not [ toml"))
			return
		}
		_, _ = w.Write([]byte("project_id = \"app\"\n[api]\nmax_rows = 500\n[auth]\nsite_url = \"https://x.test\"\n"))
	}))
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches", inst(func(w http.ResponseWriter, r *http.Request) {
		js(w, 200, []map[string]string{{"name": "main"}, {"name": "dev"}})
	}))
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches/{b...}", inst(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("b") != "main" {
			js(w, 404, map[string]string{"message": "Branch not found"})
			return
		}
		js(w, 200, map[string]string{"name": "main"})
	}))
	mux.HandleFunc("POST /app-manifests/{code}/conversions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.conversion++
		f.mu.Unlock()
		if r.PathValue("code") != "manifest-code" {
			js(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		js(w, 201, map[string]any{"id": tAppID, "slug": "supavise-x", "name": "Supavise x", "client_id": "Iv1.x", "client_secret": "client-secret",
			"webhook_secret": "whsec", "pem": f.pem(), "html_url": "https://github.example/apps/supavise-x", "owner": map[string]string{"login": "acme"}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *ghFake) pem() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(f.key)}))
}

// withGitHub gives the fixture's server the GitHub integration. With operatorApp the App comes from
// config.toml (files in a temporary directory), else the server has none.
func withGitHub(t *testing.T, f *fixture, operatorApp bool) (*github.Service, *ghFake) {
	t.Helper()
	fake := newGHFake(t)
	if operatorApp {
		dir := t.TempDir()
		write := func(name, content string) string {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}
		f.cfg.GitHub = config.GitHub{AppID: tAppID, AppSlug: "supavise-test", ClientID: "Iv1.test", APIURL: fake.srv.URL, WebURL: fake.srv.URL,
			ClientSecretFile: write("secret", "client-secret"), PrivateKeyFile: write("key.pem", fake.pem()), WebhookSecretFile: write("hook", "whsec")}
	} else {
		f.cfg.GitHub = config.GitHub{APIURL: fake.srv.URL, WebURL: fake.srv.URL}
	}
	svc, err := github.New(github.Options{Cfg: f.cfg, Registry: f.reg, Secrets: f.srv.sec})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Drain(context.Background()) })
	f.srv.gh = svc
	svc.SetHost(ghHost{f.srv})
	return svc, fake
}

func profileDisabled(t *testing.T, f *fixture) []string {
	t.Helper()
	rec := f.do("GET", "/platform/profile", nil)
	if rec.Code != 200 {
		t.Fatalf("profile: %d %s", rec.Code, rec.Body)
	}
	var prof struct {
		Disabled []string `json:"disabled_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &prof); err != nil {
		t.Fatal(err)
	}
	return prof.Disabled
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func errMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Message
}

func TestGitHubWithoutAnAppAnswersTruthfully(t *testing.T) {
	f := newFixture(t)
	// No service at all, as on a node that wires none.
	check := func() {
		t.Helper()
		if !hasString(profileDisabled(t, f), featureGitHub) {
			t.Fatal("the profile does not hide the GitHub surfaces of a server without an App")
		}
		if rec := f.do("GET", "/platform/integrations/github/authorization", nil); rec.Code != 404 {
			t.Fatalf("authorization: %d %s, want 404 (the stub said everyone was connected)", rec.Code, rec.Body)
		}
		if rec := f.do("DELETE", "/platform/integrations/github/authorization", nil); rec.Code != 404 {
			t.Fatalf("delete authorization: %d, want 404: nothing to remove", rec.Code)
		}
		if rec := f.do("POST", "/platform/integrations/github/authorization", map[string]any{"code": "x", "state": "y"}); rec.Code != 400 {
			t.Fatalf("create authorization: %d %s", rec.Code, rec.Body)
		}
		rec := f.do("GET", "/platform/integrations/github/connections?organization_id="+itoa(f.org.ID), nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"connections":[]}` {
			t.Fatalf("connections: %d %s, want an empty list (the Branches page is blanked by an error here)", rec.Code, rec.Body)
		}
		validateAgainstSpec(t, "GET /platform/integrations/github/connections", rec.Body.Bytes())
		rec = f.do("POST", "/platform/integrations/github/connections", map[string]any{"project_ref": testRef, "installation_id": 1, "repository_id": 2})
		if rec.Code != 400 || !strings.Contains(errMessage(t, rec), "not set up") {
			t.Fatalf("create connection: %d %s, want a refusal that says why", rec.Code, rec.Body)
		}
		if rec := f.do("GET", "/platform/integrations/github/repositories", nil); rec.Code != 404 {
			t.Fatalf("repositories: %d", rec.Code)
		}
		if rec := f.do("GET", "/platform/integrations/github/repositories/5/branches", nil); rec.Code != 404 {
			t.Fatalf("branches: %d", rec.Code)
		}
		if rec := f.do("GET", "/platform/integrations/github/repositories/5/branches/main", nil); rec.Code != 404 {
			t.Fatalf("branch check: %d", rec.Code)
		}
		if rec := f.do("PATCH", "/platform/integrations/github/connections/1", map[string]any{"workdir": "x"}); rec.Code != 400 {
			t.Fatalf("update connection: %d", rec.Code)
		}
		if rec := f.do("DELETE", "/platform/integrations/github/connections/1", nil); rec.Code != 404 {
			t.Fatalf("delete connection: %d", rec.Code)
		}
		if rec := f.do("GET", "/platform/integrations/github/connections/1/config", nil); rec.Code != 404 {
			t.Fatalf("config: %d", rec.Code)
		}
		rec = f.do("GET", "/v2/organizations/default/integrations/github/connections", nil)
		if rec.Code != 200 {
			t.Fatalf("v2 connections: %d %s", rec.Code, rec.Body)
		}
		validateAgainstSpec(t, "GET /v2/organizations/{slug}/integrations/github/connections", rec.Body.Bytes())
	}
	check()
	if rec := f.do("GET", "/supavise/v1/github/app", nil); rec.Code != 404 {
		t.Fatalf("app status with no service: %d", rec.Code)
	}
	if rec := f.do("POST", "/supavise/v1/github/app", map[string]any{}); rec.Code != 404 {
		t.Fatalf("app create with no service: %d", rec.Code)
	}
	if rec := f.do("DELETE", "/supavise/v1/github/app", nil); rec.Code != 404 {
		t.Fatalf("app delete with no service: %d", rec.Code)
	}
	// A service with no App on it answers the same, and says so when asked about the App.
	withGitHub(t, f, false)
	check()
	if rec := f.do("GET", "/supavise/v1/github/app", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("app status with no App: %d %s", rec.Code, rec.Body)
	}
}

func TestGitHubWithAnAppOffersTheSurfaces(t *testing.T) {
	f := newFixture(t)
	withGitHub(t, f, true)
	if hasString(profileDisabled(t, f), featureGitHub) {
		t.Fatal("the profile hides GitHub on a server that has an App")
	}
	// The key hides nothing else.
	if !hasString(profileDisabled(t, f), "integrations:vercel") {
		t.Fatal("integrations:vercel is not hidden any more")
	}
}

func TestGitHubAuthorizationConnectionsAndRepositories(t *testing.T) {
	f := newFixture(t)
	_, _ = withGitHub(t, f, true)
	if rec := f.do("GET", "/platform/integrations/github/authorization", nil); rec.Code != 404 {
		t.Fatalf("authorization before the user authorized: %d", rec.Code)
	}
	if rec := f.do("GET", "/platform/integrations/github/repositories", nil); rec.Code != 403 {
		t.Fatalf("repositories before the user authorized: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/platform/integrations/github/authorization", map[string]any{"code": "wrong"}); rec.Code != 400 {
		t.Fatalf("a bad code: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/platform/integrations/github/authorization", map[string]any{"code": "good-code", "state": "ignored"}); rec.Code != 201 {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
	}
	rec := f.do("GET", "/platform/integrations/github/authorization", nil)
	if rec.Code != 200 {
		t.Fatalf("authorization: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/integrations/github/authorization", rec.Body.Bytes())
	var auth map[string]float64
	_ = json.Unmarshal(rec.Body.Bytes(), &auth)
	if auth["sender_id"] != 7 || auth["id"] == 0 {
		t.Fatalf("authorization = %v", auth)
	}

	rec = f.do("GET", "/platform/integrations/github/repositories", nil)
	if rec.Code != 200 {
		t.Fatalf("repositories: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/integrations/github/repositories", rec.Body.Bytes())
	var repos struct {
		Partial bool `json:"partial_response_due_to_sso"`
		Repos   []struct {
			ID             json.Number `json:"id"`
			Name           string      `json:"name"`
			InstallationID json.Number `json:"installation_id"`
			DefaultBranch  string      `json:"default_branch"`
		} `json:"repositories"`
	}
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.UseNumber()
	if err := dec.Decode(&repos); err != nil || len(repos.Repos) != 1 {
		t.Fatalf("repositories = %s (%v)", rec.Body, err)
	}
	if repos.Repos[0].ID.String() != "750123456" || repos.Repos[0].InstallationID.String() != "49123456" || repos.Repos[0].Name != tRepoName {
		t.Fatalf("the ids of GitHub were changed on the way: %+v", repos.Repos[0])
	}

	// Branch check and list.
	if rec := f.do("GET", "/platform/integrations/github/repositories/750123456/branches/main", nil); rec.Code != 200 {
		t.Fatalf("branch check: %d %s", rec.Code, rec.Body)
	} else {
		validateAgainstSpec(t, "GET /platform/integrations/github/repositories/{repository_id}/branches/{branch_name}", rec.Body.Bytes())
	}
	if rec := f.do("GET", "/platform/integrations/github/repositories/750123456/branches/nope", nil); rec.Code != 404 {
		t.Fatalf("a branch that does not exist: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/platform/integrations/github/repositories/750123456/branches", nil); rec.Code != 200 {
		t.Fatalf("branches: %d %s", rec.Code, rec.Body)
	} else {
		validateAgainstSpec(t, "GET /platform/integrations/github/repositories/{repository_id}/branches", rec.Body.Bytes())
	}
	if rec := f.do("GET", "/platform/integrations/github/repositories/999/branches", nil); rec.Code != 404 {
		t.Fatalf("a repository the user cannot reach: %d", rec.Code)
	}

	// Connect: a repository the user cannot reach, then the real one.
	bad := map[string]any{"project_ref": testRef, "installation_id": tInstID, "repository_id": 5}
	if rec := f.do("POST", "/platform/integrations/github/connections", bad); rec.Code != 404 {
		t.Fatalf("connect a repository the user cannot reach: %d %s", rec.Code, rec.Body)
	}
	body := map[string]any{"project_ref": testRef, "installation_id": tInstID, "repository_id": tRepoID, "workdir": "./apps/web/", "branch_limit": 3, "new_branch_per_pr": true, "supabase_changes_only": true}
	rec = f.do("POST", "/platform/integrations/github/connections", body)
	if rec.Code != 201 {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /platform/integrations/github/connections", rec.Body.Bytes())
	var created struct {
		ID      int64  `json:"id"`
		Workdir string `json:"workdir"`
		Limit   int    `json:"branch_limit"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Workdir != "apps/web" || created.Limit != 3 {
		t.Fatalf("created = %+v", created)
	}
	if rec := f.do("POST", "/platform/integrations/github/connections", body); rec.Code != 409 {
		t.Fatalf("a second connection of the project: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/platform/integrations/github/connections", map[string]any{"project_ref": "bbbbbbbbbbbbbbbbbbbb", "installation_id": 1, "repository_id": 2}); rec.Code != 404 {
		t.Fatalf("an unknown project: %d", rec.Code)
	}

	list := f.do("GET", "/platform/integrations/github/connections?organization_id="+itoa(f.org.ID), nil)
	if list.Code != 200 {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
	validateAgainstSpec(t, "GET /platform/integrations/github/connections", list.Body.Bytes())
	var conns struct {
		Connections []struct {
			ID         int64 `json:"id"`
			Repository struct {
				ID   json.Number `json:"id"`
				Name string      `json:"name"`
			} `json:"repository"`
			Project struct {
				Ref string `json:"ref"`
			} `json:"project"`
			User *struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"connections"`
	}
	dec = json.NewDecoder(strings.NewReader(list.Body.String()))
	dec.UseNumber()
	if err := dec.Decode(&conns); err != nil || len(conns.Connections) != 1 {
		t.Fatalf("connections = %s", list.Body)
	}
	c := conns.Connections[0]
	if c.Repository.ID.String() != "750123456" || c.Repository.Name != tRepoName || c.Project.Ref != testRef || c.User == nil || c.User.Username == "" {
		t.Fatalf("connection = %+v", c)
	}
	if rec := f.do("GET", "/platform/integrations/github/connections?organization_id="+itoa(f.org.ID)+"&project_ref=bbbbbbbbbbbbbbbbbbbb", nil); !strings.Contains(rec.Body.String(), `"connections":[]`) {
		t.Fatalf("filtered by project: %s", rec.Body)
	}
	v2rec := f.do("GET", "/v2/organizations/default/integrations/github/connections", nil)
	validateAgainstSpec(t, "GET /v2/organizations/{slug}/integrations/github/connections", v2rec.Body.Bytes())
	if !strings.Contains(v2rec.Body.String(), `"type":"`+string(v2.GithubConnection)+`"`) || !strings.Contains(v2rec.Body.String(), "750123456") {
		t.Fatalf("v2 list = %s", v2rec.Body)
	}

	// Update, config, delete.
	path := "/platform/integrations/github/connections/" + itoa(created.ID)
	if rec := f.do("PATCH", path, map[string]any{"branch_limit": 9, "supabase_changes_only": false, "workdir": ""}); rec.Code != 204 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("PATCH", path, map[string]any{"branch_limit": 0}); rec.Code != 400 {
		t.Fatalf("a branch limit of 0: %d", rec.Code)
	}
	if rec := f.do("PATCH", path, map[string]any{"workdir": "../escape"}); rec.Code != 400 {
		t.Fatalf("a working directory that leaves the repository: %d", rec.Code)
	}
	cfgRec := f.do("GET", path+"/config", nil)
	if cfgRec.Code != 200 {
		t.Fatalf("config: %d %s", cfgRec.Code, cfgRec.Body)
	}
	validateAgainstSpec(t, "GET /platform/integrations/github/connections/{connection_id}/config", cfgRec.Body.Bytes())
	var cfg struct {
		Path   string         `json:"path"`
		Ref    *string        `json:"ref"`
		SHA    string         `json:"sha"`
		Config map[string]any `json:"config"`
	}
	_ = json.Unmarshal(cfgRec.Body.Bytes(), &cfg)
	if cfg.Path != "supabase/config.toml" || cfg.Ref != nil || cfg.SHA != "cfgsha" || cfg.Config["project_id"] != "app" {
		t.Fatalf("config = %+v", cfg)
	}
	if rec := f.do("GET", path+"/config?ref=broken", nil); rec.Code != 422 {
		t.Fatalf("a config.toml that is not TOML: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/platform/integrations/github/connections/9999/config", nil); rec.Code != 404 {
		t.Fatalf("config of an unknown connection: %d", rec.Code)
	}
	if rec := f.do("DELETE", path, nil); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("DELETE", path, nil); rec.Code != 404 {
		t.Fatalf("delete again: %d", rec.Code)
	}

	// Deauthorizing removes the token; the answer is then 404 again.
	if rec := f.do("DELETE", "/platform/integrations/github/authorization", nil); rec.Code != 200 {
		t.Fatalf("deauthorize: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/platform/integrations/github/authorization", nil); rec.Code != 404 {
		t.Fatalf("authorization after removing it: %d", rec.Code)
	}
}

func TestGitHubConnectionsAreForOwnersAndAdministrators(t *testing.T) {
	rf := newRolesFixture(t)
	f := rf.fixture
	svc, _ := withGitHub(t, f, true)
	ctx := context.Background()
	for _, id := range rf.ids {
		// Every user has authorized, so only the role decides.
		if _, err := svc.Authorize(ctx, id, "good-code"); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]any{"project_ref": testRef, "installation_id": tInstID, "repository_id": tRepoID}
	for _, role := range []string{"dev", "ro", "scoped", "stranger"} {
		if rec := rf.as(role, "POST", "/platform/integrations/github/connections", body); rec.Code != 403 {
			t.Errorf("%s connected a repository: %d %s", role, rec.Code, rec.Body)
		}
	}
	if rec := rf.as("admin", "POST", "/platform/integrations/github/connections", body); rec.Code != 201 {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	conn, err := f.reg.GetGitHubConnectionByProject(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	path := "/platform/integrations/github/connections/" + itoa(conn.ID)
	for role, want := range map[string]int{"owner": 200, "admin": 200, "dev": 200, "ro": 200, "scoped": 200, "stranger": 404} {
		if rec := rf.as(role, "GET", path+"/config", nil); rec.Code != want {
			t.Errorf("%s reads the config: %d %s, want %d", role, rec.Code, rec.Body, want)
		}
	}
	for _, role := range []string{"dev", "ro", "scoped"} {
		if rec := rf.as(role, "PATCH", path, map[string]any{"workdir": "x"}); rec.Code != 403 {
			t.Errorf("%s changed the connection: %d", role, rec.Code)
		}
		if rec := rf.as(role, "DELETE", path, nil); rec.Code != 403 {
			t.Errorf("%s deleted the connection: %d", role, rec.Code)
		}
	}
	if rec := rf.as("stranger", "DELETE", path, nil); rec.Code != 404 {
		t.Errorf("a stranger learned the connection exists: %d", rec.Code)
	}
	orgID := itoa(f.org.ID)
	for role, want := range map[string]int{"owner": 1, "admin": 1, "dev": 1, "ro": 1, "scoped": 1} {
		rec := rf.as(role, "GET", "/platform/integrations/github/connections?organization_id="+orgID, nil)
		var out struct {
			Connections []any `json:"connections"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != 200 || len(out.Connections) != want {
			t.Errorf("%s lists %d connections (%d), want %d", role, len(out.Connections), rec.Code, want)
		}
	}
	if rec := rf.as("stranger", "GET", "/platform/integrations/github/connections?organization_id="+orgID, nil); rec.Code != 403 {
		t.Errorf("a stranger listed the connections: %d", rec.Code)
	}
	if rec := rf.as("owner", "DELETE", path, nil); rec.Code != 204 {
		t.Fatalf("owner deletes: %d", rec.Code)
	}
}

func TestGitHubAuthorizationIsPerUser(t *testing.T) {
	rf := newRolesFixture(t)
	f := rf.fixture
	withGitHub(t, f, true)
	if rec := rf.as("owner", "POST", "/platform/integrations/github/authorization", map[string]any{"code": "good-code"}); rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := rf.as("owner", "GET", "/platform/integrations/github/authorization", nil); rec.Code != 200 {
		t.Fatalf("owner: %d", rec.Code)
	}
	if rec := rf.as("dev", "GET", "/platform/integrations/github/authorization", nil); rec.Code != 404 {
		t.Fatalf("another user is authorized by the owner's token: %d", rec.Code)
	}
	if rec := rf.as("dev", "GET", "/platform/integrations/github/repositories", nil); rec.Code != 403 {
		t.Fatalf("another user reaches the owner's repositories: %d", rec.Code)
	}
}

func TestGitHubManifestFlow(t *testing.T) {
	f := newFixture(t)
	svc, fake := withGitHub(t, f, false)
	var refreshed int
	svc.SetOnAppChange(func(context.Context) error { refreshed++; return nil })

	rec := f.do("GET", "/supavise/v1/github/app", nil)
	var st github.Status
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if rec.Code != 200 || st.Configured || !strings.HasSuffix(st.WebhookURL, "/hooks/github") {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}

	rec = f.do("POST", "/supavise/v1/github/app", map[string]any{"owner": "acme"})
	var begin struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &begin)
	if rec.Code != 200 || !strings.Contains(begin.URL, github.ManifestPath+"?state=") {
		t.Fatalf("begin: %d %s", rec.Code, rec.Body)
	}
	state := begin.URL[strings.Index(begin.URL, "state=")+6:]

	// The page a browser opens: a form to GitHub, with the manifest, and a CSP that allows nowhere else.
	page := httptest.NewRecorder()
	req := httptest.NewRequest("GET", github.ManifestPath+"?state="+state, nil)
	calledRoutes.Store("GET "+github.ManifestPath, true)
	f.srv.ServeHTTP(page, req)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, fake.srv.URL+"/organizations/acme/settings/apps/new?state="+state) ||
		!strings.Contains(body, "&#34;redirect_url&#34;:&#34;") || !strings.Contains(body, "/hooks/github") {
		t.Fatalf("manifest page: %d %s", page.Code, body)
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action "+fake.srv.URL) || !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
	// Anyone with the page URL can look at the page, but a wrong state shows nothing.
	bad := httptest.NewRecorder()
	f.srv.ServeHTTP(bad, httptest.NewRequest("GET", github.ManifestPath+"?state=nope", nil))
	if bad.Code != 404 || strings.Contains(bad.Body.String(), "manifest") && strings.Contains(bad.Body.String(), "redirect_url") {
		t.Fatalf("a wrong state: %d", bad.Code)
	}

	// GitHub sends the browser back with a code.
	calledRoutes.Store("GET "+github.ManifestCallbackPath, true)
	cb := func(code, state string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		f.srv.ServeHTTP(w, httptest.NewRequest("GET", github.ManifestCallbackPath+"?code="+code+"&state="+state, nil))
		return w
	}
	if w := cb("manifest-code", "wrongstate"); w.Code != 400 {
		t.Fatalf("callback with a wrong state: %d", w.Code)
	}
	if fake.conversion != 0 {
		t.Fatal("GitHub was asked to convert a code with a wrong state")
	}
	w := cb("manifest-code", state)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/apps/supavise-x/installations/new") {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	if refreshed != 1 {
		t.Fatalf("Studio was told %d times", refreshed)
	}
	if w := cb("manifest-code", state); w.Code != 400 {
		t.Fatalf("a used state: %d", w.Code)
	}

	// The secrets are in the registry sealed, never in the clear.
	row, err := f.reg.GetGitHubApp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, sealed := range map[string][]byte{"client secret": row.SealedClientSecret, "webhook secret": row.SealedWebhookSecret, "private key": row.SealedPrivateKey} {
		if len(sealed) == 0 || strings.Contains(string(sealed), "client-secret") || strings.Contains(string(sealed), "whsec") || strings.Contains(string(sealed), "PRIVATE KEY") {
			t.Fatalf("the %s is not sealed: %q", name, sealed)
		}
	}
	if hasString(profileDisabled(t, f), featureGitHub) {
		t.Fatal("the profile still hides GitHub after the App was created")
	}
	rec = f.do("GET", "/supavise/v1/github/app", nil)
	st = github.Status{}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Configured || st.Slug != "supavise-x" || st.Source != "manifest" || !strings.Contains(st.InstallURL, "/apps/supavise-x/installations/new") {
		t.Fatalf("status = %+v", st)
	}
	if strings.Contains(rec.Body.String(), "client-secret") || strings.Contains(rec.Body.String(), "whsec") {
		t.Fatalf("the status leaks a secret: %s", rec.Body)
	}
	// A second App is refused unless it replaces the first.
	if rec := f.do("POST", "/supavise/v1/github/app", map[string]any{}); rec.Code != 409 {
		t.Fatalf("a second App: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/supavise/v1/github/app", map[string]any{"replace": true}); rec.Code != 200 {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/supavise/v1/github/app", map[string]any{"owner": "bad owner!"}); rec.Code != 400 {
		t.Fatalf("a bad owner: %d", rec.Code)
	}
	if rec := f.do("DELETE", "/supavise/v1/github/app", nil); rec.Code != 204 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("DELETE", "/supavise/v1/github/app", nil); rec.Code != 404 {
		t.Fatalf("remove again: %d", rec.Code)
	}
	if !hasString(profileDisabled(t, f), featureGitHub) {
		t.Fatal("the profile shows GitHub after the App was removed")
	}
	if refreshed != 2 {
		t.Fatalf("Studio was told %d times", refreshed)
	}
}

func TestGitHubWebhookRoute(t *testing.T) {
	f := newFixture(t)
	// No service: not found, and no credentials needed to find out.
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/github", strings.NewReader("{}")))
	calledRoutes.Store("POST /hooks/github", true)
	if w.Code != 404 {
		t.Fatalf("no service: %d", w.Code)
	}
	withGitHub(t, f, true)
	post := func(sig string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/hooks/github", strings.NewReader(`{"zen":"x"}`))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-GitHub-Delivery", "abc-"+sig)
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(""); rec.Code != 401 {
		t.Fatalf("unsigned: %d %s", rec.Code, rec.Body)
	}
	if rec := post("sha256=00"); rec.Code != 401 {
		t.Fatalf("badly signed: %d", rec.Code)
	}
	body := `{"zen":"x"}`
	good := signHook("whsec", body)
	if rec := post(good); rec.Code != 200 {
		t.Fatalf("signed: %d %s", rec.Code, rec.Body)
	}
	// An oversized body is refused before it is read in full.
	big := httptest.NewRequest("POST", "/hooks/github", strings.NewReader(strings.Repeat("x", github.MaxWebhookBody+10)))
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, big)
	if rec.Code != 413 {
		t.Fatalf("a delivery over the limit: %d", rec.Code)
	}
}

func TestGitHubBranchesCarryTheirPullRequestNumber(t *testing.T) {
	f := newFixture(t)
	svc, _ := withGitHub(t, f, true)
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "feature", "git_branch": "feature"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if _, has := created["pr_number"]; has {
		t.Fatalf("a branch with no pull request has pr_number: %v", created)
	}
	ref := created["project_ref"].(string)
	if err := f.reg.PutGitHubBranchLink(context.Background(), &registry.GitHubBranchLink{BranchRef: ref, ProjectRef: testRef, RepositoryID: 1, RepositoryName: "a/b", InstallationID: 2, PRNumber: 17, HeadRef: "feature"}); err != nil {
		t.Fatal(err)
	}
	_ = svc
	f.waitBranch(t, "feature", "FUNCTIONS_DEPLOYED")
	rec = f.do("GET", "/v1/projects/"+testRef+"/branches/feature", nil)
	var got struct {
		PR *int `json:"pr_number"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.PR == nil || *got.PR != 17 {
		t.Fatalf("pr_number = %v in %s", got.PR, rec.Body)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/branches/{name}", rec.Body.Bytes())
	rec = f.do("GET", "/v1/projects/"+testRef+"/branches", nil)
	if !strings.Contains(rec.Body.String(), `"pr_number":17`) {
		t.Fatalf("the list lacks pr_number: %s", rec.Body)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/branches", rec.Body.Bytes())
}

func TestPushOnTheProductionRowNeedsAConnection(t *testing.T) {
	f := newFixture(t)
	withGitHub(t, f, true)
	// Without a connection the production row answers as it did: the default branch cannot be pushed.
	rec := f.do("POST", "/v1/branches/"+testRef+"/push", map[string]any{})
	if rec.Code != 400 {
		t.Fatalf("push of the default branch: %d %s", rec.Code, rec.Body)
	}
}

// The ids of GitHub fit float32 only by luck, so the bodies are hand-made for them; this keeps their
// fields equal to what the generated types (which follow the spec) name.
func TestGitHubBodiesKeepTheSpecsFields(t *testing.T) {
	keys := func(v any) []string {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		var k []string
		for key := range m {
			k = append(k, key)
		}
		sort.Strings(k)
		return k
	}
	// Optional fields (omitempty in the generated type) are filled so that they show.
	yes, n, s := true, float32(1), "x"
	gen := plat.CreateGitHubConnectionBody{BranchLimit: &n, NewBranchPerPr: &yes, SupabaseChangesOnly: &yes, Workdir: &s}
	mine := ghConnectionBody{Workdir: &s, SupabaseChangesOnly: &yes}
	l := float64(1)
	mine.BranchLimit, mine.NewBranchPerPR = &l, &yes
	if !reflect.DeepEqual(keys(gen), keys(mine)) {
		t.Errorf("create body: spec %v, ours %v", keys(gen), keys(mine))
	}
	if g, m := keys(plat.GitHubAuthorizationResponseOutput{}), keys(ghAuthorizationOut{}); !reflect.DeepEqual(g, m) {
		t.Errorf("authorization: spec %v, ours %v", g, m)
	}
	if g, m := keys(plat.CreateGitHubConnectionResponseOutput{}), keys(ghConnectionCreated{}); !reflect.DeepEqual(g, m) {
		t.Errorf("created connection: spec %v, ours %v", g, m)
	}
	var list plat.ListGitHubConnectionsResponseOutput
	list.Connections = append(list.Connections, struct {
		BranchLimit    float32 `json:"branch_limit"`
		Id             float32 `json:"id"`
		InsertedAt     string  `json:"inserted_at"`
		InstallationId float32 `json:"installation_id"`
		NewBranchPerPr bool    `json:"new_branch_per_pr"`
		Project        struct {
			Id   float32 `json:"id"`
			Name string  `json:"name"`
			Ref  string  `json:"ref"`
		} `json:"project"`
		Repository struct {
			Id   float32 `json:"id"`
			Name string  `json:"name"`
		} `json:"repository"`
		SupabaseChangesOnly bool   `json:"supabase_changes_only"`
		UpdatedAt           string `json:"updated_at"`
		User                *struct {
			Id           float32 `json:"id"`
			PrimaryEmail *string `json:"primary_email"`
			Username     string  `json:"username"`
		} `json:"user"`
		Workdir string `json:"workdir"`
	}{})
	b, _ := json.Marshal(list.Connections[0])
	var gm map[string]any
	_ = json.Unmarshal(b, &gm)
	var gk []string
	for k := range gm {
		gk = append(gk, k)
	}
	sort.Strings(gk)
	if mk := keys(ghConnectionOut{}); !reflect.DeepEqual(gk, mk) {
		t.Errorf("connection: spec %v, ours %v", gk, mk)
	}
	var repos plat.ListGitHubRepositoriesResponseOutput
	repos.Repositories = append(repos.Repositories, struct {
		DefaultBranch  string  `json:"default_branch"`
		Id             float32 `json:"id"`
		InstallationId float32 `json:"installation_id"`
		Name           string  `json:"name"`
	}{})
	if g, m := keys(repos.Repositories[0]), keys(ghRepositoryOut{}); !reflect.DeepEqual(g, m) {
		t.Errorf("repository: spec %v, ours %v", g, m)
	}
}

func TestGitHubRoleMatrix(t *testing.T) {
	rf := newRolesFixture(t)
	withGitHub(t, rf.fixture, false)
	orgID := itoa(rf.org.ID)
	// owner admin dev ro scoped stranger
	for _, c := range []routeCase{
		rc("YYYYYY", "GET", "/platform/integrations/github/authorization", nil),
		rc("YYYYYY", "GET", "/platform/integrations/github/repositories", nil),
		rc("YYYYYN", "GET", "/platform/integrations/github/connections?organization_id="+orgID, nil),
		rc("YYNNNN", "POST", "/platform/integrations/github/connections", map[string]any{"project_ref": testRef, "installation_id": 1, "repository_id": 2}),
		rc("YYYYYN", "GET", "/v2/organizations/default/integrations/github/connections", nil),
		rc("YYNNNN", "GET", "/supavise/v1/github/app", nil),
		rc("YNNNNN", "POST", "/supavise/v1/github/app", map[string]any{}),
		rd("YNNNNN", "DELETE", "/supavise/v1/github/app", nil),
	} {
		for i, role := range roleNames {
			rec := rf.as(role, c.method, c.path, c.body)
			denied := rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized
			if want := c.want[i] == 'Y'; denied == want {
				t.Errorf("%s as %s: %d %s, want allowed=%v", c, role, rec.Code, rec.Body, want)
			}
		}
	}
}

func signHook(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

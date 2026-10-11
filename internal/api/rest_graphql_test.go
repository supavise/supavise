package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
)

// fakePostgREST stands in for a project's PostgREST: the OpenAPI document at / and graphql() at /rpc/graphql.
type fakePostgREST struct {
	*httptest.Server
	mu   sync.Mutex
	seen []postgrestCall
	// status, when not zero, is what every call gets (a PostgREST that has not loaded its schema cache answers 503).
	status int
}

type postgrestCall struct {
	method, path string
	header       http.Header
	body         string
}

func (f *fakePostgREST) last() postgrestCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[len(f.seen)-1]
}

const swaggerDoc = `{"swagger":"2.0","definitions":{"todos":{"properties":{"id":{"type":"integer"},"task":{"type":"string"}}}},"paths":{"/todos":{},"/rpc/add_them":{}}}`

// withPostgREST makes the fixture's project reach a fake PostgREST.
func (f *fixture) withPostgREST(t *testing.T) *fakePostgREST {
	t.Helper()
	pg := &fakePostgREST{}
	pg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		pg.mu.Lock()
		pg.seen = append(pg.seen, postgrestCall{r.Method, r.URL.Path, r.Header.Clone(), string(b)})
		status := pg.status
		pg.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"message":"schema cache is loading"}`, status)
			return
		}
		switch {
		case r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			w.Header().Set("Content-Type", "application/openapi+json; charset=utf-8")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(swaggerDoc))
			}
		case r.URL.Path == "/rpc/graphql" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"data":{"__typename":"Query"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(pg.Close)
	prev := f.srv.upstreamOverride
	f.srv.upstreamOverride = func(p *registry.Project, svc string) string {
		if svc == upPostgREST && p.Ref == testRef {
			return pg.URL
		}
		return prev(p, svc)
	}
	return pg
}

// projectToken signs a token with the project's JWT secret (what the role-impersonation plugin of GraphiQL does).
func (f *fixture) projectToken(t *testing.T, secret string, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(method, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *fixture) projectKeys(t *testing.T) (service, secret string) {
	t.Helper()
	k, err := f.mgr.Keys(context.Background(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	return k.ServiceRoleKey, k.JWTSecret
}

// A stub answered the documents route with nothing, so Studio's Docs tab and the API docs panel threw on
// "definitions" and listed no table or function.
func TestAPIRestReturnsTheOpenAPIDocument(t *testing.T) {
	f := newFixture(t)
	pg := f.withPostgREST(t)
	service, _ := f.projectKeys(t)
	path := "/platform/projects/" + testRef + "/api/rest"

	rec := f.do("GET", path, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Error("served by the stub")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	var doc struct {
		Definitions map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"definitions"`
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Definitions["todos"].Properties) != 2 || doc.Paths["/rpc/add_them"] == nil {
		t.Errorf("document %s: want the tables as definitions and the functions as /rpc paths", rec.Body)
	}
	c := pg.last()
	if c.method != "GET" || c.path != "/" || c.header.Get("Authorization") != "Bearer "+service || c.header.Get("Accept") != "application/openapi+json" {
		t.Errorf("PostgREST got %+v; want GET / with the service key and the OpenAPI media type", c)
	}
}

// The readiness ping of Studio is a HEAD: 200 while PostgREST answers, 503 when it cannot, so that a project
// whose PostgREST is down is not shown as ready.
func TestAPIRestHeadIsTheReadinessPing(t *testing.T) {
	f := newFixture(t)
	pg := f.withPostgREST(t)
	path := "/platform/projects/" + testRef + "/api/rest"

	rec := f.do("HEAD", path, nil)
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD with PostgREST up: %d %q", rec.Code, rec.Body)
	}
	if c := pg.last(); c.method != "HEAD" {
		t.Errorf("the ping reached PostgREST as %s, want HEAD", c.method)
	}

	pg.mu.Lock()
	pg.status = http.StatusServiceUnavailable
	pg.mu.Unlock()
	for _, method := range []string{"HEAD", "GET"} {
		if rec := f.do(method, path, nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s while PostgREST is loading its schema cache: %d, want 503", method, rec.Code)
		}
	}

	pg.Close() // nothing listens any more
	for _, method := range []string{"HEAD", "GET"} {
		rec := f.do(method, path, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s with PostgREST down: %d, want 503", method, rec.Code)
		}
		if method == "GET" && !strings.Contains(rec.Body.String(), "PostgREST is not running") {
			t.Errorf("message %s", rec.Body)
		}
	}
}

func TestAPIRestOfAProjectThatIsNotThere(t *testing.T) {
	f := newFixture(t)
	f.withPostgREST(t)
	if rec := f.do("GET", "/platform/projects/bbbbbbbbbbbbbbbbbbbb/api/rest", nil); rec.Code != 404 {
		t.Errorf("unknown project: %d", rec.Code)
	}
	_ = f.reg.SetProjectStatus(t.Context(), testRef, registry.StatusInactive)
	if rec := f.do("HEAD", "/platform/projects/"+testRef+"/api/rest", nil); rec.Code != http.StatusConflict {
		t.Errorf("paused project: %d, want 409 (not ready)", rec.Code)
	}
}

// GraphiQL sends the dashboard user's own token in x-graphql-authorization when no role is impersonated; the
// project's PostgREST would refuse it, so the call runs as service_role. A token signed with the project's
// JWT secret (the impersonation plugin's) is forwarded.
func TestAPIGraphQLForwardsOnlyATokenOfTheProject(t *testing.T) {
	f := newFixture(t)
	pg := f.withPostgREST(t)
	service, secret := f.projectKeys(t)
	path := "/platform/projects/" + testRef + "/api/graphql"
	query := `{"query":"{ __typename }","operationName":null,"variables":{}}`
	exp := time.Now().Add(time.Hour).Unix()
	good := f.projectToken(t, secret, jwt.SigningMethodHS256, jwt.MapClaims{"role": "authenticated", "sub": f.userID, "exp": exp})

	cases := []struct {
		name, header, want string
	}{
		{"no header: service_role", "", service},
		{"the dashboard user's own token", "Bearer " + f.jwt, service},
		{"a token of the project", "Bearer " + good, good},
		{"a token of the project, bearer in other case", "bearer " + good, good},
		{"an expired token of the project", "Bearer " + f.projectToken(t, secret, jwt.SigningMethodHS256, jwt.MapClaims{"role": "authenticated", "exp": time.Now().Add(-time.Hour).Unix()}), service},
		{"a token without an expiry", "Bearer " + f.projectToken(t, secret, jwt.SigningMethodHS256, jwt.MapClaims{"role": "service_role"}), service},
		{"another algorithm", "Bearer " + f.projectToken(t, secret, jwt.SigningMethodHS512, jwt.MapClaims{"role": "service_role", "exp": exp}), service},
		{"not a token", "Bearer garbage", service},
		{"not a bearer", "Basic " + good, service},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var headers []string
			if tc.header != "" {
				headers = []string{"x-graphql-authorization", tc.header}
			}
			// A client-chosen profile must not reach PostgREST.
			headers = append(headers, "Content-Profile", "public")
			rec := f.do("POST", path, query, headers...)
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if rec.Header().Get("X-Supavise-Stub") != "" {
				t.Error("served by the stub")
			}
			if !strings.Contains(rec.Body.String(), `"__typename":"Query"`) {
				t.Errorf("body %s", rec.Body)
			}
			c := pg.last()
			if c.method != "POST" || c.path != "/rpc/graphql" || c.body != query {
				t.Errorf("PostgREST got %s %s %q; want the query posted to /rpc/graphql", c.method, c.path, c.body)
			}
			if got := c.header.Get("Authorization"); got != "Bearer "+tc.want {
				t.Errorf("PostgREST ran as %q, want %q", got, "Bearer "+tc.want)
			}
			if got := c.header.Get("Content-Profile"); got != "graphql_public" {
				t.Errorf("Content-Profile %q, want graphql_public whatever the client says", got)
			}
		})
	}

	// A PostgREST that answers an error passes its status on, and one that is down is a 503.
	pg.mu.Lock()
	pg.status = http.StatusBadRequest
	pg.mu.Unlock()
	if rec := f.do("POST", path, query); rec.Code != http.StatusBadRequest {
		t.Errorf("PostgREST's 400: %d", rec.Code)
	}
	pg.Close()
	if rec := f.do("POST", path, query); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("PostgREST down: %d, want 503", rec.Code)
	}
}

// The permission of the GraphQL call is a database write, as in authz.go; a read-only member cannot run it.
func TestAPIGraphQLNeedsDatabaseWritePermission(t *testing.T) {
	f := newFixture(t)
	f.withPostgREST(t)
	readOnly := "22222222-3333-4444-8555-666666666666"
	f.addMember(readOnly, members.RoleReadOnly)
	tok := f.signJWT(map[string]any{"sub": readOnly, "email": "ro@example.test", "role": "authenticated"})
	if rec := f.doAs(tok, "POST", "/platform/projects/"+testRef+"/api/graphql", `{"query":"{ __typename }"}`); rec.Code != http.StatusForbidden {
		t.Errorf("read-only member: %d, want 403", rec.Code)
	}
	if rec := f.doAs(tok, "GET", "/platform/projects/"+testRef+"/api/rest", nil); rec.Code != 200 {
		t.Errorf("read-only member reads the documents: %d, want 200", rec.Code)
	}
}

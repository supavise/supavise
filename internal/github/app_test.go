package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// manifestHarness is a service with no App, whose GitHub is the fake.
func manifestHarness(t *testing.T) (*Service, *fakeGitHub, *registry.Memory) {
	t.Helper()
	gh := newFakeGitHub(t)
	cfg := config.Default()
	cfg.Domain = "example.test"
	cfg.TLS.Mode = "off"
	cfg.GitHub = config.GitHub{APIURL: gh.URL(), WebURL: gh.URL()}
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.NewMemory()
	svc, err := New(Options{Cfg: cfg, Registry: reg, Secrets: sec})
	if err != nil {
		t.Fatal(err)
	}
	return svc, gh, reg
}

func stateOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("link %q has no state", link)
	}
	return u.Query().Get("state")
}

func TestManifestFlowStoresTheAppSealed(t *testing.T) {
	svc, gh, reg := manifestHarness(t)
	ctx := context.Background()
	if svc.Configured(ctx) {
		t.Fatal("configured before anything")
	}
	if _, err := svc.App(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("App = %v", err)
	}
	link, err := svc.BeginManifest(ctx, ManifestRequest{Owner: "acme", Name: "My Supavise", CreatedBy: "cli"})
	if err != nil || !strings.HasPrefix(link, "http://api.example.test"+ManifestPath+"?state=") {
		t.Fatalf("link = %q, %v", link, err)
	}
	state := stateOf(t, link)
	action, manifest, err := svc.ManifestForm(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(action, gh.URL()+"/organizations/acme/settings/apps/new?state="+state) {
		t.Fatalf("action = %q", action)
	}
	m := string(manifest)
	for _, want := range []string{
		`"name":"My Supavise"`, `"url":"http://studio.example.test"`, `"redirect_url":"http://api.example.test` + ManifestCallbackPath + `"`,
		`"hook_attributes":{"active":true,"url":"http://api.example.test/hooks/github"}`, `"callback_urls":["http://studio.example.test/integrations/github/authorize"]`,
		`"setup_url":"http://studio.example.test/integrations/github/authorize"`, `"public":false`,
		`"pull_requests":"write"`, `"checks":"write"`, `"contents":"read"`, `"issues":"write"`, `"metadata":"read"`,
		`"default_events":["pull_request","push","create","delete"]`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("the manifest lacks %s:\n%s", want, m)
		}
	}
	if strings.Contains(m, "request_oauth_on_install") {
		t.Error("the manifest asks for an OAuth step at install, which Studio's landing page does not expect")
	}
	if _, _, err := svc.ManifestForm(ctx, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown state: %v", err)
	}
	// A wrong state converts nothing.
	if _, err := svc.CompleteManifest(ctx, "good-manifest-code", "wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong state: %v", err)
	}
	if gh.callsTo("POST /app-manifests/") != 0 {
		t.Fatal("GitHub was asked to convert a code for a wrong state")
	}
	var told int
	svc.SetOnAppChange(func(context.Context) error { told++; return nil })
	app, err := svc.CompleteManifest(ctx, "good-manifest-code", state)
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != gh.appID || app.Slug != "supavise-test" || app.ClientID != "Iv1.test" || app.Source != SourceManifest || app.Key == nil {
		t.Fatalf("app = %+v", app)
	}
	if told != 1 {
		t.Fatalf("told %d times", told)
	}
	// Single use.
	if _, err := svc.CompleteManifest(ctx, "good-manifest-code", state); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a used state: %v", err)
	}
	row, err := reg.GetGitHubApp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, sealed := range map[string][]byte{"client secret": row.SealedClientSecret, "webhook secret": row.SealedWebhookSecret, "private key": row.SealedPrivateKey} {
		if len(sealed) == 0 || strings.Contains(string(sealed), gh.clientSecret) || strings.Contains(string(sealed), gh.webhookSecret) || strings.Contains(string(sealed), "PRIVATE KEY") {
			t.Errorf("the %s is not sealed", name)
		}
	}
	// Reading the App back opens them; the key signs a JWT GitHub accepts.
	got, err := svc.App(ctx)
	if err != nil || got.ClientSecret != gh.clientSecret || got.WebhookSecret != gh.webhookSecret {
		t.Fatalf("App = %+v, %v", got, err)
	}
	if _, err := svc.installationToken(ctx, gh.instID); err != nil {
		t.Fatalf("the stored App cannot get an installation token: %v", err)
	}
	st, _ := svc.Status(ctx)
	if !st.Configured || st.Slug != "supavise-test" || st.Owner != "acme" || !strings.HasSuffix(st.InstallURL, "/apps/supavise-test/installations/new") {
		t.Fatalf("status = %+v", st)
	}
	// A second App needs replace.
	if _, err := svc.BeginManifest(ctx, ManifestRequest{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second App: %v", err)
	}
	if _, err := svc.BeginManifest(ctx, ManifestRequest{Replace: true}); err != nil {
		t.Fatal(err)
	}
	if had, err := svc.RemoveApp(ctx); err != nil || !had {
		t.Fatalf("remove: %v, %v", had, err)
	}
	if svc.Configured(ctx) || told != 2 {
		t.Fatalf("configured=%v told=%d", svc.Configured(ctx), told)
	}
}

// The code GitHub sends back with the browser is in the path of the conversion and works as a
// credential until it is used: no error of the conversion repeats it, whether GitHub answered or the
// connection failed, because the callback logs the error.
func TestAFailedConversionDoesNotPutTheCodeInItsError(t *testing.T) {
	const code = "c0de-0f-the-manifest-flow-1234567890"
	for name, setup := range map[string]func(*Service, *fakeGitHub){
		"a bad gateway": func(_ *Service, gh *fakeGitHub) { gh.convertStatus = http.StatusBadGateway },
		"a 422":         func(_ *Service, gh *fakeGitHub) { gh.convertStatus = http.StatusUnprocessableEntity },
		"no connection": func(svc *Service, _ *fakeGitHub) { svc.cl.API = "http://127.0.0.1:1" },
		"a timeout": func(svc *Service, gh *fakeGitHub) {
			slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
			t.Cleanup(slow.Close)
			svc.cl.API = slow.URL
			svc.cl.HTTP = &http.Client{Timeout: 100 * time.Millisecond}
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, gh, _ := manifestHarness(t)
			ctx := context.Background()
			state := stateOf(t, mustLink(t, svc))
			setup(svc, gh)
			_, err := svc.CompleteManifest(ctx, code, state)
			if err == nil {
				t.Fatal("the conversion did not fail")
			}
			if strings.Contains(err.Error(), code) || strings.Contains(err.Error(), url.PathEscape(code)) {
				t.Fatalf("the error repeats the code: %v", err)
			}
			if !strings.Contains(err.Error(), "/app-manifests/{code}/conversions") {
				t.Fatalf("the error does not say which call failed: %v", err)
			}
			// What a caller can still tell: GitHub's status.
			if gh.convertStatus != 0 && !IsStatus(err, gh.convertStatus) {
				t.Fatalf("the status was lost: %v", err)
			}
			// Printing the wrapped errors one by one repeats nothing either.
			for e := error(err); e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), code) {
					t.Fatalf("a wrapped error repeats the code: %v", e)
				}
			}
		})
	}
}

func mustLink(t *testing.T, svc *Service) string {
	t.Helper()
	link, err := svc.BeginManifest(context.Background(), ManifestRequest{Owner: "acme", CreatedBy: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func TestManifestRequestsAreChecked(t *testing.T) {
	svc, _, _ := manifestHarness(t)
	ctx := context.Background()
	for _, bad := range []ManifestRequest{{Owner: "bad owner"}, {Owner: "-x"}, {Owner: strings.Repeat("a", 40)}, {Name: strings.Repeat("n", 35)}} {
		if _, err := svc.BeginManifest(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	// A state expires.
	link, _ := svc.BeginManifest(ctx, ManifestRequest{})
	state := stateOf(t, link)
	svc.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, _, err := svc.ManifestForm(ctx, state); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired state: %v", err)
	}
}

func TestAnAppFromConfigWinsAndCannotBeReplaced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app, err := h.svc.App(ctx)
	if err != nil || app.Source != SourceConfig || app.Slug != "supavise-test" || app.ClientSecret != h.gh.clientSecret || app.WebhookSecret != h.gh.webhookSecret {
		t.Fatalf("app = %+v, %v", app, err)
	}
	if _, err := h.svc.BeginManifest(ctx, ManifestRequest{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a manifest flow next to an operator App: %v", err)
	}
	if _, err := h.svc.RemoveApp(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("removing an operator App: %v", err)
	}
	slug, client, ok := h.svc.Names(ctx)
	if !ok || slug != "supavise-test" || client != "Iv1.test" {
		t.Fatalf("names = %q %q %v", slug, client, ok)
	}
}

func TestAnUnreadableOperatorAppIsNotConfigured(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.GitHub.PrivateKeyFile = "/nonexistent/key.pem" })
	ctx := context.Background()
	if h.svc.Configured(ctx) {
		t.Fatal("an App whose key cannot be read counts as configured")
	}
	st, _ := h.svc.Status(ctx)
	if st.Configured || st.Problem == "" || st.AppID == 0 {
		t.Fatalf("status = %+v", st)
	}
	if res := h.svc.HandleWebhook(ctx, Delivery{Event: "ping", ID: "x", Signature: "sha256=00"}); res.Status != 503 {
		t.Fatalf("a webhook with an unreadable App: %+v", res)
	}
}

func TestJWTIsShortLivedAndSignedByTheAppKey(t *testing.T) {
	h := newHarness(t)
	app, _ := h.svc.App(context.Background())
	now := time.Now()
	tok, err := app.JWT(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", tok)
	}
	// GitHub refuses a token valid for more than ten minutes.
	claims := decodeClaims(t, parts[1])
	if exp, iat := int64(claims["exp"].(float64)), int64(claims["iat"].(float64)); exp-iat > 600 || exp <= now.Unix() {
		t.Fatalf("iat %d exp %d now %d", iat, exp, now.Unix())
	}
	if claims["iss"] != "77" {
		t.Fatalf("iss = %v", claims["iss"])
	}
}

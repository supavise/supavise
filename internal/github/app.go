package github

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// App is the server's GitHub App with its secrets open. Never log or marshal it.
type App struct {
	ID         int64
	Slug       string // github.com/apps/<Slug>
	Name       string
	ClientID   string
	OwnerLogin string
	HTMLURL    string
	// Source is "config" for an App named in [github] and "manifest" for one the manifest flow made.
	Source string

	ClientSecret  string
	WebhookSecret string
	Key           *rsa.PrivateKey
}

// Where an App comes from.
const (
	SourceConfig   = "config"
	SourceManifest = "manifest"
)

// JWT is the token the App authenticates itself with: RS256, valid for under ten minutes, as
// GitHub requires. now is the clock; GitHub wants iat a little in the past for clock drift.
func (a *App) JWT(now time.Time) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(a.ID, 10),
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(a.Key)
}

// InstallURL is the page where an account installs the App.
func (a *App) InstallURL(web string) string {
	return strings.TrimRight(web, "/") + "/apps/" + url.PathEscape(a.Slug) + "/installations/new"
}

// ParsePrivateKey reads the RSA private key GitHub issues, PKCS#1 or PKCS#8 in PEM.
func ParsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("github: the private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, errors.New("github: the private key is neither PKCS#1 nor PKCS#8")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: the private key is not an RSA key")
	}
	return rk, nil
}

// cachedFile is an operator file read once and again when it changes.
type cachedFile struct {
	mod  time.Time
	size int64
	data []byte
}

// readFile reads path, keeping the content until the file's modification time or size changes.
func (s *Service) readFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if c, ok := s.files[path]; ok && c.mod.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.data, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o007 != 0 {
		s.log.Warn("a GitHub App secret file can be read by every user of this machine; make it mode 0600", "path", path)
	}
	s.files[path] = cachedFile{mod: fi.ModTime(), size: fi.Size(), data: b}
	return b, nil
}

// App returns the server's App: the one [github] names, else the one the manifest flow stored.
// ErrNotConfigured when there is none. The profile asks for it on every page the dashboard opens, so
// the parsed key is kept while the files or the sealed rows stay as they are.
func (s *Service) App(ctx context.Context) (*App, error) {
	if g := s.cfg.GitHub; g.OperatorApp() {
		return s.operatorApp()
	}
	row, err := s.st.GetGitHubApp(ctx)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	for _, b := range [][]byte{row.SealedClientSecret, row.SealedWebhookSecret, row.SealedPrivateKey} {
		h.Write(b)
		h.Write([]byte{0})
	}
	fmt.Fprintf(h, "%d %s %s %s", row.AppID, row.Slug, row.ClientID, row.Name)
	sig := string(h.Sum(nil))
	if a := s.cachedApp(sig); a != nil {
		return a, nil
	}
	a := &App{ID: row.AppID, Slug: row.Slug, Name: row.Name, ClientID: row.ClientID, OwnerLogin: row.OwnerLogin, HTMLURL: row.HTMLURL, Source: SourceManifest}
	open := func(sealed []byte) ([]byte, error) { return s.sec.Open(sealed) }
	cs, err := open(row.SealedClientSecret)
	if err != nil {
		return nil, fmt.Errorf("github: open the App's client secret: %w", err)
	}
	ws, err := open(row.SealedWebhookSecret)
	if err != nil {
		return nil, fmt.Errorf("github: open the App's webhook secret: %w", err)
	}
	pk, err := open(row.SealedPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("github: open the App's private key: %w", err)
	}
	if a.Key, err = ParsePrivateKey(pk); err != nil {
		return nil, err
	}
	a.ClientSecret, a.WebhookSecret = string(cs), string(ws)
	s.keepApp(sig, a)
	return a, nil
}

func (s *Service) cachedApp(sig string) *App {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if s.appSig == sig && s.app != nil {
		return s.app
	}
	return nil
}

func (s *Service) keepApp(sig string, a *App) {
	s.filesMu.Lock()
	s.appSig, s.app = sig, a
	s.filesMu.Unlock()
}

func (s *Service) operatorApp() (*App, error) {
	g := s.cfg.GitHub
	// The files are re-read when one of them changes: their size and modification time are the signature.
	var sig strings.Builder
	for _, path := range []string{g.ClientSecretFile, g.PrivateKeyFile, g.WebhookSecretFile} {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("github: %w", err)
		}
		fmt.Fprintf(&sig, "%s %d %d|", path, fi.Size(), fi.ModTime().UnixNano())
	}
	if a := s.cachedApp(sig.String()); a != nil {
		return a, nil
	}
	secret := func(name, path string) (string, error) {
		b, err := s.readFile(path)
		if err != nil {
			return "", fmt.Errorf("github: read %s: %w", name, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("github: %s (%s) is empty", name, path)
		}
		return v, nil
	}
	a := &App{ID: g.AppID, Slug: g.AppSlug, Name: g.AppSlug, ClientID: g.ClientID, Source: SourceConfig, HTMLURL: g.Web() + "/apps/" + g.AppSlug}
	var err error
	if a.ClientSecret, err = secret("github.client_secret_file", g.ClientSecretFile); err != nil {
		return nil, err
	}
	if a.WebhookSecret, err = secret("github.webhook_secret_file", g.WebhookSecretFile); err != nil {
		return nil, err
	}
	pk, err := s.readFile(g.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("github: read github.private_key_file: %w", err)
	}
	if a.Key, err = ParsePrivateKey(pk); err != nil {
		return nil, err
	}
	s.keepApp(sig.String(), a)
	return a, nil
}

// Configured reports whether the server has a GitHub App whose secrets can be read. It is what
// the dashboard's GitHub surfaces depend on.
func (s *Service) Configured(ctx context.Context) bool {
	if s == nil {
		return false
	}
	_, err := s.App(ctx)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		// The profile asks on every page: say it once a minute.
		s.filesMu.Lock()
		due := s.now().Sub(s.warned) >= time.Minute
		if due {
			s.warned = s.now()
		}
		s.filesMu.Unlock()
		if due {
			s.log.Warn("the GitHub App is configured but cannot be used", "err", err)
		}
	}
	return err == nil
}

// Names returns the App's name in github.com/apps/<name> and its client id, which Studio's
// authorize and install windows are built from. ok is false when the server has no App.
func (s *Service) Names(ctx context.Context) (slug, clientID string, ok bool) {
	a, err := s.App(ctx)
	if err != nil {
		return "", "", false
	}
	return a.Slug, a.ClientID, true
}

// Permissions and events the App asks for. Contents are read to build a branch, the pull request
// scopes and the checks to report on it. GitHub always delivers the installation events, so they
// are not subscribed to.
var (
	AppPermissions = map[string]string{
		"contents":      "read",
		"metadata":      "read",
		"pull_requests": "write",
		"checks":        "write",
		"issues":        "write",
	}
	AppEvents = []string{"pull_request", "push", "create", "delete"}
)

// Status describes the App for an operator.
type Status struct {
	Configured  bool   `json:"configured"`
	Source      string `json:"source,omitempty"`
	AppID       int64  `json:"app_id,omitempty"`
	Slug        string `json:"slug,omitempty"`
	Name        string `json:"name,omitempty"`
	ClientID    string `json:"client_id,omitempty"`
	Owner       string `json:"owner,omitempty"`
	HTMLURL     string `json:"html_url,omitempty"`
	InstallURL  string `json:"install_url,omitempty"`
	WebhookURL  string `json:"webhook_url"`
	CallbackURL string `json:"callback_url"`
	// Problem is why a configured App cannot be used (an unreadable key file), "" otherwise.
	Problem string `json:"problem,omitempty"`
}

// WebhookURL is where GitHub must send the App's webhooks.
func (s *Service) WebhookURL() string { return s.cfg.APIURL() + "/hooks/github" }

// CallbackURL is the page of the dashboard that GitHub sends users to after they authorize the
// App and after they install it.
func (s *Service) CallbackURL() string {
	return s.cfg.DashboardURL() + "/integrations/github/authorize"
}

// Status returns the App's description without its secrets.
func (s *Service) Status(ctx context.Context) (*Status, error) {
	st := &Status{WebhookURL: s.WebhookURL(), CallbackURL: s.CallbackURL()}
	a, err := s.App(ctx)
	switch {
	case errors.Is(err, ErrNotConfigured):
		return st, nil
	case err != nil:
		st.Configured, st.Problem = false, err.Error()
		if s.cfg.GitHub.OperatorApp() {
			g := s.cfg.GitHub
			st.Source, st.AppID, st.Slug, st.ClientID = SourceConfig, g.AppID, g.AppSlug, g.ClientID
		}
		return st, nil
	}
	st.Configured, st.Source, st.AppID, st.Slug, st.Name, st.ClientID = true, a.Source, a.ID, a.Slug, a.Name, a.ClientID
	st.Owner, st.HTMLURL, st.InstallURL = a.OwnerLogin, a.HTMLURL, a.InstallURL(s.cfg.GitHub.Web())
	return st, nil
}

// ---- the manifest flow --------------------------------------------------------------------

// manifestTTL is how long a manifest link stays good.
const manifestTTL = 30 * time.Minute

// ManifestRequest asks for a new App.
type ManifestRequest struct {
	// Owner is the GitHub organization that will own the App; empty means the account of whoever
	// is signed in to GitHub in the browser that follows the link.
	Owner string
	// Name is the App's name (at most 34 characters, unique on GitHub); empty means "Supavise <domain>".
	Name string
	// CreatedBy says who asked (a user id, or "cli").
	CreatedBy string
	// Replace allows a new App where one was stored; the stored one and the users' tokens go.
	Replace bool
}

type manifestParams struct {
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name,omitempty"`
}

func validOwner(o string) bool {
	if o == "" {
		return true
	}
	if len(o) > 39 {
		return false
	}
	for i, r := range o {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(o)-1:
		default:
			return false
		}
	}
	return true
}

// ManifestPath is the path, on the Management API, of the page that sends the manifest to GitHub.
const ManifestPath = "/supavise/v1/github/app/new"

// ManifestCallbackPath is where GitHub sends the browser back with the code.
const ManifestCallbackPath = "/supavise/v1/github/app/callback"

func hashState(state string) []byte {
	h := sha256.Sum256([]byte(state))
	return h[:]
}

// BeginManifest prepares the creation of the App and returns the link the operator opens in a
// browser that is signed in to GitHub. The link is good once and for half an hour.
func (s *Service) BeginManifest(ctx context.Context, r ManifestRequest) (string, error) {
	if s.cfg.GitHub.OperatorApp() {
		return "", fmt.Errorf("%w: config.toml names an App ([github] app_id %d); remove it from the file to create one here", ErrConflict, s.cfg.GitHub.AppID)
	}
	if !validOwner(r.Owner) {
		return "", invalid("%q is not a GitHub account name", r.Owner)
	}
	if len(r.Name) > 34 {
		return "", invalid("an App's name is at most 34 characters")
	}
	if _, err := s.st.GetGitHubApp(ctx); err == nil && !r.Replace {
		return "", fmt.Errorf("%w: this server already has a GitHub App; remove it first", ErrConflict)
	} else if err != nil && !errors.Is(err, registry.ErrNotFound) {
		return "", err
	}
	state := secrets.RandomString(40, "abcdefghijklmnopqrstuvwxyz0123456789")
	params, _ := json.Marshal(manifestParams{Owner: r.Owner, Name: r.Name})
	if err := s.st.PutGitHubState(ctx, hashState(state), string(params), r.CreatedBy, s.now().Add(manifestTTL)); err != nil {
		return "", err
	}
	return s.cfg.APIURL() + ManifestPath + "?state=" + state, nil
}

// manifestJSON is the manifest GitHub turns into an App.
func (s *Service) manifestJSON(p manifestParams) ([]byte, error) {
	name := p.Name
	if name == "" {
		name = "Supavise " + s.cfg.BaseDomain()
		if len(name) > 34 {
			name = name[:34]
		}
		name = strings.TrimRight(name, " .-")
	}
	m := map[string]any{
		"name":                name,
		"url":                 s.cfg.DashboardURL(),
		"hook_attributes":     map[string]any{"url": s.WebhookURL(), "active": true},
		"redirect_url":        s.cfg.APIURL() + ManifestCallbackPath,
		"callback_urls":       []string{s.CallbackURL()},
		"setup_url":           s.CallbackURL(),
		"setup_on_update":     true,
		"public":              false,
		"default_events":      AppEvents,
		"default_permissions": AppPermissions,
	}
	return json.Marshal(m)
}

// ManifestForm returns what the page that posts the manifest to GitHub needs: the URL to post
// it to, the manifest and the state. ErrNotFound when the state is unknown or has expired.
func (s *Service) ManifestForm(ctx context.Context, state string) (action string, manifest []byte, err error) {
	params, ok, err := s.st.PeekGitHubState(ctx, hashState(state), s.now())
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, ErrNotFound
	}
	var p manifestParams
	_ = json.Unmarshal([]byte(params), &p)
	if manifest, err = s.manifestJSON(p); err != nil {
		return "", nil, err
	}
	web := s.cfg.GitHub.Web()
	if p.Owner != "" {
		action = web + "/organizations/" + url.PathEscape(p.Owner) + "/settings/apps/new"
	} else {
		action = web + "/settings/apps/new"
	}
	return action + "?state=" + url.QueryEscape(state), manifest, nil
}

// conversion is GitHub's answer to the code of a manifest flow.
type conversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	HTMLURL       string `json:"html_url"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// CompleteManifest exchanges the code GitHub sent back for the App's credentials and stores them
// sealed. The state must be one BeginManifest made, and is good once.
func (s *Service) CompleteManifest(ctx context.Context, code, state string) (*App, error) {
	if s.cfg.GitHub.OperatorApp() {
		return nil, fmt.Errorf("%w: config.toml names an App", ErrConflict)
	}
	if code == "" || state == "" {
		return nil, invalid("the code and the state are required")
	}
	_, ok, err := s.st.TakeGitHubState(ctx, hashState(state), s.now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	var cv conversion
	if err := s.cl.do(ctx, "", "POST", "/app-manifests/"+url.PathEscape(code)+"/conversions", nil, &cv); err != nil {
		// The code is in the path and works as a credential until it is used or an hour has passed: it
		// is in the error of GitHub's answer and in the text of a failed connection, and the callback logs the error.
		return nil, fmt.Errorf("GitHub did not accept the code: %w", scrub(err, code))
	}
	if cv.ID == 0 || cv.Slug == "" || cv.ClientID == "" || cv.ClientSecret == "" || cv.WebhookSecret == "" || cv.PEM == "" {
		return nil, errors.New("github: the manifest conversion did not return the App's credentials")
	}
	if _, err := ParsePrivateKey([]byte(cv.PEM)); err != nil {
		return nil, err
	}
	seal := func(v string) ([]byte, error) { return s.sec.Seal([]byte(v)) }
	row := &registry.GitHubApp{AppID: cv.ID, Slug: cv.Slug, Name: cv.Name, ClientID: cv.ClientID, OwnerLogin: cv.Owner.Login, HTMLURL: cv.HTMLURL}
	if row.SealedClientSecret, err = seal(cv.ClientSecret); err != nil {
		return nil, err
	}
	if row.SealedWebhookSecret, err = seal(cv.WebhookSecret); err != nil {
		return nil, err
	}
	if row.SealedPrivateKey, err = seal(cv.PEM); err != nil {
		return nil, err
	}
	if err := s.st.PutGitHubApp(ctx, row); err != nil {
		return nil, err
	}
	s.dropTokens()
	s.auditEvent(ctx, "github.app.created", map[string]any{"app_id": cv.ID, "slug": cv.Slug, "owner": cv.Owner.Login})
	a, err := s.App(ctx)
	if err != nil {
		return nil, err
	}
	s.changed(ctx)
	return a, nil
}

// RemoveApp forgets the stored App (the App itself stays on GitHub), with the users' tokens. It
// reports whether there was one. An App named in config.toml is removed by editing the file.
func (s *Service) RemoveApp(ctx context.Context) (bool, error) {
	if s.cfg.GitHub.OperatorApp() {
		return false, fmt.Errorf("%w: config.toml names the App; remove [github] app_id from the file", ErrConflict)
	}
	had, err := s.st.DeleteGitHubApp(ctx)
	if err != nil {
		return false, err
	}
	s.dropTokens()
	if had {
		s.auditEvent(ctx, "github.app.removed", map[string]any{})
		s.changed(ctx)
	}
	return had, nil
}

// changed tells the node that the App appeared or went, so that Studio restarts with its names.
func (s *Service) changed(ctx context.Context) {
	if s.onAppChange == nil {
		return
	}
	if err := s.onAppChange(context.WithoutCancel(ctx)); err != nil {
		s.log.Warn("could not restart the dashboard after the GitHub App changed; restart supavise-studio for its GitHub buttons to follow", "err", err)
	}
}

// scrub returns err without secret in any text it prints or in the fields of the errors it wraps:
// the path of an *APIError, the URL of a *url.Error, or the message of any other error. An error
// that does not contain the secret comes back as it is.
func scrub(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	enc := url.PathEscape(secret)
	repl := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, enc, "{code}"), secret, "{code}")
	}
	var ae *APIError
	if errors.As(err, &ae) {
		c := *ae
		c.Path, c.Message = repl(c.Path), repl(c.Message)
		return &c
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		c := *ue
		c.URL = repl(c.URL)
		return &c
	}
	if msg := err.Error(); repl(msg) != msg {
		return &scrubbed{msg: repl(msg), err: err}
	}
	return err
}

// scrubbed prints a message with a secret taken out and still answers errors.Is and As for the error
// it replaces.
type scrubbed struct {
	msg string
	err error
}

func (e *scrubbed) Error() string { return e.msg }
func (e *scrubbed) Unwrap() error { return e.err }

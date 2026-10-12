// Package github connects projects to GitHub repositories, the way hosted Supabase's GitHub
// integration does, with an App that belongs to this server's operator.
//
// A server has one GitHub App. It comes from GitHub's manifest flow (BeginManifest, then the
// browser visits the page the call returns and GitHub sends it back to CompleteManifest), or from
// the operator's own App named in [github] of config.toml. The package never refers to another
// party's App: with none configured every call answers ErrNotConfigured and the dashboard hides
// its GitHub surfaces.
//
// What the App is used for:
//
//   - A Supavise user authorizes it (OAuth), which gives the server a token that sees what that
//     user sees: the repositories they may connect, and nothing else.
//   - A project is connected to one repository (registry.GitHubConnection). The App's installation
//     token reads the repository's supabase/ directory and reports checks and comments.
//   - GitHub sends webhooks to api.<domain>/hooks/github. A pull request opened, updated or closed
//     creates, updates or deletes a preview branch built from the repository (runner.go); a push to
//     the production branch deploys to production.
//
// The flow's secrets (the App's private key, client and webhook secrets, the users' tokens) are
// sealed with the node's master key before they reach the registry and are never logged.
package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

var (
	// ErrNotConfigured means the server has no GitHub App.
	ErrNotConfigured = errors.New("github: this server has no GitHub App")
	// ErrNotAuthorized means the user has not authorized the App, or GitHub no longer accepts their token.
	ErrNotAuthorized = errors.New("github: the user has not authorized the GitHub App")
	// ErrInvalid marks a request the caller can fix.
	ErrInvalid = errors.New("github: invalid request")
	// ErrNotFound marks a repository, branch or connection that does not exist or is not visible to the caller.
	ErrNotFound = errors.New("github: not found")
	// ErrConflict marks a request that clashes with the current state.
	ErrConflict = errors.New("github: conflict")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Branches is the part of branching.Service the integration drives. *branching.Service satisfies it.
type Branches interface {
	Create(ctx context.Context, ref string, in branching.CreateInput) (*branching.Branch, error)
	Delete(ctx context.Context, idOrRef string, o branching.DeleteOptions) (*branching.Branch, error)
	Resolve(ctx context.Context, idOrRef string) (*branching.Branch, error)
	List(ctx context.Context, ref string) ([]branching.Branch, error)
	WaitIdle(ctx context.Context, ref string) (*branching.Branch, error)
	Drive(ctx context.Context, idOrRef, op string, fn branching.Driver) (string, error)
	// WakeForDrive starts a branch the node paused for idleness and returns once it runs; Drive refuses one
	// that does not. A branch its owner paused, a quarantined one and one scheduled for deletion are refused.
	WakeForDrive(ctx context.Context, idOrRef string) (*branching.Branch, error)
	StartRun(ctx context.Context, op, branchRef, ownerRef, parentRef string) (*branching.Run, error)
	GetRun(ctx context.Context, ownerRef, id string) (*registry.BranchRun, error)
	Database() branching.Database
}

// FunctionFile is one file of an Edge Function's source.
type FunctionFile struct {
	// Path is relative to the supabase/ directory of the repository (functions/hello/index.ts).
	Path    string
	Content []byte
}

// FunctionDeploy is an Edge Function as the repository holds it.
type FunctionDeploy struct {
	Slug string
	// Entrypoint and ImportMap are paths among Files (ImportMap may be empty).
	Entrypoint, ImportMap string
	VerifyJWT             bool
	StaticPatterns        []string
	Files                 []FunctionFile
}

// Host is what the node gives the integration besides the branching service: the parts of the
// Management API that save settings and deploy functions.
type Host interface {
	// DeployFunction stores the function in project ref, bundling its sources where the node does.
	DeployFunction(ctx context.Context, ref string, f FunctionDeploy) error
	// FunctionSecret returns one Edge Function secret of project ref (what env(NAME) in config.toml
	// reads); ok is false when it is not set.
	FunctionSecret(ctx context.Context, ref, name string) (value string, ok bool, err error)
	// SaveSettings validates, saves and applies settings of a service of project ref, as a PATCH of
	// its config does, and returns the names of the settings that changed.
	SaveSettings(ctx context.Context, ref string, svc projectconfig.Service, patch map[string]any) (changed []string, err error)
	// SettingSaved reports whether project ref has a value saved for one setting of a service. A
	// secret counts when one is sealed; the value itself is never returned.
	SettingSaved(ctx context.Context, ref string, svc projectconfig.Service, key string) (bool, error)
}

// Options are the collaborators of Service.
type Options struct {
	Cfg      *config.Config
	Registry registry.Registry
	Secrets  secrets.Secrets
	// Branches and Host drive the branches. Without them webhooks are accepted and nothing is built.
	Branches Branches
	Host     Host
	// Audit records an event of the integration (the App created or removed, a user authorized).
	Audit func(ctx context.Context, kind string, payload map[string]any)
	// OnAppChange is told after the App was created or removed, so that Studio can restart with the
	// App's name and client id.
	OnAppChange func(ctx context.Context) error
	Log         *slog.Logger
	HTTP        *http.Client
	Now         func() time.Time
}

// Service is the GitHub integration.
type Service struct {
	cfg  *config.Config
	reg  registry.Registry
	st   registry.GitHubStore
	sec  secrets.Secrets
	br   Branches
	host Host
	log  *slog.Logger
	now  func() time.Time
	cl   *Client

	audit       func(ctx context.Context, kind string, payload map[string]any)
	onAppChange func(ctx context.Context) error

	// files caches the operator's App files by modification time (app.go).
	filesMu sync.Mutex
	files   map[string]cachedFile
	// warned is when an unusable App was last reported (app.go).
	warned time.Time
	// app is the parsed App, kept while appSig (what it was made from) stays the same.
	app    *App
	appSig string

	// tokens caches installation tokens (client_app.go).
	tokMu  sync.Mutex
	tokens map[int64]instToken

	// q serializes and supersedes the work on one branch or pull request (queue.go).
	q queue
	// wg tracks the work webhooks start, so that a shutdown can wait for it.
	wg   sync.WaitGroup
	base context.Context
	stop context.CancelFunc

	// builds admits the builds of preview branches, a few at a time (pr.go).
	builds chan struct{}

	// repoLists keeps each user's last list of repositories for a few seconds (repos.go).
	repoMu    sync.Mutex
	repoLists map[string]repoList

	// limiter admits webhooks (webhook.go).
	limit, badLimit rateLimiter
	// prunedAt is when old deliveries were last removed.
	pruneMu  sync.Mutex
	prunedAt time.Time
}

// New builds the service. It connects to nothing.
func New(o Options) (*Service, error) {
	if o.Cfg == nil || o.Registry == nil || o.Secrets == nil {
		return nil, errors.New("github: Options needs Cfg, Registry and Secrets")
	}
	st := registry.GitHubOf(o.Registry)
	if st == nil {
		return nil, errors.New("github: this registry keeps no GitHub state")
	}
	s := &Service{
		cfg: o.Cfg, reg: o.Registry, st: st, sec: o.Secrets, br: o.Branches, host: o.Host, log: o.Log, now: o.Now,
		audit: o.Audit, onAppChange: o.OnAppChange, files: map[string]cachedFile{}, tokens: map[int64]instToken{},
		builds: make(chan struct{}, o.Cfg.GitHub.Builds()),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.cl = &Client{HTTP: o.HTTP, API: o.Cfg.GitHub.API(), Web: o.Cfg.GitHub.Web(), UserAgent: "supavise"}
	if s.cl.HTTP == nil {
		s.cl.HTTP = &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	s.base, s.stop = context.WithCancel(context.Background())
	s.limit.init(s.now, hookRate, hookBurst)
	s.badLimit.init(s.now, badRate, badBurst)
	return s, nil
}

// SetHost gives the service the parts of the node it saves settings and deploys functions with. The
// Management API does it when it starts, before any webhook can arrive.
func (s *Service) SetHost(h Host) { s.host = h }

// SetBranches gives the service the branching service when it did not exist at New.
func (s *Service) SetBranches(b Branches) { s.br = b }

// SetOnAppChange sets what is told when the App appears or goes.
func (s *Service) SetOnAppChange(f func(ctx context.Context) error) { s.onAppChange = f }

// Drain waits for the work that webhooks started. When ctx ends first the work is cancelled.
func (s *Service) Drain(ctx context.Context) {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.stop()
		<-done
	}
}

func (s *Service) auditEvent(ctx context.Context, kind string, payload map[string]any) {
	if s.audit != nil {
		s.audit(ctx, kind, payload)
	}
}

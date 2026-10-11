// Package branching gives AI agents disposable databases: a branch is a project with a
// parent, so it has its own cluster, keys, host and fleet tenants (docs/design.md section 9a).
//
// The Management API's branch endpoints (internal/api) are thin wrappers over Service.
// Long operations (create, merge, reset, push) run detached from the request that started
// them; their progress is the branch's State and Detail, visible through Get and List.
package branching

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// Engine is the part of lifecycle.Engine the service drives. *lifecycle.Engine satisfies it.
type Engine interface {
	lifecycle.Manager
	DeleteWith(ctx context.Context, ref string, o lifecycle.DeleteOptions) error
}

// Deps are the collaborators of Service.
type Deps struct {
	Cfg      *config.Config
	Registry registry.Registry
	Secrets  secrets.Secrets
	Engine   Engine
	// Backup serves the base-backup path of with_data branches and the cleanup of an
	// ephemeral branch's archive; it must have its Manager set (SetManager) or not be used
	// for restores of its own. Nil: neither exists, and with_data needs a filesystem that
	// supports copy-on-write clones.
	Backup *backup.Service
	// DB runs SQL in project databases. Nil connects with pgx through Engine.ConnString.
	DB Database
	// Functions reads the Edge Function deployments of a project. When it is also a FunctionCarrier
	// (internal/api.BranchFunctions is), a new branch gets the parent's functions, a push brings the
	// parent's new ones and a merge deploys the branch's. Nil: functions are neither copied nor merged.
	Functions  FunctionSource
	Log        *slog.Logger
	Now        func() time.Time
	HTTPClient *http.Client // notify_url calls; nil builds a client that refuses private addresses
	// CreateWait bounds how long Create waits for the branch's row to exist before it
	// returns the branch in CREATING_PROJECT (default 10 seconds).
	CreateWait time.Duration
	// OpTimeout bounds a detached operation (default 1 hour).
	OpTimeout time.Duration
	// StaleAfter is how long a branch may sit in a busy state with no process working on it
	// before the sweeper marks the operation failed (default 30 minutes).
	StaleAfter time.Duration
}

// FunctionSource reads the Edge Function deployments stored for a project. Digests maps function
// slug to a digest of the deployed function: its settings and source files. A FunctionSource that is
// also a FunctionCarrier (carry.go) is what copies and merges functions.
type FunctionSource interface {
	Digests(ctx context.Context, ref string) (map[string]string, error)
}

// Service implements branching.
type Service struct {
	cfg  *config.Config
	reg  registry.Registry
	fns  FunctionSource
	sec  secrets.Secrets
	eng  Engine
	bk   *backup.Service
	db   Database
	log  *slog.Logger
	now  func() time.Time
	http *http.Client

	createWait, opTimeout, staleAfter time.Duration

	// instance, host and pid say who runs this service's operations: they are recorded in
	// the started event of every operation, so that another process (the sweeper of a
	// restarted daemon) can tell an abandoned operation from one that is still running.
	instance, host string
	pid            int

	// Seams for tests.
	detect func(srcData, dstParent string) (method, fsys, reason string)
	rotate func(ctx context.Context, ref string) error
	// isolate switches off the integrations a cloned cluster inherited (isolateBranch).
	isolate func(ctx context.Context, ref string) error
	// rewrite replaces the parent's credentials inside a cloned cluster's data with the
	// branch's own (rewriteBranchCredentials); it runs after rotate.
	rewrite func(ctx context.Context, ref string) error
	clone   func(ctx context.Context, parentRef, method, dstData string) (*CloneStats, error)
	// freeBytes is the free space of the disk holding a path (-1: unknown).
	freeBytes func(path string) int64

	mu   sync.Mutex
	runs map[string]*run // by branch ref
	// lastPrune is when old action runs were last removed (runs.go).
	lastPrune time.Time
	// notifyMu serializes the creation of the notify signing key (notify.go).
	notifyMu sync.Mutex
	wg       sync.WaitGroup
	base     context.Context
	stop     context.CancelFunc
}

// New builds the service. It connects to nothing.
func New(d Deps) (*Service, error) {
	if d.Cfg == nil || d.Registry == nil || d.Secrets == nil || d.Engine == nil {
		return nil, errors.New("branching: Deps needs Cfg, Registry, Secrets and Engine")
	}
	if _, _, err := d.Cfg.Branching.TTL(); err != nil {
		return nil, err
	}
	s := &Service{
		cfg: d.Cfg, reg: d.Registry, sec: d.Secrets, eng: d.Engine, bk: d.Backup, db: d.DB, fns: d.Functions, log: d.Log, now: d.Now,
		http: d.HTTPClient, createWait: d.CreateWait, opTimeout: d.OpTimeout, staleAfter: d.StaleAfter,
		detect: nil, runs: map[string]*run{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.createWait <= 0 {
		s.createWait = 10 * time.Second
	}
	if s.opTimeout <= 0 {
		s.opTimeout = time.Hour
	}
	if s.staleAfter <= 0 {
		s.staleAfter = 30 * time.Minute
	}
	if s.db == nil {
		s.db = &pgDatabase{dsn: func(ctx context.Context, ref string) (string, error) {
			return s.eng.ConnString(ctx, ref, lifecycle.RolePostgres)
		}}
	}
	if s.http == nil {
		s.http = newNotifyClient(d.Cfg.Branching.AllowPrivateNotifyURLs)
	}
	s.detect = func(srcData, dstParent string) (string, string, string) {
		return detectClone(srcData, dstParent)
	}
	s.instance, s.pid = secrets.NewUUID(), os.Getpid()
	s.host, _ = os.Hostname()
	s.rotate = s.rotateCredentials
	s.isolate = s.isolateBranch
	s.rewrite = s.rewriteBranchCredentials
	s.clone = s.cloneParent
	s.freeBytes = freeBytes
	s.base, s.stop = context.WithCancel(context.Background())
	return s, nil
}

// Drain waits for detached operations to finish (a daemon calls it on shutdown, after it
// stops accepting requests). When ctx ends first the operations are cancelled.
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

// ---- reading -------------------------------------------------------------------------

func (s *Service) project(ctx context.Context, ref string) (*registry.Project, error) {
	p, err := s.reg.GetProject(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, fmt.Errorf("%w: project %s", ErrNotFound, ref)
	}
	return p, err
}

// Parent resolves a project ref to the project whose branches are meant: the project
// itself, or the parent when ref is itself a branch (a client linked to a branch asks
// that branch's project for its branches and gets the whole family).
func (s *Service) Parent(ctx context.Context, ref string) (*registry.Project, error) {
	if ref == config.SystemRef {
		return nil, invalid("the system project has no branches")
	}
	p, err := s.project(ctx, ref)
	if err != nil {
		return nil, err
	}
	if p.Ref == config.SystemRef {
		return nil, invalid("the system project has no branches")
	}
	if p.Branch != nil {
		return s.project(ctx, p.Branch.ParentRef)
	}
	return p, nil
}

// List returns the branches of the project ref belongs to: the default branch (the
// project itself) first, then the others by creation time.
func (s *Service) List(ctx context.Context, ref string) ([]Branch, error) {
	parent, err := s.Parent(ctx, ref)
	if err != nil {
		return nil, err
	}
	kids, err := s.children(ctx, parent.Ref)
	if err != nil {
		return nil, err
	}
	out := []Branch{*s.defaultBranch(ctx, parent)}
	for i := range kids {
		out = append(out, *branchOf(&kids[i]))
	}
	return out, nil
}

func (s *Service) children(ctx context.Context, parentRef string) ([]registry.Project, error) {
	all, err := s.reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var kids []registry.Project
	for _, p := range all {
		if p.Branch != nil && p.Branch.ParentRef == parentRef {
			kids = append(kids, p)
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].CreatedAt.Before(kids[j].CreatedAt) })
	return kids, nil
}

// Children returns the refs of the branches of parentRef (not the default branch).
func (s *Service) Children(ctx context.Context, parentRef string) ([]string, error) {
	kids, err := s.children(ctx, parentRef)
	if err != nil {
		return nil, err
	}
	refs := make([]string, len(kids))
	for i, k := range kids {
		refs[i] = k.Ref
	}
	return refs, nil
}

// Get returns the branch of ref's family called name.
func (s *Service) Get(ctx context.Context, ref, name string) (*Branch, error) {
	parent, err := s.Parent(ctx, ref)
	if err != nil {
		return nil, err
	}
	if name == DefaultBranchName {
		return s.defaultBranch(ctx, parent), nil
	}
	kids, err := s.children(ctx, parent.Ref)
	if err != nil {
		return nil, err
	}
	for i := range kids {
		if kids[i].Branch.Name == name {
			return branchOf(&kids[i]), nil
		}
	}
	return nil, fmt.Errorf("%w: %q in project %s", ErrNotFound, name, parent.Ref)
}

// Resolve finds a branch by its UUID or by its project ref. A ref of a project that is
// not a branch resolves to that project's default branch.
func (s *Service) Resolve(ctx context.Context, idOrRef string) (*Branch, error) {
	if IsUUID(idOrRef) {
		all, err := s.reg.ListProjects(ctx)
		if err != nil {
			return nil, err
		}
		for i := range all {
			if all[i].Branch != nil && strings.EqualFold(all[i].Branch.ID, idOrRef) {
				return branchOf(&all[i]), nil
			}
			if all[i].Branch == nil && all[i].Ref != config.SystemRef && strings.EqualFold(defaultBranchID(all[i].Ref), idOrRef) {
				return s.defaultBranch(ctx, &all[i]), nil
			}
		}
		return nil, fmt.Errorf("%w: id %s", ErrNotFound, idOrRef)
	}
	p, err := s.project(ctx, idOrRef)
	if err != nil {
		return nil, err
	}
	if p.Ref == config.SystemRef {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, idOrRef)
	}
	if p.Branch == nil {
		return s.defaultBranch(ctx, p), nil
	}
	return branchOf(p), nil
}

// resolveBranch is Resolve for operations that need a real branch, not the default one.
func (s *Service) resolveBranch(ctx context.Context, idOrRef, op string) (*Branch, error) {
	b, err := s.Resolve(ctx, idOrRef)
	if err != nil {
		return nil, err
	}
	if b.IsDefault {
		return nil, invalid("cannot %s the default branch of project %s", op, b.Ref)
	}
	return b, nil
}

func busy(st registry.BranchState) bool {
	return st == registry.BranchCreatingProject || st == registry.BranchRunningMigration
}

// inFlight reports whether the registry says an operation is at work on b, here or in another
// process: its state is busy, or it is MIGRATIONS_PASSED and a run is still going on it. An
// operation moves the branch to MIGRATIONS_PASSED for the deploy step, so the state alone does not
// tell that step from a branch at rest (the end of a branch made before FUNCTIONS_DEPLOYED
// existed, or a status set through the API).
func (s *Service) inFlight(ctx context.Context, b *Branch) bool {
	if busy(b.State) {
		return true
	}
	if b.State != registry.BranchMigrationsPassed {
		return false
	}
	st := s.runStore()
	if st == nil {
		return false
	}
	runs, err := st.RunningBranchRuns(ctx)
	if err != nil {
		return false
	}
	for i := range runs {
		if runs[i].BranchRef == b.Ref {
			return true
		}
	}
	return false
}

// ---- helpers shared by the operations ---------------------------------------------------

// activeParent loads the parent of b and checks it can take part in an operation.
func (s *Service) activeParent(ctx context.Context, b *Branch) (*registry.Project, error) {
	p, err := s.project(ctx, b.ParentRef)
	if err != nil {
		return nil, err
	}
	if p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy {
		return nil, conflict("project %s is %s; it must be running", p.Ref, p.Status)
	}
	return p, nil
}

func (s *Service) activeBranch(ctx context.Context, b *Branch) error {
	if b.ProjectStatus != registry.StatusActiveHealthy && b.ProjectStatus != registry.StatusActiveUnhealthy {
		return conflict("branch %s is %s; it must be running", b.Name, b.ProjectStatus)
	}
	return nil
}

// classFor is the compute size of a new branch: the one asked for (hosted's names, Studio's
// infra_compute_size values and the add-on variants all work), else [branching] default_class.
func (s *Service) classFor(size string) (string, error) {
	if size == "" {
		size = s.cfg.Branching.Class()
	}
	if n, ok := lifecycle.ParseSize(size); ok {
		return n, nil
	}
	return "", invalid("unknown instance size %q", size)
}

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// Options configure an Engine.
type Options struct {
	Log *slog.Logger
	// Fleet registers projects with the shared services after their units are healthy.
	// Empty skips tenant calls.
	Fleet fleet.Fleet
	// Backup takes the final base backup before a project is deleted; nil skips it.
	Backup BaseBackuper
	// Timers starts and stops a project's nightly base backup timer as the project
	// becomes active, pauses and is deleted; nil does nothing (the exec backend).
	Timers Timers
	// Now is the clock for key issue times; tests set it.
	Now func() time.Time
	// Settings supplies the saved per-project settings for Storage and Realtime tenants
	// (and, through the plane, for units); nil means defaults.
	Settings Settings
	// UpgradeNotify, when set, is told when a project's upgrade starts, succeeds or fails
	// (UpgradeNotice). The daemon sets it to raise alerts; this package cannot import the alerting
	// package (which reads the node's health through this one), so the hook is how an event gets out.
	// It is called on the upgrade's own goroutine after the event is recorded and must not block:
	// the caller starts any slow delivery itself. `supavise upgrade` leaves it nil: it reports the
	// node's upgrade as one, not every project's move.
	UpgradeNotify func(ctx context.Context, n UpgradeNotice)
	// NodeID is the id of the node this Engine runs on (registry.Node.ID). StartActive, Recover and
	// EnsureTenants leave a project homed on another node (registry.Project.NodeID) to that node. Empty
	// means every project is this node's: a server outside a cluster, and tests.
	NodeID string
	// ReadOnly is the Engine of a follower, whose registry is a hot standby of the leader's: it takes
	// no advisory locks (a standby refuses them), and Recover, which settles statuses in the
	// registry, does nothing. The operations that write the registry fail with registry.ErrReadOnly.
	ReadOnly bool
	// Replicas reaches the replicas of a project on the nodes that hold them, so that a resize can
	// restart them in the right order (see ReplicaFleet). Nil: the node has none.
	Replicas ReplicaFleet
	// ReplicaRestartTimeout bounds the restart of one replica after a resize or a settings save (see
	// restartReplicas); zero means five minutes. It also bounds the restart of one replica's PostgREST
	// after a change of the project's keys (updateReplicaKeys); zero means two minutes there.
	ReplicaRestartTimeout time.Duration
	// PeerRefresh asks the other nodes that run Supavisor to drop their cached copy of a tenant after
	// the Engine changed it (a database password, a restore, a tenant that moved with its project).
	// Nil: the node is part of no cluster, or the daemon has not wired the fleet yet (SetPeerRefresher).
	PeerRefresh fleet.PeerRefresher
}

// Timers drives the per-project nightly base backup timer (supavise-basebackup@<ref>.timer).
// Failures are logged by the Engine and never fail the operation: a missing timer is
// repaired by the next start.
type Timers interface {
	StartTimer(ctx context.Context, ref string) error
	StopTimer(ctx context.Context, ref string) error
}

// Timers is the Timers the Engine drives: the node's own, or the one that sends each call to the node a
// project is homed on once SetTimers has put it in front (nil: none).
func (e *Engine) Timers() Timers { return e.opts.Timers }

// SetTimers replaces the Timers the Engine drives. internal/app calls it before the Engine serves a
// request, to put the router of internal/placement in front of the node's own timers in a cluster: a
// backup timer runs where the project's data is, which is its home. It is not safe to call while
// operations run.
func (e *Engine) SetTimers(t Timers) { e.opts.Timers = t }

func (e *Engine) startTimer(ctx context.Context, ref string) {
	if e.opts.Timers == nil {
		return
	}
	if err := e.opts.Timers.StartTimer(ctx, ref); err != nil {
		e.log.Warn("backup timer did not start; nightly base backups will not run until the project is started again", "ref", ref, "error", err)
	}
}

func (e *Engine) stopTimer(ctx context.Context, ref string) {
	if e.opts.Timers == nil {
		return
	}
	if err := e.opts.Timers.StopTimer(ctx, ref); err != nil {
		e.log.Warn("backup timer did not stop", "ref", ref, "error", err)
	}
}

// Engine is the Manager: it owns the order of registry rows, sealed secrets, units,
// fleet tenants and routes, and the project status each step implies.
type Engine struct {
	cfg   *config.Config
	reg   registry.Registry
	sec   secrets.Secrets
	arts  Artifacts
	plane Plane
	opts  Options
	log   *slog.Logger

	locks sync.Map // ref -> *sync.Mutex
	// dataAPIFailures is the last failure recorded as an event per ref (dataAPIFailure), so that a
	// project that keeps failing the Data API step does not write an event every pass.
	dataAPIFailures sync.Map
	// restoring holds the refs whose in-place restore is running in this process (restore.go).
	restoring sync.Map
	// upgrading holds the refs whose upgrade is running in this process (upgrade.go).
	upgrading sync.Map
	// freeBytes reads the free space of the disk holding a path (-1: unknown); tests replace it.
	freeBytes func(path string) int64
	// capacity is the node's room for project memory (capacity.go, resize.go).
	capacity capacityState
	// remoteBackups takes the final backup of a project homed on another node (SetRemoteBackups).
	remoteBackups RemoteBackups
	// objects removes a project's Storage objects from the S3 bucket (SetObjectRemover); nil: none.
	objects ObjectRemover
	// recordBudget is how long what a replica's restart left to record may take to write (zero: the
	// budget of a cleanup); tests shorten it.
	recordBudget time.Duration
	// schemaProbe answers which of names are not schemas of ref's database; nil asks the database
	// (probeSchemas). Tests replace it.
	schemaProbe func(ctx context.Context, ref string, names []string) ([]string, error)
}

var _ Manager = (*Engine)(nil)

// NewEngine builds the Manager over plane.
func NewEngine(cfg *config.Config, reg registry.Registry, sec secrets.Secrets, arts Artifacts, plane Plane, opts Options) *Engine {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{cfg: cfg, reg: reg, sec: sec, arts: arts, plane: plane, opts: opts, log: opts.Log, freeBytes: diskFree}
}

// advisoryPool is implemented by the Postgres registry; the in-memory one has no
// other process to coordinate with.
type advisoryPool interface{ Pool() *pgxpool.Pool }

// lock serializes mutating operations on one project: a mutex within this process and,
// when the registry is Postgres, a session-level advisory lock so that the CLI and the
// daemon (separate processes) cannot, say, delete and resume the same ref at once. The
// advisory lock uses its own connection rather than the pool, so held locks cannot
// starve registry queries, and it is released when that connection closes, even if the
// process dies. The returned function releases both.
func (e *Engine) lock(ctx context.Context, ref string) (func(), error) {
	mu := e.mutex(ref)
	mu.Lock()
	return e.lockAdvisory(ctx, ref, mu)
}

func (e *Engine) mutex(ref string) *sync.Mutex {
	m, _ := e.locks.LoadOrStore(ref, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// lockWithin is lock for a caller that must not wait longer than its context: it gives up when ctx
// ends while another operation holds the project (sync.Mutex.Lock has no context to honor).
func (e *Engine) lockWithin(ctx context.Context, ref string) (func(), error) {
	mu := e.mutex(ref)
	if !mu.TryLock() {
		t := time.NewTicker(25 * time.Millisecond)
		defer t.Stop()
		for !mu.TryLock() {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("lifecycle: lock %s: %w", ref, ctx.Err())
			case <-t.C:
			}
		}
	}
	return e.lockAdvisory(ctx, ref, mu)
}

// lockAdvisory is the rest of lock, once mu is held: it adds the advisory lock when there is one.
func (e *Engine) lockAdvisory(ctx context.Context, ref string, mu *sync.Mutex) (func(), error) {
	ap, ok := e.reg.(advisoryPool)
	if !ok || ap.Pool() == nil || e.opts.ReadOnly {
		return mu.Unlock, nil
	}
	conn, err := pgx.ConnectConfig(ctx, ap.Pool().Config().ConnConfig)
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("lifecycle: lock %s: %w", ref, err)
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext('supavise:' || $1::text))`, ref); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		mu.Unlock()
		return nil, fmt.Errorf("lifecycle: lock %s: %w", ref, err)
	}
	return func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Close(cctx) // ends the session, which drops the advisory lock
		mu.Unlock()
	}, nil
}

// homedHere reports whether p is this node's to run: its home is this node, or the Engine has no
// node (a server outside a cluster).
func (e *Engine) homedHere(p *registry.Project) bool {
	return e.opts.NodeID == "" || p.NodeID == "" || p.NodeID == e.opts.NodeID
}

// atHome refuses op on a project that is homed on another node unless the Engine drives a plane that
// routes by home. A node that holds a replica of the project would otherwise stop, restart or delete
// the replica and flip the project's status in the registry. Only an operation whose every step is a
// plane call, a registry write or a call on the shared services may use it (the leader's Timers and
// capacity check follow the home too, see SetTimers and SetRemoteNodes); an operation that reads or
// writes this node's own disk uses onHome.
func (e *Engine) atHome(p *registry.Project, op string) error {
	if e.drives(p) {
		return nil
	}
	return e.notHomedHere(p, op, "")
}

// drives reports whether the Engine acts on p: it is homed here, or the plane routes by home.
func (e *Engine) drives(p *registry.Project) bool {
	if e.homedHere(p) {
		return true
	}
	_, ok := e.plane.(HomeRouter)
	return ok
}

// onHome refuses op on a project that is not homed on this node, whatever the plane: the operation
// works on the data directory, the backup service or the artifacts of this node, which hold nothing of
// a project that runs elsewhere. why says what stays here, for the message.
func (e *Engine) onHome(p *registry.Project, op, why string) error {
	if e.homedHere(p) {
		return nil
	}
	return e.notHomedHere(p, op, why)
}

func (e *Engine) notHomedHere(p *registry.Project, op, why string) error {
	if why != "" {
		why = "; " + why
	}
	return fmt.Errorf("%w: cannot %s %s here: it is homed on node %s and this is node %s%s", ErrInvalidState, op, p.Ref, p.NodeID, e.opts.NodeID, why)
}

// SetPlane replaces the plane the Engine drives. internal/app calls it before the Engine serves a
// request, to put the plane router (internal/placement) in front of the node's own plane in a
// cluster; it is not safe to call while operations run.
func (e *Engine) SetPlane(p Plane) { e.plane = p }

// NodeID is the node the Engine runs on ("" outside a cluster).
func (e *Engine) NodeID() string { return e.opts.NodeID }

// cleanupBudget is how long the cleanup after a failure may take.
const cleanupBudget = 3 * time.Minute

// cleanupCtx outlives a cancelled request: cleanup after a failure must still run.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupBudget)
}

func (e *Engine) versions() (map[string]string, error) {
	v := map[string]string{}
	for _, svc := range config.ProjectServices {
		tag, err := e.arts.Tag(svc)
		if err != nil {
			return nil, err
		}
		v[svc] = tag
	}
	return v, nil
}

// StoreKeys seals every credential of keys into the registry.
func (e *Engine) storeKeys(ctx context.Context, ref string, keys *secrets.ProjectKeys) error {
	for name, val := range keys.Map() {
		sealed, err := e.sec.Seal([]byte(val))
		if err != nil {
			return err
		}
		if err := e.reg.PutSecret(ctx, ref, name, sealed); err != nil {
			return fmt.Errorf("lifecycle: store secret %s: %w", name, err)
		}
	}
	return nil
}

// putRecords stores key records as the sealed secrets the proxy and the API read.
func (e *Engine) putRecords(ctx context.Context, ref string, recs []secrets.APIKeyRecord) error {
	for _, r := range recs {
		b, err := secrets.MarshalRecord(r)
		if err != nil {
			return err
		}
		sealed, err := e.sec.Seal(b)
		if err != nil {
			return err
		}
		if err := e.reg.PutSecret(ctx, ref, secrets.RecordSecretName(r.ID), sealed); err != nil {
			return fmt.Errorf("lifecycle: store key record %s: %w", r.ID, err)
		}
	}
	return nil
}

// storeRecords writes back the records of old that revived replaced.
func (e *Engine) storeRecords(ctx context.Context, ref string, old *secrets.ProjectKeys, revived []secrets.APIKeyRecord) error {
	var prior []secrets.APIKeyRecord
	for _, r := range revived {
		if o, ok := old.Record(r.ID); ok {
			prior = append(prior, o)
		}
	}
	return e.putRecords(ctx, ref, prior)
}

func (e *Engine) loadKeys(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
	sealed, err := e.reg.GetSecrets(ctx, ref)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(sealed))
	for name, blob := range sealed {
		pt, err := e.sec.Open(blob)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: open secret %s of %s: %w", name, ref, err)
		}
		m[name] = string(pt)
	}
	k := secrets.KeysFromMap(m)
	if k.JWTSecret == "" || k.AdminPassword == "" || k.DBPassword == "" {
		return nil, fmt.Errorf("lifecycle: project %s has no stored credentials", ref)
	}
	return k, nil
}

func (e *Engine) event(ctx context.Context, ref, kind string, payload any) {
	if err := e.reg.AppendEvent(ctx, ref, kind, payload); err != nil {
		e.log.Warn("could not record event", "ref", ref, "kind", kind, "error", err)
	}
}

func (e *Engine) org(ctx context.Context, slug string) (*registry.Organization, error) {
	if slug == "" {
		slug = "default"
	}
	o, err := e.reg.GetOrganization(ctx, slug)
	if errors.Is(err, registry.ErrNotFound) && slug == "default" {
		o, err = e.reg.CreateOrganization(ctx, "default", "Default Organization")
		if errors.Is(err, registry.ErrConflict) {
			o, err = e.reg.GetOrganization(ctx, slug)
		}
	}
	return o, err
}

// tenantSpec describes p to the shared services, with its saved Storage, Realtime and
// pooler settings when the Engine has a Settings source.
func (e *Engine) tenantSpec(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (fleet.TenantSpec, error) {
	spec := e.baseTenantSpec(p, keys)
	if e.opts.Settings != nil && p.Ref != config.SystemRef {
		var err error
		if spec.Storage, err = e.opts.Settings.StorageSettings(ctx, p.Ref); err != nil {
			return spec, fmt.Errorf("lifecycle: saved storage settings of %s: %w", p.Ref, err)
		}
		if spec.Realtime, err = e.opts.Settings.RealtimeSettings(ctx, p.Ref); err != nil {
			return spec, fmt.Errorf("lifecycle: saved realtime settings of %s: %w", p.Ref, err)
		}
		pool, err := e.opts.Settings.PoolerSettings(ctx, p.Ref)
		if err != nil {
			return spec, fmt.Errorf("lifecycle: saved pooler settings of %s: %w", p.Ref, err)
		}
		spec.PoolSize, spec.MaxClients = pool.PoolSize, pool.MaxClients
	}
	e.poolDefaults(&spec, p)
	return spec, nil
}

func (e *Engine) baseTenantSpec(p *registry.Project, keys *secrets.ProjectKeys) fleet.TenantSpec {
	ports := e.cfg.PortsFor(p.Ref, p.Seq)
	return fleet.TenantSpec{
		Ref: p.Ref, DBHost: "127.0.0.1", DBPort: ports.Postgres, DBName: "postgres",
		DBUser: RoleAdmin, DBPassword: keys.AdminPassword, PostgresPassword: keys.DBPassword,
		JWTSecret: keys.JWTSecret, AnonKey: keys.AnonKey, ServiceRoleKey: keys.ServiceRoleKey,
		Host: e.cfg.ProjectHost(p.Ref),
	}
}

// Create implements Manager.
//
// Order: registry row (COMING_UP), sealed secrets, the settings of a new project, data plane (cluster, roles, GoTrue,
// PostgREST, all health-checked), fleet tenants, host route, then ACTIVE_HEALTHY. A
// failure after the row exists stops and removes the units and the data and leaves the
// row INIT_FAILED with an event carrying the cause; the secrets stay so that the row
// can be inspected and deleted. Nothing is left running.
func (e *Engine) Create(ctx context.Context, req CreateRequest) (*registry.Project, error) {
	if e.cfg.BaseDomain() == "" {
		return nil, errors.New("lifecycle: set domain (or public_ip) in the config before creating projects")
	}
	if req.Class == ClassSystem {
		return nil, fmt.Errorf("lifecycle: class %q is reserved", req.Class)
	}
	size, err := ClassFor(req.Class)
	if err != nil {
		return nil, err
	}
	class := size.Name
	if req.Seed != nil && req.Keys == nil {
		// A seeded cluster already holds its role passwords and Create does not reset
		// them, so fresh keys would never match and the units could not authenticate.
		return nil, errors.New("lifecycle: CreateRequest.Seed needs CreateRequest.Keys (the seeded cluster's own credentials)")
	}
	ref := req.Ref
	switch {
	case ref == "":
		ref = secrets.NewRef()
	case !secrets.ValidRef(ref):
		return nil, fmt.Errorf("lifecycle: %q is not a valid project ref (20 lowercase letters)", ref)
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()

	org, err := e.org(ctx, req.OrgSlug)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: organization %q: %w", req.OrgSlug, err)
	}
	versions, err := e.versions()
	if err != nil {
		return nil, err
	}
	limits := size.Limits()
	if req.Limits != nil {
		limits = *req.Limits
	}
	var keys *secrets.ProjectKeys
	if req.Keys != nil {
		k := *req.Keys
		keys = &k
	} else if keys, err = secrets.NewProjectKeys(ref, e.opts.Now()); err != nil {
		return nil, err
	}
	if req.DBPassword != "" {
		keys.DBPassword = req.DBPassword
	}
	region := e.cfg.ProjectRegion(req.Region)
	name := req.Name
	if name == "" {
		name = ref
	}
	p := &registry.Project{
		Ref: ref, OrgID: org.ID, Name: name, Region: region, Engine: registry.EnginePostgres,
		Class: class, Status: registry.StatusComingUp, Versions: versions, Limits: limits,
	}
	if req.Branch != nil {
		b := *req.Branch
		p.Branch = &b
		// A branch lives where its parent does: the operations that copy or compare the two
		// databases then run on one server (placement.go).
		p.NodeID = e.homeOfParent(ctx, b.ParentRef)
	}
	releaseCap, err := e.holdCapacity(ctx, req, size, e.capacityHome(ctx, req, p))
	if err != nil {
		return nil, err
	}
	defer releaseCap()
	if req.Recreate {
		cur, err := e.reg.GetProject(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: recreate %s: %w", ref, err)
		}
		if cur.Status != registry.StatusInitFailed {
			return nil, invalidState(cur, "recreate")
		}
		// The row keeps its identity (name, sequence number, branch info); the rest is renewed.
		cur.Region, cur.Class, cur.Versions, cur.Limits, cur.Status = region, class, versions, limits, registry.StatusComingUp
		if err := e.reg.UpdateProject(ctx, cur); err != nil {
			return nil, fmt.Errorf("lifecycle: recreate project %s: %w", ref, err)
		}
		p = cur
	} else if err := e.reg.CreateProject(ctx, p); err != nil {
		return nil, fmt.Errorf("lifecycle: create project %s: %w", ref, err)
	} else if p.Branch != nil {
		// The branch row exists now. A delete of the parent holds the parent's lock, not this
		// ref's: it sets GOING_DOWN and only then looks for branches, so either it sees this row
		// or this check sees GOING_DOWN. A branch must not be left on a parent that goes away.
		if par, perr := e.reg.GetProject(ctx, p.Branch.ParentRef); perr != nil || par.Status == registry.StatusGoingDown {
			if derr := e.reg.DeleteProject(context.WithoutCancel(ctx), ref); derr != nil {
				e.log.Warn("create: could not remove the row of a branch whose parent is going away", "ref", ref, "err", derr)
			}
			return nil, fmt.Errorf("%w: parent project %s is being deleted or gone", ErrInvalidState, p.Branch.ParentRef)
		}
	}
	releaseCap() // the row exists and counts against the node from here
	fail := func(stage string, cause error) (*registry.Project, error) {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		if errors.Is(cause, ErrClusterExists) && req.Recreate {
			// Foreign data under a row that was meant to be empty: leave it, keep the row failed.
			e.cleanup(cctx, p, true)
			if err := e.reg.SetProjectStatus(cctx, ref, registry.StatusInitFailed); err != nil {
				e.log.Error("could not mark project INIT_FAILED", "ref", ref, "error", err)
			}
			return nil, fmt.Errorf("lifecycle: recreate %s refused, existing data left untouched (move %s away): %w", ref, e.cfg.Paths().Project(ref), cause)
		}
		if errors.Is(cause, ErrClusterExists) {
			// The data belongs to someone else (an orphan, or a restore target that was
			// not cleared): leave it alone and forget the row this call just made.
			e.cleanup(cctx, p, true)
			if err := e.reg.DeleteProject(cctx, ref); err != nil {
				e.log.Error("could not remove the registry row of a refused create", "ref", ref, "error", err)
			}
			return nil, fmt.Errorf("lifecycle: create %s refused, existing data left untouched (move %s away or choose another ref): %w", ref, e.cfg.Paths().Project(ref), cause)
		}
		e.cleanup(cctx, p, false)
		if err := e.reg.SetProjectStatus(cctx, ref, registry.StatusInitFailed); err != nil {
			e.log.Error("could not mark project INIT_FAILED", "ref", ref, "error", err)
		}
		e.event(cctx, ref, "project.init_failed", map[string]string{"stage": stage, "error": cause.Error()})
		return nil, fmt.Errorf("lifecycle: create %s failed at %s (project left INIT_FAILED, nothing running; remove it with `supavise projects delete %s --skip-final-backup`): %w", ref, stage, ref, cause)
	}
	if err := e.storeKeys(ctx, ref, keys); err != nil {
		return fail("store secrets", err)
	}
	if !req.Recreate {
		if err := e.seedSettingsAtCreate(ctx, ref); err != nil {
			return fail("settings", err)
		}
		e.copyInitialSettings(ctx, ref, req)
	}
	if err := e.plane.Create(ctx, p, keys, req.Seed); err != nil {
		return fail("data plane", err)
	}
	if err := e.dataAPIAtCreate(ctx, p, keys, req); err != nil {
		return fail("data api", err)
	}
	if len(e.opts.Fleet) > 0 {
		spec, err := e.tenantSpec(ctx, p, keys)
		if err != nil {
			return fail("fleet tenants", err)
		}
		if err := e.opts.Fleet.EnsureTenant(ctx, spec); err != nil {
			return fail("fleet tenants", err)
		}
	}
	if err := e.reg.PutRoute(ctx, registry.Route{Host: e.cfg.ProjectHost(ref), Ref: ref, Kind: "api"}); err != nil {
		return fail("route", err)
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusActiveHealthy); err != nil {
		return fail("status", err)
	}
	e.startTimer(ctx, ref)
	e.event(ctx, ref, "project.created", map[string]string{"class": class})
	return e.reg.GetProject(ctx, ref)
}

// cleanup undoes what Create may have started, ignoring errors (it is already failing).
// keepData skips the data plane, whose Delete removes the project directory.
func (e *Engine) cleanup(ctx context.Context, p *registry.Project, keepData bool) {
	if len(e.opts.Fleet) > 0 {
		if err := e.opts.Fleet.RemoveTenant(ctx, p.Ref); err != nil {
			e.log.Warn("cleanup: remove tenants", "ref", p.Ref, "error", err)
		}
	}
	if err := e.reg.DeleteRoute(ctx, e.cfg.ProjectHost(p.Ref)); err != nil && !errors.Is(err, registry.ErrNotFound) {
		e.log.Warn("cleanup: delete route", "ref", p.Ref, "error", err)
	}
	if keepData {
		return
	}
	if err := e.plane.Delete(ctx, p.Ref); err != nil {
		e.log.Error("cleanup: data plane delete failed; units or data may remain", "ref", p.Ref, "error", err)
	}
}

func invalidState(p *registry.Project, op string) error {
	return fmt.Errorf("%w: cannot %s %s while it is %s", ErrInvalidState, op, p.Ref, p.Status)
}

// Pause implements Manager: GoTrue, PostgREST and PostgreSQL stop, in that order, and
// the project becomes INACTIVE. Its route and fleet tenants stay; the data stays. The pause is
// the owner's: nothing wakes the project but a Resume (PauseWith records the other kind).
func (e *Engine) Pause(ctx context.Context, ref string) error {
	return e.PauseWith(ctx, ref, PauseOptions{})
}

// PauseWith is Pause with a reason (pause.go).
func (e *Engine) PauseWith(ctx context.Context, ref string, o PauseOptions) error {
	if err := e.upgradeBusy(ref, "pause"); err != nil {
		return err
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if err := e.atHome(p, "pause"); err != nil {
		return err
	}
	// A project whose restore failed can be paused, and resumed after: the way back to ACTIVE_HEALTHY
	// when the original data is intact.
	if ref == config.SystemRef || !(p.Status.Running() || p.Status == registry.StatusRestoreFailed || inRestore(ctx, p)) {
		return invalidState(p, "pause")
	}
	// The record of why goes in before the status changes, so that whoever sees the project
	// INACTIVE also finds the reason; a pause the node makes for idleness is not made without it.
	if err := e.recordPause(ctx, ref, o); err != nil {
		return err
	}
	if err := e.setStatus(ctx, ref, registry.StatusPausing); err != nil {
		e.forgetPause(ctx, ref)
		return err
	}
	e.stopTimer(ctx, ref)
	e.quiesce(ctx, ref)
	if err := e.plane.Stop(ctx, ref); err != nil {
		back := registry.StatusActiveUnhealthy
		if p.Status == registry.StatusRestoreFailed {
			back = p.Status
		}
		_ = e.setStatus(context.WithoutCancel(ctx), ref, back)
		e.forgetPause(context.WithoutCancel(ctx), ref)
		return fmt.Errorf("lifecycle: pause %s: %w", ref, err)
	}
	if err := e.setStatus(ctx, ref, registry.StatusInactive); err != nil {
		return err
	}
	e.event(ctx, ref, "project.paused", nil)
	return nil
}

// Resume implements Manager: the reverse of Pause. If a unit does not come up, what
// started is stopped again and the project stays INACTIVE, so Resume can be retried.
//
// A paused project holds no room on the node, so Resume judges its memory cap and CPU against the
// node the way an upsize is judged and refuses with a *CapacityError when they no longer fit.
func (e *Engine) Resume(ctx context.Context, ref string) error { return e.resume(ctx, ref, true) }

// resume is Resume; checkNode is false for a project the daemon itself paused around a restart,
// which comes back whatever the node's budget says (it ran before).
func (e *Engine) resume(ctx context.Context, ref string, checkNode bool) error {
	if err := e.upgradeBusy(ref, "resume"); err != nil {
		return err
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if err := e.atHome(p, "resume"); err != nil {
		return err
	}
	if ref == config.SystemRef || !(p.Status == registry.StatusInactive || inRestore(ctx, p)) {
		return invalidState(p, "resume")
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return err
	}
	releaseCap := func() {}
	if checkNode && p.Status == registry.StatusInactive {
		if releaseCap, err = e.holdResume(ctx, p); err != nil {
			return err
		}
	}
	err = e.setStatus(ctx, ref, registry.StatusComingUp) // the project counts against the node from here
	releaseCap()
	if err != nil {
		return err
	}
	e.awaitDatabasePort(ctx, p)
	if err := e.plane.Start(ctx, p, keys); err != nil {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		if serr := e.plane.Stop(cctx, ref); serr != nil {
			e.log.Warn("resume: stop after failed start", "ref", ref, "error", serr)
		}
		_ = e.setStatus(cctx, ref, registry.StatusInactive)
		e.event(cctx, ref, "project.resume_failed", map[string]string{"error": err.Error()})
		return fmt.Errorf("lifecycle: resume %s: %w", ref, err)
	}
	e.applySavedSettings(ctx, p, keys)
	// A project that was paused when the node moved to the release that revokes the default
	// privileges is converged here, the first time its database answers again.
	if err := e.convergeLocked(ctx, p); err != nil {
		e.log.Warn("resume: could not revoke the default privileges; the next pass tries again", "ref", ref, "error", err)
	}
	if err := e.reg.PutRoute(ctx, registry.Route{Host: e.cfg.ProjectHost(ref), Ref: ref, Kind: "api"}); err != nil {
		e.log.Warn("resume: route", "ref", ref, "error", err)
	}
	if err := e.setStatus(ctx, ref, registry.StatusActiveHealthy); err != nil {
		return err
	}
	// The record stays until the project is running again, so that a request that arrives while it
	// starts still finds a wake in progress.
	e.forgetPause(ctx, ref)
	e.startTimer(ctx, ref)
	e.event(ctx, ref, "project.resumed", nil)
	return nil
}

// Delete implements Manager with a final base backup when a backup engine is set.
func (e *Engine) Delete(ctx context.Context, ref string) error {
	return e.DeleteWith(ctx, ref, DeleteOptions{})
}

// DeleteWith deletes ref. Order: final base backup (the database is started for it if the
// project is paused), fleet tenants, route, units and data, registry row. If the backup
// fails the project is left as it was and the error is returned; any later failure
// leaves the project GOING_DOWN so that Delete can be run again. A project that failed
// to initialize has nothing worth backing up and skips the backup.
func (e *Engine) DeleteWith(ctx context.Context, ref string, o DeleteOptions) error {
	if err := e.upgradeBusy(ref, "delete"); err != nil {
		return err
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if err := e.atHome(p, "delete"); err != nil {
		return err
	}
	if ref == config.SystemRef {
		return fmt.Errorf("%w: the system project cannot be deleted", ErrInvalidState)
	}
	if _, busy := e.restoring.Load(ref); busy {
		// A restore that is not running here (the daemon died during one) leaves the status
		// RESTORING with nothing in this map, and that project can still be deleted.
		return fmt.Errorf("%w: cannot delete %s while a restore is running on it", ErrInvalidState, ref)
	}
	if kids, err := e.branchesOf(ctx, ref); err != nil {
		return err
	} else if len(kids) > 0 {
		// Refuse before anything is stopped: the registry would reject the last step and
		// leave a parent without data.
		return fmt.Errorf("%w: %s still has branches (%s); delete them first", ErrInvalidState, ref, strings.Join(kids, ", "))
	}
	prev := p.Status
	backupDone, tookBackup := false, false
	if prev == registry.StatusGoingDown {
		// A retry of an interrupted delete: take up where it stopped.
		if dp := e.deleteProgress(ctx, ref); dp.known {
			prev, backupDone, tookBackup = dp.prev, dp.backupDone, dp.backupTaken
		}
	} else {
		e.event(ctx, ref, EventDeleteStarted, map[string]string{"prev": string(prev)})
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusGoingDown); err != nil {
		return err
	}
	// A branch created after the first look has inserted its row by now or will see GOING_DOWN
	// (Create): look again, and back out if one landed in between.
	if prev != registry.StatusGoingDown {
		if kids, err := e.branchesOf(ctx, ref); err != nil || len(kids) > 0 {
			_ = e.reg.SetProjectStatus(context.WithoutCancel(ctx), ref, prev)
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: %s still has branches (%s); delete them first", ErrInvalidState, ref, strings.Join(kids, ", "))
		}
	}
	if !backupDone {
		if e.opts.Backup != nil && !o.SkipFinalBackup && prev != registry.StatusInitFailed {
			switch err := e.finalBackup(ctx, p, prev); {
			case errors.Is(err, ErrNoRestorableState):
				e.log.Warn("delete: nothing to back up, deleting without a final backup", "ref", ref, "reason", err)
				e.event(ctx, ref, "project.final_backup_skipped", map[string]string{"reason": err.Error()})
			case err != nil:
				hint := ""
				if prev == registry.StatusGoingDown {
					hint = " (if its data is already gone, delete it with --skip-final-backup)"
				}
				_ = e.reg.SetProjectStatus(context.WithoutCancel(ctx), ref, prev)
				return fmt.Errorf("lifecycle: final backup of %s failed, project kept%s: %w", ref, hint, err)
			default:
				tookBackup = true
			}
		}
		// The backup step is settled (done, skipped or not wanted): an interrupted delete
		// continues from here, and the nightly timer must not fire against a cluster
		// that is about to go away.
		e.event(ctx, ref, EventDeleteBackupDone, map[string]bool{"final_backup": tookBackup})
	}
	e.stopTimer(ctx, ref)
	if len(e.opts.Fleet) > 0 {
		if err := e.opts.Fleet.RemoveTenant(ctx, ref); err != nil {
			e.log.Warn("delete: remove fleet tenants (continuing)", "ref", ref, "error", err)
			e.event(ctx, ref, "project.tenant_cleanup_failed", map[string]string{"error": err.Error()})
		}
	}
	if err := e.reg.DeleteRoute(ctx, e.cfg.ProjectHost(ref)); err != nil && !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	if err := e.plane.Delete(ctx, ref); err != nil {
		return fmt.Errorf("lifecycle: delete %s: %w", ref, err)
	}
	if o.KeepRecord {
		e.forgetPause(ctx, ref)
		if o.PurgeObjects {
			// A branch reset: the new cluster starts with no objects, so the files of the old one are
			// orphans. A restore keeps them, because it puts its snapshot of them back.
			e.purgeStorageObjects(ctx, ref)
		}
		if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusInitFailed); err != nil {
			return err
		}
		e.event(ctx, ref, "project.data_removed", map[string]any{"final_backup": tookBackup})
		return nil
	}
	// The Storage objects of the file backend live beside the Storage service, not in the project's
	// directory, so plane.Delete left them. They go now and not earlier: the final backup above has
	// copied them, and a delete that stopped before this point (status GOING_DOWN) comes back here.
	// A KeepRecord delete returned above: its caller (a restore, a branch reset) brings the project back.
	if err := e.removeStorageObjects(ref); err != nil {
		return fmt.Errorf("lifecycle: delete %s: remove its Storage objects: %w", ref, err)
	}
	e.purgeBucketObjects(ctx, ref)
	if err := e.reg.DeleteProject(ctx, ref); err != nil {
		return err
	}
	e.event(ctx, ref, "project.deleted", map[string]any{"final_backup": tookBackup})
	return nil
}

// removeStorageObjects deletes the directory of ref's Storage objects (file backend). A node with
// another backend, or a project that never stored an object, has none: that is not an error.
func (e *Engine) removeStorageObjects(ref string) error {
	if !secrets.ValidRef(ref) { // never build a path from anything but a project ref
		return nil
	}
	return os.RemoveAll(e.cfg.Paths().StorageObjects(ref))
}

// branchesOf lists the refs of the branches whose parent is ref.
func (e *Engine) branchesOf(ctx context.Context, ref string) ([]string, error) {
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var kids []string
	for _, q := range ps {
		if q.Branch != nil && q.Branch.ParentRef == ref {
			kids = append(kids, q.Ref)
		}
	}
	return kids, nil
}

// Events that make an operation resumable after a daemon stop.
const (
	// EventDeleteStarted opens a delete; its payload names the status to go back to.
	EventDeleteStarted = "project.delete_started"
	// EventDeleteBackupDone records that a delete's final-backup step is settled (taken,
	// skipped or not wanted); everything after it removes things and can run again.
	EventDeleteBackupDone = "project.delete_backup_done"
	// EventRestartRequested and EventRestartFinished bracket a restart (the API's pause
	// then resume). Recover resumes a project whose restart was cut after the pause.
	EventRestartRequested = "project.restart_requested"
	EventRestartFinished  = "project.restart_finished"
)

type deleteState struct {
	known       bool
	prev        registry.Status
	backupDone  bool
	backupTaken bool
}

// deleteProgress reads the newest delete's events for ref. known is false when none was
// recorded (a delete interrupted by a version that did not record them).
func (e *Engine) deleteProgress(ctx context.Context, ref string) deleteState {
	evs, err := e.reg.ListEvents(ctx, ref, 200) // newest first
	if err != nil {
		return deleteState{}
	}
	var st deleteState
	for _, ev := range evs {
		switch ev.Kind {
		case EventDeleteBackupDone:
			var pl struct {
				FinalBackup bool `json:"final_backup"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			st.backupDone, st.backupTaken = true, pl.FinalBackup
		case EventDeleteStarted:
			var pl struct {
				Prev string `json:"prev"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			if pl.Prev == "" || registry.Status(pl.Prev) == registry.StatusGoingDown {
				return deleteState{}
			}
			st.known, st.prev = true, registry.Status(pl.Prev)
			return st
		}
	}
	return deleteState{}
}

// RemoteBackups takes the backups of the projects homed on other nodes, which the backup service of
// this node cannot: it reads data directories of this node only (internal/placement implements it).
type RemoteBackups interface {
	// FinalBackup takes the delete-time backup of p, which is homed on another node: its base backup on its
	// home, its files here, where the shared services keep them.
	FinalBackup(ctx context.Context, p *registry.Project) (*registry.Backup, error)
}

// SetRemoteBackups sets the RemoteBackups the Engine takes the final backup of a project homed on
// another node through. Without it that delete is refused. internal/app calls it while it wires the
// cluster features, before the Engine serves a request.
func (e *Engine) SetRemoteBackups(r RemoteBackups) { e.remoteBackups = r }

func (e *Engine) finalBackup(ctx context.Context, p *registry.Project, prev registry.Status) error {
	remote := !e.homedHere(p)
	if remote && e.remoteBackups == nil {
		if err := e.onHome(p, "take the final backup of", "the backup service reads the data directory of this node; delete it with SkipFinalBackup (the data is gone afterwards) or move it to this node first"); err != nil {
			return err
		}
	}
	keys, err := e.loadKeys(ctx, p.Ref)
	if err != nil {
		return err
	}
	if prev == registry.StatusInactive {
		// Paused: the cluster is down. Start it alone for the backup, then stop it again.
		e.awaitDatabasePort(ctx, p)
		if err := e.plane.StartDatabase(ctx, p, keys); err != nil {
			return err
		}
		defer func() {
			cctx, cancel := cleanupCtx(ctx)
			defer cancel()
			if err := e.plane.Stop(cctx, p.Ref); err != nil {
				e.log.Warn("delete: stop database after backup", "ref", p.Ref, "error", err)
			}
		}()
	}
	if remote {
		_, err = e.remoteBackups.FinalBackup(ctx, p)
		return err
	}
	if fb, ok := e.opts.Backup.(FinalBackuper); ok {
		_, err = fb.FinalBackup(ctx, p.Ref)
		return err
	}
	_, err = e.opts.Backup.BaseBackup(ctx, p.Ref)
	return err
}

// RotateKeys implements Manager: a new JWT secret, legacy anon and service_role keys
// and opaque keys; database passwords are unchanged. GoTrue and PostgREST are
// restarted on the new secret, the fleet tenants are updated and then the PostgREST of the
// project's read replicas is restarted on its nodes. A paused project only
// gets new stored keys (and its replicas that run get them); its env files are rewritten when it
// resumes. If applying the keys fails, the previous keys are restored. A replica that could not be
// updated leaves the new keys in force: the keys come back with a *ReplicaKeysError.
func (e *Engine) RotateKeys(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
	if err := e.upgradeBusy(ref, "rotate keys of"); err != nil {
		return nil, err
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := e.atHome(p, "rotate keys of"); err != nil {
		return nil, err
	}
	if p.Status != registry.StatusInactive && !p.Status.Running() {
		return nil, invalidState(p, "rotate keys of")
	}
	old, err := e.loadKeys(ctx, ref)
	if err != nil {
		return nil, err
	}
	nk := *old
	nk.JWTSecret = secrets.NewJWTSecret()
	nk.PublishableKey = secrets.NewPublishableKey()
	nk.SecretKey = secrets.NewSecretKey()
	if err := nk.ResignLegacy(ref, e.opts.Now()); err != nil {
		return nil, err
	}
	// The new default keys are not the ones that were revoked.
	revived := nk.ReviveDefaults(ref, e.opts.Now())
	if _, restored, err := e.applyKeys(ctx, p, old, &nk, revived); err != nil {
		if !restored {
			return nil, fmt.Errorf("lifecycle: rotate keys of %s (the previous keys could not be restored; check the project's services): %w", ref, err)
		}
		return nil, fmt.Errorf("lifecycle: rotate keys of %s (previous keys restored): %w", ref, err)
	}
	e.event(ctx, ref, "project.keys_rotated", nil)
	// The new keys are in force from here, so they are returned with the error of a replica.
	return &nk, e.updateReplicaKeys(ctx, p, &nk)
}

// KeyStep names the step of a key change that failed, for JWTSecretError.
type KeyStep string

const (
	// KeyStepStore: sealing the keys into the registry.
	KeyStepStore KeyStep = "store"
	// KeyStepRestart: re-rendering the env files and restarting GoTrue and PostgREST.
	KeyStepRestart KeyStep = "restart"
	// KeyStepTenants: sending the keys to the fleet tenants (Realtime, Storage).
	KeyStepTenants KeyStep = "tenants"
)

// JWTSecretError is the failure of SetJWTSecret after it began to apply the secret. The
// previous secret and keys were put back unless NotRestored is set, which means that putting
// them back failed too (the log names the step) and the project's services may still run on
// the new secret or not run at all. Step says where the change stopped.
type JWTSecretError struct {
	Step        KeyStep
	Err         error
	NotRestored bool
}

func (e *JWTSecretError) Error() string {
	if e.NotRestored {
		return fmt.Sprintf("lifecycle: change the JWT secret (failed at %s, and the previous secret could not be restored; check the project's services): %v", e.Step, e.Err)
	}
	return fmt.Sprintf("lifecycle: change the JWT secret (previous secret restored; failed at %s): %v", e.Step, e.Err)
}

func (e *JWTSecretError) Unwrap() error { return e.Err }

// SetJWTSecret implements JWTSecretSetter: the project's legacy JWT secret becomes secret,
// the dashboard's "Change legacy JWT secret" with a secret of your own. The anon and
// service_role keys are signed again with it (the keys the project had are gone, as on
// hosted); the publishable and secret keys, the database passwords and the pgsodium root key
// stay. Everything that checks a JWT follows it, in this order: the sealed keys in the
// registry (the proxy's key cache drops them at once), the env files of GoTrue and PostgREST
// and their restart, the Realtime and Storage tenants, and last the PostgREST of each running
// read replica, which its own node restarts (ReplicaFleet.RestartAPI). A paused project only gets the
// stored keys and applies them when it resumes; its replicas that run get them at once, because they
// keep running while the project is paused. If a step up to the tenants fails the previous keys are
// stored and applied again, and the error is a *JWTSecretError. A replica that cannot be updated does
// not undo the change (that would put the previous secret back on the primary): the keys come back
// with a *ReplicaKeysError that names it. Setting the secret the project already has changes nothing
// on the primary, and updates the replicas again, which is how a replica that missed a change gets it.
// The caller restarts what this package does not run: the Edge Functions environment
// (internal/functions).
func (e *Engine) SetJWTSecret(ctx context.Context, ref, secret string) (*secrets.ProjectKeys, error) {
	if ref == config.SystemRef {
		return nil, fmt.Errorf("%w: the JWT secret of the system project is the dashboard's own", ErrInvalidState)
	}
	if err := secrets.CheckJWTSecret(secret); err != nil {
		return nil, fmt.Errorf("lifecycle: %w", err)
	}
	if err := e.upgradeBusy(ref, "change the JWT secret of"); err != nil {
		return nil, err
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := e.atHome(p, "change the JWT secret of"); err != nil {
		return nil, err
	}
	if p.Status != registry.StatusInactive && !p.Status.Running() {
		return nil, invalidState(p, "change the JWT secret of")
	}
	old, err := e.loadKeys(ctx, ref)
	if err != nil {
		return nil, err
	}
	if old.JWTSecret == secret {
		// Nothing changes for the primary. A replica that missed the change (its node was away) takes the
		// secret now; one that has it is left running (its unit renders the same).
		return old, e.updateReplicaKeys(ctx, p, old)
	}
	nk := *old
	nk.JWTSecret = secret
	if err := nk.ResignLegacy(ref, e.opts.Now()); err != nil {
		return nil, err
	}
	if step, restored, err := e.applyKeys(ctx, p, old, &nk, nil); err != nil {
		return nil, &JWTSecretError{Step: step, Err: err, NotRestored: !restored}
	}
	e.event(ctx, ref, "project.jwt_secret_changed", nil)
	// The new secret is in force from here, so the keys are returned with the error of a replica.
	return &nk, e.updateReplicaKeys(ctx, p, &nk)
}

// applyKeys makes nk the credentials of p: sealed in the registry (with revived, the default
// key records the new keys bring back), rendered into the env files of GoTrue and PostgREST,
// which restart one after the other and are health-checked, and sent to the fleet tenants. A
// paused project only gets the stored keys. When a step fails, old and its records are stored
// and applied again and applyKeys returns the failed step with the cause. restored is false
// when putting the previous keys back failed as well (each failure is logged with the ref):
// then the registry, the units or a tenant may hold the new keys. The caller holds the
// project's lock and has loaded old.
func (e *Engine) applyKeys(ctx context.Context, p *registry.Project, old, nk *secrets.ProjectKeys, revived []secrets.APIKeyRecord) (KeyStep, bool, error) {
	ref := p.Ref
	rollback := func(step KeyStep, cause error) (KeyStep, bool, error) {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		restored := true
		if err := e.storeKeys(cctx, ref, old); err != nil {
			restored = false
			e.log.Error("keys: could not restore previous keys", "ref", ref, "error", err)
		} else if err := e.storeRecords(cctx, ref, old, revived); err != nil {
			restored = false
			e.log.Error("keys: could not restore the previous key records", "ref", ref, "error", err)
		} else if p.Status.Running() {
			if err := e.plane.Reconfigure(cctx, p, old); err != nil {
				restored = false
				e.log.Error("keys: could not restart on previous keys", "ref", ref, "error", err)
			}
			// Only a failed tenant update can have left a tenant on the new keys.
			if step == KeyStepTenants && len(e.opts.Fleet) > 0 && ref != config.SystemRef {
				spec, err := e.tenantSpec(cctx, p, old)
				if err == nil {
					err = e.opts.Fleet.EnsureTenant(cctx, spec)
				}
				if err != nil {
					restored = false
					e.log.Error("keys: could not put the previous keys back into the fleet tenants", "ref", ref, "error", err)
				}
			}
		}
		return step, restored, cause
	}
	if err := e.storeKeys(ctx, ref, nk); err != nil {
		return rollback(KeyStepStore, err)
	}
	if err := e.putRecords(ctx, ref, revived); err != nil {
		return rollback(KeyStepStore, err)
	}
	if !p.Status.Running() {
		return "", true, nil
	}
	if err := e.plane.Reconfigure(ctx, p, nk); err != nil {
		return rollback(KeyStepRestart, err)
	}
	if len(e.opts.Fleet) > 0 && ref != config.SystemRef {
		spec, err := e.tenantSpec(ctx, p, nk)
		if err == nil {
			err = e.opts.Fleet.EnsureTenant(ctx, spec)
		}
		if err != nil {
			return rollback(KeyStepTenants, fmt.Errorf("update fleet tenants: %w", err))
		}
	}
	return "", true, nil
}

// StartActive starts every project the registry lists as ACTIVE (after a reboot, or
// after `supavise system stop`), one at a time, and returns the errors by ref. A project
// that fails to start is marked ACTIVE_UNHEALTHY; paused projects are left alone.
func (e *Engine) StartActive(ctx context.Context) map[string]error {
	errs := map[string]error{}
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		errs[""] = err
		return errs
	}
	for i := range ps {
		p := ps[i]
		if p.Ref == config.SystemRef || !p.Status.Running() || !e.homedHere(&p) {
			continue
		}
		if err := e.startOne(ctx, &p); err != nil {
			errs[p.Ref] = err
		}
	}
	return errs
}

// EnsureTenants registers every active project with the shared services again, one at a time, and
// returns the errors by ref. The calls are idempotent: a tenant whose configuration and whose
// services' releases are unchanged sends nothing. After a Supavise release moved the pins of Storage
// or Realtime, the new tag changes their tenants' fingerprints, so this call is what makes the new
// release run its tenant migrations in every project (Storage on the tenant update, Realtime on the
// tenant create). The daemon calls it once the shared services and the projects have started.
//
// The shared services run on the leader, whichever node a project is homed on, so a writable Engine
// registers every active project, the ones homed elsewhere too (their canonical ports are forwarders
// to the home): a shared-service database that was reset, or a tenant whose fingerprint changed, is
// put right for all of them. A read-only Engine (a follower) registers the projects homed on it only.
func (e *Engine) EnsureTenants(ctx context.Context) map[string]error {
	errs := map[string]error{}
	if len(e.opts.Fleet) == 0 {
		return errs
	}
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		errs[""] = err
		return errs
	}
	for i := range ps {
		if ps[i].Ref == config.SystemRef || !ps[i].Status.Running() || (e.opts.ReadOnly && !e.homedHere(&ps[i])) {
			continue
		}
		if err := e.ensureTenantsLocked(ctx, ps[i].Ref); err != nil {
			errs[ps[i].Ref] = err
		}
	}
	return errs
}

// EnsureTenant registers ref, and its replicas that are up, with the shared services again and asks
// the other nodes that run Supavisor to drop their cached copy: the call that follows a move of the
// project's home (the failover orchestrator's Fleet), once its database answers at the new home, so
// that Realtime creates its replication slot there. It does not take the project's lock: the
// orchestrator already holds it through Lock for the steps of a move, and the lock is not reentrant. A
// caller that needs the call serialized with pause, resume and upgrade takes Lock first.
func (e *Engine) EnsureTenant(ctx context.Context, ref string) error {
	if len(e.opts.Fleet) == 0 || ref == config.SystemRef {
		return nil
	}
	return e.ensureTenantsOf(ctx, ref, true)
}

// ensureTenantsLocked is ensureTenantsOf under the project's lock, for the sweep at start.
func (e *Engine) ensureTenantsLocked(ctx context.Context, ref string) error {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	return e.ensureTenantsOf(ctx, ref, false)
}

// QuiesceTenant asks the shared services to let go of ref's database and keeps their clients out until
// the project's cluster has stopped (the failover orchestrator's Fleet, before a planned stop):
// Realtime's logical walsender otherwise holds the shutdown until it times out. Unlike the quiesce of
// a pause, it reports the failure, so that the orchestrator can decide.
func (e *Engine) QuiesceTenant(ctx context.Context, ref string) error {
	if len(e.opts.Fleet) == 0 || ref == config.SystemRef {
		return nil
	}
	return e.opts.Fleet.QuiesceTenant(ctx, ref)
}

// Lock takes the project's operation lock, the one pause, resume, delete, restore and upgrade take,
// and returns the function that releases it. The failover orchestrator holds it for the steps of a move
// so that none of them runs on the project meanwhile.
func (e *Engine) Lock(ctx context.Context, ref string) (unlock func(), err error) {
	return e.lock(ctx, ref)
}

// SetPeerRefresher sets Options.PeerRefresh on an Engine that was built without it. The daemon calls
// it while it wires the fleet, before the Engine serves a request.
func (e *Engine) SetPeerRefresher(r fleet.PeerRefresher) { e.opts.PeerRefresh = r }

// refreshPeers asks the other nodes that run Supavisor to drop their copy of tenant after this node
// refreshed its own. A node that cannot be reached costs a warning and nothing else: the refresh is
// idempotent and the next change of the tenant sends it again.
func (e *Engine) refreshPeers(ctx context.Context, tenant string) {
	if e.opts.PeerRefresh == nil {
		return
	}
	if err := e.opts.PeerRefresh.RefreshPeers(ctx, tenant); err != nil {
		e.log.Warn("the other nodes' poolers could not drop their cached copy of a tenant; they may keep the old one until they restart or the tenant changes again", "tenant", tenant, "error", err)
	}
}

// ensureTenantsOf registers ref with the shared services, and with it the tenants of its replicas
// that are up. peers asks the other nodes to drop their cached copy afterwards, which a call that
// moved the project's home needs and the daemon's sweep at start does not (a follower learns the
// tenant rows by replication when it starts). The caller holds the project's lock.
func (e *Engine) ensureTenantsOf(ctx context.Context, ref string, peers bool) error {
	p, err := e.reg.GetProject(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// A project that is moving to another home stays RESTARTING until the move ends, and the move registers
	// it at the new home while it is (peers is the call of a move). The sweep at start leaves every project
	// that is not active to its own operation.
	if !p.Status.Running() && !(peers && p.Status == registry.StatusRestarting) {
		return nil // paused, upgrading or being restored since the list: its own operation registers it
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return err
	}
	spec, err := e.tenantSpec(ctx, p, keys)
	if err != nil {
		return err
	}
	if err := e.opts.Fleet.EnsureTenant(ctx, spec); err != nil {
		return err
	}
	if peers {
		e.refreshPeers(ctx, ref)
	}
	return e.ensureReplicaTenants(ctx, p, spec, peers)
}

// ensureReplicaTenants registers the replicas of p that are up with the shared services, each as a
// tenant of its own with the replica's identifier and port (fleet.TenantSpec.ReplicaID). The
// project's own tenant comes first: it sets the pooler password the replica replicates.
func (e *Engine) ensureReplicaTenants(ctx context.Context, p *registry.Project, base fleet.TenantSpec, peers bool) error {
	if p.Seq > e.cfg.MaxReplicaSeq() {
		return nil
	}
	rs, err := e.reg.ListReplicas(ctx, p.Ref)
	if err != nil {
		return err
	}
	var first error
	for _, r := range rs {
		if !registry.Status(r.Status).Running() {
			continue // being set up or removed: the replica controller registers it when it is healthy
		}
		spec := base
		spec.ReplicaID, spec.DBPort = r.Identifier, e.cfg.ReplicaPorts(p.Ref, p.Seq).Postgres
		if err := e.opts.Fleet.EnsureReplicaTenant(ctx, spec); err != nil {
			if first == nil {
				first = fmt.Errorf("replica %s: %w", r.Identifier, err)
			}
			continue
		}
		if peers {
			e.refreshPeers(ctx, r.Identifier)
		}
	}
	return first
}

func (e *Engine) startOne(ctx context.Context, listed *registry.Project) error {
	unlock, err := e.lock(ctx, listed.Ref)
	if err != nil {
		return err
	}
	defer unlock()
	// StartActive listed the projects before this lock, and supavise serve runs it next to the
	// API: a pause or delete that landed in between must not be undone (the units started
	// again, the backup timer running, while the registry says INACTIVE). Decide on the row
	// as it is now.
	p, err := e.reg.GetProject(ctx, listed.Ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !p.Status.Running() {
		return nil
	}
	// A primary that a peer replaced does not start at boot, whatever this node's copy of the registry
	// says. Its status is not touched either: the copy is the diverged one, and the node is rebuilt.
	if err := fencedErr(e.cfg, p.Ref); err != nil {
		return err
	}
	keys, err := e.loadKeys(ctx, p.Ref)
	if err == nil {
		err = e.plane.Start(ctx, p, keys)
	}
	want := registry.StatusActiveHealthy
	if err != nil {
		want = registry.StatusActiveUnhealthy
	}
	if want != p.Status {
		_ = e.reg.SetProjectStatus(ctx, p.Ref, want)
	}
	if err == nil {
		e.startTimer(ctx, p.Ref)
	}
	return err
}

// Keys implements Manager.
func (e *Engine) Keys(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
	if _, err := e.reg.GetProject(ctx, ref); err != nil {
		return nil, err
	}
	return e.loadKeys(ctx, ref)
}

// Health implements Manager. For an ACTIVE project it also moves the registry status
// between ACTIVE_HEALTHY and ACTIVE_UNHEALTHY to match what the checks found.
func (e *Engine) Health(ctx context.Context, ref string) ([]ServiceHealth, error) {
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	// The units of a project that runs elsewhere are not this node's to judge: its plane would find the
	// replica's units, or none, and the verdict would be written to the registry.
	if err := e.atHome(p, "check the health of"); err != nil {
		return nil, err
	}
	hs := e.plane.Health(ctx, p, nil)
	if p.Status.Running() && !e.opts.ReadOnly {
		want := registry.StatusActiveHealthy
		for _, h := range hs {
			if !h.Healthy {
				want = registry.StatusActiveUnhealthy
			}
		}
		if want != p.Status {
			e.settleStatus(ctx, ref, p.Status, want)
		}
	}
	return hs, nil
}

// settleStatus moves ref from the status a health check started under to want, but only
// if the project still has that status once it holds the per-ref lock. The checks run
// without the lock and can take seconds; a Pause, Resume or Delete that finished in the
// meantime owns the status, and a stale ACTIVE_UNHEALTHY must not overwrite INACTIVE.
func (e *Engine) settleStatus(ctx context.Context, ref string, from, want registry.Status) {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		e.log.Warn("health: lock for status update", "ref", ref, "error", err)
		return
	}
	defer unlock()
	cur, err := e.reg.GetProject(ctx, ref)
	if err != nil || cur.Status != from {
		return
	}
	if err := e.reg.SetProjectStatus(ctx, ref, want); err != nil {
		e.log.Warn("health: update status", "ref", ref, "error", err)
	}
}

// ConnString implements Manager.
func (e *Engine) ConnString(ctx context.Context, ref, role string) (string, error) {
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return "", err
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return "", err
	}
	var pw string
	switch role {
	case RolePostgres:
		pw = keys.DBPassword
	case RoleAdmin:
		pw = keys.AdminPassword
	default:
		return "", fmt.Errorf("lifecycle: ConnString supports roles %q and %q, not %q", RolePostgres, RoleAdmin, role)
	}
	return dsnURL(role, pw, e.cfg.PortsFor(ref, p.Seq).Postgres, "postgres"), nil
}

// Recovered describes one project Recover moved out of a transitional status.
type Recovered struct {
	Ref  string
	From registry.Status
	To   registry.Status
	Note string
	// Resume is set for a project whose restart was cut after the pause: it is INACTIVE
	// and ResumeRecovered brings it back.
	Resume bool
}

// StatusDeleted is the To of a Recovered entry for a delete Recover finished.
const StatusDeleted registry.Status = "DELETED"

// Recover runs once when the daemon starts, before StartActive. A crash or restart in
// the middle of an operation leaves the project in a transitional status that nothing
// else would ever move again: COMING_UP (create or resume), PAUSING and RESTARTING.
//
//   - PAUSING: the pause is finished (units stopped), status INACTIVE.
//
//   - UPGRADING: when nobody holds the project's upgrade (the process that ran it is gone), the
//     units stop, the upgrade is marked failed (its backup id stays in its events), and the
//     project becomes ACTIVE_UNHEALTHY, so that StartActive starts it on the versions the
//     registry recorded, the previous ones. An upgrade that another process (the CLI) still
//     runs is left alone; SettleUpgrades picks it up if that process dies later. A recorded
//     upgrade that never got its final status is settled as well (settleUpgradeRows).
//
//   - COMING_UP or RESTARTING with a route: the project was created before and a resume
//     or restart was cut short; its units are stopped and it becomes INACTIVE, so that
//     Resume can be run again.
//
//   - COMING_UP with no route: the create never finished. INIT_FAILED, with the data
//     left in place; Delete cleans it up.
//
//   - GOING_DOWN: an interrupted delete. If its final-backup step had settled (the events
//     say so), the removal is finished now with no second backup. If the backup was cut
//     off, the project returns to the status it had and is kept (a paused project's
//     database is stopped again). A delete from a version that recorded no events is only
//     logged: run `supavise projects delete <ref> --skip-final-backup` if its data is gone.
//
//   - A restart (the API's pause then resume) cut after the pause is recorded as an
//     intent; the project is INACTIVE and Recovered.Resume is set, so ResumeRecovered
//     brings it back. A stale intent on a project that is not paused is cleared.
//
// RESTORING is not touched: a restore is for its operator to judge. A project homed on another node
// is that node's to recover, and a read-only Engine (a follower) recovers nothing.
func (e *Engine) Recover(ctx context.Context) []Recovered {
	if e.opts.ReadOnly {
		return nil // a follower settles nothing: it cannot write the registry
	}
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		e.log.Error("recover: listing projects", "error", err)
		return nil
	}
	// A project homed on another node is that node's to recover.
	ps = slices.DeleteFunc(ps, func(p registry.Project) bool { return !e.homedHere(&p) })
	routes := map[string]bool{}
	if rs, err := e.reg.ListRoutes(ctx); err == nil {
		for _, r := range rs {
			routes[r.Ref] = true
		}
	}
	var out []Recovered
	var finishDelete []string
	for i := range ps {
		p := ps[i]
		if p.Ref == config.SystemRef {
			continue
		}
		var to registry.Status
		note := ""
		switch p.Status {
		case registry.StatusPausing:
			to, note = registry.StatusInactive, "the daemon stopped during a pause"
		case registry.StatusResizing:
			// The registry holds the size the resize was going to; the project starts on it.
			to, note = registry.StatusInactive, "the daemon stopped during a resize; the project is started again on the size recorded for it"
		case registry.StatusComingUp, registry.StatusRestarting:
			if routes[p.Ref] {
				to, note = registry.StatusInactive, "the daemon stopped during a resume or restart"
			} else {
				to, note = registry.StatusInitFailed, "the daemon stopped before the project finished creating; delete it and create it again"
			}
		case registry.StatusUpgrading:
			if e.runnerLive(ctx, p.Ref) {
				e.log.Info("recover: another process is still upgrading the project; leaving it to that process", "ref", p.Ref)
				continue
			}
			if r, ok := e.settleUpgrading(ctx, p.Ref); ok {
				out = append(out, r)
			}
			continue
		case registry.StatusGoingDown:
			dp := e.deleteProgress(ctx, p.Ref)
			switch {
			case !dp.known:
				e.log.Warn("recover: project is GOING_DOWN from an interrupted delete that left no record; run `supavise projects delete "+p.Ref+"` again, with --skip-final-backup if its data is already gone", "ref", p.Ref)
				continue
			case dp.backupDone:
				finishDelete = append(finishDelete, p.Ref)
				continue
			}
			to, note = dp.prev, "the daemon stopped during the final backup of a delete; the project is kept"
		default:
			continue
		}
		unlock, err := e.lock(ctx, p.Ref)
		if err != nil {
			e.log.Warn("recover: lock", "ref", p.Ref, "error", err)
			continue
		}
		cur, err := e.reg.GetProject(ctx, p.Ref)
		if err == nil && cur.Status == p.Status {
			if to == registry.StatusInactive {
				if serr := e.plane.Stop(ctx, p.Ref); serr != nil {
					e.log.Warn("recover: stopping units", "ref", p.Ref, "error", serr)
				}
			}
			if err := e.reg.SetProjectStatus(ctx, p.Ref, to); err != nil {
				e.log.Error("recover: set status", "ref", p.Ref, "error", err)
			} else {
				e.event(ctx, p.Ref, "project.recovered", map[string]string{"from": string(p.Status), "to": string(to), "note": note})
				e.log.Warn("recovered project after an interrupted operation", "ref", p.Ref, "from", p.Status, "to", to, "note", note)
				out = append(out, Recovered{Ref: p.Ref, From: p.Status, To: to, Note: note, Resume: p.Status == registry.StatusResizing})
			}
		}
		unlock()
	}
	for _, ref := range finishDelete {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := e.Delete(dctx, ref)
		cancel()
		if err != nil {
			e.log.Error("recover: could not finish an interrupted delete; run `supavise projects delete "+ref+"` again", "ref", ref, "error", err)
			continue
		}
		e.log.Warn("finished a delete the daemon had interrupted", "ref", ref)
		out = append(out, Recovered{Ref: ref, From: registry.StatusGoingDown, To: StatusDeleted, Note: "the daemon stopped during a delete; the removal was finished"})
	}
	out = e.recoverRestarts(ctx, ps, out)
	e.settleUpgradeRows(ctx, ps)
	return out
}

// restartPending reports whether the newest restart event of ref is an unfinished request.
func (e *Engine) restartPending(ctx context.Context, ref string) bool {
	evs, err := e.reg.ListEvents(ctx, ref, 200) // newest first
	if err != nil {
		return false
	}
	for _, ev := range evs {
		switch ev.Kind {
		case EventRestartFinished:
			return false
		case EventRestartRequested:
			return true
		}
	}
	return false
}

// recoverRestarts marks the projects whose restart was cut after the pause for
// ResumeRecovered and clears intents that no longer apply.
func (e *Engine) recoverRestarts(ctx context.Context, ps []registry.Project, out []Recovered) []Recovered {
	for i := range ps {
		ref := ps[i].Ref
		if ref == config.SystemRef || !e.restartPending(ctx, ref) {
			continue
		}
		cur, err := e.reg.GetProject(ctx, ref)
		if err != nil {
			continue
		}
		if cur.Status != registry.StatusInactive {
			e.event(ctx, ref, EventRestartFinished, map[string]string{"result": "cleared: project is " + string(cur.Status)})
			continue
		}
		found := false
		for k := range out {
			if out[k].Ref == ref {
				out[k].Resume, found = true, true
			}
		}
		if !found {
			out = append(out, Recovered{Ref: ref, From: cur.Status, To: cur.Status, Note: "the daemon stopped during a restart", Resume: true})
		}
	}
	return out
}

// ResumeRecovered resumes the projects Recover flagged (a restart cut after its pause)
// one at a time and closes each restart intent. It returns the failures by ref; a
// project that does not resume stays INACTIVE and can be resumed by hand.
func (e *Engine) ResumeRecovered(ctx context.Context, rs []Recovered) map[string]error {
	errs := map[string]error{}
	for _, r := range rs {
		if !r.Resume {
			continue
		}
		err := e.resume(ctx, r.Ref, false)
		res := "resumed"
		if err != nil {
			errs[r.Ref] = err
			res = "failed: " + err.Error()
		}
		e.event(context.WithoutCancel(ctx), r.Ref, EventRestartFinished, map[string]string{"result": res})
	}
	return errs
}

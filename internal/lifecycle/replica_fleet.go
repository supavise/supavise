package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// ReplicaFleet reaches the replicas of a project on the nodes that hold them. internal/placement
// implements it over the peer API; the Engine uses it to restart a project's replicas in the right
// order when the project is resized.
type ReplicaFleet interface {
	// Replicas lists the replicas of ref.
	Replicas(ctx context.Context, ref string) ([]registry.Replica, error)
	// Restart restarts the replica on its node so that it runs on class, the compute size to
	// render: the node's copy of the registry may not have the new size yet.
	Restart(ctx context.Context, r registry.Replica, class string) error
	// RestartAPI renders the replica's PostgREST again on its node, from the project's keys, and
	// restarts it when its unit changed (the standby's Postgres keeps running). keysDigest is
	// secrets.ProjectKeys.JWTDigest of the keys the node must render: the node's copy of the registry may
	// not hold them yet, and the node then waits for it, or fails the call, instead of rendering the
	// previous secret. It returns once PostgREST answers.
	RestartAPI(ctx context.Context, r registry.Replica, keysDigest string) error
	// Failed is told of a replica that did not come back after the Engine restarted it, so that the
	// node can raise the alert; it must not block.
	Failed(ctx context.Context, r registry.Replica, cause error)
}

// SetReplicaFleet sets Options.Replicas on an Engine that was built without it. The daemon calls it
// while it wires the cluster features, before the Engine serves a request.
func (e *Engine) SetReplicaFleet(f ReplicaFleet) { e.opts.Replicas = f }

// EventReplicaResizeFailed is the event a replica that did not come back on a new size leaves
// on its project.
const EventReplicaResizeFailed = "replica.resize_failed"

// EventReplicaRestartFailed is the event a replica that did not come back after the project's
// Postgres settings made the primary restart leaves on its project.
const EventReplicaRestartFailed = "replica.restart_failed"

// resizeReplicas restarts every replica of p on the size p has now. A replica that is still being
// set up or removed is left alone (it renders from the project row when it starts). A failure marks
// the replica ACTIVE_UNHEALTHY, records an event and tells the fleet; it never returns an error,
// because a replica must not stop the primary's resize.
func (e *Engine) resizeReplicas(ctx context.Context, p *registry.Project) {
	e.restartReplicas(ctx, p, "resize to "+p.Class, EventReplicaResizeFailed, registry.StatusResizing)
}

// replicaRestartConcurrency is how many replicas restart at once, and defaultReplicaRestartTimeout how
// long one may take (Options.ReplicaRestartTimeout): a standby waits for its base of recovery, up to
// ten minutes, and a node that does not answer would hold a resize or a settings save for that long.
const (
	replicaRestartConcurrency    = 4
	defaultReplicaRestartTimeout = 5 * time.Minute
)

// restartReplicas restarts every replica of p on the size p has now and tells the fleet of the ones
// that do not come back; why says what made the restart necessary. During is the status the registry
// shows while a replica restarts ("" leaves it as it is). The replicas restart together, a few at a
// time, and each has a deadline, so that a slow or unreachable node holds neither the primary's resize
// nor the save of a setting for long: a replica must not stop the primary.
func (e *Engine) restartReplicas(ctx context.Context, p *registry.Project, why, failEvent string, during registry.Status) {
	if e.opts.Replicas == nil || p.Ref == config.SystemRef {
		return
	}
	rs, err := e.opts.Replicas.Replicas(ctx, p.Ref)
	if err != nil {
		e.log.Warn("restarting the replicas: listing them", "ref", p.Ref, "why", why, "error", err)
		return
	}
	sem := make(chan struct{}, replicaRestartConcurrency)
	var wg sync.WaitGroup
	for _, r := range rs {
		if !registry.Status(r.Status).Running() {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			e.restartReplica(ctx, p, r, why, failEvent, during)
		}()
	}
	wg.Wait()
}

// recordCtx is the context for the writes that record how a replica's restart went. It outlives the
// caller, and its budget starts when it is made, so it is made where the writes are.
func (e *Engine) recordCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := e.recordBudget
	if budget <= 0 {
		budget = cleanupBudget
	}
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

// restartReplica restarts one replica of p and records how it went.
func (e *Engine) restartReplica(ctx context.Context, p *registry.Project, r registry.Replica, why, failEvent string, during registry.Status) {
	if during != "" {
		dctx, cancel := e.recordCtx(ctx)
		err := e.reg.SetReplicaStatus(dctx, r.Identifier, string(during), r.InitStep, r.InitError)
		cancel()
		if err != nil {
			e.log.Warn("restarting the replicas: marking a replica "+string(during), "replica", r.Identifier, "error", err)
		}
	}
	timeout := e.opts.ReplicaRestartTimeout
	if timeout <= 0 {
		timeout = defaultReplicaRestartTimeout
	}
	rctx, stop := context.WithTimeout(ctx, timeout)
	err := e.opts.Replicas.Restart(rctx, r, p.Class)
	stop()
	// The outcome is written on a context made now: the restart may have taken the whole deadline, longer
	// than a context made before it would last, or the caller's context may be gone. It is recorded either way.
	cctx, cancel := e.recordCtx(ctx)
	defer cancel()
	status := registry.StatusActiveHealthy
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// The caller gave up (a stopped daemon, a cancelled request): the replica was not judged, so it
		// goes back to the status it had and nobody is told.
		status = registry.Status(r.Status)
		e.log.Warn("restarting the replicas: stopped before a replica came back", "replica", r.Identifier, "why", why, "error", err)
	default:
		status = registry.StatusActiveUnhealthy
		e.log.Warn("a replica did not come back after its restart", "replica", r.Identifier, "node", r.NodeID, "why", why, "error", err)
		e.event(cctx, p.Ref, failEvent, map[string]any{"replica": r.Identifier, "node": r.NodeID, "class": p.Class, "error": err.Error()})
		e.opts.Replicas.Failed(cctx, r, fmt.Errorf("%s: %w", why, err))
	}
	if err := e.reg.SetReplicaStatus(cctx, r.Identifier, string(status), r.InitStep, r.InitError); err != nil {
		e.log.Warn("restarting the replicas: recording a replica's status", "replica", r.Identifier, "error", err)
	}
}

// EventReplicaKeysFailed is the event a replica whose PostgREST could not be updated to a changed JWT
// secret leaves on its project.
const EventReplicaKeysFailed = "replica.keys_failed"

// defaultReplicaKeysTimeout is how long one replica may take to render and restart its PostgREST on new
// keys (Options.ReplicaRestartTimeout, when set, replaces it). The call does not wait for a standby to
// replay anything, so it is shorter than a restart of the whole replica.
const defaultReplicaKeysTimeout = 2 * time.Minute

// errNoReplicaFleet is why a process that has no way to reach the nodes of a project's replicas (the
// command line) could not update them.
var errNoReplicaFleet = errors.New("this process cannot reach the replica's node; the daemon can")

// ReplicaKeyFailure is a replica whose PostgREST may still run on the previous JWT secret.
type ReplicaKeyFailure struct {
	Identifier string
	NodeID     string
	Err        error
}

// ReplicaKeysError is the failure of the last step of a change of a project's keys (SetJWTSecret,
// RotateKeys): the new keys are stored and in force on the project's own services and tenants, and
// the PostgREST of the replicas in Failed could not be updated, or the replicas could not be listed
// (Err). Nothing was rolled back: putting the previous keys back would restore the secret the change
// replaced, which is usually the reason for the change, on the primary and on every replica that had
// taken the new one. A replica that is listed here accepts JWTs signed with the previous secret and
// refuses the new keys until its PostgREST restarts: restarting the replica from Studio does it, and so
// does SetJWTSecret with the secret the project has now (it touches the primary no more).
type ReplicaKeysError struct {
	Failed []ReplicaKeyFailure
	// Err is why the replicas could not be listed, when Failed is empty.
	Err error
}

func (e *ReplicaKeysError) Error() string {
	const advice = "restart each replica (Studio: Database, Read Replicas) or save the project's current JWT secret again (Project settings, JWT keys, Legacy JWT secret); until then it refuses the new keys and trusts the previous secret"
	if len(e.Failed) == 0 {
		return fmt.Sprintf("lifecycle: the new keys are in force, but the project's read replicas could not be listed, so their PostgREST may still run on the previous JWT secret (%v): %s", e.Err, advice)
	}
	parts := make([]string, len(e.Failed))
	for i, f := range e.Failed {
		parts[i] = fmt.Sprintf("%s on %s: %v", f.Identifier, f.NodeID, f.Err)
	}
	return fmt.Sprintf("lifecycle: the new keys are in force, but the PostgREST of %d read replica(s) still runs on the previous JWT secret (%s): %s", len(e.Failed), strings.Join(parts, "; "), advice)
}

// Unwrap lets errors.Is and errors.As look at the cause of each replica.
func (e *ReplicaKeysError) Unwrap() []error {
	if len(e.Failed) == 0 {
		return []error{e.Err}
	}
	out := make([]error, len(e.Failed))
	for i, f := range e.Failed {
		out[i] = f.Err
	}
	return out
}

// updateReplicaKeys makes the PostgREST of every running replica of p run on keys: each replica's node
// renders the unit again from the keys and restarts it if they changed it, so that the replica endpoint
// and the load balancer (which sends reads to the replicas) accept the new keys and stop accepting JWTs
// signed with the previous secret. It runs after the primary and the tenants took the keys, so that a
// change that had to be rolled back never reached a replica. Realtime and Storage have one tenant per
// project on the leader and none per replica; Supavisor's replica tenants carry no JWT secret.
//
// The replicas are updated together, a few at a time, each under a deadline. A replica that does not
// answer, or whose PostgREST does not come back, is reported in a *ReplicaKeysError after the others
// finished; the keys stay (see ReplicaKeysError). A replica whose setup is not complete or that is being
// removed is left alone: a replica reads the keys when it starts. A process with no ReplicaFleet (the
// command line) reads the replicas from the registry and fails for each one that runs, because it cannot
// reach their nodes. The caller holds the project's lock.
func (e *Engine) updateReplicaKeys(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	if p.Ref == config.SystemRef {
		return nil
	}
	var rs []registry.Replica
	var err error
	if e.opts.Replicas != nil {
		rs, err = e.opts.Replicas.Replicas(ctx, p.Ref)
	} else {
		rs, err = e.reg.ListReplicas(ctx, p.Ref)
	}
	if err != nil {
		e.log.Warn("updating the replicas to new keys: listing them", "ref", p.Ref, "error", err)
		return &ReplicaKeysError{Err: err}
	}
	digest := keys.JWTDigest()
	var mu sync.Mutex
	var failed []ReplicaKeyFailure
	fail := func(r registry.Replica, err error) {
		mu.Lock()
		defer mu.Unlock()
		failed = append(failed, ReplicaKeyFailure{Identifier: r.Identifier, NodeID: r.NodeID, Err: err})
	}
	sem := make(chan struct{}, replicaRestartConcurrency)
	var wg sync.WaitGroup
	for _, r := range rs {
		if !registry.Status(r.Status).Running() {
			continue
		}
		if e.opts.Replicas == nil {
			fail(r, errNoReplicaFleet)
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := e.updateReplicaKeysOf(ctx, p, r, digest); err != nil {
				fail(r, err)
			}
		}()
	}
	wg.Wait()
	if len(failed) == 0 {
		return nil
	}
	slices.SortFunc(failed, func(a, b ReplicaKeyFailure) int { return strings.Compare(a.Identifier, b.Identifier) })
	for _, f := range failed {
		e.log.Warn("a replica's PostgREST could not be updated to the new keys", "ref", p.Ref, "replica", f.Identifier, "node", f.NodeID, "error", f.Err)
	}
	return &ReplicaKeysError{Failed: failed}
}

// updateReplicaKeysOf updates one replica and records how it went.
func (e *Engine) updateReplicaKeysOf(ctx context.Context, p *registry.Project, r registry.Replica, digest string) error {
	timeout := e.opts.ReplicaRestartTimeout
	if timeout <= 0 {
		timeout = defaultReplicaKeysTimeout
	}
	rctx, stop := context.WithTimeout(ctx, timeout)
	err := e.opts.Replicas.RestartAPI(rctx, r, digest)
	stop()
	if err == nil {
		return nil
	}
	// The event is written on a context made now: the call may have taken its whole deadline.
	cctx, cancel := e.recordCtx(ctx)
	defer cancel()
	e.event(cctx, p.Ref, EventReplicaKeysFailed, map[string]any{"replica": r.Identifier, "node": r.NodeID, "error": err.Error()})
	return err
}

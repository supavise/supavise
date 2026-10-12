package branching

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// Idle pause. A preview (non-persistent) branch that nothing has used for [branching] idle_pause_after
// is paused as a whole: its Postgres, GoTrue and PostgREST stop and the status becomes INACTIVE. The
// first HTTP request to it, or the first database client, starts it again. Nothing is deleted and
// nothing is lost; a paused branch holds no memory on the node.
//
// What counts as use, in order of cost:
//
//   - the branch was seen running less than the period ago (a restart of the daemon, a create, a
//     wake, a resume by hand all start the clock again);
//   - an HTTP request to its API (any host: derived, custom or vanity) in the period, or one still
//     being served, a WebSocket included (internal/reqstats);
//   - a Management API call that ran SQL on the branch in the period: Studio's SQL and table editors,
//     MCP's execute_sql and apply_migration, a migration the CLI pushed. Each opens a session and closes
//     it before it answers, which no sample of the database can see, so api.Server.running marks the
//     branch used (reqstats.Counter.Touch);
//   - a database client the sweep finds connected: one running a statement now, or whose last
//     statement ended less than the period ago (the pooler's clients, a psql session that is still open).
//     A session that opened and closed between two sweeps leaves nothing to find. The services that
//     only act for an API request (GoTrue, PostgREST, Storage) and pg_cron are not clients, because
//     their work already shows as an API request or is the branch's own;
//   - an operation of the service on the branch: a diff, a merge, a push, a reset or a commit applied by
//     the Git integration marks the branch used when it starts and again when it ends (touchIdle), and a
//     branch with an operation that has not finished is never paused (branchBusy). A wake counts too.
//
// Only a pause the node made for idleness is ever undone by the node, whether the trigger is a request, a
// database connection, a Management API call, an operation above or a commit of the repository. A branch
// the owner paused by hand has no "idle" record (registry.LifecycleStore) and stays paused until Resume,
// and so does one the node stopped because it still carries its parent's credentials
// (registry.PauseQuarantine). Persistent branches, branches
// scheduled for deletion, branches with an operation in flight (here or in another process, or a merge
// into their parent) and the default branch are never paused. A cluster of more than one server pauses
// nothing: a request that reaches another server's proxy could not wake the branch (see the README).

// Activity says how recently a project was used over HTTP or the Management API. *reqstats.Counter
// satisfies it.
type Activity interface {
	// Last is the time of the latest request counted for ref, or of the latest Management API call that
	// ran SQL on it; ok is false when neither happened since the daemon started.
	Last(ref string) (t time.Time, ok bool)
	// Active is the number of requests being served now (a WebSocket is one for as long as it is open).
	Active(ref string) int
}

// reasonPauser is what the idle pause needs of the Engine beyond Engine itself: a pause that records its
// reason. *lifecycle.Engine has it; an Engine without it cannot pause a branch for idleness, because the
// branch could never be woken.
type reasonPauser interface {
	PauseWith(ctx context.Context, ref string, o lifecycle.PauseOptions) error
}

// ErrNotIdle is returned by Wake for a project the owner paused: only a pause the node made for
// idleness wakes by itself.
var ErrNotIdle = errors.New("branching: the project was paused by its owner and is not woken by a request")

// maxWake bounds the start of a branch that a request or a connection began, whatever the caller's
// patience: the start goes on after the caller gave up.
const maxWake = 10 * time.Minute

type idleState struct {
	mu sync.Mutex
	// since is when the sweep first saw a branch running (or running again): the clock of its
	// idleness cannot start before it.
	since map[string]time.Time
	// used is when the service last worked on a branch itself (an operation began or ended on it, a
	// wake): activity that no request and no open database session shows (touchIdle).
	used map[string]time.Time
	// gates are the listeners that wake a paused branch on a database connection (idlegate.go).
	gates map[string]*gate
	kick  chan struct{}
}

func newIdleState() idleState {
	return idleState{since: map[string]time.Time{}, used: map[string]time.Time{}, gates: map[string]*gate{}, kick: make(chan struct{}, 1)}
}

// IdleResult lists what an idle sweep did.
type IdleResult struct {
	// Paused are the refs of the branches it paused.
	Paused []string
	// Resumed are the refs of idle-paused branches it started again because a second server joined.
	Resumed []string
	// Failed maps a ref to why it could not pause or resume the branch.
	Failed map[string]string
}

// IdleSweep pauses the branches that have been idle for [branching] idle_pause_after. It is one pass
// of RunIdle; a test calls it with a clock it controls. It does nothing when idle pause is off or the
// service has no Activity.
func (s *Service) IdleSweep(ctx context.Context) (*IdleResult, error) {
	res := &IdleResult{Failed: map[string]string{}}
	all, err := s.reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	multi := s.multiServer(ctx)
	idleAfter, on, err := s.cfg.Branching.IdlePause()
	if err != nil {
		return nil, err
	}
	pauser, canPause := s.eng.(reasonPauser)
	pauses := map[string]registry.Pause{}
	if st := registry.BranchLifecycleOf(s.reg); st != nil {
		ps, err := st.ListPauses(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			pauses[p.Ref] = p
		}
	}
	running := map[string]bool{}
	for i := range all {
		p := &all[i]
		if p.Branch == nil || !s.homedHere(p) {
			continue
		}
		if multi {
			// A second server cannot wake a branch from its own proxy, so a branch that sleeps when it
			// joins is started again; this server pauses no more of them.
			if pz, ok := pauses[p.Ref]; ok && pz.Reason == registry.PauseIdle && p.Status == registry.StatusInactive && p.Branch.DeletionScheduledAt == nil {
				if err := s.eng.Resume(ctx, p.Ref); err != nil {
					// RunIdle drops the result, so the log is the only place an operator learns that the
					// branch stays asleep (not enough memory on the node, say).
					s.log.Warn("could not start an idle branch when a second server joined; it stays paused", "ref", p.Ref, "err", err)
					res.Failed[p.Ref] = err.Error()
					continue
				}
				s.woken(ctx, p.Ref)
				s.event(ctx, p.Ref, "branch.idle_woken", map[string]any{"by": "cluster"})
				res.Resumed = append(res.Resumed, p.Ref)
			}
			continue
		}
		if !on || !canPause || s.activity == nil || !p.Status.Running() {
			continue
		}
		running[p.Ref] = true
		since := s.idleSince(p.Ref, now)
		b := branchOf(p)
		if b.Persistent || b.DeletionScheduledAt != nil {
			continue
		}
		if s.branchBusy(ctx, b) {
			// An operation at work is use, and so is the moment it ends: an operation of another process
			// (the CLI's merge) leaves no trace here but the registry showing it, so the clock restarts at
			// the last sweep that saw it.
			s.touchIdle(p.Ref)
			continue
		}
		last := since
		if t, ok := s.activity.Last(p.Ref); ok && t.After(last) {
			last = t
		}
		if t, ok := s.idleUsed(p.Ref); ok && t.After(last) {
			last = t
		}
		if s.activity.Active(p.Ref) > 0 || now.Sub(last) < idleAfter {
			continue
		}
		// The branch looks idle from the proxy's side; ask the database too.
		idleFor, err := s.dbIdle(ctx, p.Ref)
		if err != nil {
			s.log.Debug("idle sweep: could not read the database's activity; leaving the branch running", "ref", p.Ref, "err", err)
			continue
		}
		if idleFor < now.Sub(last) {
			last = now.Add(-idleFor)
		}
		if now.Sub(last) < idleAfter {
			continue
		}
		// Look at the proxy once more: a request may have landed while the database was read.
		if t, ok := s.activity.Last(p.Ref); (ok && now.Sub(t) < idleAfter) || s.activity.Active(p.Ref) > 0 {
			continue
		}
		// Held as an operation while the branch stops, so that a merge or a reset that starts in this
		// process meanwhile is told the branch is busy instead of finding its database going away.
		r, err := s.begin(p.Ref, "pause")
		if err != nil {
			continue
		}
		err = pauser.PauseWith(ctx, p.Ref, lifecycle.PauseOptions{Reason: registry.PauseIdle})
		s.end(p.Ref, r)
		if err != nil {
			s.log.Warn("could not pause an idle branch", "ref", p.Ref, "name", b.Name, "err", err)
			res.Failed[p.Ref] = err.Error()
			continue
		}
		// Its clocks start again when it runs again, whoever starts it and however soon.
		s.idle.mu.Lock()
		delete(s.idle.since, p.Ref)
		delete(s.idle.used, p.Ref)
		s.idle.mu.Unlock()
		s.log.Info("paused an idle branch", "ref", p.Ref, "name", b.Name, "idle", now.Sub(last).Round(time.Second).String())
		s.event(ctx, p.Ref, "branch.idle_paused", map[string]any{"idle_seconds": int64(now.Sub(last).Seconds()), "after": idleAfter.String()})
		res.Paused = append(res.Paused, p.Ref)
		s.kickGates()
	}
	s.forgetIdle(running)
	return res, ctx.Err()
}

// idleSince returns when the sweep first saw ref running, recording now when it is the first time.
func (s *Service) idleSince(ref string, now time.Time) time.Time {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	t, ok := s.idle.since[ref]
	if !ok {
		t = now
		s.idle.since[ref] = t
	}
	return t
}

// touchIdle marks ref used now: the service worked on the branch itself (a diff, a merge, a push, a reset or a
// commit of the Git integration began or ended on it, or it was woken), which no request to its API and no
// session in its database shows. It does nothing without idle pause.
func (s *Service) touchIdle(ref string) {
	if s.activity == nil {
		return
	}
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	s.idle.used[ref] = s.now()
}

// idleUsed returns when touchIdle last marked ref used.
func (s *Service) idleUsed(ref string) (time.Time, bool) {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	t, ok := s.idle.used[ref]
	return t, ok
}

// forgetIdle drops the clocks of branches that were not running at this sweep: they start again when
// they are seen running.
func (s *Service) forgetIdle(running map[string]bool) {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	for ref := range s.idle.since {
		if !running[ref] {
			delete(s.idle.since, ref)
		}
	}
	for ref := range s.idle.used {
		if !running[ref] {
			delete(s.idle.used, ref)
		}
	}
}

// homedHere reports whether p runs on this node.
func (s *Service) homedHere(p *registry.Project) bool {
	return s.nodeID == "" || p.NodeID == "" || p.NodeID == s.nodeID
}

// multiServer reports whether the cluster has more than one server. A registry that cannot say counts
// as a cluster: nothing is paused on a guess.
func (s *Service) multiServer(ctx context.Context) bool {
	nodes, err := s.reg.ListNodes(ctx)
	if err != nil {
		return true
	}
	n := 0
	for _, nd := range nodes {
		if nd.State == registry.NodeActive || nd.State == registry.NodeJoining {
			n++
		}
	}
	return n > 1
}

// branchBusy reports whether an operation works on b, or on its parent in a way that involves b: one
// that runs in this process, one the registry shows in another process, an action run that has not
// finished, or a merge into the parent.
func (s *Service) branchBusy(ctx context.Context, b *Branch) bool {
	if _, ok := s.running(b.Ref); ok {
		return true
	}
	if _, ok := s.running(b.ParentRef); ok {
		return true
	}
	if s.inFlight(ctx, b) || b.State == registry.BranchCreatingProject || b.State == registry.BranchRunningMigration {
		return true
	}
	if st := s.runStore(); st != nil {
		runs, err := st.RunningBranchRuns(ctx)
		if err != nil {
			return true // a store that cannot answer leaves the branch alone
		}
		for i := range runs {
			if runs[i].BranchRef == b.Ref || runs[i].OwnerRef == b.Ref {
				return true
			}
			if runs[i].Op == "merge" && runs[i].OwnerRef == b.ParentRef {
				return true
			}
		}
	}
	return false
}

// dbIdle is how long the project's database has gone without a client: zero when one runs a statement
// now. A session that connected and has been waiting since counts as of its last statement.
func (s *Service) dbIdle(ctx context.Context, ref string) (time.Duration, error) {
	if s.dbActivity != nil {
		return s.dbActivity(ctx, ref)
	}
	dsn, err := s.eng.ConnString(ctx, ref, lifecycle.RoleAdmin)
	if err != nil {
		return 0, err
	}
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return 0, err
	}
	cc.RuntimeParams["application_name"] = idleAppName
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := pgx.ConnectConfig(cctx, cc)
	if err != nil {
		return 0, err
	}
	defer closeConn(c)
	var secs float64
	if err := c.QueryRow(cctx, clientIdleSQL).Scan(&secs); err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// idleAppName names the sweep's own session, which it leaves out of what it reads.
const idleAppName = "supavise-idle"

// clientIdleSQL answers, in seconds, how long ago a client of the database last did something. The
// roles of GoTrue, PostgREST and Storage are left out because everything they do for a client came in
// through the proxy, which counts it; pg_cron's sessions and this one are not clients either. The
// roles postgres and supabase_admin, and any role of the project's own, are: they are the pooler's
// users, Studio's SQL and table editors, pg-meta and Realtime.
const clientIdleSQL = `
select coalesce((
  select case when bool_or(state is distinct from 'idle') then 0
              else extract(epoch from now() - max(state_change)) end
    from pg_stat_activity
   where backend_type = 'client backend'
     and pid <> pg_backend_pid()
     and coalesce(application_name, '') not in ('pg_cron', '` + idleAppName + `')
     and coalesce(usename, '') not in ('authenticator', 'supabase_auth_admin', 'supabase_storage_admin')
), 31536000)::float8`

// RunIdle looks for idle branches until ctx ends, and keeps the listeners that wake paused branches on a
// database connection. The daemon starts it on a server that leads, next to Run:
//
//	go svc.RunIdle(ctx)
//
// The first pass waits for a full period, because the clocks of the branches start when this process
// first sees them running.
func (s *Service) RunIdle(ctx context.Context) error {
	if s.activity == nil {
		return nil
	}
	every := s.idleEvery()
	t := time.NewTicker(every)
	defer t.Stop()
	go s.runGates(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		if _, err := s.IdleSweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("idle sweep failed", "err", err)
		}
	}
}

// idleEvery is the pause between idle sweeps: the sweep interval, or a third of the idle period when
// that is shorter, so that a short period is honored within a third of it.
func (s *Service) idleEvery() time.Duration {
	every := s.cfg.Branching.SweepInterval()
	if d, on, _ := s.cfg.Branching.IdlePause(); on && d/3 < every {
		every = max(d/3, time.Second)
	}
	return every
}

// PauseReason says who paused a branch: "idle" when the node did because nothing used it, "quarantine"
// when the node stopped it because it still carries its parent's credentials, "owner" when a person or a
// client did (the dashboard's Pause, the CLI, a scheduled deletion, and any pause that has no record), and
// "" when the branch is not paused.
func (s *Service) PauseReason(ctx context.Context, ref string) string {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil || p.Status != registry.StatusInactive {
		return ""
	}
	switch s.pauseRecord(ctx, ref) {
	case registry.PauseIdle:
		return registry.PauseIdle
	case registry.PauseQuarantine:
		return registry.PauseQuarantine
	}
	return "owner"
}

// pauseRecord is the reason the lifecycle store holds for ref: registry.PauseIdle, registry.PauseQuarantine
// or "" for the owner's pause. A registry with no store, a project with no record and a record that cannot
// be read all give "": nothing is woken on a guess.
func (s *Service) pauseRecord(ctx context.Context, ref string) string {
	st := registry.BranchLifecycleOf(s.reg)
	if st == nil {
		return ""
	}
	pz, err := st.GetPause(ctx, ref)
	if err != nil {
		return ""
	}
	return pz.Reason
}

// ---- waking ------------------------------------------------------------------------------------

// Wake starts a branch the node paused for idleness and returns when it runs, or when ctx ends. It is
// what the edge proxy calls for a request to a sleeping branch (proxy.IdleWaker), and what a database
// connection to a sleeping branch triggers. A branch the owner paused is not woken (ErrNotIdle).
// Calls for one branch share a single start. The start goes on when ctx ends first, bounded by
// maxWake, so that the next request finds the branch up.
func (s *Service) Wake(ctx context.Context, ref string) error {
	return s.wake(ctx, ref, "http")
}

func (s *Service) wake(ctx context.Context, ref, by string) error {
	ch := s.wakes.DoChan(ref, func() (any, error) {
		wctx, cancel := context.WithTimeout(s.base, maxWake)
		defer cancel()
		return nil, s.wakeNow(wctx, ref, by)
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		return fmt.Errorf("branching: %s is still waking up: %w", ref, ctx.Err())
	}
}

// woken is what a branch needs once it runs again after a pause: its idle clock starts again, and the Edge
// Functions runtime serves its functions now. The runtime's reconcile removes the files of a project that is
// not running, and without this the first call to a function of a woken branch finds none (a 404) until the
// next reconcile puts them back.
func (s *Service) woken(ctx context.Context, ref string) {
	s.touchIdle(ref)
	s.serveFunctionsAgain(ctx, ref)
}

func (s *Service) wakeNow(ctx context.Context, ref, by string) error {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if p.Status.Running() {
		return nil
	}
	// A branch scheduled for deletion waits to go: scheduleDelete makes its pause the owner's, and this
	// holds if that change did not go through.
	if p.Branch != nil && p.Branch.DeletionScheduledAt != nil {
		return ErrNotIdle
	}
	st := registry.BranchLifecycleOf(s.reg)
	if st == nil {
		return ErrNotIdle
	}
	pause, err := st.GetPause(ctx, ref)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return ErrNotIdle
	case err != nil:
		return err
	case !registry.WakesByItself(pause.Reason):
		return ErrNotIdle
	}
	switch p.Status {
	case registry.StatusInactive:
		start := s.now()
		if err := s.eng.Resume(ctx, ref); err != nil {
			// Another process may have started it between the read and the call.
			if cur, gerr := s.reg.GetProject(ctx, ref); gerr == nil && cur.Status.Running() {
				s.woken(ctx, ref)
				return nil
			}
			return err
		}
		s.woken(ctx, ref)
		s.log.Info("woke an idle branch", "ref", ref, "by", by, "ms", s.now().Sub(start).Milliseconds())
		s.event(ctx, ref, "branch.idle_woken", map[string]any{"by": by, "ms": s.now().Sub(start).Milliseconds()})
		return nil
	case registry.StatusComingUp, registry.StatusPausing:
		// Someone else is already starting it (the CLI, another request's flight in a process that
		// restarted): wait for it.
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
			cur, err := s.reg.GetProject(ctx, ref)
			if err != nil {
				return err
			}
			switch {
			case cur.Status.Running():
				s.woken(ctx, ref)
				return nil
			case cur.Status == registry.StatusInactive:
				return s.wakeNow(ctx, ref, by)
			}
		}
	}
	return conflict("%s is %s and cannot be woken", ref, p.Status)
}

// wakeIfIdle returns b ready for an operation that needs its database: a branch the node paused for idleness
// is started and waited for (up to [branching] idle_wake_timeout_seconds), as the edge proxy does for a request
// and the Management API does for a call that runs SQL, and the returned branch is read again. Any branch
// is marked used (touchIdle), because the operation is use.
//
// A branch paused by its owner, one stopped as a quarantine, one scheduled for deletion and one in a state
// that is not a pause come back as they are, and the operation's own check (activeBranch) refuses them
// with the reason. A start that fails or does not finish in time is a conflict that says so.
func (s *Service) wakeIfIdle(ctx context.Context, b *Branch, op string) (*Branch, error) {
	s.touchIdle(b.Ref)
	switch b.ProjectStatus {
	case registry.StatusInactive, registry.StatusComingUp:
	default:
		return b, nil
	}
	if b.DeletionScheduledAt != nil {
		return b, nil
	}
	if b.ProjectStatus == registry.StatusInactive && !registry.WakesByItself(s.pauseRecord(ctx, b.Ref)) {
		return b, nil
	}
	wctx, cancel := context.WithTimeout(ctx, s.cfg.Branching.IdleWakeTimeout())
	defer cancel()
	if err := s.wake(wctx, b.Ref, op); err != nil {
		if errors.Is(err, ErrNotIdle) {
			return b, nil
		}
		return nil, conflict("branch %s could not be woken for the %s: %v", b.Name, op, err)
	}
	return s.resolveBranch(ctx, b.Ref, op)
}

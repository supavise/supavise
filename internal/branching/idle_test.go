package branching

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// fakeActivity is the HTTP side of what a branch has been used for.
type fakeActivity struct {
	mu     sync.Mutex
	last   map[string]time.Time
	active map[string]int
}

func (a *fakeActivity) Last(ref string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.last[ref]
	return t, ok
}

func (a *fakeActivity) Active(ref string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active[ref]
}

func (a *fakeActivity) request(ref string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		a.last = map[string]time.Time{}
	}
	a.last[ref] = at
}

func (a *fakeActivity) open(ref string, n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		a.active = map[string]int{}
	}
	a.active[ref] = n
}

// idleHarness is a harness whose service watches for idleness with a 30 minute period, over a fake
// reqstats and a database that has had no client for a year unless a test says otherwise.
type idleHarness struct {
	*harness
	act    *fakeActivity
	dbIdle map[string]time.Duration
	dbErr  error
	dbMu   sync.Mutex
}

func newIdleHarness(t *testing.T, mut func(*config.Config)) *idleHarness {
	t.Helper()
	h := newHarness(t, func(c *config.Config) {
		c.Branching.IdlePauseAfter = "30m"
		if mut != nil {
			mut(c)
		}
	})
	ih := &idleHarness{harness: h, act: &fakeActivity{}, dbIdle: map[string]time.Duration{}}
	h.svc.activity = ih.act
	h.svc.dbActivity = func(_ context.Context, ref string) (time.Duration, error) {
		ih.dbMu.Lock()
		defer ih.dbMu.Unlock()
		if ih.dbErr != nil {
			return 0, ih.dbErr
		}
		if d, ok := ih.dbIdle[ref]; ok {
			return d, nil
		}
		return 365 * 24 * time.Hour, nil
	}
	return ih
}

func (ih *idleHarness) setDBIdle(ref string, d time.Duration) {
	ih.dbMu.Lock()
	defer ih.dbMu.Unlock()
	ih.dbIdle[ref] = d
}

// settle waits for the detached operations to end. WaitIdle returns when the branch's state is final,
// which is before the operation's goroutine runs its last deferred call (end, which touches idle), so a
// test that changes a field of the service that call reads waits here first. Nothing in production
// changes those fields after New.
func (ih *idleHarness) settle() {
	ih.t.Helper()
	ih.svc.wg.Wait()
}

func (ih *idleHarness) sweep() *IdleResult {
	ih.t.Helper()
	res, err := ih.svc.IdleSweep(context.Background())
	if err != nil {
		ih.t.Fatal(err)
	}
	return res
}

func (ih *idleHarness) status(ref string) registry.Status {
	ih.t.Helper()
	p, err := ih.reg.GetProject(context.Background(), ref)
	if err != nil {
		ih.t.Fatal(err)
	}
	return p.Status
}

func (ih *idleHarness) pauseReason(ref string) (string, bool) {
	p, err := ih.reg.GetPause(context.Background(), ref)
	if err != nil {
		return "", false
	}
	return p.Reason, true
}

func has(refs []string, ref string) bool {
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

// A branch nobody uses is paused once the period has passed since it was first seen running, and not before.
func TestIdleSweepPausesAnUnusedBranchAfterThePeriod(t *testing.T) {
	ih := newIdleHarness(t, nil)
	b := ih.create("a", nil)
	if res := ih.sweep(); len(res.Paused) != 0 {
		t.Fatalf("paused at the first look: %+v", res)
	}
	ih.advance(29 * time.Minute)
	if res := ih.sweep(); len(res.Paused) != 0 {
		t.Fatalf("paused after 29 minutes: %+v", res)
	}
	ih.advance(2 * time.Minute)
	res := ih.sweep()
	if !has(res.Paused, b.Ref) || len(res.Failed) != 0 {
		t.Fatalf("not paused after 31 minutes: %+v", res)
	}
	if ih.status(b.Ref) != registry.StatusInactive {
		t.Fatalf("status = %s", ih.status(b.Ref))
	}
	if reason, ok := ih.pauseReason(b.Ref); !ok || reason != registry.PauseIdle {
		t.Fatalf("pause record = %q, %v", reason, ok)
	}
	evs, _ := ih.reg.ListEvents(context.Background(), b.Ref, 10)
	found := false
	for _, e := range evs {
		found = found || e.Kind == "branch.idle_paused"
	}
	if !found {
		t.Fatal("no branch.idle_paused event")
	}
	// The parent, which is not a branch, is never paused.
	if ih.status(parentRef) != registry.StatusActiveHealthy {
		t.Fatalf("parent status = %s", ih.status(parentRef))
	}
}

func TestIdleSweepLeavesABranchThatIsInUse(t *testing.T) {
	cases := map[string]func(ih *idleHarness, ref string){
		"an HTTP request in the period": func(ih *idleHarness, ref string) { ih.act.request(ref, ih.clock().Add(-10*time.Minute)) },
		"a request still open (a WebSocket)": func(ih *idleHarness, ref string) {
			ih.act.request(ref, ih.clock().Add(-5*time.Hour))
			ih.act.open(ref, 1)
		},
		"a database client in the period": func(ih *idleHarness, ref string) { ih.setDBIdle(ref, 12*time.Minute) },
		"a statement running now":         func(ih *idleHarness, ref string) { ih.setDBIdle(ref, 0) },
	}
	for name, use := range cases {
		t.Run(name, func(t *testing.T) {
			ih := newIdleHarness(t, nil)
			b := ih.create("a", nil)
			ih.sweep()
			ih.advance(31 * time.Minute)
			use(ih, b.Ref)
			ih.act.request(b.Ref, ih.act.last[b.Ref]) // keep the value as set
			if res := ih.sweep(); len(res.Paused) != 0 {
				t.Fatalf("paused although %s: %+v", name, res)
			}
			if ih.status(b.Ref) != registry.StatusActiveHealthy {
				t.Fatalf("status = %s", ih.status(b.Ref))
			}
		})
	}
}

// Use pushes the pause back by a whole period from the use.
func TestIdleSweepCountsThePeriodFromTheLastUse(t *testing.T) {
	ih := newIdleHarness(t, nil)
	b := ih.create("a", nil)
	ih.sweep()
	ih.advance(25 * time.Minute)
	ih.act.request(b.Ref, ih.clock())
	ih.advance(25 * time.Minute) // 50 minutes since the branch appeared, 25 since the request
	if res := ih.sweep(); len(res.Paused) != 0 {
		t.Fatalf("paused 25 minutes after a request: %+v", res)
	}
	ih.advance(6 * time.Minute)
	if res := ih.sweep(); !has(res.Paused, b.Ref) {
		t.Fatalf("not paused 31 minutes after the request: %+v", res)
	}
}

// A database that cannot be read leaves the branch running, and so does a database used a moment ago.
func TestIdleSweepNeverGuesses(t *testing.T) {
	ih := newIdleHarness(t, nil)
	b := ih.create("a", nil)
	ih.sweep()
	ih.advance(time.Hour)
	ih.dbErr = errors.New("connection refused")
	if res := ih.sweep(); len(res.Paused) != 0 {
		t.Fatalf("paused without reading the database: %+v", res)
	}
	ih.dbErr = nil
	if res := ih.sweep(); !has(res.Paused, b.Ref) {
		t.Fatalf("not paused once the database answered: %+v", res)
	}
}

func TestIdleSweepSkipsWhatItMustNotPause(t *testing.T) {
	ih := newIdleHarness(t, nil)
	ctx := context.Background()
	persistent := ih.create("keep", func(in *CreateInput) { in.Persistent = true })
	scheduled := ih.create("going", nil)
	if _, err := ih.svc.Delete(ctx, scheduled.Ref, DeleteOptions{Schedule: true}); err != nil {
		t.Fatal(err)
	}
	busy := ih.create("busy", nil)
	merging := ih.create("merging", nil)
	ih.sweep()
	ih.advance(time.Hour)

	// An operation in this process, and a registry state that says one runs elsewhere.
	r, err := ih.svc.begin(busy.Ref, "reset")
	if err != nil {
		t.Fatal(err)
	}
	defer ih.svc.end(busy.Ref, r)
	ih.svc.setState(ctx, merging.Ref, registry.BranchRunningMigration, "merging", nil)

	res := ih.sweep()
	for _, b := range []*Branch{persistent, busy, merging} {
		if has(res.Paused, b.Ref) {
			t.Errorf("%s was paused", b.Name)
		}
	}
	// A scheduled delete pauses the branch by itself; the idle pause leaves it alone.
	if has(res.Paused, scheduled.Ref) {
		t.Error("a branch scheduled for deletion was paused for idleness")
	}
	if n := ih.eng.paused[persistent.Ref] + ih.eng.paused[busy.Ref] + ih.eng.paused[merging.Ref]; n != 0 {
		t.Errorf("PauseWith ran %d times for branches that must stay up", n)
	}
	// A merge into the parent keeps all its branches up.
	rm, err := ih.svc.begin(parentRef, "merge")
	if err != nil {
		t.Fatal(err)
	}
	defer ih.svc.end(parentRef, rm)
	other := ih.create("other", nil)
	ih.advance(time.Hour)
	ih.sweep() // starts other's clock
	ih.advance(time.Hour)
	if res := ih.sweep(); has(res.Paused, other.Ref) {
		t.Error("a branch was paused during a merge into its parent")
	}
}

func TestIdleSweepRespectsTheKnob(t *testing.T) {
	ih := newIdleHarness(t, func(c *config.Config) { c.Branching.IdlePauseAfter = "off" })
	b := ih.create("a", nil)
	ih.sweep()
	ih.advance(1000 * time.Hour)
	if res := ih.sweep(); len(res.Paused) != 0 || ih.status(b.Ref) != registry.StatusActiveHealthy {
		t.Fatalf("paused with idle_pause_after off: %+v", res)
	}
	// Without a view of the requests the service pauses nothing either.
	ih = newIdleHarness(t, nil)
	b = ih.create("a", nil)
	ih.settle() // the create's goroutine marks the branch used as it ends, and reads the field below
	ih.svc.activity = nil
	ih.advance(1000 * time.Hour)
	ih.sweep()
	ih.advance(1000 * time.Hour)
	if res := ih.sweep(); len(res.Paused) != 0 {
		t.Fatalf("paused without Activity: %+v", res)
	}
}

// A branch homed on another node is that node's to pause.
func TestIdleSweepPausesOnlyBranchesOfThisNode(t *testing.T) {
	ih := newIdleHarness(t, nil)
	ih.svc.nodeID = "n2"
	b := ih.create("a", nil) // the memory registry homes it on the leader, n1
	ih.sweep()
	ih.advance(time.Hour)
	if res := ih.sweep(); len(res.Paused) != 0 {
		t.Fatalf("paused a branch of another node: %+v", res)
	}
	ih.svc.nodeID = ""
	ih.sweep() // the branch is this server's now: its clock starts
	ih.advance(time.Hour)
	if res := ih.sweep(); !has(res.Paused, b.Ref) {
		t.Fatalf("a server outside a cluster did not pause it: %+v", res)
	}
}

// When a second server joins, nothing pauses any more and what sleeps is started again.
func TestIdleSweepStopsWhenAServerJoins(t *testing.T) {
	ih := newIdleHarness(t, nil)
	ctx := context.Background()
	b := ih.create("a", nil)
	ih.sweep()
	ih.advance(time.Hour)
	if res := ih.sweep(); !has(res.Paused, b.Ref) {
		t.Fatalf("not paused: %+v", res)
	}
	if err := ih.reg.CreateNode(ctx, &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	res := ih.sweep()
	if !has(res.Resumed, b.Ref) || ih.status(b.Ref) != registry.StatusActiveHealthy {
		t.Fatalf("not resumed: %+v, status %s", res, ih.status(b.Ref))
	}
	ih.advance(10 * time.Hour)
	if res := ih.sweep(); len(res.Paused) != 0 {
		t.Fatalf("paused in a cluster: %+v", res)
	}
	// A branch the owner paused is theirs: the cluster does not start it.
	if err := ih.eng.Pause(ctx, b.Ref); err != nil {
		t.Fatal(err)
	}
	if res := ih.sweep(); len(res.Resumed) != 0 || ih.status(b.Ref) != registry.StatusInactive {
		t.Fatalf("resumed a branch the owner paused: %+v", res)
	}
}

func TestWakeStartsOnlyWhatTheNodePaused(t *testing.T) {
	ih := newIdleHarness(t, nil)
	ctx := context.Background()
	idle, hand, running := ih.create("idle", nil), ih.create("hand", nil), ih.create("running", nil)
	if err := ih.eng.PauseWith(ctx, idle.Ref, lifecycle.PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	if err := ih.eng.Pause(ctx, hand.Ref); err != nil {
		t.Fatal(err)
	}

	if err := ih.svc.Wake(ctx, running.Ref); err != nil || ih.eng.resumed[running.Ref] != 0 {
		t.Fatalf("waking a running branch: %v, resumes %d", err, ih.eng.resumed[running.Ref])
	}
	if err := ih.svc.Wake(ctx, hand.Ref); !errors.Is(err, ErrNotIdle) || ih.eng.resumed[hand.Ref] != 0 {
		t.Fatalf("waking a branch the owner paused: %v, resumes %d", err, ih.eng.resumed[hand.Ref])
	}
	if err := ih.svc.Wake(ctx, idle.Ref); err != nil || ih.eng.resumed[idle.Ref] != 1 {
		t.Fatalf("waking an idle branch: %v, resumes %d", err, ih.eng.resumed[idle.Ref])
	}
	if ih.status(idle.Ref) != registry.StatusActiveHealthy {
		t.Fatalf("status = %s", ih.status(idle.Ref))
	}
	if _, ok := ih.pauseReason(idle.Ref); ok {
		t.Fatal("the record of the pause stayed")
	}
	evs, _ := ih.reg.ListEvents(ctx, idle.Ref, 10)
	found := false
	for _, e := range evs {
		found = found || e.Kind == "branch.idle_woken"
	}
	if !found {
		t.Fatal("no branch.idle_woken event")
	}
}

// Requests that arrive together share one start, one that gives up leaves the start going, and a start
// that fails is tried again by the next request.
func TestWakeSharesOneStartAndOutlivesTheCaller(t *testing.T) {
	ih := newIdleHarness(t, nil)
	ctx := context.Background()
	b := ih.create("a", nil)
	if err := ih.eng.PauseWith(ctx, b.Ref, lifecycle.PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	ih.eng.resumeWait = make(chan struct{})

	// A caller whose patience ends first.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := ih.svc.Wake(short, b.Ref); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a wake that outlasted its caller: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = ih.svc.Wake(ctx, b.Ref) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(ih.eng.resumeWait)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if ih.eng.resumed[b.Ref] != 1 {
		t.Fatalf("the branch was started %d times", ih.eng.resumed[b.Ref])
	}
	if ih.status(b.Ref) != registry.StatusActiveHealthy {
		t.Fatalf("status = %s", ih.status(b.Ref))
	}

	// A failed start leaves the record, so the next request tries again.
	if err := ih.eng.PauseWith(ctx, b.Ref, lifecycle.PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	ih.eng.resumeWait, ih.eng.resumeErr = nil, errors.New("not enough memory")
	if err := ih.svc.Wake(ctx, b.Ref); err == nil {
		t.Fatal("the start failure was not returned")
	}
	if reason, ok := ih.pauseReason(b.Ref); !ok || reason != registry.PauseIdle {
		t.Fatalf("record after a failed start = %q, %v", reason, ok)
	}
	ih.eng.resumeErr = nil
	if err := ih.svc.Wake(ctx, b.Ref); err != nil || ih.status(b.Ref) != registry.StatusActiveHealthy {
		t.Fatalf("second try: %v, status %s", err, ih.status(b.Ref))
	}
}

// freePort returns a port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The wake gate holds the first database client of a sleeping branch, lets the branch bind the port,
// and passes the client's bytes to it, the first packet included. A connection that sends nothing wakes
// nothing, and a branch the owner paused has no gate.
func TestWakeGateHoldsTheFirstClient(t *testing.T) {
	ih := newIdleHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := ih.create("a", nil)
	hand := ih.create("hand", nil)
	p, _ := ih.reg.GetProject(ctx, b.Ref)
	port := freePort(t)
	ih.cfg.Ports.ProjectBase = port - 3*p.Seq
	ih.cfg.Branching.IdleWakeTimeoutSeconds = 10

	// Resuming stands for Postgres binding the port the gate has released.
	echoUp := make(chan struct{})
	var received []byte
	var rmu sync.Mutex
	ih.eng.onResume = func(ref string) {
		if ref != b.Ref {
			return
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Errorf("Postgres could not bind the port the gate should have released: %v", err)
			return
		}
		go func() {
			defer close(echoUp)
			c, err := ln.Accept()
			ln.Close()
			if err != nil {
				return
			}
			defer c.Close()
			buf := make([]byte, 64)
			n, _ := io.ReadAtLeast(c, buf, 12)
			rmu.Lock()
			received = append([]byte(nil), buf[:n]...)
			rmu.Unlock()
			_, _ = c.Write([]byte("ready"))
		}()
	}

	if err := ih.eng.PauseWith(ctx, b.Ref, lifecycle.PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	if err := ih.eng.Pause(ctx, hand.Ref); err != nil {
		t.Fatal(err)
	}
	ih.svc.reconcileGates(ctx)
	ih.svc.idle.mu.Lock()
	gates := len(ih.svc.idle.gates)
	ih.svc.idle.mu.Unlock()
	if gates != 1 {
		t.Fatalf("%d gates, want one for the idle branch and none for the owner's pause", gates)
	}

	// A connection that sends nothing wakes nothing.
	quiet, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	quiet.Close()
	if ih.eng.resumed[b.Ref] != 0 {
		t.Fatal("a silent connection woke the branch")
	}

	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("STARTUP!more")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, 5)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ready" {
		t.Fatalf("answer = %q, %v", got, err)
	}
	<-echoUp
	rmu.Lock()
	defer rmu.Unlock()
	if string(received) != "STARTUP!more" {
		t.Fatalf("Postgres received %q", received)
	}
	if ih.eng.resumed[b.Ref] != 1 || ih.status(b.Ref) != registry.StatusActiveHealthy {
		t.Fatalf("resumes %d, status %s", ih.eng.resumed[b.Ref], ih.status(b.Ref))
	}
}

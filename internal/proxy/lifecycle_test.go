package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/domains"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/reqstats"
)

const branchRef = "bbbbbbbbbbbbbbbbbbbb"

// addBranch registers a branch of the harness project, with keys of its own.
func addBranch(t *testing.T, h *harness) {
	t.Helper()
	err := h.reg.CreateProject(context.Background(), &registry.Project{
		Ref: branchRef, Name: "feature", Status: registry.StatusActiveHealthy,
		Branch: &registry.BranchInfo{ID: "7c0c9c4e-0000-4000-8000-000000000001", ParentRef: h.ref, Name: "feature", State: registry.BranchFunctionsDeployed},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.keys.set(branchRef, testKeys(t, branchRef))
}

func setStatus(t *testing.T, h *harness, ref string, st registry.Status) {
	t.Helper()
	if err := h.reg.SetProjectStatus(context.Background(), ref, st); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the table to see "+string(st), func() bool {
		p, _ := h.srv.table.hostProject(ref + ".api." + testDomain)
		return p.status == st
	})
}

// While a branch is reset (its cluster removed and built again over the same row), the derived, custom
// and vanity hosts of the branch answer 503 with Retry-After from the proxy and nothing is dialed; the
// hostnames serve again, unchanged, when the branch is back.
func TestResetWindowAnswers503OnEveryHostOfTheBranch(t *testing.T) {
	h := newHarness(t)
	addBranch(t, h)
	ctx := context.Background()
	dns := &staticDNS{txt: map[string][]string{}, host: map[string][]string{}}
	h.cfg.PublicIP = "203.0.113.7"
	svc := domains.New(domains.Options{Reg: h.reg, Config: h.cfg, Resolver: dns})
	const custom = "feature.customer.example"
	activateHostname(t, svc, dns, branchRef, custom)
	vanity := svc.VanityHost("feature-preview")
	if err := h.reg.PutVanitySubdomain(ctx, branchRef, "feature-preview", vanity); err != nil {
		t.Fatal(err)
	}
	eventually(t, "routes reach the table", func() bool {
		return h.srv.table.routeKind(custom) == "custom" && h.srv.table.routeKind(vanity) == "vanity"
	})
	key := h.keys.keys[branchRef].PublishableKey
	hosts := []string{custom, vanity, h.host(branchRef)}

	serves := func(host string) int {
		resp, _ := h.req("GET", host, "/rest/v1/todos", "apikey", key)
		return resp.StatusCode
	}
	for _, host := range hosts {
		if got := serves(host); got != 200 {
			t.Fatalf("%s before the reset: %d", host, got)
		}
	}
	// DeleteWith(KeepRecord) sets GOING_DOWN and then INIT_FAILED; the create over the row COMING_UP.
	for _, st := range []registry.Status{registry.StatusGoingDown, registry.StatusInitFailed, registry.StatusComingUp} {
		setStatus(t, h, branchRef, st)
		before := h.ups[svcRest].count()
		for _, host := range hosts {
			resp, body := h.req("GET", host, "/rest/v1/todos", "apikey", key)
			if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
				t.Errorf("%s while %s: %d, Retry-After %q, %s", host, st, resp.StatusCode, resp.Header.Get("Retry-After"), body)
			}
		}
		if h.ups[svcRest].count() != before {
			t.Errorf("a request was dialed to the upstream while the branch was %s", st)
		}
	}
	setStatus(t, h, branchRef, registry.StatusActiveHealthy)
	for _, host := range hosts {
		if got := serves(host); got != 200 {
			t.Errorf("%s after the reset: %d", host, got)
		}
	}
	// The parent's hosts were never the branch's.
	if p, _ := h.srv.table.hostProject(custom); p.ref != branchRef {
		t.Errorf("the custom hostname reaches %s", p.ref)
	}
}

// A branch's hostname is the branch's alone: the parent's does not reach the branch, and the
// branch's keys, not the parent's, open the branch's hosts.
func TestBranchHostnameBelongsToTheBranch(t *testing.T) {
	h := newHarness(t)
	addBranch(t, h)
	dns := &staticDNS{txt: map[string][]string{}, host: map[string][]string{}}
	h.cfg.PublicIP = "203.0.113.7"
	svc := domains.New(domains.Options{Reg: h.reg, Config: h.cfg, Resolver: dns})
	const parentHost, branchHost = "app.customer.example", "preview.customer.example"
	activateHostname(t, svc, dns, h.ref, parentHost)
	activateHostname(t, svc, dns, branchRef, branchHost)
	eventually(t, "routes reach the table", func() bool {
		return h.srv.table.routeKind(parentHost) == "custom" && h.srv.table.routeKind(branchHost) == "custom"
	})
	if p, _ := h.srv.table.hostProject(parentHost); p.ref != h.ref {
		t.Errorf("parent hostname reaches %s", p.ref)
	}
	if p, _ := h.srv.table.hostProject(branchHost); p.ref != branchRef {
		t.Errorf("branch hostname reaches %s", p.ref)
	}
	branchKey, parentKey := h.keys.keys[branchRef].PublishableKey, h.k.PublishableKey
	if resp, _ := h.req("GET", branchHost, "/rest/v1/todos", "apikey", branchKey); resp.StatusCode != 200 {
		t.Errorf("branch key on the branch host: %d", resp.StatusCode)
	}
	if resp, _ := h.req("GET", branchHost, "/rest/v1/todos", "apikey", parentKey); resp.StatusCode != 401 {
		t.Errorf("parent key on the branch host: %d", resp.StatusCode)
	}
	// Deleting the branch takes its hostname and route; the parent's stays.
	if _, err := svc.Delete(context.Background(), branchRef); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the branch route to leave", func() bool { return h.srv.table.routeKind(branchHost) == "" })
	if h.srv.table.routeKind(parentHost) != "custom" {
		t.Error("the parent's hostname went with the branch's")
	}
}

// Deleting a branch (by hand or when its lifetime lapses) takes its rows with it, and the proxy forgets
// the certificate of the hostname and stops allowing a new one.
func TestBranchDeleteMakesTheProxyForgetItsCertificate(t *testing.T) {
	ctx := context.Background()
	s := tlsServer(t, func(c *config.Config) { c.Domain, c.TLS.Mode, c.PublicIP = "example.com", "http01", "203.0.113.7" })
	if err := s.reg().CreateProject(ctx, &registry.Project{
		Ref: branchRef, Name: "feature", Status: registry.StatusActiveHealthy,
		Branch: &registry.BranchInfo{ID: "7c0c9c4e-0000-4000-8000-000000000001", ParentRef: testRef, Name: "feature"},
	}); err != nil {
		t.Fatal(err)
	}
	cm, err := newCertManager(certOptions{cfg: s.cfg, mode: s.tlsMode, allow: s.allowHost, log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer cm.close()
	const host = "preview.customer.example"
	dns := &staticDNS{txt: map[string][]string{}, host: map[string][]string{}}
	svc := domains.New(domains.Options{Reg: s.reg(), Config: s.cfg, Resolver: dns})
	activateHostname(t, svc, dns, branchRef, host)
	if err := s.table.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.allowHost(ctx, host); err != nil {
		t.Fatalf("the active hostname of a branch is refused a certificate: %v", err)
	}
	st := cm.http.Storage
	issuer := cm.http.Issuers[0].IssuerKey()
	for _, key := range []string{certmagic.StorageKeys.SiteCert(issuer, host)} {
		if err := st.Store(ctx, key, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	var removed []string
	s.table.setRoutesChanged(func(_, r []string) { removed = append(removed, r...) })
	if err := s.reg().DeleteProject(ctx, branchRef); err != nil {
		t.Fatal(err)
	}
	if err := s.table.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != host {
		t.Fatalf("the table reported removed hosts %v", removed)
	}
	if err := s.allowHost(ctx, host); err == nil {
		t.Error("a certificate is still allowed for the hostname of a deleted branch")
	}
	s.forgetCertificates(ctx, cm, removed)
	if st.Exists(ctx, certmagic.StorageKeys.SiteCert(issuer, host)) {
		t.Error("the certificate of the deleted branch's hostname stayed in storage")
	}
}

// fakeWaker is an IdleWaker.
type fakeWaker struct {
	mu    sync.Mutex
	calls []string
	delay time.Duration
	err   error
	// wait blocks Wake until ctx ends, like a wake that outlasts the request's patience.
	wait bool
}

func (f *fakeWaker) Wake(ctx context.Context, ref string) error {
	f.mu.Lock()
	f.calls = append(f.calls, ref)
	delay, err, wait := f.delay, f.err, f.wait
	f.mu.Unlock()
	if wait {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return err
}

func (f *fakeWaker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// pauseIdle pauses the harness project the way the node does for idleness (reason idle), or the way the
// owner does (reason ""), and waits for the table.
func pauseProject(t *testing.T, h *harness, reason string) {
	t.Helper()
	ctx := context.Background()
	if err := h.reg.SetPause(ctx, h.ref, reason, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectStatus(ctx, h.ref, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the table to see the pause", func() bool {
		p, _ := h.srv.table.hostProject(h.host(h.ref))
		return p.status == registry.StatusInactive && p.idle == (reason == registry.PauseIdle)
	})
}

// A request to a branch paused for idleness is held while the branch wakes and then served; one to a
// project the owner paused is never woken.
func TestIdleProjectWakesOnTheFirstRequest(t *testing.T) {
	fw := &fakeWaker{delay: 50 * time.Millisecond}
	h := newHarness(t, func(o *Options) { o.IdleWaker = fw })
	pauseProject(t, h, registry.PauseIdle)

	start := time.Now()
	resp, _ := h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey)
	if resp.StatusCode != 200 || fw.count() != 1 || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("status %d, wakes %d, after %v", resp.StatusCode, fw.count(), time.Since(start))
	}
	if h.ups[svcRest].count() != 1 {
		t.Fatalf("the request reached the upstream %d times", h.ups[svcRest].count())
	}

	// A request without a valid key is refused before anything wakes.
	before := fw.count()
	if resp, _ := h.project("GET", "/rest/v1/todos"); resp.StatusCode != 401 || fw.count() != before {
		t.Fatalf("keyless request: %d, wakes %d", resp.StatusCode, fw.count()-before)
	}
	// So is a request to a path that is no route.
	if resp, _ := h.project("GET", "/nothing"); resp.StatusCode != 404 || fw.count() != before {
		t.Fatalf("unknown path: %d, wakes %d", resp.StatusCode, fw.count()-before)
	}
}

func TestHandPausedProjectIsNeverWoken(t *testing.T) {
	fw := &fakeWaker{}
	h := newHarness(t, func(o *Options) { o.IdleWaker = fw })
	pauseProject(t, h, "")
	resp, body := h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" || fw.count() != 0 {
		t.Fatalf("status %d (%s), Retry-After %q, wakes %d", resp.StatusCode, body, resp.Header.Get("Retry-After"), fw.count())
	}
	if h.ups[svcRest].count() != 0 {
		t.Fatal("a paused project was dialed")
	}
}

func TestWakeThatFailsOrTakesTooLongAnswers503(t *testing.T) {
	for name, fw := range map[string]*fakeWaker{
		"failure": {err: errors.New("not enough memory on the node")},
		"timeout": {wait: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(o *Options) { o.IdleWaker = fw })
			h.cfg.Branching.IdleWakeTimeoutSeconds = 1
			pauseProject(t, h, registry.PauseIdle)
			start := time.Now()
			resp, _ := h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey)
			if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
				t.Fatalf("status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
			}
			if name == "timeout" && (time.Since(start) < 900*time.Millisecond || time.Since(start) > 5*time.Second) {
				t.Fatalf("the request waited %v for a 1 s bound", time.Since(start))
			}
			if h.ups[svcRest].count() != 0 {
				t.Fatal("the request was forwarded although the wake failed")
			}
		})
	}
}

// Requests that arrive while the branch is already waking join the wake instead of being refused.
func TestRequestsDuringTheWakeJoinIt(t *testing.T) {
	fw := &fakeWaker{delay: 100 * time.Millisecond}
	h := newHarness(t, func(o *Options) { o.IdleWaker = fw })
	pauseProject(t, h, registry.PauseIdle)
	// The engine has set COMING_UP and has not cleared the record yet.
	setStatus(t, h, h.ref, registry.StatusComingUp)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, _ := h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey); resp.StatusCode == 200 {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 4 || fw.count() != 4 {
		t.Fatalf("%d of 4 requests served, %d wake calls", ok.Load(), fw.count())
	}
}

// The certificate gate lets a handshake through for the host of a branch that sleeps, because the
// wake comes after it, and still refuses one for a project the owner paused.
func TestSleepingBranchStillGetsItsCertificate(t *testing.T) {
	ctx := context.Background()
	s := tlsServer(t, func(c *config.Config) { c.Domain, c.TLS.Mode, c.PublicIP = "example.com", "http01", "203.0.113.7" })
	if err := s.reg().CreateProject(ctx, &registry.Project{
		Ref: branchRef, Name: "feature", Status: registry.StatusActiveHealthy,
		Branch: &registry.BranchInfo{ID: "7c0c9c4e-0000-4000-8000-000000000001", ParentRef: testRef, Name: "feature"},
	}); err != nil {
		t.Fatal(err)
	}
	dns := &staticDNS{txt: map[string][]string{}, host: map[string][]string{}}
	svc := domains.New(domains.Options{Reg: s.reg(), Config: s.cfg, Resolver: dns})
	const host = "preview.customer.example"
	activateHostname(t, svc, dns, branchRef, host)
	pause := func(reason string) {
		t.Helper()
		if err := registry.BranchLifecycleOf(s.reg()).SetPause(ctx, branchRef, reason, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.reg().SetProjectStatus(ctx, branchRef, registry.StatusInactive); err != nil {
			t.Fatal(err)
		}
		if err := s.table.reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pause("")
	if err := s.allowHost(ctx, host); err == nil {
		t.Error("a handshake for a project the owner paused was allowed")
	}
	pause(registry.PauseIdle)
	if err := s.allowHost(ctx, host); err != nil {
		t.Errorf("a handshake for a sleeping branch was refused: %v", err)
	}
}

// A connected WebSocket or a long request keeps its project active for as long as it lasts.
func TestOpenRequestsCountAsActivity(t *testing.T) {
	stats := reqstats.New()
	h := newHarness(t, func(o *Options) { o.Stats = stats })
	release := make(chan struct{})
	h.ups[svcRest].handler = func(http.ResponseWriter, *http.Request) { <-release }
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.project("GET", "/rest/v1/slow", "apikey", h.k.PublishableKey)
	}()
	eventually(t, "the request to be open", func() bool { return stats.Active(h.ref) == 1 })
	if last, ok := stats.Last(h.ref); !ok || time.Since(last) > time.Minute {
		t.Fatalf("last = %v, %v", last, ok)
	}
	close(release)
	<-done
	eventually(t, "the request to end", func() bool { return stats.Active(h.ref) == 0 })
}

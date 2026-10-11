package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// apiCalls lists the replicas whose PostgREST was asked to render the keys again, in the plane's call
// list (they sort within a run, as the replicas restart together).
func apiCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "Replica ") && strings.HasSuffix(c, " api") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(c, "Replica "), " api"))
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// The primary takes the secret first; the replicas' PostgREST follows on their nodes, with the digest of
// the secret so that a node whose registry copy trails does not render the previous one. The standby's
// Postgres is not restarted.
func TestSetJWTSecretUpdatesTheReplicasAfterThePrimary(t *testing.T) {
	h, p, fl, ids := replicaHarness(t)
	nk, err := h.e.SetJWTSecret(context.Background(), p.Ref, hostedSecret)
	if err != nil {
		t.Fatal(err)
	}
	want := "Reconfigure " + p.Ref + ",Replica " + ids[0] + " api,Replica " + ids[1] + " api"
	if got := inReplicaOrder(h.plane.calls); got != want {
		t.Fatalf("calls = %s\nwant    %s", got, want)
	}
	if len(fl.digests) != 2 || fl.digests[0] != nk.JWTDigest() || fl.digests[1] != nk.JWTDigest() {
		t.Fatalf("digests = %v, want the new secret's %s", fl.digests, nk.JWTDigest())
	}
	if len(fl.classes) != 0 || len(fl.failed) != 0 {
		t.Fatalf("a replica was restarted as a whole or reported as failed: %v %v", fl.classes, fl.failed)
	}
	if kinds := eventKinds(t, h, p.Ref); contains(kinds, EventReplicaKeysFailed) {
		t.Fatalf("events = %v", kinds)
	}
}

func TestRotateKeysUpdatesTheReplicasAfterThePrimary(t *testing.T) {
	h, p, fl, ids := replicaHarness(t)
	nk, err := h.e.RotateKeys(context.Background(), p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	want := "Reconfigure " + p.Ref + ",Replica " + ids[0] + " api,Replica " + ids[1] + " api"
	if got := inReplicaOrder(h.plane.calls); got != want {
		t.Fatalf("calls = %s\nwant    %s", got, want)
	}
	for _, d := range fl.digests {
		if d != nk.JWTDigest() {
			t.Fatalf("digest %s, want %s", d, nk.JWTDigest())
		}
	}
}

// A replica that cannot be updated does not take the primary's change back: that would restore the secret
// that was replaced, on the primary and on the replicas that had taken the new one. The change is reported
// with the replica that missed it, the others are updated, and the event is on the project.
func TestAReplicaThatCannotBeUpdatedKeepsTheNewKeysAndIsNamed(t *testing.T) {
	h, p, fl, ids := replicaHarness(t)
	ctx := context.Background()
	boom := errors.New("node third is away")
	fl.apiFailing[ids[1]] = boom

	nk, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret)
	var rk *ReplicaKeysError
	if !errors.As(err, &rk) || len(rk.Failed) != 1 || rk.Failed[0].Identifier != ids[1] || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a ReplicaKeysError naming %s", err, ids[1])
	}
	var je *JWTSecretError
	if errors.As(err, &je) {
		t.Fatal("a replica's failure was reported as a failed change that was restored")
	}
	if nk == nil || nk.JWTSecret != hostedSecret {
		t.Fatalf("keys = %+v, want the new ones with the error", nk)
	}
	if !strings.Contains(err.Error(), ids[1]) || !strings.Contains(err.Error(), "restart each replica") {
		t.Fatalf("message = %q", err)
	}
	// Nothing rolled back: one restart of the primary's services, the new secret stored, the tenants on it.
	if len(h.plane.reconf) != 1 || h.plane.reconf[0] != hostedSecret {
		t.Fatalf("reconfigure got %v", h.plane.reconf)
	}
	if stored, _ := h.e.Keys(ctx, p.Ref); stored.JWTSecret != hostedSecret {
		t.Fatal("the new secret was taken back")
	}
	if last := h.tenant.ensured[len(h.tenant.ensured)-1]; last.JWTSecret != hostedSecret {
		t.Fatalf("tenant spec = %+v", last)
	}
	// The other replica was updated all the same.
	if got := apiCalls(h.plane.calls); !contains(got, ids[0]) || !contains(got, ids[1]) {
		t.Fatalf("api calls = %v", got)
	}
	kinds := eventKinds(t, h, p.Ref)
	if !contains(kinds, EventReplicaKeysFailed) || !contains(kinds, "project.jwt_secret_changed") {
		t.Fatalf("events = %v", kinds)
	}

	// Making the change again with the same secret is the retry: the primary is not restarted, the replicas
	// are asked once more, and the one that was away takes it.
	delete(fl.apiFailing, ids[1])
	before := len(h.plane.reconf)
	h.plane.calls = nil
	nk2, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if nk2.JWTSecret != hostedSecret || len(h.plane.reconf) != before {
		t.Fatalf("the retry restarted the primary or changed the keys: %d restarts", len(h.plane.reconf))
	}
	if got := inReplicaOrder(h.plane.calls); got != "Replica "+ids[0]+" api,Replica "+ids[1]+" api" {
		t.Fatalf("retry calls = %s", got)
	}
}

// A change that is taken back never reached a replica.
func TestAChangeThatIsRolledBackLeavesTheReplicasAlone(t *testing.T) {
	ctx := context.Background()
	for name, arrange := range map[string]func(h *harness){
		"restart": func(h *harness) { h.plane.failOnce["Reconfigure"] = errors.New("gotrue will not start") },
		"tenants": func(h *harness) { h.tenant.err = errors.New("realtime refused") },
	} {
		t.Run(name, func(t *testing.T) {
			h, p, fl, _ := replicaHarness(t)
			arrange(h)
			_, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret)
			var je *JWTSecretError
			if !errors.As(err, &je) {
				t.Fatalf("err = %v", err)
			}
			if got := apiCalls(h.plane.calls); len(got) != 0 || len(fl.digests) != 0 {
				t.Fatalf("replicas were asked after a change that was taken back: %v", got)
			}
		})
	}
	h, p, fl, _ := replicaHarness(t)
	h.plane.failOnce["Reconfigure"] = errors.New("postgrest will not start")
	if _, err := h.e.RotateKeys(ctx, p.Ref); err == nil || len(fl.digests) != 0 {
		t.Fatalf("rotate: %v, digests %v", err, fl.digests)
	}
}

// A node that never answers holds the change for the replica's deadline and no longer.
func TestAHungReplicaCostsTheChangeItsDeadlineOnly(t *testing.T) {
	h, p, fl, ids := replicaHarness(t)
	h.e.opts.ReplicaRestartTimeout = 50 * time.Millisecond
	fl.apiHung[ids[0]] = true
	start := time.Now()
	_, err := h.e.SetJWTSecret(context.Background(), p.Ref, hostedSecret)
	var rk *ReplicaKeysError
	if !errors.As(err, &rk) || len(rk.Failed) != 1 || rk.Failed[0].Identifier != ids[0] || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the change took %s", time.Since(start))
	}
	if got := apiCalls(h.plane.calls); !contains(got, ids[1]) {
		t.Fatalf("the other replica was not updated: %v", got)
	}
}

// A replica that is being set up or removed reads the keys when it starts.
func TestReplicasThatAreNotRunningAreLeftAlone(t *testing.T) {
	h, p, _, ids := replicaHarness(t)
	ctx := context.Background()
	if err := h.reg.SetReplicaStatus(ctx, ids[0], string(registry.StatusGoingDown), registry.ReplicaStepDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetReplicaStatus(ctx, ids[1], string(registry.StatusActiveUnhealthy), registry.ReplicaStepDone, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret); err != nil {
		t.Fatal(err)
	}
	// An unhealthy replica is still running: its PostgREST gets the keys too.
	if got := apiCalls(h.plane.calls); len(got) != 1 || got[0] != ids[1] {
		t.Fatalf("api calls = %v, want only %s", got, ids[1])
	}
}

// The replicas of a paused project keep running, and its Resume does not render them: they get the secret
// at the change, or they would serve the previous one after the project is back.
func TestAPausedProjectsRunningReplicasStillGetTheSecret(t *testing.T) {
	h, p, fl, ids := replicaHarness(t)
	ctx := context.Background()
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	h.plane.calls = nil
	reconf := len(h.plane.reconf)
	if _, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret); err != nil {
		t.Fatal(err)
	}
	if len(h.plane.reconf) != reconf {
		t.Fatal("a paused project was restarted")
	}
	if got := inReplicaOrder(h.plane.calls); got != "Replica "+ids[0]+" api,Replica "+ids[1]+" api" {
		t.Fatalf("calls = %s", got)
	}
	if len(fl.digests) != 2 {
		t.Fatalf("digests = %v", fl.digests)
	}
}

// The command line has no way to reach the other nodes: it says which replicas it left on the previous
// secret, and still returns the keys it issued.
func TestAKeyChangeWithoutAReplicaFleetNamesTheRunningReplicas(t *testing.T) {
	h, p, _, ids := replicaHarness(t)
	h.e.opts.Replicas = nil
	nk, err := h.e.RotateKeys(context.Background(), p.Ref)
	var rk *ReplicaKeysError
	if !errors.As(err, &rk) || len(rk.Failed) != 2 || rk.Failed[0].Identifier != ids[0] || rk.Failed[1].Identifier != ids[1] || !errors.Is(err, errNoReplicaFleet) {
		t.Fatalf("err = %v", err)
	}
	if nk == nil || nk.JWTSecret == "" {
		t.Fatal("the new keys were not returned")
	}
	// A project with no replica is not told about a fleet it does not need.
	h2 := newHarness(t)
	p2 := h2.create(t)
	if _, err := h2.e.RotateKeys(context.Background(), p2.Ref); err != nil {
		t.Fatalf("a project with no replica: %v", err)
	}
}

func TestAKeyChangeThatCannotListTheReplicasSaysSo(t *testing.T) {
	h, p, fl, _ := replicaHarness(t)
	boom := errors.New("registry gone")
	fl.listErr = boom
	nk, err := h.e.SetJWTSecret(context.Background(), p.Ref, hostedSecret)
	var rk *ReplicaKeysError
	if !errors.As(err, &rk) || len(rk.Failed) != 0 || !errors.Is(err, boom) || !strings.Contains(err.Error(), "could not be listed") {
		t.Fatalf("err = %v", err)
	}
	if nk == nil || nk.JWTSecret != hostedSecret {
		t.Fatalf("keys = %+v", nk)
	}
}

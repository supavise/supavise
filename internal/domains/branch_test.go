package domains

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// A branch is a project, so a branch ref takes every hostname and vanity call a project does. These
// tests run them on a branch of refA: refC.
const refC = "cccccccccccccccccccc"

func (e *env) addBranch(t *testing.T, ref, parent string) {
	t.Helper()
	ctx := context.Background()
	p, err := e.reg.GetProject(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	err = e.reg.CreateProject(ctx, &registry.Project{
		Ref: ref, OrgID: p.OrgID, Name: "feature-" + ref[:3], Engine: registry.EnginePostgres, Status: registry.StatusActiveHealthy,
		Branch: &registry.BranchInfo{ID: "7c0c9c4e-0000-4000-8000-" + strings.Repeat("0", 11) + ref[:1], ParentRef: parent, Name: "feature-" + ref[:3], State: registry.BranchFunctionsDeployed},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBranchTakesACustomHostname(t *testing.T) {
	e := newEnv(t, nil)
	e.addBranch(t, refC, refA)
	ctx := context.Background()

	st := e.verified(t, refC, "preview.example.org")
	if st.Target != refC+".api.example.com" {
		t.Fatalf("the CNAME target of a branch is %q, want its own host", st.Target)
	}
	if st.TXTName != "_supavise-challenge.preview.example.org" {
		t.Fatalf("TXT name %q", st.TXTName)
	}
	act, changed, err := e.svc.Activate(ctx, refC)
	if err != nil || !changed || act.Status != registry.HostnameActive {
		t.Fatalf("activate: %+v %v %v", act, changed, err)
	}
	routes, _ := e.reg.ListRoutes(ctx)
	var found bool
	for _, r := range routes {
		if r.Host == "preview.example.org" {
			found = r.Ref == refC && r.Kind == registry.RouteCustom
		}
	}
	if !found {
		t.Fatalf("no custom route for the branch: %+v", routes)
	}
	// GoTrue presents it.
	if host, err := ExternalHost(ctx, e.reg, e.cfg, refC); err != nil || host != "preview.example.org" {
		t.Fatalf("external host = %q, %v", host, err)
	}
	// It is the branch's alone: the parent has no hostname, and cannot claim the branch's.
	if _, err := e.svc.Hostname(ctx, refA); kindOf(t, err) != KindNotConfigured {
		t.Fatalf("the parent got a hostname: %v", err)
	}
	if host, _ := ExternalHost(ctx, e.reg, e.cfg, refA); host != "" {
		t.Fatalf("the parent presents %q", host)
	}
	if _, err := e.svc.Initialize(ctx, refA, "preview.example.org"); kindOf(t, err) != KindConflict {
		t.Fatalf("the parent claimed the branch's hostname: %v", err)
	}
	// And a second hostname needs the first to go, as on any project.
	if _, err := e.svc.Initialize(ctx, refC, "other.example.org"); kindOf(t, err) != KindState {
		t.Fatalf("second hostname while one is active: %v", err)
	}
}

// Nothing copies a hostname from a parent to a branch: a branch made from a parent that has an active
// hostname, with data or without, starts with none.
func TestBranchDoesNotInheritTheParentsHostname(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.verified(t, refA, "app.example.org")
	if _, _, err := e.svc.Activate(ctx, refA); err != nil {
		t.Fatal(err)
	}
	e.addBranch(t, refC, refA)
	if _, err := e.svc.Hostname(ctx, refC); kindOf(t, err) != KindNotConfigured {
		t.Fatalf("the branch has a hostname it never claimed: %v", err)
	}
	if v, err := e.svc.Vanity(ctx, refC); err != nil || v.Status != VanityNotUsed {
		t.Fatalf("vanity of a new branch: %+v %v", v, err)
	}
	if host, _ := ExternalHost(ctx, e.reg, e.cfg, refC); host != "" {
		t.Fatalf("the branch presents %q", host)
	}
	// The parent's hostname is still the parent's, and the branch can claim a hostname of its own.
	if st, err := e.svc.Hostname(ctx, refA); err != nil || st.Status != registry.HostnameActive {
		t.Fatalf("the parent lost its hostname: %+v %v", st, err)
	}
	e.verified(t, refC, "preview.example.org")
}

// The vanity name is the cheap route for a preview branch: no certificate of its own, and the names of
// the parent and its branches are one namespace.
func TestBranchTakesAVanityName(t *testing.T) {
	e := newEnv(t, nil)
	e.addBranch(t, refC, refA)
	ctx := context.Background()
	host, err := e.svc.ActivateVanity(ctx, refC, "feature-one")
	if err != nil || host != "feature-one.api.example.com" {
		t.Fatalf("activate: %q %v", host, err)
	}
	if v, err := e.svc.Vanity(ctx, refC); err != nil || v.Status != VanityActive || v.CustomDomain != host {
		t.Fatalf("vanity: %+v %v", v, err)
	}
	if got, _ := ExternalHost(ctx, e.reg, e.cfg, refC); got != host {
		t.Fatalf("external host = %q", got)
	}
	// The parent and another branch cannot take it.
	if _, err := e.svc.ActivateVanity(ctx, refA, "feature-one"); kindOf(t, err) != KindConflict {
		t.Fatalf("the parent took the branch's name: %v", err)
	}
	e.addBranch(t, "dddddddddddddddddddd", refA)
	if _, err := e.svc.ActivateVanity(ctx, "dddddddddddddddddddd", "feature-one"); kindOf(t, err) != KindConflict {
		t.Fatalf("another branch took the name: %v", err)
	}
	// A vanity name and a custom hostname exclude each other on a branch too.
	e.verified(t, refC, "preview.example.org")
	if _, _, err := e.svc.Activate(ctx, refC); kindOf(t, err) != KindState {
		t.Fatalf("activating a hostname next to a vanity name: %v", err)
	}
	if err := e.svc.DeleteVanity(ctx, refC); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Activate(ctx, refC); err != nil {
		t.Fatalf("activate after the vanity name went: %v", err)
	}
	if v, _ := e.svc.Vanity(ctx, refC); v.Status != VanityCustomDomainUsed {
		t.Fatalf("vanity next to a custom domain: %+v", v)
	}
}

// The certificate budget of the node, 25 orders in 7 days, is one budget for every project and every
// branch: many branches cannot each take the whole of it.
func TestBranchesShareTheNodesCertificateBudget(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.svc = New(Options{Reg: e.reg, Config: e.cfg, Resolver: e.dns, Now: func() time.Time { return e.now },
		IssuancePerProject: 10, IssuanceProjectWindow: 24 * time.Hour, IssuancePerNode: 3, IssuanceNodeWindow: 7 * 24 * time.Hour})
	refs := []string{refC, "dddddddddddddddddddd", "eeeeeeeeeeeeeeeeeeee", "ffffffffffffffffffff"}
	for _, ref := range refs {
		e.addBranch(t, ref, refA)
	}
	for i, ref := range refs[:3] {
		if _, err := e.svc.ActivateVanity(ctx, ref, "feature-"+string(rune('a'+i))); err != nil {
			t.Fatalf("branch %d: %v", i, err)
		}
	}
	// A branch that has taken nothing yet is refused because of what the others took, with the node's
	// message and a wait.
	_, err := e.svc.ActivateVanity(ctx, refs[3], "feature-z")
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindRateLimited || !strings.Contains(de.Msg, "node") || de.RetryAfter <= 0 {
		t.Fatalf("fourth branch: %v", err)
	}
	// A parent project's change counts against the same window.
	if _, err := e.svc.ActivateVanity(ctx, refA, "main-site"); kindOf(t, err) != KindRateLimited {
		t.Fatalf("the parent after three branches: %v", err)
	}
	e.now = e.now.Add(7*24*time.Hour + time.Minute)
	if _, err := e.svc.ActivateVanity(ctx, refs[3], "feature-z"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// Deleting a branch, which is also what the sweeper does when its lifetime lapses, takes its hostname
// and name and the routes with it, and the hostname can be claimed again.
func TestDeletingABranchFreesItsHostnames(t *testing.T) {
	e := newEnv(t, nil)
	e.addBranch(t, refC, refA)
	ctx := context.Background()
	e.verified(t, refC, "preview.example.org")
	if _, _, err := e.svc.Activate(ctx, refC); err != nil {
		t.Fatal(err)
	}
	e.addBranch(t, "dddddddddddddddddddd", refA)
	if _, err := e.svc.ActivateVanity(ctx, "dddddddddddddddddddd", "feature-two"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{refC, "dddddddddddddddddddd"} {
		if err := e.reg.DeleteProject(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if rs, _ := e.reg.ListRoutes(ctx); len(rs) != 0 {
		t.Fatalf("routes left: %+v", rs)
	}
	if hs, _ := e.reg.ListCustomHostnames(ctx); len(hs) != 0 {
		t.Fatalf("hostnames left: %+v", hs)
	}
	// Both are free for the parent.
	e.verified(t, refA, "preview.example.org")
	if _, err := e.svc.ActivateVanity(ctx, refB, "feature-two"); err != nil {
		t.Fatalf("the vanity name was not released: %v", err)
	}
}

// A node whose certificates come from a DNS-01 wildcard cannot serve a hostname of its own, for a branch
// as for a project.
func TestBranchHostnameIsRefusedOnADNS01Node(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.TLS.Mode, c.TLS.DNSProvider = "dns01", "route53" })
	e.addBranch(t, refC, refA)
	if _, err := e.svc.Initialize(context.Background(), refC, "preview.example.org"); kindOf(t, err) != KindUnavailable {
		t.Fatalf("initialize on dns01: %v", err)
	}
	// A vanity name needs no certificate of its own there and works.
	if _, err := e.svc.ActivateVanity(context.Background(), refC, "feature-one"); err != nil {
		t.Fatalf("vanity on dns01: %v", err)
	}
}

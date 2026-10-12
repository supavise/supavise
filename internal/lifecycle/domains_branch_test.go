package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

// A branch is a project with hostnames of its own: GoTrue of the branch builds its links on the
// branch's custom hostname or vanity name, and the parent's keep to the parent's.
func TestBranchGoTrueUsesTheBranchsOwnDomain(t *testing.T) {
	ctx := context.Background()
	pl, cfg := testPlane(t)
	pl.opts.Settings = &settingsEcho{}
	const parent, branch = "abcdefghijklmnopqrst", "bcdefghijklmnopqrstu"
	if err := pl.reg.CreateProject(ctx, &registry.Project{Ref: parent, Name: "parent", Engine: registry.EnginePostgres}); err != nil {
		t.Fatal(err)
	}
	if err := pl.reg.CreateProject(ctx, &registry.Project{Ref: branch, Name: "feature", Engine: registry.EnginePostgres,
		Branch: &registry.BranchInfo{ID: "7c0c9c4e-0000-4000-8000-000000000001", ParentRef: parent, Name: "feature"}}); err != nil {
		t.Fatal(err)
	}
	d := registry.Domains(pl.reg)
	external := func(ref string, seq int) string {
		t.Helper()
		specs, err := pl.apiSpecs(ctx, testProject(cfg, ref, seq), testKeys(t, ref))
		if err != nil {
			t.Fatal(err)
		}
		return specs[0].Env["API_EXTERNAL_URL"]
	}
	if got := external(branch, 3); got != "https://"+branch+".api.example.test/auth/v1" {
		t.Fatalf("branch with no domain: %q", got)
	}

	// The branch's vanity name changes the branch's GoTrue and not the parent's.
	if err := d.PutVanitySubdomain(ctx, branch, "feature-one", cfg.VanityHost("feature-one")); err != nil {
		t.Fatal(err)
	}
	if got := external(branch, 3); got != "https://feature-one.api.example.test/auth/v1" {
		t.Errorf("branch vanity: %q", got)
	}
	if got := external(parent, 2); got != "https://"+parent+".api.example.test/auth/v1" {
		t.Errorf("the parent follows its branch's vanity name: %q", got)
	}

	// A custom hostname of the parent is not the branch's.
	if err := d.PutCustomHostname(ctx, &registry.CustomHostname{Ref: parent, Hostname: "app.customer.example", Status: registry.HostnameInitiated, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	h, _ := d.GetCustomHostname(ctx, parent)
	h.Status, h.CNAMEOK, h.TXTOK = registry.HostnameOriginReady, true, true
	if err := d.UpdateCustomHostname(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ActivateCustomHostname(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if got := external(parent, 2); got != "https://app.customer.example/auth/v1" {
		t.Errorf("parent custom hostname: %q", got)
	}
	if got := external(branch, 3); got != "https://feature-one.api.example.test/auth/v1" {
		t.Errorf("the branch follows its parent's hostname: %q", got)
	}
	if _, err := d.GetCustomHostname(ctx, branch); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("the branch has a hostname it was never given: %v", err)
	}
}

// A reset removes the branch's cluster and builds it again over the same row: its hostname, vanity name
// and the routes of both stay. Deleting the branch frees all three, and the parent's are untouched.
func TestResetKeepsABranchsHostnamesAndDeleteFreesThem(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	parent := h.create(t)
	branch, err := h.e.Create(ctx, CreateRequest{Name: "feature", Branch: &registry.BranchInfo{
		ID: "7c0c9c4e-0000-4000-8000-000000000001", ParentRef: parent.Ref, Name: "feature",
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := registry.Domains(h.reg)
	if err := d.PutVanitySubdomain(ctx, parent.Ref, "main-site", h.cfg.VanityHost("main-site")); err != nil {
		t.Fatal(err)
	}
	if err := d.PutVanitySubdomain(ctx, branch.Ref, "feature-one", h.cfg.VanityHost("feature-one")); err != nil {
		t.Fatal(err)
	}
	if err := d.PutCustomHostname(ctx, &registry.CustomHostname{Ref: branch.Ref, Hostname: "preview.customer.example", Status: registry.HostnameInitiated, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	routes := func() map[string]string {
		rs, _ := h.reg.ListRoutes(ctx)
		m := map[string]string{}
		for _, r := range rs {
			m[r.Host] = r.Ref
		}
		return m
	}
	before := routes()
	if before[h.cfg.VanityHost("feature-one")] != branch.Ref {
		t.Fatalf("routes = %v", before)
	}

	// The reset: the cluster goes, the row and the domain rows stay, and the same ref comes back.
	if err := h.e.DeleteWith(ctx, branch.Ref, DeleteOptions{KeepRecord: true, SkipFinalBackup: true, PurgeObjects: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetVanitySubdomain(ctx, branch.Ref); got == nil || got.Name != "feature-one" {
		t.Fatalf("the vanity name did not survive the reset: %+v", got)
	}
	if got, _ := d.GetCustomHostname(ctx, branch.Ref); got == nil || got.Hostname != "preview.customer.example" {
		t.Fatalf("the hostname did not survive the reset: %+v", got)
	}
	if after := routes(); after[h.cfg.VanityHost("feature-one")] != branch.Ref {
		t.Fatalf("the vanity route did not survive the reset: %v", after)
	}

	// Deleting the branch for good frees the name and the hostname; the parent keeps its own.
	if err := h.e.DeleteWith(ctx, branch.Ref, DeleteOptions{SkipFinalBackup: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetVanitySubdomain(ctx, branch.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("the vanity name stayed: %v", err)
	}
	if _, err := d.GetCustomHostname(ctx, branch.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("the hostname stayed: %v", err)
	}
	after := routes()
	if _, ok := after[h.cfg.VanityHost("feature-one")]; ok {
		t.Errorf("the vanity route stayed: %v", after)
	}
	if after[h.cfg.VanityHost("main-site")] != parent.Ref {
		t.Errorf("the parent's route went with the branch: %v", after)
	}
	if owner, err := d.VanitySubdomainOwner(ctx, "feature-one"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("the name is still held by %q", owner)
	}
}

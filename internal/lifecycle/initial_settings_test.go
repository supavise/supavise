package lifecycle

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
)

// A branch's units first start on the settings its parent had saved; a project made again over its
// row (a reset) keeps its own.
func TestABranchStartsOnItsParentsSettingsAndARecreateKeepsItsOwn(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	parent := h.create(t)
	patch := func(ref string, svc projectconfig.Service, body map[string]any) {
		t.Helper()
		if _, err := h.set.Patch(ctx, ref, svc, body, projectconfig.CrossContext{}); err != nil {
			t.Fatal(err)
		}
	}
	patch(parent.Ref, projectconfig.Auth, map[string]any{"site_url": "https://parent.example", "external_github_enabled": true,
		"external_github_client_id": "id", "external_github_secret": "s3cret", "mfa_max_enrolled_factors": 4})
	patch(parent.Ref, projectconfig.PostgREST, map[string]any{"max_rows": 77})
	patch(parent.Ref, projectconfig.Postgres, map[string]any{"statement_timeout": "9s", "shared_buffers": "2GB"})

	const ref = "bbbbbbbbbbbbbbbbbbbb"
	info := &registry.BranchInfo{ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: parent.Ref, Name: "feature", State: registry.BranchCreatingProject}
	if _, err := h.e.Create(ctx, CreateRequest{Name: "feature", Ref: ref, Class: "micro", Branch: info, SettingsFrom: parent.Ref}); err != nil {
		t.Fatal(err)
	}
	got := func(svc projectconfig.Service) projectconfig.Values {
		t.Helper()
		st, err := h.set.Get(ctx, ref, svc)
		if err != nil {
			t.Fatal(err)
		}
		return st.Set
	}
	auth := got(projectconfig.Auth)
	if auth["external_github_enabled"] != true || auth["external_github_secret"] != "s3cret" || auth["mfa_max_enrolled_factors"] != int64(4) {
		t.Fatalf("auth settings of the branch = %v", auth)
	}
	if _, ok := auth["site_url"]; ok {
		t.Fatalf("the parent's site_url was copied: %v", auth["site_url"])
	}
	// What a new project starts with is still there (the copy lays over it).
	if auth["mailer_otp_exp"] != int64(3600) {
		t.Fatalf("mailer_otp_exp = %v", auth["mailer_otp_exp"])
	}
	if got(projectconfig.PostgREST)["max_rows"] != int64(77) {
		t.Fatalf("postgrest = %v", got(projectconfig.PostgREST))
	}
	if pg := got(projectconfig.Postgres); pg["statement_timeout"] != "9s" || pg["shared_buffers"] != nil {
		t.Fatalf("postgres = %v (the timeout copies, the sizing does not)", pg)
	}
	var kinds []string
	evs, _ := h.reg.ListEvents(ctx, ref, 50)
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	sort.Strings(kinds)
	found := false
	for _, k := range kinds {
		found = found || k == "project.settings_copied"
	}
	if !found {
		t.Fatalf("no project.settings_copied event: %v", kinds)
	}

	// The branch's own change, then a recreate asking for the parent's settings again: it keeps its own.
	patch(ref, projectconfig.PostgREST, map[string]any{"max_rows": 5})
	if err := h.e.DeleteWith(ctx, ref, DeleteOptions{SkipFinalBackup: true, KeepRecord: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Create(ctx, CreateRequest{Ref: ref, Class: "micro", Recreate: true, SettingsFrom: parent.Ref}); err != nil {
		t.Fatal(err)
	}
	if got(projectconfig.PostgREST)["max_rows"] != int64(5) {
		t.Fatalf("a recreate took the parent's settings: %v", got(projectconfig.PostgREST))
	}
}

// The compute class sets these on the postmaster's command line, so a branch never copies them.
func TestSizingSettingsAreTheCommandLineOnes(t *testing.T) {
	a, b := SizingSettings(), append([]string(nil), projectconfig.SizingSettings...)
	sort.Strings(a)
	sort.Strings(b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("lifecycle %v, projectconfig %v", a, b)
	}
}

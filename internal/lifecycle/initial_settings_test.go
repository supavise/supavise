package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// A branch's units first start on the settings its parent had saved, less the credentials; a project
// made again over its row (a reset) keeps its own.
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
	if auth["external_github_client_id"] != "id" || auth["mfa_max_enrolled_factors"] != int64(4) {
		t.Fatalf("auth settings of the branch = %v", auth)
	}
	// The provider's secret is the parent's: the branch's owner sets its own, and the provider stays off until then.
	if _, ok := auth["external_github_secret"]; ok {
		t.Fatalf("the parent's OAuth secret was copied: %v", auth)
	}
	if _, ok := auth["external_github_enabled"]; ok {
		t.Fatalf("a provider without its secret was switched on: %v", auth)
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
	// The event names what the branch did not get, and never a value.
	var copied struct {
		From     string
		Withheld map[string][]string
		Deferred map[string][]string
	}
	for _, e := range evs {
		if e.Kind == "project.settings_copied" {
			if strings.Contains(string(e.Payload), "s3cret") {
				t.Fatalf("the event holds a secret: %s", e.Payload)
			}
			if err := json.Unmarshal(e.Payload, &copied); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !reflect.DeepEqual(copied.Withheld["auth"], []string{"external_github_enabled", "external_github_secret"}) || copied.From != parent.Ref {
		t.Fatalf("settings_copied event = %+v", copied)
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

// postgrestProbePlane records the PostgREST schemas the saved settings render when the data plane
// creates a project, which is when its units are rendered for the first time.
type postgrestProbePlane struct {
	*dataAPIPlane
	set *projectconfig.Manager
	mu  sync.Mutex
	// schemas is PGRST_DB_SCHEMAS by ref ("" when the saved settings leave the default).
	schemas map[string]string
}

func (p *postgrestProbePlane) Create(ctx context.Context, pr *registry.Project, k *secrets.ProjectKeys, seed DataSeeder) error {
	env, err := p.set.PostgRESTEnv(ctx, pr.Ref)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.schemas[pr.Ref] = env["PGRST_DB_SCHEMAS"]
	p.mu.Unlock()
	return p.dataAPIPlane.Create(ctx, pr, k, seed)
}

// A branch whose database starts empty first starts PostgREST on the default schemas, because the
// schemas the parent exposes are not there yet and PostgREST does not start on one that is missing.
// A branch that starts from the parent's data has them, and takes the list.
func TestABranchWithAnEmptyDatabaseStartsPostgRESTOnTheDefaultSchemas(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	probe := &postgrestProbePlane{dataAPIPlane: h.dp, set: h.set, schemas: map[string]string{}}
	e := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, probe, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup, Settings: h.set})
	parent, err := e.Create(ctx, CreateRequest{Name: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.set.Patch(ctx, parent.Ref, projectconfig.PostgREST, map[string]any{"db_schema": "public,api", "max_rows": 77}, projectconfig.CrossContext{}); err != nil {
		t.Fatal(err)
	}

	branch := func(ref string, seed DataSeeder) {
		t.Helper()
		info := &registry.BranchInfo{ID: secrets.NewUUID(), ParentRef: parent.Ref, Name: "b-" + ref[:1], State: registry.BranchCreatingProject}
		req := CreateRequest{Name: info.Name, Ref: ref, Class: "micro", Branch: info, SettingsFrom: parent.Ref, Seed: seed}
		if seed != nil { // a seeded cluster brings its own credentials
			k, err := secrets.NewProjectKeys(ref, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			req.Keys = k
		}
		if _, err := e.Create(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	const empty, cloned = "bbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccc"
	branch(empty, nil)
	branch(cloned, func(context.Context, *registry.Project, string) error { return nil })

	if got := probe.schemas[empty]; got != "" {
		t.Errorf("PostgREST of the empty branch first rendered PGRST_DB_SCHEMAS=%q, want the default", got)
	}
	if got := probe.schemas[cloned]; got != "public,api" {
		t.Errorf("PostgREST of the branch with data first rendered PGRST_DB_SCHEMAS=%q, want the parent's list", got)
	}
	// The rest of PostgREST's settings come along either way.
	for _, ref := range []string{empty, cloned} {
		st, err := h.set.Get(ctx, ref, projectconfig.PostgREST)
		if err != nil {
			t.Fatal(err)
		}
		if st.Set["max_rows"] != int64(77) {
			t.Errorf("%s: max_rows = %v", ref, st.Set["max_rows"])
		}
	}
	// The empty branch's event says the list waits.
	evs, _ := h.reg.ListEvents(ctx, empty, 50)
	var payload struct{ Deferred map[string][]string }
	for _, ev := range evs {
		if ev.Kind == "project.settings_copied" {
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !reflect.DeepEqual(payload.Deferred["postgrest"], []string{"db_schema"}) {
		t.Errorf("deferred = %v", payload.Deferred)
	}
}

// ExposeSchemasThatExist is what a schema-only branch calls once its schemas are built: it keeps the
// names that are schemas now, in order, and restarts PostgREST only when the list changes.
func TestExposeSchemasThatExistKeepsTheNamesThatDoAndRestartsPostgREST(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	h.e.schemaProbe = func(_ context.Context, _ string, names []string) ([]string, error) {
		var missing []string
		for _, n := range names {
			if strings.HasPrefix(n, "gone") {
				missing = append(missing, n)
			}
		}
		return missing, nil
	}
	p := h.create(t)
	saved := func() string {
		t.Helper()
		s, err := h.e.SavedExposedSchemas(ctx, p.Ref)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if saved() != "" {
		t.Fatalf("a new project has db_schema %q saved", saved())
	}

	exposed, missing, err := h.e.ExposeSchemasThatExist(ctx, p.Ref, "api, gone1 ,public,gone2")
	if err != nil || !reflect.DeepEqual(exposed, []string{"api", "public"}) || !reflect.DeepEqual(missing, []string{"gone1", "gone2"}) {
		t.Fatalf("expose: %v, missing %v, %v", exposed, missing, err)
	}
	if saved() != "api,public" || len(h.dp.reconfigured) != 1 || h.dp.reconfigured[0] != p.Ref+" postgrest" {
		t.Fatalf("saved %q, restarted %v", saved(), h.dp.reconfigured)
	}
	// The same list again changes nothing and restarts nothing.
	if _, _, err := h.e.ExposeSchemasThatExist(ctx, p.Ref, "api,public"); err != nil || len(h.dp.reconfigured) != 1 {
		t.Fatalf("a list PostgREST already runs on restarted it: %v, %v", h.dp.reconfigured, err)
	}
	// None of them exists: nothing is saved.
	exposed, missing, err = h.e.ExposeSchemasThatExist(ctx, p.Ref, "gone1,gone3")
	if err != nil || len(exposed) != 0 || len(missing) != 2 || saved() != "api,public" || len(h.dp.reconfigured) != 1 {
		t.Fatalf("expose none: %v, %v, %v; saved %q, restarted %v", exposed, missing, err, saved(), h.dp.reconfigured)
	}
	// An empty list is nothing to do; a failed restart is an error.
	if exposed, missing, err = h.e.ExposeSchemasThatExist(ctx, p.Ref, " , "); err != nil || exposed != nil || missing != nil {
		t.Fatalf("expose empty: %v %v %v", exposed, missing, err)
	}
	h.dp.reconfErr = errors.New("postgrest did not answer")
	if _, _, err = h.e.ExposeSchemasThatExist(ctx, p.Ref, "public"); err == nil || !strings.Contains(err.Error(), "postgrest did not answer") {
		t.Fatalf("expose with a failing restart: %v", err)
	}
}

// SetExposedSchemas puts a list aside or back without restarting anything; an empty list is the default.
func TestSetExposedSchemasSavesWithoutRestartingAndEmptyIsTheDefault(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	p := h.create(t)
	if err := h.e.SetExposedSchemas(ctx, p.Ref, "public, api"); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.e.SavedExposedSchemas(ctx, p.Ref); got != "public,api" || len(h.dp.reconfigured) != 0 {
		t.Fatalf("saved %q, restarted %v", got, h.dp.reconfigured)
	}
	if err := h.e.SetExposedSchemas(ctx, p.Ref, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.e.SavedExposedSchemas(ctx, p.Ref); got != "" {
		t.Fatalf("saved %q after the default was asked for", got)
	}
}

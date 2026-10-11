package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
)

// dataAPIPlane is a fakePlane that can run the Data API statements, and records them.
type dataAPIPlane struct {
	*fakePlane
	dmu  sync.Mutex
	runs []string
	err  error
}

func (d *dataAPIPlane) ApplyDataAPI(_ context.Context, p *registry.Project, s DataAPISetup) error {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	d.runs = append(d.runs, p.Ref+": "+strings.Join(s.Statements(), "; "))
	return d.err
}

func (d *dataAPIPlane) ran() []string {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	return append([]string(nil), d.runs...)
}

type dataAPIEnv struct {
	*harness
	dp  *dataAPIPlane
	set *projectconfig.Manager
}

func newDataAPIHarness(t *testing.T) *dataAPIEnv {
	t.Helper()
	h := newHarness(t)
	dp := &dataAPIPlane{fakePlane: h.plane}
	set := projectconfig.NewManager(projectconfig.NewMemory(), h.sec, projectconfig.Options{})
	h.e = NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, dp, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup, Settings: set})
	return &dataAPIEnv{harness: h, dp: dp, set: set}
}

func choiceOf(t *testing.T, reg registry.Registry, ref string) *registry.DataAPI {
	t.Helper()
	d, err := registry.DataAPIOf(reg).GetDataAPI(context.Background(), ref)
	if err != nil {
		t.Fatalf("the Data API choice of %s: %v", ref, err)
	}
	return d
}

func TestRevokeSQLIsHostedsChange(t *testing.T) {
	want := []string{
		"alter default privileges for role postgres in schema public revoke select, insert, update, delete on tables from anon, authenticated, service_role",
		"alter default privileges for role postgres in schema public revoke usage, select on sequences from anon, authenticated, service_role",
	}
	if got := (DataAPISetup{RevokeDefaultPrivileges: true}).Statements(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements:\n%s", strings.Join(got, "\n"))
	}
	if got := (DataAPISetup{}).Statements(); len(got) != 0 {
		t.Fatalf("an empty setup runs %v", got)
	}
	both := (DataAPISetup{RevokeDefaultPrivileges: true, APISchema: true}).Statements()
	if len(both) != 4 || !strings.HasPrefix(both[2], "create schema if not exists api") {
		t.Fatalf("with the api schema: %v", both)
	}
}

// A new project follows hosted's default: the grants are revoked, and the decision is recorded.
func TestCreateRevokesTheDefaultPrivileges(t *testing.T) {
	h := newDataAPIHarness(t)
	p := h.create(t)
	if got := h.dp.ran(); len(got) != 1 || !strings.HasPrefix(got[0], p.Ref+": alter default privileges for role postgres in schema public revoke select") {
		t.Fatalf("the plane ran %v", got)
	}
	if d := choiceOf(t, h.reg, p.Ref); d.DefaultPrivileges != registry.PrivilegesRevoked || d.Source != registry.DataAPICreate {
		t.Fatalf("choice = %+v", d)
	}
	// The statements ran after the cluster came up and before the project turned healthy.
	if !h.plane.has("Create " + p.Ref) {
		t.Fatal("the cluster was not created")
	}
}

func TestCreateWithExposeNewTablesKeepsTheGrants(t *testing.T) {
	h := newDataAPIHarness(t)
	p, err := h.e.Create(context.Background(), CreateRequest{Name: "open", DataAPI: DataAPIOptions{ExposeNewTables: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.dp.ran(); len(got) != 0 {
		t.Fatalf("the plane ran %v", got)
	}
	if d := choiceOf(t, h.reg, p.Ref); d.DefaultPrivileges != registry.PrivilegesGranted || d.Source != registry.DataAPICreate {
		t.Fatalf("choice = %+v", d)
	}
}

func TestCreateWithTheAPISchemaAndExposedSchemas(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	p, err := h.e.Create(ctx, CreateRequest{Name: "api", DataAPI: DataAPIOptions{APISchema: true, ExposedSchemas: "api,graphql_public"}})
	if err != nil {
		t.Fatal(err)
	}
	got := h.dp.ran()
	if len(got) != 1 || !strings.Contains(got[0], "revoke select, insert, update, delete on tables") || !strings.Contains(got[0], "create schema if not exists api authorization postgres") {
		t.Fatalf("the plane ran %v", got)
	}
	st, err := h.set.Get(ctx, p.Ref, projectconfig.PostgREST)
	if err != nil || st.Effective.Str("db_schema") != "api,graphql_public" {
		t.Fatalf("db_schema = %q, %v", st.Effective.Str("db_schema"), err)
	}
}

func TestCreateWithoutASettingsStoreRefusesExposedSchemas(t *testing.T) {
	h := newHarness(t) // Settings is nil
	_, err := h.e.Create(context.Background(), CreateRequest{Name: "x", DataAPI: DataAPIOptions{ExposedSchemas: "api"}})
	if err == nil || !strings.Contains(err.Error(), "exposed schemas") {
		t.Fatalf("create: %v", err)
	}
	ps, _ := h.reg.ListProjects(context.Background())
	for _, p := range ps {
		if p.Ref != config.SystemRef && p.Status != registry.StatusInitFailed {
			t.Fatalf("project %s is %s, want INIT_FAILED", p.Ref, p.Status)
		}
	}
}

func TestCreateFailsWhenTheDataAPIStatementsFail(t *testing.T) {
	h := newDataAPIHarness(t)
	h.dp.err = errors.New("permission denied")
	_, err := h.e.Create(context.Background(), CreateRequest{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "data api") {
		t.Fatalf("create: %v", err)
	}
	ps, _ := h.reg.ListProjects(context.Background())
	for _, p := range ps {
		if _, derr := registry.DataAPIOf(h.reg).GetDataAPI(context.Background(), p.Ref); derr == nil {
			t.Fatalf("a failed create recorded a decision for %s", p.Ref)
		}
	}
}

// A restore as new and a data branch hold their source's database, default privileges included: no
// SQL runs, and the new project takes the decision of its source.
func TestSeededCreateCopiesTheSourceDecision(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	src, err := h.e.Create(ctx, CreateRequest{Name: "src", DataAPI: DataAPIOptions{ExposeNewTables: true}})
	if err != nil {
		t.Fatal(err)
	}
	seed := func(context.Context, *registry.Project, string) error { return nil }
	keys, err := h.e.Keys(ctx, src.Ref)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := h.e.Create(ctx, CreateRequest{Name: "copy", Seed: seed, Keys: keys, DataAPI: DataAPIOptions{CopyFrom: src.Ref}})
	if err != nil {
		t.Fatal(err)
	}
	if d := choiceOf(t, h.reg, cp.Ref); d.DefaultPrivileges != registry.PrivilegesGranted || d.Source != registry.DataAPICopy {
		t.Fatalf("choice = %+v", d)
	}
	if got := h.dp.ran(); len(got) != 0 {
		t.Fatalf("a seeded create ran %v", got)
	}
	// A source with nothing recorded (a project from before, or a deleted one) leaves the copy to the next pass.
	other, err := h.e.Create(ctx, CreateRequest{Name: "other", Seed: seed, Keys: keys, DataAPI: DataAPIOptions{CopyFrom: "zzzzzzzzzzzzzzzzzzzz"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DataAPIOf(h.reg).GetDataAPI(ctx, other.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("a copy of nothing recorded a decision: %v", err)
	}
}

// existing makes a running project the way an earlier release left it: no decision recorded.
func (h *dataAPIEnv) existing(t *testing.T, name string) *registry.Project {
	t.Helper()
	ctx := context.Background()
	org, err := h.e.org(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	p := &registry.Project{Ref: strings.Repeat(string(rune('a'+len(name)%26)), 20), OrgID: org.ID, Name: name, Status: registry.StatusActiveHealthy}
	if err := h.reg.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConvergeRevokesOnceForAnExistingProject(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	old := h.existing(t, "old")
	if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	got := h.dp.ran()
	if len(got) != 1 || !strings.HasPrefix(got[0], old.Ref+": alter default privileges for role postgres in schema public revoke select") {
		t.Fatalf("the plane ran %v", got)
	}
	if strings.Contains(got[0], "create schema") {
		t.Fatalf("the upgrade created a schema: %s", got[0])
	}
	d := choiceOf(t, h.reg, old.Ref)
	if d.DefaultPrivileges != registry.PrivilegesRevoked || d.Source != registry.DataAPIUpgrade {
		t.Fatalf("choice = %+v", d)
	}
	if !h.hasEvent(t, old.Ref, EventDataAPIRevoked) {
		t.Fatal("no project.data_api_defaults_revoked event")
	}
	// A second pass, as after a restart or after the user rolled the revoke back with SQL, runs nothing.
	for i := 0; i < 2; i++ {
		if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 {
			t.Fatalf("failed: %v", failed)
		}
	}
	if got := h.dp.ran(); len(got) != 1 {
		t.Fatalf("a later pass ran %v", got)
	}
}

func (h *dataAPIEnv) hasEvent(t *testing.T, ref, kind string) bool {
	t.Helper()
	evs, err := h.reg.ListEvents(context.Background(), ref, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

func TestConvergeLeavesAGrantedProjectAndTheSystemProjectAlone(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	open, err := h.e.Create(ctx, CreateRequest{Name: "open", DataAPI: DataAPIOptions{ExposeNewTables: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.CreateProject(ctx, &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	if got := h.dp.ran(); len(got) != 0 {
		t.Fatalf("the plane ran %v", got)
	}
	if d := choiceOf(t, h.reg, open.Ref); d.DefaultPrivileges != registry.PrivilegesGranted {
		t.Fatalf("an explicit choice was overruled: %+v", d)
	}
}

func TestConvergeSkipsAPausedProjectAndResumeDoesIt(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	// An Engine whose plane cannot run the statements leaves no decision, as an earlier release did.
	before := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup})
	p, err := before.Create(ctx, CreateRequest{Name: "paused"})
	if err != nil {
		t.Fatal(err)
	}
	if err := before.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 || len(h.dp.ran()) != 0 {
		t.Fatalf("a paused project was touched: %v %v", failed, h.dp.ran())
	}
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if got := h.dp.ran(); len(got) != 1 || !strings.HasPrefix(got[0], p.Ref+": ") {
		t.Fatalf("resume ran %v", got)
	}
	if d := choiceOf(t, h.reg, p.Ref); d.Source != registry.DataAPIUpgrade || d.DefaultPrivileges != registry.PrivilegesRevoked {
		t.Fatalf("choice = %+v", d)
	}
	// A second resume (pause, resume) does not run it again.
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if got := h.dp.ran(); len(got) != 1 {
		t.Fatalf("a second resume ran %v", got)
	}
}

func TestConvergeFailureIsRetriedAndRecordsNothing(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	p := h.existing(t, "stuck")
	h.dp.err = ErrNotPrimary
	failed := h.e.ConvergeDataAPI(ctx)
	if !errors.Is(failed[p.Ref], ErrNotPrimary) {
		t.Fatalf("failed = %v", failed)
	}
	if _, err := registry.DataAPIOf(h.reg).GetDataAPI(ctx, p.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("a failed pass recorded a decision: %v", err)
	}
	if !h.hasEvent(t, p.Ref, EventDataAPIFailed) {
		t.Fatal("no failure event")
	}
	h.dp.err = nil
	if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 {
		t.Fatalf("retry failed: %v", failed)
	}
	if d := choiceOf(t, h.reg, p.Ref); d.DefaultPrivileges != registry.PrivilegesRevoked {
		t.Fatalf("choice = %+v", d)
	}
}

// A project homed on another node is not this Engine's when its plane does not route by home; a
// router answers ErrNotSupported for what only the home can do, which is no failure.
func TestConvergeNotSupportedIsNotAFailure(t *testing.T) {
	h := newDataAPIHarness(t)
	h.dp.err = ErrNotSupported
	p := h.existing(t, "elsewhere")
	if failed := h.e.ConvergeDataAPI(context.Background()); len(failed) != 0 {
		t.Fatalf("failed = %v", failed)
	}
	if _, err := registry.DataAPIOf(h.reg).GetDataAPI(context.Background(), p.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("a decision was recorded for a project this node does not run: %v", err)
	}
}

// A follower's registry is read-only: its Engine decides nothing, the leader's does.
func TestConvergeDoesNothingOnAFollower(t *testing.T) {
	h := newDataAPIHarness(t)
	p := h.existing(t, "follower")
	f := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.dp, Options{ReadOnly: true})
	if failed := f.ConvergeDataAPI(context.Background()); len(failed) != 0 || len(h.dp.ran()) != 0 {
		t.Fatalf("a follower converged: %v %v", failed, h.dp.ran())
	}
	if _, err := registry.DataAPIOf(h.reg).GetDataAPI(context.Background(), p.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("a follower recorded a decision: %v", err)
	}
}

// A project homed on another node is left to its home by an Engine whose plane does not route by
// home (a router answers for it, and is the only plane that does).
func TestConvergeLeavesAProjectHomedElsewhereToItsHome(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := h.reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	p := h.existing(t, "remote")
	cl, err := h.reg.GetCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectNode(ctx, p.Ref, n2.ID, cl.Epoch); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.dp, Options{NodeID: "n1"})
	if failed := e.ConvergeDataAPI(ctx); len(failed) != 0 || len(h.dp.ran()) != 0 {
		t.Fatalf("a project homed elsewhere was converged here: %v %v", failed, h.dp.ran())
	}
}

func TestParseExposedSchemas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		list  []string
		given bool
		api   bool
		want  string
		err   string
	}{
		{name: "omitted keeps the default", want: ""},
		{name: "omitted with the api schema", api: true, want: "api,graphql_public"},
		{name: "a list", list: []string{"public", " api ", ""}, given: true, want: "public,api"},
		{name: "a list with the api schema puts it first", list: []string{"public"}, given: true, api: true, want: "api,public"},
		{name: "the api schema already listed", list: []string{"public", "api"}, given: true, api: true, want: "public,api"},
		{name: "the Data API off exposes nothing that holds a table", list: []string{}, given: true, want: "graphql_public"},
		{name: "the Data API off with the api schema", list: []string{}, given: true, api: true, want: "api,graphql_public"},
		{name: "a quote", list: []string{`pub"lic`}, given: true, err: "quote"},
	} {
		got, err := ParseExposedSchemas(tc.list, tc.given, tc.api)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want one with %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v, want %q", tc.name, got, err, tc.want)
		}
	}
}

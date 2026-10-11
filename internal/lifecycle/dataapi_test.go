package lifecycle

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// dataAPIPlane is a fakePlane that can run the Data API statements, and records them.
type dataAPIPlane struct {
	*fakePlane
	dmu  sync.Mutex
	runs []string
	err  error
	// reconfigured are the ReconfigureService calls ("ref service"); reconfErr is what they return.
	reconfigured []string
	reconfErr    error
}

func (d *dataAPIPlane) ApplyDataAPI(_ context.Context, p *registry.Project, s DataAPISetup) error {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	run := p.Ref + ": " + strings.Join(s.Statements(), "; ")
	if len(s.MustExist) > 0 {
		run += " [must exist: " + strings.Join(s.MustExist, ",") + "]"
	}
	d.runs = append(d.runs, run)
	return d.err
}

// ConfigPlane, so that the Engine can restart PostgREST on the exposed schemas.
func (d *dataAPIPlane) ReconfigureService(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys, svc string) error {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	d.reconfigured = append(d.reconfigured, p.Ref+" "+svc)
	return d.reconfErr
}
func (d *dataAPIPlane) ApplyPostgresSettings(context.Context, *registry.Project, *secrets.ProjectKeys, bool, func(context.Context)) (bool, error) {
	return false, nil
}
func (d *dataAPIPlane) SetRolePassword(context.Context, *registry.Project, string, string) error {
	return nil
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

func TestRevokeSQLRevokesTablesSequencesAndFunctions(t *testing.T) {
	want := []string{
		"alter default privileges for role postgres in schema public revoke select, insert, update, delete on tables from anon, authenticated, service_role",
		"alter default privileges for role postgres in schema public revoke usage, select on sequences from anon, authenticated, service_role",
		"alter default privileges for role postgres in schema public revoke execute on functions from anon, authenticated, service_role",
		"alter default privileges for role postgres in schema public revoke execute on functions from public",
	}
	if got := (DataAPISetup{RevokeDefaultPrivileges: true}).Statements(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements:\n%s", strings.Join(got, "\n"))
	}
	if got := (DataAPISetup{}).Statements(); len(got) != 0 {
		t.Fatalf("an empty setup runs %v", got)
	}
	both := (DataAPISetup{RevokeDefaultPrivileges: true, APISchema: true}).Statements()
	if len(both) != 6 || !strings.HasPrefix(both[4], "create schema if not exists api") {
		t.Fatalf("with the api schema: %v", both)
	}
	// Every statement of the revoke has its undo, in the same order.
	if len(GrantDefaultPrivilegesSQL) != len(RevokeDefaultPrivilegesSQL) {
		t.Fatalf("%d revokes, %d grants", len(RevokeDefaultPrivilegesSQL), len(GrantDefaultPrivilegesSQL))
	}
	for i, r := range RevokeDefaultPrivilegesSQL {
		g := GrantDefaultPrivilegesSQL[i]
		if strings.Replace(strings.Replace(r, " revoke ", " grant ", 1), " from ", " to ", 1) != g {
			t.Errorf("statement %d: %q is not the undo of %q", i, g, r)
		}
	}
}

// The guide shows the rollback and the explicit grants: its SQL is the code's.
func TestGuideShowsTheRollbackSQL(t *testing.T) {
	b, err := os.ReadFile("../../docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := strings.Join(strings.Fields(string(b)), " ")
	for _, q := range append(append([]string{}, GrantDefaultPrivilegesSQL...), RevokeDefaultPrivilegesSQL...) {
		if !strings.Contains(guide, q+";") {
			t.Errorf("docs/guide.md does not show %q", q+";")
		}
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
	if len(got) != 1 || !strings.Contains(got[0], "revoke select, insert, update, delete on tables") || !strings.Contains(got[0], "create schema if not exists api authorization postgres") ||
		!strings.HasSuffix(got[0], "[must exist: api,graphql_public]") {
		t.Fatalf("the plane ran %v", got)
	}
	st, err := h.set.Get(ctx, p.Ref, projectconfig.PostgREST)
	if err != nil || st.Effective.Str("db_schema") != "api,graphql_public" {
		t.Fatalf("db_schema = %q, %v", st.Effective.Str("db_schema"), err)
	}
	// PostgREST started on the default schemas, because the schema api did not exist yet: it restarts on the list once it does.
	if len(h.dp.reconfigured) != 1 || h.dp.reconfigured[0] != p.Ref+" postgrest" {
		t.Fatalf("reconfigured = %v", h.dp.reconfigured)
	}
	// Without exposed schemas nothing is saved and nothing restarts.
	q, err := h.e.Create(ctx, CreateRequest{Name: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := h.set.Get(ctx, q.Ref, projectconfig.PostgREST); st.Version != 0 || len(h.dp.reconfigured) != 1 {
		t.Fatalf("a default create saved PostgREST settings or restarted it: %+v %v", st, h.dp.reconfigured)
	}
}

// settingsProbePlane records the GoTrue variable the saved settings render at the moment the data plane
// creates the project, which is when its units are rendered for the first time.
type settingsProbePlane struct {
	*dataAPIPlane
	set    *projectconfig.Manager
	otpExp map[string]string
}

func (p *settingsProbePlane) Create(ctx context.Context, pr *registry.Project, k *secrets.ProjectKeys, seed DataSeeder) error {
	env, err := p.set.AuthEnv(ctx, pr.Ref, "")
	if err != nil {
		return err
	}
	p.otpExp[pr.Ref] = env["GOTRUE_MAILER_OTP_EXP"]
	return p.dataAPIPlane.Create(ctx, pr, k, seed)
}

// A new project starts with an email OTP that lives an hour, as a new hosted project does, and GoTrue is
// first rendered with it. A project with no saved value, one that exists from before, keeps the day its
// GoTrue runs on, through a pause and a resume of the engine that saves the value for new projects.
func TestCreateSavesAnHourOfOTPExpiryAndAnExistingProjectKeepsItsDay(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	probe := &settingsProbePlane{dataAPIPlane: h.dp, set: h.set, otpExp: map[string]string{}}
	e := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, probe, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup, Settings: h.set})

	p, err := e.Create(ctx, CreateRequest{Name: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if got := probe.otpExp[p.Ref]; got != "3600" {
		t.Fatalf("GoTrue was first rendered with GOTRUE_MAILER_OTP_EXP=%q, want 3600", got)
	}
	st, err := h.set.Get(ctx, p.Ref, projectconfig.Auth)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Effective.Int("mailer_otp_exp"); got != 3600 || len(st.Set) != 1 {
		t.Fatalf("a new project has mailer_otp_exp %d and saved %v, want 3600 and nothing else", got, st.Set)
	}

	// A project created before: no saved value, whatever the engine does with it afterwards.
	old, err := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup}).Create(ctx, CreateRequest{Name: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(ctx, old.Ref); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(ctx, old.Ref); err != nil {
		t.Fatal(err)
	}
	st, err = h.set.Get(ctx, old.Ref, projectconfig.Auth)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Effective.Int("mailer_otp_exp"); got != 86400 || st.Version != 0 {
		t.Fatalf("an existing project has mailer_otp_exp %d at version %d, want 86400 at version 0", got, st.Version)
	}
	if env, _ := h.set.AuthEnv(ctx, old.Ref, ""); env["GOTRUE_MAILER_OTP_EXP"] != "" {
		t.Fatalf("an existing project renders GOTRUE_MAILER_OTP_EXP=%q", env["GOTRUE_MAILER_OTP_EXP"])
	}

	// A create without a settings store (or one that cannot save) has nothing to save and goes on.
	bare := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup})
	if _, err := bare.Create(ctx, CreateRequest{Name: "bare"}); err != nil {
		t.Fatalf("create without a settings store: %v", err)
	}
}

// A schema the caller asked to expose has to exist, or PostgREST would not start on it.
func TestCreateFailsWhenAnExposedSchemaDoesNotExist(t *testing.T) {
	h := newDataAPIHarness(t)
	h.dp.err = errors.New(`schema "app" does not exist`)
	_, err := h.e.Create(context.Background(), CreateRequest{Name: "x", DataAPI: DataAPIOptions{ExposeNewTables: true, ExposedSchemas: "app"}})
	if err == nil || !strings.Contains(err.Error(), `schema "app" does not exist`) {
		t.Fatalf("create: %v", err)
	}
	if got := h.dp.ran(); len(got) != 1 || !strings.HasSuffix(got[0], "[must exist: app]") {
		t.Fatalf("the check was not asked for with no statements to run: %v", got)
	}
	if len(h.dp.reconfigured) != 0 {
		t.Fatalf("PostgREST was restarted: %v", h.dp.reconfigured)
	}
}

func TestCreateWithoutASettingsStoreRefusesExposedSchemas(t *testing.T) {
	h := newDataAPIHarness(t)
	e := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.dp, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup}) // Settings is nil
	_, err := e.Create(context.Background(), CreateRequest{Name: "x", DataAPI: DataAPIOptions{ExposedSchemas: "api"}})
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
	// A source that is gone but whose backup remembered the decision still passes it on.
	remembered, err := h.e.Create(ctx, CreateRequest{Name: "remembered", Seed: seed, Keys: keys,
		DataAPI: DataAPIOptions{CopyFrom: "yyyyyyyyyyyyyyyyyyyy", CopyChoice: registry.PrivilegesGranted}})
	if err != nil {
		t.Fatal(err)
	}
	if d := choiceOf(t, h.reg, remembered.Ref); d.DefaultPrivileges != registry.PrivilegesGranted || d.Source != registry.DataAPICopy {
		t.Fatalf("choice = %+v", d)
	}
	// A source with nothing recorded (a project from before, or a deleted one) leaves the copy to the next pass.
	other, err := h.e.Create(ctx, CreateRequest{Name: "other", Seed: seed, Keys: keys, DataAPI: DataAPIOptions{CopyFrom: "zzzzzzzzzzzzzzzzzzzz"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DataAPIOf(h.reg).GetDataAPI(ctx, other.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("a copy of nothing recorded a decision: %v", err)
	}
	// A restore as new of a backup that recorded nothing names no source (the source's row may have
	// been written since): the copy is unrecorded and its first pass revokes the old grants.
	restored, err := h.e.Create(ctx, CreateRequest{Name: "restored", Seed: seed, Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DataAPIOf(h.reg).GetDataAPI(ctx, restored.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("a seeded create with no decision to copy recorded one: %v", err)
	}
	if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	if d := choiceOf(t, h.reg, restored.Ref); d.DefaultPrivileges != registry.PrivilegesRevoked || d.Source != registry.DataAPIUpgrade {
		t.Fatalf("choice after the first pass = %+v", d)
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
	h.dp.err = errors.New("connection refused")
	failed := h.e.ConvergeDataAPI(ctx)
	if failed[p.Ref] == nil || !strings.Contains(failed[p.Ref].Error(), "connection refused") {
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

func (h *dataAPIEnv) countEvents(t *testing.T, ref, kind string) int {
	t.Helper()
	evs, err := h.reg.ListEvents(context.Background(), ref, 200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// A project that keeps failing the same way adds one failure event, not one per pass; a different
// failure adds another, and so does the same one an hour later. A cluster in recovery and a node
// behind on its release are waited for, with no event, and succeed when they catch up.
func TestConvergeFailureEventsAreNotRepeated(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	h.e.opts.Now = func() time.Time { return now }
	p := h.existing(t, "down")

	h.dp.err = errors.New("connection refused")
	for i := 0; i < 5; i++ {
		if failed := h.e.ConvergeDataAPI(ctx); failed[p.Ref] == nil {
			t.Fatalf("pass %d did not fail", i)
		}
	}
	if n := h.countEvents(t, p.Ref, EventDataAPIFailed); n != 1 {
		t.Fatalf("%d failure events after five identical failures, want 1", n)
	}
	h.dp.err = errors.New("the database is shutting down")
	h.e.ConvergeDataAPI(ctx)
	if n := h.countEvents(t, p.Ref, EventDataAPIFailed); n != 2 {
		t.Fatalf("%d failure events after a different failure, want 2", n)
	}
	now = now.Add(dataAPIFailureEvery)
	h.e.ConvergeDataAPI(ctx)
	if n := h.countEvents(t, p.Ref, EventDataAPIFailed); n != 3 {
		t.Fatalf("%d failure events an hour later, want 3", n)
	}

	for _, waiting := range []error{ErrNotPrimary, ErrPeerBehind} {
		h.dp.err = waiting
		failed := h.e.ConvergeDataAPI(ctx)
		if !errors.Is(failed[p.Ref], waiting) {
			t.Fatalf("a project waiting on %v: failed = %v", waiting, failed)
		}
	}
	if n := h.countEvents(t, p.Ref, EventDataAPIFailed); n != 3 {
		t.Fatalf("a wait wrote an event: %d failure events", n)
	}
	h.dp.err = nil
	if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	if d := choiceOf(t, h.reg, p.Ref); d.DefaultPrivileges != registry.PrivilegesRevoked {
		t.Fatalf("choice = %+v", d)
	}
}

// A project whose lock another operation holds is skipped after the lock wait, and the pass goes on
// to the others. A project that has its row is not even looked at under its lock.
func TestConvergeDoesNotWaitOnABusyProject(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	defer func(w time.Duration) { convergeLockWait = w }(convergeLockWait)
	convergeLockWait = 50 * time.Millisecond
	busy := h.existing(t, "busy")
	free := h.existing(t, "other")
	done, err := h.e.Create(ctx, CreateRequest{Name: "done"}) // has its row
	if err != nil {
		t.Fatal(err)
	}
	unlockBusy, err := h.e.lock(ctx, busy.Ref)
	if err != nil {
		t.Fatal(err)
	}
	unlockDone, err := h.e.lock(ctx, done.Ref)
	if err != nil {
		t.Fatal(err)
	}
	h.dp.dmu.Lock()
	h.dp.runs = nil // forget the create's own run
	h.dp.dmu.Unlock()
	start := time.Now()
	failed := h.e.ConvergeDataAPI(ctx)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the pass took %v with a project locked", took)
	}
	if failed[busy.Ref] == nil || !errors.Is(failed[busy.Ref], context.DeadlineExceeded) || len(failed) != 1 {
		t.Fatalf("failed = %v, want only the locked project %s", failed, busy.Ref)
	}
	if got := h.dp.ran(); len(got) != 1 || !strings.HasPrefix(got[0], free.Ref+": ") {
		t.Fatalf("the plane ran %v, want only %s", got, free.Ref)
	}
	if h.countEvents(t, busy.Ref, EventDataAPIFailed) != 0 {
		t.Fatal("a busy project recorded a failure event")
	}
	unlockBusy()
	unlockDone()
	if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 {
		t.Fatalf("after the lock was released: %v", failed)
	}
	if d := choiceOf(t, h.reg, busy.Ref); d.DefaultPrivileges != registry.PrivilegesRevoked {
		t.Fatalf("choice = %+v", d)
	}
}

// At create, a home node on an older release is waited for when the revoke is all that was asked,
// and a failure when the caller asked for a schema only the node can make.
func TestCreateOnANodeBehindWaitsForTheRevokeOnly(t *testing.T) {
	h := newDataAPIHarness(t)
	ctx := context.Background()
	h.dp.err = ErrPeerBehind
	p, err := h.e.Create(ctx, CreateRequest{Name: "behind"})
	if err != nil {
		t.Fatalf("the default create on a node behind: %v", err)
	}
	if _, err := registry.DataAPIOf(h.reg).GetDataAPI(ctx, p.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("a revoke that did not run was recorded: %v", err)
	}
	if _, err := h.e.Create(ctx, CreateRequest{Name: "api", DataAPI: DataAPIOptions{APISchema: true, ExposedSchemas: "api,graphql_public"}}); !errors.Is(err, ErrPeerBehind) {
		t.Fatalf("a create that needs the schema: %v", err)
	}
	h.dp.err = nil
	if failed := h.e.ConvergeDataAPI(ctx); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	if d := choiceOf(t, h.reg, p.Ref); d.DefaultPrivileges != registry.PrivilegesRevoked || d.Source != registry.DataAPIUpgrade {
		t.Fatalf("choice = %+v", d)
	}
}

package branching

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// schemaTrace makes the harness's trace tell the engine's creates, the migrations the branch replays and
// the calls that expose schemas, in the order they happen.
func schemaTrace(c *contentHarness) {
	c.eng.schemaTrace = c.sdb.note
	c.sdb.note("") // an empty trace is not nil
}

// index returns the position of the first trace line with the prefix, or -1.
func (d *fakeSchemaDB) index(prefix string) int { return d.indexAfter(prefix, 0) }

// indexAfter is index for the lines from position from on.
func (d *fakeSchemaDB) indexAfter(prefix string, from int) int {
	d.tm.Lock()
	defer d.tm.Unlock()
	for i := max(from, 0); i < len(d.trace); i++ {
		if strings.HasPrefix(d.trace[i], prefix) {
			return i
		}
	}
	return -1
}

func eventPayload(t *testing.T, reg registry.Registry, ref, kind string) map[string]any {
	t.Helper()
	evs, err := reg.ListEvents(context.Background(), ref, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == kind {
			var out map[string]any
			if err := json.Unmarshal(e.Payload, &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("no %s event on %s", kind, ref)
	return nil
}

// A branch whose database starts empty first starts PostgREST on the default schemas: the parent's list
// is applied after the migrations have made the schemas, and the names the branch has no schema for are
// left out, said in the run's log and in the event.
func TestASchemaOnlyBranchExposesTheParentsSchemasAfterTheyAreBuilt(t *testing.T) {
	c := newContentHarness(t, nil)
	schemaTrace(c)
	c.eng.exposed = map[string]string{parentRef: "public,graphql_public,api,gone"}
	c.eng.missingSchemas = map[string]bool{"gone": true}

	b := c.create("one", nil)
	c.mustState(b, registry.BranchFunctionsDeployed)

	if got := c.eng.savedAtCreate[b.Ref]; got != "" {
		t.Fatalf("the branch had db_schema %q saved when its units first started", got)
	}
	if got := c.eng.exposedOf(b.Ref); got != "public,graphql_public,api" {
		t.Fatalf("the branch's db_schema = %q, want the parent's list less the schema it lacks", got)
	}
	create, apply, expose := c.sdb.index("engine.create "+b.Ref), c.sdb.index("apply "+b.Ref), c.sdb.index("expose-schemas "+b.Ref)
	if create < 0 || apply < create || expose < apply {
		t.Fatalf("order: create %d, migrations %d, expose %d; trace %q", create, apply, expose, c.sdb.trace)
	}
	if c.eng.exposeCallCount != 1 {
		t.Fatalf("expose was asked %d times", c.eng.exposeCallCount)
	}
	log := c.runLog(b.Ref)
	if !strings.Contains(log, "PostgREST starts on the default schemas") || !strings.Contains(log, "not exposed, because the branch has no such schema: gone") {
		t.Fatalf("run log:\n%s", log)
	}
	ev := eventPayload(t, c.reg, b.Ref, "branch.created")
	if !reflect.DeepEqual(ev["exposed_schemas"], []any{"public", "graphql_public", "api"}) || !reflect.DeepEqual(ev["unexposed_schemas"], []any{"gone"}) {
		t.Fatalf("branch.created = %v", ev)
	}
	// The parent is not touched.
	if got := c.eng.exposedOf(parentRef); got != "public,graphql_public,api,gone" {
		t.Fatalf("the parent's db_schema = %q", got)
	}
}

// A parent on the default schemas has nothing to hold, and a failure to expose fails the branch.
func TestABranchOfAParentOnTheDefaultSchemasExposesNothing(t *testing.T) {
	c := newContentHarness(t, nil)
	schemaTrace(c)
	b := c.create("one", nil)
	c.mustState(b, registry.BranchFunctionsDeployed)
	if c.eng.exposeCallCount != 0 || c.sdb.index("set-schemas") >= 0 {
		t.Fatalf("a parent with no list: expose called %d times, trace %q", c.eng.exposeCallCount, c.sdb.trace)
	}
}

func TestABranchFailsWhenPostgRESTCannotBeRestartedOnTheParentsSchemas(t *testing.T) {
	c := newContentHarness(t, nil)
	c.eng.exposed = map[string]string{parentRef: "public,api"}
	c.eng.exposeErr = errors.New("postgrest did not answer")
	b := c.create("one", nil)
	if b.State != registry.BranchMigrationsFailed || !strings.Contains(b.Detail, "postgrest did not answer") {
		t.Fatalf("branch = %s %s", b.State, b.Detail)
	}
	// The list is kept for a reset, which exposes it once it can.
	if got := c.eng.exposedOf(b.Ref); got != "public,api" {
		t.Fatalf("the failed branch's db_schema = %q, want the parent's list kept for a reset", got)
	}
}

// A branch made from the parent's data has the schemas already: the engine copies the list into its
// settings and the service holds nothing.
func TestABranchWithDataStartsOnTheParentsSchemasAndHoldsNothing(t *testing.T) {
	c := newContentHarness(t, nil)
	schemaTrace(c)
	c.eng.exposed = map[string]string{parentRef: "public,api"}
	c.svc.detect = func(string, string) (string, string, string) { return MethodClonefile, "apfs", "" }
	c.svc.clone = func(_ context.Context, _, m, _ string) (*CloneStats, error) { return &CloneStats{Method: m}, nil }
	b := c.create("data", func(in *CreateInput) { in.WithData = true })
	c.mustState(b, registry.BranchFunctionsDeployed)
	if got := c.eng.savedAtCreate[b.Ref]; got != "public,api" {
		t.Fatalf("a branch with data first started on db_schema %q, want the parent's", got)
	}
	if c.eng.exposeCallCount != 0 {
		t.Fatalf("a branch with data asked to expose schemas %d times", c.eng.exposeCallCount)
	}
}

// A repository's own list, saved while the branch was built, is the branch's and the parent's does not replace it.
type listSettingSource struct {
	c    *contentHarness
	list string
}

func (s listSettingSource) Schema(_ context.Context, _ *Run, ref string) (string, error) {
	_ = s.c.eng.SetExposedSchemas(context.Background(), ref, s.list)
	return "built from the repository", nil
}
func (listSettingSource) Functions(context.Context, *Run, string) error { return nil }

func TestARepositorysOwnSchemaListWinsOverTheParents(t *testing.T) {
	c := newContentHarness(t, nil)
	c.eng.exposed = map[string]string{parentRef: "public,graphql_public,api"}
	b := c.create("pr", func(in *CreateInput) { in.SchemaSource = listSettingSource{c: c, list: "public,graphql_public,app"} })
	c.mustState(b, registry.BranchFunctionsDeployed)
	if got := c.eng.exposedOf(b.Ref); got != "public,graphql_public,app" || c.eng.exposeCallCount != 0 {
		t.Fatalf("the branch's db_schema = %q (expose called %d times), want the repository's list", got, c.eng.exposeCallCount)
	}
	if !strings.Contains(c.runLog(b.Ref), "the parent's public,graphql_public,api is not applied") {
		t.Fatalf("run log:\n%s", c.runLog(b.Ref))
	}
	// A repository that sets no list gets the parent's once its schema is built.
	b2 := c.create("pr2", func(in *CreateInput) { in.SchemaSource = listSettingSource{c: c, list: ""} })
	c.mustState(b2, registry.BranchFunctionsDeployed)
	if got := c.eng.exposedOf(b2.Ref); got != "public,graphql_public,api" {
		t.Fatalf("the second branch's db_schema = %q", got)
	}
}

// A reset recreates the cluster empty. The branch's saved list is taken off its settings for the start,
// so PostgREST does not fail on schemas that the replay has not made, and put back after the replay.
func TestResetStartsOnTheDefaultSchemasAndExposesTheBranchsListAfterTheReplay(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	c.mustState(b, registry.BranchFunctionsDeployed)
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.harness.db.forget(ref) }
	schemaTrace(c)
	c.eng.mu.Lock()
	c.eng.exposed = map[string]string{b.Ref: "public,graphql_public,api"}
	c.eng.mu.Unlock()

	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)

	if got := c.eng.savedAtCreate[b.Ref]; got != "" {
		t.Fatalf("the recreated cluster first started with db_schema %q saved", got)
	}
	take := c.sdb.index("set-schemas " + b.Ref + ` ""`)
	create := c.sdb.indexAfter("engine.create "+b.Ref, take)
	apply := c.sdb.indexAfter("apply "+b.Ref, create)
	expose := c.sdb.indexAfter("expose-schemas "+b.Ref, apply)
	if take < 0 || create < 0 || apply < 0 || expose < 0 {
		t.Fatalf("order: taken off %d, create %d, replay %d, expose %d; trace %q", take, create, apply, expose, c.sdb.trace)
	}
	if got := c.eng.exposedOf(b.Ref); got != "public,graphql_public,api" {
		t.Fatalf("the branch's db_schema after the reset = %q", got)
	}
}

// A reset that fails after the list was taken off puts it back, so that nothing the branch exposed is lost.
func TestAFailedResetPutsTheBranchsExposedSchemasBack(t *testing.T) {
	c := newContentHarness(t, nil)
	b := c.create("one", nil)
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.harness.db.forget(ref) }
	c.eng.mu.Lock()
	c.eng.exposed = map[string]string{b.Ref: "public,graphql_public,api"}
	c.eng.mu.Unlock()
	c.db.mu.Lock()
	c.db.applyErr[b.Ref] = errors.New("migration failed")
	c.db.mu.Unlock()

	if _, err := c.svc.Reset(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := c.wait(b.Ref)
	if got.State != registry.BranchMigrationsFailed {
		t.Fatalf("state = %s (%s)", got.State, got.Detail)
	}
	if list := c.eng.exposedOf(b.Ref); list != "public,graphql_public,api" {
		t.Fatalf("the failed reset left db_schema %q", list)
	}
	if c.eng.exposeCallCount != 0 {
		t.Fatalf("PostgREST was restarted on a build that failed")
	}
}

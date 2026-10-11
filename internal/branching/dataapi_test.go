package branching

import (
	"context"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

func setParentChoice(t *testing.T, h *harness, d registry.DefaultPrivileges) {
	t.Helper()
	if err := registry.DataAPIOf(h.reg).PutDataAPI(context.Background(), registry.DataAPI{Ref: parentRef, DefaultPrivileges: d, Source: registry.DataAPICreate}); err != nil {
		t.Fatal(err)
	}
}

// A schema-only branch is a new cluster: it gets the default privileges its parent was given, before the
// migrations replay, so that the migrations behave as they do on the parent.
func TestSchemaOnlyBranchFollowsTheParentsDefaultPrivileges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice registry.DefaultPrivileges // "" records nothing
		expose bool
	}{
		{"revoked", registry.PrivilegesRevoked, false},
		{"granted", registry.PrivilegesGranted, true},
		{"a parent no pass has looked at is on its way to hosted's default", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			if tc.choice != "" {
				setParentChoice(t, h, tc.choice)
			}
			b := h.create("feature", nil)
			h.mustState(b, registry.BranchMigrationsPassed)
			got := h.eng.creates[0].DataAPI
			if got.ExposeNewTables != tc.expose || got.CopyFrom != parentRef {
				t.Fatalf("the branch was created with %+v, want ExposeNewTables=%v CopyFrom=%s", got, tc.expose, parentRef)
			}
			if got.APISchema || got.ExposedSchemas != "" {
				t.Fatalf("a branch does not inherit the parent's exposed schemas: %+v", got)
			}
		})
	}
}

// A branch made from the parent's data has the parent's database, default privileges included, so
// it takes the parent's row and runs no SQL (the seeded create of the Engine does that).
func TestDataBranchTakesTheParentsDecision(t *testing.T) {
	h := newHarness(t, nil)
	setParentChoice(t, h, registry.PrivilegesGranted)
	h.svc.detect = func(string, string) (string, string, string) { return MethodClonefile, "apfs", "" }
	h.svc.clone = func(_ context.Context, parent, method, dst string) (*CloneStats, error) {
		return &CloneStats{Method: method, FS: "apfs"}, nil
	}
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	h.mustState(b, registry.BranchMigrationsPassed)
	req := h.eng.creates[0]
	if req.Seed == nil || req.DataAPI.CopyFrom != parentRef {
		t.Fatalf("a clone must name the parent as the source of the decision: %+v", req.DataAPI)
	}
}

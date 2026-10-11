package registry

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMemoryAdvisorRules(t *testing.T) { testAdvisorRules(t, NewMemory()) }

// TestPostgresAdvisorRules runs the same checks against a real database (see TestPostgres).
func TestPostgresAdvisorRules(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Pool().Exec(ctx, `truncate supavise.projects, supavise.organizations cascade`); err != nil {
		t.Fatal(err)
	}
	testAdvisorRules(t, r)
}

// testAdvisorRules is the shared check of the AdvisorStore of a registry.
func testAdvisorRules(t *testing.T, r Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, ok := r.(AdvisorStore)
	if !ok {
		t.Fatal("registry has no AdvisorStore")
	}
	org, err := r.CreateOrganization(ctx, "adv-org", "Advisors")
	if err != nil {
		t.Fatal(err)
	}
	const a, b = "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"
	for _, ref := range []string{a, b} {
		if err := r.CreateProject(ctx, &Project{Ref: ref, OrgID: org.ID, Name: ref}); err != nil {
			t.Fatal(err)
		}
	}
	const user = "11111111-2222-4333-8444-555555555555"

	if l, err := st.ListAdvisorExceptions(ctx, a); err != nil || l == nil || len(l) != 0 {
		t.Fatalf("a new project holds no rule: %v %v", l, err)
	}
	if _, err := st.CreateAdvisorExceptions(ctx, "cccccccccccccccccccc", []AdvisorException{{LintName: "x", IsDisabled: true}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("rule for an unknown project: %v", err)
	}
	made, err := st.CreateAdvisorExceptions(ctx, a, []AdvisorException{
		{LintName: "rls_disabled_in_public", IsDisabled: true, LastUpdatedBy: user},
		{LintCategory: "SECURITY", LintMetadata: map[string]any{"schema": "public"}, AssignedTo: user, IsDisabled: false, Note: "mine", LastUpdatedBy: user},
	})
	if err != nil || len(made) != 2 {
		t.Fatalf("create: %v %v", made, err)
	}
	for _, e := range made {
		if len(e.ID) != 36 || e.Ref != a || e.InsertedAt.IsZero() || e.LastUpdatedBy != user {
			t.Errorf("created rule %+v", e)
		}
	}
	if made[0].ID == made[1].ID {
		t.Error("two rules share an id")
	}
	// Another project does not see them.
	if l, _ := st.ListAdvisorExceptions(ctx, b); len(l) != 0 {
		t.Errorf("project b sees %v", l)
	}
	list, err := st.ListAdvisorExceptions(ctx, a)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	if list[0].ID != made[0].ID || list[0].LintName != "rls_disabled_in_public" || !list[0].IsDisabled || list[0].AssignedTo != "" || list[0].LintMetadata != nil {
		t.Errorf("first rule: %+v", list[0])
	}
	if e := list[1]; e.LintCategory != "SECURITY" || e.LintName != "" || e.LintMetadata["schema"] != "public" || e.AssignedTo != user || e.IsDisabled || e.Note != "mine" {
		t.Errorf("second rule: %+v", e)
	}

	// Update changes the fields that are given, and only those.
	yes, off, note, name := true, false, "later", "unused_index"
	if err := st.UpdateAdvisorException(ctx, a, made[1].ID, AdvisorExceptionPatch{IsDisabled: &yes, Note: &note, LintName: &name}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAdvisorException(ctx, a, made[0].ID, AdvisorExceptionPatch{IsDisabled: &off, LastUpdatedBy: "99999999-2222-4333-8444-555555555555"}); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListAdvisorExceptions(ctx, a)
	if e := list[1]; !e.IsDisabled || e.Note != "later" || e.LintName != "unused_index" || e.LintCategory != "SECURITY" || e.AssignedTo != user {
		t.Errorf("updated rule: %+v", e)
	}
	if e := list[0]; e.IsDisabled || e.LintName != "rls_disabled_in_public" || e.LastUpdatedBy != "99999999-2222-4333-8444-555555555555" {
		t.Errorf("updated rule: %+v", e)
	}
	empty := ""
	if err := st.UpdateAdvisorException(ctx, a, made[1].ID, AdvisorExceptionPatch{AssignedTo: &empty, LintMetadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if list, _ = st.ListAdvisorExceptions(ctx, a); list[1].AssignedTo != "" || len(list[1].LintMetadata) != 0 {
		t.Errorf("clearing: %+v", list[1])
	}
	if err := st.UpdateAdvisorException(ctx, b, made[0].ID, AdvisorExceptionPatch{IsDisabled: &yes}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update through another project: %v", err)
	}
	if err := st.UpdateAdvisorException(ctx, a, "aaaaaaaa-2222-4333-8444-555555555555", AdvisorExceptionPatch{IsDisabled: &yes}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of a missing rule: %v", err)
	}

	// Delete counts what existed and keeps the others' rules.
	if n, err := st.DeleteAdvisorExceptions(ctx, b, []string{made[0].ID}); err != nil || n != 0 {
		t.Errorf("delete through another project: %d %v", n, err)
	}
	if n, err := st.DeleteAdvisorExceptions(ctx, a, []string{made[0].ID, "aaaaaaaa-2222-4333-8444-555555555555"}); err != nil || n != 1 {
		t.Errorf("delete: %d %v", n, err)
	}
	if n, err := st.DeleteAdvisorExceptions(ctx, a, nil); err != nil || n != 0 {
		t.Errorf("delete of nothing: %d %v", n, err)
	}
	if list, _ = st.ListAdvisorExceptions(ctx, a); len(list) != 1 || list[0].ID != made[1].ID {
		t.Errorf("after delete: %+v", list)
	}

	// The cap holds, in one call and across calls.
	many := make([]AdvisorException, MaxAdvisorExceptions)
	for i := range many {
		many[i] = AdvisorException{LintName: "unused_index", IsDisabled: true}
	}
	if _, err := st.CreateAdvisorExceptions(ctx, a, many); !errors.Is(err, ErrConflict) {
		t.Errorf("past the cap in one call: %v", err)
	}
	if _, err := st.CreateAdvisorExceptions(ctx, a, many[:MaxAdvisorExceptions-1]); err != nil {
		t.Errorf("up to the cap: %v", err)
	}
	if _, err := st.CreateAdvisorExceptions(ctx, a, many[:1]); !errors.Is(err, ErrConflict) {
		t.Errorf("past the cap: %v", err)
	}

	// A project's rules go with it.
	if err := r.DeleteProject(ctx, a); err != nil {
		t.Fatal(err)
	}
	if l, err := st.ListAdvisorExceptions(ctx, a); err != nil || len(l) != 0 {
		t.Errorf("rules of a deleted project: %v %v", l, err)
	}
}

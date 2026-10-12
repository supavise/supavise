package registry

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMemoryLifecycleStore(t *testing.T) { testLifecycleStore(t, NewMemory()) }

// TestPostgresLifecycleStore runs the same checks against a real database (see TestPostgres).
func TestPostgresLifecycleStore(t *testing.T) {
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
	if _, err := r.Pool().Exec(ctx, `truncate supavise.project_pause, supavise.branch_lifecycle, supavise.projects, supavise.organizations cascade`); err != nil {
		t.Fatal(err)
	}
	testLifecycleStore(t, r)
}

// testLifecycleStore is the shared check of the LifecycleStore of a registry.
func testLifecycleStore(t *testing.T, reg Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := BranchLifecycleOf(reg)
	if st == nil {
		t.Fatal("registry has no LifecycleStore")
	}
	org, err := reg.CreateOrganization(ctx, "pause-org", "Pause")
	if err != nil {
		t.Fatal(err)
	}
	const a, b = "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"
	for _, ref := range []string{a, b} {
		if err := reg.CreateProject(ctx, &Project{Ref: ref, OrgID: org.ID, Name: ref, Engine: EnginePostgres, Status: StatusActiveHealthy}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := st.GetPause(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause before any: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.SetPause(ctx, a, PauseIdle, at); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPause(ctx, b, "", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPause(ctx, a)
	if err != nil || got.Reason != PauseIdle || !got.PausedAt.Equal(at) {
		t.Fatalf("pause = %+v, %v", got, err)
	}
	// A new pause replaces the old reason: the owner pauses a branch the node had paused.
	if err := st.SetPause(ctx, a, "", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetPause(ctx, a); got.Reason != "" || !got.PausedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("replaced pause = %+v", got)
	}
	if err := st.SetPause(ctx, "cccccccccccccccccccc", PauseIdle, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause of an unknown project: %v", err)
	}
	all, err := st.ListPauses(ctx)
	if err != nil || len(all) != 2 || all[0].Ref != a || all[1].Ref != b {
		t.Fatalf("pauses = %+v, %v", all, err)
	}
	if err := st.ClearPause(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearPause(ctx, a); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
	if _, err := st.GetPause(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause after clear: %v", err)
	}

	// The expiry warning is remembered for one expiry.
	if _, err := st.GetExpiryWarning(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("warning before any: %v", err)
	}
	exp := at.Add(24 * time.Hour)
	if err := st.SetExpiryWarning(ctx, a, exp, at); err != nil {
		t.Fatal(err)
	}
	w, err := st.GetExpiryWarning(ctx, a)
	if err != nil || !w.For.Equal(exp) || !w.At.Equal(at) {
		t.Fatalf("warning = %+v, %v", w, err)
	}
	if err := st.SetExpiryWarning(ctx, a, exp.Add(time.Hour), at); err != nil {
		t.Fatal(err)
	}
	if w, _ = st.GetExpiryWarning(ctx, a); !w.For.Equal(exp.Add(time.Hour)) {
		t.Fatalf("moved expiry warning = %+v", w)
	}
	if err := st.SetExpiryWarning(ctx, "cccccccccccccccccccc", exp, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("warning of an unknown project: %v", err)
	}

	// Both go with the project.
	if err := st.SetPause(ctx, b, PauseIdle, at); err != nil {
		t.Fatal(err)
	}
	if err := reg.DeleteProject(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPause(ctx, b); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause of a deleted project: %v", err)
	}
}

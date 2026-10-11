package registry

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

func TestMemoryBranchContent(t *testing.T) { testBranchContent(t, NewMemory()) }

// TestPostgresBranchContent runs the same checks against a real database (see TestPostgres).
func TestPostgresBranchContent(t *testing.T) {
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
	// Other tests share this database: this one makes its own organization and project and removes them.
	testBranchContent(t, r)
}

// testBranchContent is the shared check of the BranchContentStore of a registry.
func testBranchContent(t *testing.T, reg Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := BranchContentOf(reg)
	if st == nil {
		t.Fatal("registry has no BranchContentStore")
	}
	slug := "content-" + strings.ToLower(secrets.RandomString(8, "abcdefghijklmnopqrstuvwxyz"))
	org, err := reg.CreateOrganization(ctx, slug, "Content")
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.ToLower(secrets.RandomString(20, "abcdefghijklmnopqrstuvwxyz"))
	if err := reg.CreateProject(ctx, &Project{Ref: ref, OrgID: org.ID, Name: "content", Class: "default", Status: StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.DeleteProject(context.Background(), ref) })
	if _, err := st.GetBranchSchemaBase(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a branch with no base: %v", err)
	}
	if err := st.PutBranchSchemaBase(ctx, BranchSchemaBase{BranchRef: "nobodyaaaaaaaaaaaaaa", Fingerprint: "x", Snapshot: []byte("{}")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a base for an unknown project: %v", err)
	}
	if err := st.PutBranchSchemaBase(ctx, BranchSchemaBase{BranchRef: ref, Engine: 1, Fingerprint: "f1", Snapshot: []byte("one"), Source: "create"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBranchSchemaBase(ctx, BranchSchemaBase{BranchRef: ref, Engine: 1, Fingerprint: "f2", Snapshot: []byte("two"), Source: "push"}); err != nil {
		t.Fatal(err)
	}
	b, err := st.GetBranchSchemaBase(ctx, ref)
	if err != nil || b.Fingerprint != "f2" || string(b.Snapshot) != "two" || b.Source != "push" || b.TakenAt.IsZero() {
		t.Fatalf("base = %+v, %v", b, err)
	}

	got, err := st.GetBranchFunctionBase(ctx, ref)
	if err != nil || len(got) != 0 {
		t.Fatalf("function base before any = %v, %v", got, err)
	}
	if err := st.PutBranchFunctionBase(ctx, ref, map[string]string{"a": "1", "b": "2"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBranchFunctionBase(ctx, ref, map[string]string{"b": "3", "c": "4"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetBranchFunctionBase(ctx, ref)
	if len(got) != 2 || got["b"] != "3" || got["c"] != "4" {
		t.Fatalf("function base = %v", got)
	}
	if err := st.PutBranchFunctionBase(ctx, "nobodyaaaaaaaaaaaaaa", map[string]string{"a": "1"}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("function base of an unknown project: %v", err)
	}
}

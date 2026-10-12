package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMissingColumn(t *testing.T) {
	if !missingColumn(&pgconn.PgError{Code: "42703"}) {
		t.Fatal("undefined_column is not a missing column")
	}
	if !missingColumn(fmt.Errorf("read: %w", &pgconn.PgError{Code: "42703"})) {
		t.Fatal("a wrapped undefined_column is not a missing column")
	}
	for _, err := range []error{nil, errors.New("boom"), &pgconn.PgError{Code: "42P01"}, ErrNotFound} {
		if missingColumn(err) {
			t.Fatalf("%v is not a missing column", err)
		}
	}
}

// TestWithOrgColsFallsBack checks the retry on its own: one more try, with the column list that does not
// name opt_in_tags, when (and only when) the first says the column is not there.
func TestWithOrgColsFallsBack(t *testing.T) {
	var tried []string
	got, err := withOrgCols(func(cols string) (string, error) {
		tried = append(tried, cols)
		if cols == orgCols {
			return "", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "42703", Message: `column "opt_in_tags" does not exist`})
		}
		return "ok", nil
	})
	if err != nil || got != "ok" || !slices.Equal(tried, []string{orgCols, orgColsBase}) {
		t.Fatalf("got %q, %v after %q", got, err, tried)
	}

	// A registry with the column is read once.
	tried = nil
	if _, err := withOrgCols(func(cols string) (int, error) { tried = append(tried, cols); return 1, nil }); err != nil || len(tried) != 1 {
		t.Fatalf("a read that worked ran %q: %v", tried, err)
	}

	// Any other error is the caller's, and the same error comes back after one try.
	for _, boom := range []error{ErrNotFound, errors.New("connection reset"), &pgconn.PgError{Code: "42P01"}} {
		tried = nil
		_, err := withOrgCols(func(cols string) (int, error) { tried = append(tried, cols); return 0, boom })
		if !errors.Is(err, boom) || len(tried) != 1 {
			t.Fatalf("%v: ran %q and returned %v", boom, tried, err)
		}
	}

	// An error of the retry is returned as it is.
	other := errors.New("down")
	_, err = withOrgCols(func(cols string) (int, error) {
		if cols == orgCols {
			return 0, &pgconn.PgError{Code: "42703"}
		}
		return 0, other
	})
	if !errors.Is(err, other) {
		t.Fatalf("error of the retry: %v", err)
	}

	// The two column lists differ only in the last column.
	if want := `id, slug, name, created_at, `; orgCols[:len(want)] != want || orgColsBase[:len(want)] != want {
		t.Fatalf("column lists diverge: %q and %q", orgCols, orgColsBase)
	}
}

// TestOrganizationsReadBeforeMigration1550 puts a registry where a release that predates
// organizations.opt_in_tags leaves it, and reads it with the handles a daemon opens before its migrations
// ran (OpenExisting, OpenReadOnly): the organizations read as having no tags, and a rename works. After
// 1550 the same handles see the column, and a rename leaves the saved tags alone.
func TestOrganizationsReadBeforeMigration1550(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db := tempDatabase(t, dsn, "supavise_org1550")
	pool, err := pgxpool.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate(ctx, pool, "1550_org_opt_in_tags.sql"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into supavise.organizations (slug, name) values ('acme', 'Acme'), ('beta', 'Beta')`); err != nil {
		t.Fatal(err)
	}

	checkRead := func(r *Postgres, wantTags []string) {
		t.Helper()
		got, err := r.GetOrganization(ctx, "acme")
		if err != nil || got.Name != "Acme" || !slices.Equal(got.OptInTags, wantTags) || got.OptInTags == nil {
			t.Fatalf("GetOrganization = %+v, %v", got, err)
		}
		byID, err := r.GetOrganizationByID(ctx, got.ID)
		if err != nil || byID.Slug != "acme" || !slices.Equal(byID.OptInTags, wantTags) {
			t.Fatalf("GetOrganizationByID = %+v, %v", byID, err)
		}
		all, err := r.ListOrganizations(ctx)
		if err != nil || len(all) != 2 || all[0].Slug != "acme" || all[1].Slug != "beta" || !slices.Equal(all[0].OptInTags, wantTags) {
			t.Fatalf("ListOrganizations = %+v, %v", all, err)
		}
		if _, err := r.GetOrganization(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an unknown organization: %v", err)
		}
	}

	existing, err := OpenExisting(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	readOnly, err := OpenReadOnly(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	checkRead(existing, []string{}) // OpenExisting: the handle `supavise upgrade` reads with
	checkRead(readOnly, []string{}) // OpenReadOnly: a follower's, on the hot standby of a leader that has not migrated

	// The leader's writes before it migrated: a create and a rename work, and a rename that carries
	// tags (which only a migrated registry can hold) is refused with the database's error.
	acme, err := existing.GetOrganization(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	created, err := existing.CreateOrganization(ctx, "gamma", "Gamma")
	if err != nil || created.Slug != "gamma" || len(created.OptInTags) != 0 {
		t.Fatalf("CreateOrganization = %+v, %v", created, err)
	}
	if err := existing.DeleteOrganization(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	acme.Name = "Acme Inc"
	acme.OptInTags = nil
	if err := existing.UpdateOrganization(ctx, acme); err != nil {
		t.Fatalf("rename before 1550: %v", err)
	}
	if got, err := existing.GetOrganizationByID(ctx, acme.ID); err != nil || got.Name != "Acme Inc" {
		t.Fatalf("renamed = %+v, %v", got, err)
	}
	acme.OptInTags = []string{"AI_SQL_GENERATOR_OPT_IN"}
	if err := existing.UpdateOrganization(ctx, acme); err == nil {
		t.Fatal("tags were saved to a registry that has no column for them")
	}

	// After 1550: tags are saved and read, and a rename with nil tags keeps them.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := existing.UpdateOrganization(ctx, acme); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*Postgres{"OpenExisting": existing, "OpenReadOnly": readOnly} {
		got, err := r.GetOrganization(ctx, "acme")
		if err != nil || !slices.Equal(got.OptInTags, []string{"AI_SQL_GENERATOR_OPT_IN"}) {
			t.Fatalf("%s: tags after 1550 = %+v, %v", name, got, err)
		}
	}
	acme.Name, acme.OptInTags = "Acme", nil
	if err := existing.UpdateOrganization(ctx, acme); err != nil {
		t.Fatal(err)
	}
	if got, _ := existing.GetOrganization(ctx, "acme"); got == nil || got.Name != "Acme" || !slices.Equal(got.OptInTags, []string{"AI_SQL_GENERATOR_OPT_IN"}) {
		t.Fatalf("a rename dropped the tags: %+v", got)
	}
}

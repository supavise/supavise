package registry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// missingTable reports whether err says a table is not there (SQLSTATE 42P01): the registry has not run
// migration 1520 (these tables) or 1540 (the GitHub state, github_store.go). A follower of a leader that
// has not been upgraded reads a schema without them. Reads treat it as "nothing recorded", so the
// follower's proxy goes on serving.
func missingTable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42P01"
}

// missingColumn reports whether err says a column is not there (SQLSTATE 42703): the registry has not run
// the migration that adds it. Migration 1550 (organizations.opt_in_tags) is the one column v0.4.0 adds to a
// table that the previous release reads, and withOrgCols (postgres.go) falls back without it;
// TestColumnsAddedToOlderTablesCanBeReadWithoutThem fails for the next one that has no fallback. Migration
// 1541 (github_branch_links.seeded_at) adds a column to a table that 1540 creates in the same release: only
// the GitHub store reads it, and the one Migrate call that applies 1540 applies 1541 next, so no registry
// holds the table without the column except between the two transactions.
func missingColumn(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42703"
}

func (r *Postgres) SetPause(ctx context.Context, ref, reason string, at time.Time) error {
	return affected(r.pool.Exec(ctx, `
		insert into supavise.project_pause (ref, reason, paused_at)
		select ref, $2::text, $3::timestamptz from supavise.projects where ref = $1
		on conflict (ref) do update set reason = excluded.reason, paused_at = excluded.paused_at`, ref, reason, at))
}

func (r *Postgres) ClearPause(ctx context.Context, ref string) error {
	_, err := r.pool.Exec(ctx, `delete from supavise.project_pause where ref = $1`, ref)
	if missingTable(err) {
		return nil
	}
	return mapErr(err)
}

func (r *Postgres) GetPause(ctx context.Context, ref string) (*Pause, error) {
	p := Pause{Ref: ref}
	err := r.pool.QueryRow(ctx, `select reason, paused_at from supavise.project_pause where ref = $1`, ref).Scan(&p.Reason, &p.PausedAt)
	switch {
	case missingTable(err):
		return nil, ErrNotFound
	case err != nil:
		return nil, mapErr(err)
	}
	return &p, nil
}

func (r *Postgres) ListPauses(ctx context.Context) ([]Pause, error) {
	rows, err := r.pool.Query(ctx, `select ref, reason, paused_at from supavise.project_pause order by ref`)
	if err != nil {
		if missingTable(err) {
			return nil, nil
		}
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []Pause
	for rows.Next() {
		var p Pause
		if err := rows.Scan(&p.Ref, &p.Reason, &p.PausedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		if missingTable(err) {
			return nil, nil
		}
		return nil, mapErr(err)
	}
	return out, nil
}

func (r *Postgres) GetExpiryWarning(ctx context.Context, ref string) (*ExpiryWarning, error) {
	var f, a *time.Time
	err := r.pool.QueryRow(ctx, `select expiry_warned_for, expiry_warned_at from supavise.branch_lifecycle where ref = $1`, ref).Scan(&f, &a)
	switch {
	case errors.Is(err, pgx.ErrNoRows), missingTable(err):
		return nil, ErrNotFound
	case err != nil:
		return nil, mapErr(err)
	case f == nil:
		return nil, ErrNotFound
	}
	w := ExpiryWarning{For: *f}
	if a != nil {
		w.At = *a
	}
	return &w, nil
}

func (r *Postgres) SetExpiryWarning(ctx context.Context, ref string, forExpiry, at time.Time) error {
	return affected(r.pool.Exec(ctx, `
		insert into supavise.branch_lifecycle (ref, expiry_warned_for, expiry_warned_at)
		select ref, $2::timestamptz, $3::timestamptz from supavise.projects where ref = $1
		on conflict (ref) do update set expiry_warned_for = excluded.expiry_warned_for, expiry_warned_at = excluded.expiry_warned_at`,
		ref, forExpiry, at))
}

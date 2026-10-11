package registry

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func (r *Postgres) GetBranchSchemaBase(ctx context.Context, branchRef string) (*BranchSchemaBase, error) {
	var b BranchSchemaBase
	err := r.pool.QueryRow(ctx, `select branch_ref, engine, fingerprint, snapshot, source, taken_at
		from supavise.branch_schema_base where branch_ref = $1`, branchRef).
		Scan(&b.BranchRef, &b.Engine, &b.Fingerprint, &b.Snapshot, &b.Source, &b.TakenAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &b, nil
}

func (r *Postgres) PutBranchSchemaBase(ctx context.Context, b BranchSchemaBase) error {
	_, err := r.pool.Exec(ctx, `insert into supavise.branch_schema_base (branch_ref, engine, fingerprint, snapshot, source)
		values ($1, $2, $3, $4, $5)
		on conflict (branch_ref) do update set engine = excluded.engine, fingerprint = excluded.fingerprint,
		  snapshot = excluded.snapshot, source = excluded.source, taken_at = now()`,
		b.BranchRef, b.Engine, b.Fingerprint, b.Snapshot, b.Source)
	return mapWriteErr(err)
}

func (r *Postgres) GetBranchFunctionBase(ctx context.Context, branchRef string) (map[string]string, error) {
	rows, err := r.pool.Query(ctx, `select slug, digest from supavise.branch_function_base where branch_ref = $1`, branchRef)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var slug, digest string
		if err := rows.Scan(&slug, &digest); err != nil {
			return nil, mapErr(err)
		}
		out[slug] = digest
	}
	return out, mapErr(rows.Err())
}

func (r *Postgres) PutBranchFunctionBase(ctx context.Context, branchRef string, set map[string]string, drop []string) error {
	return mapWriteErr(pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		for slug, digest := range set {
			if _, err := tx.Exec(ctx, `insert into supavise.branch_function_base (branch_ref, slug, digest) values ($1, $2, $3)
				on conflict (branch_ref, slug) do update set digest = excluded.digest, taken_at = now()`, branchRef, slug, digest); err != nil {
				return err
			}
		}
		if len(drop) > 0 {
			if _, err := tx.Exec(ctx, `delete from supavise.branch_function_base where branch_ref = $1 and slug = any($2::text[])`, branchRef, drop); err != nil {
				return err
			}
		}
		return nil
	}))
}

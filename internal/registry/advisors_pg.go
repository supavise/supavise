package registry

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const advisorCols = `id::text, ref, coalesce(lint_category, ''), coalesce(lint_name, ''), lint_metadata,
	coalesce(assigned_to::text, ''), is_disabled, coalesce(last_updated_by::text, ''), coalesce(note, ''), inserted_at`

func scanAdvisorException(row pgx.Row) (AdvisorException, error) {
	var e AdvisorException
	var meta []byte
	if err := row.Scan(&e.ID, &e.Ref, &e.LintCategory, &e.LintName, &meta, &e.AssignedTo, &e.IsDisabled, &e.LastUpdatedBy, &e.Note, &e.InsertedAt); err != nil {
		return e, mapErr(err)
	}
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &e.LintMetadata); err != nil {
			return e, fmt.Errorf("registry: advisor rule %s: lint_metadata: %w", e.ID, err)
		}
	}
	return e, nil
}

// nullText is "" as SQL null, so that the check constraints see a missing value.
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullJSON(m map[string]any) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (r *Postgres) ListAdvisorExceptions(ctx context.Context, ref string) ([]AdvisorException, error) {
	rows, err := r.pool.Query(ctx, `select `+advisorCols+` from supavise.advisor_exceptions where ref = $1 order by ord`, ref)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []AdvisorException{}
	for rows.Next() {
		e, err := scanAdvisorException(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, mapErr(rows.Err())
}

func (r *Postgres) CreateAdvisorExceptions(ctx context.Context, ref string, in []AdvisorException) ([]AdvisorException, error) {
	out := make([]AdvisorException, 0, len(in))
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// One writer at a time per project, so the cap holds under concurrent creates.
		var one int
		if err := tx.QueryRow(ctx, `select 1 from supavise.projects where ref = $1 for update`, ref).Scan(&one); err != nil {
			return mapErr(err) // ErrNotFound for an unknown project
		}
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from supavise.advisor_exceptions where ref = $1`, ref).Scan(&n); err != nil {
			return mapErr(err)
		}
		if n+len(in) > MaxAdvisorExceptions {
			return fmt.Errorf("%w: a project holds at most %d advisor rules", ErrConflict, MaxAdvisorExceptions)
		}
		for _, e := range in {
			meta, err := nullJSON(e.LintMetadata)
			if err != nil {
				return err
			}
			row, err := scanAdvisorException(tx.QueryRow(ctx, `
				insert into supavise.advisor_exceptions (ref, lint_category, lint_name, lint_metadata, assigned_to, is_disabled, last_updated_by, note)
				values ($1, $2, $3, $4::text::jsonb, $5::text::uuid, $6, $7::text::uuid, $8)
				returning `+advisorCols,
				ref, nullText(e.LintCategory), nullText(e.LintName), meta, nullText(e.AssignedTo), e.IsDisabled, nullText(e.LastUpdatedBy), nullText(e.Note)))
			if err != nil {
				return err
			}
			out = append(out, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Postgres) UpdateAdvisorException(ctx context.Context, ref, id string, p AdvisorExceptionPatch) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanAdvisorException(tx.QueryRow(ctx, `select `+advisorCols+` from supavise.advisor_exceptions where ref = $1 and id = $2::text::uuid for update`, ref, id))
		if err != nil {
			return err
		}
		if p.LintCategory != nil {
			cur.LintCategory = *p.LintCategory
		}
		if p.LintName != nil {
			cur.LintName = *p.LintName
		}
		if p.LintMetadata != nil {
			cur.LintMetadata = p.LintMetadata
		}
		if p.AssignedTo != nil {
			cur.AssignedTo = *p.AssignedTo
		}
		if p.IsDisabled != nil {
			cur.IsDisabled = *p.IsDisabled
		}
		if p.Note != nil {
			cur.Note = *p.Note
		}
		if p.LastUpdatedBy != "" {
			cur.LastUpdatedBy = p.LastUpdatedBy
		}
		meta, err := nullJSON(cur.LintMetadata)
		if err != nil {
			return err
		}
		return affected(tx.Exec(ctx, `
			update supavise.advisor_exceptions
			   set lint_category = $3, lint_name = $4, lint_metadata = $5::text::jsonb, assigned_to = $6::text::uuid,
			       is_disabled = $7, last_updated_by = $8::text::uuid, note = $9, updated_at = now()
			 where ref = $1 and id = $2::text::uuid`,
			ref, id, nullText(cur.LintCategory), nullText(cur.LintName), meta, nullText(cur.AssignedTo), cur.IsDisabled, nullText(cur.LastUpdatedBy), nullText(cur.Note)))
	})
}

func (r *Postgres) DeleteAdvisorExceptions(ctx context.Context, ref string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `delete from supavise.advisor_exceptions where ref = $1 and id = any($2::text[]::uuid[])`, ref, ids)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

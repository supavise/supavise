package registry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const branchRunCols = `id::text, owner_ref, branch_ref, parent_ref, op, status, check_run_id, workdir, failure,
	runner_host, runner_pid, runner_instance, created_at, updated_at`

func scanBranchRun(row pgx.Row) (BranchRun, error) {
	var r BranchRun
	err := row.Scan(&r.ID, &r.OwnerRef, &r.BranchRef, &r.ParentRef, &r.Op, &r.Status, &r.CheckRunID, &r.Workdir, &r.Failure,
		&r.RunnerHost, &r.RunnerPID, &r.RunnerInstance, &r.CreatedAt, &r.UpdatedAt)
	return r, mapErr(err)
}

// loadSteps fills the steps of the runs, in BranchRunSteps order. The logs are read only when
// withLogs is set: a list never needs them, and they can be large.
func (r *Postgres) loadSteps(ctx context.Context, runs []BranchRun, withLogs bool) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, len(runs))
	at := make(map[string]int, len(runs))
	for i := range runs {
		ids[i] = runs[i].ID
		at[runs[i].ID] = i
		runs[i].Steps = nil
	}
	logCol := "''::text"
	if withLogs {
		logCol = "log"
	}
	rows, err := r.pool.Query(ctx, `
		select run_id::text, name, status, created_at, updated_at, `+logCol+`
		  from supavise.branch_run_steps where run_id = any($1::text[]::uuid[])`, ids)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var s BranchRunStep
		if err := rows.Scan(&id, &s.Name, &s.Status, &s.CreatedAt, &s.UpdatedAt, &s.Log); err != nil {
			return mapErr(err)
		}
		runs[at[id]].Steps = append(runs[at[id]].Steps, s)
	}
	if err := rows.Err(); err != nil {
		return mapErr(err)
	}
	for i := range runs {
		SortBranchRunSteps(runs[i].Steps)
	}
	return nil
}

func (r *Postgres) CreateBranchRun(ctx context.Context, run *BranchRun) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			insert into supavise.branch_runs (id, owner_ref, branch_ref, parent_ref, op, status, check_run_id, workdir, failure,
			                                  runner_host, runner_pid, runner_instance)
			values ($1::text::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			returning created_at, updated_at`,
			run.ID, run.OwnerRef, run.BranchRef, run.ParentRef, run.Op, run.Status, run.CheckRunID, run.Workdir, run.Failure,
			run.RunnerHost, run.RunnerPID, run.RunnerInstance).Scan(&run.CreatedAt, &run.UpdatedAt)
		if err != nil {
			return mapErr(err)
		}
		for _, s := range run.Steps {
			if _, err := tx.Exec(ctx, `
				insert into supavise.branch_run_steps (run_id, name, status, log) values ($1::text::uuid, $2, $3, $4)`,
				run.ID, s.Name, s.Status, s.Log); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

func (r *Postgres) GetBranchRun(ctx context.Context, id string) (*BranchRun, error) {
	run, err := scanBranchRun(r.pool.QueryRow(ctx, `select `+branchRunCols+` from supavise.branch_runs where id = $1::text::uuid`, id))
	if err != nil {
		return nil, err
	}
	rs := []BranchRun{run}
	if err := r.loadSteps(ctx, rs, true); err != nil {
		return nil, err
	}
	return &rs[0], nil
}

func (r *Postgres) queryRuns(ctx context.Context, query string, args ...any) ([]BranchRun, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []BranchRun{}
	for rows.Next() {
		run, err := scanBranchRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, r.loadSteps(ctx, out, false)
}

func (r *Postgres) ListBranchRuns(ctx context.Context, ownerRef string, offset, limit int) ([]BranchRun, int, error) {
	var total int
	if err := r.pool.QueryRow(ctx, `select count(*) from supavise.branch_runs where owner_ref = $1`, ownerRef).Scan(&total); err != nil {
		return nil, 0, mapErr(err)
	}
	if offset < 0 {
		offset = 0
	}
	var lim any // null: no limit
	if limit >= 0 {
		lim = limit
	}
	runs, err := r.queryRuns(ctx, `select `+branchRunCols+` from supavise.branch_runs where owner_ref = $1
		order by created_at desc, id desc offset $2 limit $3`, ownerRef, offset, lim)
	return runs, total, err
}

func (r *Postgres) SetBranchRunSteps(ctx context.Context, id string, changes []BranchRunStepChange) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		// Lock the run: two writers of one run (the operation and a PATCH) apply one after the other.
		var one int
		if err := tx.QueryRow(ctx, `select 1 from supavise.branch_runs where id = $1::text::uuid for update`, id).Scan(&one); err != nil {
			return mapErr(err)
		}
		for _, c := range changes {
			var only any // null: from any status
			if len(c.OnlyFrom) > 0 {
				only = c.OnlyFrom
			}
			var name string
			err := tx.QueryRow(ctx, `
				update supavise.branch_run_steps
				   set status     = case when $3::text = '' then status else $3::text end,
				       log        = log || $4::text,
				       updated_at = now()
				 where run_id = $1::text::uuid and name = $2 and ($5::text[] is null or status = any($5::text[]))
				returning name`, id, c.Name, c.Status, c.Log, only).Scan(&name)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// Either the step does not exist (an error) or OnlyFrom excluded it (fine).
				var exists bool
				if err := tx.QueryRow(ctx, `select exists (select 1 from supavise.branch_run_steps where run_id = $1::text::uuid and name = $2)`, id, c.Name).Scan(&exists); err != nil {
					return mapErr(err)
				}
				if !exists {
					return ErrNotFound
				}
			case err != nil:
				return mapErr(err)
			}
		}
		_, err := tx.Exec(ctx, `update supavise.branch_runs set updated_at = now() where id = $1::text::uuid`, id)
		return mapErr(err)
	})
}

func (r *Postgres) FinishBranchRun(ctx context.Context, id, status, failure string) error {
	return affected(r.pool.Exec(ctx, `update supavise.branch_runs set status = $2, failure = $3, updated_at = now() where id = $1::text::uuid`, id, status, failure))
}

func (r *Postgres) SetBranchRunCheck(ctx context.Context, id string, checkRunID int64) error {
	return affected(r.pool.Exec(ctx, `update supavise.branch_runs set check_run_id = $2, updated_at = now() where id = $1::text::uuid`, id, checkRunID))
}

func (r *Postgres) RunningBranchRuns(ctx context.Context) ([]BranchRun, error) {
	return r.queryRuns(ctx, `select `+branchRunCols+` from supavise.branch_runs where status = 'RUNNING' order by created_at, id`)
}

func (r *Postgres) PruneBranchRuns(ctx context.Context, before, orphans time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		delete from supavise.branch_runs
		 where created_at < $1
		    or (created_at < $2 and not exists (select 1 from supavise.projects p where p.ref = owner_ref))`, before, orphans)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *Postgres) GetDefaultBranchGit(ctx context.Context, ref string) (string, error) {
	var g string
	err := r.pool.QueryRow(ctx, `select git_branch from supavise.default_branch_git where ref = $1`, ref).Scan(&g)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return g, mapErr(err)
}

func (r *Postgres) SetDefaultBranchGit(ctx context.Context, ref, gitBranch string) error {
	return affected(r.pool.Exec(ctx, `
		insert into supavise.default_branch_git (ref, git_branch)
		select ref, $2::text from supavise.projects where ref = $1
		on conflict (ref) do update set git_branch = excluded.git_branch, updated_at = now()`, ref, gitBranch))
}

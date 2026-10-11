-- Action runs of branches (v0.4.0, lane "runs"; range 1500-1509). Every create, push, merge and
-- reset of a branch is a run with seven steps, which Studio's Branches and merge pages, the
-- Supabase CLI and the MCP server read through /v1/projects/{ref}/actions. Additive only
-- (invariant I6).
--
-- owner_ref names the project the run is listed under: the parent for a merge, the branch for a
-- create, push and reset. It carries no foreign key because a create writes its run before the
-- branch's project row exists; rows of a project that is gone are pruned (PruneBranchRuns).

create table supavise.branch_runs (
  id              uuid        primary key,
  owner_ref       text        not null,
  branch_ref      text        not null,
  parent_ref      text        not null,
  op              text        not null,
  status          text        not null default 'RUNNING',   -- RUNNING, SUCCESS or FAILED
  check_run_id    bigint      not null default 0,           -- the GitHub check run, once there is one
  workdir         text        not null default '',          -- the repository directory of a Git-driven run
  failure         text        not null default '',          -- why the run failed
  runner_host     text        not null default '',          -- who works on the run, so that a restart
  runner_pid      integer     not null default 0,           -- can tell an interrupted run from one
  runner_instance text        not null default '',          -- that another process still drives
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now()
);
create index branch_runs_owner_idx on supavise.branch_runs (owner_ref, created_at desc);
create index branch_runs_running_idx on supavise.branch_runs (status) where status = 'RUNNING';

create table supavise.branch_run_steps (
  run_id     uuid        not null references supavise.branch_runs (id) on delete cascade,
  name       text        not null,                           -- clone, pull, health, configure, migrate, seed, deploy
  status     text        not null,                           -- CREATED, RUNNING, EXITED, PAUSED (skipped) or DEAD
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  log        text        not null default '',
  primary key (run_id, name)
);

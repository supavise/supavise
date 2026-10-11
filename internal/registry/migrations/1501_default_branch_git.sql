-- The Git branch a project's default branch tracks (v0.4.0, lane "runs"). Hosted's "Production
-- branch" setting (PATCH /v1/branches/{id} on the default branch, or a create with is_default)
-- stores it; nothing here acts on it yet. A table of its own keeps the project row unchanged.
-- Additive only (invariant I6).

create table supavise.default_branch_git (
  ref        text        primary key references supavise.projects (ref) on delete cascade,
  git_branch text        not null default '',
  updated_at timestamptz not null default now()
);

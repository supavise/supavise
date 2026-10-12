-- Why a project is paused, and what a branch has been told about its expiry (v0.4.0, lane
-- "lifecycle"). Range 1520-1529 belongs to that lane. Additive only (invariant I6).
--
-- project_pause has a row for a project that was paused through the lifecycle engine, written just
-- before the status becomes INACTIVE and removed when the project resumes. reason is '' for a
-- pause the owner asked for (the dashboard, the API, the CLI) and 'idle' for one the node made
-- because a branch was not used: only an 'idle' pause wakes on the first request or database
-- connection. paused_at is the time the pause began, which the dashboard shows as the last pause.
-- A project paused by a release that predates this table has no row; it counts as paused by its
-- owner. The proxy reads the table when it reloads a project, which the status change that follows
-- the row triggers, so the table needs no trigger of its own.
--
-- branch_lifecycle remembers that the node warned about a branch's expiry, and for which expiry,
-- so that the sweeper warns once per expiry and warns again when the expiry moves.

create table supavise.project_pause (
  ref        text        primary key references supavise.projects (ref) on delete cascade,
  reason     text        not null default '',
  paused_at  timestamptz not null default now()
);

create table supavise.branch_lifecycle (
  ref               text        primary key references supavise.projects (ref) on delete cascade,
  expiry_warned_for timestamptz,
  expiry_warned_at  timestamptz
);

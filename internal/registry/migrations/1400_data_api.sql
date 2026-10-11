-- Data API exposure of a project (internal/lifecycle, dataapi.go). Range 1400-1429 belongs to the
-- Data API work. Additive only (invariant I6).
--
-- Hosted Supabase stopped granting select, insert, update and delete on new tables of "public" (and
-- usage and select on sequences) to anon, authenticated and service_role by default. This table says,
-- per project, which way that default points and who decided it, so that a decision made once is never
-- made again:
--
--   * a project created by this release records the choice of its creation ('create');
--   * a project that existed before gets the revoke once, when the node first runs it on this release,
--     and records it ('upgrade'). A user who rolls the revoke back with SQL is not overruled at the next
--     start, because the row exists;
--   * a restore as new and a data branch copy the row of their source ('copy'); a schema-only branch
--     records the choice it was created with ('create').
--
-- default_privileges is what Supavise did or was told, not the live state of the database: a user who
-- runs the grants or the revokes themselves changes the database and leaves this row alone. A project
-- with no row has not been looked at yet by a release that knows this table.
create table supavise.project_data_api (
  ref                text        primary key references supavise.projects (ref) on delete cascade,
  default_privileges text        not null check (default_privileges in ('granted', 'revoked')),
  source             text        not null default 'create',
  decided_at         timestamptz not null default now()
);

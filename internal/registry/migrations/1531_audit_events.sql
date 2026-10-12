-- The audit log of the Management API (internal/api/audit.go): one row for each request that changed
-- something, with who made it, from where, which route, and how it ended. Studio's organization and
-- account Audit Logs pages read it. Range 1530-1539 belongs to the dashboard's dead ends. Additive only
-- (invariant I6).
--
-- No request body and no query string is stored, only the route template and its path parameters.
-- Rows older than [api] audit_retention_days are deleted by the API process (Prune), so the table is
-- bounded. org_id, not the slug, scopes a read: a slug can be used again by a new organization, an id cannot.
-- user_id has no foreign key, like org_members: the account can be removed and its history stays.

create table supavise.audit_events (
  id           bigint      generated always as identity primary key,
  at           timestamptz not null default now(),
  request_id   text        not null,
  org_id       bigint,
  org_slug     text,
  project_ref  text,
  user_id      text,
  email        text,
  token_type   text        not null,
  oauth_app_id   text,
  oauth_app_name text,
  ip           text,
  method       text        not null,
  route        text        not null,
  name         text        not null,
  status       integer     not null,
  params       jsonb       check (jsonb_typeof(params) = 'object')
);
create index audit_events_org_at on supavise.audit_events (org_id, at desc) where org_id is not null;
create index audit_events_user_at on supavise.audit_events (user_id, at desc) where user_id is not null;
create index audit_events_at on supavise.audit_events (at);

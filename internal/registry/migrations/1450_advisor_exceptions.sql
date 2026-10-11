-- Advisor rules of the Security and Performance Advisors (internal/api/advisors_exceptions.go):
-- what Studio's Advisor Rules page saves as "notification exceptions" and hosted keeps per
-- project. Range 1450-1459 belongs to the advisors. Additive only (invariant I6).
--
-- A rule selects lints by name, category and metadata (a null selector matches any) and hides them
-- from the whole project or from one member. Types follow the supabase-postgres-best-practices
-- skill: text, timestamptz, an index on the foreign key.

create table supavise.advisor_exceptions (
  id              uuid        primary key default gen_random_uuid(),
  ord             bigint      generated always as identity,  -- creation order; inserted_at ties inside a transaction
  ref             text        not null references supavise.projects (ref) on delete cascade,
  lint_category   text        check (lint_category in ('PERFORMANCE', 'SECURITY')),
  lint_name       text        check (char_length(lint_name) between 1 and 100),
  lint_metadata   jsonb       check (jsonb_typeof(lint_metadata) = 'object'),
  assigned_to     uuid,                                  -- a GoTrue user; no FK, like org_members
  is_disabled     boolean     not null,
  last_updated_by uuid,
  note            text        check (char_length(note) <= 2000),
  inserted_at     timestamptz not null default now(),
  updated_at      timestamptz not null default now()
);
create index advisor_exceptions_ref_idx on supavise.advisor_exceptions (ref, ord);

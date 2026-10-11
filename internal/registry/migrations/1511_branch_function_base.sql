-- The Edge Functions a branch was last in step with its parent (v0.4.0, lane "content"). One row
-- per function slug: the digest of the deployment both sides had, so that a merge can tell what the
-- branch changed (its digest differs from this one) from what the parent changed, and a push can
-- tell which of the parent's functions the branch has not taken in yet. Additive only (invariant I6).

create table supavise.branch_function_base (
  branch_ref text        not null references supavise.projects (ref) on delete cascade,
  slug       text        not null,
  digest     text        not null,
  taken_at   timestamptz not null default now(),
  primary key (branch_ref, slug)
);

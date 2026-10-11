-- The schema a branch was last in step with its parent (v0.4.0, lane "content"; range 1510-1519).
-- A branch is compared with this snapshot, not with its parent as it is now, so the diff the merge
-- page shows is what the branch changed and not what the parent got meanwhile: it is the common
-- ancestor of a three-way comparison. It is taken when the branch is created, advanced by a push
-- (the branch takes in the parent's changes) and by a merge (the parent takes in the branch's), and
-- kept by a reset of a schema-only branch. The snapshot is the JSON of internal/branching/schemadiff,
-- gzip compressed. Additive only (invariant I6).

create table supavise.branch_schema_base (
  branch_ref  text        primary key references supavise.projects (ref) on delete cascade,
  engine      integer     not null default 1,              -- schemadiff.FormatVersion of the snapshot
  fingerprint text        not null,                        -- Snapshot.Fingerprint
  snapshot    bytea       not null,
  source      text        not null default 'create',       -- create, push, merge or reset
  taken_at    timestamptz not null default now()
);

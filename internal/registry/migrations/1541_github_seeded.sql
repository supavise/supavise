-- Whether the seed of a repository has been applied to a preview branch (v0.4.0, lane "github").
-- The seed runs once per branch, in the first run that reaches it: a first run that failed in a
-- migration, or that a newer commit cancelled, leaves this empty and the next commit seeds the
-- branch. Additive only (invariant I6).

alter table supavise.github_branch_links add column seeded_at timestamptz;

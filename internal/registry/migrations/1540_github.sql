-- The GitHub integration (v0.4.0, lane "github"; range 1540-1549). One GitHub App per server,
-- created through GitHub's manifest flow or supplied by the operator in [github]; the people who
-- authorize it; the repositories connected to projects; and the pull requests that preview
-- branches follow. Additive only (invariant I6).
--
-- Secrets are sealed with the node's master key (internal/secrets) before they reach this
-- file's tables: the App's client secret, webhook secret and private key, and the users' GitHub
-- tokens. Nothing here stores one in the clear.

-- The server's App. At most one row. An App from [github] in config.toml has no row: its
-- credentials stay in the files the operator named.
create table supavise.github_app (
  singleton        boolean     primary key default true check (singleton),
  app_id           bigint      not null,
  slug             text        not null,                     -- github.com/apps/<slug>
  name             text        not null default '',
  client_id        text        not null,
  owner_login      text        not null default '',          -- the account that owns the App
  html_url         text        not null default '',
  sealed_client_secret  bytea  not null,
  sealed_webhook_secret bytea  not null,
  sealed_private_key    bytea  not null,
  created_by       text        not null default '',          -- the Supavise user, or "cli"
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now()
);

-- The state of a manifest flow in progress: GitHub hands it back with the code, once.
create table supavise.github_manifest_states (
  state_hash  bytea       primary key,                       -- SHA-256 of the state
  params      text        not null default '',               -- what the flow was asked for: the owning account and the App's name (JSON)
  created_by  text        not null default '',
  created_at  timestamptz not null default now(),
  expires_at  timestamptz not null
);
create index github_manifest_states_expiry_idx on supavise.github_manifest_states (expires_at);

-- A Supavise user's authorization of the App (the OAuth user-to-server token). One per user.
create table supavise.github_authorizations (
  id                 bigint      generated always as identity primary key,
  user_id            text        not null unique,            -- GoTrue user id; no FK, like org_members
  app_id             bigint      not null,                   -- the App the token belongs to
  github_user_id     bigint      not null,
  github_login       text        not null,
  sealed_access      bytea       not null,
  sealed_refresh     bytea,
  access_expires_at  timestamptz,
  refresh_expires_at timestamptz,
  created_at         timestamptz not null default now(),
  updated_at         timestamptz not null default now()
);

-- A repository connected to a project (hosted: GitHub connections of a project's integration page).
create table supavise.github_connections (
  id                    bigint      generated always as identity primary key,
  org_id                bigint      not null references supavise.organizations (id) on delete cascade,
  project_ref           text        not null unique references supavise.projects (ref) on delete cascade,
  installation_id       bigint      not null,
  repository_id         bigint      not null,
  repository_name       text        not null,                -- owner/name
  workdir               text        not null default '',     -- the folder that holds supabase/
  supabase_changes_only boolean     not null default false,
  branch_limit          integer     not null default 50,
  new_branch_per_pr     boolean     not null default false,
  created_by            text        not null default '',     -- GoTrue user id
  prod_functions        jsonb       not null default '{}'::jsonb, -- slug -> hash of the source last deployed to production
  inserted_at           timestamptz not null default now(),
  updated_at            timestamptz not null default now()
);
create index github_connections_org_idx on supavise.github_connections (org_id);
create index github_connections_repo_idx on supavise.github_connections (repository_id);

-- The Git side of a preview branch: the pull request or Git branch it follows, the commit it was
-- last built from, and the check run and comment that report on it.
create table supavise.github_branch_links (
  branch_ref      text        primary key references supavise.projects (ref) on delete cascade,
  project_ref     text        not null references supavise.projects (ref) on delete cascade, -- the parent
  repository_id   bigint      not null,
  repository_name text        not null,
  installation_id bigint      not null,
  pr_number       integer     not null default 0,            -- 0: a Git branch with no pull request
  head_ref        text        not null,
  head_sha        text        not null default '',
  check_run_id    bigint      not null default 0,
  comment_id      bigint      not null default 0,
  functions       jsonb       not null default '{}'::jsonb,  -- slug -> hash of the source last deployed
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now()
);
create index github_branch_links_project_idx on supavise.github_branch_links (project_ref);
create index github_branch_links_pr_idx on supavise.github_branch_links (repository_id, pr_number) where pr_number > 0;
create index github_branch_links_head_idx on supavise.github_branch_links (repository_id, head_ref);

-- Webhook deliveries seen, so that a replay of X-GitHub-Delivery does nothing twice. Old rows are
-- pruned.
create table supavise.github_deliveries (
  delivery_id text        primary key,
  event       text        not null default '',
  received_at timestamptz not null default now()
);
create index github_deliveries_received_idx on supavise.github_deliveries (received_at);

# internal/branching

Branches for agents (docs/design.md section 9a). A branch is a project with a parent: its own
Postgres, GoTrue and PostgREST, its own ref, keys and host, its own fleet tenants. An agent
creates one per task, merges what worked and deletes the rest. Like a hosted branch, one made from
the dashboard, the API, the CLI or MCP has no lifetime unless the node sets one; a branch nobody uses
sleeps (see "Idle pause").

`branching.Service` is the library. The Management API's branch endpoints
(`internal/api/branches.go`) and `supavise branches` (`cmd/supavise/cmd_branches.go`) are thin
wrappers over it, so the stock Supabase CLI (`supabase branches ...`) and
the Supabase MCP server's branch tools work unchanged. The MCP server must run unscoped (no
`--project-ref`): the project scope hides the account tools `create_branch` needs. `testdata/mcp-branches.mjs`
drives them against the API.

## Wiring

`supavise serve` (`internal/app/serve.go`) builds the service, hands it to the API as
`Deps.Branching` and to the edge proxy as `Options.IdleWaker`, runs the expiry sweeper (`svc.Run(gctx)`)
and the idle loop (`svc.RunIdle(gctx)`, on a server that leads, once the projects are up), and drains
running branch operations with the API's drain at shutdown (a cut-off operation ends `MIGRATIONS_FAILED`).

```go
svc, err := branching.New(branching.Deps{
    Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets,
    Engine: n.Engine,   // *lifecycle.Engine
    Backup: bk,         // optional *backup.Service (app.NewBackupService), with SetManager(n.Engine)
    Functions: fns,     // optional api.NewBranchFunctions(store, secrets): copies Edge Functions between a branch and its parent
    Log:    log,
    Activity: requests, // the *reqstats.Counter the proxy adds to; without it nothing sleeps
})
go svc.Run(ctx)                                  // the expiry sweeper
go svc.RunIdle(ctx)                              // the idle pause and the wake gates
srv := api.NewServer(api.Deps{ /* ... */ Branching: svc })
svc.Drain(shutdownCtx)                           // on shutdown, after the API stops accepting requests
```

Without `Backup`, `with_data` needs a filesystem that can clone files, and a branch's WAL archive is
not cleaned up on delete. The base-backup path restores through `Backup.WithManager(...)`, a copy of
the service that stamps the new project as a branch. Without `Deps.Branching` the API lists only the
default branch and refuses to create one.

## Source layout

One file per operation, so that a change to one operation stays in one file:

| File | Holds |
|---|---|
| `service.go` | `Service`, `Deps`, reading (`List`, `Get`, `Resolve`) and the helpers the operations share |
| `ops.go` | `ActionInput`, the in-process operation table (`begin`, `end`, `checkIdle`, `WaitIdle`) and the detached run wrapper `spawn` |
| `create.go` | `Create` and `doCreate` |
| `merge.go`, `push.go`, `reset.go` | `Merge` (history flow and the recorded-diff flow of Studio's merge page), `Push`, `Reset` |
| `diff.go` | `Diff` and `DiffWith` (`GET .../diff`) over the schema base |
| `baseline.go` | the baseline of a parent without a migration history, and the schema base of a branch (`getBase`, `putBase`, `recordBase`) |
| `carry.go` | what is carried between a branch and its parent besides schema: Edge Functions (`FunctionCarrier`), their base digests and the create-body secrets |
| `schemadiff/` | the schema engine: reads a database into a `Snapshot`, plans the DDL between two, merges and syncs snapshots |
| `update.go` | `Update` (PATCH of a branch) |
| `driven.go` | `Drive` (an operation on an existing branch whose work the caller does), `WakeForDrive` (resumes a paused branch, whoever paused it, and waits until it runs; `Wake` in `idle.go` is the request-driven one and leaves an owner's pause alone) and `Database` (what the service runs SQL with), all for the Git integration |
| `delete.go` | `Delete` (and the scheduled delete that pauses), `Restore`, `DisableBranching` and `removeBranch` |
| `idle.go`, `idlegate.go` | the idle pause (`IdleSweep`, `RunIdle`), `Wake`, and the listeners that wake a sleeping branch on a database connection |
| `expiry.go`, `placement.go` | the warning before a branch with a hostname expires, and the refusal of a clone whose parent is on another server |
| `migrations.go` | the migration history (`Migration`, `Compare`, `Apply`, covered migrations), `SchemaDatabase` (snapshots and history written without running it) and `replay` into a schema-only branch |
| `stmts.go` | which statements of a migration a recorded schema diff holds and which it does not (`carriedStatements`) |
| `sweep.go`, `notify.go` | the expiry sweeper and the `notify_url` call |

## Data model

Migration `0700_branching.sql` adds nullable columns to `supavise.projects`: `branch_id` (UUID,
stable across reset), `parent_ref` (foreign key, `on delete restrict`), `branch_name` (unique per
parent), `git_branch`, `persistent`, `with_data`, `expires_at`, `deletion_scheduled_at`,
`notify_url`, `branch_state`, `branch_detail`, `clone_method`, `review_requested_at`.
`registry.Project.Branch` (`*registry.BranchInfo`) is set on branches only.
`Registry.UpdateBranch` writes the branch fields and nothing else, so a concurrent lifecycle
status change is not overwritten. The egress policy (`branch_egress`) changes only through the
compare-and-set `Registry.SetBranchEgress(ref, from, to)` (`ErrConflict` when the policy is no longer
`from`), used by reset and by the end of isolation.

Branches are hidden from `GET /v1/projects` and `GET /platform/projects` and reported under
their parent (`preview_branch_refs`); they open by ref like any project (`parent_project_ref`
is set on their `/platform/projects/{ref}`). `GET /v1/projects/{ref}/branches` returns the
default branch (the project itself, `name: main`, `is_default: true`, a stable UUID derived
from the ref) first, as on hosted; called with a branch's ref it returns the whole family.

Migration `1510` adds `supavise.branch_schema_base` (the schema a branch and its parent last agreed on: a gzip
of the JSON of a `schemadiff.Snapshot`, its fingerprint and what took it: `create`, `push`, `merge` or `reset`)
and `1511` adds `supavise.branch_function_base` (the digest of each Edge Function both sides last had). Both are
keyed by the branch and removed with it; `registry.BranchContentStore` reads and writes them.

A parent cannot be deleted while it has branches: `Engine.DeleteWith` refuses before it stops
anything, and the foreign key backs that up. A create and a delete hold different lifecycle locks, so
each checks the other's write and no branch is left on a deleted parent.

## Operations

Create, merge, reset and push run detached from the request. The API answers at once (`201` with the
new branch in `CREATING_PROJECT` for a create, `{workflow_run_id, message: "ok"}` for the others) and
the branch's `status` follows: `CREATING_PROJECT`, `RUNNING_MIGRATIONS`, `MIGRATIONS_PASSED` while the
functions deploy, then `FUNCTIONS_DEPLOYED`, or `MIGRATIONS_FAILED` / `FUNCTIONS_FAILED` with the cause in
`branch_detail` (`supavise_detail` in the branch JSON, `supavise branches get`). A branch that finished
before these statuses existed keeps `MIGRATIONS_PASSED`; everything that waits for a branch accepts both.
`preview_project_status` is the project's own status. A busy branch refuses another operation with `409`
(an in-process table per branch ref plus the registry state, so a push during a merge of the same branch is
refused). The registry state counts as busy while it is `CREATING_PROJECT` or `RUNNING_MIGRATIONS`, and while it
is `MIGRATIONS_PASSED` with a run still `RUNNING` on the branch (the deploy step), so the CLI and the sweeper of
another process leave a deploying branch alone; `MIGRATIONS_PASSED` with no run going is a branch at rest. Events `branch.<op>.started`, `.succeeded` and `.failed` (with the run id) go to the branch's and
the parent's event log. The sweeper marks an operation that no process works on as failed: the process that
started it (host, pid and service instance in the started event) is gone, or the branch has been busy for
30 minutes. After a daemon crash the first sweep recovers its interrupted operations at once; an operation
of another host or of a live process keeps the 30 minute rule.

### Action runs

Every create, push, merge and reset is an **action run**: the record Studio's Branches and merge pages poll
(`GET /v1/projects/{ref}/actions`), the CLI and MCP clients can follow, and a `notify_url` receiver is told
about. `runs.go` is the recorder; the tables are `supavise.branch_runs` and `supavise.branch_run_steps`
(migration 1500).

* **Seven steps**, in this order: `clone`, `pull`, `health`, `configure`, `migrate`, `seed`, `deploy`, with
  the statuses `CREATED`, `RUNNING`, `EXITED`, `PAUSED` (skipped, or cut off by a failure before it) and `DEAD`
  (failed). The run is stored with all seven steps before the HTTP call returns, because Studio reads a run
  without steps as running for ever. `clone` and `configure` belong to the Git flow and start `PAUSED`.
* **Phases.** An operation reports progress with `s.phase(ctx, StepX)`: it closes the open step (`EXITED`),
  pauses the steps it skipped and opens the next. `spawn` finishes the run: a success closes the open step and
  pauses what never started; a failure makes the open step `DEAD` with the error in its log, pauses the steps
  after it and marks the run `FAILED` (`Run.Finish`). The run ends before the branch's status changes, so a
  client that sees the new status finds the run finished.

  | Operation | pull | health | migrate | seed | deploy |
  |---|---|---|---|---|---|
  | create, reset | the parent's state is read, the old cluster removed (reset) | the new project starts | the parent's history is replayed | the seed runs (skipped without one) | `MIGRATIONS_PASSED`, then the functions |
  | push | both histories are read | skipped | the plan is made (a divergence fails here) and applied | skipped | the functions |
  | merge | both histories are read | skipped | same | skipped | the functions |
* **Log.** Each step has a text log of lines `<time> [<step>] <text>`: the progress text of the operation (what
  `setState` records as `branch_detail`), the final outcome and the error of a failure. A step's log stops at
  256 KiB (cut at a character boundary; invalid UTF-8 is replaced and NUL bytes dropped, because Postgres
  refuses both). The run lists (`ListBranchRuns`, `RunningBranchRuns`) return steps without their logs; only
  `GetBranchRun` reads them. `GET .../actions/{id}/logs` returns the steps' logs in order as `text/plain`.
* **Owner.** A merge run is listed under the **parent** ref; create, push and reset runs under the branch ref.
  The other ref answers `404` for the run, its log and its status (Studio polls both refs and stops polling the
  one that answers 404). `Service.BranchRuns` gathers a branch's own runs and the merges of it.
* **Status.** The Management API has none; Studio derives it from the steps (no steps or a step not done:
  running; all `EXITED` or `PAUSED`: success; a `DEAD` step: failed), `DeriveRunStatus` is that rule, and
  `branch_runs.status` (`RUNNING`, `SUCCESS`, `FAILED`) stores the same answer. The default branch's status
  follows the merges into it: `RUNNING_MIGRATIONS` during one, `MIGRATIONS_FAILED` (reason in the detail) after a
  failed one, `FUNCTIONS_DEPLOYED` after a good one. A failure shows only while the merged branch still exists
  and is still failed: deleting that branch, or a later push, reset or merge of it that works, clears it.
* **Recovery.** A run still `RUNNING` whose runner (host, pid, service instance, stored on the run) is gone is
  failed by the next sweep with "interrupted by restart" (`DEAD` open step, `PAUSED` rest) and the branch it left
  busy becomes `MIGRATIONS_FAILED`; a run of an unknown runner is failed after the 30 minutes. Runs older than 30
  days, and those of a project that no longer exists, are removed.
* **PATCH `/actions/{id}/status`** sets the status of steps for a CI system that runs them itself (`UpdateRunStatusBody`).
  A run this process is working on refuses it (`409`). The run's status follows the steps. `REMOVING` and
  `RESTARTING` are container words this node's steps never have and are refused.
* **For code outside the package** (the GitHub runner): `Service.StartRun(ctx, op, branchRef, ownerRef, parentRef)`
  records a run the caller drives itself (all seven steps `CREATED`; the service leaves the branch's status and
  `notify_url` to the caller), and `Service.Recorder(ctx, runID)` returns a `*Run` for a run that exists, with
  `Step(name).Start/Finish/Fail/Skip`, `Phase`, `Log`/`Logf` and `Finish(err)`; `Create` returns the run it started
  in `Branch.RunID` (the run row exists before `Create` returns), and `Service.LatestRun(ctx, ref)` finds the newest run
  listed under a ref later. All of them are no-ops on a nil
  `*Run`. `registry.BranchRun.CheckRunID` and `Workdir` are for that runner.

### Notifications

A branch with a `notify_url` gets a POST when a run ends, for every operation (hosted sends it for persistent
branches only; this is a superset). The body is the envelope hosted documents
(<https://supabase.com/docs/guides/deployment/branching/working-with-branches>), with what the node sent
before the envelope existed under `supavise`:

```json
{
  "type": "run.completed",
  "timestamp": "2026-10-11T20:11:12Z",
  "data": {
    "project_ref": "<the branch's ref>",
    "details_url": "https://studio.<domain>/project/<parent ref>/branches",
    "action_run": {
      "id": "<run id>", "created_at": "...",
      "steps": [{"name": "clone", "status": "paused", "updated_at": "..."}, "..."],
      "failure": "migrate: ..."
    }
  },
  "supavise": {"branch_id": "...", "branch_name": "...", "project_ref": "...", "parent_project_ref": "...",
               "operation": "create", "status": "FUNCTIONS_DEPLOYED", "message": "...", "project_status": "ACTIVE_HEALTHY"}
}
```

`failure` is present only after a failed run (`<step>: <reason>`; the shape is Supavise's, hosted documents none).
Step statuses are lowercase. The call carries the Standard Webhooks headers `webhook-id` (`msg_` and the run id without its dashes, the same for every delivery of a run),
`webhook-timestamp` (Unix seconds) and `webhook-signature` (`v1,` and the base64 HMAC-SHA256 of
`<id>.<timestamp>.<body>`), signed with one node-wide secret (`whsec_` and base64) that the node creates when it is
first needed and keeps sealed under the system project. `supavise branches notify-secret` prints it, `--rotate`
replaces it. A node that cannot read the secret still sends the call, unsigned. The call has one attempt and a
10 second timeout, follows no redirect and refuses private targets (below).

### Create

`POST /v1/projects/{ref}/branches` (and `POST /v2/projects/{ref}/branches`, the JSON:API form of the same
create: `{data: {type: "branch", attributes: {name, git_branch, region, persistent, with_data, desired_instance_size,
notify_url}}}`; `desired_disk_size_gb` is refused with the reason) body `{branch_name, is_default, git_branch, region,
desired_instance_size, persistent, with_data, notify_url, secrets}`. The parent must be running. `region` is
accepted and the parent's is used (the answer says so in `supavise_region`). `secrets` (names of letters, digits and
underscores, not starting with `SUPABASE_`, at most 100) become the branch's Edge Function secrets; secrets are
never copied from the parent. A `release_channel` other than `ga` and a `postgres_engine` other than the
parent's are refused with 400, as is an unknown `desired_instance_size`
(`pico` is Nano; see `internal/lifecycle/README.md`, "Compute sizes"); without one a branch gets
`[branching] default_class` (`micro`, as on hosted) and counts against the node's memory budget. A
`with_data` create is refused with `409` before anything exists when the state disk cannot hold the clone
(Free disk), and its outbound side effects are blocked unless the request opts out (Outbound isolation).

**Schema only** (the default, as on hosted): a new project, then the parent's migration history
(`supabase_migrations.schema_migrations`: versions, names and statements, each migration in its own
transaction, or outside one when a statement refuses to run in a transaction block), then the seed. The Management API has no seed field, so the seed is stored per parent (`supavise branches seed set
<parent-ref> --file seed.sql`, sealed like a project secret under `branch_seed`) or given for one branch
with `--seed-file`. A failing migration or seed leaves the branch in `MIGRATIONS_FAILED` for inspection.

**A parent without a migration history** (everything was built in the SQL editor, the Table Editor or
`execute_sql`) gets a **baseline**, as on hosted: the first branch reads the parent's schema (tables, columns,
constraints, indexes, policies, triggers, functions, views, types, sequences, extensions, grants, default
privileges and comments of the user schemas, plus the policies and triggers users added to the tables of the
managed schemas, such as a policy on `storage.objects` or a trigger on `auth.users`), writes it as one migration
named `remote_schema`, runs it on the branch and, **only when that worked**, records it as the **first migration of
the parent's history without running it there** (with the event `branch.baseline_recorded`). The parent's apply lock is held from the look at
the history to the record, so a second branch made meanwhile waits and then replays the history the first one
recorded; a baseline that fails on the branch leaves the parent's history empty. The parent now has a history, so a
merge has a common ancestor, and the next branch replays it: the
schema is dumped only while the history is empty (hosted does not dump when migrations exist), and a parent that
has a history plus changes made outside it gives a branch the history only (those changes show up as the parent's
drift, which a push brings in). The branch applies the parent's Data API default-privilege choice when its project
is created and the baseline after it, so the baseline's own `ALTER DEFAULT PRIVILEGES` statements win. Custom
roles are not captured: a grant, a default privilege or a policy that names a role the branch lacks is left out and
the run log says which. What the engine does not model (aggregates, operators, casts, rules, event triggers,
publications, foreign tables and servers, range types, collations, extended statistics, inherited tables, column
privileges) is named in the run log and is not in the baseline, and does not by itself make a schema: a parent with
only such objects and what a new project has gets no baseline. The stock event triggers of a project (`pgrst_*`,
`issue_*`, `graphql_watch_*`) and its Realtime publications (`supabase_realtime*`) are never listed as not modeled.

> **A `with_data` branch is a copy of production.** Whoever can run SQL in it can read all of the parent's
> data. Supavise replaces the credentials it knows about and cuts the branch off from the outside (below), but
> it does not detect node-local credentials that users put in their own tables, function bodies, Vault entries
> or settings, and a branch can reach every port on loopback. `with_data` is opt-in and meant for trusted users
> and agents; see "What a branch with data can and cannot reach".
>
> Creating a `with_data` branch through the Management API, and resetting one, needs the Owner or
> Administrator role (also when scoped to the parent project); Developers keep creating and resetting
> schema-only branches, and get 403 otherwise. `supavise branches create` runs on the node as its operator
> and is not checked.

`is_default: true` returns the default branch (the project itself) and creates nothing, again and again; a
`git_branch` in the same request is stored as the Git branch the default branch tracks (Studio's "Production
branch" save sends it). `PATCH` on the default branch stores `git_branch` too (migration 1501,
`supavise.default_branch_git`; nothing acts on it yet) and refuses what a default branch cannot have: a rename
by itself (Studio sends `branch_name` together with `git_branch`, which is ignored), `persistent: false`, a
`notify_url`, `request_review: true` and a status other than the current.

### What a branch contains

| | Schema-only | With data |
|---|---|---|
| Database schema and migration history | the parent's `supabase_migrations` history, replayed, then the seed; the parent's schema as a baseline when it has no history (above) | everything in the parent's database |
| Rows | none, except what the migrations and the seed insert | all of them (auth users and identities included; their sessions and one-time tokens are removed) |
| Storage buckets | those the migrations create | the parent's buckets, with their settings and policies |
| Storage objects | none | none: the rows of `storage.objects` (and `storage.prefixes`, `storage.s3_multipart_uploads*`) are removed from the clone; the files stay with the parent. Exception: when a table of yours has a foreign key into one of them, the rows stay (below) |
| Edge Functions | every function deployed on the parent (code, `verify_jwt`, entrypoint, import map, static files), with the parent's version, update time and content digest | the same |
| Function secrets | none; the `secrets` of the create body are stored for the branch | the same |
| Saved settings (Auth providers, SMTP, MFA, hooks, PostgREST, Storage and Realtime limits, non-sizing Postgres settings) | the parent's, minus what identifies, addresses or sizes it (below) | the same |
| API keys, JWT secret, database password | the branch's own | the branch's own (the parent's are replaced everywhere Supavise finds them) |
| Default privileges of `public` for the Data API roles | the parent's recorded choice, applied before the migrations replay (revoked unless the parent keeps the grants) | the parent's, in the copied database; a clone copies the parent's recorded choice, a branch made from a base backup takes the choice the backup recorded (none: the first pass revokes) |

Hosted Supabase copies a project's Edge Functions and configuration into a branch and never its Storage objects or data (unless the branch is created with data), and so does Supavise: a branch has its own Storage tenant and functions, so nothing is shared.

- **Storage objects are not copied.** A clone would carry the rows without the files, so every listed object would answer 404. Isolation empties the object metadata tables and leaves the buckets (`branch.isolated` event, `storage_rows_deleted`; user triggers do not fire), in a database that has Storage's own tables. If a table outside Storage has a foreign key into them, the wipe is skipped in that database (changing your tables is not isolation's job) and the branch lists the parent's objects, which answer 404 until you remove or re-upload them; the event names the constraints (`storage_wipe_skipped`).
- **Edge Functions are copied, function secrets are not.** At create, the deploy step copies every function row and its files from the parent's store entry to the branch's (`FunctionCarrier.Copy`, implemented over the API's store in `internal/api/branches_functions.go`): a new id, and the parent's version, `updated_at` and content digest (`ezbr_sha256`, which Studio's merge page compares to tell equal functions from changed ones). The Edge Functions syncer serves them at once (the carrier tells it) and the digests become the branch's *function base* (`branch_function_base`). Secrets are per branch, as on hosted: nothing is copied from the parent, and the create body's `secrets` are the branch's only ones. A reset keeps the branch's functions, secrets and settings (they are rows in the API's store and `project_settings`, which a `KeepRecord` delete leaves alone) and tells the syncer to serve them again; files the branch uploaded stay in its Storage backend, unreferenced.
- **Settings are copied, less the parent's credentials.** The engine gives the new project the parent's saved settings (`projectconfig.Manager.CopyForBranch`, called from `lifecycle.Engine.copyInitialSettings` before the units first start) in both modes. Never copied: the JWT secret, the API keys and the database password (a branch always has its own; they are not settings); the host-bound settings `site_url`, `uri_allow_list` and `saml_external_url`, and the WebAuthn relying party (`webauthn_rp_id`, `webauthn_rp_origins`) with the three switches that need it (`passkey_enabled`, `mfa_web_authn_enroll_enabled`, `mfa_web_authn_verify_enabled`); the custom hostname and vanity subdomain (not settings); sizing (`db_max_pool_size`, PostgREST's `db_pool`, Realtime's pools, `suspend`, the whole Pooler service and the nine Postgres settings the compute class puts on the command line, `projectconfig.SizingSettings`). **No credential is copied, whatever the branch's egress.** The egress policy covers the branch's Postgres unit only (`DenyEgress`), a schema-only branch has none, and a sealed value binds to no project, so a copied SMTP password, SMS token or hook secret would let anyone with the branch's anon key send mail or SMS, or call a hook, as the parent (`POST /auth/v1/recover`, `/otp`). `projectconfig.BranchWithheld` lists what is left out: every sealed secret (the SMTP password, the SMS providers' tokens and keys, the OAuth clients' secrets, the captcha secret, the hook secrets) and the settings that validation ties to one, so that no provider is left half set up: the SMTP host, port and user, the SMS provider and its `sms_<provider>_*` settings, the switch of each external OAuth provider (its client id is copied), the captcha switch, and the uri and switch of each hook that calls an HTTP(S) URL (a `pg-functions://` hook runs in the branch's database and is copied). The branch's owner sets its own, as hosted keeps secrets per branch; until then an OAuth provider, the captcha and an HTTP hook are off, and the branch sends no mail or SMS. `mailer_autoconfirm` is a behavior setting and is copied, so a parent that turned it off keeps it off on a branch that has no SMTP yet (its signups cannot send the confirmation mail until the branch has an SMTP server). The exception is `mailer_allow_unverified_email_sign_ins` of a parent that saved an SMTP host and no `mailer_autoconfirm`: that parent has confirmation on only because it has SMTP, the branch runs with auto-confirm on, and GoTrue refuses the two together, so the setting is withheld (it would have lost the branch its whole Auth copy). The `project.settings_copied` event lists the withheld names, never a value. A clone's inherited `postgresql.auto.conf` loses the same sizing lines (`dropSizing`), so a micro branch never inherits a larger parent's `shared_buffers`. Push and merge do not move settings (hosted ignores API and Auth configuration on merge). Golden tests list the exclusions and the withheld names, and fail for a new setting that looks like an address or a new secret.
- **PostgREST's exposed schemas wait for the schema of an empty branch.** PostgREST does not start on a schema that does not exist, and a schema-only branch starts with an empty database. So `CopyForBranch` defers the parent's `db_schema` when the create has no seed (`BranchCopyOptions.Seeded`; `lifecycle.Engine` sets it from `CreateRequest.Seed`), the branch's PostgREST starts on the default schemas, and `doCreate` calls `SchemaExposer.ExposeSchemasThatExist` (implemented by `lifecycle.Engine`) once the migrations and the seed, or the `SchemaSource`, have built the schema. It saves the names of the parent's list that are schemas of the branch, in order, and restarts PostgREST on them; a name the branch lacks is left out, said in the run's log and listed in the `branch.created` event (`exposed_schemas`, `unexposed_schemas`), and does not fail the create. A list that the build saved itself (a repository's `[api] schemas`) is the branch's own and wins. A reset recreates the cluster empty under the branch's saved list, so `doCreate` takes the list off the settings (`SetExposedSchemas`) before the create and applies it after the replay; a build that fails puts the list back, so nothing is lost and the next reset tries again. A branch with data takes the list with the copy, because the clone has the schemas.
- **Clone method** (`clone_method`, the cheapest available): `clonefile` on APFS, `reflink` on XFS with `reflink=1`, btrfs, bcachefs and OpenZFS 2.2+ with block cloning, else `base-backup` (restore of the parent's latest base backup plus all archived WAL; `[branching] clone = "backup"` forces it). The copy-on-write methods keep time and disk flat as the database grows; the restore grows with it. `detectClone` tries to clone the parent's `PG_VERSION` into the new directory, so it tests the exact pair of paths and names the filesystem and reason when a clone is impossible. ZFS without block cloning takes the base-backup path. With neither a clone nor a backup backend the create fails before anything exists.
- **How the clone is taken:** `pg_backup_start` on a superuser session that stays open, a file-by-file clone of PGDATA (`pg_control` last), `pg_backup_stop`, `backup_label`, then the WAL segments up to the end-of-backup record; the branch starts as a crash recovery and needs no archive. A recycled segment retries the whole procedure (three attempts), a file that cannot be cloned is byte-copied, tablespaces are refused.
- **Service credentials are rotated.** The clone's roles carry the parent's passwords, so once the cluster is up the service seals new passwords for the six service roles, sets them over the private socket as SCRAM verifiers and calls `Manager.RotateKeys` (new JWT secret and API keys, GoTrue and PostgREST restart, fleet tenants updated). Until the rotation finishes only the six role passwords are the parent's: the JWT secret and API keys are replaced before the project is created, so the parent's service key never works on the branch. The parent's database password does not open the branch, and the parent's temporary CLI login roles (`cli_login_*`, `supavise_cli_ro_*`) are set `NOLOGIN` with no password. The pgsodium root key cannot change (Vault secrets are encrypted with it), so the branch shares it with its parent: whoever holds the parent's root key can decrypt the branch's copy of the parent's Vault secrets.
- **The parent's credentials inside the data are replaced.** A cloned service key is a key to the parent that anyone with SQL in the branch can read, and a branch reaches the node's loopback. After the rotation `rewriteCredentials` (`credswap.go`) replaces, with the branch's value, every parent credential in Vault secrets, pg_cron job commands (paused or not) and database and role settings (`pg_db_role_setting`). A parent credential is an anon or service_role JWT that verifies against the parent's JWT secret, the `sb_publishable_` and `sb_secret_` keys, the JWT secret or one of the six database passwords. A value is rewritten when it contains one (`Bearer <key>`, a connection string); a credential under 20 characters must match whole. Replacements are recorded by name, never value (event `branch.credentials_rewritten`, table `supavise_branch.rewritten_credentials`). A failure stops the branch.
- **Sessions of the parent's users are removed**, because a refresh token read in the branch is a session of a production user. In every `with_data` branch, with or without `--allow-egress`, `wipeAuthSessions` (`isolate_auth.go`) empties the session, refresh-token, MFA-challenge, one-time-token, flow-state, SAML/OAuth-state and WebAuthn-challenge tables of `auth`, and blanks the token columns of `auth.users` and the passkey challenge of `auth.mfa_factors`. Users and identities stay, so branch users sign in again with their passwords and get tokens of the branch. What stays is production data, not a session: password hashes, TOTP secrets, the argon2id hashes of MFA recovery codes, passkey public keys, OAuth and SSO configuration, SCIM provisioning (the provisioned users and the SHA-256 digests of the SCIM bearer tokens) and third-party client secrets (`auth.custom_oauth_providers`). The event records counts per table (`auth_rows_deleted`, `auth_rows_cleared`).
- **Auth tables are classified.** The two lists in `isolate_auth.go` (wipe, blank) cover the GoTrue release pinned in `internal/versions/versions.yaml`. A table in neither list is named in the event (`auth_tables_not_reviewed`) and the integration test fails on it, so a GoTrue upgrade that adds a table gets classified.
- **Subscriptions, cron jobs and pg_net are neutralized**, because a copy would attach to the parent's replication slot and run cron jobs and queued pg_net calls twice. Both with_data paths start the cluster first with `max_logical_replication_workers = 0`, `cron.launch_active_jobs = off` and `pg_net.database_name` pointing at a missing database (lines marked `# supavise-branch-quarantine` in `postgresql.auto.conf`). Then, in every database, subscriptions are disabled and detached from their slot (so the publisher's slot survives), `cron.job` rows are set inactive (unless `[branching] keep_cron_jobs = true`) and `net.http_request_queue` is emptied; the marked lines go, the cluster restarts and the credentials rotate. A failing step stops the branch (`MIGRATIONS_FAILED`).
- **Paused cron jobs** are recorded in `supavise_branch.paused_cron_jobs`; the owner opts in with `update cron.job set active = true where jobid in (select jobid from supavise_branch.paused_cron_jobs);`. Job `nodename` and `nodeport` point at the branch's own cluster, and a connection string written as a literal in a command is replaced by a disabled one (`supavise_branch.neutralized_cron_commands`; the original is not kept, it may hold a password).
- **Every database but `template0`** is isolated, templates and `datallowconn = false` databases included. Such a database is opened with `ALTER DATABASE ... ALLOW_CONNECTIONS true` for the duration and closed again (`databases_opened`; if Supavise stops between the two statements the flag stays open and the unfinished branch ends up failed), so its owner cannot find its foreign servers and Vault secrets as the parent had them.
- **Foreign servers are disabled** in every database of a branch without the egress opt-out (`neutralizeForeign`, `foreign.go`; safe to run twice), loopback or not, because a loopback server reaches another project on this node with its stored password. This covers `postgres_fdw`, `dblink_fdw` and any server with a libpq option (`host`, `hostaddr`, `port`, `dbname`, `service`): `host` becomes the nonexistent socket `/nonexistent/supavise-branch-disabled`, `hostaddr` and `service` are dropped, and user mappings lose a `password`, `sslpassword` or `passwd` option. A wrapper whose validator refuses the change is named in the event (`foreign_servers_not_neutralized`). `supavise_branch.paused_foreign_servers` records what was done, **but not the passwords** (the table is readable by whoever owns the branch). To turn a server back on, set its options and re-enter the password: `alter server s options (set host '...'); alter user mapping for ... server s options (add password '...');`. Foreign tables and functions that use the server fail until then.

### Outbound isolation

Everything else the parent's data can do toward the outside world is outbound traffic from the branch's Postgres: database webhooks and triggers that call `pg_net`, cron jobs that make HTTP calls, other Wrappers servers, any extension that connects out. A branch with data has these defaults:

1. **Its Postgres unit may reach 127.0.0.1 and ::1 only.** Under systemd the unit gets `IPAddressDeny=any` and `IPAddressAllow=127.0.0.1/32 ::1/128` through the per-unit drop-in (`units.Spec.DenyEgress`, as for `MemoryMax`). The allow list is not systemd's `localhost` (127.0.0.0/8), whose other addresses include systemd-resolved's stub at 127.0.0.53, a DNS side channel. The filter is a cgroup BPF program, so it covers every process of the cluster (backends, pg_net and pg_cron workers, `COPY ... PROGRAM`) and survives restarts and reboots.
2. **Unix sockets are hidden instead**, because an IP filter does not touch them: `deploy/systemd/supavise-postgres@.service` sets `InaccessiblePaths` for `/etc/supavise`, `/run/dbus`, systemd-resolved's varlink socket and nscd's socket, for every project's Postgres (systemd 255 cannot change it over D-Bus). Names then resolve only through `/etc/hosts` or a resolver on 127.0.0.1:53. A node installed before this needs the new unit template (the installer and self-update install it).
3. **Cron jobs, subscriptions, the pg_net queue, foreign servers and the parent's credentials** are handled as above, whatever the supervisor.

The filter is applied after the first start (a base-backup restore may need the backup backend to finish recovery). The branch's `egress` field (`supavise branches get --json`; `supavise_egress` in the API's branch JSON, an extra field that spec-decoding clients ignore) is `pending` (until isolation finishes), `denied`, `allowed` or `unenforced`, and its `detail` says in words what the branch can reach. A reset keeps the policy: the new cluster is created open, then denied again after its first start.

**Opt-out:** `supavise branches create ... --with-data --allow-egress`, or `POST /v1/projects/{ref}/branches?allow_egress=true` (our query parameter, so the stock CLI and MCP server cannot set it). The branch keeps the parent's outbound side effects: no egress block, cron jobs active, foreign servers, user mappings and cron connection strings as the parent had them (a loopback foreign server still reaches another project on the node); `egress` is `allowed`. Subscriptions are still detached and credentials still replaced. `[branching] keep_cron_jobs` keeps only the cron jobs. Use the opt-out for a branch that must call a real service, not for an agent you do not trust.

**The exec backend cannot block egress.** `supervisor = "exec"` (development and tests) runs units as plain child processes with no cgroup, so a branch with data reports `egress: unenforced` ("egress NOT blocked"). The other neutralizing steps still run, but a webhook, pg_net or any extension can reach the outside: do not clone a production database through the exec backend.

Limits of the block:

- A **remote backup backend** (`s3://`) is outbound traffic too, so WAL archiving of a branch with denied egress cannot reach it (the default `file://` backend works). On an S3 node the `archive_command` of a with-data branch keeps failing and its WAL piles up in `pg_wal`: give a persistent or write-heavy branch there `--allow-egress`, or delete it when its work is done.
- A schema-only branch replays the parent's migrations, which may schedule cron jobs or install triggers that call out. It inherits no data, so it is not isolated.
- Only the branch's Postgres unit is confined, not its Realtime, Storage and pooler tenants.

### What a branch with data can and cannot reach

For a `with_data` branch created without `--allow-egress`, on the systemd supervisor, and nothing more.

**It cannot:**

* Send anything from its Postgres unit to an address other than 127.0.0.1 and ::1 (a lookup can still carry data out through a resolver on 127.0.0.1:53 where one runs; IPv6 is not tested).
* Use a foreign server of the cloned data or a cron `dblink` string to reach the parent or another project.
* Use the parent's API keys, JWT secret or database passwords where its setup kept them (Vault, cron commands, settings); only the keys Supavise issued are covered.
* Open another project's Postgres through its unix socket or read its files: the unit sees an empty `/var/lib/supavise` with only its own cluster directory.
* Keep the parent's subscriptions, queued pg_net requests, active cron jobs or user sessions.

**It can:**

* Reach **any port on 127.0.0.1 and ::1**, because the filter is on addresses, not ports: the parent's Postgres, every other project's, the proxy, the admin API (`127.0.0.1:7000` by default) and the fleet services, through pg_net, `COPY ... PROGRAM` (superuser), untrusted procedural languages or a `dblink` string in a query. Only their own authentication stands in the way.
* Use any credential of the parent that the data holds in a place that is not rewritten.

**Residual risk:**

* **Node-local credentials that users stored themselves are not detected**, because Supavise cannot tell which strings in a copy of production are secrets. Credentials in user tables, function bodies, trigger arguments (database webhook headers), foreign table or other wrapper options, Storage objects, Vault entries other than the keys Supavise issued, settings other than those rewritten, encoded forms or `ALTER SYSTEM` stay as they were. That includes a connection string or key for another project on this node or a custom login role of the parent, an `sbp_` token for the admin API, and a JWT with a role other than `anon` or `service_role`. Egress denial does not help, because the target is on the node; a third-party credential (a Stripe key) cannot leave the node while egress is denied, but it is in the branch.
* Whoever can run SQL in the branch has a copy of the parent's data, whatever is rewritten, and the shared pgsodium root key decrypts unrewritten Vault secrets.
* The unit's isolation is a mount namespace and an IP filter, not a sandbox: all units run as the same user and share `/proc`, so code execution inside a branch's Postgres is not contained.
* A schema-only branch replays migrations verbatim, so a credential written into a migration is in the branch.
* Without systemd (the exec backend) the network is not filtered.

For untrusted agents prefer schema-only branches (the default, as on hosted): they copy no data.

### Free disk

A `with_data` create or reset refuses to start when the state directory's disk (`statfs` of the parent's
cluster directory) has less free space than **1.2 times the parent's data (the apparent size of its PGDATA,
pg_wal included) plus a reserve**, `[branching] disk_reserve_mb` (default 2048). The answer is `409` in the
API's message envelope and nothing has been created or removed. The check is the same for every clone method, because a copy-on-write clone diverges as either side writes. A reset of a
base-backup branch counts the space of its old private copy, which the reset removes first; a copy-on-write
branch's does not count. A free-space figure that cannot be read skips the check with a log warning.
Schema-only branches copy nothing and are not checked.

### Merge, push, reset

* **Merge** (into the parent only). Two flows end here.
  *History* (the CLI and the MCP server: `db push`, `apply_migration`): the branch's migrations that the parent lacks run on the
  parent in one transaction under a session advisory lock (`lock_timeout` 30 s, so DDL on a busy parent fails instead of queueing
  traffic behind it). Versions and divergence are checked again under the lock (a version another merge applied meanwhile is
  refused with `409`), and a statement that cannot run in a transaction block makes it fall back to one transaction per migration
  (the result says so). It **refuses on divergence**: the parent has migrations the branch lacks, a version exists on both sides
  with different content, or a branch migration is older than the parent's latest. `?force=true` (our extension) merges anyway and
  skips the versions that differ in content. `migration_version` merges up to and including that version. Changes the branch made
  outside migrations (the Table Editor, the SQL editor) are not merged by this flow, as on hosted.
  *Recorded diff* (Studio's merge page): Studio reads `GET .../diff`, records it with `PUT /v1/projects/{branch}/database/migrations`
  as `branch_merge_<time>` **without running it**, then calls merge. The merge takes this flow when the newest migration of the
  branch is such a recording and the parent lacks it (and `migration_version` does not cut it off). It (1) fails if the diff holds a
  change the engine cannot express, unless `force` (those changes are then not merged, and the result names them), (2) computes the
  diff again and compares its hash with the recorded one, failing with "production or the branch changed since the diff was recorded;
  refresh" when they differ (`force` does not merge a diff that is out of date: what would run is not what the user approved),
  (3) fails when production and the branch changed the same schema object differently, unless `force`, and (4) in **one transaction**
  runs the recorded migration on the parent, records the branch's other migrations that the parent lacks in the parent's history
  **as covered by it**, without running their schema changes (the diff holds those), and then runs **the statements of those migrations
  that a schema diff does not hold**: INSERT, UPDATE and DELETE of data (a lookup table, a bucket), `cron.schedule`,
  `ALTER PUBLICATION`, roles, and the objects the engine does not model (`stmts.go` decides: DDL of tables, columns, constraints,
  indexes, views, functions, triggers, policies, types, sequences, extensions, schemas, grants, default privileges and comments is
  the diff's; `CREATE TABLE ... AS`, a temporary table and DDL on objects of the services' own schemas such as `auth.users` or
  `storage.objects`, apart from policies and triggers, are not). Anything that fails rolls all of it back, so a retry starts from
  the same state. The statements run after the diff, in version order, and not in between: a migration whose later DDL changes
  what an earlier INSERT needs can fail, loudly and with a rollback. A reset, a new branch and a push replay a covered migration the
  same way (its DDL recorded, the rest run, after the migrations that are not covered). The mark on the branch's own rows is written
  after the parent merged; if that fails the run log says so and the next merge writes it. A recording that is **not the newest
  migration** of the branch is out of date: the branch moved on after it (a failed attempt, then `apply_migration` or `db push`).
  The merge ignores it and all older ones, takes the history flow, and says so; the schema changes it held that no migration made
  are merged by opening the merge page again. A recorded migration that is covered by an ignored one runs in full. The same divergence
  rules as the history flow apply to the parent's own migrations; `force` skips them.
  Either way the branch's Edge Functions are deployed to the parent: a function the branch created, or changed since it was made
  (its digest differs from the base), is copied over the parent's; a function only the parent changed is left alone; one **both
  changed** fails the merge before anything runs, naming the slugs (`force` deploys the branch's version); nothing is ever deleted
  from the parent. The merge ends by moving the schema base to where the two sides now agree. A merge that finds nothing says
  "nothing to merge". The branch is kept.
* **Push** (the rebase, `POST .../push`, Studio's "Update branch") brings the parent into the branch: its migrations the branch lacks
  (a version that differs in content stops it unless forced); then the parent's **schema changes that were never migrations** (the
  objects the parent added, changed or removed since the schema base, that the branch did not change itself) in one transaction,
  recorded in the branch's history as `branch_push_<time>` (a reset replays it; a merge leaves it out, because the parent has the
  changes); then the parent's Edge Functions that are new or changed (same slug: the parent's overwrites the branch's, unless only
  the branch changed it; branch-only functions stay; the run log says what was overwritten). It refuses, unless forced, when the
  parent and the branch changed the same schema object differently (`force` takes the parent's version). It never reseeds and
  never touches data, and it moves the schema base. `migration_version` stops the migrations at that version and skips the rest,
  including the parent's changes that were never migrations; the base still moves to where the two sides agree, so that the objects
  the pushed migrations made are not listed in the branch's diff as its own changes.
  A branch with no base (made by v0.3.x) only gets migrations and functions.
* **Reset** recreates the database from the branch's **own** recorded migration history, not from the parent's: the migrations the
  branch added itself survive, and so do those it got from the parent; tables, rows and any change made outside migrations do not.
  `migration_version` replays up to that version (it may be a version only the branch has) and `0` gives an empty database (no seed).
  The branch's history and the version are read and checked **before anything is removed**: an unknown version is refused with `400`
  and the branch is untouched. The old cluster is then removed (no final backup, archive purged: **a persistent branch loses its
  WAL and base backups on reset**, because the recreated cluster reuses the ref; take a backup first if you may want them) and a
  new one is created over the same registry row, with the same ref, id, name, Edge Functions, function secrets and saved settings. What can
  fail without touching the branch is checked first (the parent runs, a with_data branch can still get its data, the parent's
  credentials can be read). A later failure leaves the branch registered as `MIGRATIONS_FAILED` with `INIT_FAILED` project status under
  the same id, so it can be reset again or deleted. A schema-only branch keeps its credentials, its compute class and its schema
  base; a branch with data gets new credentials as at creation and a new base. Its lifetime is kept, with at least 15 minutes left, and
  the branch stays resolvable (its `status` is `CREATING_PROJECT` meanwhile). A branch that recorded no history, or whose state is
  not a working one (a failed creation, a failed merge) and whose cluster cannot be read, is reset from the parent's history as a new
  branch is, and the run log says which history was used. A working branch (`FUNCTIONS_DEPLOYED`, `MIGRATIONS_PASSED`) whose history
  cannot be read is **not** reset (`500`, nothing is removed): replaying the parent's history would drop the migrations the branch
  added itself; try again, or pass `force` to reset from the parent's history anyway. Recordings of Studio's merge page that were
  never merged are replayed as one: the newest runs, and every older recording and tracked migration before it is covered by it
  (`migration_version` cuts the history first, and a migration whose covering recording is cut away runs in full).
  A reset of a preview branch that a pull request built (a schema-only branch with a `github_branch_links` row) applies no stored seed of the parent, since the seed is the repository's, and clears `seeded_at` on the link (`ClearGitHubBranchLinkSeed`) once the rebuild works, so the next commit to the pull request applies `seed.sql` again; the run's log says so.
* **Diff** (`GET /v1/branches/{id}/diff`, `text/plain`) is **what the branch changed in its schema**, as SQL: the statements that would
  take the parent to the branch's schema where the branch moved away from its schema base. It is **empty** when nothing changed
  (Studio shows "No changes detected"), accepts `included_schemas` (comma separated; default: the user schemas and the policies and
  triggers on managed tables) and `pgdelta` (true or false: both give the same answer, there is one engine). A change the engine
  cannot express is a comment line `-- supavise:unsupported <what>`; a merge of such a diff fails and says what.

### The schema engine

`schemadiff` reads the catalog of a database into a `Snapshot` (objects with an identity, a canonical definition from `pg_get_*`, and the
objects each needs, from `pg_depend`) and plans the DDL between two snapshots: what is gone is dropped in reverse dependency order, then
what is new or changed is created or altered in dependency order, with the usual Migra/pg-delta behavior for the cases a plain
`ALTER` cannot do (a changed index, constraint, trigger or materialized view is dropped and created; a view whose columns only grow is
`CREATE OR REPLACE`d; a function with another result type is dropped and created; enum values can only be added; a column's default
is dropped and set again around a type change). Renames are not detected (a rename is a drop and a create, as in Migra: review a
diff that drops a table). It is used four ways: the **baseline** (the diff from an empty database), the **diff** of a branch (its
base to its schema), the **three-way merge** of a push (`Merge3`: what the parent changed since the base, minus what the branch
changed itself) and the **base** that moves with `Sync` (to where both sides agree, for the objects it does not model too) after a push or a merge. Properties tests
check: applying a plan to the source database makes it read like the target, for tables, columns, constraints, indexes, policies,
functions, triggers, views, materialized views, types, sequences, partitions, generated columns, identity columns, grants,
default privileges and comments (`schemadiff/integration_test.go`, a real Postgres).

Not modeled, and named in the output instead of dropped: aggregates, operators, casts, rules, event triggers, publications, foreign
tables and servers, range types, collations, extended statistics, inherited tables, column-level privileges (not the stock event
triggers and Realtime publications of a project, which every project has). Objects a user creates
*inside* a managed schema (a function in `auth`, a table in `storage`) are not read either; policies and triggers on the managed
tables are. Known limits of a plan: it does not drop and recreate the dependents of an object it changes (a view over a column
whose type changes makes the transaction fail and roll back, loudly), and a new enum value cannot be used by another statement of
the same transaction.

**Why not pg-delta.** The spike (v0.4.0) ran `@supabase/pg-delta` 1.0.0-alpha.58 (Node 20 or later) against two real databases: its
plan between live databases is good, and it has `snapshot` and `drift` commands, but `plan` takes two *live* databases, so the
three-way model (a stored base against a live branch) would need a Node program on its library API, which is alpha with breaking
changes between releases. Node is on a node only inside the Studio artifact, so pg-delta would be a new artifact class with its own
fetch, checksum, manifest and upgrade path, and the engine would depend on Studio being installed. The Go engine runs in the daemon,
stores its snapshots in the registry, is tested against a real Postgres on a development machine, and fails loudly (never falls back
to a list of migrations) when it cannot read or express something.

### Git-linked branches

`internal/github` builds a preview branch from a pull request (docs/github-integration.md) through two hooks of this package, so that the run, the events, the branch's status and `notify_url` stay the ones of every other operation:

* **`CreateInput.SchemaSource`** (types.go) builds a schema-only branch from something other than its parent. `Create` makes the branch's project as always (keys, ports, the parent's saved settings) and then calls `Schema` in place of the replay of the parent's migrations and seed, and `Functions` in the deploy step in place of the copy of the parent's Edge Functions. The branch gets no schema base (nothing it started from is the parent's), but it gets a function base: `doCreate` records the parent's Edge Function digests before it calls `Schema` (`recordParentFunctions`), the base a copy of them would have given, so that a push keeps a function the pull request changed and a merge deploys it without `force`. The run's `clone`, `configure`, `migrate` and `seed` steps are the source's to report, with `Run.Step`. It needs a schema-only branch.
* **`Drive(ctx, idOrRef, op, fn)`** (driven.go) starts a run on an existing branch whose steps `fn` performs, with the checks of a push (the branch is idle and running) and the ending of one (`FUNCTIONS_DEPLOYED` or `MIGRATIONS_FAILED`/`FUNCTIONS_FAILED`, events, `notify_url`). A caller that is not a branch (production) uses `StartRun` and ends the run with `Run.Finish`.
* **`WakeForDrive(ctx, idOrRef)`** (driven.go) resumes a branch whose project is paused (`INACTIVE`) and waits while it comes up; `Drive` refuses a branch that is not running, so the Git integration wakes first. **`ApplyOptions.Lock`** takes the project's apply lock (the one a merge holds) without `Atomic`'s single transaction.

A branch made for a pull request is created with `TTL` below zero, so it has no `expires_at`: it lives until the pull request closes and the integration deletes it. A branch somebody made with `--git-branch` keeps the lifetime `[branching] default_ttl` gives it.

### Delete, expiry

`DELETE /v1/branches/{id}` deletes at once: fleet tenants, units, data, registry row. A **non-persistent**
branch gets no final backup and its whole WAL archive and base backups are removed (a reset reuses the
ref). A **persistent** branch gets the lifecycle's final base backup when a backup backend is configured and
its archive stays, like any deleted project's. The same holds for the other two ways to delete a branch:
`DELETE /v1/projects/{branchRef}` and `DELETE /platform/projects/{branchRef}` (what a client that treats the
branch as a project sends) and `supavise projects delete <branchRef>` go through `Service.Delete`, so the
rules, the archive purge and the parent's `branch.deleted` event are the same everywhere. A project's
Storage objects in an S3 bucket (`[fleet] storage_backend = "s3"`) are emptied too, by delete and by reset
(`lifecycle.ObjectRemover`, `storagemigrate.ObjectRemover`); the file backend's directory has always gone with
the project, and a reset now removes it as well.

`?force=false` schedules the deletion after `soft_delete_grace_minutes` (60) **and pauses the branch**:
`preview_project_status` becomes `INACTIVE`, the one state in which Studio enables Restore. The pause is the
owner's, so a request does not wake it (a branch the node had paused for idleness becomes the owner's).
`POST /v1/branches/{id}/restore`, and a resume from the paused page (`POST /platform/projects/{ref}/restore`),
cancels the deletion and starts the branch again; Restore also starts a paused branch that has no deletion
scheduled, and answers 409 for one that is neither. A pause that fails leaves the deletion scheduled and the
branch running (`branch.deletion_pause_failed`). `supavise branches list` shows `deleted in 59m0s (restore
cancels)` in the EXPIRES column. `DELETE /v1/projects/{ref}/branches` deletes every branch of the project.

**Lifetime.** A branch has none unless the node asks for one: `[branching] default_ttl` is empty (never)
by default, as a hosted branch that is not tied to a pull request never expires, and `supavise branches create
--ttl 6h` gives one branch a lifetime whatever the default. A branch tied to a pull request lives until the pull
request closes (the GitHub integration deletes it). A node that sets `default_ttl` (`36h`, `7d`) gives a
non-persistent branch `expires_at = now + default_ttl` when it becomes ready; `PATCH` with `persistent: true`
clears it, `false` starts the default lifetime again (none without `default_ttl`). A branch made before this
release keeps the expiry it was given. `Service.Run(ctx)` sweeps every `sweep_interval_seconds` (60), at start
too; `supavise branches sweep [--dry-run]` does one pass by hand. The API shows the expiry as
`supavise_expires_at` and `supavise branches list` as a remaining time.

**Warning.** A branch with a custom or vanity hostname loses the hostname, its certificate and its DNS target
when it expires. When such a non-persistent branch is within `expiry_warning_hours` (24) of `expires_at`, the
sweeper records `branch.expiring` on the branch and its parent, and posts `{"type": "branch.expiring", ...}` to
the branch's `notify_url` in the envelope and with the Standard Webhooks headers of the run notifications
(`supavise.expires_at`, `supavise.hostnames`). Once per expiry: moving the expiry warns again.

### Idle pause

A **non-persistent** branch that nothing has used for `[branching] idle_pause_after` (30 minutes; `0` or `off`
never) is paused as a whole: Postgres, GoTrue and PostgREST stop, the status is `INACTIVE`, the data stays, and
the branch holds no memory. The first HTTP request or database connection starts it again. The loop is
`RunIdle` (`idle.go`): every `sweep_interval_seconds`, or a third of the period when that is shorter, it looks at
each branch homed on this server.

What counts as use: the branch was seen running less than the period ago (a restart of the daemon, a create,
a wake or a resume starts the clock again); an HTTP request to its API on any of its hosts in the period, or one
still being served, a WebSocket included (`reqstats.Counter.Last` and `Active`); a Management API call that ran
SQL on the branch in the period (Studio's SQL and table editors, MCP's `execute_sql` and `apply_migration`, a
migration the CLI pushed): it opens a session and closes it before it answers, so `api.Server.running` marks the
branch used with `reqstats.Counter.Touch`, which moves `Last` and counts no request; a database client the loop
finds connected (`pg_stat_activity`: a session running a statement now, or whose last statement ended less than the
period ago, such as the pooler's users or a `psql` that is still open). A session that opened and closed between two
sweeps leaves nothing to find, so a `psql -c` or a direct `supabase db push` against the database port alone is not
noticed (the loop looks every `sweep_interval_seconds`, or a third of the period when that is shorter). The roles of
GoTrue, PostgREST and Storage, `pg_cron` and the loop's own session are not clients, because what they do for a
client came in through the proxy, which counts it. A database that cannot be read leaves the branch running.

Never paused: persistent branches, branches scheduled for deletion, the default branch (the project itself),
a branch with an operation in flight (in this process, in another, an unfinished action run, or a merge into its
parent), and everything when the cluster has more than one server (below). Every pause is the node's own
`PauseWith(registry.PauseIdle)`; `supavise.project_pause` (migration 1520) records its reason, so a branch the
owner paused by hand (`supavise projects pause`, the dashboard, `supabase branches pause`) has none and **never
wakes by itself**. A pause event `branch.idle_paused` and a wake `branch.idle_woken` are recorded.

**Wake on a request.** The proxy holds a request that reaches a sleeping branch, after the keys have checked,
while `Service.Wake` starts it (one start per branch, shared by everyone who waits), up to
`idle_wake_timeout_seconds` (60); then it forwards. Past the bound, or when the start fails (not enough memory
on the node), the answer is 503 with `Retry-After`, and the start goes on without the request. A TLS handshake
for the host of a sleeping branch is allowed, because the wake comes after it.

**Wake on a Management API call.** A call that runs SQL on a sleeping branch (`database/query`, a migration, Studio's
SQL and table editors, MCP's `execute_sql`) wakes it the same way before it runs (`api.Server.running`), so an agent that
left a branch alone need not resume it first. A branch the owner paused answers 409 "Project is paused; restore it first"
as before.

**Wake on a database connection.** A sleeping branch's `127.0.0.1:<20000+3n>` port, where Postgres listened and
where the pooler and every other client dial, is held by a **wake gate** (`idlegate.go`). The first connection
that sends its first packet makes the gate stop listening so that Postgres can bind the port, start the branch,
open a connection to Postgres when it is up, replay the packet and pass the bytes both ways: the client sees a
slow connect. A connection that sends nothing wakes nothing. `lifecycle.Engine` waits, before it starts Postgres,
for nothing to listen on the port (`awaitDatabasePort`, at most three seconds), which is what lets a resume
made by the CLI or by the owner, in another process, take the port back from the gate. Limits of the gate: a
client that connects after the gate closed and before Postgres listens (the first seconds of the start) is
refused, and one whose own connect timeout is shorter than the start gives up; both reconnect. The pooler's
own limits on connecting to a database are Supavisor's.

**In a cluster nothing sleeps.** A request that reaches another server's proxy could not wake the branch (the
follower's registry is read-only) and its requests are not counted on the leader, so with more than one active
server `IdleSweep` pauses nothing and starts the branches that sleep (`branch.idle_woken`, `by: cluster`). A
follower's proxy has no `IdleWaker`.

### Placement

A branch is **homed on its parent's server** (`lifecycle.Engine.Create` copies the parent's `node_id`), so
that the two databases are on one machine. What reads or writes both through SQL works wherever they are, the
canonical ports of a project that runs on another server being forwarders to it: merge, push, diff and the
migration replay of a schema-only create or reset. What copies the parent's data directory does not: a create
or reset **with data** (a copy-on-write clone, or the parent's base backup restored on this server) is refused
with 409 before anything exists when the parent is homed on another server, naming it and saying what to do
(create the branch without data, or move the project here first: add a replica with `supavise replicas add`,
then `supavise projects failover <ref>`). A reset **with data** of a branch whose own cluster is on another server than the parent's data (the parent or the leader moved after the branch was made) is refused the same way, before the old cluster is removed; a reset without data works. Failover leaves branches behind (`internal/failover/README.md`): a branch has
no replica, so a server move does not move it, and a project move does not move the branches of the project.
A branch homed on a server that is gone cannot be deleted until the server is back.

## Limits and guards

`[branching] max_per_project` (10) and `max_total` (50): every branch is a cluster. Branch names are
git-branch-like (`A-Za-z0-9._/-`, 100 characters); `main` is reserved. `notify_url` must be http(s), and the
call refuses loopback, private, link-local, shared (100.64.0.0/10), benchmarking (198.18.0.0/15),
IETF-reserved (192.0.0.0/24, 240.0.0.0/4) and NAT64 (64:ff9b::/96) addresses at connect time unless
`allow_private_notify_urls` is set.

Limits:

* A merge moves migrations (or the recorded schema diff) and Edge Functions, never data, settings, Storage objects or function secrets.
* The schema engine covers the object classes listed in "The schema engine"; the rest is named in the output, not carried.
* A branch made by v0.3.x has no schema base, so its diff compares it with the parent as it is (additions and changes only) until a
  merge sets one. In a cluster, a v0.3.x server that runs an operation on a branch does not keep the base or the function base up
  to date (the next diff may list changes that already crossed, and Studio's merge then fails with an "already exists" and rolls
  back): upgrade both servers before using Studio's merge page.
* Re-encrypting `vault.secrets` under a key of the branch's own is not done (the pgsodium root key is shared).
* Paused cron jobs and disabled foreign servers are turned back on by hand (the SQL above); no `supavise` command does it.
* Egress is blocked only under systemd, covers the branch's Postgres unit only, and blocks WAL archiving to an
  `s3://` backend.
* Idle sleep applies on a single server only, and a sleeping branch's first seconds of waking refuse new
  database connections (Idle pause).
* The `deploy` step is where a create copies the parent's Edge Functions and stores the create body's secrets, where a push copies
  the parent's changed ones and a merge deploys the branch's; a step with nothing to do is `EXITED`.
* In a cluster, an operation a v0.3.x server runs leaves no action run (Studio's polls get 404 for it); the
  branch's status and `branch_detail` still follow it.
* `GET /v1/branches/{id}` reports the pooler (`pooler.<domain>`, session port, user `postgres.<ref>`) as the
  database address: Supavise has no direct database host.
* Two operations on one branch are excluded through the registry state and an in-process table, not a cross-process lock, so two processes starting one in the same instant can both proceed.
* The clone paths ran on APFS (`clonefile`), XFS (`reflink`) and ext4 (base backup); btrfs and OpenZFS
  block cloning were not run.

## Configuration (`[branching]`, or `SUPAVISE_BRANCHING_*`)

| Key | Default | Meaning |
|---|---|---|
| `default_ttl` | none | lifetime of a non-persistent branch (`36h`, `7d`); empty and `off` never expire |
| `idle_pause_after` | `30m` | a non-persistent branch nobody uses is paused after this; `0`/`off` disables |
| `idle_wake_timeout_seconds` | 60 | how long a request or a connection waits for a sleeping branch |
| `expiry_warning_hours` | 24 | a branch with a hostname is warned about this long before it expires |
| `default_class` | `micro` | class of a branch without `desired_instance_size` |
| `max_per_project`, `max_total` | 10, 50 | caps |
| `sweep_interval_seconds` | 60 | |
| `clone` | `auto` | `backup` always restores from the base backup |
| `soft_delete_grace_minutes` | 60 | |
| `allow_private_notify_urls` | false | |
| `keep_cron_jobs` | false | keep the parent's pg_cron jobs active in a branch with data (egress stays denied); `--allow-egress` keeps them per branch and opens egress |
| `disk_reserve_mb` | 2048 | free disk that must remain after a with_data create or reset, on top of 1.2 times the parent's data |

## CLI

```
supavise branches list <project-ref> [--json]
supavise branches create <project-ref> <name> [--with-data [--allow-egress]] [--persistent] [--ttl 6h|off] [--size micro]
                      [--git-branch b] [--seed-file f] [--notify-url u] [--no-wait] [--json]
supavise branches get|delete|restore|diff <id|ref|name --project <ref>>   # delete --schedule (pauses; restore resumes)
supavise branches merge|reset|push <id|ref|name> [--project <ref>] [--migration-version v] [--force] [--no-wait]
supavise branches runs <id|ref|name> [--project <ref>] [--limit 20] [--json]    # the action runs of a branch
supavise branches logs <id|ref|name> [run-id] [--project <ref>] [--json]         # the newest run's log, or run-id's
supavise branches notify-secret [--rotate]                                       # the Standard Webhooks signing secret
supavise branches sweep [--dry-run]
supavise branches seed set|get|clear <project-ref> [--file seed.sql]
```

Like `supavise projects`, these open the node in the calling process (system cluster running, run as the
`supavise` user).

## Tests

The `ci.yml` job `test` runs the unit tests. The
`linux.yml` job `branching-xfs` runs `tests/linux/branching-xfs.sh` (the whole branching scenario on a
loop-file XFS with `reflink=1` and on ext4) and `tests/linux/branching-egress.sh` (egress isolation under
systemd: the denied unit cannot reach a non-loopback host while the parent can, the allow list is exactly
127.0.0.1 and ::1, `--allow-egress` reaches the host, the restriction survives pause, resume and reset). After
the clone runs, `tests/linux/branching/runs.sh` starts `TestServe` (the Management API over real clusters) and
checks the action runs over HTTP: seven steps, logs, the owner rule, `FUNCTIONS_DEPLOYED`, a forced divergence
as a `DEAD` migrate step, and the `notify_url` envelope and its signature at a python receiver. The
`tests/linux/branching/content.sh` (sourced by `branching-xfs.sh`) checks the content of a branch over the Management API: a parent
built only with `execute_sql` (a table, a policy on `storage.objects`, a trigger on `auth.users`, an extension), reset, the dashboard
merge flow over `curl`, and parent drift; `functions-smoke.sh` and `settings-smoke.sh` check Edge Functions and saved settings on a
branch. `internal/branching/content_pg_test.go` and `schemadiff/integration_test.go` run the same flows over real databases on the
Postgres of `SUPAVISE_TEST_DATABASE_URL`. The
integration tests in `internal/branching` run real clusters and are skipped unless `SUPAVISE_TEST_UNPACKED`
points at the unpacked artifacts directory. `tests/linux/branching/lifecycle.sh` starts `TestServeLifecycle` (the
same node with the edge proxy and an idle period of 8 seconds) and checks over HTTP and `psql`: a scheduled
delete pauses and restore resumes, an idle branch pauses, a request through the proxy and a connection to its
port wake it, an owner's pause stays, a persistent branch and the parent never sleep, and a delete through the
project routes purges the archive. The unit tests (`idle_test.go`, `lifecycle_test.go`, `placement_test.go`) use
a fake clock, a fake `reqstats` and a fake engine.

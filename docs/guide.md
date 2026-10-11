# Using Supavise

This guide picks up after the install in the [README](../README.md#get-started). Your Supavise server is called a node, and a second server that joins the first is another node. For server and AWS options in depth, see the [deploy guide](../deploy/README.md).

- [Claim the node and sign in](#claim-the-node-and-sign-in)
- [Create a project and connect an app](#create-a-project-and-connect-an-app)
  - [New tables need a grant](#new-tables-need-a-grant)
- [Use the Supabase CLI](#use-the-supabase-cli)
- [Give an agent or a preview its own branch](#give-an-agent-or-a-preview-its-own-branch)
- [Connect an AI tool to a project](#connect-an-ai-tool-to-a-project)
- [Check a project with the advisors](#check-a-project-with-the-advisors)
- [Back up and restore](#back-up-and-restore)
- [Move a project from hosted Supabase](#move-a-project-from-hosted-supabase)
- [Move a project off Supavise](#move-a-project-off-supavise)
- [Monitor the node](#monitor-the-node)
- [What the dashboard shows differently](#what-the-dashboard-shows-differently)
- [Read replicas and failover](#read-replicas-and-failover)
- [Updates and maintenance](#updates-and-maintenance)
- [How Supavise compares](#how-supavise-compares)
- [Sizing and cost](#sizing-and-cost)
- [FAQ](#faq)

## Claim the node and sign in

The installer prints the dashboard URL and a one-time claim token, valid for 72 hours. On AWS, the stack outputs give you both. Open `https://api.<domain>/claim`, enter the token to create the first administrator, then sign in to the dashboard at `https://studio.<domain>`.

Invite your team from the organization's **Team** page, or on the server:

```bash
sudo -u supavise supavise users invite dev@example.com --role developer
```

## Create a project and connect an app

Create a project in the dashboard. Copy its publishable key from the project's API key settings, then connect with `supabase-js`:

```js
import { createClient } from '@supabase/supabase-js'

const supabase = createClient('https://<ref>.api.<domain>', '<publishable key>')
```

`<ref>` is the project ID, shown in the dashboard URL and in **Project Settings**. The URL is the only part that differs from supabase.com. Auth, REST, Realtime, Storage and Edge Functions all answer on it.

To serve a project on your own hostname or a short `<name>.api.<domain>` address, use **Project Settings**, **Custom Domains**, or `supabase domains` (see the deploy guide's [custom domains](../deploy/README.md#custom-domains-and-vanity-subdomains)).

### New tables need a grant

As on hosted Supabase, a table you create in `public` is not reachable through the Data API (REST and GraphQL) until you grant it. If you skip the grant, a request to `/rest/v1/<table>` answers `42501` (permission denied). Grant each role what it needs, then turn on row level security:

```sql
create table public.todos (id bigint generated always as identity primary key, title text not null);
grant select on public.todos to anon;
grant select, insert, update, delete on public.todos to authenticated;
grant all on public.todos to service_role;
alter table public.todos enable row level security;
```

A serial column also needs `grant usage, select on sequence public.todos_id_seq to authenticated;`, and a function that only signed-in users may call through `rpc` needs `revoke execute on function public.my_function() from public, anon;`, because Postgres gives `execute` on a new function to everyone (`public`) and the default privileges below cannot take that away. A migration written this way runs unchanged on hosted Supabase.

**New projects.** A project created from the dashboard follows the **Automatically expose new tables** box, which starts unchecked. `supavise projects create` and `POST /v1/projects` revoke the default grants too. To keep them, check the box, pass `--expose-new-tables`, or send `data_api_revoke_default_privileges: false` to `POST /platform/projects`. Behind the box, Supavise runs this once, after the project's database starts:

```sql
alter default privileges for role postgres in schema public revoke select, insert, update, delete on tables from anon, authenticated, service_role;
alter default privileges for role postgres in schema public revoke usage, select on sequences from anon, authenticated, service_role;
alter default privileges for role postgres in schema public revoke execute on functions from anon, authenticated, service_role;
alter default privileges for role postgres in schema public revoke execute on functions from public;
```

The two function statements remove the grants the image gave new functions for those three roles. They don't make functions private: `public` keeps `execute` on every new function, as on hosted. A branch follows its parent. A restore as a new project follows its backup: if the backup was taken before the node upgraded, the new project gets the revoke at its first pass. A restore in place, from the dashboard or with `supavise backups restore`, follows its backup too. The restored database has the default privileges it had when the backup was taken, and the project's recorded choice is set to match. A backup taken before the node upgraded gets the revoke once, right after the restore. If you rolled the revoke back with SQL and then restore from such a backup, the restore revokes once more, so run your grants again afterward. After the restore, SQL you run keeps its effect.

**Existing projects.** Hosted applies the same change to its existing projects on 2026-10-30. When you upgrade a node, Supavise does it once to every project that has no recorded choice, after the upgrade ends (a project that is paused gets it when it resumes). Tables that exist keep their grants and keep answering. Supavise records the change in the registry, not in your database, so if you roll it back with the SQL below, no restart or later upgrade revokes it again. The change runs on the project's primary; replicas receive it by replication.

To bring the old behavior back, run this as `postgres`. The first block restores the defaults for new objects, the second grants the objects created since the change:

```sql
alter default privileges for role postgres in schema public grant select, insert, update, delete on tables to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant usage, select on sequences to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant execute on functions to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant execute on functions to public;
```

```sql
grant select, insert, update, delete on all tables in schema public to anon, authenticated, service_role;
grant usage, select on all sequences in schema public to anon, authenticated, service_role;
grant execute on all functions in schema public to anon, authenticated, service_role;
```

To keep one table private afterward, run `revoke all on table public.my_table from anon, authenticated, service_role;`. Supavise revokes the defaults for the role `postgres` only: a table created by another role keeps the grants that role's defaults give.

**GraphQL.** Hosted no longer enables `pg_graphql` in new projects, and neither does Supavise. Until you turn it on in **Database**, **Extensions** (or with `create extension pg_graphql;`), `/graphql/v1` answers that it is not enabled. A project that already has the extension keeps it.

**Other creation fields.** `data_api_exposed_schemas` sets the schemas PostgREST serves (default `public` and `graphql_public`). An empty list is the Data API turned off: a node cannot switch PostgREST off, so the project exposes only `graphql_public`, which holds no table. `data_api_use_api_schema: true` creates a schema `api` that the Data API roles may enter and serves it, as the **Switch the default API schema** dialog in the dashboard's Data API settings does by hand.

## Use the Supabase CLI

The Supabase CLI reaches a node through a profile file. Print the profile on the node, then save the output as `supavise-profile.yaml` on your machine:

```bash
sudo -u supavise supavise api profile --format yaml
```

Create an access token in the dashboard, under **Account settings**, **Access Tokens**, then:

```bash
supabase --profile ./supavise-profile.yaml login --token sbp_...
supabase --profile ./supavise-profile.yaml link --project-ref <ref>
supabase --profile ./supavise-profile.yaml db push
supabase --profile ./supavise-profile.yaml functions deploy --use-api
```

## Give an agent or a preview its own branch

A branch is a full project with its own database, keys and URL. Create one with data copied from the parent:

```bash
supabase --profile ./supavise-profile.yaml branches create agent-task-42 --project-ref <ref> --with-data
supabase --profile ./supavise-profile.yaml branches get agent-task-42 --project-ref <ref>
```

`branches get` prints the branch's connection details. A branch with data needs the Owner or Administrator role; Developers create schema-only branches (leave out `--with-data`). A branch deletes itself after 7 days unless you mark it persistent.

The same branch is one call to `POST /v1/projects/<ref>/branches` with `{"branch_name": "agent-task-42", "with_data": true}`. The Supabase MCP server's branch tools work when you start it with `--api-url https://api.<domain>` and without `--project-ref`.

## Connect an AI tool to a project

An MCP client such as Claude Code, Cursor, VS Code, Codex or the Claude desktop connector signs in to your node with OAuth, so you don't create a personal access token. In Studio, open **Connect**, then **MCP**. The panel shows the URL of your node's MCP server:

```
https://api.<domain>/mcp?project_ref=<ref>
```

Add it to your client with the snippet the panel shows. For Claude Code:

```bash
claude mcp add --scope project --transport http supabase "https://api.<domain>/mcp?project_ref=<ref>"
```

Then start `claude`, run `/mcp` and choose **Authenticate**. The client opens Studio's "Authorize API access" page. Sign in to Studio if it asks, pick the organization and approve. The client keeps working with that organization's projects until you revoke it.

- **Who can approve.** Owners and Administrators of the organization. A Developer or Read-only member who opens the link cannot approve.
- **What the client can do.** It acts as the person who approved, in the one organization they picked, with that person's current role: if the role is reduced, the client loses the same rights at once. Each client also asks for scopes (for example `database:read` or `projects:write`), and the page lists them before you approve. A client cannot create organizations, read your profile or call the dashboard's own endpoints.
- **Read-only and narrower sessions.** Add `&read_only=true` to the URL and the client gets no tools that write, and its SQL runs read-only. `&features=docs,database` limits the tools to those feature groups. Leave out `project_ref` and the client works across the organization's projects, with account tools such as `list_projects`.
- **Revoke.** In Studio, open the organization's **OAuth Apps** page, then **Authorized**, and choose **Revoke** next to the client. The client's next call gets a 401, which MCP clients read as the signal to refresh or sign in again. On the server, `sudo -u supavise supavise oauth grants list` shows the grants, and `supavise oauth grants revoke --user <email>` (or `<id>`, `--org`, `--app` or `--all`; `--yes` skips the confirmation) ends them. Removing a user with `supavise users remove` ends theirs.
- **Tokens.** An access token lasts 1 hour. A refresh token lasts 90 days and each use renews it, so a client that you use at least every 90 days doesn't ask you to sign in again. The client refreshes on its own.
- **Your own clients.** Owners and Administrators can publish an app with fixed scopes and a client secret on the **OAuth Apps** page, **Published** tab. Clients that register themselves (all the tools above) are listed under **Authorized** once someone approves them.
- **Personal access tokens still work.** A client that can set a header can send `Authorization: Bearer sbp_…` to the same URL.

Two things to know about the server. Its documentation search, `search_docs`, calls `supabase.com` from your node, as on hosted Supabase. And the `skip_elicitations` option is accepted and ignored, because the endpoint cannot ask the client to confirm a cost.

To turn OAuth sign-in off, set `disable_oauth = true` in the `[api]` section of `/etc/supavise/config.toml` and restart `supavise.service`: the MCP URL, the sign-in endpoints and the OAuth Apps page then stop working, and Studio is no longer given the URL. Personal access tokens and the dashboard keep working. [Design, section 13](design.md#13-oauth-sign-in-and-the-remote-mcp-endpoint) describes how it works.

## Check a project with the advisors

The advisors list problems in a project's schema and settings. Studio shows them under **Advisors**, on the **Security Advisor** and **Performance Advisor** pages, and in the advisor panel of the project home. The Supabase CLI and an MCP client read the same lists:

```bash
supabase --profile ./supavise-profile.yaml db advisors --linked --type security --level warn
```

In an MCP client, ask for `get_advisors` with `type` `security` or `performance`. Every role that can see the project can read the advisors.

Supavise runs the checks when you ask, in the project's own database, as Studio's self-hosted mode does. The checks for what the Data API exposes, such as a table without row level security or a view that runs with its owner's rights, look only at the schemas that the project's Data API serves (the PostgREST setting `db_schema`), so a table in a schema that is not exposed is not reported. Supavise also reports four findings from the Auth settings: leaked password protection off, fewer than two MFA methods, an email OTP that lives longer than an hour, and an Auth connection pool set in connections and not in percent. A new Supavise project starts with an OTP expiry of an hour, as a new hosted project does, so it does not report the third finding; a project that an earlier release created keeps the day it has run on and reports it until you set `mailer_otp_exp` to 3600 or less. The first two findings show on a new project until you turn on leaked password protection and a second MFA method.

- **How fresh.** Supavise keeps a result for 10 seconds. A migration, or a statement that creates, alters or drops something, clears it when it reaches the project through the Management API: Studio's SQL editor and an MCP client's `execute_sql` and `apply_migration` do that. A change over a direct or pooler connection, such as `supabase db push`, shows within 10 seconds. A project runs one check at a time, and a few projects at once, so a burst of changes waits for the check under way and then shares one new check.
- **Time limit.** The checks stop after 30 seconds. If the dashboard answers "took longer than 30 seconds", run them again when the database is less busy.
- **Rules.** If you turn on the **Disable Advisor rules** feature preview in Studio, you can disable a check for the whole project. Owners and Administrators change the rules; everyone reads them. A disabled check leaves the dashboard, the CLI and MCP lists.
- **What is missing.** Hosted Supabase also reports health checks (error rates of the API, Auth, Storage and Edge Functions, and infrastructure alerts). Supavise keeps no logs or metrics for them, so the checks answer "unavailable", which Studio and the MCP server leave out. The compliance checks (SSL enforcement, connection logging, network restrictions, point-in-time recovery, password policy, backup admin) and the Postgres security-patch check are not reported: they look like hosted's compliance checks, and Supavise has no add-on to key them on.

## Back up and restore

Backups start on their own. Every project archives its write-ahead log (WAL) continuously and takes a nightly base backup, and the same nightly run copies its Storage files and Edge Functions.

Backups go to local disk by default. To keep them off the server, pass `--s3-bucket` and `--s3-region` at install (see the deploy guide's [flags](../deploy/README.md#flags)). The AWS stack uses its own bucket.

**Restore from the dashboard.** Open **Database > Backups > Point in time**, pick a time within the retention window (7 days by default) and confirm. The project shows RESTORING, then returns to normal; anything written after that time is lost. Only Owners and Administrators can restore. The API equivalent is `POST /v1/projects/<ref>/database/backups/restore-pitr`.

**Restore to a new project from the dashboard.** Open **Database > Backups > Restore to new project**, pick a backup or a point in time, name the new project and give it a database password. The dashboard shows the clone as In progress while the server restores it, which takes minutes to hours depending on the size; the new project appears in the same organization when it is done. The copy has the database as of that moment. As on hosted Supabase, it does not carry the source's Storage objects, Edge Functions or Auth settings, and it gets new API keys. One clone of a project runs at a time, and the new project is created on the server that handled the request. Only Owners and Administrators can start one. A server without backups hides the page. If the restart of `supavise` interrupts a clone, the dashboard shows it as Failed; start it again.

**Restore on the server**, in place or as a copy under a new project:

```bash
sudo -u supavise supavise backups list <ref>
sudo -u supavise supavise backups restore <ref> --to 2026-10-06T14:30:00Z --as <newref>   # a copy at that time
sudo -u supavise supavise backups restore <ref> --to latest --force                      # in place
```

Good to know:

- Don't restart `supavise` while a project shows RESTORING.
- A restore needs free disk for a second copy of the project's data. The previous data stays on the server until the project's next successful restore.
- If a restore fails, the project shows RESTORE_FAILED and keeps its original data where possible. Restore again, or pause and resume the project. The [backup docs](../internal/backup/README.md) cover the details.

## Move a project from hosted Supabase

You move a project in five parts: the database (roles, schema, data and Auth users), the files in Storage, the Edge Functions, the settings, and the keys your app holds. The database part uses the three dumps of Supabase's [backup and restore guide](https://supabase.com/docs/guides/platform/migrating-within-supabase/backup-restore), restored through the node's pooler. The hosted project keeps running until you switch your app, so you can rehearse the move on a throwaway project first.

**Before you start**

- On your machine, install the Supabase CLI, Docker (`supabase db dump` runs `pg_dump` in a container), `psql` 17, `jq` and `curl`.
- Run the CLI commands in a directory that holds `supabase/config.toml` (`supabase init` creates it) with `major_version = 17` under `[db]`. The dump then runs with Postgres 17 tools, which read hosted projects on Postgres 15 and 17. The node runs Postgres 17.
- Check that your machine can reach port 5432 of the node.
- If a hosted project uses an extension that the node's Postgres doesn't ship, `CREATE EXTENSION` stops the restore. List the extensions on both sides and compare:

  ```bash
  psql "$OLD_DB_URL" --command "select extname from pg_extension order by 1"
  psql "$NEW_DB_URL" --command "select name from pg_available_extensions order by 1"
  ```

- Stop writes to the hosted project before the dump you restore from. Writes made after the dump don't move.

**1. Create the project with your keys**

The commands in this step save credentials of the hosted project in files: its legacy JWT secret, its `anon` and `service_role` keys and, in `api-keys.json`, its `sb_secret_` keys too. Whoever reads them can act as `service_role` on the hosted project. Work in a directory that only you and the `supavise` user can read, and delete it after the create command. The directory is group `supavise` with the setgid bit, so the files in it get that group, and the umask leaves them readable by the group and by no one else:

```bash
KEYS_DIR=$(mktemp -d /tmp/migrate.XXXXXX)
sudo chgrp supavise "$KEYS_DIR" && sudo chmod 2750 "$KEYS_DIR"
cd "$KEYS_DIR" && umask 037
```

If you want your app to keep the `anon` and `service_role` keys it holds, create the project with the hosted project's legacy JWT secret. GoTrue, PostgREST, Realtime and Storage then accept those keys, and users keep their sessions. Copy the secret from **Project settings**, **JWT keys**, **Legacy JWT secret** in the hosted dashboard, or ask the Management API:

<!-- migrate-smoke: jwt-secret -->
```bash
curl -fsS -H "Authorization: Bearer $OLD_TOKEN" "$OLD_API/v1/projects/$OLD_REF/postgrest" | jq -r .jwt_secret > jwt-secret.txt
```

A project that exists already takes the secret in the dashboard: **Project settings**, **JWT keys**, **Legacy JWT secret**, **Change legacy JWT secret**, **Create my own secret**. The node signs the `anon` and `service_role` keys again with it, so the hosted keys then work only if the project's ref is the hosted one.

If the hosted project has revoked its legacy secret, there is nothing to copy. Create the project without the three key flags below: the node then generates new keys, and your app needs them.

To keep the keys byte for byte, save the two legacy keys in files as well (**Project settings**, **API keys**, **Legacy API keys**, or the API):

<!-- migrate-smoke: legacy-keys -->
```bash
curl -fsS -H "Authorization: Bearer $OLD_TOKEN" "$OLD_API/v1/projects/$OLD_REF/api-keys?reveal=true" > api-keys.json
jq -r '.[] | select(.type == "legacy" and .name == "anon") | .api_key' api-keys.json > anon-key.txt
jq -r '.[] | select(.type == "legacy" and .name == "service_role") | .api_key' api-keys.json > service-role-key.txt
```

Create the project on the node. The command runs as the `supavise` user, which reads the files through the group of the directory. Set `NEW_REF` to the hosted project's ref to give the project the same ref: the proxy then accepts the hosted keys because their `ref` claim names the project. With another ref, only the two key files make the node accept them. Set `NEW_SIZE` to a compute size (`supavise projects sizes` lists them).

<!-- migrate-smoke: create -->
```bash
sudo -u supavise supavise projects create --name "$NEW_NAME" --ref "$NEW_REF" --size "$NEW_SIZE" \
  --jwt-secret-file jwt-secret.txt --anon-key-file anon-key.txt --service-role-key-file service-role-key.txt --show-keys
```

The command prints the new project's database password. The project also has its own publishable and secret keys (`sb_...`), because hosted keys of that kind can't be imported: if your app uses them, switch it to the new ones.

Delete the files now, with the directory:

```bash
cd - && rm -rf "$KEYS_DIR"
```

**2. Set the connection strings**

Use the session pooler of each side (port 5432). For hosted, copy the string from the **Connect** panel and put in the database password (reset it under **Database settings** if you don't have it). For the node, the user is `postgres.<ref>`:

```bash
export OLD_DB_URL='postgresql://postgres.<hosted ref>:<hosted password>@<hosted pooler host>:5432/postgres'
export NEW_DB_URL='postgresql://postgres.<ref>:<node password>@pooler.<domain>:5432/postgres'
```

**3. Dump and restore the database**

<!-- migrate-smoke: dump -->
```bash
supabase db dump --db-url "$OLD_DB_URL" -f roles.sql --role-only
supabase db dump --db-url "$OLD_DB_URL" -f schema.sql
supabase db dump --db-url "$OLD_DB_URL" -f data.sql --use-copy --data-only -x "storage.buckets_vectors" -x "storage.vector_indexes"
```

<!-- migrate-smoke: restore -->
```bash
sed -i.bak -E '/^(GRANT|REVOKE) .* ON PARAMETER /s/^/-- /' roles.sql
psql --single-transaction --variable ON_ERROR_STOP=1 \
  --command 'alter default privileges in schema public revoke all on tables from anon, authenticated, service_role' \
  --command 'alter default privileges in schema public revoke all on functions from anon, authenticated, service_role' \
  --command 'alter default privileges in schema public revoke all on sequences from anon, authenticated, service_role' \
  --file roles.sql --file schema.sql \
  --command 'SET session_replication_role = replica' \
  --file data.sql --dbname "$NEW_DB_URL"
```

The `sed` command comments out the lines of `roles.sql` that grant privileges on configuration parameters, such as `GRANT SET ON PARAMETER "log_min_messages" TO "supabase_realtime_admin"`. The dump lists them because Supabase's own roles hold them. The `postgres` role can't grant them, so without the command the restore stops with `permission denied for parameter log_min_messages`. Those roles exist on the new project with their grants, so the lines add nothing. The original file stays as `roles.sql.bak`.

The three `alter default privileges` commands keep your access rules. The dump only adds grants: it never takes one away, so the project must not hand out grants that hosted doesn't. A project created with `--expose-new-tables`, or rolled back with the SQL in [New tables need a grant](#new-tables-need-a-grant), gives a new table, function or sequence in `public` `ALL` for `anon`, `authenticated` and `service_role` from its default privileges. There, a table that you locked down on hosted (`revoke all ... from anon`) would be open again through the Data API without the commands. A project created as in step 1 has the `select`, `insert`, `update`, `delete` and `execute` defaults revoked already, and the commands clear what is left of them (`truncate`, `references`, `trigger` and `maintain` on tables, `update` on sequences), so that step 8 finds no grant that hosted doesn't have. The commands run in the restore's transaction, so a failed restore leaves the default privileges as they were. The dump lists the grants of each object and sets the default privileges of your `postgres` role again at its end, so the project ends up with the grants it had on hosted. Step 8 compares them.

The restore runs in one transaction, so a failure leaves the project as it was. Fix the cause and run the restore again. If psql stops, look for the message:

- **`relation "..." does not exist` or `column "..." of relation "..." does not exist`, in the `auth` or `storage` schema.** If you created the project a moment ago, wait until Auth and Storage have set up their schemas and run the restore again. Otherwise the hosted project runs a newer Auth or Storage than the node. Update the node first (`sudo supavise upgrade`), or delete that table's `COPY` block from `data.sql` (down to the `\.` line) and accept that its rows don't move. `sudo -u supavise supavise projects get <ref> --json | jq .versions` shows the node's versions.
- **`permission denied`, on a line that sets an owner to `supabase_admin` or on a `GRANT` to `cli_login_postgres`.** Comment that line out.
- **`permission denied for parameter ...`.** The line is a `GRANT` or `REVOKE` on a configuration parameter that the `sed` command did not match. Comment that line out.
- **Any error in the `supabase_functions` schema.** The hosted project uses Database Webhooks. Turn them on for the new project (**Integrations**, **Database Webhooks**) and run the restore again.

The dumps carry Auth users with their password hashes, identities, MFA factors and refresh tokens, so users sign in with the same passwords and refresh the sessions they have. Supabase's guide also lists optional dumps of the migration history (`--schema supabase_migrations`); they work against the node the same way.

**4. Bring back what the dump leaves out**

The dump creates the `pg_cron` extension but not its jobs. Create the jobs again from the hosted project's list (a job that was switched off comes back switched on):

<!-- migrate-smoke: cron -->
```bash
psql "$OLD_DB_URL" --no-align --tuples-only --command "select case when jobname is null then format('select cron.schedule(%L, %L);', schedule, command) else format('select cron.schedule(%L, %L, %L);', jobname, schedule, command) end from cron.job" > cron-jobs.sql
psql "$NEW_DB_URL" --file cron-jobs.sql
```

Then handle the rest by hand:

- **Passwords of your own login roles.** `roles.sql` creates the roles without passwords. Set each one: `alter role "app_user" with password '<new password>';`.
- **Realtime publications.** `schema.sql` adds tables to `supabase_realtime`. Check **Database**, **Publications** and add what is missing.
- **Triggers and policies you added to `auth` or `storage`.** The dump leaves them out. Supabase's guide recreates them from `supabase db diff --linked --schema auth,storage`, run against the hosted project.
- **Vault secrets and the pgsodium root key.** They don't move. The dumps leave the `vault` and `pgsodium` schemas out, each cluster on the node has its own root key, and the node refuses `PUT /v1/projects/<ref>/pgsodium`, the call that Supabase's guide uses to copy the key. Create the secrets again with `vault.create_secret`.

**5. Copy the settings**

Read the Auth, PostgREST, Storage and Realtime settings from hosted and send them to the node with a personal access token for each side. Do this before you copy the files: the Storage settings hold the largest upload the project accepts (`fileSizeLimit`), which is 50 MiB on a new project. Remove the secrets first: hosted reports them as hashes, which the node would take as new values. Set the secrets again in the dashboard (OAuth client secrets, the SMTP password, hook secrets, the captcha secret and SMS provider keys).

<!-- migrate-smoke: settings -->
```bash
secrets='^(external_.*_secret|hook_.*_secrets|nimbus_oauth_client_secret|security_captcha_secret|sms_messagebird_access_key|sms_textlocal_api_key|sms_twilio_auth_token|sms_twilio_verify_auth_token|sms_vonage_api_secret|smtp_pass|jwt_secret)$'
for path in config/auth postgrest config/storage config/realtime; do
  curl -fsS -H "Authorization: Bearer $OLD_TOKEN" "$OLD_API/v1/projects/$OLD_REF/$path" |
    jq --arg re "$secrets" 'with_entries(select(.key | test($re) | not))' |
    curl -fsS -X PATCH -H "Authorization: Bearer $NEW_TOKEN" -H 'Content-Type: application/json' --data @- "$NEW_API/v1/projects/$NEW_REF/$path" > /dev/null
done
```

The node ignores a setting it doesn't know. If it answers 400, the message names a setting it refuses (for example a hook URL that isn't HTTPS): fix or remove that setting and send again.

**6. Copy the files in Storage**

The restore brought the buckets and the list of objects. Copy the files. This loop reads that list from the new database, downloads each file from the hosted project and uploads it to the node, with its content type and cache control. Set the URLs and the `service_role` keys of both projects first (a hosted URL looks like `https://<hosted ref>.supabase.co`, the node's like `https://<ref>.api.<domain>`):

<!-- migrate-smoke: storage -->
```bash
enc() { jq -rn --arg p "$1" '$p | split("/") | map(@uri) | join("/")'; }
rm -f storage-failed.txt
psql "$NEW_DB_URL" --no-align --tuples-only --field-separator $'\t' --command "select bucket_id, name, coalesce(metadata->>'mimetype', 'application/octet-stream'), coalesce(metadata->>'cacheControl', 'max-age=3600') from storage.objects where metadata is not null order by 1, 2" |
while IFS=$'\t' read -r bucket name mime cache; do
  path="$bucket/$(enc "$name")"
  if curl -fsS "$OLD_URL/storage/v1/object/authenticated/$path" -H "apikey: $OLD_KEY" -H "Authorization: Bearer $OLD_KEY" |
    curl -fsS -o /dev/null -X POST "$NEW_URL/storage/v1/object/$path" -H "apikey: $NEW_KEY" -H "Authorization: Bearer $NEW_KEY" \
      -H "x-upsert: true" -H "Content-Type: $mime" -H "Cache-Control: $cache" --data-binary @-; then
    echo "copied $bucket/$name"
  else
    echo "$bucket/$name" >> storage-failed.txt
  fi
done
if [ -s storage-failed.txt ]; then echo "Not copied:"; cat storage-failed.txt; false; fi
```

The loop goes on after an object fails and lists the failures at the end. If an upload answers 413, the object is larger than the project's `fileSizeLimit`: check that step 5 copied the Storage settings, raise the limit if the hosted value is lower than the object, and run the loop again (it copies every object once more). An object name with a tab or a line break breaks the loop, and curl holds one object in memory while it uploads. For a large bucket, the script in Supabase's guide, which works against both URLs, copies in batches.

**7. Deploy the Edge Functions and set the secrets**

Download each function from the hosted project (after `supabase login` with a hosted token) and deploy it to the node with the profile and token from [Use the Supabase CLI](#use-the-supabase-cli):

```bash
supabase functions list --project-ref <hosted ref>
supabase functions download <slug> --project-ref <hosted ref>
supabase --profile ./supavise-profile.yaml functions deploy --use-api --project-ref <ref>
```

The download doesn't include import maps or `deno.json`, so add them back. Set `verify_jwt` for each function in `supabase/config.toml` (`[functions.<slug>]`) before you deploy, because the download doesn't carry it either. Secret values can't be read from hosted: `supabase secrets list` shows names only. Set each value again:

```bash
supabase --profile ./supavise-profile.yaml secrets set --env-file ./prod.env --project-ref <ref>
```

**8. Check, then switch your app**

Compare the grants of `anon`, `authenticated`, `service_role` and `PUBLIC` (shown as `-`) on the tables, views, sequences and functions of `public`. The command prints nothing when both projects agree. A line that starts with `<` is a grant that only the first project has, and one that starts with `>` a grant that only the second has: fix the difference with `grant` or `revoke`.

<!-- migrate-smoke: grants -->
```bash
grants="select format('%s %s %s %s', kind, name, grantee, privilege) from (
  select 'table' as kind, c.oid::regclass::text as name, a.grantee::regrole::text as grantee, a.privilege_type as privilege
  from pg_class c join pg_namespace n on n.oid = c.relnamespace, aclexplode(c.relacl) a
  where n.nspname = 'public' and c.relkind in ('r', 'p', 'v', 'm', 'f', 'S')
    and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.deptype = 'e')
  union all
  select 'function', p.oid::regprocedure::text, a.grantee::regrole::text, a.privilege_type
  from pg_proc p join pg_namespace n on n.oid = p.pronamespace, aclexplode(p.proacl) a
  where n.nspname = 'public'
    and not exists (select 1 from pg_depend d where d.classid = 'pg_proc'::regclass and d.objid = p.oid and d.deptype = 'e')
) g where grantee in ('anon', 'authenticated', 'service_role', '-') order by 1"
psql "$OLD_DB_URL" --no-align --tuples-only --command "$grants" > grants-old.txt
psql "$NEW_DB_URL" --no-align --tuples-only --command "$grants" > grants-new.txt
diff grants-old.txt grants-new.txt
```

Then compare row counts, sign in as a user and read a table with RLS as your app does (`ANON_KEY` is the anon key your app holds):

```bash
psql "$NEW_DB_URL" --command "select count(*) from auth.users"
curl -fsS "$NEW_URL/auth/v1/token?grant_type=password" -H "apikey: $ANON_KEY" -H 'Content-Type: application/json' -d '{"email":"<user email>","password":"<password>"}' | jq .user.email
curl -fsS "$NEW_URL/rest/v1/<table>?select=id&limit=1" -H "apikey: $ANON_KEY" -H "Authorization: Bearer $ANON_KEY"
```

Then change the URL in your app to `https://<ref>.api.<domain>`. If you serve the node's project on the same hostname your app already uses (see [custom domains](../deploy/README.md#custom-domains-and-vanity-subdomains)), you change DNS instead and the app needs no change.

What doesn't move, in short: the `sb_` keys, Vault secrets and the pgsodium key, passwords of your own login roles, secret values, and the access tokens that hosted signed with an asymmetric key. Those tokens fail on the node and users sign in again, while their refresh tokens keep working.

## Move a project off Supavise

Hosted Supabase, or any Postgres 17, takes a Supavise project the same way: you dump through the node's pooler and restore into the new database. Follow [the steps above](#move-a-project-from-hosted-supabase) with the two sides swapped: `OLD_DB_URL` is the node's connection string (`postgres.<ref>` on `pooler.<domain>:5432`) and `NEW_DB_URL` the new project's. The dump commands, the restore, the pg_cron step, the grants comparison and the Storage loop are the same.

- **Keys.** Hosted can't import keys. If you want users' access tokens to stay valid, set the node's JWT secret on the new hosted project (**Project settings**, **JWT keys**, **Change legacy JWT secret**, **Create my own secret**; read the node's secret with `supavise projects get <ref> --show-keys`). Hosted then signs new `anon` and `service_role` keys, and your app needs them.
- **Edge Functions.** Download each function from the node with the profile (`supabase --profile ./supavise-profile.yaml functions download <slug> --project-ref <ref>`), then deploy it to the hosted project. The node never returns secret values: `supabase secrets list` shows digests, so set the values again from your own records.
- **Settings.** Use the loop in step 5 the other way round. The node reports the same hashes for secrets, so the same filter applies.
- **Vault and pgsodium.** The dumps leave them out. `GET /v1/projects/<ref>/pgsodium` returns the node project's root key if you need to decrypt data that you export yourself.
- **Auth and Storage versions.** If the restore stops on an `auth` or `storage` table or column that the new project lacks, the node runs a newer version than the target. Fix it as in step 3.

## Monitor the node

```bash
sudo -u supavise supavise status   # one verdict for the node and every project
curl https://api.<domain>/healthz  # for an uptime monitor; reveals nothing else
```

To read a project's logs, run `supavise logs` on the server:

```bash
sudo supavise logs <ref>                                  # the last 100 lines of everything the project writes
sudo supavise logs <ref> --service postgres -n 500        # one service: postgres, auth, rest, realtime, storage, pooler, functions
sudo supavise logs <ref> --since 1h                       # an age (30m, 2h, 1d) or a time (2026-10-10T12:00:00Z)
sudo supavise logs <ref> -f                               # keep printing new lines
```

It reads the systemd journal, so run it with `sudo`, on the server that hosts the project (on a cluster, the dashboard's message names the node). A project's Postgres, Auth and REST units log under their own names. Realtime, Storage, the pooler and the Edge Runtime serve every project, so the command shows only their lines that mention the project's ref. Auth, REST, Realtime and Storage are set to log warnings and errors, so a healthy project prints little from them. The dashboard has no log store: its Logs pages, the Reports that read logs and the MCP server's log tools answer with an error that names this command, instead of an empty list. Use `supavise logs system` for the dashboard's own Postgres and Auth.

Add an `[alerts]` section to `/etc/supavise/config.toml` to get a webhook or email when a backup fails, disk runs low, the server's memory runs low, a project's disk quota is nearly full, a project turns unhealthy, a certificate nears expiry, an update is available, or an upgrade starts, succeeds or fails. With a second server you also get one when a replica is unhealthy or lags, a server stops answering, a failover starts, ends or fails, or a server is fenced. Before planned work, `supavise maintenance announce` shows a notice in the dashboard. The deploy guide's [health and alerts](../deploy/README.md#health-alerts-and-maintenance-notices) section has the details.

Studio's **Observability > Database** report, its overview cards and the **Compute and Disk** page show what each project uses: CPU, memory, connections, disk size, and the block reads and writes of Postgres. The server measures them once a minute and keeps the minutes of the last 3 days and the hours of the last 92. CPU is a share of the project's CPU quota, and memory is measured against its limit. When a project uses 80% of its disk quota or its CPU quota, or 85% of its memory limit, Studio shows its usual warning above the project; at 90% (95% for memory) the warning is critical. The charts of network traffic, the disk I/O budget, memory commitment, PgBouncer and Realtime have no source on a Supavise server and stay at zero. [`internal/metrics`](../internal/metrics/README.md) has the full list.

A server can run out of memory while every project is under its own limit, because the limits add up to more than the memory (`[compute] overcommit`). You get a `memory_low` alert when under 10% of the memory is available, or when programs wait for memory while under 30% is available (`[health] memory_low_percent` sets the percentage; half of it is critical). When a project's disk quota is 80% used, and again at 90%, you get `project_disk_high`. Raise the disk size in Studio under **Settings > Compute and Disk**, or delete data. In a cluster, each server alerts for the projects it runs, so set `[alerts]` destinations on every server. After a restart of the daemon or a reboot, an alert that is open stays open for up to 10 minutes while the project starts and is measured again, instead of ending and being raised a second time. When the kernel kills a process of a project for lack of memory, Studio shows a critical memory warning for that project for 15 minutes. The server sees the kill in the project's counters or, when systemd restarted the service after it, in a restart that coincides with the kernel's count for all projects. The second case is an estimate: a project that restarts in the minute when another project lost a process shows the warning too. Anyone who can restart a project (Developers can) can therefore learn from its warning that some project on the server lost a process, but not which one.

## What the dashboard shows differently

Studio is the real hosted dashboard. Where a page depends on something a server does not have, it says so instead of showing an empty list or a success that did nothing.

- **Logs and Reports.** The Logs Explorer and the log pages of each service show an error that points to `supavise logs <ref>`. The charts of the Auth, Edge Functions and some other reports have no way to show an error and stay empty.
- **Requests on the project home.** The chart counts the requests that the server's proxy served to the project's REST, Auth, Storage and Realtime APIs, by minute, hour or day. The counts live in memory: they start at zero when `supavise` restarts, and on a two-server cluster they leave out the requests that the second server served.
- **Database Webhooks.** Select **Enable webhooks** to create the `supabase_functions` schema and turn on `pg_net`. If you created a schema with that name yourself, the button fails with a message: drop or rename your schema first. Webhooks you create after that send an HTTP request when a row is inserted, updated or deleted, as you chose; the response is in `net._http_response`. A branch created with data cannot send those requests to the network unless it was created with `--allow-egress`.
- **Audit logs.** **Organization settings > Audit Logs** lists the requests that changed something in the organization: when, who (email, and whether it was a dashboard session, a personal access token or an OAuth app), from which address, the route and the status. **Account > Audit Logs** lists your own. Reads, the SQL editor, saved snippets, log queries and other calls that only read are not recorded, and neither are request bodies, so a password or a key never lands in the log. The server keeps 90 days (`audit_retention_days` under `[api]` in `/etc/supavise/config.toml`); a page covers at most 62 days and shows the newest 5000 events. A member who is scoped to one project cannot open the organization's page.
- **Image transformation.** Storage's **Enable image transformation** switch is refused with a message, because the server does not run the image service (imgproxy) that resizes images. Leave `[storage.image_transformation] enabled` at `false` in the CLI's `config.toml`: the default file says `true`, and `supabase config push` then stops at that setting with the message.
- **Settings the server cannot honor.** **Auth > Multi-factor > Allow low AAL** and similar switches that have no equivalent in a node's GoTrue are refused with a message when you change them, where they used to save and snap back. **Auth > Audit Logs > Disable Postgres writes** is kept and applied to GoTrue. **Auth > Emails > Reset to default** restores the default template and keeps the rest of the settings. **Auth > Users > Remove MFA factors** removes the user's factors. Changing the email address of a dashboard account, deleting an account, the organization's legal documents, the transfer of a project and the storage archive and backup download of a paused project are hidden or refused with a message; **Organization settings > SSO** is shown.
- **Features a server cannot offer.** Iceberg and vector buckets in Storage are hidden. SSL enforcement shows as unavailable. Adding a Third-party Auth provider, creating JWT signing keys and sending feedback are refused with a message. A project signs its tokens with the legacy JWT secret: change it under **Project settings**, **JWT keys**, **Legacy JWT secret**, or rotate it together with the API keys with `sudo -u supavise supavise projects rotate-keys <ref>`. The read replicas of the project take the new secret too (see "Do read replicas follow a change of the JWT secret?").

## Read replicas and failover

A second Supavise server gives a project a read replica, a place to move the project to, and a standby of the registry that can take over. Both servers need an S3-compatible backup bucket, because the second server reads its first copy from it: pass `--s3-bucket` at install (see the deploy guide's [flags](../deploy/README.md#flags)). A replica always lives on a server other than the project's own. On AWS, make the second server with `supavise-aws-deploy.sh replica` ([Add a replica server](../deploy/README.md#add-a-replica-server)).

**Join a second server.** On the first server (the leader), create a one-time token and save it in a file. Copy the file to the new server as `/root/join-token` with mode 0600, and install there with it:

```bash
# on the leader
(umask 077; sudo -u supavise supavise node token --region eu-west-1 > join-token)
# on the new server
curl -fsSL https://github.com/supavise/supavise/releases/latest/download/install.sh | sudo bash -s -- --join-token-file /root/join-token
# on the leader: the new node is joining, then active
sudo -u supavise supavise node ls
```

The servers talk over TCP port 7443, so open it between them. `--region` is the region that Studio shows for the new server's replicas. The token works once and expires after an hour.

**Add a replica.** In Studio, open **Project Settings**, **Infrastructure**, then **Add read replica**, and pick the new server's region. The button needs a project of size Small or larger. The same from the leader's shell:

```bash
sudo -u supavise supavise replicas add <ref> --region eu-west-1
sudo -u supavise supavise replicas ls          # the setup step, status and lag
```

To give every project a replica on every other server, set `[replicas] default = "all"` in the leader's `/etc/supavise/config.toml`. That roughly doubles the footprint of the cluster.

**Use it.** A replica answers reads at `https://<identifier>.api.<domain>/rest/v1`, with the project's own keys, and through the pooler as the user `postgres.<identifier>`. Studio lists both next to the primary. `https://<ref>-lb.api.<domain>` is a load balancer that sends reads (`GET` and `HEAD` on `/rest/v1`) to the nearest healthy database and everything else to the primary. Point a latency-routed DNS record at each server to spread reads; a name that resolves to the primary's server answers from the primary. [Read replicas reference](reference/replicas.md#names-and-ports) has the names.

**Switch over or fail over.** A switchover is planned and loses nothing. A failover follows a failure and loses at most what the replica had not received. Run each command with `--dry-run` first: it prints every precondition and changes nothing.

```bash
sudo -u supavise supavise projects failover <ref> --dry-run     # on the leader: one project
sudo -u supavise supavise failover --dry-run                    # on the server that should lead: the whole server
```

A project move needs a replica of that project. A server move needs one for every project, and Storage on S3 (`supavise storage migrate --to s3`, in the deploy guide's [Storage on S3](../deploy/README.md#storage-on-s3)). When the old primary is alive the move stops it cleanly and it follows as a replica, so moving back is the same command on the other server. When it is dead, the survivor fences it first. On AWS, `[failover] fencing = "aws"` stops the old instance and takes its Elastic IP. Elsewhere, set `fence_command` or pass `--old-primary-is-down`, which states that the old server cannot write. A server that returns after a failover starts no database until `sudo supavise node rejoin` rebuilds it.

By default every move is yours to start. `[failover] mode = "project"` or `"server"` lets the servers act on their own, on AWS only. The [reference](reference/replicas.md#failover) lists what a move does, the data a failure can cost and the failure matrix.

## How Supavise compares

| | Self-hosted Supabase (Docker Compose) | Supavise | Hosted Supabase |
|---|---|---|---|
| Projects | One per stack | Many per server, about 20 on an 8 GB server | Managed for you; each project gets its own Postgres instance |
| Organizations | One | Many, each with its own members and projects | Many |
| Dashboard | Studio for a single project | Supabase Studio in its hosted, multi-project mode | Supabase Studio |
| Management API and `supabase link` | Not available | Yes: the parts that Studio, the Supabase CLI and the MCP server call | Yes |
| Remote MCP server with OAuth sign-in | No OAuth: the stack's MCP route stays closed to outside connections until you open it | Yes, at `api.<domain>/mcp`, one organization per sign-in | Yes |
| Logs pages | Logflare and Vector, with an override file | No log store. `supavise logs <ref>` on the server reads the journal; the dashboard says so | Yes |
| Branching | Not available | Yes, schema-only or with data | Yes |
| Security and Performance Advisors | Studio's advisor pages for its one project; `supabase db advisors --db-url` checks a database by its URL | Yes: Studio, `supabase db advisors --linked` and the MCP server's `get_advisors`, with the database checks Studio runs and checks from the Auth settings; the health checks answer "unavailable" | Yes, with health checks too, and the Management API |
| Data API exposure of new tables | New tables in `public` keep the default grants to `anon`, `authenticated` and `service_role` unless you revoke them (the change is a platform default); `pg_graphql` is off in new installs | New projects expose no new `public` table until you grant it, and `pg_graphql` is off; an upgrade revokes the default grants of existing projects once, and existing tables keep their grants | New projects follow it from 2026-05-30 (`pg_graphql` off from 2026-05-18); existing projects are scheduled for 2026-10-30 |
| Database Webhooks | Yes: the stack's database setup creates `pg_net` and the `supabase_functions` schema | Yes: **Enable webhooks** sets them up for a project; it refuses a `supabase_functions` schema that a project created itself | Yes |
| Point-in-time restore | Not included; you set up backups yourself | Yes, in the dashboard (in place) or with `supavise backups restore` on the server | Yes |
| Move a project from hosted Supabase | Supabase's restore guide covers the database only; Storage files, Edge Functions, settings and keys are yours to move | Yes: [a guide](#move-a-project-from-hosted-supabase) moves the database, users, Storage files, Edge Functions, settings and legacy keys, and [back](#move-a-project-off-supavise) | Between hosted projects, with Supabase's backup and restore guide |
| Edge Functions | Yes | Yes, on by default | Yes |
| Team roles and SSO | One shared dashboard login | Owner, Administrator, Developer and Read-only roles; SAML SSO | Yes |
| Usage reports and resource warnings: CPU, memory, connections, disk | Not available: Supabase's docs say reports are not available for self-hosted instances | Yes: Studio's Database report and resource warnings, measured on the server and kept for 92 days; the network, disk I/O budget, memory commitment, PgBouncer and Realtime charts stay at zero | Yes; detail depends on your plan |
| Read replicas | Not available | Yes, per project on a second Supavise server, added in Studio or with `supavise replicas` | Yes |
| API load balancer | Not available | Yes, once a project has a replica: reads go to the nearest healthy database | Yes |
| Failover | Not built in; you set it up yourself | A switchover or failover of one project or the whole server, by hand; automatic on AWS only, with a fence | Managed by Supabase; depends on your plan |
| Storage on S3 | You set it up yourself | Optional, with `supavise storage migrate --to s3`; a server failover needs it | Yes |
| High availability | Not built in; you set it up yourself | Two servers: a standby copy of every replicated project and of the registry, so the second server can take over. One server alone has none. | Managed by Supabase; depends on your plan |
| Where it runs | Your server | Your server or your AWS account | Supabase's cloud |
| Who you pay | Your hosting provider | Your hosting provider | Supabase |

Some hosted features depend on your Supabase plan. The self-hosted column follows [Supabase's self-hosting docs](https://supabase.com/docs/guides/self-hosting) and its Docker Compose setup. The docs also say that self-hosted Supabase is community-supported.

## Updates and maintenance

A Supavise release is a tested bundle: the `supavise` binary plus pinned versions of every Supabase service. Before a release ships, it passes the conformance suite and a test that upgrades a node with data from the previous release.

**The routine.** Run these on the server:

```bash
sudo supavise upgrade --check   # is there a newer release, and what changes?
sudo supavise upgrade --plan    # exactly what will update and restart
sudo supavise upgrade           # apply it
sudo -u supavise supavise status
```

`supavise upgrade` checks that the node is healthy and the release is signed, then backs up every project before it changes anything. It also brings the host forward (unit files, directories, the firewall rule for the mesh port) with `supavise system converge`, which restarts no project. It restarts Supavise (HTTPS pauses for a few seconds), updates the shared services one at a time, then updates projects: one canary first, then batches of five, with a health check after each. A project that restarts drops its connections briefly. The rollout stops at the first failure and puts the node back on the previous release.

**Two servers and AWS stacks.** Run `supavise upgrade` on each server, the leader first when the release adds registry migrations: a follower one release behind runs against the leader's newer registry. While the two run different releases, automatic failover waits. On AWS, `sudo -E supavise upgrade --aws` also brings the CloudFormation stack forward, with your AWS credentials. It shows a change set and refuses any change that would replace the instance, the data volume or the address. [Bring a v0.1.x AWS stack forward](../deploy/README.md#bring-a-v01x-aws-stack-forward) has the steps, and the FAQ below the order to follow.

**Going back.** `sudo supavise rollback` returns to the previous release; the node keeps the last three. If the newer release changed Supavise's own database, rollback refuses and explains how to restore that database from its pre-upgrade backup first.

**Automatic upgrades (opt-in).** By default the node only tells you a release exists. To let it upgrade itself inside a weekly window:

```bash
sudo supavise update config --mode auto --window "Sun 03:00-05:00"
sudo supavise update status    # settings, the next window, the last automatic run
```

An automatic upgrade runs only inside the window and only when the node is healthy and backed up. If one fails, automatic upgrades pause until you run `sudo supavise update resume`.

**Project upgrades.** As on hosted Supabase, a project's Owner or Administrator upgrades that project's services from **Project Settings**, **General**, **Service versions**, or with `sudo -u supavise supavise projects upgrade <ref>`. `sudo supavise upgrade --include-postgres` moves every project's Postgres release as part of a node upgrade.

**OS patches.** New installs apply security updates for Ubuntu or Debian automatically. When a patch needs a reboot, the node reboots inside the maintenance window, and projects start again on their own. Turn this off with `sudo supavise update config --os-security-updates=false` or `--os-reboot never`.

**Planned work.** `sudo -u supavise supavise maintenance announce --at "2026-10-12 22:00" --duration 2h --message "Database maintenance"` shows a notice in the dashboard and quiets alerts during the window.

## Sizing and cost

A node needs about 1.5 GB of memory for itself and about 150 MB per idle project. A project that serves traffic needs more, so size up for busy projects.

| Instance (AWS example) | Memory | Idle projects that fit |
|---|---|---|
| `t4g.medium` | 4 GiB | about 10 (a trial) |
| `t4g.large` (default) | 8 GiB | about 35, or about 20 with room for traffic |
| `t4g.xlarge` | 16 GiB | about 90 |
| `m7g.2xlarge` | 32 GiB | about 200 |

These figures come from measurements up to 50 projects on amd64 and arm64 ([docs/reference/footprint.md](reference/footprint.md)); the 32 GiB row extends them.

Each project has a compute size, Nano to 16XL as on hosted Supabase, Micro by default. Change it in Studio under Compute and Disk or with `supavise projects resize <ref> --size small`; `supavise projects sizes` shows which sizes the node can give now ([project sizes and disk](../deploy/README.md#project-sizes-and-disk)). On AWS you pay AWS directly for the instance, storage and traffic; the deploy guide lists each item in [What it costs](../deploy/README.md#what-it-costs).

## FAQ

**Is Supavise an official Supabase product?**
No. Supavise is an independent open-source project and is not affiliated with or endorsed by Supabase Inc. It runs Supabase's open-source services.

**Does it replace hosted Supabase?**
Not entirely. You get organizations, projects, the dashboard, the CLI, branching and backups on a server you run. The differences:

- High availability needs a second server. On one server, if the server stops, its projects stop. With two, you move a project or the whole server by hand, and on AWS the servers can do it on their own. [Read replicas and failover](#read-replicas-and-failover) has the steps.
- The dashboard restores a project in place. "Restore to new project" is missing: restore a copy with `supavise backups restore --as` on the server.
- Management API operations that Studio, the CLI and the MCP server don't call, such as billing and log drains, answer with empty placeholders.

**Do my apps need to change?**
No. Apps use the same client libraries and API paths as on supabase.com; only the host name changes, to `<ref>.api.<domain>`.

**Why do replicas need a second server?**
A replica on the project's own server would share the failures it should protect against (the disk, the host, the power) and would draw on the same memory and CPU, so Supavise does not offer one there: the setup answers "Read replicas on the same server as the primary are not offered." The second server also holds a standby copy of the registry, which lets it take over as leader. It can sit in another availability zone or region.

**What happens to Auth during a failover?**
A project's Auth (GoTrue) runs where the project's primary runs. A failover starts it again on the server that took over, behind the same project URL, with the same keys and JWT secret, so tokens that clients already hold stay valid. Its users and sessions live in the project's database, which the replica copied, so a failover loses the sign-ins and token refreshes that were inside the replication lag (the lag shows in Studio, and a failover refuses a replica more than 30 seconds behind unless you pass `--force`). A planned switchover loses nothing. While a move runs, the project shows RESTARTING and requests to it can fail; they work again at the same URL when the move ends.

**Do read replicas follow a change of the JWT secret?**
Yes, while their server answers. Changing the legacy JWT secret in the dashboard restarts the PostgREST of each running replica on its own server, after the project's own services; the replica's database keeps running. If a replica can't be updated, the dashboard shows a failure and names it. The change stays in force, because taking it back would restore the old secret. Until you restart that replica (**Database**, **Read Replicas**) or save the same secret again, it refuses the new keys and still accepts tokens signed with the old secret. `supavise projects rotate-keys` can't reach other servers: it prints the new keys and then names the replicas to restart.

**Can I use the file backend?**
It depends on which one:

- **Backups on local disk** (`[backup]` is a `file://` path). A second server cannot read them, so `supavise node token` refuses, and `[replicas] default = "all"` and the automatic failover modes are refused. Point `[backup]` at an S3-compatible bucket first.
- **Storage on files** (`[fleet] storage_backend = "file"`, the default). Replicas and the failover of one project work. A failover of the whole server refuses, because the files exist on the old server only. Move them with `sudo -u supavise supavise storage migrate --to s3`: it copies while Storage serves, then pauses Storage's writes for a short time (uploads get 503 with `Retry-After: 5`) while it switches, and reads fail while Storage restarts on the bucket. `--rollback` goes back.

**How do I upgrade an AWS stack made by v0.1.x?**
Follow the [deploy guide](../deploy/README.md#bring-a-v01x-aws-stack-forward) and keep this order. First rehearse on a throwaway stack: `deploy/aws/rehearse.sh` (in a checkout of the release's tag) makes one from the v0.1.1 template, updates it, checks that the instance was not replaced and deletes it; it costs money while it runs. Then on the node, run `sudo supavise upgrade`: the v0.1.x binary drives this first step and brings the binary and the host forward. After that, run `sudo -E supavise upgrade --aws` with your AWS credentials, or `supavise-aws-deploy.sh update --stack <name>` in CloudShell. Read the change set. It adds resources and permissions and changes tags; the script refuses anything that would replace the instance, the data volume or the address, and applies the rest only after you type `apply`. A stack made in the CloudFormation console (its `AmiId` is empty) shows the instance and what refers to it as guarded: CloudFormation decides only while the change set runs whether they are replaced, so the change set runs under a temporary stack policy that denies any replacement, and an update that would need one rolls back instead. Rehearse that case with `rehearse.sh --console-stack`. Nothing has run against a real stack in this repository's tests, so the rehearsal is the check.

**Is my data backed up?**
Yes: databases, Storage files and Edge Functions, unless Storage uses its S3 backend (`[fleet] storage_backend = "s3"`), whose files stay in your own bucket: turn on versioning there. Every database archives its WAL continuously and takes a nightly base backup, and you can restore to any point in the retention window (7 days by default). The nightly run also copies each project's Storage files and Edge Function deployments to the same place, keeping only what changed.

- Backups go to local disk unless you give an S3 bucket. A bucket keeps them off the server.
- A restore returns files to the last nightly copy before the time you choose, not to that second. Hosted Supabase's database backups do not include Storage files, so this goes further than hosted.
- The master key, `/etc/supavise/master.key`, unseals the passwords inside backups and is not in them. Run `sudo -u supavise supavise system export-key` and keep the output offline, or keep an encrypted copy in the bucket with `system escrow-key`. Without the key, a lost server cannot be rebuilt from its backups.
- On AWS, daily snapshots of the data volume add a whole-node fallback.

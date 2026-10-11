# Using Supavise

This guide picks up after the install in the [README](../README.md#get-started). Your Supavise server is called a node, and a second server that joins the first is another node. For server and AWS options in depth, see the [deploy guide](../deploy/README.md).

- [Claim the node and sign in](#claim-the-node-and-sign-in)
- [Create a project and connect an app](#create-a-project-and-connect-an-app)
- [Use the Supabase CLI](#use-the-supabase-cli)
- [Give an agent or a preview its own branch](#give-an-agent-or-a-preview-its-own-branch)
- [Connect an AI tool to a project](#connect-an-ai-tool-to-a-project)
- [Back up and restore](#back-up-and-restore)
- [Move a project from hosted Supabase](#move-a-project-from-hosted-supabase)
- [Move a project off Supavise](#move-a-project-off-supavise)
- [Monitor the node](#monitor-the-node)
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

## Back up and restore

Backups start on their own. Every project archives its write-ahead log (WAL) continuously and takes a nightly base backup, and the same nightly run copies its Storage files and Edge Functions.

Backups go to local disk by default. To keep them off the server, pass `--s3-bucket` and `--s3-region` at install (see the deploy guide's [flags](../deploy/README.md#flags)). The AWS stack uses its own bucket.

**Restore from the dashboard.** Open **Database > Backups > Point in time**, pick a time within the retention window (7 days by default) and confirm. The project shows RESTORING, then returns to normal; anything written after that time is lost. Only Owners and Administrators can restore. The API equivalent is `POST /v1/projects/<ref>/database/backups/restore-pitr`.

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

Create the project on the node. The command runs as the `supavise` user, so that user must be able to read the files. Set `NEW_REF` to the hosted project's ref to give the project the same ref: the proxy then accepts the hosted keys because their `ref` claim names the project. With another ref, only the two key files make the node accept them. Set `NEW_SIZE` to a compute size (`supavise projects sizes` lists them).

<!-- migrate-smoke: create -->
```bash
sudo -u supavise supavise projects create --name "$NEW_NAME" --ref "$NEW_REF" --size "$NEW_SIZE" \
  --jwt-secret-file jwt-secret.txt --anon-key-file anon-key.txt --service-role-key-file service-role-key.txt --show-keys
```

The command prints the new project's database password. The project also has its own publishable and secret keys (`sb_...`), because hosted keys of that kind can't be imported: if your app uses them, switch it to the new ones.

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
psql --single-transaction --variable ON_ERROR_STOP=1 \
  --command 'alter default privileges in schema public revoke all on tables from anon, authenticated, service_role' \
  --command 'alter default privileges in schema public revoke all on functions from anon, authenticated, service_role' \
  --command 'alter default privileges in schema public revoke all on sequences from anon, authenticated, service_role' \
  --file roles.sql --file schema.sql \
  --command 'SET session_replication_role = replica' \
  --file data.sql --dbname "$NEW_DB_URL"
```

The first three commands keep your access rules. A new table, function or sequence in `public` gets `ALL` for `anon`, `authenticated` and `service_role` from the project's default privileges, and the dump only adds grants: it never takes one away. Without the commands, a table that you locked down on hosted (`revoke all ... from anon`) is open again through the Data API. The commands run in the restore's transaction, so a failed restore leaves the default privileges as they were. The dump lists the grants of each object and sets the default privileges of your `postgres` role again at its end, so the project ends up with the grants it had on hosted. Step 8 compares them.

The restore runs in one transaction, so a failure leaves the project as it was. Fix the cause and run the restore again. If psql stops, look for the message:

- **`relation "..." does not exist` or `column "..." of relation "..." does not exist`, in the `auth` or `storage` schema.** If you created the project a moment ago, wait until Auth and Storage have set up their schemas and run the restore again. Otherwise the hosted project runs a newer Auth or Storage than the node. Update the node first (`sudo supavise upgrade`), or delete that table's `COPY` block from `data.sql` (down to the `\.` line) and accept that its rows don't move. `sudo -u supavise supavise projects get <ref> --json | jq .versions` shows the node's versions.
- **`permission denied`, on a line that sets an owner to `supabase_admin` or on a `GRANT` to `cli_login_postgres`.** Comment that line out.
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

Add an `[alerts]` section to `/etc/supavise/config.toml` to get a webhook or email when a backup fails, disk runs low, a project turns unhealthy, a certificate nears expiry, an update is available, or an upgrade starts, succeeds or fails. With a second server you also get one when a replica is unhealthy or lags, a server stops answering, a failover starts, ends or fails, or a server is fenced. Before planned work, `supavise maintenance announce` shows a notice in the dashboard. The deploy guide's [health and alerts](../deploy/README.md#health-alerts-and-maintenance-notices) section has the details.

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
| Remote MCP server with OAuth sign-in | Not available | Yes, at `api.<domain>/mcp`, one organization per sign-in | Yes |
| Branching | Not available | Yes, schema-only or with data | Yes |
| Point-in-time restore | Not included; you set up backups yourself | Yes, in the dashboard (in place) or with `supavise backups restore` on the server | Yes |
| Edge Functions | Yes | Yes, on by default | Yes |
| Team roles and SSO | One shared dashboard login | Owner, Administrator, Developer and Read-only roles; SAML SSO | Yes |
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

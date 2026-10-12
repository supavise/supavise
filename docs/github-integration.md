# GitHub integration

A connected repository gets a preview branch for every pull request, the way hosted Supabase does. The branch is built from the repository's `supabase` directory (migrations, seed, `config.toml`, Edge Functions) and not from your production schema, a check named `Supabase Preview` and one comment on the pull request report how it went, and the branch is deleted when the pull request closes. A push to the production branch deploys the repository to the project itself.

This needs a GitHub App that belongs to you. Studio's buttons open Supabase's own App on hosted Supabase, and that App cannot send webhooks to your server, so Supavise never uses it: until your server has an App, the dashboard hides every GitHub button. [design.md](design.md) section 9b records the design; [../internal/github/README.md](../internal/github/README.md) describes the code.

## Before you start

- **GitHub must reach your server.** The App sends its events to `https://api.<domain>/hooks/github`. A server behind NAT, or one whose `api.<domain>` is not reachable from the internet, cannot receive them; use the [Actions workflow](#a-server-github-cannot-reach) instead.
- **The dashboard must be reachable by the people who connect repositories.** GitHub sends their browsers back to `https://studio.<domain>/integrations/github/authorize`.
- **Run the commands on the leader.** The App's credentials live in the registry, which only the leader writes. A cluster needs no more: a follower forwards the webhook and the dashboard's calls to the leader, and the App moves with the registry at a failover. An App named in `[github]` needs the same section and the same files on every node that may become the leader.
- **Upgrade the leader first in a cluster.** The GitHub state lives in the registry (migrations 1540 and 1541), which the leader migrates. A follower on this release that reads a registry the leader has not migrated yet sees no App and no pull requests, and shows no GitHub surfaces until the leader is upgraded; nothing else on it is affected.
- **The daemon must run.** `supavise github app create` prepares the flow; the daemon serves the page and the address GitHub sends the browser back to.

## Create the App

```bash
sudo -u supavise supavise github app create --owner my-org
```

`--owner` names the GitHub organization that will own the App; leave it out for the account that is signed in to GitHub in your browser. `--name` sets the App's name (at most 34 characters, unique on GitHub; the default is `Supavise <domain>`).

The command prints a link. It is good once, for 30 minutes.

1. Open the link in a browser that is signed in to GitHub. The page on your server shows what it will ask for; choose **Continue to GitHub**.
2. GitHub shows the App with the name, the webhook URL and the permissions. Confirm it.
3. GitHub sends the browser back to your server, which exchanges the code for the App's credentials and stores them sealed with the node's master key. The page then offers the App's install link.
4. Install the App on the account or organization that owns your repositories, and choose the repositories it may read.

The server restarts Studio so that its install and authorize windows name your App. `supavise github app status` shows the App; `supavise github app remove` forgets it (the App stays on GitHub; delete it there too when you no longer need it). `supavise github app create --replace` replaces the App. The users' authorizations and every project's connection to a repository go with the old one, because they name its installations: connect the projects again afterwards. The preview branches stay and stop following their pull requests; `supavise github app remove` does the same.

The App is private to its owner. It asks for read access to contents and metadata, and write access to pull requests, checks and issues (the comment on a pull request is an issue comment). It subscribes to `pull_request`, `push`, `create` and `delete`; GitHub sends the installation events to every App on its own.

### An App you made yourself

Put its credentials in files readable only by the `supavise` user and name them in `/etc/supavise/config.toml`:

```toml
[github]
app_id = 123456
app_slug = "my-supavise"          # github.com/apps/my-supavise
client_id = "Iv1.0123456789abcdef"
client_secret_file = "/etc/supavise/github/client_secret"
private_key_file = "/etc/supavise/github/private-key.pem"
webhook_secret_file = "/etc/supavise/github/webhook_secret"
# allow_forks = false
# max_concurrent_builds = 3      # preview branches built at once; more wait their turn
```

Configure the App on GitHub with the permissions and events above, the webhook URL `https://api.<domain>/hooks/github` (content type JSON), the callback URL and the setup URL `https://studio.<domain>/integrations/github/authorize`. An App named here wins over one the manifest flow stored, and `supavise github app create` refuses while it is set. The same keys are `SUPAVISE_GITHUB_*` environment variables.

`api_url` and `web_url` (both or neither) point at a GitHub Enterprise Server. Studio's own install and authorize windows still open `github.com`, so the dashboard flow works with github.com only.

## Connect a repository

In Studio, open the project's **Settings**, **Integrations**, **GitHub**.

1. **Authorize** the App. Each person authorizes for themselves; the server keeps the token sealed and uses it to list the repositories that person can reach, and no others.
2. Choose the **repository** (install the App on more repositories from the picker if the one you want is missing).
3. Set the **working directory**, the folder of the repository that contains `supabase/` (empty for the root).
4. Choose the options:

| Option | Meaning |
|---|---|
| Automatic branching | Create a preview branch for every pull request. Off: pull requests are ignored, and branches you made yourself with a Git branch (`supabase branches create --git-branch`) follow their Git branch. |
| Supabase changes only | Create a branch only for pull requests that change a file under the working directory's `supabase/`. A push to the production branch deploys only when its commits do. |
| Branch limit | The most preview branches the connection keeps. A pull request over the limit gets a neutral check and a comment that says so. The node has its own caps: `[branching] max_per_project` and `max_total`. |
| Deploy to production | Apply the repository to the project when you push to the production branch. |
| Production branch | The Git branch production follows (usually `main`). |

Owners and Administrators connect, change and disconnect; every role that can see the project reads the connection. Disconnecting keeps the preview branches that exist: delete them from the Branches page. The picker lists only repositories you can push to; the App posts check runs and comments on them, so a read-only collaborator cannot connect a project. If a repository is renamed or transferred, its connection follows it (the next event from GitHub carries the new name).

## What happens to a pull request

| Event | What the server does |
|---|---|
| Pull request opened or reopened | Creates the preview branch (named like the head branch, with `git_branch` and `pr_number` set) and runs the steps below. |
| New commits | Runs the steps on the same branch: only the migrations it lacks, only the Edge Functions that changed, and the seed only if an earlier run never got past it (see below). A newer commit cancels the work on an older one. A branch that was paused, by hand or by the idle pause of preview branches, is resumed first. |
| Closed or merged | Deletes the branch. A persistent branch is kept. |
| Pull request from a fork | Ignored (see [Forks](#forks)). |
| Git branch deleted | Deletes the preview branch that followed it, when no pull request owns it. |
| Push to a Git branch that a branch follows | Runs the steps on that branch (a persistent `staging` branch, for example). |
| Push to the production branch | Runs the steps on the project (see [Deploy to production](#deploy-to-production)). |

A branch made for a pull request never expires by time; it lives until the pull request closes. Branches you create from Studio, the CLI or the MCP server keep the lifetime that `[branching] default_ttl` gives them.

### The steps

The run is an ordinary action run with seven steps, readable in Studio (**View Logs**), through `GET /v1/projects/<branch>/actions` and with `supavise branches runs <branch>`.

| Step | What it does |
|---|---|
| clone | Lists `<working directory>/supabase` at the head commit through GitHub's API (no git on the server). |
| pull | Creates the branch's project (a new branch only). |
| health | Waits for its database. |
| configure | Saves `config.toml` as the branch's settings (see below). |
| migrate | Applies the migrations the branch does not have, each in one transaction, in version order. A migration that changed after it was applied stays as applied; the log says so. |
| seed | Runs the seed files once per branch, in the first run that gets this far. |
| deploy | Deploys the Edge Functions that changed and creates the Storage buckets of `config.toml`. |

The branch starts from **nothing but the repository**: its schema comes from `supabase/migrations`, not from production. It still starts with the parent's saved settings, as every branch does, and `config.toml` is laid over them. The seed belongs to the first run that gets past `migrate`. If that run fails in a migration, or a newer commit cancels it, the next commit applies the missing migrations and then the seed. A seed that fails is tried again by the next commit. A run that found no seed file, or `[db.seed] enabled = false`, used its chance: a `seed.sql` added later is not applied to a branch that has data. A branch you made yourself before the pull request is never seeded. To rebuild a branch and seed it again, close and reopen the pull request.

The node builds at most `[github] max_concurrent_builds` preview branches at once (3 when unset); further pull requests wait their turn, and a newer commit replaces one that waits. All the files a run reads from the repository together may not exceed 100 MiB; a run that would read more fails with that reason.

Several projects can be connected to one repository (each with its own working directory). Each keeps its own comment on the pull request, marked with the project's ref in its first line; a comment that another GitHub App wrote is never edited. If the App may no longer edit its comment, it writes a new one.

### The files it reads

- `supabase/migrations/<version>_<name>.sql`, as `supabase db push` names them. Other files are listed in the log and skipped. A file with only whitespace is skipped. Declarative schemas (`supabase/schemas`) are not applied: commit the generated migration.
- The seed: `[db.seed] sql_paths` (default `./seed.sql`), in glob order. `[db.seed] enabled = false` turns it off. A persistent branch is seeded only when `enabled = true` is written.
- `supabase/config.toml`.
- `supabase/functions/<name>/` with `index.ts` (or the `entrypoint` of `[functions.<name>]`). The `_shared` directories and the files directly in `functions/` (`import_map.json`, `deno.json`) go with every function. `verify_jwt`, `import_map` and `static_files` of `[functions.<name>]` apply. A function that was deployed before and is not in the repository any more stays deployed: nothing is deleted.

A commit may carry at most 10 MB per file, 8 MB per migration, 40 MB per function and 100 MB in all.

### config.toml

The top-level values apply to every preview branch. A persistent branch is configured by the `[remotes.<name>]` block whose `project_id` is the branch's project ref (find it with `supabase branches list`), laid over the top-level values; with no such block its configure step is skipped. Production is configured the same way, and only that way: the top-level values never reach it, because a file made for local development carries values such as `site_url = "http://127.0.0.1:3000"`.

`env(NAME)` reads the Edge Function secret `NAME` of the branch (`supabase secrets set --project-ref <branch>`). A value that cannot be read is not applied, and the log names it; set the secret and push again. `encrypted:` values cannot be read and are not applied.

| Section | Applied as |
|---|---|
| `[api]` | `schemas`, `extra_search_path` and `max_rows` become the PostgREST settings. |
| `[auth]` | Site URL, redirect URLs, JWT expiry, refresh token rotation, sign-ups, anonymous sign-ins, manual linking, password length and requirements. |
| `[auth.rate_limit]`, `[auth.email]` and `[auth.email.smtp]`, `[auth.email.template.*]`, `[auth.email.notification.*]`, `[auth.sms]` and its providers, `[auth.mfa]`, `[auth.captcha]`, `[auth.sessions]`, `[auth.passkey]`, `[auth.webauthn]`, `[auth.web3]` | The Auth settings of the same names. Email template bodies come from the files named in `content_path`. |
| `[auth.external.<provider>]` | `enabled`, `client_id`, `secret`, `url`, `skip_nonce_check`. The redirect URI of a provider is the project's own and cannot be set. |
| `[auth.hook.<name>]` | `enabled`, `uri`, `secrets`. |
| `[storage]` | `file_size_limit`, `[storage.image_transformation]`, `[storage.s3_protocol]` and the buckets in `[storage.buckets.<name>]` (`public`, `file_size_limit`, `allowed_mime_types`). Files in `objects_path` are not uploaded. |
| `[db.settings]` | The Postgres settings Supavise saves. A setting that needs a restart waits for the next one. |
| `[db.seed]`, `[functions.<name>]` | Read by the seed and deploy steps. |

Everything else is named in the run's log as not applied, with the key, so nothing is dropped silently: `[auth.third_party.*]`, `[auth.oauth_server]`, `[db.pooler]`, `[db.network_restrictions]`, `[storage.analytics]` and `[storage.vector]`, and so on. The sections that configure the local stack (`[studio]`, `[inbucket]`, `[analytics]`, `[edge_runtime]`, `[experimental]`, `[realtime]`, and the ports of `[api]` and `[db]`) are listed once as local development settings.

## Deploy to production

Set **Deploy to production** and the **production branch** in the connection form. A push to that Git branch (a merge, usually) runs the steps on the project: the migrations it lacks, the Edge Functions that changed and the buckets of `config.toml`; no seed. It is a run listed under the project, with a check named `Supabase Production` on the commit. The check shows the step table and a short failure message; the full log stays in the dashboard, because production's errors can quote its data and the check is visible to everyone who reads the repository.

With Deploy to production on, Studio disables the dashboard's **Merge** button for the project: changes reach production through the repository. **Resync** on the production row of the Branches page deploys the head of the production branch again.

Deploying needs the project running; a paused project fails the run with that reason. Each migration is applied under the project's apply lock, the one a merge takes, so a merge from the CLI or the MCP server and a deployment do not interleave their DDL; they take turns.

## Forks

The author of a pull request chooses the SQL, the function code and `config.toml` of the preview branch. A preview branch has its own database and keys, but it starts on the parent project's saved settings, which include the SMTP password and the OAuth client secrets, so a person who can push to the repository can make the branch use them. Supavise limits that: when `config.toml` sets the SMTP host or the `url` of an OAuth provider, the secret the branch inherited for it (`smtp_pass`, `external_<provider>_secret`) is cleared, and the log says so; the secret that `config.toml` gives by `env(NAME)` from a secret of the branch is applied. Other inherited secrets stay, so connect only repositories whose writers you trust with them. Anybody can also open a pull request from a fork of a public repository. Supavise therefore ignores pull requests whose head is not in the connected repository. `[github] allow_forks = true` builds them too; turn it on only for a private repository whose forks you trust.

## What is stored

| Secret | Where |
|---|---|
| The App's private key, client secret and webhook secret | Sealed with `/etc/supavise/master.key` in the registry (migration 1540), or in the files `[github]` names. Never in a log, an event or an answer of the API. |
| A user's GitHub token and refresh token | Sealed, per user; refreshed before they expire; removed when GitHub refuses them or the user removes the connection (the token is revoked at GitHub too). |
| Installation tokens | In memory, for under an hour. |

The webhook checks the `X-Hub-Signature-256` header in constant time before it reads anything, remembers each `X-GitHub-Delivery` so that a replay does nothing, refuses a request whose signature header cannot be GitHub's, and one declaring a body over 4 MiB, before it reads the body, then reads at most 16 bodies at a time (15 seconds each) and limits the rate of deliveries. A push of more commits than fit in 4 MiB is refused with 413; **Resync** deploys production for it.

## A server GitHub cannot reach

A server behind NAT cannot receive webhooks, so it cannot use the App. A GitHub Actions workflow can do the same work with the Supabase CLI, which needs only to reach the node's API:

```yaml
# .github/workflows/supavise-preview.yml
name: Supavise preview
on:
  pull_request:
    types: [opened, reopened, synchronize, closed]

env:
  API_URL: https://api.example.com                    # your node's API
  PROJECT_REF: ${{ vars.SUPAVISE_PROJECT_REF }}       # the project the branches belong to
  SUPABASE_ACCESS_TOKEN: ${{ secrets.SUPAVISE_ACCESS_TOKEN }}   # a personal access token of a user who may create branches
  BRANCH: pr-${{ github.event.pull_request.number }}

jobs:
  preview:
    if: github.event.action != 'closed'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: supabase/setup-cli@v1
      - name: Write the profile of the node
        env:
          PROFILE: ${{ secrets.SUPAVISE_PROFILE }}     # `supavise api profile --format yaml`
        run: printf '%s\n' "$PROFILE" > supavise-profile.yaml
      - name: Create the branch if it does not exist
        env:
          HEAD_REF: ${{ github.head_ref }}             # chosen by whoever pushed the branch
        run: |
          api() { curl -fsS -H "Authorization: Bearer $SUPABASE_ACCESS_TOKEN" -H 'Content-Type: application/json' "$@"; }
          api "$API_URL/v1/projects/$PROJECT_REF/branches/$BRANCH" >/dev/null 2>&1 \
            || api -X POST "$API_URL/v1/projects/$PROJECT_REF/branches" \
                 -d "$(jq -n --arg name "$BRANCH" --arg git "$HEAD_REF" '{branch_name: $name, git_branch: $git}')"
      - name: Apply the repository to the branch
        run: |
          api() { curl -fsS -H "Authorization: Bearer $SUPABASE_ACCESS_TOKEN" "$@"; }
          for i in $(seq 1 60); do
            st=$(api "$API_URL/v1/projects/$PROJECT_REF/branches/$BRANCH" | jq -r .status)
            [ "$st" = FUNCTIONS_DEPLOYED ] && break; sleep 5
          done
          REF=$(api "$API_URL/v1/projects/$PROJECT_REF/branches/$BRANCH" | jq -r .project_ref)
          D=$(api "$API_URL/v1/branches/$REF")
          export SUPABASE_DB_PASSWORD=$(echo "$D" | jq -r .db_pass)
          supabase --profile supavise-profile.yaml link --project-ref "$REF" --yes
          supabase --profile supavise-profile.yaml db push --yes
          supabase --profile supavise-profile.yaml functions deploy --use-api --project-ref "$REF"

  cleanup:
    if: github.event.action == 'closed'
    runs-on: ubuntu-latest
    steps:
      - run: |
          curl -fsS -X DELETE -H "Authorization: Bearer $SUPABASE_ACCESS_TOKEN" \
            "$API_URL/v1/branches/$(curl -fsS -H "Authorization: Bearer $SUPABASE_ACCESS_TOKEN" "$API_URL/v1/projects/$PROJECT_REF/branches/$BRANCH" | jq -r .project_ref)"
```

The workflow passes the branch name of the pull request and the profile through `env:` and never writes a `${{ }}` expression into a script, because a Git branch name can hold quote characters and `$(...)`, and the runner holds the access token. Keep it that way when you change the workflow. It is a starting point that has not been run against a node; try it on a test repository first.

Differences from the App: no check run named `Supabase Preview` and no comment (add them with `gh`), the seed does not run (run it with `psql` against the branch's connection string from `branches get`), `config.toml` is not applied (use `supabase config push --project-ref <branch ref>` with a `[remotes]` block), and the node must be reachable from the runner.

## Troubleshooting

- **The dashboard shows no GitHub buttons.** The server has no App: run `supavise github app status`.
- **Nothing happens on a pull request.** In the App's settings on GitHub, open **Advanced** and look at the recent deliveries: a `401` is a webhook secret that does not match, a connection error is `api.<domain>` unreachable. A `202` with "no project is connected" means the repository is not connected, or the delivery came from another installation. A pull request from a fork is ignored, and so is one that changes nothing under `supabase/` when **Supabase changes only** is on.
- **"Supavise could not create the preview branch".** The node refused: the project must be running, and `[branching] max_per_project`, `max_total` or the disk can stop a branch. The comment and the check carry the reason.
- **A migration fails.** The run's `migrate` step is `DEAD`, the log has the migration and the error, and the branch is `MIGRATIONS_FAILED`. Push a fix: the same branch is brought up to date with only the migrations it still lacks. If the first run never reached the seed, the run that applies the fix seeds the branch.
- **`env(NAME)` is not applied.** Set the secret on the branch with `supabase secrets set NAME=... --project-ref <branch ref>`, then push again.
- **The pull request stopped getting reports after the repository was renamed.** The connection follows the new name with the next event GitHub sends; push a commit or reopen the pull request. After `app create --replace` or `app remove` the connections are gone: connect the project again.
- **"Supavise could not wake the paused preview branch".** The branch is paused and could not be resumed (the node is full, or the project is being deleted). Resume it from the Branches page.
- **The Storage buckets step says the schema is not ready.** A new branch's Storage service has not finished; the step retries for 30 seconds.

## Differences from hosted

- The App belongs to you, so you create it once (hosted has one App for everybody).
- Declarative schemas, `[db.pooler]`, `[auth.third_party.*]` and the other sections listed above are not applied.
- A removed Edge Function stays deployed.
- Automatic branching off follows manually made branches by Git branch; hosted's behavior for pushes and `create` events is not documented, and `create` events are acknowledged and ignored.
- The dashboard's GitHub windows open github.com.

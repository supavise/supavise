# internal/github: the GitHub integration

Connects projects to GitHub repositories with a GitHub App that belongs to the server's operator, the way hosted Supabase does. User documentation: [docs/github-integration.md](../../docs/github-integration.md); design: [docs/design.md](../../docs/design.md) section 9b. The Management API in front of it is `internal/api/github_integration.go` and `github_webhook.go`; the state is `internal/registry/github_store.go` (migration 1540); the section of `config.toml` is `internal/config/github.go`.

```go
svc, _ := github.New(github.Options{Cfg: cfg, Registry: reg, Secrets: sec, Branches: branchingService, Audit: ...})
// internal/api gives it the rest when its Server starts (SetHost, SetOnAppChange).
svc.Configured(ctx)                       // does the server have an App whose secrets can be read
res := svc.HandleWebhook(ctx, github.Delivery{Event: ..., ID: ..., Signature: ..., Body: ...})
```

## Files

| File | What |
|---|---|
| `github.go` | `Service`, `Options`, the interfaces to the node (`Branches` is `*branching.Service`, `Host` is the part of the Management API that saves settings and deploys functions). |
| `app.go` | The App: from `[github]` (files, re-read when they change) or the registry (sealed); the manifest flow (`BeginManifest`, `ManifestForm`, `CompleteManifest`), `Status`. |
| `client.go` | GitHub's REST API. A token is sent only to the configured API origin, `Link` headers are followed only inside it. |
| `tokens.go` | Installation tokens (cached), the users' OAuth tokens (sealed, refreshed once at a time per user, dropped when GitHub refuses them). |
| `repos.go` | Repositories and branches through the user's token, connections, `config.toml` as JSON. |
| `webhook.go` | `HandleWebhook`: rate limit, signature in constant time, delivery dedupe (by id and by body hash, kept a week), dispatch. |
| `pr.go` | A pull request: the branch is created, updated or deleted; the check run and the comment. |
| `production.go` | The push to the production branch, `DeployProduction` (Resync), pushes and deletions of Git branches; `pushIsHead` compares a push with the head of its branch. |
| `runner.go` | The seven steps (`repoSource`): it is a `branching.SchemaSource` for a new branch and a `branching.Driver` for an existing one. |
| `snapshot.go` | The `supabase` directory at a commit through the tree and blob API: migrations, seed globs, functions (hashes decide what is read). |
| `configtoml.go` | `config.toml` mapped onto `internal/projectconfig`, with `[remotes]`, `env()` and a warning for every key it does not apply. |
| `report.go` | The `Supabase Preview` check run and the one comment per pull request. |
| `redact.go` | What a report may say (`outcome.public`): a failure cut and redacted, none for a branch that may hold data, an excerpt of the failed step's log for the check run. |
| `refname.go` | `checkBranchName` (git's rules plus dot segments and encoded separators) and the dot-segment guard of `Client.newRequest`. |
| `queue.go` | One piece of work at a time per pull request or branch; a newer event cancels an older (a job that never starts calls its `dropped` function, so that a run made for it is closed). `[github] max_concurrent_builds` bounds the builds across keys (`acquireBuild`). |

## Rules the code keeps

- **No secret in a log, an event or an answer.** The App's key, client secret and webhook secret, and the users' tokens, are sealed before they reach the registry. Errors from GitHub carry the path without its query.
- **Believe nothing before the signature.** The webhook route has no credentials: it refuses a signature header that cannot be GitHub's and a declared length over 4 MiB before it reads anything, reads at most 16 bodies at a time (each within 15 seconds, enforced on the connection: `internal/api` keeps `statusWriter.Unwrap` for it), rate limits, checks the HMAC, and only then parses. A delivery id seen before does nothing, and neither does a body seen before under another id (the signature covers the body and not the id).
- **A token goes to one origin.** `Client` sends the Authorization header to `[github] api_url` only; a `Link: next` to another host ends the listing.
- **Fork pull requests are ignored** unless `[github] allow_forks` is set.
- **A user sees what their own token sees.** Repositories are listed with the user's token (those the user can push to; the list answers the branch pickers for 30 seconds) and a connection checks the repository against that list.
- **A repository's rename is followed, a redirect is never.** The HTTP client does not follow redirects, so a 3xx is an error; `connectionsFor` updates the stored name from the signed payload.
- **The seed is recorded.** `github_branch_links.seeded_at` (migration 1541) is set when a run gets past the seed step; the seed is applied by the first such run, not by the call that creates the branch. A branch somebody made before the pull request is marked seeded. A reset of a branch that has a link (branching `Reset`, `repositoryBuilt`) clears the mark with `ClearGitHubBranchLinkSeed`, because the reset drops the rows, and skips the parent's stored seed; the next commit seeds again if the repository has a seed. The link does not say who made the branch, so a branch made by hand and followed by a pull request is reset like a preview branch; telling the two apart would need a column on the link.
- **PostgREST's settings of `config.toml` wait for the migrations.** `[api] schemas` can name a schema that a migration of the same commit creates, and PostgREST does not start on a missing one. `configure` saves the other services' settings; the PostgREST patch is saved at the end of `migrate` (`exposeSchemas`), in one restart, and `Host.SaveSettings` refuses a list that still names a missing schema, with its name, before it restarts anything.
- **Image transformation is never switched on.** `[storage.image_transformation] enabled = true` is left out of the plan with a warning (the node runs no imgproxy); `false` applies.
- **A branch the node paused for idleness is woken** (`branching.Service.WakeForDrive`) before a commit is driven onto it. A branch its owner paused, one stopped as a quarantine (it still carries the parent's credentials) and one scheduled for deletion are not woken: the check fails with the reason, because a commit is not the owner's decision to run a branch again.
- **A branch's switches wait for their secrets.** A branch has none of the parent's credentials and a new preview branch no function secret, so `env(NAME)` cannot be read on its first run, and the node refuses `enabled = true` of an OAuth provider (not Apple), the captcha or an HTTP hook without its secret. `holdBackSwitchesWithoutSecrets` leaves such a switch out of the patch, with a warning, unless the patch carries the secret or `Host.SettingSaved` says the branch holds one; the run still finishes and the next commit switches it on. It runs for branches only; the connected project is configured as the file says.
- **A PR's `config.toml` cannot redirect a branch's secret.** A branch starts without the parent's credentials (`projectconfig.BranchWithheld`). On a branch, setting the SMTP host or an OAuth provider's `url` clears the secret the branch holds for it unless `config.toml` gives one (`dropInheritedSecrets`).
- **One run reads at most 100 MiB** from the repository in all (`Snapshot.budget`), and migrations are applied under the project's apply lock (`ApplyOptions.Lock`).
- **One comment per project and pull request.** The first line is `<!-- supavise-preview:<project ref> -->`; a comment of another App is not adopted.
- **One writer per branch.** Work on a pull request is serialized; before it drives a branch it waits for the branch to be idle, and the branching service refuses a second operation anyway.
- **Production is deployed from migrations and functions only.** `config.toml` reaches it through a `[remotes]` block whose `project_id` is the project's ref, and the check run of a production deploy carries neither a log nor the error.
- **A push deploys only while it is the head.** `pushIsHead` reads the head of the branch inside the queued job; an older, reordered, replayed or redelivered push is skipped. Deployments of a project take turns on the queue key `prod:<project>`, newest first.
- **A report is public.** `outcome.public` is the only way to the check run and the comment: failures are cut to 300 characters, credentials and the secrets `config.toml` read are taken out, and a branch that may hold data (`WithData` or `Persistent`) and production report no error at all.
- **A branch the integration creates has its link.** `createFromPR` makes the branch and stores the link under `context.WithoutCancel`, so that a newer commit that cancels the job does not leave a branch the next event would take for a hand-made one.
- **A branch name is checked before it is a path.** `checkBranchName` and the dot-segment guard of `newRequest` keep a name from climbing out of `/branches/` with the installation token.
- **The manifest code is not in an error.** `scrub` takes the one-time code out of the conversion's errors, which the callback logs.
- **The App is the operator's.** `internal/api` has no route that creates, replaces or removes it; `supavise github app` does, at the node.
- **Nothing is dropped silently.** Every key of `config.toml` that is not applied, every migration file that is skipped and every function that is left out is a line in the run's log.

## Tests

`go test ./internal/github` runs against an httptest fake of GitHub (`fakegithub_test.go`) and fakes of the branching service, its database and the node (`harness_test.go`). The Management API's side is `go test ./internal/api -run GitHub`. A real node is exercised by `tests/linux/github-smoke.sh` with `tests/linux/github/fake-github.mjs`; real GitHub is never contacted by a test.

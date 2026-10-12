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
| `webhook.go` | `HandleWebhook`: rate limit, signature in constant time, delivery dedupe, dispatch. |
| `pr.go` | A pull request: the branch is created, updated or deleted; the check run and the comment. |
| `production.go` | The push to the production branch, `DeployProduction` (Resync), pushes and deletions of Git branches. |
| `runner.go` | The seven steps (`repoSource`): it is a `branching.SchemaSource` for a new branch and a `branching.Driver` for an existing one. |
| `snapshot.go` | The `supabase` directory at a commit through the tree and blob API: migrations, seed globs, functions (hashes decide what is read). |
| `configtoml.go` | `config.toml` mapped onto `internal/projectconfig`, with `[remotes]`, `env()` and a warning for every key it does not apply. |
| `report.go` | The `Supabase Preview` check run and the one comment per pull request. |
| `queue.go` | One piece of work at a time per pull request or branch; a newer event cancels an older (a job that never starts calls its `dropped` function, so that a run made for it is closed). `[github] max_concurrent_builds` bounds the builds across keys (`acquireBuild`). |

## Rules the code keeps

- **No secret in a log, an event or an answer.** The App's key, client secret and webhook secret, and the users' tokens, are sealed before they reach the registry. Errors from GitHub carry the path without its query.
- **Believe nothing before the signature.** The webhook route has no credentials: it refuses a signature header that cannot be GitHub's and a declared length over 4 MiB before it reads anything, reads at most 16 bodies at a time, rate limits, checks the HMAC, and only then parses. A delivery id seen before does nothing.
- **A token goes to one origin.** `Client` sends the Authorization header to `[github] api_url` only; a `Link: next` to another host ends the listing.
- **Fork pull requests are ignored** unless `[github] allow_forks` is set.
- **A user sees what their own token sees.** Repositories are listed with the user's token (those the user can push to; the list answers the branch pickers for 30 seconds) and a connection checks the repository against that list.
- **A repository's rename is followed, a redirect is never.** The HTTP client does not follow redirects, so a 3xx is an error; `connectionsFor` updates the stored name from the signed payload.
- **The seed is recorded.** `github_branch_links.seeded_at` (migration 1541) is set when a run gets past the seed step; the seed is applied by the first such run, not by the call that creates the branch. A branch somebody else made is marked seeded.
- **A paused branch is woken** (`branching.Service.Wake`) before a commit is driven onto it.
- **A PR's `config.toml` cannot redirect a parent's secret.** On a branch, setting the SMTP host or an OAuth provider's `url` clears the inherited secret unless `config.toml` gives one (`dropInheritedSecrets`).
- **One run reads at most 100 MiB** from the repository in all (`Snapshot.budget`), and migrations are applied under the project's apply lock (`ApplyOptions.Lock`).
- **One comment per project and pull request.** The first line is `<!-- supavise-preview:<project ref> -->`; a comment of another App is not adopted.
- **One writer per branch.** Work on a pull request is serialized; before it drives a branch it waits for the branch to be idle, and the branching service refuses a second operation anyway.
- **Production is deployed from migrations and functions only.** `config.toml` reaches it through a `[remotes]` block whose `project_id` is the project's ref, and the check run of a production deploy carries no log.
- **Nothing is dropped silently.** Every key of `config.toml` that is not applied, every migration file that is skipped and every function that is left out is a line in the run's log.

## Tests

`go test ./internal/github` runs against an httptest fake of GitHub (`fakegithub_test.go`) and fakes of the branching service, its database and the node (`harness_test.go`). The Management API's side is `go test ./internal/api -run GitHub`. A real node is exercised by `tests/linux/github-smoke.sh` with `tests/linux/github/fake-github.mjs`; real GitHub is never contacted by a test.

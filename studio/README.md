# studio

Platform-mode Studio for `supavise`: a build of upstream Studio with `NEXT_PUBLIC_IS_PLATFORM=true` and a few small patches (listed under Patches), packaged like the slim-services `studio` artifact so the systemd unit does not change. Also the mock Management API and the browser spike that tests it. The calls Studio makes are listed in [docs/reference/studio-platform-calls.md](../docs/reference/studio-platform-calls.md).

| Path | What |
|---|---|
| `patches/` | The patches, one `git format-patch` file each, made against `supabase/supabase@94b8b06eb294cf6b217c68d30357566cc8f146d9` (`internal/versions/versions.yaml` `studio.tag`). |
| `build.sh <platform>` | Fetch, patch, install, build, package, verify. Writes `dist/supavise-studio-<tag>-p<N>-<platform>.tar.zst` and a line in `dist/SHA256SUMS`. |
| `Dockerfile.build` | The same build in an Ubuntu 24.04 image (`docker buildx build --target artifact --output type=local,dest=studio/dist`). |
| `ci-prepare.sh` | For a GitHub-hosted runner (needs `SUPAVISE_CI=1` or `GITHUB_ACTIONS`, which `sudo` drops: `sudo env SUPAVISE_CI=1 studio/ci-prepare.sh`): frees disk, adds 6 GB swap, installs the build tools. |
| `verify.sh` | Starts a packaged build on a loopback port and checks it, including the MCP route of patch 0004 (`build.sh` runs it before it writes the artifact). |
| `runtime/` | What goes into the artifact: launcher, entrypoint, runtime substitution and its tests. |
| `placeholders.json` | The per-install values baked into the build as placeholders. |
| `PATCHSET` | Revision `N` in the artifact name; bump it when the patches or `runtime/` change without a new upstream tag, together with `studio.patchset` in `internal/versions/versions.yaml` (`build.sh`, a Go test and the release tool refuse a difference). The build `<tag>-p<N>` is what a node runs and `supavise upgrade` compares, so a bump moves Studio on every node. |
| `mock/` | Stand-in Management API for `spike.sh` (Go, `package main`), with the endpoints behind the OAuth consent page. See `mock/README.md`. |
| `spike.sh`, `spike/` | The CI spike: small stack plus mock plus Studio, driven by Playwright. Besides the project pages it opens Connect > MCP, approves and declines a request on the consent page, and calls `/api/mcp`. |

## The artifact

Same layout and launcher contract as `studio-2026.10.05-sha-94b8b06-r0`:

```
bin/studio              POSIX sh launcher; cd app/; exec node/bin/node apps/studio/docker-entrypoint.mjs
bin/.runtime-env.sh     NODE_OPTIONS default (--max-old-space-size=384), NEXT_TELEMETRY_DISABLED=1; a set variable wins
node/bin/node           Node 22.23.3 from nodejs.org (checksum pinned in build.sh)
app/                    Next standalone output, plus .next/static and public/
share/licenses/         Studio (Apache-2.0), Node, supavise
share/supavise/         build-info.json, runtime-config.json (placeholders and the files that carry them), the patches
```

The upstream dotenv file is not shipped; configuration is environment only.

### Environment at start

| Variable | Required | Meaning |
|---|---|---|
| `PORT`, `HOSTNAME` | no | Next's listener (default 3000 and `0.0.0.0`; the unit should set `HOSTNAME=127.0.0.1`). |
| `NEXT_PUBLIC_API_URL` | yes | The Management API as the browser reaches it, e.g. `https://api.example.com/platform`. A missing `/platform` suffix is added; trailing slashes are dropped. Do not use a host name that starts with `platform.`. |
| `NEXT_PUBLIC_GOTRUE_URL` | yes | The dashboard GoTrue as the browser reaches it, including its path, e.g. `https://api.example.com/auth/v1`. |
| `NEXT_PUBLIC_SITE_URL` | yes | The dashboard's own URL, e.g. `https://studio.example.com` (redirect URLs). |
| `NEXT_PUBLIC_HCAPTCHA_SITE_KEY` | no | Empty by default: no captcha widget (patch 0001). Maps from `[studio] hcaptcha_site_key`. |
| `NEXT_PUBLIC_MCP_URL` | no | The remote MCP endpoint that Connect > MCP shows, e.g. `https://api.example.com/mcp`. Default `http://localhost:8080/mcp`, what Studio shows without it, so a unit that does not set it still starts. |
| `NEXT_PUBLIC_DISABLED_FEATURES` | no | Comma-separated feature keys hidden before login. Default hides sign-up, GitHub, ChatGPT and SSO sign-in, the testimonial and the terms text (patch 0002). |
| `CSP_EXTRA_PROJECT_HOSTS` | no | Host sources serving project APIs, e.g. `*.api.example.com` (patch 0003); each is allowed over `https` and `wss`, and also over `http` and `ws` when the project URLs are `http` (`NIMBUS_PROD_PROJECTS_URL` below names the scheme, `NEXT_PUBLIC_API_URL` when it is not set): a node with `tls.mode = "off"`, where every call to a project host is `http` (`runtime/supavise-runtime-config.mjs`). |
| `NEXT_PUBLIC_CONFIGCAT_PROXY_URL` | no | The origin that serves Studio's feature flags, e.g. `https://api.example.com`: the client fetches `<origin>/configuration-files/configcat-proxy/frontend-v2/config_v6.json`, which the Management API answers (`internal/api/configcat.go`). Empty (the default): Studio reads every flag as off. The fleet sets it to the API origin, which the CSP already allows. Patch 0005 reads one flag of that document, `supaviseBranchClones`. |
| `NIMBUS_PROD_PROJECTS_URL`, `NIMBUS_PROD_PROJECTS_URL_WS` | no | Read per request by Studio's server, not substituted into the build: `https://*.api.example.com` and `wss://*.api.example.com`, or `http://*.api.example.com` and `ws://*.api.example.com` on a node with `tls.mode = "off"` (the fleet takes the scheme of the project URLs). The Edge Function Test tab posts to `/api/edge-functions/test`, which accepts only a function URL on the first one's scheme and apex, with one label of letters, digits and hyphens in front (patch 0010). A platform build without it accepts `*.supabase.co` only. The tab builds its URL from the project's `app_config.endpoint`, which the Management API sets to the project's `<ref>.api.<domain>` host whether or not a custom hostname is active, so a custom hostname does not reach the check. |
| `NEXT_PUBLIC_GITHUB_INTEGRATION_APP_NAME`, `NEXT_PUBLIC_GITHUB_INTEGRATION_CLIENT_ID` | no | The name (`github.com/apps/<name>`) and the client id of the server's GitHub App, which Studio's install and authorize windows are built from (`lib/github.ts`). The unit sets them when the server has an App (`supavise github app create`, or `[github]` in `config.toml`); without one they keep a default that names nobody, because Studio reads an empty value as Supabase's own App. The GitHub surfaces are hidden then (patch 0007). |
| `SUPAVISE_MANAGEMENT_API_URL` | for MCP | The Management API as this process reaches it, e.g. `http://127.0.0.1:7000`. Read for every request to `/api/mcp` (patch 0004), not substituted into the build. Without it, with `SUPAVISE_PROJECT_URL_TEMPLATE` missing, or if either is malformed, the route answers 500 "MCP is not configured". |
| `SUPAVISE_PROJECT_URL_TEMPLATE` | for MCP | The public API URL of a project with `{ref}` where its ref goes, e.g. `https://{ref}.api.example.com`. The MCP tool `get_project_url` answers from it. |
| `OPENAI_API_KEY` | for AI | The key of the model provider behind the AI Assistant and the SQL editor's AI features. Read on the server for each request (`@ai-sdk/openai`). Without it `/api/ai/sql/check-api-key` answers `{"hasKey": false}` and Studio says that no provider is set (patch 0006). Maps from `[studio.ai] api_key_file`; a provider that takes no key gets the placeholder `unused`. |
| `OPENAI_BASE_URL` | no | The root of an OpenAI-compatible API, for example `https://openrouter.ai/api/v1` or `http://127.0.0.1:11434/v1`. Default OpenAI's. Must not be empty when set. Maps from `[studio.ai] base_url`. |
| `SUPAVISE_AI_MODEL` | no | The model of the Assistant's chat. Replaces the three OpenAI names upstream fixes in code, and the model picker shows it (patch 0006). Maps from `[studio.ai] model`. |
| `SUPAVISE_AI_COMPLETION_MODEL` | no | The model of the small calls (titles, filters, cron and policy suggestions, inline completion). Default `SUPAVISE_AI_MODEL`. |
| `SUPAVISE_AI_API` | no | `chat` sends `/chat/completions`, which every gateway offers; anything else keeps upstream's `/responses`. Supavise sets `chat` when `OPENAI_BASE_URL` is set, unless `[studio.ai] api` is `responses`. |
| `SUPAVISE_AI_REASONING_EFFORT` | no | `none`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max`, sent with every request. A model named by the variables above gets no effort otherwise. |
| `SUPAVISE_STUDIO_SKIP_RUNTIME_CONFIG=1` | no | Start without substitution (debugging). |

A missing required value or an unsafe one exits with status 78 and a message naming it. The `SUPAVISE_*` and `OPENAI_*` variables are not placeholders and are not checked at start. Values may only contain `A-Za-z0-9._~:/@%+,*=-`, so they cannot break out of a JS string, JSON string or HTML attribute.

### How the substitution works

Next inlines `NEXT_PUBLIC_*` and evaluates the CSP at build time, so `build.sh` builds with unique placeholder strings (`https://supavise-placeholder-api.invalid/platform`, ...). `runtime/supavise-runtime-config.mjs prepare` keeps a pristine `<file>.supavise-tpl` copy of every file that contains one (about 280 files) and fails the build if a placeholder did not survive the bundler. At every start the entrypoint rewrites those files from the copies, so starting again with other values works. The API and GoTrue placeholders carry a marker path so the bare origin used in the CSP is replaced by the bare origin of the real URL (a CSP source with a path only matches that exact path).

**Unit requirements.** The artifact directory (`app/`) must be writable by the service user at start (for example `ReadWritePaths=` under `ProtectSystem=strict`), because Next also writes `app/apps/studio/.next/cache` (image cache). The launcher exits 78 (`EX_CONFIG`) on a missing or unsafe value and mirrors the child's exit status, so a SIGTERM stop ends as 143: the unit needs `RestartPreventExitStatus=78` (a bad value would otherwise restart-loop) and `SuccessExitStatus=143`.

`/_next/static` chunks keep their file names when a value changes and Next serves them as immutable, so after changing `NEXT_PUBLIC_API_URL` or `NEXT_PUBLIC_GOTRUE_URL` users need a hard reload once.

### Studio's own routes the proxy answers

Studio's `/api/incident-banner` route answers 500 without an incident.io key, which delays sign-in by about 22 s (docs/reference/studio-platform-calls.md, "Findings from running Studio"). The artifact is not changed for this: Supavise's proxy answers `GET studio.<domain>/api/incident-banner` with `{"incidents":[]}` (`internal/proxy`). Running the artifact without the proxy (`verify.sh`) shows the 500; the spike's browser script answers the route itself.

`/api/mcp` is the remote MCP endpoint in platform mode (patch 0004; Supavise's proxy forwards `api.<domain>/mcp` to it after it has checked the caller's token). Upstream answers 404 for it in platform mode, where Supabase runs MCP as its own service.

`/api/incident-status` answers 500 without a statuspage.io key, so the console logged the failure and the status chip of `/support/new` never resolved. The proxy answers `GET` and `HEAD studio.<domain>/api/incident-status` with `200 []` (`internal/proxy/studio.go`), as it does for the banner.

The proxy also rewrites Studio's Content-Security-Policy (`internal/proxy/studio.go`):

- The browser cannot reach `usercentrics.eu`, the consent-banner vendor Studio calls on every page load.
- When the node's public dashboard URL is `http` (`tls.mode = "off"`), the `upgrade-insecure-requests` directive is removed from every Studio response. Studio's production build sends it, and a browser that honors it asks for every script, style sheet and font of a page, and for the chunks a Web Worker imports, over `https`, where a plain-http node has no listener: the page stayed blank (`net::ERR_SSL_PROTOCOL_ERROR`) and GraphiQL's worker failed to load. A node behind a TLS terminator that names its `https` dashboard URL (`[api] dashboard_url`) keeps the directive.

## Patches

Applied with `git am --3way` onto the pinned commit. Each carries its own tests where it has code to test (`csp.test.ts`, `enabled-features/index.test.ts`, `lib/api/platform-mcp.test.ts`, `lib/ai/self-hosted-model.test.ts`, `lib/api/edgeFunctions.test.ts`).

- **0001 hCaptcha only with a site key.** Covers every page that mounts the widget with the build-time key (`SignInForm`, `ForgotPasswordWizard`, `SignInSSOForm`, `SignUpForm`, `ChangeEmailAddress`, `/new`) through a module-level `HCAPTCHA_SITE_KEY` that each checks. Not touched: the four billing dialogs, which need Stripe and are not reachable without billing.
- **0002 feature flags through `NEXT_PUBLIC_DISABLED_FEATURES`.** Upstream resolves `dashboard_auth:*` from `profile.disabled_features` (unavailable before login) and the `ENABLED_FEATURES_*` override (a no-op when `IS_PLATFORM` is true). The patch merges a build-time list into the static set, so it covers the six `dashboard_auth:*` flags before login. The SSO button is the one flag a node turns on at run time: while the dashboard has a SAML identity provider (`supavise sso add`), `internal/fleet` starts Studio without `dashboard_auth:sign_in_with_sso` in the list, and restarts it when the first provider is added or the last removed.
- **0003 extra project hosts in the platform CSP.** `CSP_EXTRA_PROJECT_HOSTS`, hosts validated against a strict pattern. It is read when `next.config.ts` computes the headers, which is build time, hence the placeholder. `verify.sh` checks the substituted CSP.
- **0004 the MCP route in platform mode.** Studio already bundles the MCP server package with every tool, and Supavise has no process that could run it as hosted does, so `pages/api/mcp` serves it (`lib/api/platform-mcp.ts`). It exists because this deployment has no separate MCP service, so it is not an upstreamable change. Tests: `lib/api/platform-mcp.test.ts` (vitest) and `verify.sh`.
  - Each request needs `POST` and `Authorization: Bearer <token>` (`405` and `401` otherwise). The route builds a server for that request from the package's Management API platform (`createSupabaseApiPlatform`) with the token and `SUPAVISE_MANAGEMENT_API_URL`. The API, not Studio, decides what the token may do. The token is never logged or stored.
  - The query is checked strictly, and anything else is a `400`. `project_ref` is 20 lowercase letters and scopes the server to that project, which hides the account tools. `read_only` is `true` or `false` (default `false`); `true` hides the write tools and sends `read_only` with every query. `features` names groups the package has (`storage` is opt-in). `skip_elicitations` is accepted and has no effect, because the stateless JSON transport cannot ask the client anything.
  - `get_project_url` is filled from `SUPAVISE_PROJECT_URL_TEMPLATE`. The package derives a hosted domain from the API host (`supabase.red` for any host it does not know), which is wrong here. The project ref comes from a tool argument, so it is checked before it goes into the template.
  - The patch adds `/mcp` to the allowlist of API routes (`lib/hosted-api-allowlist.ts`) and sets the body limit of the route to 8 MB, because deploying an Edge Function sends the files. Only the pages router changes; the TanStack twin of the route (`routes/api/mcp`) is not part of this build.
  - `search_docs` calls `supabase.com/docs/api/graphql` from the node, as hosted does.
- **0005 the branching dialog and menu, and the price block of Restore to new project.** Four changes, each keeping hosted's behavior when the deployment signals nothing. Create branch (`CreateBranchModal.tsx`) prints the dollar prices of compute and disk and the tax notice only when `billing:all` is enabled, and Supavise disables it. Its Include data switch is enabled by the ConfigCat flag `supaviseBranchClones` (the node's flag document, `internal/api/configcat.go`) as well as by the PITR add-on, because a node clones the parent database itself and needs no add-on for that. The branch menu (`Overview.tsx`) lists Create pull request only when the project has a GitHub connection: the item linked to github.com for every branch that had a git branch. The confirmation and create dialogs of Database > Backups > Restore to new project end in `AdditionalMonthlySpend.tsx`, hosted's compute and disk prices and the claim that the disk will be 1.5 times larger; it renders nothing unless `billing:all` is enabled. `build.sh` checks that all the patches apply at the pin (`git am --3way`); `verify.sh` looks in the packaged build's client chunks for the flag name `supaviseBranchClones`, which proves that the patch is in the build and not that the dialogs render correctly; the dialogs are for a browser run. The reason is in [design.md](../docs/design.md) section 10.
- **0006 the operator's model provider for the AI features.** Upstream's AI routes call OpenAI with fixed model names, and in platform mode the client never asks the server whether a key exists (`checkOpenAIKey` answers `hasKey: true` for the hosted dashboard), so the Assistant showed a chat box that failed on a node without a key. It is not upstreamable as it stands: it serves a deployment that lets its operator choose the provider. `lib/ai/self-hosted-model.ts` reads the variables of the table above on the server for each request:
  - `getModel` takes the model id, the API (`/chat/completions` through `openai.chat`, or upstream's `/responses`) and the provider options from `resolveOpenAICall`. The Assistant's chat uses `SUPAVISE_AI_MODEL`; the small calls (`DEFAULT_COMPLETION_MODEL` and `LOGS_REWRITE_MODEL` in upstream) use `SUPAVISE_AI_COMPLETION_MODEL`, then `SUPAVISE_AI_MODEL`. A chat request leaves out `store`, a `/responses` setting. Without the variables the call is upstream's.
  - `/api/ai/sql/check-api-key` is added to the platform allowlist (`lib/hosted-api-allowlist.ts`), answers `{hasKey, model}` (the model only when the operator chose one), and `checkOpenAIKey` asks it in platform mode too. The route needs a signed-in caller, so `verify.sh` expects `401` without a token.
  - The model picker shows the operator's model in place of the three OpenAI names, and the messages in the Assistant, the snippet dialog and the rename dialog point to the server's administrator instead of telling the user to add an `OPENAI_API_KEY`.
  - Not changed: the Assistant's tools that come from the remote MCP server (docs search, advisors, logs and the `list_*` tools) need an OAuth or personal access token, and the dashboard session is not one (`internal/api/mcp.go`), so Studio runs without them (`getTools` logs the refusal and goes on). `/api/ai/docs`, the legacy docs bot, needs a Supabase project that holds the docs and is not set up.
- **0007 the GitHub surfaces follow the platform.** Adds the feature key `integrations:github` (default on, like `integrations:vercel`). The Management API lists it in `disabled_features` while the server has no GitHub App, and Studio then hides the GitHub section of the project and organization integration settings, the Connections section of the account preferences, the GitHub status of the Branches side menu and the "Sync with a GitHub branch" block of the branch dialogs (the Create pull request action of a branch needs a GitHub connection too, which is patch 0005's). Without it Studio's buttons open Supabase's own GitHub App, which a platform run by someone else cannot receive webhooks from. It could go upstream as a flag. Checked by the crawler and the spike; no unit test of its own.

- **0008 Monaco from the public folder.** `configureMonacoLoader` loaded Monaco from cdnjs whenever `IS_PLATFORM` is true, so on a network without internet access every code editor (the Auth email templates, the Logs explorer, the SQL and function editors) stayed on a spinner, and the template page had no Reset template button. Every build ships `public/monaco-editor/vs`, which is where the self-hosted build loads it, and the platform build now does the same. The CSP needs no change (`script-src` and `worker-src` allow `'self'` and `blob:`). `verify.sh` fetches `/monaco-editor/vs/loader.js` and looks for the cdnjs path in the client chunks.
- **0009 Wait for the project ref before redirecting.** A page that Next prerendered renders once with an empty route query. The SQL editor's index page redirected to `/project/undefined/sql/new` when the local-storage query for its history resolved first, and Studio's project-not-found handling then landed on the organization page; the Compute layout (a redirect to the project home while the `compute` flag is off) and the project webhooks page (a redirect to Settings > General while the flag is off) did the same with `/project/undefined`. Their effects now wait for the ref. Found by the crawl, read from the source; no browser run has reproduced the race.
- **0010 The Edge Function tester on a node without TLS.** `isValidEdgeFunctionURL` built an `https`-only expression from `NIMBUS_PROD_PROJECTS_URL` and allowed lowercase letters only, so `POST /api/edge-functions/test` answered 400 "Provided URL is not a valid Supabase edge function URL" for every function of a node whose project URLs are `http`, and for a project host with a digit or a hyphen. `isNimbusEdgeFunctionURL` takes the scheme from the value and allows digits and hyphens in the one label. Tests: `lib/api/edgeFunctions.test.ts` (vitest).
- **0011 The Edge Functions status asks the deployment.** The Edge Functions row of the project home probed `https://obuldanrptloktxcffvn.supabase.co/functions/v1/health-check`, a function Supabase runs on its own platform. It reports that platform, it is unreachable without internet access, and a failed probe made the row, and so the whole project, read Unhealthy and poll every five seconds for ever. The row now asks `GET /v1/projects/{ref}/health?services=functions`; the Management API answers for the node's Edge Runtime (`internal/api/projects.go`, `functionsHealth`) and with `DISABLED` when `[functions]` is off, which Studio shows without alarm. The spec's service enum has no such name, so the entry is added only when the request names it.

Patches 0008 to 0011 are in the patch set of 0005 to 0007, which no release has shipped, so `PATCHSET` stays at 4. None of the four could be tested in a Studio build here: they are checked by applying the whole set with `git am --3way` at the pin and, for 0010, by a node check of the expression; the crawl and `verify.sh` exercise them in CI. The artifact name stays `...-p4`, so a run that picks the newest green build of that name (`tests/linux/crawl/fetch-studio.sh` without `--fresh`, as a manual dispatch of the crawl does) can take a build made before 0008 to 0011. A push that changes `studio/` passes `--fresh`; for a manual run, use a `studio.yml` run of the commit under test.

## Build on CI

```bash
sudo studio/ci-prepare.sh          # GitHub-hosted runner only
studio/build.sh linux-amd64        # or linux-arm64, on a machine of that architecture
```

`build.sh` downloads its own checksum-pinned Node and pnpm, so it needs only `git`, `curl`, `tar`, `zstd`, `python3` and a C toolchain (`build-essential`: the pnpm install compiles native modules such as `libpg-query`). `spike.sh` needs `go` (or `SPIKE_MOCK_BIN`), `node` with `npm`, and `sudo` without a password or root for `playwright install-deps`.

`build.sh` runs `next build` with one static-generation worker (`STUDIO_BUILD_WORKERS`) and a 4 GB V8 heap per node process (`STUDIO_BUILD_HEAP_MB`). Expect about 9 GB of disk and a peak near 8 GB of RAM for `next build` (Turbopack, measured on a Mac); `build.sh` warns if RAM plus swap is under 11 GB. The artifact is about 75 MB compressed. `STUDIO_UNTIL=prune` runs only the fetch, patch, prune and lockfile check.

## Tests

```bash
node --test studio/runtime/test/                     # substitution (placeholders, runtime config)
go test ./studio/mock/                               # the mock (needs no Studio)
```

The tests that the patches carry run with Studio's own vitest, inside the build's work tree (`STUDIO_KEEP_WORK=1`, then `pnpm --filter studio exec vitest --run lib/api/platform-mcp.test.ts` in its `prune` directory). `build.sh` does not run them.

The `studio` workflow's `build` job runs `ci-prepare.sh`, `build.sh` and then `spike.sh` on amd64 and arm64 runners. To test packaging without the Next build, set `STUDIO_PREBUILT=<apps/studio with .next/standalone>` and run `studio/build.sh <platform>`.

## Limits

- Studio calls `api.usercentrics.eu` on every page load even without a ruleset id (it fails soft). The proxy's CSP rewrite blocks it; removing the cause needs a fourth patch that skips Usercentrics initialization when `NEXT_PUBLIC_USERCENTRICS_RULESET_ID` is unset.
- The spike does not exercise Realtime, Edge Functions, Logs, Advisors, Integrations or Reports (see the reference for the pages it visits).

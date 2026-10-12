# Studio platform calls

What the platform-mode Studio asks of the Management API, GoTrue and the project hosts, and which answers it needs. It describes Studio at `supabase/supabase@94b8b06` (`internal/versions/versions.yaml`, `studio.tag` `2026.10.05-sha-94b8b06`), built with `NEXT_PUBLIC_IS_PLATFORM=true`.

To refresh it after a Studio bump, run `studio/spike.sh`: it writes `request-log.jsonl` and `calls.md` into `$SPIKE_OUT`, and the studio mock's `summarize` command prints the rows of calls seen. The list of calls below is checked against the source and against a real node by two tools (`docs/development.md`, "The Studio crawl and the call gate").

## Method

- **The static list.** `studio/scan-calls.mjs` parses every TypeScript and JavaScript file of `apps/studio/{data,lib,components,hooks,state,pages,app,routes}` and `packages/{common,ui,ui-patterns}` of the pinned commit with the patches applied (tests, mocks and declaration files left out; `pages/api/**` and `lib/api/self-hosted/**` left out, because the platform build answers 404 for Studio's own `/api` routes except the allow-list under "Studio's own /api routes"). It finds the calls of the openapi-fetch client (`get`, `post`, `put`, `patch`, `del`, `head`, `client.GET` and the others, including an alias such as `const deleteRequest = del as ...`), of the legacy `fetchGet`, `fetchPost`, `fetchHeadWithTimeout` and friends, and `fetch()` with a path under `/platform`, `/v1` or `/v2`. The first argument is resolved to a path through string and template literals (`${x}` becomes `{x}`), conditionals (with `IS_PLATFORM` true), constants and members of constant objects, imported constants, small functions that return a path, and the call sites of a function that takes the path as a parameter (`executeAnalyticsSql`, the pg-meta `query` source). A leading `${API_URL}` is the platform build's `/platform`. A query module whose `enabled` is false when `IS_PLATFORM` is true is skipped (`deployment-mode`, the CLI release version, the local S3 keys). Result, in `studio/calls.json`: 387 (method, path) pairs over 292 paths, of which 12 are reached through a computed path, and 4 strings that look like API paths and are not the argument of a call the scan understands (`mentions`: the MCP URL builder, the GraphiQL URL and two prefixes). The specs hold 446 paths; Studio never calls the others. Priorities: P0 breaks sign-in, project list, table editor or SQL editor; P1 degrades a project or organization page; P2 is billing, integrations and other pages outside the P0 flows.
- **The gate.** `internal/api/studio_calls_test.go` joins `studio/calls.json` with the route table of the server: a call that no hand-written handler answers is answered by a stub of the spec (an empty list, an empty object, the answer Studio takes for "it worked") or by the unknown-route fallback. Today that is 198 of the 387 pairs, each listed in `tests/linux/crawl/allowlist.json` with the reason it is acceptable (billing screens that `billing:all` hides, hosted features in `disabled_features`, and calls whose handler a release has yet to write). The test fails for a call that is not listed, which is how a handler that regresses to a stub, or a new Studio call, is noticed, and for a listed call that a handler answers now, so the list only shrinks.
- **The crawl.** `tests/linux/studio-crawl.sh` opens every page of the Studio build in a headless Chromium against a real node, with fixtures, and records every response. It is the authority on what the scan cannot see (a call made through a wrapper, a path built at run time), and it checks the rows of this file that say what Studio tolerates: it fails on a stub answer, a 4xx or a 5xx that the allow-list does not explain, on a page or console error, and on the texts that mean a dead end. Its probes (`tests/linux/crawl/probes.mjs`) drive the flows this file describes, such as the Create branch dialog, the merge page, Resume, custom domains, GraphiQL and the Edge Function tester, and judge them by the state of the server. Until the first green run on `main` the statements below about what Studio tolerates come from reading the code, and "Findings from running Studio" is the one place they are confirmed by running it; a statement the crawl contradicts is a defect of this file.
- Not covered by the scan: GoTrue, calls from the browser straight to a project, and Studio's own `/api/*` routes. Each has a section below.

## Wire contract

- **Base URL.** `NEXT_PUBLIC_API_URL` ends in `/platform` (hosted: `https://api.supabase.com/platform`). The openapi-fetch client strips that suffix and uses absolute paths, so `/platform/...`, `/v1/...` and `/v2/...` all hang off one origin. A few callers use the value as is, and `common` adds the suffix when missing. The Studio artifact's launcher normalizes the configured URL to end in `/platform`. Do not give the API host a name that starts with `platform.`: the client removes the first occurrence of the text `/platform`, and `https://platform.example.com/platform` contains an earlier one.
- **Request headers on every call.** `Authorization: Bearer <GoTrue access token>` (when a session exists), `X-Request-Id`, `Accept: application/json`, `credentials: 'include'`, `referrerPolicy: no-referrer-when-downgrade`. `GET /platform/profile` also sends `Version: 2`. Calls to `/platform/pg-meta/*` add `x-connection-encrypted` (the project's `connectionString`) and `x-pg-application-name` (`supabase/dashboard`, or `supabase/dashboard-query-editor` when the statement timeout is disabled), plus a client-side `?key=<query key>` parameter.
- **`x-connection-encrypted` is untrusted input.** Studio only checks that the project's `connectionString` is a non-empty string (`pgMetaGuard`) and sends it back unchanged. The API server returns an opaque, non-secret token there (random or constant; never the database URL, encrypted or not, because it reaches every dashboard user). On `/platform/pg-meta/{ref}/query` and the other pg-meta routes it derives the postgres-meta connection from the authorized `{ref}` on the server, after checking that the caller may access that project, and never decrypts, parses or forwards the client-supplied header. Otherwise a user of project A could replay A's value on `/platform/pg-meta/{B}/...` and bypass the per-ref check. The mock does this (`studio/mock/pgmeta.go`).
- **CORS.** The browser calls the API cross-origin (`studio.<domain>` to `api.<domain>`) with credentials mode `include` and non-simple headers. Every route needs an `OPTIONS` answer, and responses need `Access-Control-Allow-Origin` echoing the Studio origin (a literal `*` is rejected with credentials), `Access-Control-Allow-Credentials: true`, `Access-Control-Allow-Headers` covering `authorization, content-type, x-request-id, x-connection-encrypted, x-pg-application-name, version`, methods `GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS`, and `Access-Control-Expose-Headers: Retry-After, X-RateLimit-Reset, X-Total-Count`.
- **Success.** Any 2xx counts as success and Studio does not branch on the exact code; match the spec's codes anyway, because the CLI is strict. An empty 2xx body is fine (`204`, or `Content-Length: 0`).
- **Errors.** Any non-2xx is an error. Studio reads a string from the JSON field `message` (or `msg`), takes the code from the HTTP status, and reads `Retry-After` or `X-RateLimit-Reset` for 429. Use the envelope `{"message": "..."}`. Two statuses have behavior attached: a **401 on `GET /platform/profile` signs the user out**, and a `GET /platform/profile` error whose `message` is exactly `User's profile not found` makes Studio `POST /platform/profile` to create the profile.
- **Identifiers.** `{slug}` is the organization slug, `{ref}` the 20-letter project ref. `organization_id` and `id` fields are numbers, so the registry needs a numeric id per organization and project besides the ref.
- **Disable levers.** ConfigCat (`NEXT_PUBLIC_CONFIGCAT_SDK_KEY`, `NEXT_PUBLIC_CONFIGCAT_PROXY_URL`), PostHog (`NEXT_PUBLIC_POSTHOG_KEY`), Sentry (`NEXT_PUBLIC_SENTRY_DSN`) and Usercentrics (`NEXT_PUBLIC_USERCENTRICS_RULESET_ID`) stay off when their `NEXT_PUBLIC_*` keys are unset; the artifact build sets none of them. With `NEXT_PUBLIC_ENVIRONMENT` set to `local` or `staging`, Studio grants telemetry consent automatically, so never set it to either (`studio/build.sh` sets `prod`). `GET /platform/telemetry/feature-flags` is requested on every page and cannot be turned off; an empty object or a 404 is tolerated, but the node answers with the one flag it must fix (see its row below). Billing screens are hidden by returning `disabled_features: ["billing:all", ...]` from `GET /platform/profile`.

## Dashboard sign-in (GoTrue)

`NEXT_PUBLIC_GOTRUE_URL` points at one `@supabase/auth-js` client (`packages/common/gotrue.ts`; storage key `supabase.dashboard.auth.token`, `localStorage`). These calls go to `supavise-gotrue@system`, not to the Management API. They carry no `apikey` header, so a front that insists on one breaks them. GoTrue answers CORS itself.

| Prio | Call | Used for |
|---|---|---|
| P0 | `POST /token?grant_type=password` | e-mail and password sign-in; the body carries `gotrue_meta_security.captcha_token` only when an hCaptcha key is set |
| P0 | `POST /token?grant_type=refresh_token` | session refresh (runs in the background when the token nears expiry) |
| P0 | `POST /logout` | sign-out |
| P1 | `GET /user` | profile menu and account preferences; also the fallback of auth-js `getClaims` for an HS256 token. The MFA assurance-level check after sign-in is computed locally and sends no request |
| P1 | `GET /.well-known/jwks.json` | `getClaims` when the project signs with an asymmetric key |
| P2 | `POST /recover`, `PUT /user` | forgot password, account preferences |
| P2 | `GET/POST /factors`, `POST /factors/{id}/challenge`, `/verify`, `DELETE /factors/{id}` | MFA enrollment in account settings |
| P2 | `POST /signup`, `/otp`, `/verify`, `/sso`, `/authorize`, `/token?grant_type=id_token` | sign-up, magic link, SSO, GitHub and ChatGPT sign-in; hidden by the `dashboard_auth:*` flags in the Supavise build |

After sign-in, `GET /platform/profile` must accept the GoTrue access token (a 401 signs the user out again).

## The P0 flows

1. **Sign-in page** (`/sign-in`): GoTrue `POST /token?grant_type=password`, then the session lands in `localStorage`. No Management API call happens before login. With the Studio patches the page shows e-mail sign-in only and renders no captcha.
2. **App shell after login:** `GET /platform/profile` (header `Version: 2`; Studio waits for it and the permissions query), `GET /platform/profile/permissions` (gates every action check), `GET /platform/organizations` (starts once the profile has loaded). In parallel and not blocking: `GET /platform/telemetry/feature-flags`, `GET /platform/notifications`, `GET /platform/projects-resource-warnings`.
3. **Project list** (`/organizations`, `/org/{slug}`): `GET /platform/organizations/{slug}`, `GET /platform/organizations/{slug}/projects?limit=96&offset=0&sort=name_asc[&search=&statuses=]` (the cards need `pagination.count` and each project's `databases`, `status`, `ref`, `name`, `region`, `cloud_provider`, `inserted_at`, `is_branch`, `integration_source`), `GET /platform/projects` (project switcher), and the organization's `entitlements`, `billing/subscription`, `usage`, `members` and `roles` (organization pages degrade without them).
4. **Open a project, table editor** (`/project/{ref}/editor`): `GET /platform/projects/{ref}` must return a non-empty `connectionString` (see the wire contract). `status` must be `ACTIVE_HEALTHY`, or Studio shows the connecting screen and polls `GET /platform/projects/{ref}/status` and `HEAD /platform/projects/{ref}/api/rest`; `is_hibernating: true` makes it `POST .../wake` first. Then `GET /platform/projects/{ref}/settings`, and **every table, column, schema, role, policy and row read is `POST /platform/pg-meta/{ref}/query`** with SQL in the body. Also on open: `GET /v1/projects/{ref}/api-keys`, `POST /platform/projects/{ref}/api-keys/temporary`, `GET /v1/projects/{ref}/branches` (return `[]` if there are none), `GET /v1/projects/{ref}/health`, `GET /v2/projects/{ref}/config`, `GET .../config/postgrest`, `GET .../config/storage`, `GET /platform/projects/{ref}/content`, `GET .../analytics/endpoints/usage.api-counts`.
5. **SQL editor** (`/project/{ref}/sql/...`): `POST /platform/pg-meta/{ref}/query` with body `{"query": string, "disable_statement_timeout": boolean}` (an optional `explain <sql>` preflight uses `?key=preflight-check`); snippets use `GET/PUT/POST/DELETE /platform/projects/{ref}/content` and `.../content/folders`. The response must be the JSON array of row objects exactly as pg-meta returns it. Pass pg-meta's status code and error body (`{error, formattedError, ...}`) through unchanged: the editor shows `formattedError` and parses the `LINE n:` text.

Pitfalls in the required fields of the P0 responses (the full shapes are in the generated types):

| Response | Pitfalls |
|---|---|
| `ProfileResponse_Output` | `id` number, `gotrue_id` = GoTrue user id, `auth0_id` string (Studio checks `startsWith('github')` for the avatar), `username`, `primary_email`, `is_alpha_user`, `free_project_limit` (number or null); optional `disabled_features` (string array, merged with the build-time list) |
| `AccessControlPermission[]` | `actions`, `resources`, `condition`, `organization_slug`, `organization_id`, `project_ids`, `project_refs`, `restrictive`. `%` is a wildcard and `condition: null` is unconditional; an empty list denies every action |
| `OrganizationResponse_Output[]` | `plan` is an inline object `{id: 'free'|'pro'|'team'|'enterprise'|'platform', name}`; `restriction_data` and `restriction_status` are `null` when unrestricted; `opt_in_tags` array (the AI data-sharing level, saved by `PATCH /platform/organizations/{slug}`; see "The AI Assistant"); `slug` and numeric `id` |
| `OrganizationProjectsResponse_Output` | `{pagination: {count, limit, offset}, projects: [...]}`; each project needs `databases[]` (`identifier`, `region`, `status`, `type: 'PRIMARY'`, `cloud_provider`) and the card fields above |
| `ListProjectsPaginatedResponse_Output` | `{pagination, projects: [...]}`; each project adds `organization_id`, `organization_slug`, `subscription_id`, `is_branch_enabled`, `is_physical_backups_enabled`, `preview_branch_refs` |
| `ProjectDetailResponse_Output` | `connectionString` (see the wire contract), `restUrl`, `db_host`, `status` (enum: `ACTIVE_HEALTHY`, `COMING_UP`, `INACTIVE`, `UNKNOWN`, `GOING_DOWN`, `INIT_FAILED`, `REMOVED`, `RESTORING`, `UPGRADING`, `PAUSING`, `RESTORE_FAILED`, `RESTARTING`, `PAUSE_FAILED`, `RESIZING`, `ACTIVE_UNHEALTHY`), `subscription_id` string, numeric `id` and `organization_id`; optional `is_hibernating`, `dbVersion` |
| `ProjectSettingsResponse_Output` | `db_host`, `db_dns_name`, `db_name`, `db_user`, `db_port`, `ssl_enforced`, `status`, `ref`, `name`, `region`, `cloud_provider`, `inserted_at`; optional but used: `app_config` (endpoint and protocol build the project URL), `jwt_secret`, `service_api_keys` |
| `GET /platform/projects/{ref}/status` | body `{"status": "..."}`; the spec documents no body but Studio reads `data.status` |

## Calls that need a real answer

These are the documented routes for which an empty stub is not enough: the page needs real data, or a mutation must persist or act. Every other documented route gets the spec's empty value from the generic stub (arrays become `[]`, objects their required fields with neutral values).

| Prio | Method | Path | What Studio needs |
|---|---|---|---|
| P1 | GET | `/platform/auth/{ref}/config` | 200 `GoTrueConfigResponse` |
| P1 | PATCH | `/platform/auth/{ref}/config` | 200 `GoTrueConfigResponse` |
| P1 | PATCH | `/platform/auth/{ref}/config/hooks` | 200 `GoTrueConfigResponse` |
| P1 | POST | `/platform/auth/{ref}/invite` | 201 no body |
| P1 | POST | `/platform/auth/{ref}/magiclink` | 201 no body |
| P1 | POST | `/platform/auth/{ref}/otp` | 201 no body |
| P1 | POST | `/platform/auth/{ref}/recover` | 201 no body |
| P1 | POST | `/platform/auth/{ref}/templates/{template}/reset` | 200 `GoTrueConfigResponse`: the project's config after the template's subject and body are cleared (`internal/api/auth_extras.go`). Studio caches the answer as the config, so the Site URL and the other templates must be in it. |
| P1 | POST | `/platform/auth/{ref}/users` | 201 `CreateUserResponse_Output` |
| P1 | PATCH | `/platform/auth/{ref}/users/{id}` | 200 `UpdateUserResponse_Output` |
| P1 | DELETE | `/platform/auth/{ref}/users/{id}` | 200 no body |
| P1 | DELETE | `/platform/auth/{ref}/users/{id}/factors` | 200 no body. Lists the user's factors through GoTrue's admin API (`GET /admin/users/{id}/factors`) and deletes each (`DELETE .../factors/{factor_id}`); `502` naming the factor GoTrue refuses (`auth_extras.go`). |
| P1 | POST | `/platform/auth/{ref}/validate/spam` | 200 `ValidateSpamResponse_Output`. Still the stub's `{rules: []}`: no rule ever matches, because the SpamAssassin rules are hosted's. Nothing depends on it. |
| P1 | POST | `/platform/cli/login` | 201 no body |
| P0 | GET | `/platform/organizations` | 200 array of `OrganizationResponse_Output` |
| P0 | GET | `/platform/organizations/{slug}` | 200 `OrganizationSlugResponse_Output` |
| P2 | PATCH | `/platform/organizations/{slug}` | 200 `UpdateOrganizationResponse_Output`; `name` and `opt_in_tags` (an array of `AI_SQL_GENERATOR_OPT_IN`, `AI_LOG_GENERATOR_OPT_IN`, `AI_DATA_GENERATOR_OPT_IN`; anything else is 400; `[]` clears) are saved, the other fields of the body are not. Owner only |
| P2 | HEAD | `/platform/organizations/{slug}/billing/invoices` | 200 no body |
| P1 | GET | `/platform/organizations/{slug}/members` | 200 array of `Member_Output` |
| P1 | GET | `/platform/organizations/{slug}/members/invitations` | 200 `InvitationResponse_Output` |
| P1 | POST | `/platform/organizations/{slug}/members/invitations` | 201 inline object |
| P1 | DELETE | `/platform/organizations/{slug}/members/invitations/{id}` | 200 no body |
| P1 | GET | `/platform/organizations/{slug}/members/invitations/{token}` | 200 `InvitationByTokenResponse_Output` |
| P1 | POST | `/platform/organizations/{slug}/members/invitations/{token}` | 201 no body |
| P1 | GET | `/platform/organizations/{slug}/members/mfa/enforcement` | 201 `MfaStatusResponse_Output` |
| P1 | PATCH | `/platform/organizations/{slug}/members/mfa/enforcement` | 201 `MfaStatusResponse_Output` |
| P1 | GET | `/platform/organizations/{slug}/members/reached-free-project-limit` | 200 array of `MemberWithFreeProjectLimit_Output` (requires `free_project_limit`, `primary_email`, `username`) |
| P1 | PATCH | `/platform/organizations/{slug}/members/{gotrue_id}` | 200 no body |
| P1 | DELETE | `/platform/organizations/{slug}/members/{gotrue_id}` | 200 no body |
| P1 | DELETE | `/platform/organizations/{slug}/members/{gotrue_id}/roles/{role_id}` | 200 no body |
| P0 | GET | `/platform/organizations/{slug}/projects` | 200 `OrganizationProjectsResponse_Output` (requires `pagination`, `projects`) |
| P0 | POST | `/platform/pg-meta/{ref}/query` | 200 JSON array of row objects (pg-meta passthrough). Failure: 4xx/5xx JSON `{error, formattedError, ...}` as pg-meta sends it. Spec documents 201 without a body |
| P0 | GET | `/platform/profile` | 200 `ProfileResponse_Output` |
| P0 | POST | `/platform/profile` | 201 `ProfileResponse_Output` |
| P1 | GET | `/platform/profile/access-tokens` | 200 array of `AccessToken_Output` |
| P1 | POST | `/platform/profile/access-tokens` | 201 `CreateAccessTokenResponse_Output` |
| P1 | DELETE | `/platform/profile/access-tokens/{id}` | 200 `AccessToken_Output` |
| P0 | GET | `/platform/profile/permissions` | 200 array of `AccessControlPermission` |
| P1 | GET | `/platform/profile/scoped-access-tokens` | 200 `GetScopedAccessTokensResponse_Output` |
| P1 | POST | `/platform/profile/scoped-access-tokens` | 201 `CreateScopedAccessTokenResponse_Output` |
| P1 | GET | `/platform/profile/scoped-access-tokens/{id}` | 200 `GetScopedAccessTokenResponse_Output` |
| P1 | DELETE | `/platform/profile/scoped-access-tokens/{id}` | 200 no body |
| P0 | GET | `/platform/projects` | 200 `ListProjectsPaginatedResponse_Output` (requires `pagination`, `projects`) |
| P0 | POST | `/platform/projects` | 201 with the new project (`ref`, `anon_key`, `service_key`, `endpoint`). The form (`ProjectCreationForm.tsx`) sends `data_api_revoke_default_privileges` (`dataApi && !dataApiDefaultPrivileges`, the box "Automatically expose new tables" unchecked), `data_api_exposed_schemas` (`[]` when "Enable Data API" is off, omitted otherwise) and `data_api_use_api_schema` (always `false`); the node reads all three (docs/guide.md, "New tables need a grant"). An omitted `data_api_revoke_default_privileges` means revoke, as for `POST /v1/projects`, whose body has no such field |
| P0 | GET | `/platform/projects/{ref}` | 200 `ProjectDetailResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}` | 200 `ProjectRefResponse_Output` (requires `id`, `name`, `ref`) |
| P1 | GET | `/platform/telemetry/feature-flags` | 200 `{"dataApiRevokeOnCreateDefault": true, "proBenefitWording": false, "pricingPanelPlanPresentation": false}`. Studio reads the first PostHog flag through `usePHFlag` to start the box "Automatically expose new tables" unchecked (hosted has had every user in the treatment arm since 2026-05-30); an empty object leaves the box checked, so a project created from the form would keep the grants. Once the answer holds any flag, Studio logs `Flag key "x" does not exist in PostHog flag store` for each key it reads and the answer lacks, so the other two keys Studio reads (`usePHFlag` in the paused-project page and the plan panel) are listed as false, the control arm of their experiments (`internal/api/telemetry.go`) |
| P1 | POST | `/platform/projects/{ref}/api-keys/temporary` | 201 `TemporaryApiKeyResponse_Output` |
| P1 | HEAD | `/platform/projects/{ref}/api/rest` | any 2xx means PostgREST is up; not in the spec (the spec has GET only). `200` while the project's PostgREST answers, `503` when it does not (`internal/api/rest_graphql.go`). |
| P1 | GET | `/platform/projects/{ref}/config/pgbouncer` | 200 `PgbouncerConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/pgbouncer` | 200 `UpdatePoolingConfigResponse_Output` (requires `pgbouncer_enabled`, `pgbouncer_status`) |
| P1 | GET | `/platform/projects/{ref}/config/postgrest` | 200 `GetPostgrestConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/postgrest` | 200 `UpdatePostgrestConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/secrets` | 200 `UpdateSecretsResponse_Output`; Studio sends `{jwt_secret, change_tracking_id}` from the Legacy JWT secret page. See "Legacy JWT secret" |
| P1 | GET | `/platform/projects/{ref}/config/secrets/update-status` | 200 `GetJwtSecretUpdateStatus_Output`; `update_status` is null or `{change_tracking_id, status, progress, error?}` |
| P1 | GET | `/platform/projects/{ref}/config/realtime` | 200 `RealtimeConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/realtime` | 204 no body |
| P1 | GET | `/platform/projects/{ref}/config/storage` | 200 `StorageConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/storage` | 200 `StorageConfigResponse_Output` |
| P1 | GET | `/platform/projects/{ref}/content` | 200 `GetUserContentResponse_Output` |
| P1 | PUT | `/platform/projects/{ref}/content` | 200 no body |
| P1 | DELETE | `/platform/projects/{ref}/content` | 200 array of `BulkDeleteUserContentResponse_Output` |
| P1 | GET | `/platform/projects/{ref}/content/count` | 200 `GetContentCountV2Response_Output` (requires `favorites`, `private`, `shared`) |
| P1 | GET | `/platform/projects/{ref}/content/folders` | 200 `GetUserContentFolderResponse_Output` |
| P1 | POST | `/platform/projects/{ref}/content/folders` | 201 `CreateUserContentFolderResponse_Output` |
| P1 | DELETE | `/platform/projects/{ref}/content/folders` | 200 no body |
| P1 | GET | `/platform/projects/{ref}/content/folders/{id}` | 200 `GetUserContentFolderResponse_Output` |
| P1 | GET | `/platform/projects/{ref}/content/item/{id}` | 200 `GetUserContentByIdResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/db-password` | 200 `UpdatePasswordResponse_Output` |
| P1 | POST | `/platform/projects/{ref}/pause` | 201 no body |
| P1 | POST | `/platform/projects/{ref}/restart` | 201 no body |
| P1 | POST | `/platform/projects/{ref}/restart-services` | 201 no body |
| P1 | POST | `/platform/projects/{ref}/restore` | 200 `UnpauseProjectResponse_Output` |
| P0 | GET | `/platform/projects/{ref}/settings` | 200 `ProjectSettingsResponse_Output` |
| P0 | GET | `/platform/projects/{ref}/status` | 200 `{"status": "<project status>"}` (the spec documents no body; Studio reads `data.status`) |
| P1 | GET | `/platform/projects/{ref}/pause/status` | 200 `PauseStatusResponse_Output`: `can_restore: true`, `max_days_till_restore_disabled: 365`, `remaining_days_till_restore_disabled: null` (Studio reads null as no expiry), `latest_downloadable_backup_id: null`, `last_paused_on` (the date the pause began, null while the project runs); see "Paused projects and branches" |
| P1 | POST | `/platform/projects/{ref}/wake` | 201 `ProjectWakeResponse_Output` (requires `connection_string`, `connection_string_read_only`) |
| P1 | GET | `/platform/storage/{ref}/buckets` | 200 array of `StorageBucketResponse_Output` |
| P1 | POST | `/platform/storage/{ref}/buckets` | 201 no body |
| P1 | GET | `/platform/storage/{ref}/buckets/{id}` | 200 `StorageBucketResponse_Output` |
| P1 | PATCH | `/platform/storage/{ref}/buckets/{id}` | 200 no body |
| P1 | DELETE | `/platform/storage/{ref}/buckets/{id}` | 200 no body |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/empty` | 201 no body |
| P1 | DELETE | `/platform/storage/{ref}/buckets/{id}/objects` | 200 no body |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/list` | 201 array of `StorageObject_Output` |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/list-v2` | 201 `StorageListResponseV2_Output` (requires `folders`, `hasNext`, `objects`) |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/move` | 201 no body |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/public-url` | 201 `PublicUrlResponse_Output` |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/sign` | 201 `SignedUrlResponse_Output` |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/sign-multi` | 201 array of `SignedUrlsResponse_Output` (requires `error`, `path`, `signedUrl`) |
| P1 | GET | `/platform/storage/{ref}/credentials` | 200 `GetStorageCredentialsResponse_Output` |
| P1 | POST | `/platform/storage/{ref}/credentials` | 201 `CreateStorageCredentialResponse_Output` |
| P1 | DELETE | `/platform/storage/{ref}/credentials/{id}` | 204 no body |
| P1 | GET | `/v1/projects/{ref}/api-keys` | 200 array of `ApiKeyResponse_Output` |
| P1 | POST | `/v1/projects/{ref}/api-keys` | 201 `ApiKeyResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/api-keys/legacy` | 200 `LegacyApiKeysResponse_Output` |
| P1 | PUT | `/v1/projects/{ref}/api-keys/legacy` | 200 `LegacyApiKeysResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/api-keys/{id}` | 200 `ApiKeyResponse_Output` |
| P1 | DELETE | `/v1/projects/{ref}/api-keys/{id}` | 200 `ApiKeyResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/config/auth/signing-keys` | 200 `SigningKeysResponse_Output` |
| P1 | POST | `/v1/projects/{ref}/config/auth/signing-keys` | 501: signing keys are not supported |
| P1 | GET | `/v1/projects/{ref}/config/auth/signing-keys/legacy` | 404: the legacy secret is not migrated to a signing key (Studio then offers "Change legacy JWT secret") |
| P1 | POST | `/v1/projects/{ref}/config/auth/signing-keys/legacy` | 501: signing keys are not supported |
| P1 | PATCH | `/v1/projects/{ref}/config/auth/signing-keys/{id}` | 501: signing keys are not supported |
| P1 | DELETE | `/v1/projects/{ref}/config/auth/signing-keys/{id}` | 501: signing keys are not supported |
| P1 | GET | `/v1/projects/{ref}/config/auth/third-party-auth` | 200 array of `ThirdPartyAuth_Output` |
| P1 | POST | `/v1/projects/{ref}/config/auth/third-party-auth` | 201 `ThirdPartyAuth_Output` |
| P1 | DELETE | `/v1/projects/{ref}/config/auth/third-party-auth/{tpa_id}` | 200 `ThirdPartyAuth_Output` |
| P1 | GET | `/v1/projects/{ref}/config/database/postgres` | 200 `PostgresConfigResponse_Output` |
| P1 | PUT | `/v1/projects/{ref}/config/database/postgres` | 200 `PostgresConfigResponse_Output` |
| P1 | PUT | `/v1/projects/{ref}/database/migrations` | 200 no body. Not a stub: it records the migration in `supabase_migrations.schema_migrations` **without running it**, once per `Idempotency-Key` and once per name plus statements (whitespace and a final `;` aside). Studio's merge page calls it on the branch with the diff it just read (`branch_merge_<time>`) and then merges; when that migration is the newest of the branch, the merge runs it on the parent, in one transaction with the statements of the branch's other migrations that a schema diff does not hold (data, cron jobs, publications; their schema changes are in the diff), and ignores it when the branch changed after it (`internal/api/database.go`, `internal/branching/merge.go`) |
| P1 | GET | `/v1/branches/{branch_id_or_ref}/diff` | 200 `text/plain`: the SQL of what the branch changed in its schema since it was made or last updated, **empty** when nothing changed (the merge page then says "No changes detected" and does not call `PUT .../database/migrations`). `included_schemas` and `pgdelta` are accepted; both `pgdelta` values give one answer |
| P1 | GET | `/v1/projects/{ref}/functions` | 200 array of `FunctionResponse_Output`; each carries `ezbr_sha256`, the content digest Studio's merge page compares (a function with none counts as modified), and `updated_at` in epoch milliseconds |
| P1 | POST | `/v1/projects/{ref}/functions/deploy` | 201 `DeployFunctionResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/functions/{function_slug}` | 200 `FunctionSlugResponse_Output` |
| P1 | PATCH | `/v1/projects/{ref}/functions/{function_slug}` | 200 `FunctionSlugResponse_Output` |
| P1 | DELETE | `/v1/projects/{ref}/functions/{function_slug}` | 200 no body |
| P1 | GET | `/platform/projects/{ref}/notifications/advisor/exceptions` | 200 `ListNotificationExceptionsResponse_Output`; the Advisor Rules page, a feature preview (see Advisors) |
| P1 | POST | `/platform/projects/{ref}/notifications/advisor/exceptions` | 201 `CreateNotificationExceptionsResponse_Output`; Studio then reads `run-lints` again, so the rule must already hide its lints |
| P1 | DELETE | `/platform/projects/{ref}/notifications/advisor/exceptions` | 200 no body; the rule ids are the comma-separated `ids` query |
| P1 | PATCH | `/platform/projects/{ref}/notifications/advisor/exceptions/{id}` | 200 no body |
| P1 | GET | `/platform/projects/{ref}/run-lints` | 200 array of `GetProjectLintsResponse_Output` items: every lint, both categories (see Advisors) |
| P1 | GET | `/v1/projects/{ref}/functions/{function_slug}/body` | 200 `StreamableFile` |
| P1 | GET | `/v1/projects/{ref}/advisors/performance` | 200 `V1ProjectAdvisorsResponse_Output`; the CLI and the MCP server, not Studio |
| P1 | GET | `/v1/projects/{ref}/advisors/security` | 200 `V1ProjectAdvisorsResponse_Output`; the same |
| P1 | GET | `/v1/projects/{ref}/secrets` | 200 array of `SecretResponse_Output` (requires `name`, `value`) |
| P1 | POST | `/v1/projects/{ref}/secrets` | 201 no body |
| P1 | DELETE | `/v1/projects/{ref}/secrets` | 200 no body |
| P1 | GET | `/v1/projects/{ref}/types/typescript` | 200 `TypescriptResponse_Output` |
| P1 | POST | `/v2/projects/{ref}/advisors/run` | 200 `V2ProjectAdvisorsResponse_Output`; Studio sends it for the health advisor only when its `healthAdvisor` flag is on, and the MCP server for `get_advisors` with type `health` |

## Branch pages and action runs

The Branches page, the merge page (`/project/{ref}/merge`) and "View Logs" read the action runs of branch operations (`internal/branching/README.md`, "Action runs"). Studio derives a run's status from its steps (`data/actions/action-detail-query.ts`): no steps, or a step that is `CREATED` or `RUNNING`, is running; every step `EXITED` or `PAUSED` is a success; a `DEAD` step is a failure. A run without steps therefore reads as running for ever, so every run is stored with its seven steps before the call that starts it returns.

| Prio | Method | Path | What Studio needs |
|---|---|---|---|
| P1 | GET | `/v1/projects/{ref}/actions` | the runs listed under `{ref}`, newest first (`View Logs` on a branch lists them with `{ref}` = the branch's `project_ref`); a run whose `id` equals the project ref renders specially in `WorkflowLogs.tsx`, which Supavise never produces |
| P1 | GET | `/v1/projects/{ref}/actions/{run_id}` | polled every 2 s by `useWorkflowManagement` until it answers 404, for **both** the branch ref and the parent ref with the same `workflow_run_id`. The owner answers 200 (a merge: the parent; create, push and reset: the branch) and the other ref 404, which stops its polling |
| P1 | GET | `/v1/projects/{ref}/actions/{run_id}/logs` | `text/plain`; Studio splits it on `\n` and `\r` and trims it |
| P2 | PATCH | `/v1/branches/{id}` with `request_review` | `true` puts the branch on the merge-request list, `false` removes it (Studio toasts "Merge request closed"); after a merge Studio sends `false` itself when the run finishes under the parent ref, and the node clears the mark as well, so the CLI and MCP merges agree |
| P1 | POST | `/v1/branches/{id}/merge`, `/push` | `201 {workflow_run_id}`; the page adds it to the query string, shows "Processing" while the run is running and offers "Close branch" when it succeeded. A refusal found while the run works (divergence) is a failed run, not an error status |
| P1 | POST, PATCH | `/v1/projects/{ref}/branches`, `/v1/branches/{id}` of the default branch | the Production branch form (GitHub integration settings) creates the default branch with `is_default: true` and later patches `git_branch` and `branch_name`; both are accepted and the answer is the default branch with the Git branch stored |

## Paused projects and branches

A project, or a branch, with status `INACTIVE` renders `ProjectPausedState` instead of its pages. The page reads `GET /platform/projects/{ref}/pause/status` (only while the project is `INACTIVE`): with the generic stub the object came back with `can_restore: false`, and Studio then said the project "can no longer be restored" with no Resume button, so the handler (`internal/api/pause_status.go`) answers:

| Field | Value | Why |
|---|---|---|
| `can_restore` | `true` | the page offers **Resume** (`POST /platform/projects/{ref}/restore`) whenever it is true; any project the engine accepts a resume for can be restored |
| `max_days_till_restore_disabled` | `365` | hosted's window (one year); not used by the page when the next field is null |
| `remaining_days_till_restore_disabled` | `null` | `ProjectPausedState.tsx` shows "You can resume this project from the dashboard" with no date for null; a node keeps a paused project's data until its owner deletes it |
| `latest_downloadable_backup_id` | `null` | there is no archive download; the "Export your data" menu then says "No backups available for download", and the storage archive call is refused in `backups.go` |
| `last_paused_on` | RFC 3339, the time the pause began (`supavise.project_pause.paused_at`; the last change of the project for one paused before that table existed) | |

A branch is paused by its owner (Pause in the dashboard, `supabase branches pause`), by a scheduled deletion (`DELETE /v1/branches/{id}?force=false`: Studio enables **Restore** on the Branches page only when `preview_project_status` is `INACTIVE`), or by the node for idleness (`internal/branching/README.md`, "Idle pause"). Resume works for all three: `POST /platform/projects/{ref}/restore` resumes the project and, for a branch whose deletion is scheduled, cancels the deletion too. Only an idle pause also wakes on a request.

Two related answers for a **branch ref**: `GET /v1/projects/{ref}` and `GET /v1/projects/{ref}/branches` answer `404`, as hosted does (the CLI reads the 404 and keeps the parent as the linked project); Studio reads a branch through `GET /platform/projects/{ref}` and lists its family from the parent. `DELETE /v1/projects/{ref}` and `DELETE /platform/projects/{ref}` on a branch go through the branch service, like `DELETE /v1/branches/{id}`.

The Settings > General > Custom domains section renders for a project and for a branch (`project_settings:custom_domains` is not in the profile's `disabled_features`) unless `tls.mode` is `dns01`; its calls are `/v1/projects/{ref}/custom-hostname` and `.../initialize`, `.../reverify`, `.../activate`, and Studio's own `/api/check-cname`, answered by the proxy. Vanity subdomains have no Studio page.

## GitHub integration

The GitHub card of Settings > Integrations (project and organization), the Connections row of Account > Preferences, the GitHub status of the Branches side menu and the "Sync with a GitHub branch" block of the branch dialogs are shown only while the profile does not list `integrations:github` in `disabled_features`. The profile lists it while the server has no GitHub App (patch 0007, `docs/github-integration.md`). The install and authorize windows are built from `NEXT_PUBLIC_GITHUB_INTEGRATION_APP_NAME` and `..._CLIENT_ID`, which the unit sets from the server's App.

| Prio | Method | Path | What Studio needs |
|---|---|---|---|
| P1 | GET | `/platform/integrations/github/authorization` | `{id, sender_id, user_id}` when the signed-in user authorized the server's App, an error (Studio reads it as "not connected") when not. A stub answered with zeros for everybody, which showed "Connected" and "You are authorized with the Supabase GitHub app". `404` here. |
| P1 | POST | `/platform/integrations/github/authorization` | `201`. Body `{code}`: the authorize page (`/integrations/github/authorize`) compares the `state` with the one in the browser and posts only the code. |
| P2 | DELETE | `/platform/integrations/github/authorization` | `200`; `404` when the user has none. Account > Preferences > Remove connection. |
| P1 | GET | `/platform/integrations/github/connections?organization_id=` | `{connections: [...]}` of the organization, each with `project`, `repository` and `user`. The Branches page shows only an alert when this call fails, so a server without an App answers an empty list. Repository and installation ids are exact integers (they do not fit a `float32`). |
| P1 | POST, PATCH, DELETE | `/platform/integrations/github/connections[/{id}]` | The connection form. `POST` body `{project_ref, installation_id, repository_id, workdir, supabase_changes_only, branch_limit, new_branch_per_pr}`, answer `201`; `PATCH` and `DELETE` answer `204`. The production branch of the same form is a create or patch of the default branch (`is_default`, `git_branch`), not a call of this family. |
| P1 | GET | `/platform/integrations/github/repositories` | `{partial_response_due_to_sso, repositories: [{id, name, installation_id, default_branch}]}`: the repositories the user's own GitHub token reaches through the server's App and may push to (a read-only repository is left out). |
| P2 | GET | `/platform/integrations/github/repositories/{id}/branches[/{name}]` | The branch check of the production branch field and the branch dialogs: `{name}`, `404` when the branch does not exist. The repository is checked against the user's last listing, kept for 30 seconds. |
| P2 | GET | `/platform/integrations/github/connections/{id}/config?ref=` | The parsed `supabase/config.toml` for the configuration drift page: `{path, ref, sha, config}`; `404` without the file, `422` for a file that is not TOML. |
| P2 | GET | `/v2/organizations/{slug}/integrations/github/connections` | The same list in the JSON:API shape. |

Branches carry `pr_number` while they follow a pull request, which turns "Create pull request" off for them; the action needs a connection (patch 0005). With Deploy to production on, `mainBranch.git_branch` is set and a connection exists, so Studio disables the merge page's Merge button; `POST /v1/branches/{id}/push` on the production row (Resync) deploys the production branch.

## Advisors

The Security Advisor, the Performance Advisor, the advisor panel and the project home widget, the table editor's RLS badge and Studio's "project needs securing" banner all read `GET /platform/projects/{ref}/run-lints` (`data/lint/lint-query.ts`). Studio filters the array itself: a page keeps the lints whose `categories` include `SECURITY` or `PERFORMANCE`, groups them by `level`, and picks the selected one by `cache_key`, so the answer carries every lint of both categories. Studio runs the same lints in its own self-hosted mode (`lib/api/self-hosted/lints.ts`, `getLintsSQL` of `packages/pg-meta`, as `supabase_admin`, with `PGRST_DB_SCHEMAS` as `pgrst.db_schemas`), which is where Supavise's SQL comes from (`internal/advisors/README.md`).

- **Names outside the spec.** The lint `name` enum of `GetProjectLintsResponse_Output` is shorter than what hosted returns (its own recorded `auth_db_connections_absolute`) and than the names Studio's `lintInfoMap` knows (`sensitive_columns_exposed`, `rls_policy_always_true`, `public_bucket_allows_listing`, the GraphQL exposure lints and the two security-definer-function lints). The CLI reads `name` as a plain string and Studio does not check it, so Supavise returns the names it has. Four lints of the vendored SQL are in neither the enum nor `lintInfoMap`: `table_bloat`, `fkey_to_auth_unique`, `extension_versions_outdated` and `insecure_queue_exposed_in_api`. Studio lists their rows without a type title, and its detail panel says "Unknown", as it does for its own self-hosted runner at this commit. Supavise keeps them because they are findings, one of them an error.
- **`metadata`.** Studio reads `metadata.schema` and `metadata.name` to link a lint to its object, and `metadata.bucket_id` for `public_bucket_allows_listing`; the rows keep their whole `metadata`.
- **Lints from the Auth settings.** Hosted adds a few findings computed from the project's configuration to the same answer. The CLI's recorded response of a hosted project (v2.120.0) holds `auth_db_connections_absolute` (INFO, `PERFORMANCE`, `metadata` `{"type": "auth", "entity": "Auth"}`) and no security finding, although `password_hibp_enabled` and every MFA method are off. Studio's lint catalog at the pinned commit (`Linter.utils.tsx`, `94b8b06`) has a display entry for `auth_leaked_password_protection` and for `auth_insufficient_mfa_options`, which means hosted's API returns them, and guides for hosted projects widely report "Leaked Password Protection Disabled"; hosted may gate them on something the recording does not show (an entitlement of the organization, for example), and that rule is unknown. Supavise reports `auth_leaked_password_protection`, `auth_insufficient_mfa_options`, `auth_otp_long_expiry` and `auth_db_connections_absolute` whenever the setting they read is off, because its organizations hold every entitlement. It does not report the compliance lints of the spec's name list (`ssl_not_enforced`, `log_connections_not_enabled`, `network_restrictions_not_set`, `pitr_not_enabled` and the others; `internal/advisors/config.go` gives the reason for each). A new Supavise project starts at `mailer_otp_exp` 3600 and `db_max_pool_size` 5, where a new hosted project starts at 3600 and 10; a project created before that keeps `mailer_otp_exp` 86400 and answers `auth_otp_long_expiry`.
- **Health lints.** `POST /v2/projects/{ref}/advisors/run` asks for lints by name (1 to 10). `useProjectHealthLintsQuery` drops `project_not_active` and `advisor_check_unavailable` from what it shows, and so does the MCP server, which is how Supavise answers the checks it cannot run.
- **Advisor Rules.** The "Disable Advisor rules" feature preview lists, creates, edits and deletes `notifications/advisor/exceptions`. Studio sends `{"lint_name": ..., "is_disabled": true}` with no member for a rule of the whole project, and reads `run-lints` again afterwards. A GET answers `{"exceptions": []}` when nothing is set; every field of a rule except `id`, `project_ref`, `is_disabled`, `last_updated_by` and `inserted_at` is null when unset.

## Legacy JWT secret

Project settings, JWT keys, Legacy JWT secret is Studio's page for the secret that signs the `anon` and `service_role` keys and the sessions GoTrue issues. It reads `GET /platform/projects/{ref}/config/postgrest` for the secret and `GET /v1/projects/{ref}/config/auth/signing-keys/legacy` to decide what to offer, and it changes the secret with `PATCH /platform/projects/{ref}/config/secrets`.

| Call | What Studio does with it | What Supavise answers |
|---|---|---|
| `GET /v1/projects/{ref}/config/auth/signing-keys/legacy` | shows "Change legacy JWT secret" only when this call fails, and "Legacy JWT secret has been migrated to new JWT Signing Keys" when it succeeds | `404` for every project, as on a hosted project that has not migrated its secret. The spec-derived stub answered `200` with a made-up key, which hid the button |
| `PATCH /platform/projects/{ref}/config/secrets` `{jwt_secret, change_tracking_id}` | "Generate a random secret" sends `ROLL`; "Create my own secret" sends the text, which the form limits to 32 characters or more without `@` or `$` | `200 {message}` once the change is done: the secret is applied (a random 40-character one for `ROLL`), the `anon` and `service_role` keys are signed again, GoTrue and PostgREST restart, the Realtime and Storage tenants and the Edge Functions follow. `400` for a secret under 32 characters, a first character of `@`, `{` or `[`, or control characters (`$` and `@` inside are accepted). `409` while the project is in a state that cannot restart, `500` with the previous secret restored when a step fails. Owners and Administrators only (`infra:Execute` on `queue_job.projects.update_jwt`, the check behind the button) |
| `GET /platform/projects/{ref}/config/secrets/update-status` | polls every second while `status` is `0` (updating) and toasts the result when it turns to `1` or `2` | `{update_status: null}` until the first change since the API server started, then `{change_tracking_id, status, progress}`: `status` `1` and `progress` `5` when done, `2` with `error` `1`, `2` or `5` when the store, the restart or the tenant step failed. The change runs inside the `PATCH`, so the status is final by the time Studio asks, except for a request that arrives while the work is running |
| `POST` and `PATCH`, `DELETE` under `/v1/projects/{ref}/config/auth/signing-keys` | "Migrate JWT secret", "Create a standby key", rotate, revoke | `501` with a message that points to the legacy secret. GoTrue here signs with the legacy secret only (HS256), so a signing key would be a record that nothing signs with |

Not reproduced: hosted's 20-minute wait between two changes, and its "up to 2 minutes" of errors while the project restarts (here the restart takes seconds and requests that reach a unit during it fail).

## Read replicas

The Infrastructure page, the SQL editor's source selector and the reports read these calls. Supavise answers them from the registry and the replica controller (`internal/api`, `replicas.go`, `databases.go`, `load_balancers.go`, `infra_monitoring.go`). The answers use the shapes of the three specs, with no new fields. A node that has no replica controller lists the primary alone and answers `503` ("Read replicas are not set up on this Supavise server") to setup, remove and the restart of a replica.

| Call | What Studio does with it | What Supavise answers |
|---|---|---|
| `GET /platform/profile` | hides the Infrastructure page, the source selector and the replica rows of the reports when `disabled_features` holds `infrastructure:read_replicas` | The key is in `disabled_features` until a replica controller is wired and two nodes are active. It is a feature flag, not a permission check, and costs one registry read of the nodes per profile request once a controller is wired. |
| `GET /platform/projects/{ref}` | decides whether the Add button is enabled | `dbVersion` is `supabase-postgres-<version>` (a bare version counts as below Postgres 15); `is_physical_backups_enabled` is true when the node has a backup service; `cloud_provider` is `AWS`; `high_availability` is false |
| `GET /platform/organizations/{slug}/projects` | the project cards | `databases[]` has one element per replica, `type: 'READ_REPLICA'`, read for the whole page in one query |
| `GET /platform/projects/{ref}/databases` | the Infrastructure page and the source selector | The primary first, then each replica oldest first. A replica row has the primary's shape: its identifier, the region of its node, `restUrl` `https://<identifier>.api.<domain>/rest/v1/`, `db_host` `db.<identifier>.api.<domain>`, and `connectionString` and `connection_string_read_only` with a `[YOUR-PASSWORD]` placeholder. `size` is the project's size. |
| `GET /platform/projects/{ref}/databases-statuses` | polls until a replica is up | The same databases, the primary included (Studio refetches until the two lists have the same length). A replica has `replicaInitializationStatus`: `in_progress` with the step reached and the estimates, `completed`, or `failed` with the step's error. |
| `GET /platform/projects/{ref}/load-balancers` | the "API Load Balancer" node | `[]` without a replica, otherwise one balancer: `https://<ref>-lb.api.<domain>` and every database with its type and status |
| `POST /v1/projects/{ref}/read-replicas/setup` `{read_replica_region}` | the Add button | `204` once the controller records the replica; it comes up in the background. Owners and Administrators only. A refusal is `400` with a message that Studio shows as it is. |
| `POST /v1/projects/{ref}/read-replicas/remove` `{database_identifier}` | remove a replica | `204`; `404` "Read replica not found" for an identifier that is not one of the project's replicas, the primary's included |
| `POST /platform/projects/{ref}/restart` | restart a database | With a `database_identifier` that is a replica of the project, restarts that replica alone (`201`, once the replica is `RESTARTING`; the units restart in the background and databases-statuses shows `RESTARTING`, then `ACTIVE_HEALTHY` or `ACTIVE_UNHEALTHY`); `404` for another identifier. Without one, or with the project's own, it restarts the project, which is the primary only. A body that is not JSON is `400` on a node with a replica controller. A node without one ignores a body it cannot parse, and answers `503` to a replica identifier. |
| `GET /platform/projects/{ref}/config/supavisor`, `GET /v1/projects/{ref}/config/database/pooler` | the connect strings of each database | One entry per database. A replica entry has `database_type: 'READ_REPLICA'`, `db_user: 'postgres.<identifier>'`, `db_host` the node's public host, the transaction port and the project's pool settings. |
| `GET /platform/projects/{ref}/infra-monitoring` | the replication lag chart, and every chart of the Database report | With `databaseIdentifier` of a replica and `physical_replication_lag_physical_replication_lag_seconds`: the mean of the controller's one-minute samples per `interval`, as strings. Every other attribute of a replica is zero with an `errors` entry. Without a replica, or with the project's own identifier, the project's utilization (see "Utilization and resource warnings"). |
| `POST /platform/pg-meta/{ref}/query` and the other pg-meta routes | run SQL against the database that is selected | The header `x-connection-encrypted` selects the database and is never a credential (see below). |

**The header selects a database.** Studio sends back the `connectionString` (or `connection_string_read_only`) of the database it is working on. The host of that string, `db.<identifier>.api.<domain>`, names the project or one of its replicas. The handler authorizes by `{ref}`: an identifier that is neither the project nor one of its replicas answers `403`, a replica that is not `ACTIVE_*` answers `503`, and a header that is not a database host of this node, or is absent, selects the primary. The connection is built from the project's own credentials at the replica's port on this node. The user named in the string is not read. A caller keeps the role it has on the primary, so Studio's reports read `pg_stat_statements` as `postgres` and no role has to reach the replica with the WAL first. A caller who may only query runs as `supavise_read_only` on both. `supabase_read_only_user`, hosted's name, is created by the pinned Postgres artifact; the API only names it.

**The Add button.** Studio enables it when the project has fewer replicas than its size allows (none up to Micro, four from Small to Large, five above), reports Postgres 15 or later and the cloud provider AWS, has `is_physical_backups_enabled`, is not high availability and the organization has the `instances.read_replicas` entitlement, which Supavise grants. `internal/api/studio_eligibility_test.go` ports these rules at the pinned Studio version and evaluates them against the handlers. Studio also tells a user who opens Database, Backups, Point in time on a project with a replica to remove the replicas first.

Not a Studio call: `GET /supavise/v1/failover/readiness` (Owners and Administrators) returns what the failover block of `supavise status` shows. A server that is not part of a cluster answers `404`.

## OAuth sign-in for MCP clients

The consent page (`pages/authorize.tsx`), the organization's OAuth Apps page (`pages/org/[slug]/apps.tsx`) and the Connect > MCP panel work with the pinned Studio unchanged. Supavise answers their calls from `internal/api` (`oauth_consent.go`, `oauth_apps_api.go`) over `internal/oauth`, with the shapes of the platform spec. [design.md](../design.md#13-oauth-sign-in-and-the-remote-mcp-endpoint) section 13 has the rules.

| Call | What Studio does with it | What Supavise answers |
|---|---|---|
| (not Studio) `GET /v1/oauth/authorize` | The MCP client opens it. | `302` to `studio.<domain>/authorize?auth_id=<uuid>[&organization_slug=<slug>]`, where Studio's `withAuth` signs the user in through `api.<domain>/auth/v1` and returns to the page. Whether that return works through SSO and MFA sign-in is unverified. |
| `GET /platform/oauth/authorizations/{id}` | The page shows the client's name, the redirect host, the scopes and an organization picker. | `200 {name, website, icon?, domain, redirect_uri, expires_at, approved_at?, approved_organization_slug?, scopes, registration_type}`. `domain` is the host of the requested redirect URI, and `icon` is left out for a dynamic app. An unknown id is `404`. An expired request still answers `200`, because Studio computes "expired" from `expires_at`. Owners and Administrators only, so a Developer gets `403` (how Studio shows that is unverified). |
| `POST /platform/organizations/{slug}/oauth/authorizations/{id}?skip_browser_redirect=true` | Approve. Studio sets `window.location.href` to the returned `url`. | `201 {url}` with `<redirect_uri>?code=…&state=…&iss=…`, and `Cache-Control: no-store`. `409` for a request decided already, `410` for an expired one, `403` when `{slug}` differs from the request's `organization_slug` or the caller is not an Owner or Administrator of it. A session below aal2 is refused when the caller belongs to an organization that enforces MFA. |
| `DELETE /platform/organizations/{slug}/oauth/authorizations/{id}` | Decline. Studio shows a toast and goes to `/organizations`. | `200 {id}`. The client is not told and waits for its own timeout; hosted behaves the same way. |
| `GET /platform/organizations/{slug}/oauth/apps?type=authorized` | The Authorized tab. | One item per app with a live grant in the organization: `id`, `app_id` and `client_id` are the app's UUID; `authorized_at` is the newest live grant's creation time; `scopes` are the union of the grants' effective scopes. A dynamic app has no `icon` and an empty `website`. |
| `GET …/oauth/apps?type=published` | The Published tab. | The organization's manual apps. |
| `POST …/oauth/apps`, `PUT` and `DELETE …/oauth/apps/{id}` | Publish, edit and delete an app. | `201 {id, client_id, client_secret, client_secret_expires_at: 0, redirect_uris}` (the plaintext secret appears here only); `200`; `200`, which also revokes the app's grants. |
| `POST …/oauth/apps/{id}/revoke` | The Revoke button. | `201 {id, name, website, icon?, authorized_at?}`; revokes the app's live grants in this organization for every user. |
| `GET`, `POST` `…/oauth/apps/{app_id}/client-secrets`, `DELETE …/client-secrets/{secret_id}` | A manual app's secrets. | Secret ids are UUIDs, the list shows `sba_xxxx********` aliases only, and the plaintext appears once, on create. A dynamic app's id answers `404`. |

The Owner and Administrator rule of the existing `oauth_apps` permission covers the organization routes, so no permission list changes.

**The Connect > MCP panel.** It reads `NEXT_PUBLIC_MCP_URL` (patch 0004 adds the placeholder; `studio/placeholders.json`), which the fleet sets to `<api url>/mcp`, and shows `<url>?project_ref=<ref>` with the Claude Code, Cursor, VS Code and Codex snippets. The panel's own text, "OAuth 2.1 with dynamic client registration", describes what the node does.

## Utilization and resource warnings

Studio reads `GET /platform/projects/{ref}/infra-monitoring` for the Database report (Observability > Database), the overview cards of Observability, the Compute and Disk page, the replica details and the Realtime report, and `GET /platform/projects-resource-warnings` for the banners above a project. The collector in `internal/metrics` (see its README) answers both from the node's control groups, one SQL session of each project and its disk quota.

**The request.** `attributes` is an array (repeated, bracketed or comma-separated; the older `attribute` works too), `startDate` and `endDate` are required RFC 3339 timestamps, and `interval` is `1m`, `5m`, `10m`, `30m`, `1h` (the default) or `1d`. A name that is not one of the 57 of the spec's enum is refused with `400`, as hosted refuses it. `databaseIdentifier` names a replica. Studio picks the interval from the range (`getIntervalGranularity`): `1m` up to an hour, `2m` up to 12 hours (it asks for `1m` and averages pairs itself), `1h` up to 7 days and `1d` beyond. The date range presets go to 28 days, and the plan Supavise reports (`enterprise`) allows custom ranges of 90 days; the store keeps minutes for 3 days and hours for 92.

**The response.** One attribute is answered in the single shape, `{yAxisLimit, format, total, totalAverage, data: [{period_start, <attribute>: "<value>"}]}`, several in the multi shape, `{data: [{period_start, values: {<attribute>: "<value>"}}], series: {<attribute>: {yAxisLimit, format, total, totalAverage}}, errors?: {<attribute>: {message}}}`. Studio tells them apart by the `series` key. Values are strings. **A name with no entry in `series` blanks its chart**: `mapResponseToAnalyticsData` skips it, and the chart handler shows its error state while any attribute of the chart has no data. The response therefore has a `series` entry for every name asked for, a value of zero on each point of an attribute Supavise cannot measure, and an `errors` entry for it. Studio shows an `errors` entry in place of the value on the Observability cards and counts the attribute as not available for the compute summary. A point is a bucket that holds at least one sample; there are none for a period when the project was down. `format` is `%` for percentages (the custom report chart prints those with a percent sign), `bytes`, `s`, `ms`, or empty for counts.

| Page | Attributes it asks for |
|---|---|
| Database report, memory | `ram_usage_used`, `ram_usage_cache_and_buffers`, `ram_usage_free`, `ram_usage_total`, `ram_usage_swap`; with a flag `ram_commit_used`, `ram_commit_limit`; hidden `swap_usage` |
| Database report, CPU | `cpu_usage_busy_system`, `_user`, `_iowait`, `_irqs`, `_other`, `_idle` |
| Database report, network, disk | `network_receive_bytes`, `network_transmit_bytes`, `disk_iops_read`, `disk_iops_write`, `disk_bytes_read`, `disk_bytes_written`, `disk_io_budget` (burstable disks only) |
| Database report, connections | `pg_stat_database_num_backends`, `client_connections_postgres`, `_authenticator`, `_supabase_admin`, `_supabase_auth_admin`, `_supabase_storage_admin`, `_other`, `client_connections_pgbouncer`, `supavisor_connections_active`. `max_db_connections` is a reference line Studio fills from `SHOW max_connections`. |
| Database report, disk size | `disk_fs_used_system`, `disk_fs_used_wal`, `pg_database_size`, `disk_fs_size` |
| Observability overview, `useComputeMetrics` | `avg_cpu_usage`, `ram_usage` and `disk_io_consumption` (percent), `pg_stat_database_num_backends`, and the four disk attributes above. The card shows the `totalAverage` of each series, and for connections the highest point. |
| Compute and Disk | `max_cpu_usage`, `ram_usage`, `disk_io_consumption`, and the four disk attributes, daily over 7 days |
| Replica details | `physical_replication_lag_physical_replication_lag_seconds` with `databaseIdentifier` |
| Realtime report | one at a time: `realtime_sum_connections_connected`, `realtime_channel_events`, `realtime_channel_presence_events`, `realtime_channel_db_events`, `realtime_channel_joins`, `realtime_payload_size`, `realtime_replication_connection_lag`, `realtime_read_authorization_rls_execution_time`, `realtime_write_authorization_rls_execution_time` |

The Compute and Disk page reads the disk percentage as `(disk_fs_used_system + disk_fs_used_wal + pg_database_size) / disk_fs_size`, so the three parts add up to the project's directory and the size is its quota. Which attributes have a source, and what each one reads, is in the README of `internal/metrics`. The Realtime attributes, the network, the I/O budget and consumption, memory commitment and PgBouncer have none here.

**Resource warnings.** `GET /platform/projects-resource-warnings?ref=<ref>` or `?slug=<org>` (neither: every project the caller can see) returns one object per project with the nine fields of the spec. Studio picks its own project out of the list and shows a banner for the first non-null field, with a Troubleshoot menu; a mixed list becomes "multiple resources". Supavise fills `disk_space_exhaustion` (80% and 90% of the project's disk quota), `cpu_exhaustion` (mean busy share of the CPU quota over five minutes, 80% and 90%) and `memory_and_swap_exhaustion` (85% and 95% of the memory limit, or a process killed for lack of memory in the last 15 minutes). `is_readonly_mode_enabled` is false, the others are null. The route needs no organization rule: it lists only what the caller can see, a branch for whoever sees its parent.

## Logs, request charts and hosted-only pages

A node has no log store and no hosted platform behind it. A stub that answers "success" or "nothing" for such a call reads as "no errors" or "it worked", so these calls are answered by handlers (`internal/api/analytics.go`, `unsupported.go`, `webhooks.go`). The renderings below come from reading Studio at the pinned commit; `studio/spike.sh` does not visit these pages.

**Log queries.** `GET` and `POST` `/platform/projects/{ref}/analytics/endpoints/logs.all`, `logs.all.otel` and `logs`, and `GET /v1/projects/{ref}/analytics/endpoints/logs` answer `200 {"result": [], "error": "<string>"}`. The string says that Supavise collects no logs for the dashboard and names ``supavise logs <ref>``; once the cluster has a second node it also names the node that hosts the project, because the command reads the journal of the server it runs on. The spec's `error` is a string or an object whose `code`, `errors`, `message` and `status` are all required, so a string is the only valid short form. `GET /v1/projects/{ref}/analytics/endpoints/logs.all` answers `410`, as hosted does since 2026-09-23 (its spec lists nothing else).

| Caller | What it does with the string |
|---|---|
| Logs explorer, per-product log pages (`useLogsQuery`, `useLogsPreview`) | Show it in the "Error fetching logs" block in place of the table. The event chart above the table stays empty. |
| API report (`useApiReport`), Auth overview error cards (`OverviewErrors.constants.ts`) | Throw it: each chart or card shows the message. |
| Unified logs (`unified-logs-infinite-query.ts`) | Throws it; the page shows its error state. |
| Edge Functions list, last-hour stats | `handleError` on a string, which gives a generic error. |
| API keys, "last used" (`api_keys.last_used.otel`) | Throws it; the page would say it cannot load the requests of the last 24 hours, but the flag that shows the column (`showApiKeysLastUsed`) is off here. |
| MCP `get_logs`, `query_logs` | The tool returns the whole body wrapped as untrusted data, so the model reads the string. |
| Auth report charts (`data/reports/v2/auth.config.ts`), Edge Function overview (`functions.combined-stats`, `functions.req-stats`, `functions.resource-usage`) | **Ignore it and draw zeros.** Studio reads `result` only. No answer from the server changes that short of a log store. |

`auth.metrics` (the Auth overview's numbers) answers `400` with the same pointer: an empty answer shows zero sign-ups and zero active users, and Studio shows its own error alert for a refusal. `service-health` and `project.metrics` answer like the log queries; Studio's Observability overview, which reads them, is behind a feature flag that is off here.

**Request charts.** `GET /platform/projects/{ref}/analytics/endpoints/usage.api-counts` (and the v1 twin) answers one row per bucket of the `interval` Studio sends: `1hr` is 61 minute buckets, `1day` 25 hour buckets, `7day` 8 UTC day buckets (the other values of the spec's enum, `15min`, `30min`, `3hr` and `3day`, work too). Each row holds `timestamp` and `total_rest_requests`, `total_auth_requests`, `total_storage_requests` and `total_realtime_requests`, counted by the edge proxy of the server that answers (`internal/reqstats`). Empty buckets are in the answer, so Studio draws a continuous chart and does not fill gaps itself. `total_rest_requests` counts `/rest/v1` only (`/graphql/v1` and `/functions/v1` are counted separately and not charted). Studio declares the four field names and reads them; it does not show how hosted computes them, so the grouping is Supavise's. The counts start at zero when `supavise serve` starts and cover the requests this server's proxy served: on a two-server cluster the Management API runs on the leader, so the chart leaves out what the follower's proxy served. `usage.api-requests-count` answers the total of the current UTC hour and the 23 before it; Studio does not call it.

**Database Webhooks.** "Enable webhooks" (`POST /platform/database/{ref}/hook-enable`, `POST /v1/projects/{ref}/database/webhooks/enable`) runs `internal/api/webhooks.sql` as `supabase_admin`: `pg_net`, the `supabase_functions` schema with `hooks` and `migrations`, and `supabase_functions.http_request()`, owned by `supabase_functions_admin`. It answers `201` with no body and can run again. Studio then finds the `supabase_functions` schema (that is how it knows hooks are enabled), and creates, edits and drops a webhook as a trigger through `POST /platform/pg-meta/{ref}/query` as the `postgres` role; there is no other webhook call. The file is upstream's `docker/volumes/db/webhooks.sql` with six changes, listed in its header: it can run twice, the pg_net grants that the postgres artifact's own migration removes for pg_net 0.12 and newer are not applied again, the artifact's `grant_pg_net_access` event trigger is kept, and three keep a project member from turning the superuser script into superuser code: it refuses a `supabase_functions` schema that `supabase_admin` does not own (`400`, "The schema supabase_functions already exists and belongs to ..."), it inserts migration rows only into a table it creates in the same run (a trigger, default or rule the project put on an existing table never runs), and it leaves a `supabase_pg_net_admin` role alone. The first `postgres` role call that creates a schema of that name therefore blocks Enable until the schema is dropped or renamed.

**Hosted features this server refuses.** Each answer is a `4xx`, which Studio shows as the reason and does not retry. `disabled_features` in the profile also hides `storage:analytics` and `storage:vectors` (Storage runs without the Iceberg catalog and vector services), `organization:show_legal_documents` (Supabase Inc.'s own documents), `profile:show_account_deletion` (a request to Supabase's support desk) and `projects:transfer`, and `database:restore_to_new_project` on a node whose backup service cannot restore as a new project. The organization's SSO page (`organization:show_sso_settings`) is shown: its calls are the routes of `sso_routes.go`. Logs, Reports and Third-party Auth stay in the menus: Third-party Auth is a feature a server could offer later (PostgREST, Storage and Realtime accept a JWKS), so its page stays and an add is refused with the reason.

| Call | Answer | Studio shows |
|---|---|---|
| `GET`, `PUT /v1/projects/{ref}/ssl-enforcement` | `400` "This project is not allowed to configure SSL enforcements: ..." | The switch is disabled with "Your project does not have access to SSL enforcement". Studio matches this text. |
| `GET .../signing-keys/{id}`, `GET`, `DELETE .../third-party-auth/{tpa_id}` | `404` | |
| `POST .../config/auth/third-party-auth` | `400` | The add dialog's error toast; the list stays empty, which is true |
| `POST /platform/feedback/send` | `400` pointing to the project's issue tracker | "Failed to submit feedback" (the widget would otherwise say the feedback was sent) |
| `GET .../analytics/endpoints/auth.metrics` | `400` | The Auth overview's error alert |
| `GET`, `POST /platform/storage/{ref}/archive` | `400` "Supavise does not build storage archives..." | The paused project's Download backups > Storage objects: an error toast, where the stub made Studio poll every 5 seconds behind a loading toast for ever |
| `POST /platform/database/{ref}/backups/download` | `400` pointing to `supavise backups list` and `pg_dump` | The error toast of Download backups |
| `PUT /platform/update-email` | `400` pointing to `supavise users invite` and `remove` | "Failed to update email: ..." (the stub said a confirmation email had been sent) |
| `POST`, `DELETE /platform/projects/{ref}/billing/addons` for `pitr`, `custom_domain` and any other type than `compute_instance` | `400` "included on this node" | The add-on panels' error toast, where the stub said "Successfully updated" |
| `PATCH` of `config/storage` (`/platform` and `/v1`) that switches `features.imageTransformation.enabled` on | `400` "the node runs no imgproxy" | The Storage settings' error toast. A save that leaves it as it is, or switches it off, passes. |
| `PATCH /platform/auth/{ref}/config` that would change `MFA_ALLOW_LOW_AAL` (or another name the node cannot honor) | `400` naming the setting and the reason, nothing saved | The MFA form's error toast. A value equal to what the GET reports passes. |

The legacy JWT secret and the other signing-keys calls are in "Legacy JWT secret".

Still answered by a stub and therefore empty or inert, unfixed: email template spam validation (`POST /platform/auth/{ref}/validate/spam`: no rule ever matches) and the hosted integrations pages (Vercel, AWS PrivateLink, replication, log drains, billing), most of which are hidden through `disabled_features`. The organization and account audit logs, `daily-stats`, `api/rest` and `api/graphql` have handlers now (below).

**Dashboard dead ends closed by handlers.** Each of these was a stub that answered with an empty or zeroed success. The last column names the Studio page that shows the answer, for a browser run (the release's runtime crawl) and for a person checking by hand.

| Call | Answer | Studio shows | Page to visit |
|---|---|---|---|
| `GET /configuration-files/configcat-proxy/frontend-v2/config_v6.json` (the API origin, no credentials, CORS for the dashboard) | ConfigCat's `config_v6.json`: `allowDataBranching` and `supaviseBranchClones` on, every other flag Studio reads off (`internal/api/configcat.go`) | The Create branch dialog's Include data switch (with patch 0005). Studio logs `Flag key "x" does not exist` for a key the document lacks, so a Studio bump that reads a new key needs the key added. | `/project/{ref}/branches`: open Create branch |
| `GET`, `HEAD /platform/projects/{ref}/api/rest` | PostgREST's OpenAPI document, `503` when PostgREST is down | Integrations > Data API > Docs, the API docs side panel (tables, views and functions), the readiness ping | `/project/{ref}/integrations/data_api/docs` |
| `POST /platform/projects/{ref}/api/graphql` | `graphql_public.graphql()` through the project's PostgREST as `service_role`, or as the role of `x-graphql-authorization` when that token verifies against the project's JWT secret | Integrations > GraphQL (GraphiQL): introspection and queries; Studio's default value of that header, the dashboard user's own token, is not forwarded | `/project/{ref}/integrations/graphiql/graphiql` |
| `POST /platform/auth/{ref}/templates/{template}/reset` | the project's config, with that template's subject and body cleared | Auth > Emails > Reset to default; custom SMTP off | `/project/{ref}/auth/templates/{templateId}` |
| `GET /platform/auth/{ref}/config`: `MAILER_TEMPLATES_CUSTOM_CONTENTS` and `MAILER_SUBJECTS_CUSTOM_CONTENTS` | an object per marker with one flag per template setting, true when a body or a subject is saved for it (`internal/api/config_auth.go`); the spec's minimal object had every flag false | Auth > Emails shows Reset template only for a template reported custom (`hasCustomTemplate` in `TemplateEditor.tsx`), so with every flag false a saved template could not be reset from the dashboard | `/project/{ref}/auth/templates/{templateId}` |
| `DELETE /platform/auth/{ref}/users/{id}/factors` | `200` after GoTrue deleted each factor | Auth > Users > Remove MFA factors | `/project/{ref}/auth/users` |
| `PATCH /platform/auth/{ref}/config` with `AUDIT_LOG_DISABLE_POSTGRES` | stored and rendered as `GOTRUE_AUDIT_LOG_DISABLE_POSTGRES` | Auth > Audit Logs keeps its switch | `/project/{ref}/auth/audit-logs` |
| `GET /platform/organizations/{slug}/audit`, `GET /platform/profile/audit` | the events of requests that changed something (`internal/api/audit.go`, table `supavise.audit_events`, `[api] audit_retention_days`), microsecond timestamps, at most 62 days a query (`400` "Time range exceeded" past it, which Studio tests for) and 5000 rows | Organization > Audit Logs and Account > Audit Logs. A project-scoped member has no organization-wide read, so the organization's page is refused for them. A refused request of somebody who is not a member of the organization it names is in that person's Account page only. | `/org/{slug}/audit` and `/account/audit` |
| `GET /platform/organizations/{slug}/usage`, `GET .../usage/daily`, and the period of `GET .../billing/subscription` | Organization > Usage is in the organization menu and under Project Settings > Billing, so every owner can open it, and every project page asks for the same numbers (the header's "Exceeding usage limits" badge, the Plan usage card). A node has no plan with quotas, so the answer says what the node measures, as unlimited: one `DATABASE_SIZE` entry, the sum over the organization's databases (or the project named by `project_ref`) of the collector's daily mean `pg_database_size`. Only `usage` is in GB (decimal, 1e9 bytes, as the label next to the metric and the tooltip's Total row print it); `usage_original` and the `usage` of each entry of `project_allocations` are in bytes, because Studio divides an allocation by 1e9 in the hover table (`formatUsage`) and passes it to `formatBytes` in the Database size section, whose 0.5 GB check compares it with `0.5 * 1e9`. The daily answer has one point a day, with the same two fields, and the charts read `usage_original` (`internal/api/usage.go`). It has no entry for egress, Storage size, active users, Edge Function invocations, Realtime or logs, which the node does not measure, so the page shows those sections without a figure and not with a zero. The subscription's period is the calendar month in UTC, as the page prints it (the stub's zero printed "01 Jan 1970 - 01 Jan 1970") | Organization > Usage and Project Settings > Billing > Usage: the period in the header, Database size in the usage summary. Egress, Storage and Activity show "No data in period" | `/org/{slug}/usage` |
| `Access-Control-Expose-Headers: ETag, Retry-After, X-Request-Id` on every answer to the dashboard's origin | ConfigCat's browser SDK reads the `ETag` of the flag document with a cross-origin request, which the browser allows for an exposed header only: the console logged `Refused to get unsafe header "ETag"` on every sign-in. `Retry-After` is what Studio reads from a `429` (`internal/api/server.go`) | No visible change; the console is clean | any signed-in page |
| `GET /v1/projects/{ref}/health?services=functions` | the node's Edge Runtime as a `functions` entry (`ACTIVE_HEALTHY`, `COMING_UP`, `UNHEALTHY`, or `DISABLED` when `[functions]` is off), from the node's health report (`internal/api/projects.go`). Added only when the request names it, because the spec's service enum has no such name | The project home's Status and its Edge Functions row (patch 0011). Upstream probed a function on Supabase's platform, which made every project read Unhealthy without internet access | `/project/{ref}` |
| `GET`, `POST /platform/database/{ref}/clone`, `GET .../clone/status` | the backups to restore from, the start of a restore as a new project, and the clones of the project (`IN_PROGRESS`, `COMPLETED`, `FAILED`, `REMOVED`) from the source project's events (`internal/api/clone.go`) | Database > Backups > Restore to new project | `/project/{ref}/database/backups/restore-to-new-project` |
| `GET /platform/projects/{ref}/daily-stats` | `AnalyticsData` for the request counts the edge proxy keeps (`total_requests`, `total_rest_requests`, `total_auth_requests`, `total_storage_requests`, `total_realtime_requests`, `total_func_invocations`), one point for each UTC day of the last eight, counted from the daemon's start; `hasNoData` for the other attributes | Reports > custom chart blocks | `/project/{ref}/observability/{id}`: a custom report with a chart block |

**Shown, not yet seen in a browser.** The organization's SSO page (`/org/{slug}/sso`) was hidden through `disabled_features` (`organization:show_sso_settings`) and is shown since this release. Its four calls (`GET`, `POST`, `PUT` and `DELETE /platform/organizations/{slug}/sso`, `sso_routes.go`) match the page's source at the pinned commit, including the `404` "Failed to find an existing SSO Provider" that tells it no provider is set up, but no browser run has loaded the page: Studio shows an "unknown interface" for it while the key is disabled, so only a run with the key enabled can settle it. If it is a dead end, put the key back in `disabledFeatures` (`internal/api/identity.go`) and in the hide list of `unsupported_test.go`.

Open page questions a browser run settles: whether the Auth report and the Edge Function overview draw an error or zeros when a log query answers a non-200 (they draw zeros for the `200` with `error` they get now), and whether the Edge Function Test tab sends a request on a node without TLS (patch 0010 lets it; the crawl's probe `edge-function-test-tab` decides).

## Where the spec and Studio disagree

- `GET /platform/projects/{ref}/status`: the spec documents a 200 without a body; Studio reads `{status}`.
- `POST /platform/pg-meta/{ref}/query`: the spec documents 201 and no body; Studio needs the row array. The spec also lists ten `GET /platform/pg-meta/{ref}/...` routes (tables, columns, ...) that Studio's client does not call in this version.
- `HEAD /platform/projects/{ref}/api/rest`: only `GET` is in the spec; Studio sends `HEAD` to check that PostgREST is up. `GET /platform/projects/{ref}/api/rest` returns the PostgREST OpenAPI document (API docs page); both are handled by `internal/api/rest_graphql.go`. Before that the stub answered both with an empty `200`, which the docs page read as a TypeError and the ping as "up" whether or not PostgREST ran.
- `GET /platform/projects/{ref}/daily-stats`, `.../infra-monitoring`, `GET /platform/organizations/{slug}/sso` and `GET /v1/projects/{ref}/jit-access`: the spec documents no body. Supavise answers `infra-monitoring` in the two shapes Studio reads (see "Utilization and resource warnings") and `daily-stats` as `AnalyticsData` (below), and keeps the empty answer for the rest.

## Studio's own /api routes

`apps/studio/proxy.ts` answers 404 for every `/api/*` path except the allowlist in `lib/hosted-api-allowlist.ts`. The Studio process serves those, not the Management API. `studio/verify.sh` checks that `/api/platform/profile` answers 404 and `/api/get-utc-time` answers 200 in the packaged build.

| Route | What it does | Needs outside access? |
|---|---|---|
| `/api/get-utc-time`, `/api/get-ip-address`, `/api/get-deployment-commit` | clock, caller IP, build commit | no |
| `/api/mcp` | Studio's MCP server (`POST` only, stateless Streamable HTTP) | in platform mode patch 0004 serves it, built on the bearer token it receives and calling the Management API on loopback (`SUPAVISE_MANAGEMENT_API_URL`). The proxy answers `404` for it on `studio.<domain>` and forwards to it only from `api.<domain>/mcp`, after the gate has authenticated the caller. `search_docs` reaches `supabase.com` |
| `/api/incident-banner` | incident.io banner list, requested on every page | yes: incident.io. Answers 500 without a key and delays sign-in; the Supavise proxy answers it with a static `{"incidents": []}` on the Studio host (see "Findings from running Studio") |
| `/api/incident-status` | the status chip of `/support/new` and the new-project page | yes: statuspage.io. In platform mode the route answers `500 {"error": "Unable to fetch incidents at this time"}` while `STATUSPAGE_PAGE_ID` and `STATUSPAGE_API_KEY` are unset, which is every node; Studio logged `[getIncidentStatus] Failed`, react-query retried with backoff and the chip never resolved. The Supavise proxy answers `GET` and `HEAD studio.<domain>/api/incident-status` with `200 []`, as it does for the banner (`internal/proxy/studio.go`) |
| `/api/status-page`, `/api/status-override` | status banner and incident pages | yes: statuspage.io, incident.io; failure behavior unverified |
| `/api/ai/*` | AI Assistant and the SQL editor's AI features | the model provider the operator set (`[studio.ai]`), and nothing else; patch 0006 and "The AI Assistant" below |
| `/api/check-cname`, `/api/edge-functions/test`, `/api/edge-functions/body`, `/api/generate-attachment-url`, `/api/content/graphql`, `/api/parse-query`, `/api/scoped-access-token-permissions`, `/api/integrations/stripe-sync` | custom domains, edge function tester, support attachments, docs search, SQL parsing, token scopes, Stripe | mixed; none is on the P0 flows |

## The AI Assistant

Studio's AI routes (`/api/ai/sql/generate-v4` for the Assistant, `title-v2`, `policy`, `cron-v2`, `filter-v1`, `code/complete` and the others on the allowlist of `lib/hosted-api-allowlist.ts`) run in Studio's server, one `fetch` per call, read from the code at the pinned commit. Each needs `Authorization: Bearer <dashboard session>`: `apiWrapper` checks it with the dashboard GoTrue (`NEXT_PUBLIC_GOTRUE_URL`, so the node's `api.<domain>`), and the calls below also go to `NEXT_PUBLIC_API_URL`. Studio's server therefore has to reach the node's public address. Patch 0006 and `supavise studio ai` supply the provider (`studio/README.md`, `docs/guide.md`).

| Studio asks | Supavise |
|---|---|
| `GET /api/ai/sql/check-api-key` | Patch 0006 allowlists it and asks it in platform mode (upstream answers `{hasKey: true}` without asking there). Answers `{hasKey, model}`: `hasKey` is whether the operator set a provider, `model` the operator's chat model when there is one. A caller without a session gets 401 |
| `GET /platform/organizations` | answered: `opt_in_tags` is the saved AI level; `getAiOptInLevel` maps `AI_SQL_GENERATOR_OPT_IN` to schema, plus `AI_LOG_GENERATOR_OPT_IN` to schema and logs, plus `AI_DATA_GENERATOR_OPT_IN` to schema, logs and data |
| `PATCH /platform/organizations/{slug}` with `{opt_in_tags}` | answered and stored (migration 1550). Owner only, as the organization's update permission is |
| `GET /platform/organizations/{slug}/entitlements` (`assistant.advance_model`) | answered: every feature is granted |
| `GET /platform/projects/{ref}`, `GET /platform/projects/{ref}/settings` | answered; the pair confirms that the project belongs to the organization, otherwise the Assistant falls back to the most restrictive level |
| `POST /platform/pg-meta/{ref}/query` (the schema list for the context, `execute_sql`, `list_policies`) | answered |
| `GET /platform/projects/{ref}/content` and `.../content/item/{id}` (`list_reports`, `get_report`: the saved reports) | answered, as for the Reports page |
| the remote MCP server at `NEXT_PUBLIC_MCP_URL` (`search_docs`, `list_tables`, `list_extensions`, `list_edge_functions`, `list_branches`, `get_advisors`, `query_logs`) | refused with 401 by the `/mcp` gate: it accepts OAuth and personal access tokens, and Studio forwards the dashboard session. `getTools` logs the failure and goes on without these tools |
| `https://supabase.com/docs/api/graphql` (docs search) | not reached: `search_docs` is one of the MCP tools above |

What each opt-in level lets Studio put in a request to the provider is in the guide. Studio's server sends no telemetry for the Assistant when no Braintrust key is set, and Supavise sets none.

## Calls from the browser to a project

These are not Management API calls. They need the project hosts (`<ref>.api.<domain>`) reachable from the browser and allowed by the CSP (`CSP_EXTRA_PROJECT_HOSTS`, Studio patch 0003; over `https` and `wss`, and also over `http` and `ws` on a node with `tls.mode = "off"`, where `studio/runtime` widens them: the sources used to be `https` and `wss` only and the browser blocked every one of these calls, with a CSP violation in the console).

| Call | Feature |
|---|---|
| `wss://<ref-host>/realtime/v1` | Realtime inspector (P2) |
| `<ref-host>/functions/v1/<slug>` | edge function details and AI tester (P2) |
| `<ref-host>/auth/v1/admin/oauth/*`, `/auth/v1/.well-known/*` | OAuth server apps, custom providers and JWKS dialog (P2); the client uses a temporary API key from `POST /platform/projects/{ref}/api-keys/temporary` |
| `<ref-host>/rest/v1/`, `/storage/v1/s3`, `/storage/v1/iceberg`, `/storage/v1/vector` | shown as copyable URLs, not called |
| `https://obuldanrptloktxcffvn.supabase.co/functions/v1/health-check` | Supabase's own edge-functions health probe: a hard-coded host (it is not blocked: `*.supabase.co` is in `connect-src`), which reports Supabase's platform, not the node, and fails for good without internet access, so the Edge Functions row and the project's overall status read Unhealthy. Patch 0011 removes the call: the row asks `GET /v1/projects/{ref}/health?services=functions` |

## Findings from running Studio

`studio/spike.sh` runs this Studio build against the mock Management API with a real Postgres, GoTrue and postgres-meta, and drives it with headless Chrome. It visits sign-in, the project list, and for each of two projects the table editor, SQL editor, project home, Auth users, Storage files, Database tables and Settings. Other pages (Realtime, Edge Functions, Logs, Advisors, Integrations, Reports, organization and account pages) are readings of the code only. The findings:

1. **Sign-in waits for the incident banner.** After the token call Studio waits for `GET /api/incident-banner`, which answers 500 without an incident.io key; react-query retries after 1, 4 and 16 s, and the sign-in form awaits the query cache reset, so sign-in took about 22 s. The Supavise proxy answers `GET studio.<domain>/api/incident-banner` with a static `{"incidents": []}` (`internal/proxy/studio.go`), which needs no patch to Studio. Sign-in then takes about 1 s.
2. **`null` in an array field crashes a page.** `GET /v1/projects/{ref}/upgrade/eligibility` with `validation_errors: null` threw in Settings, General. Stubs return `[]` for array fields. The analytics endpoints (`usage.api-counts`) need `{"result": []}`: a bare `{}` shows "Failed to load project usage" on the project home. (`usage.api-counts` now answers real counts; see "Logs, request charts and hosted-only pages".)
3. **Studio calls Usercentrics on every page load** (`https://api.usercentrics.eu/settings//latest/languages.json`, with an empty settings id) even though `NEXT_PUBLIC_USERCENTRICS_RULESET_ID` is unset. Studio's CSP allows the host by default, so the proxy removes every `usercentrics.eu` source from the `Content-Security-Policy` it forwards and the browser refuses the request. A patch that skips initialization when the ruleset id is unset would remove the cause; none exists.
4. **Health status values.** `/v1/projects/{ref}/health` entries use `status: "ACTIVE_HEALTHY"` (enum `COMING_UP | ACTIVE_HEALTHY | UNHEALTHY`). Any other value shows an alert icon and the raw text per service and re-polls every 5 s.
5. **Other values the P0 handlers must get right.** `GET /platform/projects/{ref}/databases` must list the primary database (an empty list makes the SQL editor offer a "Read Replica" source), and one more entry for each replica. The project `region` must be a real AWS region code (an unknown one requests `/img/regions/undefined.svg`). The project database needs GoTrue's migrated `auth` schema, or the Users page reports `column users.banned_until does not exist`.
6. **Dashboard sessions are identified by `aud` and signature.** GoTrue's admin-created user has an empty `auth.users.role`, so its access token carries `"role": ""`. The API server accepts a session when `aud` is `authenticated` and the signature is valid, and does not read the role claim.
7. **postgres-meta closes idle keep-alive connections after 5 s.** A Go client that reuses a connection at that moment sees `EOF`, so `IdleConnTimeout` stays below 5 s (the mock uses 2 s) or the call is retried. A browser navigation cancels in-flight queries; that is not a server error.
8. **Console noise that does not matter:** `Minified React error #418` once on `/sign-in` (hydration mismatch from the "last used" badge), Monaco `Canceled` rejections, one 401 from `telemetry/feature-flags` before the session exists, and the blocked edge-functions health probe.
9. **`upgrade-insecure-requests` blanks every page of a plain-http node.** Studio's production build sends the directive, and a browser that honors it turns the page's scripts, style sheets and fonts into `https` requests even when the page came over `http`. A node with `tls.mode = "off"` listens for no `https`, so the Next redirect pages (`/project/{ref}/api`, `/maintenance`, ...) stayed blank with `net::ERR_SSL_PROTOCOL_ERROR` on every asset, and the chunk that GraphiQL's Web Worker imports failed the same way. The crawl found it: its browser removed the directive from the documents it fulfilled itself, which a redirect hop and a worker script are not, so they kept the policy of their own response (the part about the redirect hop is read from the crawl's report, not reproduced). The proxy removes the directive from every Studio response while the dashboard URL is `http` (`internal/proxy/studio.go`).
10. **The code editors need Monaco, and the platform build loaded it from cdnjs.** `configureMonacoLoader` takes `https://cdnjs.cloudflare.com/ajax/libs/monaco-editor/0.52.2/min/vs` when `IS_PLATFORM` is true. Without a route to cdnjs the Auth email template editor, the Logs explorer, the SQL editor and the function editors stay on a spinner (`Monaco initialization: error: Event`). Patch 0008 loads it from `/monaco-editor/vs`, which every build ships in `public/`, as an absolute URL (the page's origin first): Monaco starts its editor worker from a `blob:` URL and gives it the base of `vs`, and a `blob:` URL is no base for a relative URL, so with a relative path the HTML and TypeScript language workers failed with `Failed to parse URL from /monaco-editor/vs/language/html/htmlWorker.js` (a page error on every Auth template page, `/functions/new` and `/functions/{slug}/code`).
11. **Some redirects use the ref before the router has it.** On a page that Next prerendered the first render has an empty route query. `/project/{ref}/sql` redirected to `/project/undefined/sql/new` (the history query resolved first) and the Compute layout and the project webhooks page redirected to `/project/undefined`; Studio then showed the organization page and asked for `/platform/projects/undefined`. Patch 0009 makes the three effects wait for the ref. Read from the source, not reproduced in a browser.

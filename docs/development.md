# Development

How to build, test and change Supavise. [design.md](design.md) is the contract the code follows; each package documents itself in its own `README.md`.

## Build

- Go 1.26 or later (`go.mod`). The binary is static: `CGO_ENABLED=0`.
- `make build` writes `bin/supavise` for this machine. `make build-linux` writes `bin/supavise-linux-amd64` and `bin/supavise-linux-arm64`.
- `make fmt` runs `gofmt -w` over the tracked Go files.

## Test

- `make test` runs `go vet ./...` and `go test ./...`.
- CI runs `gofmt -l`, `go vet ./...` and `go test -race -count=1 ./...` on amd64 and arm64, with a Postgres 17 service for `SUPAVISE_TEST_DATABASE_URL`.
- The Deno main service of Edge Functions (`internal/functions/mainservice`) has its own checks: `deno fmt --check`, `deno lint`, `deno check` and `deno test`.
- Node (22) runs `tests/conformance`, the Studio build and spike (`studio/`), the OAuth walk (`tests/linux/oauth/`), the Studio call scan (`studio/scan-calls.mjs`) and the Studio crawl (`tests/linux/crawl/`).

## Integration tests on a workstation

These tests skip unless their variable is set. They start real Postgres and service processes from the unpacked slim-services artifacts, so they need a machine with a few hundred MB to 1 GB free.

| Variable | Needs | Runs |
|---|---|---|
| `SUPAVISE_TEST_DATABASE_URL` | a Postgres 17 connection URL | registry, proxy and lifecycle tests against a real database |
| `SUPAVISE_TEST_UNPACKED` | directory of unpacked artifacts | lifecycle, branching and app integration tests |
| `SUPAVISE_API_INTEGRATION=1` | `SUPAVISE_PG_BIN` (Postgres `bin` directory), `SUPAVISE_PGMETA_BIN` (pgmeta binary) | `go test ./internal/api -run Integration -v`; listens on 127.0.0.1 in 32100-32999 |
| `SUPAVISE_SSO_INTEGRATION=1` | `SUPAVISE_TEST_UNPACKED`, `SUPAVISE_SSO_PYTHON` (a Python with `signxml`) | `go test ./internal/api -run IntegrationDashboardSSO -v` |
| `SUPAVISE_SETTINGS_E2E=1` | `SUPAVISE_TEST_UNPACKED` | `TestSettingsIntegration` in `internal/app` |

The unpacked artifacts default to `~/.cache/sbctl/unpacked`, a path the test code reads (`serve-stack.sh` and the backup tests use it when no variable is set). The backup tests also accept `SUPAVISE_TEST_PG_BIN`.

`internal/api/testdata/serve-stack.sh /path/to/stack.json` starts the API integration stack (Postgres, pgmeta and the API server on 127.0.0.1) and keeps it running for real CLI and MCP clients. It writes `{api_url, pat, jwt, ref, dsn, pg_port}` to the file; delete the file to stop the stack.

`node --test tests/linux/crawl/selftest.mjs` checks the logic of the Studio crawl (which pages to visit, the allow-list, the mutation guard, the report) with no variables; `CRAWL_STANDIN_CHROME=/path/to/chrome` adds two tests that run the crawl driver in a real browser against a stand-in node (`tests/linux/crawl/standin.mjs`). `node tests/linux/oauth/selftest.mjs [--mcp]` runs the OAuth walk of the Linux job against a stand-in node (`fake-node.mjs`) with no variables, no root and no Postgres. It checks the walk against [design.md](design.md) section 13, so run it after changing the walk and before pushing, instead of waiting for CI.

## CI

| Workflow | Jobs |
|---|---|
| `ci` | `test`, `cfn-lint`, `aws-deploy`, `actionlint`, `functions-main` |
| `linux` | `systemd-smoke`, `backup-integration`, `pebble`, `fleet-smoke`, `settings-smoke`, `compute-smoke`, `backups-smoke`, `upgrade-smoke`, `upgrade-e2e`, `roles-smoke`, `sso-smoke`, `install-e2e`, `os-updates`, `functions-smoke`, `branching-xfs` (which also runs `branching-egress`), `converge-e2e`, `firstboot-e2e`, `cluster-identity`, `replica-blocks`, `fleet-follower`, `storage-migrate`, `oauth-smoke`, `migrate-smoke` |
| `conformance` | `suites`, `specdiff`, `bump-check` |
| `studio` | `changes` (which parts the push touched), `build` (runs `studio/build.sh`, then `studio/spike.sh`; only when the push touched Studio), `mcp-e2e` (the OAuth walk, MCP over HTTP and the six branch tools on the amd64 build; `tests/linux/oauth-smoke.sh --studio-artifact`) |
| `studio-crawl` | `changes`, `calls` (`studio/calls.json` is what the scan of the pinned, patched Studio source gives), `selftest` (the logic of the crawl), `crawl` (`tests/linux/studio-crawl.sh`: every page of the Studio build and the user-flow probes against a real node, in a headless Chromium) |
| `footprint` | manual; see [reference/footprint.md](reference/footprint.md) |
| `bump-proposals` | nightly; opens one pull request per upstream release newer than its pin |
| `release` | on a `v*` tag |
| `replication-spike` | the two-node Incus harness of `tests/linux/multi` on both architectures, when the harness changes on `main` or `integrate/**`, or by hand |
| `replication` | `two-servers`: the two-server release test (`tests/linux/multi/replication.sh`) on both architectures: install, join, a read replica, planned and unplanned failover, rejoin, and the upgrade of both servers. It runs on pushes to `integrate/**` and by hand, on both runners |
| `screenshots` | README screenshots |

The `studio` and `studio-crawl` workflows start on every push, and their `changes` job (`tests/linux/crawl/changes.sh`) decides what runs, so that a push to `main` always has a run of both: the release gate (`deploy/release-gate.sh`) requires the `mcp-e2e` job of `studio.yml` and the `crawl` job of `studio-crawl.yml` on the tagged commit, besides `ci.yml`, `linux.yml`, the conformance suite, the upgrade test and the two-server test. `build` runs when the push touched `studio/`, `internal/versions/versions.yaml` or `studio.yml`; `mcp-e2e` runs on `main` always and on other branches when the push touched Studio, the API server, the proxy, the fleet, the OAuth walk or `lib.sh`; `crawl` runs on `main` always, nightly, by hand, and on other branches when the push touched the API server, the proxy, the fleet, project configuration, branching, domains, GitHub, the registry, `studio/` or `tests/linux/crawl/`. Neither builds Studio unless the push changed it: they take the amd64 build of the newest successful `studio.yml` run on `main` that has one (`tests/linux/crawl/fetch-studio.sh`), or, for a push that changed Studio, the build that `studio.yml` makes for that commit. Start either by hand with `gh workflow run <file> --ref <branch>`, for example after a change to the walk or the crawl alone on a branch that does not match the paths.

### The Studio crawl and the call gate

Two checks keep the dashboard from gaining a dead end unnoticed. Both read the same file, `tests/linux/crawl/allowlist.json`.

- **The static gate.** `studio/scan-calls.mjs` parses the pinned Studio source (with `studio/patches` applied) with the TypeScript parser pinned in `studio/scan/package.json` and writes `studio/calls.json`: every `(method, path)` Studio sends to the Management API, with the paths resolved through template literals, constants, small functions and the call sites of a function that takes the path as a parameter. `internal/api/studio_calls_test.go` joins the file with the server's route table (the same mux `Server.build` makes) on every `go test`, and fails for a Studio call that only a spec-derived stub or the unknown-route fallback answers, unless the call is in the allow-list with a reason; it also fails for an allow-list entry whose call has a handler now, so the list only shrinks. After a Studio bump, a patch that changes calls, or a new handler, run `node studio/scan-calls.mjs --fetch` (it needs git and network; `npm ci --prefix studio/scan` first) and commit `studio/calls.json`; the `calls` job of `studio-crawl.yml` fails while the file is stale. The scan finds what the source states: a call made through a wrapper it does not follow shows up under `mentions` or `unresolved` in the file, and the crawl is the authority on it.
- **The runtime crawl.** `tests/linux/studio-crawl.sh` starts a node on the Studio build, makes fixtures (projects, branches in each state, a second organization, custom hostnames) and runs `tests/linux/crawl/crawl.mjs`: every page of the build, then user-flow probes judged by the state of the server (see `tests/linux/README.md`). The failure rule is in the header of `crawl.mjs`. When the first run after a change fails on something that is not a regression (a page the fixtures do not cover, a refusal that is honest), fix the cause or list it with a reason in `allowlist.json` (stub answers and refusals), `noise.json` (browser noise) or `routes.json` (a page not visited); the report says which entry an allow-list line should be, and entries that a run hit but are marked `static` are listed so they can become `crawl` entries.

## Conventions

Rules the code follows. A change that breaks one needs a reason in [design.md](design.md).

| Thing | Convention |
|---|---|
| Binary and user | `supavise`; units run as the system user `supavise` (the one-shot `supavise-edge-bundle@` unit uses a dynamic uid), so artifacts are relocatable and nothing needs root after install |
| Config | `/etc/supavise/config.toml`, `SUPAVISE_*` environment overrides, secrets key `/etc/supavise/master.key` (mode 0600) |
| State root | `/var/lib/supavise/`: `artifacts/<service>/<version>/`, `projects/<ref>/<service>/` with env files at `projects/<ref>/<service>.env` (0600), `system/<service>/` for fleet services, `certs/`, `backups/` (local backend). The Edge Functions tree is `system/edge-runtime/tenants/<ref>` |
| Logs | journald; the unit name selects the log |
| Units | templates `supavise-postgres@`, `supavise-gotrue@`, `supavise-postgrest@`; singletons `supavise-supavisor`, `-realtime`, `-storage`, `-pgmeta`, `-studio`, optional `-imgproxy` and `-edge-runtime`; all in `supavise.slice`, with `MemoryMax` and `CPUQuota` per project |
| Ref | 20 lowercase ASCII letters; `system` is the reserved ref of the system project |
| Ports | project services listen on loopback: Postgres `20000 + 3n`, GoTrue `20001 + 3n`, PostgREST `20002 + 3n` (`n` is the project's registry sequence). Public: Supavisor 5432 (session) and 6543 (transaction), the proxy on 80 and 443. Loopback: Realtime 4000, Storage 5000 (admin 5001), imgproxy 5002, Studio 3000, pgmeta 8080, edge-runtime 9000, system Postgres 5433, system GoTrue 9999, admin API 7000, the Storage credential endpoint 4010 (`[fleet] storage_credentials_port`). A replica of project `n` on a node that is not its home: Postgres `10000 + 3n`, PostgREST `10002 + 3n` (`[ports] replica_base`), and the standby of the system cluster on `10000`. Between the nodes of a cluster: TCP 7443 (`[node] peer_listen`). All are set in `internal/config/layout.go` and changeable in `[ports]` |
| Hostnames | project `<ref>.api.<domain>`, replica `<ref>-rr-<region>-<id6>.api.<domain>`, load balancer `<ref>-lb.api.<domain>`, dashboard `studio.<domain>`, API `api.<domain>`, pooler `pooler.<domain>`; Realtime gets `Host: <ref>.realtime.internal`, Storage gets `x-forwarded-host: <ref>.api.<domain>`. Without a domain, `<ip>.sslip.io` is the base domain |
| Keys | per project: a 40-character JWT secret, legacy `anon` and `service_role` JWTs, and `sb_publishable_<base58>` and `sb_secret_<base58>` keys that the proxy maps to the legacy JWTs. Personal access tokens are `sbp_` plus 40 lowercase hex characters. OAuth access tokens are `sbp_oauth_` plus 40 hex, refresh tokens `sbr_` plus 64 hex, authorization codes `sbc_` plus 64 hex and client secrets `sba_` plus 64 hex; they live in the `oauth_*` tables and not in `access_tokens` |
| Registry | database `supavise` in the system cluster, schema `supavise`. Migrations in `internal/registry/migrations` are embedded in the binary and applied in file-name order; number ranges per area are listed in `0001_init.sql` and continue in the later files. Migrations from 1300 on only add (create a table or index, add a column with a default), so a binary one minor behind runs against the newer schema; `migrations_lint_test.go` enforces it |
| Versions | `internal/versions/versions.yaml` is the only pin source: `artifacts.<service>`, `studio.tag`, `studio.patchset`, `cli.version_tested`. `studio.patchset` equals `studio/PATCHSET`: the `internal/versions` test, `studio/build.sh` and `releasetool manifest -patchset` refuse a difference, so bump both in the commit that changes the Studio patches or runtime. The Studio build is `<tag>-p<patchset>`. A new `studio.tag` also needs `internal/advisors/lints.sql` generated again at the new commit (`internal/advisors/README.md`); `TestVendoredSQLMatchesTheStudioPin` fails until it is. |
| API types | generated from the three Management API specs (`v1`, `v2`, `platform`) in `internal/api/gen/` with `go generate`; never hand-written. Refresh the specs with `internal/api/gen/fetch-specs.sh` |
| Interfaces | `lifecycle.DataPlane` (`Create`, `Delete`, `Snapshot`, `Route`, `Usage`), `fleet.Tenant` (`EnsureTenant`, `RemoveTenant`), `units.Supervisor` (`Render`, `Start`, `Stop`, `Status`, `Remove`), `secrets.Secrets` (`Seal`, `Open`), `backup.Backup` (`PushWAL`, `FetchWAL`, `BaseBackup`, `Restore`) |
| Errors | the Management API envelope `{"message": "..."}` with the upstream status codes |
| License | Apache-2.0 for everything written here; the Studio patches carry upstream's Apache-2.0 |
| Provenance | no code copied from `kmhari/supastack` (AGPL); reading it for reference is fine |
| Name | the product name must not contain "Supabase"; Supavise is not affiliated with Supabase Inc. |

**Decided.** Do not reintroduce these without a written reason in [design.md](design.md) section 10: one Postgres cluster per project; native artifacts and no Docker; the proxy inside the binary and no Envoy; Supavisor as the only Postgres entry point; WAL archiving through the binary and no wal-g; wildcard DNS required; the platform-mode Studio build with exactly four patches (three upstreamable, and patch 0004 that serves the MCP route; the reason is in section 10); API types generated from the specs.

## Releases

A `v*` tag starts `.github/workflows/release.yml`: it checks that the commit has passed its tests, builds the binaries and the Studio artifact, signs the checksum list and publishes the release. Signing key setup and the release steps are in [deploy/README.md](../deploy/README.md), "Release signing".

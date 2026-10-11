# internal/svclog

`supavise logs <ref>`: the log lines of one project, read from the systemd journal. A node keeps no log store for the dashboard (the Logs pages and the MCP log tools answer with an error that points here; `internal/api/analytics.go`), and the unit name selects the log (design section 2).

## Which logs

| `--service` | Unit | Lines shown |
|---|---|---|
| `postgres` (`db`, `database`) | `supavise-postgres@<ref>` | all |
| `auth` (`gotrue`) | `supavise-gotrue@<ref>` | all |
| `rest` (`postgrest`, `api`) | `supavise-postgrest@<ref>` | all |
| `realtime` | `supavise-realtime` | lines that mention the ref |
| `storage` | `supavise-storage` | lines that mention the ref |
| `pooler` (`supavisor`) | `supavise-supavisor` | lines that mention the ref |
| `functions` (`edge-runtime`, `edge-functions`) | `supavise-edge-runtime` | lines that mention the ref |

Without `--service` the command merges all of them. Realtime, Storage, Supavisor and the Edge Runtime serve every project and carry the project as the tenant in their lines, so a line that does not contain the ref belongs to another project and is not shown. Function console output has no tenant tag and is not selected (`supavise functions logs --all` shows the whole runtime log). The filter is a text match, so a service that logs the tenant under another form is not found. The ref `system` is the dashboard's own Postgres and Auth.

## How it reads

One `journalctl -u <unit> --quiet --all -o json -n N` per log (with `--grep <ref>` for a shared unit, and `--since` when given), merged by time, the last `-n` lines printed. `--quiet` keeps `-- No entries --` out of the JSON stream. With `--grep`, journalctl exits 1 without a message when nothing matched, as grep does; that is an empty answer, not a failure. The default is 100 lines. `--all` is needed because `journalctl -o json` prints any field of 4096 bytes or more as `null`: without it a long statement or JSON line would come out as an empty message, and a long line of a shared unit would not match the ref. A line is `<UTC time> <service> <message>`. A message with control characters (a statement in the Postgres log can hold an escape sequence) is printed with them as `\xNN`.

With `--follow` the command starts one `journalctl -f -n 0` per log first, prints the history, then the new lines; a line that is in both is printed once, by its journal cursor. Ctrl-C ends it.

`--since` takes an age (`90s`, `30m`, `2h`, `1d`, `1w`) or a time (`2026-10-10T12:00:00Z`, `2026-10-10 12:00:00` or `2026-10-10`, all UTC). The command checks the ref only for its shape (20 lowercase letters or `system`) and not against the registry, so it works while the system database is down, which is when logs are most needed.

Reading the system journal takes root or the `systemd-journal` group; the `supavise` user is in neither, so run `sudo supavise logs <ref>`. A run that prints nothing says so and, for a user who is not root, says why that may be.

With `supervisor = "exec"` (development and the tests that start real services) there is no journal: the command prints the end of each unit's log file under `<state_dir>/logs`, and `--since` is refused and `--follow` ignored.

## Limits

- Postgres logs only what its settings log; `log_statement` is `ddl` by default, so a query that is not DDL is not in the log.
- The journal is a node's. On a cluster the command reads the journal of the server it runs on, which holds the units that run there: run it on the node that hosts the project (the dashboard's log message names the node once a second node exists). A project that moved to another node (failover) has its earlier lines on the old one.
- Auth, REST, Realtime and Storage are started with a warning log level (`internal/lifecycle/env.go`, `internal/fleet/services.go`), so they print lines only for problems; a quiet service is not a missing log. `tests/linux/roles-smoke.sh` provokes a warning from Realtime, Storage and the pooler and requires only that every line shown mentions the ref: whether each service's warning carries the tenant is not verified.
- A journal that is not persistent (`Storage=volatile`) loses its lines at reboot.

## Tests

`go test -race ./internal/svclog` runs the merge, the filter, `--since` and follow against a fake `Journal`, and the process handling (a failing exit with its message, a clean exit, a program stopped while it still runs) against shell scripts that stand in for `journalctl`. The real command against `systemd` is checked by `tests/linux/roles-smoke.sh`, which runs in the `roles-smoke` job.

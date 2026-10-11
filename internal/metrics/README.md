# internal/metrics

What each project uses, measured on the node and kept for Studio's reports. Once a minute the collector reads the control groups of every project homed on this node, asks its Postgres one short question, reads its disk quota, and appends one sample to a file. The Management API turns those samples into the series of Studio's Database report (`GET /platform/projects/{ref}/infra-monitoring`) and into the banners above a project (`GET /platform/projects-resource-warnings`). The alert checker reads the same data for the `memory_low` and `project_disk_high` alerts.

Nothing here runs a process of its own: the collector is a goroutine of the daemon, and it opens no connection that outlasts one sample.

## What a sample holds

| Source | Read | Fields |
|---|---|---|
| Control groups of `supavise-postgres@<ref>`, `supavise-gotrue@<ref>` and `supavise-postgrest@<ref>` under `/sys/fs/cgroup/supavise.slice/` | `cpu.stat`, `memory.current`, `memory.stat` (`anon`, `shmem`, `file`), `memory.swap.current`, `memory.events` (`oom_kill`), `memory.max` | CPU as a share of the project's quota, memory used and cached, swap, OOM kills |
| One SQL session over the project's private unix socket as `supabase_admin`, closed after the sample | `pg_stat_activity`, `max_connections`, `pg_database_size`, `pg_ls_waldir()`, `pg_stat_io`, `pg_stat_wal` | connections by role, by state and from Supavisor, database and WAL size, block reads and writes |
| The project's disk quota | `statfs` on the project's directory where an XFS project quota is in force; the sum of the files' sizes (at most every five minutes) otherwise | disk used and size |

- **CPU** is the project's CPU time in the minute divided by the quota's (`CPUQuota=100%` is one core; a project without a quota is measured against the node's cores). The three units of a project each have the quota to themselves, so the sum is scaled to stay at or below 100.
- **Memory** used is `anon + shmem`, summed over the units (Postgres's `shared_buffers` is shmem). Cache is `file - shmem`. The total is the project's `MemoryMax`.
- **Rates** need two readings. The first sample after the daemon starts has no CPU or I/O rates (they stay unknown, which the charts draw as gaps), and a counter that went backward (a unit restarted) counts from zero.
- **OOM kills of a unit that restarted.** With systemd's default `OOMPolicy=stop`, a unit that has a process killed for lack of memory is stopped and started again in a new control group, whose `oom_kill` counter starts at zero and says nothing of the kill. The collector therefore also reads `oom_kill` in the `memory.events` of `supavise.slice`, which the kernel keeps for all groups below the slice and does not lose when a group is removed. A project one of whose units started over since the last sample (another control group, or counters that went backward) in a minute when that slice count grew, and whose own counters show no kill, is counted as having lost one process. It is a lower bound, and a kill of another project in the same minute can be taken for this project's if this one restarted for another reason too.
- **Disk** against a quota in force is block-accurate: the kernel answers `statfs` on a directory with the project-inherit flag with the quota's limit and what is left of it. Supavise trusts that only when the size it reports is the limit the daemon set, and sums the files otherwise. Without a quota (a volume that is not XFS with `prjquota`) the size is the volume's, which the projects share, and no quota warning is raised.
- **Block I/O** is Postgres's own: the reads and writes it asked the operating system for, from `pg_stat_io`, and the WAL it wrote (bytes only), from `pg_stat_wal`. The page cache sits between those and the device, so they are an upper bound on the device's I/O. Reading the cgroup's `io.stat` instead needs `IOAccounting=yes` in the unit templates, which Supavise does not set.
- A probe that fails (the cluster is starting) loses the SQL values of that minute and keeps the rest.

## What Studio's attributes read

`attributes.go` lists every value of the platform spec's `attribute` enum, and a test compares the list with the vendored spec. An attribute Studio asks for always gets a series entry, because Studio draws nothing for a chart whose series metadata is missing.

| Attribute | Value |
|---|---|
| `cpu_usage`, `avg_cpu_usage`, `max_cpu_usage` | busy share of the quota: mean and peak of the minute |
| `cpu_usage_busy_user`, `_system`, `_idle` | user, kernel, and what is left |
| `cpu_usage_busy_iowait`, `_irqs`, `_other` | zero: a control group does not tell them apart |
| `ram_usage_used`, `_cache_and_buffers`, `_free`, `_total`, `_available`, `_swap`, `swap_usage`, `ram_usage` (percent) | memory as above against `MemoryMax` |
| `pg_stat_database_num_backends`, `client_connections_<role>` (`postgres`, `authenticator`, `supabase_admin`, `supabase_auth_admin`, `supabase_storage_admin`, `other`), `supavisor_connections_active`, `max_db_connections` | the SQL session |
| `pg_database_size`, `disk_fs_used_wal`, `disk_fs_used_system` (what the project's directory holds besides databases and WAL), `disk_fs_used`, `disk_fs_avail`, `disk_fs_size` | database size, WAL, and the disk figures |
| `disk_iops_read`, `disk_iops_write`, `disk_bytes_read`, `disk_bytes_written` | block I/O as above |
| `physical_replication_lag_...` | a replica's lag from the replica controller; a primary has no points |
| `network_*`, `disk_io_budget`, `disk_io_consumption`, `disk_io_usage`, `ram_commit_*`, `client_connections_pgbouncer`, `pgbouncer_pools_*`, `realtime_*` | no source: zero on every point, and an `errors` entry that says why, which Studio's cards print in place of a value |

The route refuses a name that is not in the catalog with `400`, as hosted refuses a name outside its list (the catalog is the 57 names of the vendored `platform.json`, and a test checks that they agree). That also keeps a request from making the answer as large as its query string allows. `Assemble` itself still gives a name it does not know a series entry, zeros and a message that names it, so a caller that skips the check cannot blank a chart. The lag of a replica that has not reported yet has no points and no error, as on a primary.

## The store

Samples live in `<state_dir>/system/metrics/<ref>.ring`, one file per project, with a fixed size of about 1 MiB (`fileSize` in `store.go`): a ring of one-minute samples for the last 3 days and a ring of hourly rollups (the mean and the peak of each field) for the last 92 days. The first covers every chart Studio draws at one-minute resolution (ranges up to 12 hours), the second the ranges it draws by the hour (up to 7 days) and by the day (the presets go to 28 days, and the date range limit of the plan Supavise reports is 90). A write replaces the oldest record, so nothing grows and nothing needs pruning, and the files of a deleted project are removed once a day.

For 20 to 35 projects that is 20 to 37 MiB on disk, no memory beyond the last 30 samples of each project (29 values of 8 bytes each, under 10 KiB a project), and per project per minute one file open, one write of 124 bytes, one read of 7 KiB and one write of 248 bytes. The numbers are why it is not a table in the registry: a registry table of the same history would be about 6,500 rows a project, 130,000 to 230,000 rows for the node, written by the system cluster that every project and the standby depend on, replicated to the follower and archived with its WAL, for data that is only worth reading on the node that took it. Restarting the daemon loses nothing but the rates of the first minute. The history stays on the node that sampled it, so a project that moves to another node starts a new history there, and a follower that becomes the leader has the history of what it sampled.

A file written by another layout (`NumFields` changed) is started again. Add a field only at the end of the list in `sample.go`, and only when a value cannot be derived from the ones there.

## Other nodes

The Management API runs on the leader, and a project's control groups can be read only on its home. A node that is not the leader adds the newest sample of each project homed on it to the report it sends every 10 seconds (`peerapi.Report.Metrics`, `internal/app/wire_metrics.go`), and the leader stores what it is told with `Service.Ingest` as if it had sampled the project itself. The leader accepts a sample only from the project's home node. A report arrives 10 seconds apart and a sample is a minute, so the leader sees each sample within about 10 seconds of the follower taking it and ignores the repeats. The history of a follower-homed project is therefore on the follower (what it sampled) and on the leader (what it was told).

A read replica has no samples: a request for its metrics answers its lag and an error entry for every other attribute.

## Resource warnings and alerts

`Service.Warnings(ref)` judges a project by its recent samples, and `Service.Pressure()` returns the alert conditions. The thresholds are constants in `pressure.go`:

| What | Warning | Critical |
|---|---|---|
| `disk_space_exhaustion` and the `project_disk_high` alert (against a quota in force) | 80% of the quota | 90% |
| `cpu_exhaustion` (mean busy over the last 5 samples) | 80% | 90% |
| `memory_and_swap_exhaustion` (mean of memory used over the limit, last 5 samples) | 85% | 95%, or a process of the project killed for lack of memory in the last 15 minutes |
| `memory_low` alert (node) | `MemAvailable` under `[health] memory_low_percent` of the node's memory (default 10%), or some task waited for memory for 25% of the last minute (PSI `some avg60`) while under 3 times that percentage is available | under half of that percentage, or all tasks waited for 10% of the minute (PSI `full avg60`) while under 2 times it is available |

`is_readonly_mode_enabled` is always false, `need_pitr`, `disk_io_exhaustion` and the `auth_*` fields are null. Supavise has no read-only mode: a project past its quota fails its writes with `No space left on device`, for that project alone.

An alert starts at its line and ends only when the value is well back across it: the disk alerts end 5 points below the line that raised them (75% and 85%), the memory alert at 1.5 times the line (15% available) and at half the PSI value. A warning and the critical state of the same matter have keys of their own, so the critical one is told even when the warning was sent minutes ago. A condition that is not reported for 10 minutes (the project was paused, its node stopped reporting) ends. A minute in which a project's disk could not be read keeps the alert on the last reading, up to those 10 minutes. Only the projects this node samples itself raise `project_disk_high` here and count in the memory limits that `memory_low` names: a project of another server is judged by that server, whose alert state and `[alerts]` destinations are its own, and Studio's warnings for it still come from the samples it reports. In the first 10 minutes after the daemon starts, `Service.Unjudged(key)` is true for a key that was not measured yet (the node's memory, or a project's disk whose Postgres is not up), and the alert checker leaves an open alert for it alone: a host that reboots while a project is at 85% of its quota does not tell the alert as over and raise it again. The checker's debounce (`[alerts] unhealthy_after_seconds`) applies as to every condition.

The stall times of `/proc/pressure/memory` are the whole node's and include a project that reclaims within its own memory limit, so they count only together with little available memory. The node can run out of memory while every project is under its own limit, because `[compute] overcommit` lets the limits add up to more than the memory. `memory_low` is the check that sees it, from `/proc/meminfo` and `/proc/pressure/memory`.

## Tests

`go test ./internal/metrics/` runs against a fake cgroup tree and a fake procfs (`Cgroup.Root`, `Proc.Root`), so it runs anywhere, with a fake probe and fake clock. It covers the parsers, the rates and their edge cases, the store (wrap-around, rollups, layout change, sweeping), the catalog against the vendored spec, the warnings, and the alert hysteresis. `TestProbeStatementsRunOnPostgres` runs the probe's SQL on the CI Postgres (`SUPAVISE_TEST_DATABASE_URL`). That the real kernel's files and the quota's `statfs` look as the fakes do is checked by `tests/linux/compute-smoke.sh` in the `compute-smoke` job.

## Limits

- The control groups must be the unified (v2) hierarchy under `/sys/fs/cgroup`, as on Ubuntu 24.04 and Debian 12; on a host without it the daemon logs that `supavise.slice` has no control group and the charts stay empty. Control groups, the XFS quota and `/proc/pressure` exist only on Linux under systemd. With the exec supervisor (development and tests) there is no collector, and the routes answer series with no points.
- A project that is paused or stopped has no samples; its charts have gaps.
- `oom_kills` cannot tell why a unit restarted, so a project that restarts in the minute another project loses a process may be counted as having lost one too (see "Rates" above). Setting `OOMPolicy=continue` on the project units would keep each unit's own counter, and would also let Postgres recover from the death of a backend by itself instead of being stopped; the units do not set it.
- A follower's own projects alert on the follower. Set `[alerts]` destinations on every server of a cluster.
- Network traffic, an EBS-style I/O budget, memory commitment, PgBouncer and Realtime have no source (see the table above).
- Disk I/O is Postgres's own, not the device's, and counts WAL in bytes only.
- The warnings' thresholds are fixed. Hosted does not publish its own.

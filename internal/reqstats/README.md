# internal/reqstats

An in-memory count of the requests that reach each project's API, per service and per time bucket. The edge proxy adds the counts (`internal/proxy`, `Options.Stats`); the Management API reads them to answer the project home's request chart (`usage.api-counts`, `usage.api-requests-count`; `internal/api/analytics.go`).

## What is counted

One count for every request that matches a route of a project's API, added when the route is known and before the request is served: `rest` (`/rest/v1`), `graphql` (`/graphql/v1`), `auth` (`/auth/v1` and the OAuth discovery document), `storage`, `realtime` and `functions`. A request that a key check turns away (401, 403) is a request and counts. A CORS preflight, a path that matches no route, a request to a replica host for a path other than REST and GraphQL, and the requests to the API and Studio hosts do not count. A WebSocket counts when it connects, not when it closes.

The dashboard charts four of the six (`total_rest_requests` counts `/rest/v1` only; GraphQL is counted separately and not charted). The others are kept so that a later answer, such as the total of `usage.api-requests-count`, does not need a second counter.

## How much it keeps

Each project has two rings of 32-byte slots, one per bucket, that hold one `uint32` per service:

| Ring | Slots | Covers | Answers |
|---|---|---|---|
| minutes | 181 | 3 hours and the minute in progress | `usage.api-counts` with `15min`, `30min`, `1hr`, `3hr` |
| hours | 200 | 8 days and 8 hours | `1day`, `3day` (hour buckets) and `7day` (UTC day buckets, summed from hours), and the 24-hour total |

That is 12 KB per project, so 35 projects take about 430 KB. The hour ring is longer than 7 days (168 hours) because the first UTC day of a seven-day range begins up to 24 hours before the range does. A slot is reused when its bucket is 181 minutes or 200 hours old (a `3hr` range is 181 minute buckets, and the ring holds all of them); a count with an older time than the bucket in its slot is dropped rather than overwriting newer counts. `Series` returns at most 240 buckets, so a request for a year of minutes costs the same as one for three hours.

A project that has counted nothing for eight days is dropped. `Add` checks for such projects when a project first appears, at most once an hour, so the counts of branches that were created and deleted do not pile up. `Forget` drops one project at once: the Management API calls it when it deletes a project, and the metrics collector when its tick finds a project deleted or created again under the same ref (`internal/app/serve.go`), so that a ref used again starts at zero.

## Last request and open requests

`Last(ref)` is the time of the latest request counted for a project, to the nanosecond (`ok` false when none was counted since the process started), and `Active(ref)` the number being served now. `Touch(ref, t)` moves `Last` without counting a request: the Management API calls it for a call that ran SQL on the project, which is use the idle pause should see and not traffic for the request charts. The proxy brackets each forward with `Begin(ref)` and the function it returns. `Last` and `Active` are what the idle pause of branches reads (`internal/branching/README.md`, "Idle pause"): a branch is not idle while a WebSocket is open or a request arrived in the period. A project with an open request is not pruned. Both are in memory like the counts, so a restart of the daemon forgets them, and the idle pause starts its clocks again at the restart.

## Concurrency

`Add` runs on the proxy's request path. Projects live in a `sync.Map`, so finding one takes no lock, and each project has its own mutex around a few integer operations; requests to different projects never wait for each other. `go test -bench AddParallel` measures about 27 ns per `Add` with four cores adding to 35 projects. `Series` holds one project's mutex while it copies a few hundred slots.

## Limits

- Counts live in memory: they start at zero when `supavise serve` starts, and an upgrade or a restart empties the charts.
- A node counts the requests its own proxy served. On a cluster the Management API runs on the leader, so the chart leaves out what a follower's proxy served. Forwarding the counts over the mesh is not done.
- Buckets are whole minutes, hours and UTC days, so the first bucket of a range counts from its start, not from the start of the range.
- There is no status class: the charts show a request, not whether it failed.

## Tests

`go test -race ./internal/reqstats` covers the buckets, the gap filling, UTC days, ring reuse, the bounds, pruning and 16 goroutines adding to shared and separate projects (the total is exact). `internal/proxy/stats_test.go` checks what the proxy counts.

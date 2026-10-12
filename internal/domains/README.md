# internal/domains

Per-project custom hostnames and vanity subdomains, the way hosted Supabase's Custom Domains work
(`supabase.com/docs/guides/platform/custom-domains`, and `/v1/projects/{ref}/custom-hostname` and
`/vanity-subdomain` of the Management API). The user guide is the deploy README's "Custom domains and
vanity subdomains"; this file is for people changing the code.

```go
svc := domains.New(domains.Options{Reg: reg, Config: cfg}) // Resolver defaults to net.DefaultResolver
st, err := svc.Initialize(ctx, ref, "api.acme.com")  // claim; returns the TXT token and CNAME target
st, err = svc.Reverify(ctx, ref)                      // DNS from this node; rate limited
st, changed, err := svc.Activate(ctx, ref)            // route + status 5; idempotent
wasActive, err := svc.Delete(ctx, ref)
```

The Management API (`internal/api/domains.go`) calls the service and then restarts the project's GoTrue
(`lifecycle.Reconfigurer.ApplyConfig`, Auth), which renders its external URL from the registry
(`domains.ExternalHost`, used by `lifecycle.presentedAuthURL`).

## State

`internal/registry/migrations/1190_custom_domains.sql` (range 1190-1219): `custom_hostnames` (one row per project:
hostname, status, TXT token, what the last DNS check found) and `vanity_subdomains`. They sit behind
`registry.DomainStore`, implemented by the Postgres and the memory registry with one conformance suite
(`internal/registry/domains_test.go`). The store writes the `routes` row itself, in the same transaction as the status
change, because routes are what the proxy serves and what its certificate gate allows: an activated hostname has a
`custom` route, a vanity subdomain a `vanity` route, a claim has none.

## Status values

The ones of hosted's API, which Studio's page switches on:

| Status | Meaning here |
|---|---|
| `2_initiated` | claimed; the TXT record is not found (`1_not_started` is never stored) |
| `3_challenge_verified` | the TXT record is there, the hostname does not reach this node |
| `4_origin_setup_completed` | both are there; ready to activate |
| `5_services_reconfigured` | active: routed, certificate requested, Auth presents it |

`Reverify` recomputes the status from DNS every time (a hostname whose DNS moved away goes back), except for an active
one, which it reports without a lookup. `Activate` does the same check first and refuses a claim that no longer reaches 4.

## DNS checks

From the node's own resolver (`Options.Resolver`; tests pass a fake): the TXT record `_supavise-challenge.<hostname>` must
contain the claim's token (`supavise-verify=` and 48 hex digits, one per claim), and the hostname must be a CNAME of the
project's host `<ref>.api.<domain>` or resolve to an address the node answers on (the addresses of `api.<domain>` and of
the project's host, and `public_ip`). Studio's own pre-check of the CNAME, which asks Cloudflare's DNS-over-HTTPS from the
Studio process, is answered by the proxy from the same resolver (`internal/proxy/studio_cname.go`).

## Abuse limits

- A hostname is held by one project: a claim in status 4 or 5 holds it (a partial unique index in the registry, enforced
  again when a claim reaches 4), and `Initialize` refuses a hostname another project holds. Pending claims do not hold it, so
  nobody can lock a name they do not control; none can pass the TXT check without the DNS.
- A verified claim holds the name only while its proof is fresh: `Options.ClaimTTL` (24 hours) after the last successful DNS
  check, another project's `Initialize` or `Reverify` releases it (`DomainStore.ReleaseStaleClaims`). Each successful
  verification renews the hold, and `Activate` checks the DNS again before it routes anything.
- Certificate orders: `Activate` of a custom hostname and a change of the vanity name take from a per-project sliding window
  (`Options.IssuancePerProject` in `IssuanceProjectWindow`: 5 in 24 hours) and from a node-wide one
  (`IssuancePerNode` in `IssuanceNodeWindow`: 25 in 7 days, only for names that need a certificate of their own, so not for
  vanity names under a DNS-01 wildcard). A refusal is a 429 with `Retry-After` and records nothing. Activating the current
  vanity name again is free. The counters live in memory.
- Verification attempts: five per project in a burst, one more every 15 seconds, and fifty for the node in a burst with one
  more every 1.5 seconds (`Options.Attempts`, `Options.Refill`). A refused attempt makes no DNS query.
- Names: `ValidateHostname` refuses IP addresses, wildcards, single labels, bad labels, reserved zones and the node's own
  hosts; `ValidateVanityName` refuses reserved names, ref-shaped names, and any name that contains `-rr-` or ends in `-lb`
  (the shape of a replica's host `<ref>-rr-<region>-<id6>` and a project's load balancer host `<ref>-lb`, design 2.7.1); a
  reserved name is "not available" in `CheckVanity`. The proxy applies `RoutableVanityName` to a route row: the same checks
  without the `-rr-` and `-lb` rule, so a row made before that rule keeps serving, and a vanity route cannot shadow a
  project even if written directly (replica and balancer hosts are resolved before routes).
- A custom domain and a vanity subdomain are mutually exclusive, as on hosted.
- `tls.mode = "dns01"` nodes refuse custom hostnames (no per-host certificates there).

## Branches

A branch is a project, so every call here takes a branch ref and nothing rejects one (only the system project is
refused). What follows is what that means, each point with a test (`branch_test.go`, and in the neighbours
`internal/api/domains_branch_test.go`, `internal/lifecycle/domains_branch_test.go`,
`internal/proxy/lifecycle_test.go` and, against a CA, `internal/proxy/pebble_branch_test.go`):

- **Its own hostname.** The CNAME target (`State.Target`) is the branch's `<ref>.api.<domain>`, and GoTrue of the
  branch presents the branch's hostname or vanity name (`ExternalHost`). The parent's hostname is not the branch's and
  the branch's is not the parent's; a hostname held by one cannot be claimed by the other.
- **Nothing is copied.** `with_data` clones the database, which holds no hostname, and the domain rows are keyed by ref
  (`custom_hostnames`, `vanity_subdomains`, the routes): a new branch starts with none, from any parent.
- **Reset keeps it.** A reset removes the branch's cluster and builds it again over the same row (`DeleteOptions.KeepRecord`),
  and the rows and routes stay, so the hostname and its certificate come back unchanged. While the cluster is rebuilt
  the project is `GOING_DOWN`, `INIT_FAILED` and `COMING_UP`, and the proxy answers every host of it (derived, custom,
  vanity) with 503 and `Retry-After` instead of dialing a dead upstream; a handshake for the hostname still completes
  with the cached certificate.
- **Delete frees it.** Deleting the branch, by hand or when its lifetime lapses, deletes the rows with the project (`on delete
  cascade`); the proxy sees the route go, forgets the certificate (`forgetCertificates`) and refuses a new one
  (`allowHost`), and the name is free for another project. The branch service warns first when the branch has a
  lifetime and an active hostname or vanity name (`branch.expiring`, `internal/branching/README.md`).
- **Idle pause keeps it.** A branch paused for idleness keeps its rows; the handshake for its host is allowed
  (`allowHost` lets a sleeping branch through) and the first request wakes the branch.
- **One budget.** The node's 25 certificate orders in 7 days are shared by every project and every branch (the
  per-project window of 5 in 24 hours is per ref, so a branch has its own); a vanity name under a DNS-01 or `auto` wildcard
  takes none, which makes it the cheap route for many preview branches.
- **Vanity names** are one namespace for projects and branches, and a branch uses a custom hostname or a vanity name, not
  both, like any project.
- **Dashboard.** Settings > General > Custom domains renders for a project and for a branch (Studio says "for your
  branch") unless `tls.mode` is `dns01`, where the profile hides it (`project_settings:custom_domains`). The page
  covers custom hostnames only; vanity names are set with the CLI or the API. The DNS pre-check it calls
  (`/api/check-cname`) is answered by the proxy (`studio_cname.go`) and does not depend on the project.

## Tests

```
go test ./internal/domains/     # names, the whole flow with a fake resolver, squatting, rate limits, vanity, branches
```

The routing, the certificate gate and the Auth settings are tested in `internal/proxy` and `internal/lifecycle`; the
Pebble test (`internal/proxy/pebble-test.sh`, the `pebble` job of `linux.yml`) takes a hostname through the real flow
against an ACME test server and a DNS stub.

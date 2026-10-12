#!/usr/bin/env bash
# The Studio crawl: the real Studio against the real daemon, in a headless Chromium, with fixtures made through the
# Management API. It visits every page of the build, runs the probes of the user flows, and fails on what the lane's
# failure rule says (tests/linux/crawl/crawl.mjs): a stub answer nobody listed, a 5xx, a 4xx without a reason, a page
# or console error, a failed probe, a page without a fixture, an allow-list entry that no page hit.
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/path/to/supavise-linux-amd64 [CRAWL_OUT=/dir] [GITHUB_STEP_SUMMARY=...] \
#     tests/linux/studio-crawl.sh --studio-artifact /path/to/supavise-studio-<tag>-p<N>-linux-amd64.tar.zst [--teardown]
#
# The node is the one of oauth-smoke.sh (domain supavise.test, TLS off, the artifact served to it from a local HTTP
# server, the first user claims it and becomes the Owner of two organizations) plus Edge Functions, a resolver of
# its own (dnsmasq on 127.0.0.1, mounted over /etc/resolv.conf) that answers the DNS records of the custom-hostname
# fixtures, and a base backup of the main fixture project. The browser needs no DNS: --host-resolver-rules maps the
# node's names to the proxy.
#
# Results: $CRAWL_OUT (default $LOG_DIR/crawl): crawl-report.json, summary.md, calls.jsonl, fixtures.json and
# screenshots/. Needs node 22 and npm. Not run in development (root, systemd and Linux; it installs Chromium and a
# resolver); CI runs it on an ephemeral Ubuntu 24.04 VM (studio-crawl.yml). `node --test tests/linux/crawl/selftest.mjs`
# checks the logic of the crawl without any of that.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
STUDIO_ARTIFACT=${STUDIO_ARTIFACT:-}
while (($#)); do
  case $1 in
    --teardown) TEARDOWN=1 ;;
    --studio-artifact) STUDIO_ARTIFACT=${2:?--studio-artifact needs a path}; shift ;;
    *) fail "unknown argument $1" ;;
  esac
  shift
done
[[ -n $STUDIO_ARTIFACT ]] || fail "--studio-artifact is required: the crawl visits the pages of the Studio build under test"
[[ -s $STUDIO_ARTIFACT ]] || fail "the Studio artifact '$STUDIO_ARTIFACT' is missing or empty"

CRAWL_OUT=${CRAWL_OUT:-$LOG_DIR/crawl}
WORK=$(mktemp -d)
HTTP_PID=""
MOUNTED=0
cleanup() {
  local rc=$?
  [[ -n $HTTP_PID ]] && kill "$HTTP_PID" 2>/dev/null
  if [[ $MOUNTED -eq 1 ]]; then umount /etc/resolv.conf 2>/dev/null || true; fi
  systemctl stop crawl-dnsmasq.service 2>/dev/null || true
  # whatever the crawl wrote is kept for the artifact, also when it failed half way
  if [[ -d $WORK/out && $WORK/out != "$CRAWL_OUT" ]]; then mkdir -p "$CRAWL_OUT" && cp -a "$WORK/out/." "$CRAWL_OUT/" 2>/dev/null || true; fi
  collect_logs
  [[ $TEARDOWN -eq 1 ]] && teardown
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000 P_EDGE=19000
OWNER_EMAIL=owner@example.com
ORG_NAME=Crawl
ORG2=second
PASSWORD=crawl-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')   # for this run only; reaches the crawl through a 0600 file
PW_VERSION=1.63.0                                                         # the version studio/spike.sh pins

preflight
for c in node npm python3 curl; do command -v "$c" >/dev/null || fail "missing $c"; done
mkdir -p "$WORK/out" "$WORK/studio" "$WORK/dns.d"

# ---- the Studio artifact, served to the node from a local HTTP server ------------------------------------------------
STUDIO_SHA=$(sha256sum "$STUDIO_ARTIFACT" | cut -d' ' -f1)
SUMS=$(dirname "$STUDIO_ARTIFACT")/SHA256SUMS
if [[ -f $SUMS ]]; then grep -q "^$STUDIO_SHA " "$SUMS" || fail "SHA256SUMS next to the artifact does not list $STUDIO_SHA"; fi
cp "$STUDIO_ARTIFACT" "$WORK/studio/"
STUDIO_HTTP_PORT=$(free_port)
python3 -m http.server "$STUDIO_HTTP_PORT" --bind 127.0.0.1 --directory "$WORK/studio" >"$WORK/studio-http.log" 2>&1 &
HTTP_PID=$!

# ---- the crawler's own tools: Playwright at the pinned version, Chromium, dnsmasq -----------------------------------------
log "Playwright $PW_VERSION and Chromium"
(cd "$REPO_ROOT/tests/linux/crawl" && npm ci --no-audit --no-fund --silent) || fail "npm ci in tests/linux/crawl"
[[ $(node -p "require('$REPO_ROOT/tests/linux/crawl/node_modules/playwright-core/package.json').version") == "$PW_VERSION" ]] \
  || fail "tests/linux/crawl installed another Playwright than $PW_VERSION (studio/spike.sh pins it)"
export PLAYWRIGHT_BROWSERS_PATH="$WORK/browsers"
PW_CLI="$REPO_ROOT/tests/linux/crawl/node_modules/playwright-core/cli.js"
wait_apt_idle 300
node "$PW_CLI" install-deps chromium >"$WORK/pw-deps.log" 2>&1 || { tail -20 "$WORK/pw-deps.log" >&2; fail "playwright install-deps chromium"; }
node "$PW_CLI" install chromium >"$WORK/pw-install.log" 2>&1 || { tail -20 "$WORK/pw-install.log" >&2; fail "playwright install chromium"; }
if ! command -v dnsmasq >/dev/null; then
  export DEBIAN_FRONTEND=noninteractive
  wait_apt_idle 300
  apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends dnsmasq-base >/dev/null 2>&1 \
    || { apt-get -o DPkg::Lock::Timeout=300 update -qq && apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends dnsmasq-base >/dev/null; } \
    || fail "dnsmasq-base is not installed and could not be installed"
fi
command -v dnsmasq >/dev/null || fail "no dnsmasq after the install"

install_binary
setup_node

# ---- the node ----------------------------------------------------------------------------------------------------------
# idle auto-pause is set only for a build that knows the setting; an unknown key would be read as a typo by a later build.
# With it on, every branch the crawl makes is persistent (exempt from idle pause) except the idle fixture itself and the two
# that are paused by design: seeding and the probes leave a branch alone for minutes. See awake() in crawl/node.mjs.
IDLE_PAUSE=0
if grep -qa 'idle_pause_after' /usr/local/bin/supavise; then IDLE_PAUSE=1; fi
cat >>"$SUPAVISE_CONF" <<CONF

[ports]
supavisor_session = $P_SESSION
supavisor_transaction = $P_TRANSACTION
realtime = $P_REALTIME
storage = $P_STORAGE
storage_admin = $P_STORAGE_ADMIN
pgmeta = $P_PGMETA
studio = $P_STUDIO
edge_runtime = $P_EDGE

[fleet]
supavisor_api_port = $P_API

[functions]
enabled = true

[studio]
artifact_url = "http://127.0.0.1:$STUDIO_HTTP_PORT/$(basename "$STUDIO_ARTIFACT")"
artifact_sha256 = "$STUDIO_SHA"
CONF
if [[ $IDLE_PAUSE -eq 1 ]]; then
  cat >>"$SUPAVISE_CONF" <<'CONF'

[branching]
idle_pause_after = "2m"
CONF
fi
# Same VM-local workaround as oauth-smoke.sh and roles-smoke.sh.
install -d /etc/systemd/system/supavise-postgres@.service.d
cat >/etc/systemd/system/supavise-postgres@.service.d/10-studio-crawl.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/supavise/artifacts
CONF
systemctl daemon-reload

# ---- DNS: the records of the custom-hostname fixtures ---------------------------------------------------------------------
# The daemon checks a customer's hostname with its own resolver (net.DefaultResolver). dnsmasq answers the made-up names, the
# fixtures' records (seed.mjs writes zone.conf and restarts it), and passes everything else to the resolver the VM had.
UPSTREAM=$(awk '/^nameserver/ {print $2; exit}' /etc/resolv.conf)
[[ -n $UPSTREAM && $UPSTREAM != 127.0.0.1 ]] || UPSTREAM=127.0.0.53
cat >"$WORK/dnsmasq.conf" <<CONF
port=53
listen-address=127.0.0.1
bind-interfaces
no-resolv
no-hosts
server=$UPSTREAM
address=/$SUPAVISE_DOMAIN/127.0.0.1
conf-dir=$WORK/dns.d,*.conf
CONF
: >"$WORK/dns.d/zone.conf"
printf 'nameserver 127.0.0.1\noptions timeout:2 attempts:2\n' >"$WORK/resolv.conf"
systemd-run --quiet --unit=crawl-dnsmasq --property=Restart=on-failure dnsmasq --keep-in-foreground --conf-file="$WORK/dnsmasq.conf" \
  || fail "starting dnsmasq"
python3 - "$WORK/dnsmasq.conf" <<'PY' || fail "dnsmasq does not answer"
import socket, sys, time
# a DNS question for the node's name straight to dnsmasq: the A record 127.0.0.1
q = b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00" + b"".join(bytes([len(p)]) + p.encode() for p in "api.supavise.test".split(".")) + b"\x00\x00\x01\x00\x01"
for _ in range(20):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(1)
    try:
        s.sendto(q, ("127.0.0.1", 53)); a = s.recv(512)
        if a[-4:] == bytes([127, 0, 0, 1]): sys.exit(0)
    except OSError:
        time.sleep(0.5)
sys.exit(1)
PY
mount --bind "$WORK/resolv.conf" /etc/resolv.conf || fail "mounting the resolver over /etc/resolv.conf"
MOUNTED=1

# ---- start the node ---------------------------------------------------------------------------------------------------------
log "system init, fleet, daemon"
system_up
supavise fleet start || fail "fleet start"
PSQL=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
reg() { sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/system/postgres/sock port=5433 user=supabase_admin dbname=supavise" -Atc "$1" </dev/null; }
reg "insert into supavise.organizations (slug, name) values ('$ORG2', 'Second')" >/dev/null || fail "making organization $ORG2"
start_daemon

log "Studio from the build under test"
wait_active supavise-studio.service 120
for ((i = 0; i < 120; i++)); do
  [[ $(http_code -H "Host: studio.$SUPAVISE_DOMAIN" http://127.0.0.1/api/get-utc-time) == 200 ]] && break
  sleep 1
done
[[ $(http_code -H "Host: studio.$SUPAVISE_DOMAIN" http://127.0.0.1/api/get-utc-time) == 200 ]] || { journalctl --no-pager -u supavise-studio.service | tail -40 >&2; fail "Studio does not answer through studio.$SUPAVISE_DOMAIN"; }
wait_active supavise-edge-runtime.service 120

# ---- the first user: Owner of both organizations, with a personal access token -------------------------------------------------
log "claim, Owner, personal access token"
CLAIM=$(supavise claim token | tr -d '[:space:]')
[[ $CLAIM =~ ^sbc_[0-9a-f]{48}$ ]] || fail "claim token shape"
[[ $(api POST /claim -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' --data-binary @- \
  <<<"{\"token\":\"$CLAIM\",\"email\":\"$OWNER_EMAIL\",\"password\":\"$PASSWORD\",\"organization_name\":\"$ORG_NAME\"}") == 201 ]] || fail "claim"
unset CLAIM
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' --data-binary @- \
  <<<"{\"email\":\"$OWNER_EMAIL\",\"password\":\"$PASSWORD\"}" | json_get 'd["access_token"]') || fail "owner sign-in"
OWNER_PAT=$(api POST /platform/profile/access-tokens -H @<(printf 'Authorization: Bearer %s\n' "$JWT") -H 'Content-Type: application/json' -d '{"name":"studio-crawl"}' | json_get 'd["token"]') || fail "owner PAT"
[[ $OWNER_PAT =~ ^sbp_[0-9a-f]{40}$ ]] || fail "owner PAT shape"
ORG=$(api GET /v1/organizations -H @<(printf 'Authorization: Bearer %s\n' "$OWNER_PAT") | json_get '[o["slug"] for o in d if o["slug"] != "'"$ORG2"'"][0]') || fail "listing organizations"
unset JWT
supavise users role "$OWNER_EMAIL" owner --org "$ORG2" >/dev/null || fail "making the Owner of $ORG2"

# ---- the crawl's configuration (0600) and the fixtures ----------------------------------------------------------------------------
MANIFEST=$(find "$SUPAVISE_STATE/artifacts/studio" -name routes-manifest.json -path '*apps/studio/.next*' 2>/dev/null | head -1)
[[ -s $MANIFEST ]] || fail "no .next/routes-manifest.json under $SUPAVISE_STATE/artifacts/studio: the crawl takes its list of pages from the build"
(
  umask 077
  CR_PASSWORD=$PASSWORD CR_PAT=$OWNER_PAT CR_OUT=$WORK/out CR_ORG=$ORG CR_ORG2=$ORG2 CR_EMAIL=$OWNER_EMAIL CR_MANIFEST=$MANIFEST \
  CR_DOMAIN=$SUPAVISE_DOMAIN CR_DNS=$WORK/dns.d CR_IDLE=$IDLE_PAUSE CR_PATH=$WORK/crawl-config.json \
  python3 - <<'PY'
import json, os
e = os.environ
cfg = {
    "domain": e["CR_DOMAIN"], "proxy": {"host": "127.0.0.1", "port": 80},
    "owner": {"email": e["CR_EMAIL"], "password": e["CR_PASSWORD"]}, "owner_pat": e["CR_PAT"],
    "org": e["CR_ORG"], "org2": e["CR_ORG2"], "out": e["CR_OUT"], "routes_manifest": e["CR_MANIFEST"],
    "dns_dir": e["CR_DNS"], "dns_unit": "crawl-dnsmasq", "idle_pause": e["CR_IDLE"] == "1", "functions": True,
}
fd = os.open(e["CR_PATH"], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(cfg, f)
PY
)
unset PASSWORD OWNER_PAT

log "fixtures"
node "$REPO_ROOT/tests/linux/crawl/seed.mjs" "$WORK/crawl-config.json" || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "seeding the fixtures failed"; }
FIX_A=$(json_get 'd["projects"]["A"]' <"$WORK/out/fixtures.json")
log "a base backup of the main fixture project, for the restore probe"
supavise backups create "$FIX_A" || fail "supavise backups create $FIX_A"

# ---- the crawl ----------------------------------------------------------------------------------------------------------------------------
log "the crawl"
set +e
node "$REPO_ROOT/tests/linux/crawl/crawl.mjs" "$WORK/crawl-config.json"
RC=$?
set -e
if [[ $RC -ne 0 ]]; then
  journalctl --no-pager -u supavise.service | tail -60 >&2
  fail "the crawl failed (exit $RC); crawl-report.json and summary.md have the details"
fi
systemctl is-active --quiet supavise.service || fail "supavise.service is not active after the crawl"
systemctl is-active --quiet supavise-studio.service || fail "Studio is not active after the crawl"
log "studio crawl passed"

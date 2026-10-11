#!/usr/bin/env bash
# Studio's AI Assistant with a provider the operator brings, under real systemd units and the Studio build
# under test (the `ai-e2e` job of studio.yml, which downloads the artifact its `build` job made):
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/path/to/supavise-linux-amd64 \
#     tests/linux/ai-smoke.sh --studio-artifact /path/to/supavise-studio-<tag>-p<N>-linux-amd64.tar.zst
#
# A fake OpenAI-compatible provider listens on loopback (tests/linux/ai/fake-openai.py). The script
# configures Supavise with its URL and a dummy key (`supavise studio ai set`), then calls Studio's own
# routes through the edge (studio.<domain>) with a dashboard session, as the browser does:
#
#   1. Before any provider: the key check says no key, the Assistant refuses, `studio ai show` says unset.
#   2. After `studio ai set`: the key check says there is one and names the model; the Assistant's route
#      (generate-v4) streams the provider's answer; the request reached the fake on /v1/chat/completions
#      with the dummy key and the configured model, and /v1/responses was never used.
#   3. The Assistant tells the provider about the database only as far as the organization's opt-in
#      allows: with no opt-in the context says it has no schema access; after PATCH opt_in_tags by the
#      Owner the schema names are in the request, and the tags are still there on the next read.
#   4. A small call (title-v2) uses the completion model and structured output.
#   5. Nothing went elsewhere: api.openai.com is pointed at a trap on loopback and nothing connected to
#      it, and Studio's log names no failed connection.
#   6. The key is in Studio's environment file (mode 0600) and nowhere else: not in config.toml, not in the
#      drop-in, not in any journal, not in `studio ai show`.
#   7. `studio ai clear` takes the provider away again and the key check, the route and the environment file
#      say so.
#
# Not run in development (root, systemd, Linux and the Studio artifact are required); CI runs it on an
# ephemeral Ubuntu 24.04 VM.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

STUDIO_ARTIFACT=${STUDIO_ARTIFACT:-}
while (($#)); do
  case $1 in
    --studio-artifact) STUDIO_ARTIFACT=${2:?--studio-artifact needs a path}; shift ;;
    *) fail "unknown argument $1" ;;
  esac
  shift
done
[[ -n $STUDIO_ARTIFACT && -s $STUDIO_ARTIFACT ]] || fail "--studio-artifact must name the Studio build under test (a non-empty file)"

WORK=$(mktemp -d)
HTTP_PID="" FAKE_PID="" TRAP_PID=""
cleanup() {
  local rc=$?
  for p in $HTTP_PID $FAKE_PID $TRAP_PID; do kill "$p" 2>/dev/null || true; done
  sed -i '/# ai-smoke$/d' /etc/hosts 2>/dev/null || true
  mkdir -p "$LOG_DIR"
  cp "$WORK"/fake-openai.log "$WORK"/trap.log "$LOG_DIR/" 2>/dev/null || true   # request facts only, never a key
  collect_logs
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

# Ports away from anything the runner may listen on, as in oauth-smoke.sh.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000
OWNER_EMAIL=owner@example.com
# Values for this run only. The dummy key reaches the node through standard input, never a command line.
PASSWORD=ai-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
KEY=sk-dummy-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
FAKE_LOG=$WORK/fake-openai.log
TRAP_LOG=$WORK/trap.log
STUDIO_HOST=studio.$SUPAVISE_DOMAIN

command -v python3 >/dev/null || fail "python3 is required (the fake provider and the checks)"
STUDIO_SHA=$(sha256sum "$STUDIO_ARTIFACT" | cut -d' ' -f1)
SUMS=$(dirname "$STUDIO_ARTIFACT")/SHA256SUMS
if [[ -f $SUMS ]]; then
  grep -q "^$STUDIO_SHA " "$SUMS" || fail "SHA256SUMS next to the artifact does not list $STUDIO_SHA"
fi
mkdir -p "$WORK/studio"
cp "$STUDIO_ARTIFACT" "$WORK/studio/"
STUDIO_HTTP_PORT=$(free_port)
python3 -m http.server "$STUDIO_HTTP_PORT" --bind 127.0.0.1 --directory "$WORK/studio" >"$WORK/studio-http.log" 2>&1 &
HTTP_PID=$!

log "the fake provider on loopback, and a trap where api.openai.com would lead"
FAKE_PORT=$(free_port)
FAKE_EXPECT_KEY=$KEY python3 "$REPO_ROOT/tests/linux/ai/fake-openai.py" serve "$FAKE_PORT" "$FAKE_LOG" >"$WORK/fake-openai.out" 2>&1 &
FAKE_PID=$!
# Studio resolves api.openai.com to 127.0.0.2. If it ignored the base URL, the connection would end in the trap
# (when the port is free) or be refused, and either shows below. The same file lets Studio reach the node's own
# edge by name: its server calls api.<domain> to check the session and to read the organization.
{
  echo "127.0.0.2 api.openai.com # ai-smoke"
  echo "127.0.0.1 api.$SUPAVISE_DOMAIN $STUDIO_HOST # ai-smoke"
} >>/etc/hosts
: >"$TRAP_LOG"
python3 "$REPO_ROOT/tests/linux/ai/fake-openai.py" trap 127.0.0.2 443 "$TRAP_LOG" >"$WORK/trap.out" 2>&1 &
TRAP_PID=$!
sleep 1
kill -0 "$FAKE_PID" 2>/dev/null || { cat "$WORK/fake-openai.out" >&2; fail "the fake provider did not start"; }
if ! kill -0 "$TRAP_PID" 2>/dev/null; then
  log "the trap on 127.0.0.2:443 could not start ($(head -c 200 "$WORK/trap.out")); only Studio's log is checked for a connection to api.openai.com"
  TRAP_PID=""
fi

preflight
install_binary
setup_node
cat >>"$SUPAVISE_CONF" <<CONF

[ports]
supavisor_session = $P_SESSION
supavisor_transaction = $P_TRANSACTION
realtime = $P_REALTIME
storage = $P_STORAGE
storage_admin = $P_STORAGE_ADMIN
pgmeta = $P_PGMETA
studio = $P_STUDIO

[fleet]
supavisor_api_port = $P_API

[studio]
artifact_url = "http://127.0.0.1:$STUDIO_HTTP_PORT/$(basename "$STUDIO_ARTIFACT")"
artifact_sha256 = "$STUDIO_SHA"
CONF
# Same VM-local workaround as roles-smoke.sh.
install -d /etc/systemd/system/supavise-postgres@.service.d
cat >/etc/systemd/system/supavise-postgres@.service.d/10-ai-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/supavise/artifacts
CONF
systemctl daemon-reload

log "system init, fleet, a project"
system_up
supavise fleet start || fail "fleet start"
REF=$(create_project aismoke micro)
[[ $REF =~ ^[a-z]{20}$ ]] || fail "bad ref '$REF'"
supavise fleet ensure-tenant "$REF" || fail "ensure-tenant $REF"
log "daemon: supavise.service"
start_daemon

studio_up() {
  wait_active supavise-studio.service 120
  local i
  for ((i = 0; i < 120; i++)); do
    [[ $(http_code -H "Host: $STUDIO_HOST" http://127.0.0.1/api/get-utc-time) == 200 ]] && return 0
    sleep 1
  done
  journalctl --no-pager -u supavise-studio.service | tail -40 >&2
  fail "Studio does not answer through $STUDIO_HOST"
}
studio_up

log "the first user is the Owner, with a dashboard session"
CLAIM=$(supavise claim token | tr -d '[:space:]')
[[ $CLAIM =~ ^sbc_[0-9a-f]{48}$ ]] || fail "claim token shape"
[[ $(api POST /claim -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' --data-binary @- \
  <<<"{\"token\":\"$CLAIM\",\"email\":\"$OWNER_EMAIL\",\"password\":\"$PASSWORD\",\"organization_name\":\"Default\"}") == 201 ]] || fail "claim"
unset CLAIM
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' --data-binary @- \
  <<<"{\"email\":\"$OWNER_EMAIL\",\"password\":\"$PASSWORD\"}" | json_get 'd["access_token"]') || fail "owner sign-in"
ORG=$(japi GET /platform/organizations | json_get 'd[0]["slug"]') || fail "listing organizations"
CONNSTR=$(japi GET "/platform/projects/$REF" | json_get 'd["connectionString"]') || fail "reading the project"

# ---- helpers: Studio's own routes through the edge, with the session ---------------------------------------
studio_api() { # METHOD PATH [curl args]: a Studio /api route as the browser calls it
  local m=$1 p=$2; shift 2
  curl -sS -m 120 -X "$m" -H "Host: $STUDIO_HOST" -H @<(printf 'Authorization: Bearer %s\n' "$JWT") "$@" "http://127.0.0.1$p"
}
check_key() { studio_api GET /api/ai/sql/check-api-key; }
assistant() { # prints the answer of the Assistant's route; $1 is the message
  local body
  body=$(python3 - "$1" "$REF" "$CONNSTR" "$ORG" <<'PY'
import json, sys
text, ref, conn, org = sys.argv[1:]
print(json.dumps({"messages": [{"id": "m1", "role": "user", "parts": [{"type": "text", "text": text}]}],
                  "projectRef": ref, "connectionString": conn, "orgSlug": org, "chatId": "c1"}))
PY
  )
  studio_api POST /api/ai/sql/generate-v4 -H 'Content-Type: application/json' -w '\nHTTP %{http_code}\n' --data-binary @- <<<"$body"
}
# fake EXPR: evaluates a Python expression over L, the list of requests the fake provider has seen.
fake() {
  python3 -c '
import json, sys
try:
    L = [json.loads(x) for x in open(sys.argv[1])]
except FileNotFoundError:
    L = []
print(eval(sys.argv[2]))' "$FAKE_LOG" "$1"
}
chat='e["method"] == "POST" and e["path"] == "/v1/chat/completions"'

log "1. no provider: the key check says so, the Assistant refuses, show says unset"
[[ $(check_key) == '{"hasKey":false}' ]] || fail "check-api-key before any provider: $(check_key)"
[[ $(http_code -H "Host: $STUDIO_HOST" http://127.0.0.1/api/ai/sql/check-api-key) == 401 ]] || fail "check-api-key answers a caller without a session"
OUT=$(assistant "Say hello")
[[ $OUT == *"OPENAI_API_KEY not available"* && $OUT == *"HTTP 500"* ]] || fail "the Assistant without a provider: $OUT"
SHOW=$(supavise studio ai show)
[[ $SHOW == *"No AI provider is set"* ]] || fail "studio ai show with no provider: $SHOW"
[[ $(fake 'len(L)') == 0 ]] || fail "the fake provider heard from Studio before one was configured"

log "2. studio ai set: the key goes in on standard input, Studio restarts with it"
printf '%s\n' "$KEY" | supavise studio ai set --base-url "http://127.0.0.1:$FAKE_PORT/v1" --api-key-stdin \
  --model fake-model --completion-model fake-small || fail "studio ai set"
studio_up
SHOW=$(supavise studio ai show)
[[ $SHOW == *"http://127.0.0.1:$FAKE_PORT/v1"* ]] || fail "studio ai show lacks the base URL: $SHOW"
# "not applied yet" also contains "applied", so the line is matched whole.
grep -qE '^Studio: +applied$' <<<"$SHOW" || fail "studio ai show does not say that Studio runs with the settings: $SHOW"
CHECK=$(supavise studio ai check 2>&1) || fail "studio ai check: $CHECK"
[[ $CHECK == *"answered 200"* ]] || fail "studio ai check: $CHECK"
[[ $(fake 'any(e["method"] == "GET" and e["path"] == "/v1/models" and e["auth_ok"] for e in L)') == True ]] || fail "the check did not reach the fake with the key"
[[ $(check_key) == '{"hasKey":true,"model":"fake-model"}' ]] || fail "check-api-key with a provider: $(check_key)"

log "   the Assistant answers from the fake provider, on the chat API, with the key and the model"
: >"$FAKE_LOG"
OUT=$(assistant "Say hello")
[[ $OUT == *"Hello from the fake provider"* && $OUT == *"HTTP 200"* ]] || { echo "$OUT" | head -c 1500 >&2; journalctl --no-pager -u supavise-studio.service | tail -30 >&2; fail "the Assistant did not stream the fake provider's answer"; }
[[ $(fake "len([e for e in L if $chat and e['stream'] and e['auth_ok'] and e['model'] == 'fake-model'])") -ge 1 ]] || { cat "$FAKE_LOG" >&2; fail "no streamed chat completion with the key and the model reached the fake provider"; }
[[ $(fake 'len([e for e in L if e["path"] != "/v1/chat/completions"])') == 0 ]] || fail "Studio used another API of the provider: $(cat "$FAKE_LOG")"
[[ $(fake "any(e['has_store'] or e['reasoning_effort'] is not None for e in L if $chat)") == False ]] || fail "a chat request carried OpenAI-only options"

log "3. what the provider is told follows the organization's opt-in"
[[ $(fake "any(e['no_schema_access'] for e in L if $chat)") == True && $(fake "any(e['has_schema_names'] for e in L)") == False ]] \
  || fail "with no opt-in the provider must be told the Assistant has no schema access"
[[ $(japi GET "/platform/organizations/$ORG" | json_get 'd["opt_in_tags"]') == '[]' ]] || fail "a new organization has opted in"
[[ $(japi PATCH "/platform/organizations/$ORG" -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
  -d '{"opt_in_tags":["AI_SQL_GENERATOR_OPT_IN"]}') == 200 ]] || fail "the Owner could not save the AI opt-in"
[[ $(japi GET "/platform/organizations/$ORG" | json_get 'd["opt_in_tags"]') == "['AI_SQL_GENERATOR_OPT_IN']" ]] || fail "the opt-in did not stick"
: >"$FAKE_LOG"
OUT=$(assistant "List my schemas")
[[ $OUT == *"HTTP 200"* ]] || fail "the Assistant after the opt-in: $OUT"
[[ $(fake "any(e['has_schema_names'] for e in L if $chat)") == True && $(fake "any(e['no_schema_access'] for e in L)") == False ]] \
  || { cat "$FAKE_LOG" >&2; fail "after the opt-in the provider must be told the schema names"; }
[[ $(fake "any(e['tools'] > 0 for e in L if $chat)") == True ]] || fail "the Assistant sent the provider no tools after the opt-in"

log "4. a small call uses the completion model and structured output"
: >"$FAKE_LOG"
OUT=$(studio_api POST /api/ai/sql/title-v2 -H 'Content-Type: application/json' -d '{"sql":"create table todos (id int)"}')
[[ $OUT == *"Fake title"* ]] || { echo "$OUT" >&2; fail "title-v2 did not return the fake provider's title"; }
[[ $(fake "len([e for e in L if $chat and e['model'] == 'fake-small' and e['auth_ok'] and e['response_format'] in ('json_schema', 'json_object')])") -ge 1 ]] \
  || { cat "$FAKE_LOG" >&2; fail "the small call did not use the completion model with structured output"; }

log "5. nothing went elsewhere"
[[ ! -s $TRAP_LOG ]] || fail "something connected to api.openai.com: $(cat "$TRAP_LOG")"
journalctl --no-pager -o cat -u supavise-studio.service >"$WORK/studio.log"
if grep -E 'api\.openai\.com|127\.0\.0\.2' "$WORK/studio.log" >"$WORK/leaks.txt"; then
  head -5 "$WORK/leaks.txt" >&2
  fail "Studio tried api.openai.com, which is where it would go if it ignored the base URL"
fi

log "6. the key is in Studio's environment file and nowhere else"
ENVFILE=$SUPAVISE_STATE/projects/system/studio.env
[[ $(stat -c '%a %U' "$ENVFILE") == "600 $SUPAVISE_USER" ]] || fail "$ENVFILE is $(stat -c '%a %U' "$ENVFILE"), want 600 $SUPAVISE_USER"
grep -q "^OPENAI_API_KEY=\"$KEY\"$" "$ENVFILE" || fail "Studio's environment file has no key"
grep -q "^OPENAI_BASE_URL=\"http://127.0.0.1:$FAKE_PORT/v1\"$" "$ENVFILE" || fail "Studio's environment file has no base URL"
[[ $(stat -c '%a %U' /etc/supavise/studio-ai.key) == "600 $SUPAVISE_USER" ]] || fail "the key file is $(stat -c '%a %U' /etc/supavise/studio-ai.key)"
[[ $(stat -c '%a' /etc/supavise/config.d/40-studio-ai.toml) == 600 ]] || fail "the drop-in is mode $(stat -c '%a' /etc/supavise/config.d/40-studio-ai.toml)"
{
  cat /etc/supavise/config.toml /etc/supavise/config.d/*.toml
  supavise studio ai show; supavise studio ai show --json
  journalctl --no-pager -o cat -u 'supavise-*' -u supavise.service
  japi GET /platform/organizations; japi GET "/platform/projects/$REF"; japi GET /platform/profile
} >"$WORK/everywhere.txt" 2>&1
if grep -q -F "$KEY" "$WORK/everywhere.txt"; then fail "the key is in config, show output, a journal or an API answer"; fi
HOLDERS=$(find /etc/supavise -type f ! -name studio-ai.key -exec grep -l -F "$KEY" {} + 2>/dev/null || true)
[[ -z $HOLDERS ]] || fail "the key is in a file under /etc/supavise other than studio-ai.key: $HOLDERS"

log "7. studio ai clear takes the provider away"
supavise studio ai clear || fail "studio ai clear"
studio_up
[[ $(check_key) == '{"hasKey":false}' ]] || fail "check-api-key after clear: $(check_key)"
OUT=$(assistant "Say hello")
[[ $OUT == *"OPENAI_API_KEY not available"* ]] || fail "the Assistant after clear: $OUT"
if grep -q -E '^(OPENAI_|SUPAVISE_AI_)' "$ENVFILE"; then fail "Studio's environment file still carries the provider"; fi
[[ ! -e /etc/supavise/studio-ai.key ]] || fail "clear left the key file"
SHOW=$(supavise studio ai show)
[[ $SHOW == *"No AI provider is set"* ]] || fail "studio ai show after clear: $SHOW"
systemctl is-active --quiet supavise.service || fail "supavise.service is not active at the end"

log "ai smoke passed"

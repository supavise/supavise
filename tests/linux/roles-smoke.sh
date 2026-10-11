#!/usr/bin/env bash
# Members and roles under real systemd units: users with each organization role, what
# Studio's permission list says about them, what the Management API lets them do through
# personal access tokens, and the real Supabase CLI and MCP server with a Read-only user's token.
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/path/to/supavise-linux-amd64 [SUPABASE_CLI=/path/to/supabase] [REQUIRE_CLIENTS=1] \
#     tests/linux/roles-smoke.sh [--teardown]
#
# - the claimed first user is Owner; `supavise users invite --role` makes an Administrator, a
#   Developer and a Read-only member through the claim page (the link carries the token);
# - GET /platform/profile/permissions answers what each role may do in Studio's own terms;
# - with a PAT, Read-only reads (SELECT, list, types) and is refused every write (secrets,
#   migrations, settings, keys, lifecycle; SQL fails in the database), a Developer writes SQL and
#   migrations but not settings or secrets (nor a branch with data), an Administrator manages settings and members but not
#   Owners, the last Owner cannot be demoted or removed;
# - the pinned Postgres creates supabase_read_only_user, the user of the read-only connection string
#   the databases listing hands Studio;
# - Database Webhooks: a Read-only member is refused "Enable webhooks", a Developer's call creates the
#   supabase_functions schema and its role, a second call changes nothing, and a webhook that Studio
#   creates (a trigger on supabase_functions.http_request through pg-meta) answers an insert with a
#   request that a local HTTP server receives and a net._http_response row with its status; the script
#   runs as a superuser, so a Developer's own supabase_functions schema is refused, a trigger the
#   Developer adds to the migrations table does not fire, and a role of theirs is not dropped;
# - the dashboard's honesty about logs: a log query answers an error that names `supavise logs <ref>`
#   (never a bare empty result), `supavise logs` prints the project's Postgres lines and only the
#   project's lines of the shared services, and the project home's request chart counts the
#   requests the proxy served (usage.api-counts);
# - the MCP server (apply_migration, execute_sql) and, when SUPABASE_CLI is set, the Supabase CLI
#   (secrets set) list as Read-only and are refused writes; an Owner's can;
# - [mail] reaches supavise-gotrue@system: an invitation to a new address (GoTrue invite) and to an
#   existing account (sign-in link) arrive at a mail sink, and following each link signs the person
#   in on the invitation, which they accept and join with the invited role.
#
# Without SUPAVISE_BIN the script builds supavise with the go toolchain. It needs network access for the
# artifact downloads and, for the MCP server, npm. Not run in development (root, systemd and Linux
# required); CI runs it on an ephemeral Ubuntu 24.04 VM (amd64 and arm64).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
WORK=$(mktemp -d)
SMTP_PID="" HOOK_PID=""
trap 'rc=$?; [[ -n $SMTP_PID ]] && kill "$SMTP_PID" 2>/dev/null; [[ -n $HOOK_PID ]] && kill "$HOOK_PID" 2>/dev/null; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; rm -rf "$WORK"; exit $rc' EXIT

# Ports away from anything the runner may listen on, as in settings-smoke.sh.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000
P_SMTP=12526 P_HOOK=12527
PASSWORD=roles-correct-horse-battery
ORG=default   # the claim keeps the organization the first project created

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

# supavise-gotrue@system sends the dashboard's mail through this relay (a sink started below).
[mail]
smtp_host = "127.0.0.1"
smtp_port = $P_SMTP
smtp_from = "supavise@example.com"
smtp_name = "Roles smoke"
CONF

# Same VM-local workaround as settings-smoke.sh.
install -d /etc/systemd/system/supavise-postgres@.service.d
cat >/etc/systemd/system/supavise-postgres@.service.d/10-roles-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/supavise/artifacts
CONF
systemctl daemon-reload

# The mail sink (the same one settings-smoke.sh uses): messages are appended to a file.
cat >"$WORK/smtp.py" <<'PY'
import socketserver, sys
out = sys.argv[2]
class H(socketserver.StreamRequestHandler):
    def handle(self):
        w = lambda s: self.wfile.write((s + "\r\n").encode())
        w("220 sink ESMTP")
        data = False; buf = []
        for raw in self.rfile:
            line = raw.decode(errors="replace")
            if data:
                if line == ".\r\n":
                    data = False
                    open(out, "a").write("".join(buf) + "\n=====\n")
                    w("250 queued"); buf = []
                else:
                    buf.append(line)
                continue
            u = line.upper()
            if u.startswith(("EHLO", "HELO")): w("250 sink")
            elif u.startswith("DATA"): data = True; w("354 go")
            elif u.startswith("QUIT"): w("221 bye"); return
            else: w("250 ok")
class S(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
S(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
python3 "$WORK/smtp.py" "$P_SMTP" "$WORK/mails.txt" &
SMTP_PID=$!

# The webhook target: answers 200 to anything and logs each request line, so the test sees that the
# database's HTTP request arrived.
cat >"$WORK/hook.py" <<'PY'
import http.server, sys
out = sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode(errors="replace")
        open(out, "a").write(self.path + " " + body.replace("\n", " ") + "\n")
        self.send_response(200); self.send_header("Content-Length", "2"); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
python3 "$WORK/hook.py" "$P_HOOK" "$WORK/hooks.txt" &
HOOK_PID=$!

log "system init, fleet, one project"
system_up
supavise fleet start || fail "fleet start"
REF=$(create_project roles micro)
[[ $REF =~ ^[a-z]{20}$ ]] || fail "bad ref '$REF'"
supavise fleet ensure-tenant "$REF" || fail "ensure-tenant"
CFG="/v1/projects/$REF"
PGPORT=$(project_field "$REF" 'd["ports"]["Postgres"]')
PSQL=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
sql() { sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/$REF/postgres/sock port=$PGPORT user=supabase_admin dbname=postgres" -Atc "$1" </dev/null; }

log "daemon: supavise.service"
start_daemon

dash() { curl -sS -m 60 -X "$1" -H "Host: api.$SUPAVISE_DOMAIN" "${@:3}" "http://127.0.0.1$2"; }
sign_in() { dash POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' -d "{\"email\":\"$1\",\"password\":\"$PASSWORD\"}" | json_get 'd["access_token"]'; }
jwt_sub() { python3 -c 'import base64,json,sys; p=sys.argv[1].split(".")[1]; print(json.loads(base64.urlsafe_b64decode(p+"="*(-len(p)%4)))["sub"])' "$1"; }
declare -A JWT PAT UID_OF

# st TOKEN METHOD PATH [BODY]: the status of a Management API call. expect WANT TOKEN METHOD PATH [BODY].
st() {
  local tok=$1 m=$2 p=$3 b=${4:-}
  if [[ -n $b ]]; then
    curl -s -o /dev/null -w '%{http_code}' -m 300 -X "$m" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' -d "$b" "$ADMIN$p" || true
  else
    curl -s -o /dev/null -w '%{http_code}' -m 300 -X "$m" -H "Authorization: Bearer $tok" "$ADMIN$p" || true
  fi
}
body() { # TOKEN METHOD PATH [BODY]: the response body
  local tok=$1 m=$2 p=$3 b=${4:-}
  curl -sS -m 300 -X "$m" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' ${b:+-d "$b"} "$ADMIN$p"
}
expect() { # WANT TOKEN METHOD PATH [BODY]
  local want=$1 got; shift
  got=$(st "$@")
  [[ $got == "$want" ]] || { log "response: $(body "$@" 2>&1 | head -c 400)"; fail "$2 $3 answered $got, want $want"; }
}
papi() { # JWT METHOD PATH [BODY]: a /platform call with a dashboard session; the body goes to $WORK/last.json
  local j=$1 m=$2 p=$3 b=${4:-}
  curl -sS -m 120 -o "$WORK/last.json" -w '%{http_code}' -X "$m" -H "Authorization: Bearer $j" -H 'Content-Type: application/json' ${b:+-d "$b"} "$ADMIN$p" || true
}

log "the first user is the Owner"
CLAIM=$(supavise claim token | tr -d '[:space:]')
[[ $CLAIM =~ ^sbc_[0-9a-f]{48}$ ]] || fail "claim token '$CLAIM'"
[[ $(dash POST /claim -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
  -d "{\"token\":\"$CLAIM\",\"email\":\"owner@example.com\",\"password\":\"$PASSWORD\",\"organization_name\":\"Roles\"}") == 201 ]] || fail "claim"
mkauth() { # NAME: JWT, PAT and id of the signed-in user NAME
  local n=$1
  JWT[$n]=$(sign_in "$n@example.com") || fail "sign-in $n"
  UID_OF[$n]=$(jwt_sub "${JWT[$n]}")
  PAT[$n]=$(dash POST /platform/profile/access-tokens -H "Authorization: Bearer ${JWT[$n]}" -H 'Content-Type: application/json' -d "{\"name\":\"roles-$n\"}" | json_get 'd["token"]') || fail "PAT $n"
  [[ ${PAT[$n]} =~ ^sbp_[0-9a-f]{40}$ ]] || fail "PAT $n '${PAT[$n]}'"
}
mkauth owner
supavise users list >"$WORK/users.txt"
grep -q "owner@example.com.*$ORG:owner" "$WORK/users.txt" || { cat "$WORK/users.txt" >&2; fail "users list does not show the owner"; }
[[ $(body "${PAT[owner]}" GET "/v1/organizations/$ORG/members" | json_get 'd[0]["role_name"]') == Owner ]] || fail "the first user is not Owner"

log "invite an Administrator, a Developer and a Read-only member (the claim page; --no-mail keeps the link local)"
mkuser() { # NAME ROLE
  local n=$1 role=$2 link tok
  link=$(supavise users invite "$n@example.com" --role "$role" --org "$ORG" --no-mail 2>"$WORK/invite.err") || { cat "$WORK/invite.err" >&2; fail "invite $n"; }
  [[ $link == */claim#* ]] || fail "invite link for a new address is not the claim page: $link"
  tok=$(python3 -c 'import sys,urllib.parse as u; print(u.parse_qs(u.urlparse(sys.argv[1]).fragment)["token"][0])' "$link")
  [[ $tok =~ ^sbi_[0-9a-f]{48}$ ]] || fail "invite token '$tok'"
  [[ $(dash POST /claim -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
    -d "{\"token\":\"$tok\",\"email\":\"$n@example.com\",\"password\":\"$PASSWORD\"}") == 201 ]] || fail "claim page for $n"
  mkauth "$n"
}
mkuser admin administrator
mkuser dev developer
mkuser ro read-only
mkuser outsider read-only
supavise users list >"$WORK/users.txt"
for pair in "admin administrator" "dev developer" "ro read-only"; do
  set -- $pair
  grep -q "$1@example.com.*$ORG:$2" "$WORK/users.txt" || { cat "$WORK/users.txt" >&2; fail "users list: $1 is not $2"; }
done
supavise users invite dev@example.com --role developer --org "$ORG" --no-mail >"$WORK/again.out" 2>&1 && fail "inviting a member again was not refused"
grep -q "already a member" "$WORK/again.out" || { cat "$WORK/again.out" >&2; fail "inviting a member again: unexpected message"; }

log "Studio: what the permission list says (Studio's own matching rule)"
cat >"$WORK/perm.py" <<'PY'
import json, re, sys
role, slug = sys.argv[1], sys.argv[2]
perms = json.load(sys.stdin)
def rx(p): return re.compile("^" + ".*".join(re.escape(x) for x in p.split("%")) + "$")
def match(p, action, resource): return any(rx(a).match(action) for a in p["actions"]) and any(rx(r).match(resource) for r in p["resources"])
def can(action, resource):
    ps = [p for p in perms if p["organization_slug"] == slug and not p["project_refs"] and p["condition"] is None and match(p, action, resource)]
    if any(p["restrictive"] for p in ps): return False
    return any(not p["restrictive"] for p in ps)
want = {
  "owner": {("write:Update", "organizations"): True, ("write:Create", "projects"): True, ("read:Read", "service_api_keys"): True, ("tenant:Sql:Write:Insert", "*"): True},
  "admin": {("write:Update", "organizations"): False, ("write:Create", "projects"): True, ("read:Read", "service_api_keys"): True, ("tenant:Sql:Write:Insert", "*"): True, ("infra:Execute", "queue_job.walg.prepare_restore"): True, ("infra:Execute", "queue_job.restore.prepare"): True},
  "dev": {("write:Update", "organizations"): False, ("write:Create", "projects"): False, ("read:Read", "service_api_keys"): True, ("tenant:Sql:Write:Insert", "*"): True, ("write:Update", "custom_config_gotrue"): False, ("infra:Execute", "reboot"): True, ("infra:Execute", "queue_job.walg.prepare_restore"): False, ("infra:Execute", "queue_job.restore.prepare"): False},
  "ro": {("write:Update", "organizations"): False, ("write:Create", "projects"): False, ("read:Read", "service_api_keys"): False, ("tenant:Sql:Write:Insert", "*"): False, ("tenant:Sql:Query", "*"): True, ("read:Read", "organizations"): True, ("infra:Execute", "queue_jobs.projects.pause"): False},
}[role]
bad = [f"{a} on {r}: want {w}" for (a, r), w in want.items() if can(a, r) != w]
if bad: sys.exit(role + ": " + "; ".join(bad))
# Saved content: a Developer's and a Read-only member's changes are conditioned on the item's owner.
if role in ("dev", "ro") and not any(p["resources"] == ["user_content"] and "write:Update" in p["actions"] and "resource.owner_id" in json.dumps(p["condition"]) for p in perms):
    sys.exit(role + ": saved-content writes are not conditioned on the owner")
if role == "owner" and not any(p["actions"] == ["%"] and p["resources"] == ["%"] and not p["restrictive"] and p["condition"] is None for p in perms):
    sys.exit("owner has no unconditional % on %")
PY
for r in owner admin dev ro; do
  curl -sS -m 30 -H "Authorization: Bearer ${JWT[$r]}" "$ADMIN/platform/profile/permissions" | python3 "$WORK/perm.py" "$r" "$ORG" || fail "permissions of $r"
done
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[owner]}" "$ADMIN/platform/organizations" | json_get 'd[0]["is_owner"]') == True ]] || fail "organization list: owner is_owner"
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[ro]}" "$ADMIN/platform/organizations" | json_get 'd[0]["is_owner"]') == False ]] || fail "organization list: read-only is_owner"
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[ro]}" "$ADMIN/platform/organizations/$ORG/roles" | json_get '",".join(r["name"] for r in d["org_scoped_roles"])') == Owner,Administrator,Developer,Read-only ]] || fail "roles"
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[ro]}" "$ADMIN/platform/organizations/$ORG/members" | json_get 'len(d)') == 5 ]] || fail "members"

log "Read-only: reads work, every write is refused"
# The table is made through the API (as the project's postgres role, like every role's SQL), so
# that the Developer's writes below meet the privileges they will have in real use.
expect 201 "${PAT[owner]}" POST "$CFG/database/query" '{"query":"create table if not exists public.roles_t (id int primary key, v text); insert into public.roles_t values (1, '"'"'a'"'"') on conflict do nothing"}'
for r in ro dev admin owner; do
  expect 200 "${PAT[$r]}" GET /v1/projects
  expect 200 "${PAT[$r]}" GET "/v1/organizations/$ORG/members"
  expect 200 "${PAT[$r]}" GET "$CFG/secrets"
  expect 200 "${PAT[$r]}" GET "$CFG/database/migrations"
  expect 200 "${PAT[$r]}" GET "$CFG/types/typescript"
  expect 201 "${PAT[$r]}" POST "$CFG/database/query" '{"query":"select v from public.roles_t"}'
done
[[ $(body "${PAT[ro]}" GET /v1/projects | json_get 'len(d)') == 1 ]] || fail "read-only lists the project"
expect 403 "${PAT[ro]}" POST "$CFG/secrets" '[{"name":"RO_SECRET","value":"x"}]'
expect 403 "${PAT[ro]}" DELETE "$CFG/secrets" '["RO_SECRET"]'
expect 403 "${PAT[ro]}" POST "$CFG/database/migrations" '{"query":"create table public.ro_migration (id int)","name":"ro_migration"}'
expect 403 "${PAT[ro]}" PATCH "$CFG/config/auth" '{"site_url":"https://ro.example.com"}'
expect 403 "${PAT[ro]}" POST "$CFG/api-keys" '{"type":"publishable","name":"ro_key"}'
expect 403 "${PAT[ro]}" GET "$CFG/api-keys?reveal=true"
expect 403 "${PAT[ro]}" POST "$CFG/pause"
# Backups: Read-only lists them; a restore overwrites production data, so it is for Owners and Administrators.
expect 200 "${PAT[ro]}" GET "$CFG/database/backups"
expect 403 "${PAT[ro]}" POST "$CFG/database/backups/restore-pitr" '{"recovery_time_target_unix":1760000000}'
expect 403 "${PAT[ro]}" POST "$CFG/database/backups/restore" '{"id":1}'
expect 403 "${PAT[ro]}" DELETE "$CFG"
expect 403 "${PAT[ro]}" PATCH "$CFG" '{"name":"renamed"}'
for q in "insert into public.roles_t values (2, 'b')" "update public.roles_t set v = 'x'" "delete from public.roles_t" "drop table public.roles_t" \
         "begin read write; delete from public.roles_t; commit" "reset role; delete from public.roles_t"; do
  code=$(st "${PAT[ro]}" POST "$CFG/database/query" "{\"query\":\"$q\"}")
  [[ $code =~ ^4 ]] || fail "read-only SQL '$q' answered $code"
done
[[ $(sql "select count(*) from public.roles_t") == 1 && $(sql "select v from public.roles_t") == a ]] || fail "a read-only statement changed data"
[[ $(sql "select to_regclass('public.ro_migration') is null") == t ]] || fail "a read-only migration ran"
# The read-only connection string the databases listing hands Studio names hosted's read-only user
# (the API never connects as it); the shipped Postgres creates it, with login, bypassrls and pg_read_all_data.
[[ $(sql "select r.rolcanlogin and r.rolbypassrls and pg_has_role(r.oid, 'pg_read_all_data', 'member') from pg_roles r where r.rolname = 'supabase_read_only_user'") == t ]] || fail "supabase_read_only_user is missing, or lacks login, bypassrls or pg_read_all_data, in the pinned Postgres"
# Its sessions open read-only (the Go test of the SQL text in internal/api skips in CI, so this is where it is checked).
[[ $(sql "select coalesce('default_transaction_read_only=on' = any(r.rolconfig), false) from pg_roles r where r.rolname = 'supabase_read_only_user'") == t ]] || fail "supabase_read_only_user does not open its sessions with default_transaction_read_only = on in the pinned Postgres"
# The login role the CLI uses for `db dump` is the read-only one, even when read-write is asked.
LR=$(body "${PAT[ro]}" POST "$CFG/cli/login-role" '{"read_only":false}')
[[ $(json_get 'd["role"].startswith("supavise_cli_ro_")' <<<"$LR") == True ]] || fail "read-only member got a read-write login role: $LR"
# Dropping the project's login roles would break every member's CLI session.
expect 403 "${PAT[ro]}" DELETE "$CFG/cli/login-role"
# Shared saved items belong to their owner: a Read-only member cannot rewrite the snippet an Owner shares.
SNIP=44444444-4444-4444-8444-444444444444
code=$(papi "${JWT[owner]}" PUT "/platform/projects/$REF/content" "{\"id\":\"$SNIP\",\"name\":\"shared\",\"type\":\"sql\",\"visibility\":\"project\",\"content\":{\"sql\":\"select 1\"}}")
[[ $code == 200 ]] || fail "the Owner could not share a snippet: $code"
code=$(papi "${JWT[ro]}" PUT "/platform/projects/$REF/content" "{\"id\":\"$SNIP\",\"name\":\"shared\",\"type\":\"sql\",\"visibility\":\"project\",\"content\":{\"sql\":\"drop table public.roles_t\"}}")
[[ $code == 403 ]] || fail "a read-only member rewrote a shared snippet: $code"
code=$(papi "${JWT[ro]}" DELETE "/platform/projects/$REF/content?ids=$SNIP")
[[ $code == 403 ]] || fail "a read-only member deleted a shared snippet: $code"
papi "${JWT[owner]}" GET "/platform/projects/$REF/content/item/$SNIP" >/dev/null
[[ $(json_get 'd["content"]["sql"]' <"$WORK/last.json") == "select 1" ]] || fail "the shared snippet changed"
# The same SQL through Studio's pg-meta route (a dashboard session).
code=$(papi "${JWT[ro]}" POST "/platform/pg-meta/$REF/query" '{"query":"delete from public.roles_t"}')
[[ $code =~ ^4 ]] || fail "read-only pg-meta delete answered $code"
[[ $(sql "select count(*) from public.roles_t") == 1 ]] || fail "pg-meta changed data for a read-only member"
code=$(papi "${JWT[ro]}" POST "/platform/pg-meta/$REF/query" '{"query":"select v from public.roles_t"}')
[[ $code == 201 || $code == 200 ]] || fail "read-only pg-meta select answered $code"
# Secrets stay with the roles that may read them.
SETTINGS=$(curl -sS -m 30 -H "Authorization: Bearer ${JWT[ro]}" "$ADMIN/platform/projects/$REF/settings")
[[ $(json_get '"jwt_secret" in d' <<<"$SETTINGS") == False ]] || fail "the JWT secret is shown to a read-only member"
[[ $(json_get 'len(d["service_api_keys"])' <<<"$SETTINGS") == 1 ]] || fail "the service key is shown to a read-only member"
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[dev]}" "$ADMIN/platform/projects/$REF/settings" | json_get 'len(d["service_api_keys"])') == 2 ]] || fail "a developer must see the service key"

log "Developer: content yes, settings and secrets no"
expect 201 "${PAT[dev]}" POST "$CFG/database/query" '{"query":"insert into public.roles_t values (2, '"'"'dev'"'"')"}'
[[ $(sql "select v from public.roles_t where id = 2") == dev ]] || fail "the developer's insert is missing"
code=$(st "${PAT[dev]}" POST "$CFG/database/migrations" '{"query":"create table public.dev_migration (id int)","name":"dev_migration"}')
[[ $code == 200 || $code == 201 ]] || fail "the developer's migration answered $code"
[[ $(sql "select to_regclass('public.dev_migration') is not null") == t ]] || fail "the developer's migration did not run"
expect 403 "${PAT[dev]}" POST "$CFG/secrets" '[{"name":"DEV_SECRET","value":"x"}]'
expect 403 "${PAT[dev]}" PATCH "$CFG/config/auth" '{"site_url":"https://dev.example.com"}'
expect 403 "${PAT[dev]}" POST "$CFG/api-keys" '{"type":"publishable","name":"dev_key"}'
expect 403 "${PAT[dev]}" POST "$CFG/pause"
expect 200 "${PAT[dev]}" GET "$CFG/database/backups"
expect 403 "${PAT[dev]}" POST "$CFG/database/backups/restore-pitr" '{"recovery_time_target_unix":1760000000}'
expect 403 "${PAT[dev]}" POST "$CFG/database/backups/restore" '{"id":1}'
expect 403 "${PAT[dev]}" PATCH "$CFG/database/password" '{"password":"a-new-database-password"}'
# A branch with data copies production data: Owner and Administrator only (a schema-only branch is the Developer's).
expect 403 "${PAT[dev]}" POST "$CFG/branches" '{"branch_name":"dev-data","with_data":true}'
expect 403 "${PAT[ro]}" POST "$CFG/branches" '{"branch_name":"ro-data","with_data":true}'
# The list has the default branch (the project itself); nothing else may have been made.
[[ $(body "${PAT[owner]}" GET "$CFG/branches" | json_get 'len([b for b in d if not b.get("is_default")])') == 0 ]] || fail "a refused branch with data was created"
expect 403 "${PAT[dev]}" POST "/v1/projects" "{\"name\":\"nope\",\"organization_slug\":\"$ORG\",\"db_pass\":\"correct-horse-battery\"}"

log "Database Webhooks: Enable creates the schema, a trigger fires a request, the response is recorded"
[[ $(sql "select to_regnamespace('supabase_functions') is not null") == f ]] || fail "supabase_functions exists before webhooks were enabled"
# Enabling needs the right to change triggers: refused for Read-only on both routes, before the database is touched.
expect 403 "${PAT[ro]}" POST "$CFG/database/webhooks/enable"
code=$(papi "${JWT[ro]}" POST "/platform/database/$REF/hook-enable")
[[ $code == 403 ]] || fail "a read-only member enabled webhooks through the dashboard route: $code"
[[ $(sql "select to_regnamespace('supabase_functions') is not null") == f ]] || fail "a refused call created supabase_functions"
# The script runs as a superuser, and a Developer (the postgres role) can make objects before Enable is called. Trying the two
# ways to turn that into superuser code: a supabase_functions schema of the Developer's own with a trigger on its migrations
# table (the script's INSERT would fire it), and a role named supabase_pg_net_admin (upstream's cleanup reassigns what it owns
# to supabase_admin). Setup that the pinned Postgres does not allow a Developer is not an attack, so it is skipped with a note.
devq() { # SQL: run SQL through pg-meta as the Developer (the postgres role); the status is the result
  papi "${JWT[dev]}" POST "/platform/pg-meta/$REF/query" "$(python3 -c 'import json,sys; print(json.dumps({"query": sys.argv[1]}))' "$1")"
}
ok2xx() { [[ $1 == 200 || $1 == 201 ]]; }
ok2xx "$(devq "create table public.hook_spy_log (who text, su boolean);
create function public.hook_spy() returns trigger language plpgsql as \$\$ begin insert into public.hook_spy_log select current_user, rolsuper from pg_roles where rolname = current_user; return null; end \$\$;")" \
  || fail "a Developer could not create the spy table and function: $(head -c 300 "$WORK/last.json")"
PGNET_ROLE=0
if ok2xx "$(devq "create role supabase_pg_net_admin")"; then PGNET_ROLE=1; else log "note: a Developer cannot create the role supabase_pg_net_admin here; that check is skipped"; fi
if ok2xx "$(devq "create schema supabase_functions;
create table supabase_functions.migrations (version text primary key, inserted_at timestamptz not null default now());
create trigger spy before insert on supabase_functions.migrations for each row execute function public.hook_spy();
create trigger spy_stmt before insert on supabase_functions.migrations for each statement execute function public.hook_spy();")"; then
  code=$(papi "${JWT[dev]}" POST "/platform/database/$REF/hook-enable")
  [[ $code == 400 ]] || fail "enabling webhooks over a schema the Developer made answered $code, want 400: $(head -c 300 "$WORK/last.json")"
  grep -q "not to supabase_admin" "$WORK/last.json" || { cat "$WORK/last.json" >&2; fail "the refusal does not say that the schema does not belong to supabase_admin"; }
  [[ $(sql "select count(*) from public.hook_spy_log") == 0 ]] || { sql "select * from public.hook_spy_log" >&2; fail "enabling webhooks fired a trigger of the project's as $(sql "select string_agg(who || ' superuser=' || su, ', ') from public.hook_spy_log")"; }
  [[ $(sql "select to_regprocedure('supabase_functions.http_request()') is null and to_regclass('supabase_functions.hooks') is null") == t ]] \
    || fail "a refused enable left the webhooks objects behind"
  expect 201 "${PAT[dev]}" POST "$CFG/database/query" '{"query":"drop schema supabase_functions cascade"}'
else
  log "note: a Developer cannot create a schema or trigger here; the check of a schema the project made is skipped"
  devq "drop schema if exists supabase_functions cascade" >/dev/null
fi
[[ $(sql "select to_regnamespace('supabase_functions') is not null") == f ]] || fail "supabase_functions is still there after the refused-schema check"
expect 201 "${PAT[dev]}" POST "$CFG/database/webhooks/enable"
if [[ $PGNET_ROLE == 1 ]]; then
  [[ $(sql "select count(*) from pg_roles where rolname = 'supabase_pg_net_admin'") == 1 ]] || fail "enabling webhooks dropped the role supabase_pg_net_admin that the Developer made"
  sql "drop role supabase_pg_net_admin"
fi
[[ $(sql "select to_regprocedure('supabase_functions.http_request()') is not null and exists (select 1 from pg_extension where extname = 'pg_net') and exists (select 1 from pg_roles where rolname = 'supabase_functions_admin')") == t ]] \
  || fail "enabling webhooks left no supabase_functions.http_request(), pg_net or supabase_functions_admin"
# Studio shows "enabled" when the schema is listed; the function runs as its owner, which is not postgres.
[[ $(sql "select proowner::regrole::text || ' ' || prosecdef from pg_proc where oid = 'supabase_functions.http_request'::regproc") == 'supabase_functions_admin t' ]] \
  || fail "http_request() is not a SECURITY DEFINER function of supabase_functions_admin"
# The artifact's pg_net (0.12 and newer) keeps net.http_post executable by PUBLIC: the script must not have reverted that.
[[ $(sql "select prosecdef from pg_proc where oid = 'net.http_post(text,jsonb,jsonb,jsonb,integer)'::regprocedure") == f ]] \
  || fail "enabling webhooks made net.http_post SECURITY DEFINER"
# A second call (another click, a retry) succeeds and changes nothing.
before=$(sql "select count(*) from supabase_functions.migrations")
code=$(papi "${JWT[dev]}" POST "/platform/database/$REF/hook-enable")
[[ $code == 201 ]] || fail "enabling webhooks a second time answered $code"
[[ $(sql "select count(*) from supabase_functions.migrations") == "$before" ]] || fail "a second enable added migration rows"
# Once the tables exist, the Developer holds privileges on them and can add a trigger. A repeated enable must not run it: it
# writes no row into a table that exists (a BEFORE INSERT trigger fires even for an INSERT ... ON CONFLICT DO NOTHING).
if ok2xx "$(devq "create trigger spy before insert on supabase_functions.migrations for each row execute function public.hook_spy();
create trigger spy_stmt before insert on supabase_functions.migrations for each statement execute function public.hook_spy();")"; then
  code=$(papi "${JWT[dev]}" POST "/platform/database/$REF/hook-enable")
  [[ $code == 201 ]] || fail "enabling webhooks with a trigger on the migrations table answered $code: $(head -c 300 "$WORK/last.json")"
  [[ $(sql "select count(*) from public.hook_spy_log") == 0 ]] || fail "a repeated enable ran the project's trigger as $(sql "select string_agg(who || ' superuser=' || su, ', ') from public.hook_spy_log")"
  ok2xx "$(devq "drop trigger spy on supabase_functions.migrations; drop trigger spy_stmt on supabase_functions.migrations;")" || fail "could not drop the spy triggers"
else
  log "note: a Developer cannot add a trigger to supabase_functions.migrations here; that check is skipped"
fi
devq "drop function public.hook_spy(); drop table public.hook_spy_log;" >/dev/null
# What Studio does next: create the webhook as a trigger through pg-meta (the postgres role), here on a table the Developer owns.
expect 201 "${PAT[dev]}" POST "$CFG/database/query" '{"query":"create table public.hook_t (id int primary key, v text)"}'
code=$(papi "${JWT[dev]}" POST "/platform/pg-meta/$REF/query" "{\"query\":\"create trigger roles_hook after insert on public.hook_t for each row execute function supabase_functions.http_request('http://127.0.0.1:$P_HOOK/roles-hook', 'POST', '{\\\"Content-Type\\\":\\\"application/json\\\"}', '{}', '5000')\"}")
[[ $code == 200 || $code == 201 ]] || fail "creating a webhook trigger through pg-meta answered $code: $(head -c 300 "$WORK/last.json")"
# Studio lists webhooks as the triggers whose function is in supabase_functions.
[[ $(sql "select count(*) from pg_trigger t join pg_proc p on p.oid = t.tgfoid where not t.tgisinternal and t.tgname = 'roles_hook' and p.pronamespace = 'supabase_functions'::regnamespace") == 1 ]] \
  || fail "the webhook trigger is not listed under supabase_functions"
expect 201 "${PAT[dev]}" POST "$CFG/database/query" '{"query":"insert into public.hook_t values (1, '"'"'fires'"'"')"}'
for i in $(seq 1 30); do
  [[ $(sql "select count(*) from net._http_response where status_code = 200") -ge 1 ]] && break
  sleep 1
done
[[ $(sql "select count(*) from net._http_response where status_code = 200") -ge 1 ]] \
  || { sql "select id, status_code, error_msg from net._http_response" >&2; fail "the insert produced no net._http_response row with status 200"; }
grep -q '^/roles-hook .*"record".*"fires"' "$WORK/hooks.txt" || { cat "$WORK/hooks.txt" >&2; fail "the webhook target did not receive the insert"; }
[[ $(sql "select count(*) from supabase_functions.hooks where hook_name = 'roles_hook'") == 1 ]] || fail "supabase_functions.hooks has no row for the webhook"
# Deleting the webhook is a DROP TRIGGER through pg-meta.
code=$(papi "${JWT[dev]}" POST "/platform/pg-meta/$REF/query" '{"query":"drop trigger roles_hook on public.hook_t"}')
[[ $code == 200 || $code == 201 ]] || fail "dropping the webhook trigger answered $code"

log "Logs: queries say there are no logs, supavise logs reads the journal, the request chart counts proxy requests"
# A log query is an error, not an empty answer (Studio's logs pages, the reports and the MCP tools show it).
code=$(papi "${JWT[dev]}" GET "/platform/projects/$REF/analytics/endpoints/logs.all?sql=select%201&iso_timestamp_start=2026-01-01T00:00:00Z&iso_timestamp_end=2026-01-01T01:00:00Z")
[[ $code == 200 ]] || fail "logs.all answered $code"
[[ $(json_get 'len(d["result"])' <"$WORK/last.json") == 0 ]] || fail "logs.all returned rows"
json_get 'd["error"]' <"$WORK/last.json" | grep -q "supavise logs $REF" || { cat "$WORK/last.json" >&2; fail "logs.all does not say to run supavise logs $REF"; }
[[ $(st "${PAT[owner]}" GET "$CFG/analytics/endpoints/logs.all") == 410 ]] || fail "v1 logs.all is not gone (410)"
json_get 'd["error"]' < <(body "${PAT[owner]}" GET "$CFG/analytics/endpoints/logs") | grep -q "supavise logs $REF" || fail "v1 logs does not say to run supavise logs $REF"
# Iceberg and vector buckets are hidden: the profile lists them as disabled features.
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[owner]}" "$ADMIN/platform/profile" | json_get '"storage:analytics" in d["disabled_features"] and "storage:vectors" in d["disabled_features"]') == True ]] \
  || fail "the profile does not hide storage:analytics and storage:vectors"

# supavise logs runs as root here: the supavise user may not read the system journal.
SVLOG=/usr/local/bin/supavise
$SVLOG logs "$REF" --service postgres -n 20000 >"$WORK/pglog.txt" 2>"$WORK/pglog.err" || { cat "$WORK/pglog.err" >&2; fail "supavise logs --service postgres failed"; }
[[ -s $WORK/pglog.txt ]] || { cat "$WORK/pglog.err" >&2; fail "supavise logs printed no Postgres line"; }
grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z postgres +' "$WORK/pglog.txt" || { head -5 "$WORK/pglog.txt" >&2; fail "supavise logs lines are not '<time> postgres <message>'"; }
grep -q "ready to accept connections" "$WORK/pglog.txt" || { tail -5 "$WORK/pglog.txt" >&2; fail "supavise logs does not show the Postgres start"; }
# A shared service shows only the lines that mention the project. Realtime and Storage log at warning level, so the
# journal holds a line only when one of them reports a problem: provoke one per service (a Realtime join with a
# malformed token, a Storage call with a bad key, a pooler login with a wrong password), then require that every line
# a service prints mentions the ref. A service that prints nothing is reported, not failed: whether its warning
# carries the ref depends on the service's logger, which this test cannot see.
ws_join "$P_REALTIME" "$REF.realtime.internal" "not-a-token" >/dev/null 2>&1 || true
curl -s -o /dev/null -m 10 -H "Host: $REF.api.$SUPAVISE_DOMAIN" -H "apikey: not-a-token" -H "Authorization: Bearer not-a-token" "http://127.0.0.1/storage/v1/bucket" || true
PGPASSWORD=a-wrong-password "$PSQL" "host=127.0.0.1 port=$P_SESSION user=postgres.$REF dbname=postgres sslmode=disable connect_timeout=10" -Atc 'select 1' </dev/null >/dev/null 2>&1 || true
sleep 2
for svc in realtime storage pooler functions; do
  $SVLOG logs "$REF" --service "$svc" -n 500 >"$WORK/shared-$svc.txt" 2>/dev/null || fail "supavise logs --service $svc failed"
  if grep -v "$REF" "$WORK/shared-$svc.txt" | grep -q .; then head -3 "$WORK/shared-$svc.txt" >&2; fail "supavise logs shows $svc lines that do not mention the project"; fi
  log "supavise logs --service $svc: $(wc -l <"$WORK/shared-$svc.txt" | tr -d ' ') line(s) that mention the project"
done
# Every service together, a window, and the refusals.
$SVLOG logs "$REF" -n 50 --since 1d >"$WORK/alllog.txt" 2>/dev/null || fail "supavise logs (all services, --since 1d) failed"
[[ $(wc -l <"$WORK/alllog.txt") -le 50 ]] || fail "supavise logs -n 50 printed more than 50 lines"
$SVLOG logs "$REF" --since yesterday >/dev/null 2>"$WORK/since.err" && fail "supavise logs accepted --since yesterday"
grep -q -- "--since" "$WORK/since.err" || fail "supavise logs --since yesterday: no explanation"
$SVLOG logs not-a-ref >/dev/null 2>"$WORK/ref.err" && fail "supavise logs accepted a bad ref"
# --follow prints a line that Postgres writes after it started (a statement the log records at LOG level).
( timeout 10 $SVLOG logs "$REF" --service postgres -n 0 -f >"$WORK/follow.txt" 2>/dev/null || true ) &
FOLLOW_PID=$!
sleep 2
sql "do \$\$ begin raise warning 'supavise-logs-follow-marker'; end \$\$"
wait "$FOLLOW_PID"
grep -q "supavise-logs-follow-marker" "$WORK/follow.txt" || { cat "$WORK/follow.txt" >&2; fail "supavise logs -f did not print a line written after it started"; }

# The project home's request chart (usage.api-counts) counts what the proxy served: unauthenticated
# requests answered 401 are requests too.
req_sum() { # the REST requests of the last hour, as the dashboard asks for them
  papi "${JWT[owner]}" GET "/platform/projects/$REF/analytics/endpoints/usage.api-counts?interval=1hr" >/dev/null
  json_get 'sum(r["total_rest_requests"] for r in d["result"])' <"$WORK/last.json"
}
before=$(req_sum)
for _ in 1 2 3 4 5; do
  [[ $(curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Host: $REF.api.$SUPAVISE_DOMAIN" "http://127.0.0.1/rest/v1/roles_t") == 401 ]] || fail "an unauthenticated REST request was not refused with 401"
done
after=$(req_sum)
[[ $((after - before)) -ge 5 ]] || fail "usage.api-counts counted $((after - before)) of 5 requests (before $before, after $after)"
[[ $(json_get 'len(d["result"])' <"$WORK/last.json") -ge 60 ]] || fail "usage.api-counts 1hr has fewer than 60 minute buckets"

# Hosted features a node does not offer say so instead of succeeding: SSL enforcement is "not allowed" (Studio shows a
# disabled switch), the legacy signing key does not exist (Studio offers to migrate), a JWT secret change is refused with the
# command that does it, and no JWT secret update is "running" (a stub's status 0 means "updating").
[[ $(st "${PAT[owner]}" GET "$CFG/ssl-enforcement") == 400 ]] || fail "GET ssl-enforcement is not refused with 400"
body "${PAT[owner]}" GET "$CFG/ssl-enforcement" | grep -q "not allowed to configure SSL enforcements" || fail "ssl-enforcement does not say that configuring it is not allowed"
[[ $(st "${PAT[owner]}" GET "$CFG/config/auth/signing-keys/legacy") == 404 ]] || fail "GET signing-keys/legacy is not 404"
code=$(papi "${JWT[owner]}" PATCH "/platform/projects/$REF/config/secrets" '{"jwt_secret":"a-new-secret-a-new-secret-a-new","change_tracking_id":"00000000-0000-4000-8000-000000000000"}')
[[ $code == 400 ]] || fail "PATCH config/secrets answered $code, want 400"
grep -q "supavise projects rotate-keys $REF" "$WORK/last.json" || { cat "$WORK/last.json" >&2; fail "PATCH config/secrets does not name supavise projects rotate-keys"; }
papi "${JWT[owner]}" GET "/platform/projects/$REF/config/secrets/update-status" >/dev/null
[[ $(json_get 'd["update_status"]' <"$WORK/last.json") == None ]] || fail "a JWT secret update is reported as running"

log "Administrator: settings and members, never Owners"
# An Administrator passes the permission check of a restore; the time is far outside any window, so
# the request is refused on its merits (400) before the project is touched.
expect 400 "${PAT[admin]}" POST "$CFG/database/backups/restore-pitr" '{"recovery_time_target_unix":1760000000}'
expect 201 "${PAT[admin]}" POST "$CFG/secrets" '[{"name":"ADMIN_SECRET","value":"x"}]'
expect 200 "${PAT[admin]}" PATCH "$CFG/config/auth" '{"site_url":"https://admin.example.com"}'
expect 201 "${PAT[admin]}" POST "$CFG/api-keys" '{"type":"publishable","name":"admin_key"}'
M="/platform/organizations/$ORG/members"
[[ $(papi "${JWT[admin]}" PATCH "$M/${UID_OF[owner]}" '{"role_id":3}') == 403 ]] || fail "an administrator demoted an owner"
[[ $(papi "${JWT[admin]}" PATCH "$M/${UID_OF[dev]}" '{"role_id":1}') == 403 ]] || fail "an administrator made an owner"
[[ $(papi "${JWT[admin]}" DELETE "$M/${UID_OF[owner]}") == 403 ]] || fail "an administrator removed an owner"
[[ $(papi "${JWT[admin]}" PATCH "$M/${UID_OF[dev]}" '{"role_id":2}') == 200 ]] || fail "an administrator could not promote a developer"
[[ $(papi "${JWT[admin]}" PATCH "$M/${UID_OF[dev]}" '{"role_id":3}') == 200 ]] || fail "an administrator could not demote an administrator"
[[ $(papi "${JWT[dev]}" PATCH "$M/${UID_OF[ro]}" '{"role_id":3}') == 403 ]] || fail "a developer changed a role"
[[ $(papi "${JWT[ro]}" DELETE "$M/${UID_OF[dev]}") == 403 ]] || fail "a read-only member removed a member"
[[ $(papi "${JWT[admin]}" PATCH "/platform/organizations/$ORG" '{"name":"Renamed"}') == 403 ]] || fail "an administrator changed the organization"
[[ $(papi "${JWT[owner]}" PATCH "/platform/organizations/$ORG" '{"name":"Roles"}') == 200 ]] || fail "an owner could not change the organization"

log "the organization keeps its last Owner"
[[ $(papi "${JWT[owner]}" PATCH "$M/${UID_OF[owner]}" '{"role_id":2}') == 400 ]] || fail "the last owner was demoted"
[[ $(papi "${JWT[owner]}" DELETE "$M/${UID_OF[owner]}") == 400 ]] || fail "the last owner left"
if supavise users remove owner@example.com >"$WORK/rm.out" 2>&1; then fail "users remove deleted the only owner"; fi
grep -q "only Owner" "$WORK/rm.out" || { cat "$WORK/rm.out" >&2; fail "users remove: unexpected refusal"; }

log "deleting an organization: Owners only, and never the node's last"
[[ $(papi "${JWT[owner]}" POST /platform/organizations '{"name":"Scrap"}') == 201 ]] || { cat "$WORK/last.json" >&2; fail "creating a second organization"; }
SCRAP=$(json_get 'd["slug"]' <"$WORK/last.json")
supavise users role admin@example.com administrator --org "$SCRAP" >/dev/null || fail "making an administrator of the second organization"
[[ $(papi "${JWT[admin]}" DELETE "/platform/organizations/$SCRAP") == 403 ]] || fail "an administrator deleted an organization"
[[ $(papi "${JWT[dev]}" DELETE "/platform/organizations/$ORG") == 403 ]] || fail "a developer deleted an organization"
if supavise orgs delete "$SCRAP" >"$WORK/orgs.out" 2>&1; then fail "orgs delete without --yes deleted the organization"; fi
supavise orgs list >"$WORK/orgs.txt" || fail "orgs list"
grep -q "^$SCRAP " "$WORK/orgs.txt" || { cat "$WORK/orgs.txt" >&2; fail "orgs list does not show $SCRAP"; }
[[ $(papi "${JWT[owner]}" DELETE "/platform/organizations/$SCRAP") == 200 ]] || { cat "$WORK/last.json" >&2; fail "an owner could not delete an organization"; }
[[ $(papi "${JWT[owner]}" GET /platform/organizations) == 200 ]] || fail "listing organizations"
[[ $(json_get '",".join(sorted(o["slug"] for o in d))' <"$WORK/last.json") == "$ORG" ]] || fail "the deleted organization is still listed: $(cat "$WORK/last.json")"
supavise orgs list >"$WORK/orgs.txt" || fail "orgs list"
if grep -q "^$SCRAP " "$WORK/orgs.txt"; then fail "orgs list still shows $SCRAP"; fi
[[ $(papi "${JWT[owner]}" DELETE "/platform/organizations/$ORG") == 409 ]] || fail "the node's last organization was not kept"
[[ $(body "${PAT[owner]}" GET /v1/projects | json_get 'len(d)') != 0 ]] || fail "a refused organization deletion removed projects"

log "a project-scoped role: only that project's rights"
[[ $(papi "${JWT[owner]}" PATCH "$M/${UID_OF[outsider]}" "{\"role_id\":3,\"role_scoped_projects\":[\"$REF\"]}") == 200 ]] || fail "scoped role"
[[ $(papi "${JWT[owner]}" DELETE "$M/${UID_OF[outsider]}/roles/4") == 200 ]] || fail "dropping the organization-wide role"
expect 201 "${PAT[outsider]}" POST "$CFG/database/query" '{"query":"select 1"}'
expect 403 "${PAT[outsider]}" POST "$CFG/secrets" '[{"name":"X","value":"y"}]'
[[ $(papi "${JWT[outsider]}" GET "/platform/organizations/$ORG/members/invitations") == 403 ]] || fail "a project-scoped member read the invitations"
# Removing the member takes the access of the tokens they made with it.
[[ $(papi "${JWT[owner]}" DELETE "$M/${UID_OF[outsider]}") == 200 ]] || fail "removing a member"
expect 403 "${PAT[outsider]}" POST "$CFG/database/query" '{"query":"select 1"}'
[[ $(body "${PAT[outsider]}" GET /v1/projects | json_get 'len(d)') == 0 ]] || fail "a removed member lists projects"

log "the real MCP server: Read-only lists and is refused writes, Owner writes"
if command -v node >/dev/null && command -v npx >/dev/null; then
  SUPABASE_ACCESS_TOKEN=${PAT[owner]} node "$REPO_ROOT/internal/api/testdata/mcp-roles.mjs" "$ADMIN" "$REF" owner || fail "MCP as Owner"
  SUPABASE_ACCESS_TOKEN=${PAT[ro]} node "$REPO_ROOT/internal/api/testdata/mcp-roles.mjs" "$ADMIN" "$REF" read-only || fail "MCP as Read-only"
else
  [[ ${REQUIRE_CLIENTS:-} == 1 ]] && fail "node and npx are required (REQUIRE_CLIENTS=1)"
  log "node is not installed: the MCP checks are skipped"
fi

log "the real Supabase CLI: Read-only lists and is refused secrets set, Owner sets"
if [[ -n ${SUPABASE_CLI:-} ]]; then
  mkdir -p "$WORK/proj" "$WORK/home"
  # pooler_host is the registrable domain of the pooler host; supavise derives it, as `supavise api profile` does for users.
  POOLER_DOMAIN=$(supavise api profile --format json | json_get 'd["pooler_host"]') || fail "supavise api profile"
  cat >"$WORK/profile.yaml" <<PROFILE
name: supavise-roles
api_url: $ADMIN
dashboard_url: http://127.0.0.1:1
project_host: api.$SUPAVISE_DOMAIN
pooler_host: $POOLER_DOMAIN
PROFILE
  cli() { local t=$1; shift; (cd "$WORK/proj" && HOME="$WORK/home" SUPABASE_ACCESS_TOKEN=$t SUPABASE_NO_KEYRING=1 DO_NOT_TRACK=1 "$SUPABASE_CLI" --profile="$WORK/profile.yaml" "$@"); }
  cli "${PAT[ro]}" projects list >"$WORK/cli.out" 2>&1 || { cat "$WORK/cli.out" >&2; fail "CLI projects list as Read-only"; }
  grep -q "$REF" "$WORK/cli.out" || fail "CLI projects list does not show the project"
  cli "${PAT[ro]}" secrets list --project-ref "$REF" >"$WORK/cli.out" 2>&1 || { cat "$WORK/cli.out" >&2; fail "CLI secrets list as Read-only"; }
  cli "${PAT[ro]}" gen types typescript --project-id "$REF" >"$WORK/cli.out" 2>&1 || { cat "$WORK/cli.out" >&2; fail "CLI gen types as Read-only"; }
  if cli "${PAT[ro]}" secrets set CLI_RO=1 --project-ref "$REF" >"$WORK/cli.out" 2>&1; then cat "$WORK/cli.out" >&2; fail "CLI secrets set worked for a Read-only member"; fi
  grep -q "does not allow" "$WORK/cli.out" || { cat "$WORK/cli.out" >&2; fail "CLI secrets set: no explanation in the refusal"; }
  cli "${PAT[owner]}" secrets set CLI_OWNER=1 --project-ref "$REF" >"$WORK/cli.out" 2>&1 || { cat "$WORK/cli.out" >&2; fail "CLI secrets set as Owner"; }
  cli "${PAT[ro]}" secrets list --project-ref "$REF" >"$WORK/cli.out" 2>&1 || fail "CLI secrets list after the Owner's set"
  grep -q CLI_OWNER "$WORK/cli.out" || { cat "$WORK/cli.out" >&2; fail "the Owner's secret is not listed"; }
else
  [[ ${REQUIRE_CLIENTS:-} == 1 ]] && fail "SUPABASE_CLI is required (REQUIRE_CLIENTS=1)"
  log "SUPABASE_CLI is not set: the CLI checks are skipped"
fi

log "mail: invitations by email, to a new address and to an existing account"
cat >"$WORK/links.py" <<'PY'
import email, html, json, re, sys
raw = open(sys.argv[1]).read().split("\n=====\n")
out = []
for m in raw:
    if not m.strip(): continue
    msg = email.message_from_string(m)
    body = ""
    for part in (msg.walk() if msg.is_multipart() else [msg]):
        if part.get_content_type() in ("text/html", "text/plain"):
            body += part.get_payload(decode=True).decode(errors="replace")
    links = [html.unescape(h) for h in re.findall(r'href="([^"]+)"', body)] or re.findall(r"https?://\S+", body)
    out.append({"to": msg["To"], "subject": msg["Subject"], "links": links})
print(json.dumps(out))
PY
: >"$WORK/mails.txt"
mkuser outsider2 read-only   # an account that is then removed from the organization
[[ $(papi "${JWT[owner]}" DELETE "$M/${UID_OF[outsider2]}") == 200 ]] || fail "removing outsider2"
CODE=$(papi "${JWT[owner]}" POST "$M/invitations" '{"emails":["outsider2@example.com","newhire@example.com"],"role_id":3}')
[[ $CODE == 201 ]] || { cat "$WORK/last.json" >&2; fail "invitations answered $CODE"; }
[[ $(json_get 'sorted(d["succeeded"])' <"$WORK/last.json") == "['newhire@example.com', 'outsider2@example.com']" ]] || fail "invitations: $(cat "$WORK/last.json")"
for ((i = 0; i < 30; i++)); do
  [[ $(grep -c '^=====$' "$WORK/mails.txt" 2>/dev/null || echo 0) -ge 2 ]] && break
  sleep 2
done
python3 "$WORK/links.py" "$WORK/mails.txt" >"$WORK/mails.json"
[[ $(json_get 'len(d)' <"$WORK/mails.json") -ge 2 ]] || { cat "$WORK/mails.txt" >&2; fail "fewer than two invitation mails reached the sink"; }
accept_from_mail() { # ADDRESS: follow the mailed link, accept the invitation, check the role
  local addr=$1 link loc jwt tok sub
  link=$(json_get '[l for m in d if "'"$addr"'" in (m["to"] or "") for l in m["links"] if "/verify" in l][0]' <"$WORK/mails.json") || fail "no link mailed to $addr"
  loc=$(curl -s -o /dev/null -D - -m 30 --resolve "api.$SUPAVISE_DOMAIN:80:127.0.0.1" "$link" | tr -d '\r' | awk 'tolower($1)=="location:" {print $2}')
  [[ $loc == http://studio.$SUPAVISE_DOMAIN/join\?* ]] || fail "the mailed link for $addr does not lead to the invitation page: $loc"
  jwt=$(python3 -c 'import sys,urllib.parse as u; f=u.parse_qs(u.urlparse(sys.argv[1]).fragment); print(f["access_token"][0])' "$loc") || fail "the link for $addr carries no session: $loc"
  tok=$(python3 -c 'import sys,urllib.parse as u; print(u.parse_qs(u.urlparse(sys.argv[1]).query)["token"][0])' "$loc")
  [[ $tok =~ ^sbo_ ]] || fail "invitation token '$tok'"
  [[ $(papi "$jwt" GET "$M/invitations/$tok") == 200 ]] || { cat "$WORK/last.json" >&2; fail "invitation state for $addr"; }
  [[ $(json_get 'd["email_match"] and d["authorized_user"] and not d["expired_token"]' <"$WORK/last.json") == True ]] || fail "invitation state for $addr: $(cat "$WORK/last.json")"
  [[ $(papi "$jwt" POST "$M/invitations/$tok") == 201 ]] || { cat "$WORK/last.json" >&2; fail "accepting the invitation for $addr"; }
  [[ $(papi "$jwt" POST "$M/invitations/$tok") == 409 ]] || fail "a second accept for $addr was not refused"
  [[ $(papi "$jwt" PATCH "/platform/organizations/$ORG" '{"name":"x"}') == 403 ]] || fail "$addr changed the organization as a developer"
  sub=$(jwt_sub "$jwt")
  [[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[owner]}" "$ADMIN$M" | json_get '[x for x in d if x["gotrue_id"]=="'"$sub"'"][0]["role_ids"]') == "[3]" ]] || fail "$addr is not a developer"
}
accept_from_mail outsider2@example.com
accept_from_mail newhire@example.com
supavise users list >"$WORK/users.txt"
grep -q "newhire@example.com.*$ORG:developer" "$WORK/users.txt" || { cat "$WORK/users.txt" >&2; fail "users list does not show the invited user"; }

log "SSO default role rule"
supavise users default-role set corp.example.com developer --org "$ORG" >/dev/null || fail "default-role set"
supavise users default-role list >"$WORK/rules.txt"
grep -q "corp.example.com.*$ORG.*developer" "$WORK/rules.txt" || { cat "$WORK/rules.txt" >&2; fail "default-role list"; }
supavise users default-role remove corp.example.com || fail "default-role remove"

log "roles smoke passed"

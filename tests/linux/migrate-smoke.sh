#!/usr/bin/env bash
# Moving a project onto a node and off it again, with the commands of docs/guide.md ("Move a
# project from hosted Supabase" and "Move a project off Supavise") and the stock Supabase CLI.
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/path/to/supavise-linux-amd64 SUPABASE_CLI=/path/to/supabase \
#     tests/linux/migrate-smoke.sh [--teardown]
#
# A first project plays the hosted one: a JWT secret of the shape hosted generates, tables with
# RLS and explicit grants, a function and a trigger, a login role, a pg_cron job, Auth users with
# passwords (one signed in, so a refresh token exists), a Storage bucket and an object, and
# non-default Auth and PostgREST settings. The script reads the guide, takes the fenced block that
# follows each `<!-- migrate-smoke: NAME -->` marker, and runs it with the variables the guide
# names, so the guide and the test cannot drift apart. The steps are the guide's:
#
#   - the JWT secret and the two legacy keys are read through the Management API
#     (GET .../postgrest, GET .../api-keys?reveal=true) and `supavise projects create
#     --jwt-secret-file --anon-key-file --service-role-key-file` makes the second project, with a
#     ref of its own;
#   - `supabase db dump --db-url` (roles, schema, data; Docker) through the pooler on the session
#     port, and the single-transaction psql restore;
#   - the pg_cron jobs, which the dump leaves out, are created again; the Storage loop copies the
#     file; the settings loop copies Auth, PostgREST, Storage and Realtime settings without secrets.
#
# It then asserts what a moved project must do: row counts, RLS through PostgREST with the first
# project's anon key (the key bytes are the same), password sign-in and a refresh token of a moved
# user, the pg_cron job, the Storage object with its content type and cache control, the copied
# settings, and that the dump leaves the extension but not the job. The reverse direction dumps
# the second project the same way and restores into a third. On the third project it changes the
# JWT secret as Studio does (PATCH /platform/projects/{ref}/config/secrets): a key signed with the
# new secret works through the proxy, the old key does not, and the pgsodium root key is read and
# not replaced. The Postgres artifact's pg_dump, pg_dumpall and pg_restore are listed in
# $LOG_DIR/pg-tools.txt.
#
# Needs Docker (the CLI runs pg_dump in a container), jq and network access (artifacts, the
# CLI's Postgres image). Without SUPAVISE_BIN the script builds supavise with the go toolchain.
# SUPABASE_CLI is the CLI binary of the version in internal/versions/versions.yaml
# (cli.version_tested). Not run in development (root, systemd, Docker and Linux required); CI runs
# it on an ephemeral Ubuntu 24.04 VM (job migrate-smoke of linux.yml).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
WORK=$(mktemp -d)
chmod 0755 "$WORK" # the supavise user reads the key files the guide's commands write here
trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; rm -rf "$WORK"; exit $rc' EXIT

: "${SUPABASE_CLI:?set SUPABASE_CLI to the Supabase CLI binary}"
for c in docker jq curl python3; do command -v "$c" >/dev/null || fail "missing $c"; done
docker info >/dev/null 2>&1 || fail "Docker does not answer: supabase db dump runs pg_dump in a container"

# Ports away from anything the runner may listen on, as in settings-smoke.sh.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000

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
CONF

# Same VM-local workaround as settings-smoke.sh: the Postgres launcher writes into the artifact
# directory on its first boot, which ProtectSystem=strict forbids.
install -d /etc/systemd/system/supavise-postgres@.service.d
cat >/etc/systemd/system/supavise-postgres@.service.d/10-migrate-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/supavise/artifacts
CONF
systemctl daemon-reload

# The guide's commands name projects by host (https://<ref>.api.<domain>); with TLS off and no DNS
# every name is an alias of the proxy on :80.
host_alias() { echo "127.0.0.1 $1.api.$SUPAVISE_DOMAIN" >>/etc/hosts; }
echo "127.0.0.1 api.$SUPAVISE_DOMAIN" >>/etc/hosts

rand_ref() { python3 -c 'import random,string; print("".join(random.choice(string.ascii_lowercase) for _ in range(20)))'; }
# hs256 SECRET REF ROLE: a legacy API key of the project REF signed with SECRET.
hs256() {
  python3 - "$@" <<'PY'
import base64, hashlib, hmac, json, sys, time
def b64(b): return base64.urlsafe_b64encode(b).rstrip(b"=").decode()
secret, ref, role = sys.argv[1:4]
h = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
now = int(time.time())
p = b64(json.dumps({"iss": "supabase", "ref": ref, "role": role, "iat": now, "exp": now + 3600}).encode())
print(f"{h}.{p}.{b64(hmac.new(secret.encode(), f'{h}.{p}'.encode(), hashlib.sha256).digest())}")
PY
}

PSQL=""; PGBIN=""
log "system init, fleet, the project that plays the hosted one"
system_up
PSQL=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
PGBIN=$(dirname "$PSQL")
{
  for t in pg_dump pg_dumpall pg_restore psql pg_basebackup; do
    if [[ -x $PGBIN/$t ]]; then echo "$t: present ($("$PGBIN/$t" --version))"; else echo "$t: absent"; fi
  done
} | tee "$LOG_DIR/pg-tools.txt" >&2

# What `supabase` and `psql` mean in the guide's commands.
mkdir -p "$WORK/bin" "$WORK/hosted" "$WORK/reverse"
chmod 0755 "$WORK/bin" "$WORK/hosted" "$WORK/reverse"
cat >"$WORK/bin/supabase" <<SH
#!/usr/bin/env bash
exec env SUPABASE_NO_KEYRING=1 DO_NOT_TRACK=1 SUPABASE_DISABLE_UPDATE_CHECK=1 "$SUPABASE_CLI" "\$@" </dev/null
SH
chmod 0755 "$WORK/bin/supabase"
export PATH="$WORK/bin:$PGBIN:$PATH"

supavise fleet start || fail "fleet start"
HOSTED_SECRET=$(python3 -c 'import base64,secrets; print(base64.b64encode(secrets.token_bytes(64)).decode())')
printf '%s\n' "$HOSTED_SECRET" >"$WORK/hosted-secret.txt"
chmod 0644 "$WORK/hosted-secret.txt"
OLD_REF=$(rand_ref)
supavise projects create --name hosted --size micro --ref "$OLD_REF" --jwt-secret-file "$WORK/hosted-secret.txt" --json >/dev/null || fail "creating the project that plays the hosted one"
supavise fleet ensure-tenant "$OLD_REF" || fail "ensure-tenant"
host_alias "$OLD_REF"
OLD_PW=$(project_field "$OLD_REF" 'd["keys"]["db_password"]' --show-keys)
OLD_ANON=$(project_field "$OLD_REF" 'd["keys"]["anon_key"]' --show-keys)
OLD_SVC=$(project_field "$OLD_REF" 'd["keys"]["service_role_key"]' --show-keys)
[[ $(project_field "$OLD_REF" 'd["keys"]["jwt_secret"]' --show-keys) == "$HOSTED_SECRET" ]] || fail "the first project does not carry the secret it was created with"

log "daemon: supavise.service, a personal access token and a dashboard session"
start_daemon
claim_and_token
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
  -d '{"email":"smoke@example.com","password":"smoke-correct-horse-battery"}' | json_get 'd["access_token"]') || fail "dashboard sign-in"

OLD_DB_URL="postgresql://postgres.$OLD_REF:$OLD_PW@127.0.0.1:$P_SESSION/postgres?sslmode=disable"
for ((i = 0; i < 60; i++)); do
  psql "$OLD_DB_URL" -Atc 'select 1' >/dev/null 2>&1 && break
  sleep 2
done
psql "$OLD_DB_URL" -Atc 'select 1' >/dev/null || fail "the pooler does not serve the first project"
OLD_URL="http://$OLD_REF.api.$SUPAVISE_DOMAIN"
for ((i = 0; i < 60; i++)); do
  [[ $(http_code -H "apikey: $OLD_ANON" "$OLD_URL/auth/v1/settings") == 200 ]] && break
  sleep 1
done

log "seed the first project: tables with RLS, grants, a function, a trigger, a role, a cron job"
psql "$OLD_DB_URL" --variable ON_ERROR_STOP=1 -q <<'SQL' || fail "seeding the first project"
create role app_user login;
create extension if not exists pg_cron;
create table public.todos (
  id bigint generated always as identity primary key,
  owner uuid not null,
  title text not null,
  done boolean not null default false
);
alter table public.todos enable row level security;
create policy "owners read" on public.todos for select to authenticated using (owner = auth.uid());
create policy "anon reads done" on public.todos for select to anon using (done);
grant usage on schema public to anon, authenticated, app_user;
grant select on public.todos to anon, app_user;
grant select, insert, update, delete on public.todos to authenticated;
grant usage, select on all sequences in schema public to authenticated;
create function public.add_numbers(a int, b int) returns int language sql immutable as $$ select a + b $$;
grant execute on function public.add_numbers(int, int) to anon, authenticated;
create function public.shout() returns trigger language plpgsql as $$ begin new.title := upper(new.title); return new; end $$;
create trigger shout_todos before insert on public.todos for each row execute function public.shout();
select cron.schedule('nightly-cleanup', '0 3 * * *', $$delete from public.todos where done and id < 0$$);
SQL

log "Auth users with passwords; user 1 signs in so that a refresh token exists"
for n in 1 2; do
  [[ $(http_code -X POST -H "apikey: $OLD_ANON" -H 'Content-Type: application/json' \
    -d "{\"email\":\"user$n@example.com\",\"password\":\"correct-horse-$n-battery\"}" "$OLD_URL/auth/v1/signup") == 200 ]] || fail "sign-up of user $n"
done
REFRESH=$(curl -sS -m 30 -X POST -H "apikey: $OLD_ANON" -H 'Content-Type: application/json' \
  -d '{"email":"user1@example.com","password":"correct-horse-1-battery"}' "$OLD_URL/auth/v1/token?grant_type=password" | json_get 'd["refresh_token"]') || fail "sign-in of user 1"
psql "$OLD_DB_URL" --variable ON_ERROR_STOP=1 -q -c "insert into public.todos (owner, title, done) select u.id, 'task ' || g, g % 2 = 0 from auth.users u, generate_series(1, 5) g" || fail "inserting rows"
[[ $(psql "$OLD_DB_URL" -Atc 'select count(*) from public.todos') == 10 ]] || fail "rows in the first project"

log "a Storage object, and settings that differ from the defaults"
sapi() { curl -fsS -m 30 -H "apikey: $OLD_SVC" -H "Authorization: Bearer $OLD_SVC" "$@"; }
sapi -X POST -H 'Content-Type: application/json' -d '{"id":"avatars","name":"avatars","public":false}' "$OLD_URL/storage/v1/bucket" >/dev/null || fail "creating a bucket"
sapi -X POST -H 'Content-Type: text/plain' -H 'Cache-Control: max-age=7200' --data-binary 'hello from the hosted project' \
  "$OLD_URL/storage/v1/object/avatars/docs/hello%20world.txt" >/dev/null || fail "uploading an object"
papi PATCH "/v1/projects/$OLD_REF/config/auth" -H 'Content-Type: application/json' \
  -d '{"site_url":"https://app.example.test","uri_allow_list":"https://app.example.test/**"}' -o /dev/null -w '%{http_code}' | grep -qx 200 || fail "auth settings of the first project"
papi PATCH "/v1/projects/$OLD_REF/postgrest" -H 'Content-Type: application/json' -d '{"max_rows":777}' -o /dev/null -w '%{http_code}' | grep -qx 200 || fail "PostgREST settings of the first project"

# ---- the guide, step 1: the project on the node, with the keys of the first --------------------
export OLD_REF OLD_TOKEN=$PAT OLD_API="http://api.$SUPAVISE_DOMAIN" OLD_URL OLD_KEY=$OLD_SVC OLD_DB_URL
NEW_REF=$(rand_ref)
export NEW_REF NEW_NAME=moved NEW_SIZE=micro NEW_TOKEN=$PAT NEW_API="http://api.$SUPAVISE_DOMAIN"
export ANON_KEY=$OLD_ANON

# guide_block NAME: the fenced block after <!-- migrate-smoke: NAME --> in docs/guide.md.
guide_block() {
  python3 - "$1" "$REPO_ROOT/docs/guide.md" <<'PY'
import re, sys
name, path = sys.argv[1:3]
m = re.search(r"<!-- migrate-smoke: %s -->\n```\w*\n(.*?)\n```" % re.escape(name), open(path).read(), re.S)
if not m:
    sys.exit("docs/guide.md has no block for " + name)
print(m.group(1))
PY
}
# guide NAME [DIR]: runs the guide's block of that name in DIR (default $WORK/hosted).
guide() {
  local block
  block=$(guide_block "$1") || fail "$block"
  log "guide: $1"
  (cd "${2:-$WORK/hosted}" && bash -euo pipefail -c "$block") || fail "the guide's $1 commands failed"
}

guide jwt-secret
[[ $(<"$WORK/hosted/jwt-secret.txt") == "$HOSTED_SECRET" ]] || fail "GET .../postgrest did not return the secret the first project was made with"
guide legacy-keys
[[ $(<"$WORK/hosted/anon-key.txt") == "$OLD_ANON" && $(<"$WORK/hosted/service-role-key.txt") == "$OLD_SVC" ]] || fail "api-keys?reveal=true did not return the legacy keys"
host_alias "$NEW_REF"
guide create >"$WORK/create.out" 2>&1 || { cat "$WORK/create.out" >&2; fail "projects create"; }
cat "$WORK/create.out" >&2
[[ $(project_field "$NEW_REF" 'd["keys"]["jwt_secret"]' --show-keys) == "$HOSTED_SECRET" ]] || fail "the new project has another JWT secret"
[[ $(project_field "$NEW_REF" 'd["keys"]["anon_key"]' --show-keys) == "$OLD_ANON" ]] || fail "the new project's anon key is not the given one"
[[ $(project_field "$NEW_REF" 'd["keys"]["service_role_key"]' --show-keys) == "$OLD_SVC" ]] || fail "the new project's service_role key is not the given one"
NEW_PW=$(project_field "$NEW_REF" 'd["keys"]["db_password"]' --show-keys)
[[ $NEW_PW != "$OLD_PW" ]] || fail "the new project reuses a database password"
NEW_DB_URL="postgresql://postgres.$NEW_REF:$NEW_PW@127.0.0.1:$P_SESSION/postgres?sslmode=disable"
NEW_URL="http://$NEW_REF.api.$SUPAVISE_DOMAIN"
export NEW_DB_URL NEW_URL NEW_KEY=$OLD_SVC
for ((i = 0; i < 60; i++)); do
  psql "$NEW_DB_URL" -Atc 'select 1' >/dev/null 2>&1 && break
  sleep 2
done
psql "$NEW_DB_URL" -Atc 'select 1' >/dev/null || fail "the pooler does not serve the new project"

# ---- steps 3 and 4: dump, restore, cron --------------------------------------------------------
log "supabase init, so that the dump runs with Postgres 17 tools"
(cd "$WORK/hosted" && supabase init >"$WORK/init.out" 2>&1) || { cat "$WORK/init.out" >&2; log "supabase init failed; writing the one setting the dump reads"; }
if [[ ! -f $WORK/hosted/supabase/config.toml ]]; then
  mkdir -p "$WORK/hosted/supabase"
  printf '[db]\nmajor_version = 17\n' >"$WORK/hosted/supabase/config.toml"
fi
sed -i 's/^major_version = .*/major_version = 17/' "$WORK/hosted/supabase/config.toml"
grep -q '^major_version = 17' "$WORK/hosted/supabase/config.toml" || fail "supabase/config.toml does not say major_version = 17"

# The restore writes into the storage schema, which Storage sets up for a new project in the background.
for ((i = 0; i < 90; i++)); do
  [[ $(psql "$NEW_DB_URL" -Atc "select to_regclass('storage.objects') is not null") == t ]] && break
  sleep 2
done
[[ $(psql "$NEW_DB_URL" -Atc "select to_regclass('storage.objects') is not null") == t ]] || fail "Storage did not set up the new project's schema"

guide dump >"$WORK/dump.out" 2>&1 || { tail -40 "$WORK/dump.out" >&2; fail "supabase db dump"; }
for f in roles schema data; do [[ -s $WORK/hosted/$f.sql ]] || fail "$f.sql is empty"; done
grep -q 'CREATE TABLE IF NOT EXISTS "public"."todos"' "$WORK/hosted/schema.sql" || fail "the schema dump has no todos table"
grep -q 'COPY "public"."todos"' "$WORK/hosted/data.sql" || fail "the data dump has no COPY of todos (--use-copy)"
grep -q 'COPY "auth"."users"' "$WORK/hosted/data.sql" || fail "the data dump has no Auth users"
grep -q 'CREATE ROLE "app_user"' "$WORK/hosted/roles.sql" || fail "the role dump has no app_user"
guide restore >"$WORK/restore.out" 2>&1 || { tail -40 "$WORK/restore.out" >&2; fail "the restore"; }

nq() { psql "$NEW_DB_URL" -Atc "$1"; }
[[ $(nq 'select count(*) from public.todos') == 10 ]] || fail "rows of todos after the restore"
[[ $(nq 'select count(*) from auth.users') == 2 ]] || fail "Auth users after the restore"
[[ $(nq "select count(*) from pg_roles where rolname = 'app_user'") == 1 ]] || fail "the login role"
[[ $(nq "select count(*) from pg_policies where tablename = 'todos'") == 2 ]] || fail "policies"
[[ $(nq 'select public.add_numbers(2, 3)') == 5 ]] || fail "the function"
[[ $(nq "select count(*) from pg_trigger where tgname = 'shout_todos'") == 1 ]] || fail "the trigger"
[[ $(nq "select count(*) from pg_extension where extname = 'pg_cron'") == 1 ]] || fail "the schema dump does not create pg_cron"
log "pg_cron jobs in the restored database before the guide's cron step: $(nq 'select count(*) from cron.job')"
guide cron
[[ $(nq "select count(*) from cron.job where jobname = 'nightly-cleanup' and schedule = '0 3 * * *'") == 1 ]] || fail "the cron job does not exist exactly once after the guide's cron step"
nq "alter role app_user with password 'a-new-password-for-app-user'" >/dev/null || fail "setting a role password (step 4)"

# ---- step 5: Storage ---------------------------------------------------------------------------
guide storage >"$WORK/storage.out" 2>&1 || { cat "$WORK/storage.out" >&2; fail "the Storage loop"; }
grep -q 'copied avatars/docs/hello world.txt' "$WORK/storage.out" || { cat "$WORK/storage.out" >&2; fail "the Storage loop copied nothing"; }
got=$(curl -fsS -m 30 -D "$WORK/storage.headers" -H "apikey: $OLD_SVC" -H "Authorization: Bearer $OLD_SVC" \
  "$NEW_URL/storage/v1/object/authenticated/avatars/docs/hello%20world.txt") || fail "reading the object on the new project"
[[ $got == 'hello from the hosted project' ]] || fail "the copied object holds '$got'"
tr -d '\r' <"$WORK/storage.headers" | grep -qi '^content-type: text/plain' || fail "the copied object lost its content type"
[[ $(psql "$NEW_DB_URL" -Atc "select metadata->>'cacheControl' from storage.objects where bucket_id = 'avatars' and name = 'docs/hello world.txt'") == \
   $(psql "$OLD_DB_URL" -Atc "select metadata->>'cacheControl' from storage.objects where bucket_id = 'avatars' and name = 'docs/hello world.txt'") ]] \
  || fail "the copied object lost its cache control"

# ---- step 7: settings --------------------------------------------------------------------------
guide settings >"$WORK/settings.out" 2>&1 || { cat "$WORK/settings.out" >&2; fail "the settings loop"; }
[[ $(papi GET "/v1/projects/$NEW_REF/config/auth" | json_get 'd["site_url"]') == https://app.example.test ]] || fail "site_url was not copied"
[[ $(papi GET "/v1/projects/$NEW_REF/config/auth" | json_get 'd["uri_allow_list"]') == 'https://app.example.test/**' ]] || fail "uri_allow_list was not copied"
[[ $(papi GET "/v1/projects/$NEW_REF/postgrest" | json_get 'd["max_rows"]') == 777 ]] || fail "max_rows was not copied"

# ---- step 8: what the app sees ----------------------------------------------------------------
log "the first project's anon key against the new project: RLS through PostgREST"
rest_count() { # KEY BEARER QUERY: rows PostgREST answers on the new project, retried while its schema cache loads
  local n="" i
  for ((i = 0; i < 30; i++)); do
    n=$(curl -sS -m 30 -H "apikey: $1" -H "Authorization: Bearer $2" "$NEW_URL/rest/v1/todos?select=id&$3" | json_get 'len(d) if isinstance(d, list) else -1' 2>/dev/null || true)
    [[ $n =~ ^[0-9]+$ ]] && ((n >= 0)) && { echo "$n"; return; }
    sleep 2
  done
  echo "${n:--1}"
}
[[ $(rest_count "$OLD_ANON" "$OLD_ANON" 'done=eq.true') == 4 ]] || fail "anon sees $(rest_count "$OLD_ANON" "$OLD_ANON" 'done=eq.true') rows of todos on the new project, want the 4 done ones"
[[ $(rest_count "$OLD_ANON" "$OLD_ANON" 'done=eq.false') == 0 ]] || fail "anon sees rows its policy hides"
TOKEN=$(curl -sS -m 30 -X POST -H "apikey: $OLD_ANON" -H 'Content-Type: application/json' \
  -d '{"email":"user1@example.com","password":"correct-horse-1-battery"}' "$NEW_URL/auth/v1/token?grant_type=password" | json_get 'd["access_token"]') \
  || fail "password sign-in of a moved user on the new project"
[[ $(rest_count "$OLD_ANON" "$TOKEN" '') == 5 ]] || fail "a moved user does not see exactly their 5 rows"
curl -fsS -m 30 -X POST -H "apikey: $OLD_ANON" -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$REFRESH\"}" \
  "$NEW_URL/auth/v1/token?grant_type=refresh_token" | json_get 'd["user"]["email"]' | grep -qx user1@example.com || fail "the refresh token of the first project does not work on the new project"
# A key of the first project's secret with the new project's ref is the project's own key too.
[[ $(curl -s -o /dev/null -w '%{http_code}' -H "apikey: $(hs256 "$HOSTED_SECRET" "$NEW_REF" anon)" "$NEW_URL/rest/v1/todos?select=id") == 200 ]] || fail "a key signed with the secret and naming the new ref is refused"

# ---- the reverse direction: dump the node's project, restore into a third --------------------
log "the reverse direction: the same dumps from the new project, restored into a third"
THIRD_REF=$(rand_ref)
host_alias "$THIRD_REF"
supavise projects create --name third --size micro --ref "$THIRD_REF" --json >/dev/null || fail "creating the third project"
THIRD_PW=$(project_field "$THIRD_REF" 'd["keys"]["db_password"]' --show-keys)
THIRD_DB_URL="postgresql://postgres.$THIRD_REF:$THIRD_PW@127.0.0.1:$P_SESSION/postgres?sslmode=disable"
for ((i = 0; i < 60; i++)); do
  psql "$THIRD_DB_URL" -Atc 'select 1' >/dev/null 2>&1 && break
  sleep 2
done
psql "$THIRD_DB_URL" -Atc 'select 1' >/dev/null || fail "the pooler does not serve the third project"
cp -r "$WORK/hosted/supabase" "$WORK/reverse/supabase"
(
  export OLD_DB_URL=$NEW_DB_URL NEW_DB_URL=$THIRD_DB_URL
  guide dump "$WORK/reverse" >"$WORK/reverse-dump.out" 2>&1 || { tail -40 "$WORK/reverse-dump.out" >&2; exit 1; }
  grep -q 'COPY "public"."todos"' "$WORK/reverse/data.sql" || exit 2
  guide restore "$WORK/reverse" >"$WORK/reverse-restore.out" 2>&1 || { tail -40 "$WORK/reverse-restore.out" >&2; exit 3; }
) || fail "the reverse direction failed (status $?)"
tq() { psql "$THIRD_DB_URL" -Atc "$1"; }
[[ $(tq 'select count(*) from public.todos') == 10 && $(tq 'select count(*) from auth.users') == 2 && $(tq 'select public.add_numbers(2, 3)') == 5 ]] || fail "the third project does not hold what the second held"

# ---- the legacy JWT secret as Studio changes it, and the pgsodium root key ---------------------
log "PATCH /platform/projects/{ref}/config/secrets on the third project, then the pgsodium key"
THIRD_OLD_ANON=$(project_field "$THIRD_REF" 'd["keys"]["anon_key"]' --show-keys)
third_rest() { curl -s -o /dev/null -w '%{http_code}' -m 30 -H "apikey: $1" "http://$THIRD_REF.api.$SUPAVISE_DOMAIN/rest/v1/todos?select=id&done=eq.true"; }
for ((i = 0; i < 30; i++)); do [[ $(third_rest "$THIRD_OLD_ANON") == 200 ]] && break; sleep 2; done
[[ $(third_rest "$THIRD_OLD_ANON") == 200 ]] || fail "the third project does not serve its own anon key"
[[ $(japi GET "/platform/projects/$THIRD_REF/config/secrets/update-status" | json_get 'd["update_status"]') == None ]] || fail "update-status is not null before a change"
[[ $(japi PATCH "/platform/projects/$THIRD_REF/config/secrets" -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
  -d '{"jwt_secret":"short","change_tracking_id":"smoke-short"}') == 400 ]] || fail "a secret under 32 characters was accepted"
[[ $(japi PATCH "/platform/projects/$THIRD_REF/config/secrets" -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
  -d "{\"jwt_secret\":$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$HOSTED_SECRET"),\"change_tracking_id\":\"smoke-1\"}") == 200 ]] || fail "PATCH config/secrets with the hosted secret"
ST=$(japi GET "/platform/projects/$THIRD_REF/config/secrets/update-status")
[[ $(json_get 'd["update_status"]["status"]' <<<"$ST") == 1 && $(json_get 'd["update_status"]["change_tracking_id"]' <<<"$ST") == smoke-1 ]] || fail "update-status after the change: $ST"
[[ $(papi GET "/v1/projects/$THIRD_REF/postgrest" | json_get 'd["jwt_secret"]') == "$HOSTED_SECRET" ]] || fail "the project does not report the secret that was set"
for ((i = 0; i < 30; i++)); do [[ $(third_rest "$(hs256 "$HOSTED_SECRET" "$THIRD_REF" anon)") == 200 ]] && break; sleep 2; done
[[ $(third_rest "$(hs256 "$HOSTED_SECRET" "$THIRD_REF" anon)") == 200 ]] || fail "a key signed with the new secret is refused"
[[ $(third_rest "$THIRD_OLD_ANON") == 401 ]] || fail "the key of the old secret still works: $(third_rest "$THIRD_OLD_ANON")"
[[ $(third_rest "$OLD_ANON") == 401 ]] || fail "a key naming another project's ref was accepted (stored keys only: $(third_rest "$OLD_ANON"))"
NEW_ANON_THIRD=$(papi GET "/v1/projects/$THIRD_REF/api-keys?reveal=true" | json_get '[k["api_key"] for k in d if k["type"] == "legacy" and k["name"] == "anon"][0]') || fail "legacy keys of the third project"
[[ $(third_rest "$NEW_ANON_THIRD") == 200 ]] || fail "the anon key the API reports after the change is refused"
[[ $(project_field "$THIRD_REF" 'd["keys"]["jwt_secret"]' --show-keys) == "$HOSTED_SECRET" ]] || fail "the CLI reports another secret"
[[ $(papi GET "/v1/projects/$THIRD_REF/pgsodium" | json_get 'len(d["root_key"])') == 64 ]] || fail "GET pgsodium does not return a 64-character key"
[[ $(code PUT "/v1/projects/$THIRD_REF/pgsodium" "{\"root_key\":\"$(printf 'ab%.0s' {1..32})\"}") == 501 ]] || fail "PUT pgsodium was not refused"
[[ $(code GET "/v1/projects/$THIRD_REF/config/auth/signing-keys/legacy") == 404 ]] || fail "the legacy signing key is not 404"

log "migrate-smoke passed"

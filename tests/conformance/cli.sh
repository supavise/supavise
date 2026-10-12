#!/usr/bin/env bash
# The Supabase CLI against a node, as a customer uses it: link, migrations, db push through the
# pooler, gen types, functions deploy, secrets. Run by run.sh; it needs a node that is up.
#
# The CLI reaches a node through a profile file (--profile), which `supavise api profile` prints:
# the Management API URL, the project host and the pooler's registrable domain. That is the
# documented way to point the CLI at a self-hosted Management API, and it is what a node's
# owner is told to do. (The CLI also reads SUPABASE_PROFILE, with the same value.) No Docker:
# functions are bundled by the node (`functions deploy --use-api`); Docker-based bundling is
# covered by tests/linux/functions-smoke.sh.
#
# Environment: SUPABASE_CLI (the binary), PROFILE (the file), API_URL, PAT, REF, DBPASS,
# PROJECT_URL, WORK (scratch), SUPABASE_CLI_VERSION (checked against `supabase --version`).
# Every step runs; the exit status is non-zero when any failed.
set -uo pipefail

: "${SUPABASE_CLI:?}" "${PROFILE:?}" "${API_URL:?}" "${PAT:?}" "${REF:?}" "${DBPASS:?}" "${PROJECT_URL:?}" "${WORK:?}"
PROJ=$WORK/cli-project
OUT=$WORK/cli-out
mkdir -p "$PROJ" "$OUT" "$WORK/cli-home"
FAILS=0
N=0

log() { printf '%s cli: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
bad() { log "FAIL: $*"; FAILS=$((FAILS + 1)); }

# cli ARGS...: the CLI in the project directory with the node's profile and the token in the
# environment (never in argv). The keyring and telemetry are off; there is no terminal. The CLI uses
# SUPABASE_DB_PASSWORD as given, so a step on a branch sets CLI_DBPASS to the branch's own password.
cli() {
  (cd "$PROJ" && env HOME="$WORK/cli-home" SUPABASE_ACCESS_TOKEN="$PAT" SUPABASE_DB_PASSWORD="${CLI_DBPASS:-$DBPASS}" \
    SUPABASE_NO_KEYRING=1 DO_NOT_TRACK=1 SUPABASE_DISABLE_UPDATE_CHECK=1 \
    "$SUPABASE_CLI" --profile "$PROFILE" "$@" </dev/null)
}

# step NAME ARGS...: runs the CLI, keeps its output in $OUT/<n>-<name>.log and sets LAST to the
# path. A non-zero exit is a failure and its output is printed.
step() {
  local name=$1; shift
  N=$((N + 1))
  LAST=$OUT/$(printf '%02d' "$N")-$name.log
  log "supabase $*"
  if ! cli "$@" >"$LAST" 2>&1; then
    bad "supabase $* exited with status $?"
    sed 's/^/    /' "$LAST" | tail -40 >&2
    return 1
  fi
}

# has FILE TEXT DESCRIPTION: the file contains the text (fixed string).
has() {
  grep -qF -- "$2" "$1" || { bad "$3 (looked for '$2' in $(basename "$1"))"; sed 's/^/    /' "$1" | tail -20 >&2; return 1; }
}

mapi() { # METHOD PATH [curl args]: the Management API
  local m=$1 p=$2; shift 2
  curl -sS -m 60 -X "$m" -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' "$@" "$API_URL$p"
}
query_on() { # REF SQL: rows as JSON through database/query
  mapi POST "/v1/projects/$1/database/query" -d "$(python3 -c 'import json,sys; print(json.dumps({"query": sys.argv[1]}))' "$2")"
}
query() { query_on "$REF" "$1"; } # SQL: rows of the project

step version --version && has "$LAST" "${SUPABASE_CLI_VERSION:-.}" "the CLI is not the pinned version"

step init init --force
[[ -f $PROJ/supabase/config.toml ]] || bad "init wrote no supabase/config.toml"

step projects projects list -o json && has "$LAST" "$REF" "projects list does not show the project"

step link link --project-ref "$REF" --yes
if [[ $(cat "$PROJ/supabase/.temp/project-ref" 2>/dev/null) != "$REF" ]]; then bad "link did not record the project ref"; fi
POOLER_URL=$(cat "$PROJ/supabase/.temp/pooler-url" 2>/dev/null || true)
# The file holds the user, host and port of the pooler (the CLI drops the password placeholder).
[[ $POOLER_URL == *"postgres.$REF"*"@pooler."* ]] || bad "link recorded no pooler URL for $REF (got '$POOLER_URL')"

# The output holds the service_role key and the log directory is uploaded, so this step keeps
# no log and prints nothing of the output.
N=$((N + 1))
log "supabase projects api-keys --project-ref $REF"
if keys=$(cli projects api-keys --project-ref "$REF" 2>&1); then
  grep -qF -- "anon" <<<"$keys" || bad "projects api-keys does not list the anon key"
else
  bad "supabase projects api-keys exited non-zero"
fi
unset keys

# A migration, pushed through the pooler.
step migration-new migration new conf_widgets
MIG=$(ls "$PROJ"/supabase/migrations/*_conf_widgets.sql 2>/dev/null | head -1)
if [[ -z $MIG ]]; then
  bad "migration new wrote no file"
else
  VERSION=$(basename "$MIG" | cut -d_ -f1)
  cat >"$MIG" <<SQL
create table public.conf_widgets (id int primary key, name text not null);
insert into public.conf_widgets values (1, 'sprocket'), (2, 'gear');
-- The Data API may read it. The security advisor reports a table without RLS only when anon or
-- authenticated can read it, and a project does not have to grant that to tables by default.
grant select on public.conf_widgets to anon;
SQL
  step migration-list-before migration list --linked
  line=$(grep -F "$VERSION" "$LAST" | head -1)
  [[ $(grep -o "$VERSION" <<<"$line" | wc -l) -eq 1 ]] || bad "before the push the migration should be local only: '$line'"

  step db-push db push --yes && has "$LAST" "Finished supabase db push" "db push did not finish"
  rows=$(query "select name from public.conf_widgets order by id")
  [[ $rows == *sprocket* && $rows == *gear* ]] || bad "the pushed migration did not create the rows: $rows"
  recorded=$(query "select version from supabase_migrations.schema_migrations")
  [[ $recorded == *"$VERSION"* ]] || bad "the migration is not recorded in supabase_migrations.schema_migrations: $recorded"

  step migration-list-after migration list --linked
  line=$(grep -F "$VERSION" "$LAST" | head -1)
  [[ $(grep -o "$VERSION" <<<"$line" | wc -l) -eq 2 ]] || bad "after the push the migration should be on both sides: '$line'"

  step db-push-again db push --yes && has "$LAST" "up to date" "a second db push should find nothing to do"

  # The advisors. The migration made a table in public without row level security, which hosted's
  # Security Advisor reports as an error: `db advisors --linked` reads GET /v1/projects/{ref}/advisors/security
  # and /performance, and prints the lints it gets as JSON. The Auth settings add their own lints.
  step db-advisors-security db advisors --linked --type security --level error \
    && has "$LAST" "rls_disabled_in_public" "db advisors does not report the table without RLS" \
    && has "$LAST" "conf_widgets" "the finding does not name the migration's table"
  step db-advisors-performance db advisors --linked --type performance --level info \
    && has "$LAST" "auth_db_connections_absolute" "db advisors --type performance lacks the lint of the Auth settings"
  if grep -qF rls_disabled_in_public "$LAST"; then bad "the performance advisor reports a security lint"; fi
  # --fail-on turns an error-level lint into a failing exit status.
  N=$((N + 1))
  log "supabase db advisors --linked --type security --level error --fail-on error (must fail)"
  if cli db advisors --linked --type security --level error --fail-on error >"$OUT/$(printf '%02d' "$N")-db-advisors-fail-on.log" 2>&1; then
    bad "db advisors --fail-on error exited 0 with an error-level lint present"
  fi
  # The endpoint's own shape, which the CLI and the MCP server read: the table's finding is an
  # ERROR in the SECURITY category with its schema and table in metadata, and the other
  # category's list does not hold it.
  # The answers go through files: a project with many findings outgrows the 128 KiB that Linux
  # allows for one command-line argument.
  mapi GET "/v1/projects/$REF/advisors/security" >"$OUT/advisors-security.json"
  mapi GET "/v1/projects/$REF/advisors/performance" >"$OUT/advisors-performance.json"
  python3 - "$OUT/advisors-security.json" "$OUT/advisors-performance.json" <<'PY' || bad "GET /v1/projects/{ref}/advisors/{security,performance} do not answer as hosted does (see $OUT/advisors-*.json)"
import json, sys
sec, perf = (json.load(open(a))["lints"] for a in sys.argv[1:3])
hits = [l for l in sec if l["name"] == "rls_disabled_in_public" and l.get("metadata", {}).get("name") == "conf_widgets"]
assert len(hits) == 1, "no rls_disabled_in_public for public.conf_widgets: %r" % [l["name"] for l in sec]
l = hits[0]
assert l["level"] == "ERROR" and l["categories"] == ["SECURITY"] and l["facing"] == "EXTERNAL", l
assert l["metadata"]["schema"] == "public" and l["metadata"]["type"] == "table", l
assert l["cache_key"] == "rls_disabled_in_public_public_conf_widgets", l
assert l["remediation"].startswith("https://supabase.com/docs/guides/database/database-linter?lint=0013"), l
assert all("SECURITY" in x["categories"] for x in sec) and all("PERFORMANCE" in x["categories"] for x in perf)
assert not [x for x in perf if x["name"] == "rls_disabled_in_public"]
PY
fi

# Types from the Management API.
step gen-types gen types typescript --project-id "$REF" && has "$LAST" "conf_widgets" "gen types does not include the pushed table"
step gen-types-linked gen types typescript --linked && has "$LAST" "conf_widgets" "gen types --linked does not include the pushed table"

# A function, bundled by the node, then called through the project's gateway.
SLUG=cliconf
mkdir -p "$PROJ/supabase/functions/$SLUG"
cat >"$PROJ/supabase/functions/$SLUG/index.ts" <<'TS'
Deno.serve(() => new Response(JSON.stringify({ from: 'cli', secret: Deno.env.get('CONF_CLI_SECRET') ?? null }), { headers: { 'Content-Type': 'application/json' } }))
TS
step secrets-set secrets set CONF_CLI_SECRET=from-the-cli
step secrets-list secrets list && has "$LAST" "CONF_CLI_SECRET" "secrets list does not show the secret"
step functions-deploy functions deploy "$SLUG" --use-api --project-ref "$REF"
step functions-list functions list --project-ref "$REF" && has "$LAST" "$SLUG" "functions list does not show the function"
ANON=$(mapi GET "/v1/projects/$REF/api-keys?reveal=true" | python3 -c 'import json,sys; print([k["api_key"] for k in json.load(sys.stdin) if k.get("name") == "anon"][0])')
answer=""
for ((i = 0; i < 60; i++)); do
  answer=$(curl -sS -m 20 -H "Authorization: Bearer $ANON" -H "apikey: $ANON" "$PROJECT_URL/functions/v1/$SLUG" 2>/dev/null || true)
  [[ $answer == *'"from":"cli"'* && $answer == *from-the-cli* ]] && break
  sleep 2
done
[[ $answer == *'"from":"cli"'* ]] || bad "the deployed function does not answer: $answer"
[[ $answer == *from-the-cli* ]] || bad "the function does not see the secret set with the CLI: $answer"
step functions-delete functions delete "$SLUG" --project-ref "$REF" --yes
step secrets-unset secrets unset CONF_CLI_SECRET --yes
step secrets-list-after secrets list
if grep -qF CONF_CLI_SECRET "$LAST"; then bad "secrets unset left the secret in the list"; fi

# ---- branches ------------------------------------------------------------------------------------------------------
# The branches commands of the CLI (create, list, get -o env, update, pause, unpause, delete, disable), `link <branch name>`
# (which keeps the PARENT cached, so that `branches` still works afterwards), and the commands that take a branch's own
# ref: db push (through the pooler, after linking the branch), functions deploy, secrets set and config push with a
# [remotes] block. The branch is created after the migration above, so it replays that history.
BR=clibr
BR2=clibr2
jpy() { python3 -c 'import json,sys; d=json.load(sys.stdin); print('"$1"')'; } # a Python expression over the JSON on stdin
# cli_quiet ARGS...: the CLI with its output kept in $QUIET and never on disk (a branch's environment holds its database
# password and JWT secret, and the log directory is uploaded).
cli_quiet() {
  N=$((N + 1))
  log "supabase $*"
  if ! QUIET=$(cli "$@" 2>&1); then bad "supabase ${*:1:3} exited non-zero"; QUIET=""; return 1; fi
}
# branch_ready NAME: waits for the branch's create to finish and its project to run; sets BID and BREF.
branch_ready() {
  local name=$1 j st="" i
  for ((i = 0; i < 180; i++)); do
    j=$(mapi GET "/v1/projects/$REF/branches/$name" 2>/dev/null || true)
    st=$(jpy 'd["status"]' <<<"$j" 2>/dev/null || true)
    [[ $st == FUNCTIONS_DEPLOYED ]] && break
    [[ $st == *FAILED ]] && { bad "branch $name is $st"; return 1; }
    sleep 3
  done
  [[ $st == FUNCTIONS_DEPLOYED ]] || { bad "branch $name did not finish its create (status '$st')"; return 1; }
  BID=$(jpy 'd["id"]' <<<"$j")
  BREF=$(jpy 'd["project_ref"]' <<<"$j")
  for ((i = 0; i < 60; i++)); do
    [[ $(mapi GET "/v1/branches/$BID" | jpy 'd["status"]' 2>/dev/null || true) == ACTIVE_HEALTHY ]] && return 0
    sleep 2
  done
  bad "the project of branch $name does not run"
  return 1
}

if step branches-create branches create "$BR" --project-ref "$REF" && branch_ready "$BR"; then
  step branches-list branches list --project-ref "$REF" -o json && has "$LAST" "$BR" "branches list does not show the branch"
  has "$LAST" "$BREF" "branches list does not show the branch's project ref"

  # -o env needs the branch's api-keys and pooler config (the CLI reads both for the ref of the branch)
  if cli_quiet branches get "$BR" --project-ref "$REF" -o env; then
    for want in POSTGRES_URL= POSTGRES_URL_NON_POOLING= SUPABASE_URL= SUPABASE_JWT_SECRET= SUPABASE_ANON_KEY= SUPABASE_SERVICE_ROLE_KEY=; do
      grep -qF -- "$want" <<<"$QUIET" || bad "branches get -o env has no $want"
    done
    grep -qF -- "$BREF" <<<"$QUIET" || bad "branches get -o env does not name the branch's project ref"
    grep -qF -- "******" <<<"$QUIET" && bad "branches get -o env masks a value: the CLI got no password or JWT secret from the node"
  fi
  QUIET=""
  if cli_quiet branches get "$BR" --project-ref "$REF"; then
    grep -qF -- "$BREF" <<<"$QUIET" || true # the table has host, port, user, password and JWT secret; its shape is the CLI's
  fi
  QUIET=""

  # persistent, a notify URL, and a rename back
  step branches-update branches update "$BR" --persistent --notify-url "https://example.com/supavise-conformance" --project-ref "$REF"
  upd=$(mapi GET "/v1/projects/$REF/branches/$BR")
  [[ $(jpy 'd["persistent"]' <<<"$upd") == True ]] || bad "branches update --persistent did not persist: $upd"
  [[ $(jpy 'd.get("notify_url")' <<<"$upd") == "https://example.com/supavise-conformance" ]] || bad "branches update --notify-url did not store the URL: $upd"
  step branches-update-back branches update "$BR" --no-persistent --project-ref "$REF"
  [[ $(mapi GET "/v1/projects/$REF/branches/$BR" | jpy 'd["persistent"]') == False ]] || bad "branches update --no-persistent did not clear persistent"

  # pause and unpause: the CLI calls POST /v1/projects/{branch ref}/pause and /restore
  step branches-pause branches pause "$BR" --project-ref "$REF"
  inactive=0
  for ((i = 0; i < 90; i++)); do
    [[ $(mapi GET "/v1/branches/$BID" | jpy 'd["status"]' 2>/dev/null || true) == INACTIVE ]] && { inactive=1; break; }
    sleep 2
  done
  [[ $inactive -eq 1 ]] || bad "branches pause did not pause the branch's project"
  step branches-unpause branches unpause "$BR" --project-ref "$REF"
  running=0
  for ((i = 0; i < 120; i++)); do
    [[ $(mapi GET "/v1/branches/$BID" | jpy 'd["status"]' 2>/dev/null || true) == ACTIVE_HEALTHY ]] && { running=1; break; }
    sleep 2
  done
  [[ $running -eq 1 ]] || bad "branches unpause did not bring the branch's project back"

  # Linking a branch by NAME: the branch's project becomes the linked ref, the parent stays cached (GET /v1/projects/{branch}
  # answers 404, which the CLI takes as "this is a branch"), and `branches` still works without --project-ref.
  # A branch has a database password of its own (the parent's does not open it): link and db push use it.
  CLI_DBPASS=$(mapi GET "/v1/branches/$BID" | jpy 'd["db_pass"]' 2>/dev/null || true)
  [[ -n $CLI_DBPASS && $CLI_DBPASS != None ]] || bad "GET /v1/branches/<id> returned no db_pass for the branch"
  step link-branch link "$BR" --yes
  [[ $(cat "$PROJ/supabase/.temp/project-ref" 2>/dev/null) == "$BREF" ]] || bad "link <branch> did not record the branch's project ref"
  cached=$(cat "$PROJ/supabase/.temp/linked-project.json" 2>/dev/null || true)
  [[ $cached == *"$REF"* && $cached != *"$BREF"* ]] || bad "link <branch> replaced the cached parent: $cached"
  step branches-list-linked branches list -o json && has "$LAST" "$BR" "branches list after link <branch> does not list the branch (the parent is not cached)"

  # db push to the linked branch, through the pooler: a migration only the branch has
  step migration-new-branch migration new conf_branch_only
  BMIG=$(ls "$PROJ"/supabase/migrations/*_conf_branch_only.sql 2>/dev/null | head -1)
  if [[ -z $BMIG ]]; then bad "migration new wrote no file for the branch"; else
    printf 'create table public.conf_branch_only (id int primary key);\n' >"$BMIG"
    step db-push-branch db push --yes && has "$LAST" "Finished supabase db push" "db push to the branch did not finish"
    present=$(query_on "$BREF" "select to_regclass('public.conf_branch_only') is not null as present")
    [[ $present == *true* ]] || bad "the branch has no table from the pushed migration: $present"
    onparent=$(query "select to_regclass('public.conf_branch_only') is not null as present")
    [[ $onparent == *false* ]] || bad "db push to the branch reached the parent: $onparent"
  fi
  unset CLI_DBPASS

  # functions deploy, secrets set and config push with a [remotes] block, each with the branch's own ref
  BSLUG=cliconfbr
  mkdir -p "$PROJ/supabase/functions/$BSLUG"
  cat >"$PROJ/supabase/functions/$BSLUG/index.ts" <<'TS'
Deno.serve(() => new Response(JSON.stringify({ from: 'cli-branch' }), { headers: { 'Content-Type': 'application/json' } }))
TS
  step functions-deploy-branch functions deploy "$BSLUG" --use-api --project-ref "$BREF"
  step functions-list-branch functions list --project-ref "$BREF" && has "$LAST" "$BSLUG" "functions list does not show the function deployed to the branch"
  step functions-list-parent functions list --project-ref "$REF"
  if grep -qF "$BSLUG" "$LAST"; then bad "a function deployed to the branch is on the parent"; fi
  step secrets-set-branch secrets set CONF_BRANCH_SECRET=branch-only --project-ref "$BREF"
  step secrets-list-branch secrets list --project-ref "$BREF" && has "$LAST" "CONF_BRANCH_SECRET" "secrets list does not show the secret set on the branch"
  step secrets-list-parent secrets list --project-ref "$REF"
  if grep -qF CONF_BRANCH_SECRET "$LAST"; then bad "a secret set on the branch is on the parent"; fi

  cp "$PROJ/supabase/config.toml" "$WORK/config.toml.orig"
  cat >"$PROJ/supabase/config.toml" <<TOML
project_id = "cli-conformance"

[api]
enabled = true

[auth]
enabled = true
site_url = "http://localhost:3000"

[remotes.$BR]
project_id = "$BREF"

[remotes.$BR.auth]
site_url = "http://cli-branch.example.test"
TOML
  step config-push-branch config push --project-ref "$BREF" --yes
  [[ $(mapi GET "/v1/projects/$BREF/config/auth" | jpy 'd.get("site_url")') == "http://cli-branch.example.test" ]] || bad "config push did not set the branch's site_url from its [remotes] block"
  [[ $(mapi GET "/v1/projects/$REF/config/auth" | jpy 'd.get("site_url")') != "http://cli-branch.example.test" ]] || bad "config push to the branch changed the parent's site_url"
  cp "$WORK/config.toml.orig" "$PROJ/supabase/config.toml"

  # back to the parent, then delete the branch by name
  step link-parent link --project-ref "$REF" --yes
  step branches-delete branches delete "$BR" --project-ref "$REF" --yes
  gone=0
  for ((i = 0; i < 90; i++)); do
    [[ $(mapi GET "/v1/projects/$REF/branches" | jpy 'any(b["name"] == "'"$BR"'" for b in d)' 2>/dev/null || true) == False ]] && { gone=1; break; }
    sleep 2
  done
  [[ $gone -eq 1 ]] || bad "branches delete left the branch in the list"
fi

# `branches disable` removes every preview branch of the project (it is unlisted in the CLI's help)
if step branches-create-2 branches create "$BR2" --project-ref "$REF" && branch_ready "$BR2"; then
  step branches-disable branches disable --project-ref "$REF" --yes
  gone=0
  for ((i = 0; i < 90; i++)); do
    [[ $(mapi GET "/v1/projects/$REF/branches" | jpy 'len([b for b in d if not b.get("is_default")])' 2>/dev/null || true) == 0 ]] && { gone=1; break; }
    sleep 2
  done
  [[ $gone -eq 1 ]] || bad "branches disable left a preview branch"
fi

step unlink unlink --yes

log "$N steps, $FAILS failure(s)"
exit $((FAILS > 0))

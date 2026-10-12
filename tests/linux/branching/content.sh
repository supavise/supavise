# shellcheck shell=bash
#!/usr/bin/env bash
# CI only: what a branch carries, on real clusters. Sourced by tests/linux/branching-xfs.sh, which has built
# "$work/branching.test" and unpacked the artifacts into "$unpacked"; it calls branching_content_check once,
# after branching_runs_check.
#
# TestServe (internal/branching/serve_test.go) starts a node on real clusters with a parent project, the
# Management API and the Edge Functions store, and writes {api_url, pat, parent_ref, state_dir, psql,
# parent_admin_dsn, ...} to a file. This script drives that API over HTTP and checks:
#
#   - a parent built only with execute_sql (a table, an RLS policy, an index, an extension, a trigger on
#     auth.users, a policy on a table of a managed schema) and with no migration history gives a branch with
#     all of them; the parent's history now holds the baseline, recorded once and not run on the parent; a
#     second branch does not dump again;
#   - reset replays the branch's own history: an unknown version is refused before anything is removed, the
#     tracked migration of the branch survives, an untracked table and its rows do not, version 0 gives an empty
#     schema;
#   - the dashboard flow of Studio's merge page (GET diff, PUT it as a migration, POST merge) next to the CLI
#     flow: the same shape reaches the parent, production's own drift is not reverted, the histories agree, a
#     branch made from the merged parent and a reset of the merged branch both replay without creating a table
#     twice, a second merge is a no-op, and a merge fails with a reason (and runs nothing) when the branch moved
#     after the diff was recorded; the row a tracked migration inserted reaches the parent after the diff (and
#     the replays), and a recorded diff that a newer migration passed is ignored by the merge;
#   - push takes the parent's untracked changes into a branch and keeps the branch's own;
#   - Edge Functions are copied at create with their digest, version, update time and verify_jwt; secrets are the
#     create body's alone; a push overwrites the branch's copy; a merge deploys the branch's new and changed
#     functions, deletes nothing, and fails with a reason when both sides changed one;
#   - saved settings (a provider's client id, MFA, sign-up, a Postgres setting) reach the branch while site_url stays its own, and
#     the credentials do not (the OAuth secret and its switch, the SMTP server, user and password); a schema the parent exposes is
#     exposed by an empty branch after its replay made the schema;
#   - a branch with data does not inherit the parent's sizing lines from postgresql.auto.conf.
#
# The stack has no Storage service, so the policy on a managed table is one on auth.users; settings-smoke.sh
# checks storage.objects on a full node. The state lives on the XFS of branching-xfs.sh (reflink clones).
# Needs: python3, the Go toolchain (TestServe builds the binary), no root, no systemd.

branching_content_check() {
  local file=$work/content-stack.json log=$work/content-serve.log pid i
  rm -f "$file"
  (
    cd internal/branching
    SUPAVISE_BRANCH_SERVE_FILE=$file SUPAVISE_TEST_UNPACKED=$unpacked SUPAVISE_BRANCH_SERVE_STATE_DIR=${CONTENT_STATE_DIR:-/mnt/supavise-xfs} \
      exec "$work/branching.test" -test.run '^TestServe$' -test.v -test.timeout 40m
  ) >"$log" 2>&1 &
  pid=$!
  for ((i = 0; i < 300; i++)); do
    [[ -s "$file" ]] && break
    kill -0 "$pid" 2>/dev/null || { echo "content: the stack exited early" >&2; tail -n 40 "$log" >&2; return 1; }
    sleep 1
  done
  [[ -s "$file" ]] || { echo "content: the stack did not come up in 300 s" >&2; tail -n 40 "$log" >&2; kill "$pid" 2>/dev/null || true; return 1; }
  local status=0
  CONTENT_STACK=$file python3 - <<'PY' || status=$?
import json, os, subprocess, sys, time, urllib.error, urllib.request, uuid

stack = json.load(open(os.environ["CONTENT_STACK"]))
API, PAT, PARENT = stack["api_url"], stack["pat"], stack["parent_ref"]
PSQL, ADMIN_DSN, STATE = stack["psql"], stack["parent_admin_dsn"], stack["state_dir"]

failures = []


def check(cond, what):
    print(("ok   " if cond else "FAIL ") + what, flush=True)
    if not cond:
        failures.append(what)
    return cond


def call(method, path, body=None, headers=None, raw=False, data=None, ctype="application/json"):
    if data is None and body is not None:
        data = json.dumps(body).encode()
    req = urllib.request.Request(API + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + PAT)
    if data is not None:
        req.add_header("Content-Type", ctype)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            payload, code = r.read(), r.status
    except urllib.error.HTTPError as e:
        payload, code = e.read(), e.code
    if raw:
        return code, payload.decode(errors="replace")
    try:
        return code, (json.loads(payload) if payload else None)
    except ValueError:
        return code, payload.decode(errors="replace")


def sql(ref, q):
    code, rows = call("POST", f"/v1/projects/{ref}/database/query", {"query": q})
    if code != 201:
        raise RuntimeError(f"sql on {ref} answered {code}: {rows}\n{q}")
    return rows


def names(ref, q, col="n"):
    return sorted(r[col] for r in sql(ref, q))


def tables(ref):
    return names(ref, "select relname n from pg_class where relnamespace = 'public'::regnamespace and relkind in ('r','p') order by 1")


def wait_status(parent, name, want, timeout=300):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        code, b = call("GET", f"/v1/projects/{parent}/branches/{name}")
        if code == 200:
            last = b
            if b["status"] == want:
                return b
            if b["status"] in ("MIGRATIONS_FAILED", "FUNCTIONS_FAILED") and want not in ("MIGRATIONS_FAILED", "FUNCTIONS_FAILED"):
                break
        time.sleep(1)
    check(False, f"branch {name} reached {want} (last: {last and last.get('status')} {last and last.get('supavise_detail')})")
    return last or {}


made = []  # the branches created, for the clean-up before the branch with data (the stack allows 10 per project)


def new_branch(name, **kw):
    code, br = call("POST", f"/v1/projects/{PARENT}/branches", {"branch_name": name, **kw})
    if code != 201:
        check(False, f"create {name} answers 201 (got {code} {br})")
        return None
    made.append(br)
    b = wait_status(PARENT, name, "FUNCTIONS_DEPLOYED")
    return b if b.get("status") == "FUNCTIONS_DEPLOYED" else None


def act(bid, verb, body=None, timeout=300):
    """Start merge, reset or push and wait for its run to finish; returns (http code, the run or the answer)."""
    code, out = call("POST", f"/v1/branches/{bid}/{verb}", body if body is not None else {})
    if code != 201:
        return code, out
    run_id = out["workflow_run_id"]
    owner = PARENT if verb == "merge" else bid_ref(bid)
    end = time.time() + timeout
    while time.time() < end:
        c, run = call("GET", f"/v1/projects/{owner}/actions/{run_id}")
        steps = run["run_steps"] if c == 200 else []
        if steps and (all(s["status"] in ("EXITED", "PAUSED") for s in steps) or any(s["status"] == "DEAD" for s in steps)):
            return 201, run
        time.sleep(1)
    return 201, None


_refs = {}


def bid_ref(bid):
    if bid not in _refs:
        code, b = call("GET", f"/v1/branches/{bid}")
        _refs[bid] = b["ref"]
    return _refs[bid]


def psql(dsn, q):
    r = subprocess.run([PSQL, dsn, "-Atc", q], capture_output=True, text=True, timeout=60)
    if r.returncode != 0:
        raise RuntimeError(r.stderr)
    return r.stdout


def multipart(fields, files):
    boundary = uuid.uuid4().hex
    out = b""
    for name, value in fields.items():
        out += f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"\r\nContent-Type: application/json\r\n\r\n{value}\r\n'.encode()
    for name, filename, content in files:
        out += f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"; filename="{filename}"\r\nContent-Type: application/octet-stream\r\n\r\n'.encode() + content + b"\r\n"
    out += f"--{boundary}--\r\n".encode()
    return out, f"multipart/form-data; boundary={boundary}"


def deploy(ref, slug, body, verify=True):
    data, ctype = multipart({"metadata": json.dumps({"entrypoint_path": "index.ts", "name": slug, "verify_jwt": verify})},
                            [("file", "index.ts", f'Deno.serve(() => new Response("{body}"))\n'.encode())])
    return call("POST", f"/v1/projects/{ref}/functions/deploy?slug={slug}", data=data, ctype=ctype)


def fn(ref, slug):
    code, f = call("GET", f"/v1/projects/{ref}/functions/{slug}")
    return f if code == 200 else None


tag = uuid.uuid4().hex[:6]

# ==== 1. a parent built only through execute_sql =======================================================
# The history is emptied (the stack's parent was made with one migration), and everything below is SQL.
sql(PARENT, "delete from supabase_migrations.schema_migrations")
sql(PARENT, """
  create extension if not exists pg_trgm with schema extensions;
  create table public.todos (id bigserial primary key, title text not null, owner uuid default gen_random_uuid());
  alter table public.todos enable row level security;
  create policy todos_read on public.todos for select to authenticated using (true);
  create index todos_title_trgm on public.todos using gin (title extensions.gin_trgm_ops);
  create function public.on_signup() returns trigger language plpgsql security definer as $$ begin return new; end $$;
  create trigger on_auth_user_created after insert on auth.users for each row execute function public.on_signup();
  create policy managed_read on auth.users for select to authenticated using (true);
  grant select on public.todos to anon;
""")
check(call("GET", f"/v1/projects/{PARENT}/database/migrations")[1] == [], "the parent has no migration history")

a = new_branch(f"baseline-{tag}")
if a:
    ra = a["project_ref"]
    t = tables(ra)
    check(t == ["items", "todos"], f"the branch has every table the SQL editor made ({t})")
    check(names(ra, "select polname n from pg_policy") == ["managed_read", "todos_read"], "the branch has the RLS policy and the policy on a managed table")
    check(names(ra, "select tgname n from pg_trigger where not tgisinternal") == ["on_auth_user_created"], "the branch has the trigger on auth.users")
    check("pg_trgm" in names(ra, "select extname n from pg_extension"), "the branch has the extension")
    check(names(ra, "select indexname n from pg_indexes where schemaname = 'public' and tablename = 'todos' order by 1") == ["todos_pkey", "todos_title_trgm"], "the branch has the index")
    pm = call("GET", f"/v1/projects/{PARENT}/database/migrations")[1]
    check(len(pm) == 1 and pm[0].get("name") == "remote_schema", f"the parent's history was initialised with the baseline ({pm})")
    check(call("GET", f"/v1/projects/{ra}/database/migrations")[1] == pm, "the branch's history is the parent's")
    check(sql(PARENT, "select count(*) c from supabase_migrations.schema_migrations")[0]["c"] == 1, "the baseline was recorded once on the parent")
    code, d = call("GET", f"/v1/branches/{a['id']}/diff", raw=True)
    check(code == 200 and d == "", f"a fresh branch has an empty diff ({code} {d!r})")
    # Another branch replays the recorded baseline: nothing is dumped again.
    a2 = new_branch(f"baseline2-{tag}")
    check(a2 is not None and len(call("GET", f"/v1/projects/{PARENT}/database/migrations")[1]) == 1, "a second branch does not dump again")

    # ==== 2. reset replays the branch's own history ==================================================
    code, _ = call("POST", f"/v1/projects/{ra}/database/migrations", {"name": "tracked_only", "query": "create table public.tracked_only (id int primary key)"})
    check(code == 200, f"a tracked migration applies on the branch ({code})")
    sql(ra, "create table public.scratch (id int); insert into public.items values (1000001, 'scratch row')")
    code, out = call("POST", f"/v1/branches/{a['id']}/reset", {"migration_version": "29990101000000"})
    check(code == 400 and "migration_version" in json.dumps(out), f"a reset to an unknown version is refused before anything is removed ({code} {out})")
    check("scratch" in tables(ra), "the refused reset left the branch alone")
    code, run = act(a["id"], "reset")
    wait_status(PARENT, a["name"], "FUNCTIONS_DEPLOYED")
    t = tables(ra)
    check("tracked_only" in t and "todos" in t and "scratch" not in t, f"reset to latest keeps the tracked branch-only migration and drops the untracked table ({t})")
    check(sql(ra, "select count(*) c from public.items")[0]["c"] == 0 if "items" in t else True, "reset drops rows")
    code, run = act(a["id"], "reset", {"migration_version": "0"})
    wait_status(PARENT, a["name"], "FUNCTIONS_DEPLOYED")
    check(tables(ra) == [], f"reset to version 0 gives an empty schema ({tables(ra)})")
    check(call("GET", f"/v1/projects/{ra}/database/migrations")[1] == [], "and an empty history")

# ==== 3. the dashboard flow of the merge page against the CLI flow ======================================
cli = new_branch(f"cli-{tag}")
dash = new_branch(f"dash-{tag}")
if cli and dash:
    rc, rd = cli["project_ref"], dash["project_ref"]
    ddl = "create table public.flow_{n} (id int primary key, name text not null default 'x')"
    call("POST", f"/v1/projects/{rc}/database/migrations", {"name": "flow_cli", "query": ddl.format(n="cli")})
    time.sleep(2)  # a version is the second it was made in: two branches must not make the same one
    # (a row too: a schema diff holds the table and not the row, which must still reach the parent)
    call("POST", f"/v1/projects/{rd}/database/migrations", {"name": "flow_dash", "query": ddl.format(n="dash") + "; insert into public.flow_dash (id) values (1)"})
    sql(rd, "create table public.flow_untracked (id int primary key, dash_id int references public.flow_dash (id)); alter table public.flow_untracked enable row level security")
    sql(PARENT, "create table public.parent_drift (id int)")  # production moved on outside migrations

    # CLI / MCP flow: the branch's migrations run on the parent.
    code, run = act(cli["id"], "merge")
    wait_status(PARENT, cli["name"], "FUNCTIONS_DEPLOYED")
    check("flow_cli" in tables(PARENT), "the history flow merged the tracked migration")

    # Studio's flow. The parent got a migration the branch lacks, so Studio offers "Update branch" first.
    code, run = act(dash["id"], "merge")
    refused = wait_status(PARENT, dash["name"], "MIGRATIONS_FAILED")
    check("migrations the source lacks" in (refused.get("supavise_detail") or ""), f"a merge of a branch that is not up to date is refused ({refused.get('supavise_detail')})")
    code, run = act(dash["id"], "push")
    wait_status(PARENT, dash["name"], "FUNCTIONS_DEPLOYED")
    check("flow_cli" in tables(rd) and "parent_drift" in tables(rd), "update branch brought the parent's migration and its untracked table")
    sql(PARENT, "create table public.parent_drift2 (id int)")  # production moves again after the update
    # Then: diff, PUT it as a migration of the branch, merge.
    code, diff = call("GET", f"/v1/branches/{dash['id']}/diff", raw=True)
    check(code == 200 and "flow_dash" in diff and "flow_untracked" in diff and "DROP" not in diff and "parent_drift" not in diff and "flow_cli" not in diff,
          f"the diff has the tracked and the untracked change and nothing of the parent's drift ({diff[:300]!r})")
    put = {"name": "branch_merge_2026-10-11T00-00-00", "query": diff}
    check(call("PUT", f"/v1/projects/{rd}/database/migrations", put)[0] == 200, "PUT records the diff")
    check(call("PUT", f"/v1/projects/{rd}/database/migrations", put)[0] == 200, "and again")
    hist = call("GET", f"/v1/projects/{rd}/database/migrations")[1]
    check(len([m for m in hist if m.get("name") == put["name"]]) == 1, "the repeated PUT recorded one migration")
    code, run = act(dash["id"], "merge")
    final = wait_status(PARENT, dash["name"], "FUNCTIONS_DEPLOYED")
    pt = tables(PARENT)
    check("flow_dash" in pt and "flow_untracked" in pt, f"the dashboard merge brought the tracked and the untracked table ({pt}; {final.get('supavise_detail')})")
    check("parent_drift" in pt and "parent_drift2" in pt, "production's drift was not reverted")
    check(sql(PARENT, "select count(*) c from public.flow_dash")[0]["c"] == 1, "the row the tracked migration inserted reached the parent, after the diff")
    cols = lambda t: sql(PARENT, f"select column_name c, data_type d, column_default x from information_schema.columns where table_schema = 'public' and table_name = '{t}' order by ordinal_position")
    check(cols("flow_cli") == cols("flow_dash"), "the dashboard result has the shape the CLI flow gives")
    ph, bh = call("GET", f"/v1/projects/{PARENT}/database/migrations")[1], call("GET", f"/v1/projects/{rd}/database/migrations")[1]
    # (the update recorded the parent's untracked changes on the branch alone, as branch_push_*)
    check({(m["version"], m.get("name")) for m in bh if not m.get("name", "").startswith("branch_push_")} <= {(m["version"], m.get("name")) for m in ph},
          "the branch's history is part of the parent's")
    # A branch made from the merged parent replays its history, the covered migration included, once.
    again_b = new_branch(f"replayed-{tag}")
    if again_b:
        check({"flow_cli", "flow_dash", "flow_untracked"} <= set(tables(again_b["project_ref"])), "a new branch replays the merged history without creating a table twice")
        check(sql(again_b["project_ref"], "select count(*) c from public.flow_dash")[0]["c"] == 1, "and the covered migration's row once")
    # Resetting the merged branch replays its own history, covered rows included.
    code, run = act(dash["id"], "reset")
    wait_status(PARENT, dash["name"], "FUNCTIONS_DEPLOYED")
    check({"flow_dash", "flow_untracked"} <= set(tables(rd)), "a reset of the merged branch rebuilds its tables once")
    check(sql(rd, "select count(*) c from public.flow_dash")[0]["c"] == 1, "and the covered migration's row once")
    code, diff2 = call("GET", f"/v1/branches/{dash['id']}/diff", raw=True)
    check(code == 200 and diff2 == "", f"nothing is left to merge ({diff2[:200]!r})")
    # Again: the same PUT and merge are a no-op.
    call("PUT", f"/v1/projects/{rd}/database/migrations", put)
    code, run = act(dash["id"], "merge")
    again = wait_status(PARENT, dash["name"], "FUNCTIONS_DEPLOYED")
    check("nothing to merge" in (again.get("supavise_detail") or ""), f"a second merge is a no-op ({again.get('supavise_detail')})")
    check(len(call("GET", f"/v1/projects/{PARENT}/database/migrations")[1]) == len(ph), "and the parent's history did not grow")

    # The branch moved after the diff was recorded: the merge fails with the reason and runs nothing.
    late = new_branch(f"late-{tag}")
    if late:
        rl = late["project_ref"]
        sql(rl, "create table public.late_one (id int)")
        code, d = call("GET", f"/v1/branches/{late['id']}/diff", raw=True)
        call("PUT", f"/v1/projects/{rl}/database/migrations", {"name": "branch_merge_late", "query": d})
        sql(rl, "create table public.late_two (id int)")
        code, run = act(late["id"], "merge")
        wait_status(PARENT, late["name"], "MIGRATIONS_FAILED")
        code, b = call("GET", f"/v1/projects/{PARENT}/branches/{late['name']}")
        check("refresh" in (b.get("supavise_detail") or ""), f"the reason is readable ({b.get('supavise_detail')})")
        check("late_one" not in tables(PARENT), "and nothing ran on the parent")

    # A recording the branch moved past (an abandoned Merge click, then a migration) does not decide the merge.
    time.sleep(2)
    stale = new_branch(f"stale-{tag}")
    if stale:
        rs = stale["project_ref"]
        call("POST", f"/v1/projects/{rs}/database/migrations", {"name": "stale_a", "query": "create table public.stale_a (id int primary key)"})
        code, d = call("GET", f"/v1/branches/{stale['id']}/diff", raw=True)
        call("PUT", f"/v1/projects/{rs}/database/migrations", {"name": "branch_merge_stale", "query": d})
        call("POST", f"/v1/projects/{rs}/database/migrations", {"name": "stale_b", "query": "create table public.stale_b (id int primary key)"})
        code, run = act(stale["id"], "merge")
        got = wait_status(PARENT, stale["name"], "FUNCTIONS_DEPLOYED")
        pt = tables(PARENT)
        check("stale_a" in pt and "stale_b" in pt, f"a merge after a newer migration ignores the recorded diff and runs the migrations ({got.get('supavise_detail')})")
        check("branch_merge_stale" not in {m.get("name") for m in call("GET", f"/v1/projects/{PARENT}/database/migrations")[1]},
              "and the ignored recording stays out of the parent's history")

# ==== 4. push brings the parent's untracked changes =========================================================
pb = new_branch(f"push-{tag}")
if pb:
    rp = pb["project_ref"]
    sql(rp, "create table public.push_branch_only (id int)")
    sql(PARENT, "create table public.push_parent_untracked (id int primary key)")
    code, run = act(pb["id"], "push")
    got = wait_status(PARENT, pb["name"], "FUNCTIONS_DEPLOYED")
    t = tables(rp)
    check("push_parent_untracked" in t and "push_branch_only" in t, f"push brought the parent's untracked table and kept the branch's own ({got.get('supavise_detail')})")
    code, d = call("GET", f"/v1/branches/{pb['id']}/diff", raw=True)
    check("push_branch_only" in d and "push_parent_untracked" not in d, "the diff after a push is the branch's own change")

# ==== 5. Edge Functions ===========================================================================================
code, out = deploy(PARENT, f"hello", "v1", verify=True)
check(code in (200, 201), f"a function deploys on the parent ({code} {out})")
deploy(PARENT, "open", "o1", verify=False)
call("POST", f"/v1/projects/{PARENT}/secrets", [{"name": "PARENT_ONLY", "value": "p"}])
fb = new_branch(f"fn-{tag}", secrets={"BRANCH_ONLY": "b"})
if fb:
    rf = fb["project_ref"]
    for slug in ("hello", "open"):
        p, b = fn(PARENT, slug), fn(rf, slug)
        check(b is not None and b["ezbr_sha256"] == p["ezbr_sha256"] and b["version"] == p["version"] and b["updated_at"] == p["updated_at"] and b["verify_jwt"] == p["verify_jwt"] and b["id"] != p["id"],
              f"{slug} was copied with its digest, version, update time and verify_jwt, under a new id")
    sec = call("GET", f"/v1/projects/{rf}/secrets")[1]
    check([s["name"] for s in sec] == ["BRANCH_ONLY"], f"the branch's secrets are the create body's alone ({sec})")
    # push brings a new body; merge deploys a branch-only function and a change.
    deploy(PARENT, "hello", "v2", verify=True)
    code, run = act(fb["id"], "push")
    wait_status(PARENT, fb["name"], "FUNCTIONS_DEPLOYED")
    check(fn(rf, "hello")["ezbr_sha256"] == fn(PARENT, "hello")["ezbr_sha256"], "push overwrote the branch's copy with the parent's new body")
    deploy(rf, "branch-only", "bo", verify=False)
    deploy(rf, "open", "o2", verify=False)
    code, run = act(fb["id"], "merge")
    final = wait_status(PARENT, fb["name"], "FUNCTIONS_DEPLOYED")
    check(fn(PARENT, "branch-only") is not None and fn(PARENT, "open")["ezbr_sha256"] == fn(rf, "open")["ezbr_sha256"],
          f"merge deployed the branch's new and changed functions to the parent ({final.get('supavise_detail')})")
    check(fn(PARENT, "hello") is not None, "and deleted nothing")
    # both sides changed the same function
    deploy(rf, "open", "o3", verify=False)
    deploy(PARENT, "open", "o4", verify=False)
    code, run = act(fb["id"], "merge")
    bad = wait_status(PARENT, fb["name"], "MIGRATIONS_FAILED")
    check("both changed the Edge Function" in (bad.get("supavise_detail") or ""), f"a function both sides changed fails the merge with a reason ({bad.get('supavise_detail')})")
    check(fn(PARENT, "open")["ezbr_sha256"] != fn(rf, "open")["ezbr_sha256"], "and the parent kept its own")

# ==== 6. settings =====================================================================================================
code, out = call("PATCH", f"/v1/projects/{PARENT}/config/auth", {"site_url": "https://parent.example.com", "external_github_enabled": True,
                 "external_github_client_id": "gh-id", "external_github_secret": "gh-secret", "mfa_max_enrolled_factors": 7, "disable_signup": True,
                 "smtp_host": "127.0.0.1", "smtp_port": "2525", "smtp_user": "mailer", "smtp_pass": "smtp-secret",
                 "smtp_admin_email": "ops@example.com", "smtp_sender_name": "Parent"})
check(code == 200, f"auth settings save on the parent ({code})")
call("PUT", f"/v1/projects/{PARENT}/config/database/postgres", {"statement_timeout": "7s"})
# The parent exposes a schema one of its migrations made. An empty branch cannot start PostgREST on it, so it
# starts on the default schemas and exposes the list once the replay has made the schema.
code, _ = call("POST", f"/v1/projects/{PARENT}/database/migrations", {"name": "exposed_schema",
               "query": "create schema if not exists exposed_x authorization postgres; grant usage on schema exposed_x to anon, authenticated, service_role"})
check(code == 200, f"a migration that makes a schema applies on the parent ({code})")
code, out = call("PATCH", f"/v1/projects/{PARENT}/postgrest", {"db_schema": "public,graphql_public,exposed_x"})
check(code == 200, f"the parent exposes the schema ({code} {out})")
sb = new_branch(f"cfg-{tag}")
if sb:
    code, cfg = call("GET", f"/v1/projects/{sb['project_ref']}/config/auth")
    check(code == 200 and cfg["external_github_client_id"] == "gh-id" and cfg["mfa_max_enrolled_factors"] == 7 and cfg["disable_signup"] is True,
          "the branch starts with the parent's provider id, MFA and sign-up settings")
    check(cfg["site_url"] != "https://parent.example.com", f"site_url stays the branch's default ({cfg['site_url']})")
    # No credential comes along, and no provider is left half set up: the branch sends no mail and the provider is off.
    pcode, pcfg = call("GET", f"/v1/projects/{PARENT}/config/auth")
    check(cfg.get("external_github_enabled") is not True, f"the OAuth provider stays off without its secret ({cfg.get('external_github_enabled')})")
    check(cfg.get("external_github_secret") != "gh-secret" and cfg.get("external_github_secret") != pcfg.get("external_github_secret"), "the parent's OAuth secret is not on the branch")
    check(not cfg.get("smtp_host") and not cfg.get("smtp_user") and not cfg.get("smtp_pass"), "the branch has no SMTP server, user or password of the parent's")
    check(cfg.get("smtp_admin_email") == "ops@example.com" and cfg.get("smtp_sender_name") == "Parent", "and keeps the sender's address and name")
    code, pg = call("GET", f"/v1/projects/{sb['project_ref']}/config/database/postgres")
    check(code == 200 and pg.get("statement_timeout") == "7s", f"a non-sizing Postgres setting was copied ({pg.get('statement_timeout')})")
    code, rest = call("GET", f"/v1/projects/{sb['project_ref']}/postgrest")
    check(code == 200 and rest.get("db_schema") == "public,graphql_public,exposed_x", f"the branch exposes the parent's schemas once they exist ({rest})")
    check(int(sql(sb["project_ref"], "select count(*) n from pg_namespace where nspname = 'exposed_x'")[0]["n"]) == 1, "and the schema is there")

# ==== 7. a branch with data ===================================================================================================
# The sections above made ten branches, as many as [branching] max_per_project allows, and are done with them: deleting a
# branch gives its slot back, which the branch below needs.
for br in made:
    code, _ = call("DELETE", f"/v1/branches/{br['id']}")
    check(code == 200, f"branch {br.get('name')} is deleted to make room ({code})")
made.clear()
for q in ("alter system set work_mem = '97MB'", "alter system set statement_timeout = '13s'", "select pg_reload_conf()"):
    psql(ADMIN_DSN, q)
dbr = new_branch(f"data-{tag}", with_data=True)
if dbr:
    conf = open(os.path.join(STATE, "projects", dbr["project_ref"], "postgres", "data", "postgresql.auto.conf")).read()
    check("work_mem" not in conf, "a data branch does not inherit the parent's sizing")
    check("statement_timeout" in conf, "and keeps its other settings")
    code, p = call("GET", f"/v1/projects/{PARENT}")
    code, b = call("GET", f"/v1/projects/{dbr['project_ref']}")
    check(code == 200, f"the data branch is a project ({code})")
for q in ("alter system reset work_mem", "alter system reset statement_timeout", "select pg_reload_conf()"):
    psql(ADMIN_DSN, q)

print(f"{len(failures)} check(s) failed" if failures else "all content checks passed", flush=True)
sys.exit(1 if failures else 0)
PY
  rm -f "$file"
  for ((i = 0; i < 60; i++)); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  if ((status != 0)); then
    echo "content: checks failed; the end of the stack's log:" >&2
    tail -n 60 "$log" >&2
    return 1
  fi
  echo "branching content: done"
}

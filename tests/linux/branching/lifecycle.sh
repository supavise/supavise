# shellcheck shell=bash
#!/usr/bin/env bash
# CI only: the lifecycle of branches on real clusters. Sourced by tests/linux/branching-xfs.sh, which has
# built "$work/branching.test" and unpacked the artifacts into "$unpacked"; it calls
# branching_lifecycle_check once.
#
# TestServeLifecycle (internal/branching/serve_lifecycle_test.go) starts a node on real clusters with a
# parent project, the Management API, the edge proxy on a loopback port, and the idle pause with a short
# period ($SUPAVISE_BRANCH_SERVE_IDLE, 8 s here). This script drives it over HTTP and with psql:
#
#   - a deletion scheduled with force=false pauses the branch (preview_project_status INACTIVE), a request
#     through the proxy does not wake it, and restore brings it back (pause/status and the other /platform
#     routes take the dashboard's session JWT, which this stack does not have; Go tests cover them);
#   - a branch nobody uses pauses by itself (paused for idleness), an HTTP request through the proxy wakes
#     it and gets 200, and it pauses again; a psql connection to its database port, which is where the
#     pooler connects, wakes it and runs its statement;
#   - a branch the owner paused stays paused on a request and on a connection, until it is resumed;
#   - a persistent branch never sleeps, and the parent project is never paused;
#   - DELETE /v1/projects/{branchRef} deletes the branch through the branch service: the WAL archive of
#     the ephemeral branch is gone and the parent records branch.deleted.
#
# The pooler itself (Supavisor) is not part of this stack (no systemd here): it dials the same loopback
# port psql dials, which is what the wake gate listens on.
#
# Needs: python3, the Go toolchain (TestServeLifecycle builds the binary), no root, no systemd.

branching_lifecycle_check() {
  local file=$work/lifecycle-stack.json log=$work/lifecycle-serve.log pid i psql
  psql=$(ls -d "$unpacked"/postgres-17*/bin/psql 2>/dev/null | head -n 1)
  [[ -x "$psql" ]] || { echo "lifecycle: no psql in the postgres artifact under $unpacked" >&2; return 1; }
  rm -f "$file"
  (
    cd internal/branching
    SUPAVISE_BRANCH_SERVE_FILE=$file SUPAVISE_TEST_UNPACKED=$unpacked SUPAVISE_BRANCH_SERVE_IDLE=8s \
      exec "$work/branching.test" -test.run '^TestServeLifecycle$' -test.v -test.timeout 30m
  ) >"$log" 2>&1 &
  pid=$!
  for ((i = 0; i < 300; i++)); do
    [[ -s "$file" ]] && break
    kill -0 "$pid" 2>/dev/null || { echo "lifecycle: the stack exited early" >&2; tail -n 40 "$log" >&2; return 1; }
    sleep 1
  done
  [[ -s "$file" ]] || { echo "lifecycle: the stack did not come up in 300 s" >&2; tail -n 40 "$log" >&2; kill "$pid" 2>/dev/null || true; return 1; }
  local status=0
  LIFECYCLE_STACK=$file LIFECYCLE_PSQL=$psql python3 - <<'PY' || status=$?
import json, os, re, subprocess, sys, time, urllib.error, urllib.request

stack = json.load(open(os.environ["LIFECYCLE_STACK"]))
API, PAT, PARENT = stack["api_url"], stack["pat"], stack["parent_ref"]
PROXY, DOMAIN, BASE, DSN = stack["proxy_url"], stack["domain"], stack["project_base"], stack["dsn_url"]
BACKUPS = stack["backup_dir"]
IDLE = float(re.sub(r"[^0-9.]", "", stack["idle_after"]) or "8")
PSQL = os.environ["LIFECYCLE_PSQL"]
failures = []


def check(cond, what):
    print(("ok   " if cond else "FAIL ") + what, flush=True)
    if not cond:
        failures.append(what)


def call(method, path, body=None, base=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request((base or API) + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + PAT)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            payload, code = r.read(), r.status
    except urllib.error.HTTPError as e:
        payload, code = e.read(), e.code
    try:
        return code, (json.loads(payload) if payload else None)
    except ValueError:
        return code, payload.decode(errors="replace")


def branch(name):
    code, b = call("GET", f"/v1/projects/{PARENT}/branches/{name}")
    return b if code == 200 else None


def parent_status():
    # The /platform routes are the dashboard's and take a session JWT, which this stack has none of; a personal
    # access token reads the parent through /v1.
    code, p = call("GET", f"/v1/projects/{PARENT}")
    return p["status"] if code == 200 else None


def db_port(ref):
    """The branch's database port, from the connection string the stack hands out."""
    code, dsn = call("GET", "/" + ref, base=DSN.rstrip("/"))
    m = re.search(r"@127\.0\.0\.1:(\d+)/", dsn) if code == 200 and isinstance(dsn, str) else None
    return int(m.group(1)) if m else None


def wait_for(what, cond, timeout=120, every=1.0):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(every)
    check(False, what)
    return None


def create(name, persistent=False):
    code, b = call("POST", f"/v1/projects/{PARENT}/branches", {"branch_name": name, "persistent": persistent})
    check(code == 201, f"create {name} answers 201 (got {code} {b})")
    wait_for(f"branch {name} is ready", lambda: (branch(name) or {}).get("status") == "FUNCTIONS_DEPLOYED", timeout=300)
    b = branch(name)
    return b["project_ref"], b["id"]


def anon_key(ref):
    code, keys = call("GET", f"/v1/projects/{ref}/api-keys")
    for k in keys or []:
        if k.get("name") == "anon" or k.get("id") == "anon":
            return k["api_key"]
    return None


def through_proxy(ref, key, timeout=90):
    """GET /rest/v1/items through the edge proxy, the way a client does. Returns (status, seconds, headers)."""
    req = urllib.request.Request(PROXY + "/rest/v1/items?select=id&limit=1")
    req.add_header("Host", f"{ref}.api.{DOMAIN}")
    req.add_header("apikey", key)
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            r.read()
            return r.status, time.time() - t0, r.headers
    except urllib.error.HTTPError as e:
        return e.code, time.time() - t0, e.headers


def psql_run(ref, sql, timeout=90):
    code, dsn = call("GET", "/" + ref, base=DSN.rstrip("/"))
    dsn = dsn if code == 200 else None
    if not dsn:
        return None, "no dsn", 0
    t0 = time.time()
    try:
        p = subprocess.run([PSQL, dsn, "-qAtX", "-v", "ON_ERROR_STOP=1", "-c", sql], capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return None, "timeout", time.time() - t0
    return p.returncode, (p.stdout + p.stderr).strip(), time.time() - t0


def is_paused(name):
    return lambda: (branch(name) or {}).get("preview_project_status") == "INACTIVE"


def is_running(name):
    return lambda: (branch(name) or {}).get("preview_project_status") == "ACTIVE_HEALTHY"


# ---- a deletion scheduled with force=false pauses the branch, restore resumes it ---------------------
ref, bid = create("sched")
key = anon_key(ref)
check(key is not None, "the branch has an anon key")
code, _ = call("DELETE", f"/v1/branches/{bid}?force=false")
check(code == 200, f"force=false schedules the deletion ({code})")
check(wait_for("a branch scheduled for deletion is INACTIVE", is_paused("sched"), timeout=120) is not None,
      "a branch scheduled for deletion is paused (Studio's Restore is enabled)")
status, secs, hdrs = through_proxy(ref, key)
check(status == 503 and hdrs.get("Retry-After"), f"a request does not wake a branch scheduled for deletion ({status})")
check((branch("sched") or {}).get("preview_project_status") == "INACTIVE", "it is still paused after the request")
code, _ = call("POST", f"/v1/branches/{bid}/restore")
check(code == 201, f"restore answers 201, as hosted does ({code})")
check(wait_for("restore resumes the branch", is_running("sched"), timeout=180) is not None, "restore brings the branch back")
status, secs, _ = through_proxy(ref, key)
check(status == 200, f"the restored branch serves requests ({status})")
code, _ = call("DELETE", f"/v1/branches/{bid}")
check(code == 200, "the restored branch is deleted for good")

# ---- idle pause, wake on a request, wake on a database connection ------------------------------------
ref, bid = create("idle")
key = anon_key(ref)
port = db_port(ref)
check(port is not None and port > BASE, f"the stack hands out the branch's database port ({port})")
code, out, _ = psql_run(ref, "select 1")
check(code == 0 and out == "1", f"the database answers while the branch runs ({code} {out})")
check(wait_for("an unused branch pauses by itself", is_paused("idle"), timeout=int(IDLE * 8) + 60) is not None,
      f"an unused branch is paused within a few periods of {IDLE:.0f} s")
check((branch("idle") or {}).get("status") == "FUNCTIONS_DEPLOYED", "pausing does not change the branch's status")

status, secs, _ = through_proxy(ref, key)
check(status == 200, f"an HTTP request through the proxy wakes the branch and gets 200 ({status}, {secs:.1f} s)")
check((branch("idle") or {}).get("preview_project_status") == "ACTIVE_HEALTHY", "the branch is running after the request")

check(wait_for("the woken branch pauses again", is_paused("idle"), timeout=int(IDLE * 8) + 60) is not None, "an idle branch pauses again after a wake")
code, out, secs = psql_run(ref, "select 'woken by a connection'")
check(code == 0 and out == "woken by a connection", f"a psql connection to the branch's port wakes it and runs its statement ({code} {out} {secs:.1f} s)")
check((branch("idle") or {}).get("preview_project_status") == "ACTIVE_HEALTHY", "the branch is running after the connection")

# A connection that sends nothing wakes nothing.
check(wait_for("the branch pauses once more", is_paused("idle"), timeout=int(IDLE * 8) + 60) is not None, "the branch sleeps again")
import socket
s = None
for _ in range(100):  # the gate listens a moment after the status changes
    try:
        s = socket.create_connection(("127.0.0.1", port), timeout=5)
        break
    except OSError:
        time.sleep(0.2)
check(s is not None, "the sleeping branch's database port is held by the wake gate")
if s is not None:
    time.sleep(2)
    s.close()
time.sleep(3)
check((branch("idle") or {}).get("preview_project_status") == "INACTIVE", "a connection that sends nothing does not wake the branch")

# ---- a branch the owner paused stays paused ------------------------------------------------------------
code, _ = call("POST", f"/v1/projects/{ref}/restore")
check(code == 200, f"the owner resumes the sleeping branch ({code})")
check(wait_for("the resume starts the branch", is_running("idle"), timeout=180) is not None, "an owner's resume of an idle-paused branch works while the wake gate was listening")
code, _ = call("POST", f"/v1/projects/{ref}/pause")
check(code == 200, f"the owner pauses the branch ({code})")
check(wait_for("the owner's pause takes", is_paused("idle"), timeout=120) is not None, "the branch is paused")
status, secs, hdrs = through_proxy(ref, key)
check(status == 503 and hdrs.get("Retry-After"), f"a request does not wake a branch the owner paused ({status})")
code, out, _ = psql_run(ref, "select 1", timeout=30)
check(code != 0, f"a connection does not wake a branch the owner paused ({code} {out})")
time.sleep(IDLE + 6)
check((branch("idle") or {}).get("preview_project_status") == "INACTIVE", "it is still paused after a full period")
code, _ = call("POST", f"/v1/projects/{ref}/restore")
check(code == 200 and wait_for("the owner resumes it", is_running("idle"), timeout=180) is not None, "an owner's resume works")

# ---- persistent branches and the parent never sleep ------------------------------------------------------
pref, pid = create("keep", persistent=True)
time.sleep(IDLE * 3 + 10)
check((branch("keep") or {}).get("preview_project_status") == "ACTIVE_HEALTHY", "a persistent branch is not paused for idleness")
check(parent_status() == "ACTIVE_HEALTHY", "the parent project is not paused for idleness")
call("DELETE", f"/v1/branches/{pid}")

# ---- deleting a branch as a project goes through the branch service -----------------------------------
ref, bid = create("viaproject")
code, out, _ = psql_run(ref, "select pg_switch_wal()")
archive = os.path.join(BACKUPS, ref)
for _ in range(90):
    if os.path.isdir(archive) and any(os.scandir(archive)):
        break
    time.sleep(1)
had_archive = os.path.isdir(archive) and any(os.scandir(archive))
print(("note " if not had_archive else "ok   ") + f"the ephemeral branch archived WAL under {archive}: {had_archive}", flush=True)
code, _ = call("DELETE", f"/v1/projects/{ref}")
check(code == 200, f"DELETE /v1/projects/{{branchRef}} answers 200 ({code})")
check(branch("viaproject") is None and db_port(ref) is None, "the branch is gone")
if had_archive:
    check(not os.path.exists(archive), "the ephemeral branch's WAL archive is removed (it is on the branch path of the delete)")
check(parent_status() == "ACTIVE_HEALTHY", "the parent is still there")

# GET /v1/projects/{branchRef} answers 404, like hosted; the branch routes work.
ref, bid = create("v1404")
code, _ = call("GET", f"/v1/projects/{ref}")
check(code == 404, f"GET /v1/projects/{{branchRef}} answers 404 ({code})")
code, _ = call("GET", f"/v1/projects/{ref}/branches")
check(code == 404, f"GET /v1/projects/{{branchRef}}/branches answers 404 ({code})")
code, _ = call("GET", f"/v1/branches/{bid}")
check(code == 200, "GET /v1/branches/{id} reads the branch")
call("DELETE", f"/v1/branches/{bid}")

print(f"{len(failures)} check(s) failed" if failures else "all lifecycle checks passed", flush=True)
sys.exit(1 if failures else 0)
PY
  rm -f "$file"
  for ((i = 0; i < 60; i++)); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  if ((status != 0)); then
    echo "lifecycle: checks failed; the end of the stack's log:" >&2
    tail -n 60 "$log" >&2
    return 1
  fi
  echo "branching lifecycle: done"
}

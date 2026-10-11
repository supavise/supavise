# shellcheck shell=bash
#!/usr/bin/env bash
# CI only: the action runs of branch operations on real clusters. Sourced by
# tests/linux/branching-xfs.sh, which has built "$work/branching.test" and unpacked the artifacts
# into "$unpacked"; it calls branching_runs_check once.
#
# TestServe (internal/branching/serve_test.go) starts a node on real clusters with a parent project
# and the Management API, and writes {api_url, pat, parent_ref, notify_secret, ...} to a file. This
# script drives that API over HTTP:
#
#   - a persistent branch is created with a notify_url: its create is a run with seven steps
#     (clone and configure skipped), a text log, owned by the branch (404 under the parent);
#   - a python receiver gets the hosted envelope with Standard Webhooks headers, and the signature
#     verifies with the node's notify secret (what `supavise branches notify-secret` prints);
#   - the branch ends FUNCTIONS_DEPLOYED, and its run reads SUCCESS by Studio's rule;
#   - push, merge and reset are runs too; the merge is owned by the parent (404 under the branch);
#   - a forced divergence is a 201 followed by a failed run: a DEAD migrate step with the reason in
#     the log, the steps after it PAUSED, the branch MIGRATIONS_FAILED, and the receiver told.
#
# Needs: python3, the Go toolchain (TestServe builds the binary), no root, no systemd.

branching_runs_check() {
  local file=$work/runs-stack.json log=$work/runs-serve.log pid i
  rm -f "$file"
  (
    cd internal/branching
    SUPAVISE_BRANCH_SERVE_FILE=$file SUPAVISE_TEST_UNPACKED=$unpacked SUPAVISE_BRANCH_SERVE_PRIVATE_NOTIFY=1 \
      exec "$work/branching.test" -test.run '^TestServe$' -test.v -test.timeout 30m
  ) >"$log" 2>&1 &
  pid=$!
  for ((i = 0; i < 300; i++)); do
    [[ -s "$file" ]] && break
    kill -0 "$pid" 2>/dev/null || { echo "runs: the stack exited early" >&2; tail -n 40 "$log" >&2; return 1; }
    sleep 1
  done
  [[ -s "$file" ]] || { echo "runs: the stack did not come up in 300 s" >&2; tail -n 40 "$log" >&2; kill "$pid" 2>/dev/null || true; return 1; }
  local status=0
  RUNS_STACK=$file python3 - <<'PY' || status=$?
import base64, hashlib, hmac, json, os, sys, threading, time, urllib.error, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

stack = json.load(open(os.environ["RUNS_STACK"]))
API, PAT, PARENT = stack["api_url"], stack["pat"], stack["parent_ref"]
NOTIFY_SECRET = stack["notify_secret"]

STEPS = ["clone", "pull", "health", "configure", "migrate", "seed", "deploy"]
failures = []


def check(cond, what):
    print(("ok   " if cond else "FAIL ") + what, flush=True)
    if not cond:
        failures.append(what)


def call(method, path, body=None, headers=None, raw=False):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(API + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + PAT)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            payload = r.read()
            hdrs = r.headers
            code = r.status
    except urllib.error.HTTPError as e:
        payload, hdrs, code = e.read(), e.headers, e.code
    if raw:
        return code, payload.decode(errors="replace"), hdrs
    try:
        return code, (json.loads(payload) if payload else None), hdrs
    except ValueError:
        return code, payload.decode(errors="replace"), hdrs


def wait_status(parent, name, want, timeout=240):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        code, b, _ = call("GET", f"/v1/projects/{parent}/branches/{name}")
        if code == 200:
            last = b
            if b["status"] == want:
                return b
        time.sleep(1)
    check(False, f"branch {name} reached {want} (last: {last and last.get('status')} {last and last.get('supavise_detail')})")
    return last or {}


def derive(run):
    # Studio's rule (data/actions/action-detail-query.ts).
    steps = run["run_steps"]
    if not steps:
        return "RUNNING"
    if all(s["status"] in ("EXITED", "PAUSED") for s in steps):
        return "SUCCESS"
    if any(s["status"] == "DEAD" for s in steps):
        return "FAILED"
    return "RUNNING"


def wait_run(owner, run_id, timeout=240):
    end = time.time() + timeout
    while time.time() < end:
        code, run, _ = call("GET", f"/v1/projects/{owner}/actions/{run_id}")
        if code == 200 and derive(run) != "RUNNING":
            return run
        time.sleep(1)
    check(False, f"run {run_id} under {owner} finished")
    return None


# ---- the receiver of notify_url --------------------------------------------------------------
received = []


class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        received.append((dict(self.headers), self.rfile.read(n)))
        self.send_response(200)
        self.end_headers()

    def log_message(self, *a):
        pass


srv = HTTPServer(("127.0.0.1", 0), Hook)
threading.Thread(target=srv.serve_forever, daemon=True).start()
hook_url = f"http://127.0.0.1:{srv.server_port}/hook"


def wait_hooks(n, timeout=60):
    end = time.time() + timeout
    while time.time() < end and len(received) < n:
        time.sleep(0.5)
    return len(received) >= n


def wait_hook_with(text, since, timeout=60):
    """The first notification after index `since` whose body contains text, or None."""
    end = time.time() + timeout
    while time.time() < end:
        for h, body in received[since:]:
            if text.encode() in body:
                return h, body
        time.sleep(0.5)
    return None


def standard_webhooks_ok(secret, headers, body):
    # The receiver side of https://www.standardwebhooks.com, written from the specification.
    h = {k.lower(): v for k, v in headers.items()}
    if not secret.startswith("whsec_"):
        return False
    key = base64.b64decode(secret[len("whsec_"):])
    msg = h["webhook-id"].encode() + b"." + h["webhook-timestamp"].encode() + b"." + body
    want = hmac.new(key, msg, hashlib.sha256).digest()
    if abs(time.time() - int(h["webhook-timestamp"])) > 300:
        return False
    for entry in h["webhook-signature"].split():
        ver, _, sig = entry.partition(",")
        if ver == "v1" and hmac.compare_digest(base64.b64decode(sig), want):
            return True
    return False


# ---- create a persistent branch with a notify_url ----------------------------------------------
code, br, _ = call("POST", f"/v1/projects/{PARENT}/branches", {"branch_name": "runs-a", "persistent": True, "notify_url": hook_url})
check(code == 201, f"create answers 201 (got {code} {br})")
ref, bid = br["project_ref"], br["id"]

# The run exists, with all its steps, before the branch is ready (Studio reads zero steps as running for ever).
code, runs, hdrs = call("GET", f"/v1/projects/{ref}/actions")
check(code == 200 and len(runs) == 1 and len(runs[0]["run_steps"]) == 7, f"the create is a run with seven steps ({code} {runs})")
create_run = runs[0]["id"]
check(hdrs.get("X-Total-Count") == "1", "X-Total-Count counts the runs")

branch = wait_status(PARENT, "runs-a", "FUNCTIONS_DEPLOYED")
check(branch.get("status") == "FUNCTIONS_DEPLOYED", "a successful create ends FUNCTIONS_DEPLOYED")
run = wait_run(ref, create_run)
check(run is not None and derive(run) == "SUCCESS", "the create run reads SUCCESS by Studio's rule")
if run:
    names = [s["name"] for s in run["run_steps"]]
    states = {s["name"]: s["status"] for s in run["run_steps"]}
    check(names == STEPS, f"the steps come in the order of the DAG ({names})")
    check(states["clone"] == "PAUSED" and states["configure"] == "PAUSED", "clone and configure are skipped (PAUSED) outside the Git flow")
    check(all(states[n] in ("EXITED", "PAUSED") for n in STEPS), f"every step is EXITED or PAUSED ({states})")
    check(run["branch_id"] == bid, "branch_id is the branch")
code, text, hdrs = call("GET", f"/v1/projects/{ref}/actions/{create_run}/logs", raw=True)
check(code == 200 and hdrs.get("Content-Type", "").startswith("text/plain") and "[migrate]" in text, f"the log is text/plain and has the migrate step ({code} {hdrs.get('Content-Type')})")
code, _, _ = call("GET", f"/v1/projects/{PARENT}/actions/{create_run}")
check(code == 404, "a create run answers 404 under the parent")
code, _, hdrs = call("HEAD", f"/v1/projects/{ref}/actions", raw=True)
check(code == 200 and hdrs.get("X-Total-Count") == "1", "HEAD counts the runs")

# ---- the notification --------------------------------------------------------------------------
check(wait_hooks(1), "the receiver got a notification for the persistent branch")
if received:
    headers, body = received[0]
    env = json.loads(body)
    check(env.get("type") == "run.completed", "type is run.completed")
    ar = env["data"]["action_run"]
    check(ar["id"] == create_run and env["data"]["project_ref"] == ref, "action_run.id and project_ref are the run's and the branch's")
    check(env["data"]["details_url"].endswith(f"/project/{PARENT}/branches"), f"details_url points at the parent's branches page ({env['data']['details_url']})")
    check([s["name"] for s in ar["steps"]] == STEPS and all(s["status"] == s["status"].lower() for s in ar["steps"]), "steps carry lowercase statuses")
    check("failure" not in ar and env["supavise"]["status"] == "FUNCTIONS_DEPLOYED", "a success has no failure; the old fields moved under supavise")
    check(standard_webhooks_ok(NOTIFY_SECRET, headers, body), "the Standard Webhooks signature verifies with the node's secret")

# ---- push, merge, reset --------------------------------------------------------------------------
code, mig, _ = call("POST", f"/v1/projects/{ref}/database/migrations", {"name": "runs_feature", "query": "create table public.runs_feature (id int primary key)"})
check(code in (200, 201), f"a migration applies on the branch ({code} {mig})")

code, out, _ = call("POST", f"/v1/branches/{bid}/push", {})
check(code == 201 and out and out.get("workflow_run_id"), f"push answers 201 with a run id ({code} {out})")
push_run = out["workflow_run_id"]
run = wait_run(ref, push_run)
check(run is not None and derive(run) == "SUCCESS", "the push run is a success under the branch")
wait_status(PARENT, "runs-a", "FUNCTIONS_DEPLOYED")

code, out, _ = call("POST", f"/v1/branches/{bid}/merge", {})
check(code == 201 and out and out.get("workflow_run_id"), f"merge answers 201 with a run id ({code} {out})")
merge_run = out["workflow_run_id"]
code, _, _ = call("GET", f"/v1/projects/{ref}/actions/{merge_run}")
check(code == 404, "a merge run answers 404 under the branch")
run = wait_run(PARENT, merge_run)
check(run is not None and derive(run) == "SUCCESS", "the merge run is a success under the parent")
branch = wait_status(PARENT, "runs-a", "FUNCTIONS_DEPLOYED")
code, mlist, _ = call("GET", f"/v1/projects/{PARENT}/database/migrations")
check(code == 200 and any(m.get("name") == "runs_feature" for m in mlist), "the merge reached the parent")

code, out, _ = call("POST", f"/v1/branches/{bid}/reset", {})
check(code == 201 and out and out.get("workflow_run_id"), f"reset answers 201 with a run id ({code} {out})")
reset_run = out["workflow_run_id"]
run = wait_run(ref, reset_run, timeout=300)
check(run is not None and derive(run) == "SUCCESS", "the reset run is a success under the branch")
branch = wait_status(PARENT, "runs-a", "FUNCTIONS_DEPLOYED", timeout=300)

# ---- a forced divergence --------------------------------------------------------------------------
n_hooks = len(received)
call("POST", f"/v1/projects/{PARENT}/database/migrations", {"name": "runs_parent_only", "query": "create table public.runs_parent_only (id int)"})
code, _, _ = call("POST", f"/v1/projects/{ref}/database/migrations", {"name": "runs_branch_only", "query": "create table public.runs_branch_only (id int)"})
code, out, _ = call("POST", f"/v1/branches/{bid}/merge", {})
check(code == 201 and out and out.get("workflow_run_id"), f"a diverged merge is still a 201 ({code} {out})")
bad_run = out["workflow_run_id"]
run = wait_run(PARENT, bad_run)
if run:
    states = {s["name"]: s["status"] for s in run["run_steps"]}
    check(states["migrate"] == "DEAD" and states["seed"] == "PAUSED" and states["deploy"] == "PAUSED", f"the diverged merge has a DEAD migrate step and PAUSED dependents ({states})")
    check(derive(run) == "FAILED", "Studio reads the run as FAILED")
code, text, _ = call("GET", f"/v1/projects/{PARENT}/actions/{bad_run}/logs", raw=True)
check(code == 200 and "diverged" in text, "the reason is in the log")
failed = wait_status(PARENT, "runs-a", "MIGRATIONS_FAILED")
check("diverged" in (failed.get("supavise_detail") or ""), "supavise_detail carries the reason on the branch")
hit = wait_hook_with("diverged", n_hooks)
check(hit is not None, "the receiver was told about the failed merge")
if hit:
    env = json.loads(hit[1])
    check(env["data"]["action_run"]["id"] == bad_run and "diverged" in env["data"]["action_run"].get("failure", ""), "the envelope carries the run and the failure")
# The default branch shows the failed merge too, until the next one works.
code, main, _ = call("GET", f"/v1/projects/{PARENT}/branches/main")
check(code == 200 and main["status"] == "MIGRATIONS_FAILED", f"the default branch reads MIGRATIONS_FAILED ({main.get('status') if main else code})")

# Deleting the branch clears the failure: there is nothing left to fix, and the default branch reads the
# merge that worked before (a persistent branch takes a final backup first, so the delete can take a while).
code, _, _ = call("DELETE", f"/v1/branches/{bid}")
check(code == 200, f"the failed branch is deleted ({code})")
main = wait_status(PARENT, "main", "FUNCTIONS_DEPLOYED", timeout=300)

print(f"{len(failures)} check(s) failed" if failures else "all run checks passed", flush=True)
sys.exit(1 if failures else 0)
PY
  rm -f "$file"
  for ((i = 0; i < 60; i++)); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  if ((status != 0)); then
    echo "runs: checks failed; the end of the stack's log:" >&2
    tail -n 60 "$log" >&2
    return 1
  fi
  echo "branching runs: done"
}

#!/usr/bin/env bash
# Runs tests/linux/github/drive.mjs against the stand-in node of internal/api (TestServeGitHubStandIn: the
# real Management API, branching and GitHub services over a fake project manager and an in-memory registry)
# and the fake GitHub. It needs Go and Node 22 and nothing else: no Postgres, root or network. Run it
# after changing the driver, the fake or the routes it calls, instead of waiting for the Linux job.
#
#   tests/linux/github/selftest.sh
set -euo pipefail
cd "$(dirname "$0")/../../.."
work=$(mktemp -d)
pids=()
cleanup() {
  rm -f "$work/stack.json"
  for p in "${pids[@]:-}"; do [[ -n "$p" ]] && kill "$p" 2>/dev/null || true; done
  rm -rf "$work"
}
trap cleanup EXIT

openssl genrsa -out "$work/key.pem" 2048 2>/dev/null
printf 'client-secret' >"$work/client_secret"
printf 'selftest-webhook-secret' >"$work/webhook_secret"

node tests/linux/github/fake-github.mjs --port 0 --app-id 918273 --key "$work/key.pem" --client-secret client-secret --file "$work/fake.json" 2>"$work/fake.log" &
pids+=($!)
for _ in $(seq 1 50); do [[ -s "$work/fake.json" ]] && break; sleep 0.2; done
[[ -s "$work/fake.json" ]] || { cat "$work/fake.log" >&2; echo "the fake GitHub did not start" >&2; exit 1; }
fake=$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1])).url)' "$work/fake.json")

cat >"$work/github.json" <<JSON
{"app_id": 918273, "app_slug": "supavise-selftest", "client_id": "Iv1.selftest", "client_secret_file": "$work/client_secret",
 "private_key_file": "$work/key.pem", "webhook_secret_file": "$work/webhook_secret", "api_url": "$fake", "web_url": "$fake"}
JSON

SUPAVISE_GITHUB_STANDIN_FILE="$work/stack.json" SUPAVISE_GITHUB_STANDIN_CONFIG="$work/github.json" \
  go test ./internal/api -run '^TestServeGitHubStandIn$' -count=1 -timeout 10m >"$work/standin.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 600); do [[ -s "$work/stack.json" ]] && break; sleep 0.2; done
[[ -s "$work/stack.json" ]] || { tail -n 40 "$work/standin.log" >&2; echo "the stand-in did not start" >&2; exit 1; }

node tests/linux/github/drive.mjs --stack "$work/stack.json" --fake "$fake" --webhook-secret "$(cat "$work/webhook_secret")"

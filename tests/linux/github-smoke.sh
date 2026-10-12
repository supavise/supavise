#!/usr/bin/env bash
# CI only: the GitHub integration on a node with real clusters, against a fake GitHub.
#
#   tests/linux/github-smoke.sh
#
# TestServeGitHub (internal/github/serve_test.go) starts a node (system and parent project on real
# Postgres, GoTrue and PostgREST under the exec supervisor, pgmeta, the Management API with the branching
# and GitHub services) whose [github] section names an App with files made here and the fake GitHub
# (tests/linux/github/fake-github.mjs) as its API. tests/linux/github/drive.mjs then plays GitHub and the
# dashboard: it authorizes, connects the project, posts signed webhooks and checks, over HTTP and in the
# databases:
#
#   - a pull_request opened makes a branch with git_branch and pr_number that ends FUNCTIONS_DEPLOYED, with
#     one run of seven steps, the migration, the seed and the function of the repository, the settings of
#     config.toml, the check run "Supabase Preview" and one comment;
#   - a synchronize applies only the migration the branch lacks, deploys the function that changed and
#     updates the comment; a replayed delivery, a fork and a bad signature do nothing;
#   - closed deletes the branch;
#   - a push to the production branch (after "Deploy to production" is set) migrates the project and
#     reports "Supabase Production"; Resync does it again.
#
# It runs as an unprivileged user (initdb refuses root); no Docker, no systemd. The Linux artifacts come
# from the releases pinned in internal/versions/versions.yaml, checked against SHA256SUMS. Needs Go and
# Node 22. Real GitHub is never contacted.
set -euo pipefail
cd "$(dirname "$0")/../.."

if [[ "$(id -u)" -eq 0 ]]; then
  echo "refusing to run as root: initdb will not start. Run this script as an unprivileged user." >&2
  exit 1
fi
for c in go node openssl curl zstd; do command -v "$c" >/dev/null || { echo "missing $c" >&2; exit 1; }; done

case "$(uname -m)" in
  x86_64) platform=linux-amd64 ;;
  aarch64 | arm64) platform=linux-arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

work=$(mktemp -d)
unpacked=$work/unpacked
mkdir -p "$unpacked"
pids=()
cleanup() {
  rm -f "$work/stack.json"
  for p in "${pids[@]:-}"; do [[ -n "$p" ]] && kill "$p" 2>/dev/null || true; done
  sleep 2
  rm -rf "$work"
}
trap cleanup EXIT

fetch() { # fetch <artifact> : unpack the pinned release into $unpacked/<tag>-<platform>
  local name=$1 tag
  tag=$(awk -v re="^  $name:" '$0 ~ re {print $2; exit}' internal/versions/versions.yaml)
  [[ -n "$tag" ]] || { echo "no artifacts.$name in internal/versions/versions.yaml" >&2; exit 1; }
  local base="https://github.com/supabase/slim-services/releases/download/${tag}"
  curl -fsSL "$base/${tag}-${platform}.tar.zst" -o "$work/$name.tar.zst"
  curl -fsSL "$base/SHA256SUMS" -o "$work/$name.sums"
  local want
  want=$(awk -v f="${tag}-${platform}.tar.zst" '$2 == f || $2 == "*" f {print $1; exit}' "$work/$name.sums")
  [[ -n "$want" ]] || { echo "SHA256SUMS of ${tag} has no entry for ${tag}-${platform}.tar.zst" >&2; exit 1; }
  echo "${want}  $work/$name.tar.zst" | sha256sum -c -
  mkdir "$unpacked/${tag}-${platform}"
  zstd -dc "$work/$name.tar.zst" | tar -x -C "$unpacked/${tag}-${platform}"
  rm -f "$work/$name.tar.zst"
}
for a in postgres auth postgrest pgmeta; do fetch "$a"; done

# The App: a key and two secrets in files, as an operator keeps them ([github] in config.toml).
openssl genrsa -out "$work/key.pem" 2048 2>/dev/null
chmod 600 "$work/key.pem"
printf 'client-secret' >"$work/client_secret"
printf 'smoke-webhook-secret' >"$work/webhook_secret"
chmod 600 "$work/client_secret" "$work/webhook_secret"

node tests/linux/github/fake-github.mjs --port 0 --app-id 918273 --key "$work/key.pem" --client-secret client-secret --file "$work/fake.json" 2>"$work/fake.log" &
pids+=($!)
for _ in $(seq 1 50); do [[ -s "$work/fake.json" ]] && break; sleep 0.2; done
[[ -s "$work/fake.json" ]] || { cat "$work/fake.log" >&2; echo "the fake GitHub did not start" >&2; exit 1; }
fake=$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1])).url)' "$work/fake.json")

cat >"$work/github.json" <<JSON
{"app_id": 918273, "app_slug": "supavise-smoke", "client_id": "Iv1.smoke", "client_secret_file": "$work/client_secret",
 "private_key_file": "$work/key.pem", "webhook_secret_file": "$work/webhook_secret", "api_url": "$fake", "web_url": "$fake"}
JSON

CGO_ENABLED=0 go test -c -o "$work/github.test" ./internal/github
log=$work/serve.log
(
  cd internal/github
  SUPAVISE_GITHUB_SERVE_FILE=$work/stack.json SUPAVISE_GITHUB_SERVE_CONFIG=$work/github.json SUPAVISE_TEST_UNPACKED=$unpacked \
    exec "$work/github.test" -test.run '^TestServeGitHub$' -test.v -test.timeout 40m
) >"$log" 2>&1 &
pid=$!
pids+=($pid)
for ((i = 0; i < 300; i++)); do
  [[ -s "$work/stack.json" ]] && break
  kill -0 "$pid" 2>/dev/null || { echo "github: the stack exited early" >&2; tail -n 60 "$log" >&2; exit 1; }
  sleep 1
done
[[ -s "$work/stack.json" ]] || { echo "github: the stack did not come up in 300 s" >&2; tail -n 60 "$log" >&2; exit 1; }

status=0
node tests/linux/github/drive.mjs --stack "$work/stack.json" --fake "$fake" --webhook-secret "$(cat "$work/webhook_secret")" --db || status=$?
if [[ $status -ne 0 ]]; then
  echo "--- the node's log (tail)" >&2
  tail -n 80 "$log" >&2
  echo "--- the fake GitHub's log" >&2
  tail -n 40 "$work/fake.log" >&2
  exit "$status"
fi
echo "github-smoke: done"

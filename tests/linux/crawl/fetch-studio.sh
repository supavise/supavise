#!/usr/bin/env bash
# Puts the amd64 Studio artifact for this checkout in DEST without building Studio (a build takes the best part of an
# hour on a runner, and the crawl needs only the result):
#
#   GH_TOKEN=... GITHUB_REPOSITORY=owner/name GITHUB_SHA=... tests/linux/crawl/fetch-studio.sh DEST [--fresh]
#
# Without --fresh it takes the artifact `studio-linux-amd64` of the newest successful run of studio.yml on main that has one,
# which is the same code as this checkout when nothing under studio/ changed since. With --fresh (a push that changed Studio) it
# waits for the studio.yml run of this very commit to finish and takes the artifact of that. Either way the file name
# must be the one internal/versions/versions.yaml pins (supavise-studio-<tag>-p<patchset>-linux-amd64.tar.zst): the node
# refuses any other build, and an older build on main would crawl the wrong pages.
#
# STUDIO_WAIT_SECONDS (default 9000) bounds the wait for the run of the commit; STUDIO_GRACE_SECONDS (default 900) is how
# long the run may take to appear after the push.
set -euo pipefail

dest=${1:?usage: fetch-studio.sh DEST [--fresh]}
fresh=0
[[ ${2:-} == --fresh ]] && fresh=1
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}
sha=${GITHUB_SHA:?GITHUB_SHA is not set}
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)

fail() { echo "::error::fetch-studio: $*" >&2; exit 1; }

tag=$(awk '/^studio:/{s=1;next} s&&/^[^ ]/{s=0} s&&/^[ ]+tag:/{print $2; exit}' "$root/internal/versions/versions.yaml")
ps=$(awk '/^studio:/{s=1;next} s&&/^[^ ]/{s=0} s&&/^[ ]+patchset:/{print $2; exit}' "$root/internal/versions/versions.yaml")
[[ -n $tag ]] || fail "studio.tag is not in internal/versions/versions.yaml"
want="supavise-studio-${tag}${ps:+-p$ps}-linux-amd64.tar.zst"
mkdir -p "$dest"

if [[ $fresh -eq 1 ]]; then
  begin=$SECONDS
  limit=${STUDIO_WAIT_SECONDS:-9000}
  grace=${STUDIO_GRACE_SECONDS:-900}
  while :; do
    run=$(gh run list --repo "$repo" --workflow studio.yml --commit "$sha" --json databaseId,status,conclusion --jq 'sort_by(.databaseId) | last // empty')
    if [[ -z $run ]]; then
      (( SECONDS - begin < grace )) || fail "studio.yml has no run on $sha: the Studio build this crawl needs was not started"
    elif [[ $(jq -r .status <<<"$run") == completed ]]; then
      break
    else
      echo "studio.yml run $(jq -r .databaseId <<<"$run") is $(jq -r .status <<<"$run")" >&2
    fi
    (( SECONDS - begin < limit )) || fail "gave up after ${limit}s waiting for the Studio build of $sha"
    sleep 60
  done
  id=$(jq -r .databaseId <<<"$run")
  echo "Studio build from studio.yml run $id" >&2
  gh run download "$id" --repo "$repo" --name studio-linux-amd64 --dir "$dest" \
    || fail "run $id has no studio-linux-amd64 artifact (the build failed, or the artifact expired)"
else
  # The newest successful run on main that has the artifact: a run of a push that did not touch Studio skips the build, so
  # the newest run is not always the one to take.
  got=0
  for id in $(gh run list --repo "$repo" --workflow studio.yml --branch main --status success --limit 30 --json databaseId --jq '.[].databaseId'); do
    if gh run download "$id" --repo "$repo" --name studio-linux-amd64 --dir "$dest" >/dev/null 2>&1; then
      echo "Studio build from studio.yml run $id" >&2
      got=1
      break
    fi
  done
  [[ $got -eq 1 ]] || fail "none of the last successful studio.yml runs on main has a studio-linux-amd64 artifact"
fi

f=$(ls "$dest"/*.tar.zst 2>/dev/null | head -1 || true)
[[ -n $f ]] || fail "run $id's artifact holds no .tar.zst"
[[ $(basename "$f") == "$want" ]] || fail "the build of run $id is $(basename "$f"), and this checkout pins $want"
echo "$f"

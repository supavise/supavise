#!/usr/bin/env bash
# Which parts of the repository a push changed, for the workflows that run only when their part did
# (studio.yml, studio-crawl.yml):
#
#   tests/linux/crawl/changes.sh NAME=ERE [NAME=ERE ...] >> "$GITHUB_OUTPUT"
#
# Prints NAME=true when a changed file matches the extended regular expression ERE, NAME=false otherwise, then
# always=true|false and run=true|false. A push to main, a manual run and a scheduled run are `always`: the release gate
# (deploy/release-gate.sh) needs a run of these workflows on the tagged commit, so main does not skip them for a change
# that touched neither part. run is true when always is, or when any NAME is. The changed files are those of the push
# (github.event.before..HEAD, from EVENT_BEFORE), and for the first push of a branch, or when the old commit is not in
# the clone, those of the branch against the merge base with origin/main. Needs the full history (fetch-depth: 0).
set -euo pipefail

always=false
if [[ ${GITHUB_EVENT_NAME:-push} != push || ${GITHUB_REF:-} == refs/heads/main ]]; then always=true; fi

base=""
before=${EVENT_BEFORE:-}
if [[ -n $before && $before != 0000000000000000000000000000000000000000 ]] && git cat-file -e "$before^{commit}" 2>/dev/null; then
  base=$before
else
  base=$(git merge-base origin/main HEAD 2>/dev/null || true)
fi
files=""
if [[ -n $base ]]; then files=$(git diff --name-only "$base" HEAD || true); fi

run=$always
for spec in "$@"; do
  name=${spec%%=*}
  re=${spec#*=}
  if [[ -n $files ]] && grep -Eq "$re" <<<"$files"; then
    echo "$name=true"
    run=true
  else
    echo "$name=false"
  fi
done
echo "always=$always"
echo "run=$run"

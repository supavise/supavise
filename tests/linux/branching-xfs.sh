#!/usr/bin/env bash
# CI only: branch clones on real Linux filesystems.
#
#   tests/linux/branching-xfs.sh
#
# 1. XFS: a loop file is formatted with reflink=1 and mounted; the state directory lives on
#    it. A parent of about 1 GB is created, a with_data branch is cloned with FICLONE, and
#    the clone time and the extra disk are reported (also in the job summary).
# 2. ext4 (the runner's root): the same parent size, but the filesystem cannot clone files, so
#    supavise restores the parent's latest base backup plus WAL (internal/backup) and says so.
#
# After them, tests/linux/branching/runs.sh drives the Management API of a node on real clusters:
# the action runs of branch operations, the notify_url envelope and its signature; and
# tests/linux/branching/content.sh the same way: what a branch carries from its parent and what a
# merge brings back (schema, Edge Functions, settings).
#
# tests/linux/branching/lifecycle.sh does the same for the lifecycle: the scheduled delete, the idle pause
# and the wake on a request or a connection, the owner's pause, and the delete through the project routes.
#
# Both runs also run the full branching scenario (schema-only and with_data branches,
# isolation in both directions, merge, push, reset, delete, expiry), a clone of a parent under
# write load checked with amcheck (file-clone filesystems only), the isolation of a clone from
# the parent's subscriptions and cron jobs, and the merge lock.
#
# It runs as an unprivileged user (initdb refuses root) and uses sudo only to format and
# mount the loop file. No Docker, no systemd: the exec supervisor starts the clusters.
# Linux artifacts come from the releases pinned in internal/versions/versions.yaml, checked against the
# release's SHA256SUMS.
set -euo pipefail
cd "$(dirname "$0")/../.."

if [[ "$(id -u)" -eq 0 ]]; then
  echo "refusing to run as root: initdb will not start. Run this script as an unprivileged user (it calls sudo itself)." >&2
  exit 1
fi
PARENT_MB=${PARENT_MB:-1024}

case "$(uname -m)" in
  x86_64) platform=linux-amd64 ;;
  aarch64 | arm64) platform=linux-arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

work=$(mktemp -d)
unpacked=$work/unpacked
mkdir -p "$unpacked"
cleanup() {
  mountpoint -q /mnt/supavise-xfs 2>/dev/null && sudo umount /mnt/supavise-xfs || true
  sudo losetup -j "$work/xfs.img" 2>/dev/null | cut -d: -f1 | xargs -r sudo losetup -d || true
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
# pgmeta is for the stack of tests/linux/branching/runs.sh (TestServe serves the Management API with it).
for a in postgres auth postgrest pgmeta; do fetch "$a"; done
ls "$unpacked"

# Build the test binary once; both runs use it. The supavise binary the tests build for
# archive_command is built by the tests themselves.
CGO_ENABLED=0 go test -c -o "$work/branching.test" ./internal/branching

run() { # run <label> <state dir> <expected method>
  local label=$1 state=$2 expect=$3
  echo "=== $label: state dir $state (expecting $expect)"
  df -hT "$state" | tail -n 2
  # The package directory is the working directory of a test run.
  (cd internal/branching && \
    SUPAVISE_TEST_UNPACKED="$unpacked" SUPAVISE_TEST_STATE_DIR="$state" SUPAVISE_TEST_EXPECT_METHOD="$expect" \
    SUPAVISE_TEST_PARENT_MB="$PARENT_MB" SUPAVISE_TEST_LABEL="$label" \
    "$work/branching.test" -test.run '^(TestIntegrationBranching|TestIntegrationCloneSize|TestIntegrationCloneUnderWriteLoad|TestIntegrationCloneIsolatesTheParentsIntegrations|TestIntegrationCloneNeutralizesForeignServersAndParentCredentials|TestIntegrationApplyRefusesAVersionAlreadyApplied)$' -test.v -test.timeout 35m)
}

# 1. XFS with reflink.
sudo apt-get install -y --no-install-recommends xfsprogs >/dev/null
sudo mkdir -p /mnt/supavise-xfs
truncate -s 8G "$work/xfs.img"
loop=$(sudo losetup --find --show "$work/xfs.img")
sudo mkfs.xfs -q -m reflink=1 "$loop"
sudo mount "$loop" /mnt/supavise-xfs
sudo chown "$(id -u):$(id -g)" /mnt/supavise-xfs
xfs_info /mnt/supavise-xfs | grep -o 'reflink=[01]'
run "XFS (reflink=1) on a loop file" /mnt/supavise-xfs reflink

# 2. ext4 on the runner's root (no file cloning).
ext4=/var/tmp/supavise-ext4
sudo mkdir -p "$ext4"
sudo chown "$(id -u):$(id -g)" "$ext4"
findmnt -no FSTYPE -T "$ext4"
run "ext4 on the runner root" "$ext4" base-backup

# 3. The action runs of branch operations over HTTP (tests/linux/branching/runs.sh): a node on
#    real clusters, the Management API, a notify_url receiver.
# shellcheck source=branching/runs.sh
source tests/linux/branching/runs.sh
branching_runs_check

# 4. What a branch carries (tests/linux/branching/content.sh): the schema of a parent built with
#    execute_sql, reset from the branch's own history, Studio's merge flow next to the CLI's, Edge
#    Functions, saved settings, and the sizing of a branch with data, on the XFS of step 1.
# shellcheck source=branching/content.sh
source tests/linux/branching/content.sh
branching_content_check

# 5. The lifecycle of branches over HTTP and psql (tests/linux/branching/lifecycle.sh): the scheduled delete
#    pauses a branch, an idle branch pauses and wakes on a request through the proxy or a connection, a
#    branch the owner paused stays paused, a delete through the project routes purges the archive.
# shellcheck source=branching/lifecycle.sh
source tests/linux/branching/lifecycle.sh
branching_lifecycle_check
echo "branching-xfs: done"

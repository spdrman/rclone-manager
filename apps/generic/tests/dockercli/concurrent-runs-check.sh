#!/usr/bin/env bash
#
# Issue #185's acceptance criterion 2, run rather than asserted: two (or
# more) dockercli runs, from different worktrees of this repository,
# against the one Docker daemon this machine has, at the same time. Both
# must pass, and each must have tested the image it built itself.
#
# Before the per-run reference this checks, both runs built and retagged
# one name, `backup-manager:dockercli-test`. Whichever built last owned
# it, and the other run went on inspecting, running and compose-ing an
# image built from a different commit, with nothing able to notice. The
# suite's own TestTheImageUnderTestIsTheOneThisRunBuilt is what would
# notice now, and this script is what puts two real runs in each other's
# way so that test has something to notice.
#
# Each worktree gets its own marker file under core/cmd/backup-manager,
# which container/Dockerfile copies into the build. That is deliberate: it
# makes the two images differ in content and not only in name, so a run
# that picked up the other worktree's image would be running a different
# binary, exactly as it did in the failures this issue was filed for.
#
# Usage:  bash apps/generic/tests/dockercli/concurrent-runs-check.sh [runs]
# Needs:  a running Docker daemon, and a clean enough HEAD to build.

set -euo pipefail

runs="${1:-2}"
if [ "$runs" -lt 2 ]; then
  echo "concurrent-runs-check: need at least 2 runs to demonstrate anything, got $runs" >&2
  exit 2
fi

root="$(git rev-parse --show-toplevel)"
head_sha="$(git -C "$root" rev-parse HEAD)"
stamp="$$-$(date +%s)"
worktrees="$root/.claude/worktrees"
logdir="$(mktemp -d)"

dirs=()
cleanup() {
  for d in "${dirs[@]:-}"; do
    [ -n "$d" ] || continue
    git -C "$root" worktree remove --force "$d" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

echo "==> $runs concurrent dockercli runs from $runs worktrees of $head_sha"
echo "    logs: $logdir"

pids=()
for i in $(seq 1 "$runs"); do
  dir="$worktrees/dockercli-concurrency-$stamp-$i"
  git -C "$root" worktree add --detach "$dir" "$head_sha" >/dev/null
  dirs+=("$dir")

  # The content divergence. An unused constant is legal Go, changes the
  # COPY layer container/Dockerfile builds from, and touches nothing the
  # suite asserts on.
  cat > "$dir/core/cmd/backup-manager/zz_concurrency_marker.go" <<EOF
package main

const concurrencyMarker = "worktree-$i-$stamp"
EOF

  ( cd "$dir/apps/generic" && go test ./tests/dockercli/ -count=1 -v ) \
    > "$logdir/run-$i.log" 2>&1 &
  pids+=("$!")
  # Indexed from the counter, not with a negative index: /bin/bash on
  # macOS is 3.2 and has no negative array indices.
  echo "    run $i: pid ${pids[$((i - 1))]}  worktree $dir"
done

status=0
for i in $(seq 1 "$runs"); do
  if wait "${pids[$((i - 1))]}"; then
    echo "    run $i: PASS"
  else
    echo "    run $i: FAIL"
    status=1
  fi
done

echo
echo "==> what each run built and tested"
refs=()
ids=()
for i in $(seq 1 "$runs"); do
  line="$(grep -o 'dockercli image under test: .*' "$logdir/run-$i.log" | head -1 || true)"
  if [ -z "$line" ]; then
    echo "    run $i: never reported an image under test; see $logdir/run-$i.log"
    status=1
    continue
  fi
  echo "    run $i: $line"
  refs+=("$(echo "$line" | sed -n 's/.*reference=\([^ ]*\).*/\1/p')")
  ids+=("$(echo "$line" | sed -n 's/.*id=\([^ ]*\).*/\1/p')")
done

echo
echo "==> the property under test"
if [ "${#refs[@]}" -ne "$runs" ]; then
  status=1
elif [ "$(printf '%s\n' "${refs[@]}" | sort -u | wc -l | tr -d ' ')" -ne "$runs" ]; then
  echo "    FAIL: $runs runs shared an image reference, which is issue #185 unfixed"
  status=1
elif [ "$(printf '%s\n' "${ids[@]}" | sort -u | wc -l | tr -d ' ')" -ne "$runs" ]; then
  echo "    FAIL: $runs runs resolved to one image id, so one run tested the other's build"
  status=1
else
  echo "    OK: $runs distinct references, resolving to $runs distinct images"
fi

for ref in "${refs[@]:-}"; do
  if [ "$ref" = "backup-manager:dockercli-test" ]; then
    echo "    FAIL: a run built the old globally shared tag $ref"
    status=1
  fi
done

echo
if [ "$status" -eq 0 ]; then
  echo "==> concurrent-runs-check: ok"
else
  echo "==> concurrent-runs-check: FAILED (logs kept in $logdir)"
fi
exit "$status"

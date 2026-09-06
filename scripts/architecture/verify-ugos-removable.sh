#!/usr/bin/env bash
# EPIC-B WP1.1 RED plan: "a CI check asserting apps/ugos/ can be deleted
# without breaking core or ui/shared tests" (docs/EPIC-B-multi-nas.md §69
# WP1.1). This is the acceptance criterion "adding/removing a provider app
# requires no lifecycle changes" made concrete for the one provider that
# exists furthest along today.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

wt=""
# Trapped on EXIT rather than run at the end, because this check deletes
# whole directories inside the worktree and then runs a build in it: an
# interrupt or a failing build partway through would otherwise leave a
# registered git worktree full of holes behind, and the next run inherits
# it. The guard is for the interrupt that lands before the worktree
# exists.
cleanup() { [ -n "$wt" ] && arch::cleanup_worktree "$wt"; }
trap cleanup EXIT

arch::make_worktree wt
rm -rf "$wt/apps/ugos"

if [ ! -d "$wt/core" ]; then
  echo "FAIL: core/ module does not exist yet." >&2
  exit 1
fi

echo "==> go test ./... (core/, with apps/ugos deleted)"
(cd "$wt/core" && GOWORK=off go test ./...)

echo "==> npm ci && npm test (ui/shared, with apps/ugos deleted)"
(cd "$wt/ui/shared" && npm ci --no-audit --no-fund && npm test)

echo "OK: apps/ugos/ can be deleted without breaking core or ui/shared tests."

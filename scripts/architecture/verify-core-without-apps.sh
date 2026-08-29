#!/usr/bin/env bash
# EPIC-B WP1.1 behavioral contract: "core/ builds and its full test suite
# passes with apps/ deleted entirely." (docs/EPIC-B-multi-nas.md §7.1, §69
# WP1.1). Proves it by actually deleting apps/ in a throwaway worktree,
# rather than trusting a static import scan to have caught every path.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

wt=""
cleanup() { [ -n "$wt" ] && arch::cleanup_worktree "$wt"; }
trap cleanup EXIT

arch::make_worktree wt

if [ ! -d "$wt/core" ]; then
  echo "FAIL: core/ module does not exist yet." >&2
  exit 1
fi

rm -rf "$wt/apps"

# GOWORK=off: the repo root's go.work also lists ./apps/common (for local
# multi-module development convenience), and apps/ is now gone in this
# worktree. Without this, `go build` would walk up to that go.work file and
# fail on the missing apps/common — a workspace-tooling artifact, not the
# thing this check exists to prove. core/'s own go.mod is what must stand
# alone.
echo "==> go build ./... (core/, with apps/ deleted entirely)"
(cd "$wt/core" && GOWORK=off go build ./...)

echo "==> go test ./... (core/, with apps/ deleted entirely)"
(cd "$wt/core" && GOWORK=off go test ./...)

echo "OK: core/ builds and its full test suite passes with apps/ deleted entirely."

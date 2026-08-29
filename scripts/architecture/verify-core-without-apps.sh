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

echo "==> go build ./... (core/, with apps/ deleted entirely)"
(cd "$wt/core" && go build ./...)

echo "==> go test ./... (core/, with apps/ deleted entirely)"
(cd "$wt/core" && go test ./...)

echo "OK: core/ builds and its full test suite passes with apps/ deleted entirely."

#!/usr/bin/env bash
# EPIC-B WP1.1 RED plan: "a dependency-rule check (import-graph test via `go
# list` or a lint rule) asserting core/ has zero imports from apps/ or any
# provider SDK" (docs/EPIC-B-multi-nas.md §69 WP1.1, §7.1).
#
# This is the fast, static safety net: it inspects core/'s resolved import
# graph in place, on every run, with no filesystem mutation.
# verify-core-without-apps.sh is the heavier, literal proof of the same
# claim by actually deleting apps/ in a throwaway worktree.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

if [ ! -d core ]; then
  echo "FAIL: core/ module does not exist yet (dependency rule is inapplicable)." >&2
  exit 1
fi

cd core

# GOWORK=off: resolve core/'s import graph against its own go.mod only,
# never against the repo-root go.work (which also lists ./apps/common for
# local development convenience). core/ standing alone is the claim.
#
# `go list`'s own exit status is checked BEFORE grepping its output: this
# check's entire job is to fail loudly on a boundary violation, and
# `... | grep -F '/apps/' || true` alone fails OPEN on the worst case —
# `go list` erroring for a reason that has nothing to do with apps/ (core/
# doesn't even build, say) produces error text that just doesn't match the
# grep, and `|| true` was swallowing the pipeline's real exit code and
# printing "OK." regardless.
if ! output=$(GOWORK=off go list -deps ./... 2>&1); then
  echo "FAIL: core/ (GOWORK=off) go list -deps ./... itself failed:" >&2
  echo "$output" >&2
  exit 1
fi

bad=$(printf '%s\n' "$output" | grep -F '/apps/' || true)
if [ -n "$bad" ]; then
  echo "FAIL: core/ imports code from apps/:" >&2
  echo "$bad" >&2
  exit 1
fi

echo "OK: core/ imports nothing from apps/."

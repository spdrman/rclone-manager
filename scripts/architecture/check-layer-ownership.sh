#!/usr/bin/env bash
# Ownership check (issue #165).
#
# The dependency checks next to this one answer "who may import whom". This
# one answers a different question that #165's acceptance criteria ask
# separately:
#
#   "GIVEN a provider or distribution package that attempts to hold
#    lifecycle state, retention policy, validation rules, catalog truth or
#    backup policy, WHEN the ownership check runs, THEN it fails, because
#    those may only live in the provider-neutral core."
#
# An import rule cannot catch this. A runtime profile that grows its own
# retention type imports nothing it should not; it just quietly becomes a
# second place retention is decided, which is exactly how a fork starts.
#
# The scanning itself is scripts/architecture/ownership.go, which parses Go
# rather than grepping it, because the thing being detected is a
# declaration and grep cannot tell one from a comment. This wrapper decides
# WHAT to scan: every path the layer manifest puts in the runtime-platform
# or distribution layer, so adding a platform or an adapter brings it under
# the rule automatically.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

manifest=$(arch::manifest)
if [ ! -f "$manifest" ]; then
  echo "FAIL: $manifest does not exist, so there is no way to know which paths this rule applies to." >&2
  exit 1
fi

targets=()
while IFS= read -r path; do
  [ -n "$path" ] || continue
  targets+=("$path")
done < <(arch::layer_paths platform; arch::layer_paths distribution)

if [ "${#targets[@]}" -eq 0 ]; then
  echo "FAIL: the manifest puts no path in the runtime-platform or distribution layer, so this check would scan nothing." >&2
  exit 1
fi

echo "==> scanning ${#targets[@]} runtime-platform and distribution path(s) for core-owned declarations"
GOWORK=off go run scripts/architecture/ownership.go . "${targets[@]}"

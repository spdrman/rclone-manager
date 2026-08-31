#!/usr/bin/env bash
# Move scripts/e2e/tests-repo.pin to a newer rclone-manager-tests commit.
#
#   scripts/e2e/bump-tests-pin.sh              # the pinned branch's tip
#   scripts/e2e/bump-tests-pin.sh main         # a named ref
#   scripts/e2e/bump-tests-pin.sh <full sha>   # an exact commit
#
# The bump is one line in one file, and it does not carry its own proof:
# the commit that lands it runs through .husky/pre-commit like any other,
# which runs the gate, which runs the newly pinned suites. So a pin that
# points at a red or unreachable tests commit cannot be committed without
# --no-verify. That is the whole safety story, and it is why this script
# deliberately does not "verify" anything itself: a check that runs here and
# again in the gate would just be a check that can disagree with the gate.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

pin_file="scripts/e2e/tests-repo.pin"
# shellcheck source=/dev/null
. "$pin_file"

target="${1:-}"

if [[ "$target" =~ ^[0-9a-f]{40}$ ]]; then
  sha="$target"
else
  ref="${target:-HEAD}"
  echo "==> resolving $ref in $TESTS_REPO_URL"
  sha="$(git ls-remote "$TESTS_REPO_URL" "$ref" | awk 'NR==1 {print $1}')"
  if [ -z "$sha" ]; then
    echo "bump-tests-pin: $TESTS_REPO_URL has no ref matching '$ref'." >&2
    echo "                Pass a full 40-character sha to pin one that is not a ref tip." >&2
    exit 1
  fi
fi

if [ "$sha" = "$TESTS_REPO_SHA" ]; then
  echo "==> already pinned to $sha, nothing to do"
  exit 0
fi

# In place, preserving the header: the header is the only place the pin's
# reasoning is written down, and a rewrite that dropped it would leave the
# next reader with a bare sha and no argument.
tmp="$pin_file.bump"
sed "s|^TESTS_REPO_SHA=.*|TESTS_REPO_SHA=$sha|" "$pin_file" >"$tmp"
mv "$tmp" "$pin_file"

echo "==> $TESTS_REPO_SHA"
echo "==> $sha"
echo ""
echo "Commit it. The pre-commit gate will run the newly pinned suites against this"
echo "working tree, so a bad bump fails there rather than after it has landed."

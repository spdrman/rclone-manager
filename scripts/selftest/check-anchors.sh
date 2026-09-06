#!/usr/bin/env bash
# Are the mutation anchors in the selftests still anchored to real code?
#
# scripts/compat/selftest.sh, scripts/conformance/selftest.sh,
# scripts/race/selftest.sh, scripts/format/selftest.sh and
# scripts/docs/selftest.sh plant deliberate violations to prove each cell of
# their gate can go red. Every plant is anchored to a verbatim copy of
# product source living in a script the author of the product change never
# opens, so a refactor drifts the anchor and the mutation stops planting
# anything. That is caught, loudly, but until #458 it was only caught at the
# end of a 25-minute gate run, one stale anchor at a time.
#
# This is that same check with nothing else attached: every anchor in every
# one of them, dry-run against the real tree, building nothing, in about a
# second. Belongs at the top of the gate, so drift costs seconds.
#
# Exit code contract, which is all a gate step needs from it:
#
#   0        every anchor is present exactly once
#   non-zero at least one is not, and the run printed which ones
#
# Both selftests always run, even when the first one has stale anchors, so
# one run gives the whole list. Fixing them one gate at a time is the thing
# this script exists to stop.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

status=0

for selftest in scripts/compat/selftest.sh scripts/conformance/selftest.sh scripts/race/selftest.sh scripts/format/selftest.sh scripts/docs/selftest.sh; do
  echo "==> $selftest --check-anchors"
  if ! bash "$selftest" --check-anchors; then
    status=1
  fi
  echo
done

if [ "$status" -ne 0 ]; then
  echo "FAIL: at least one mutation anchor no longer matches the tree it names." >&2
  echo "      Each STALE ANCHOR above is a control that would plant nothing, so re-anchor" >&2
  echo "      it to the code as it is now rather than deleting it." >&2
  exit 1
fi

echo "OK: every mutation anchor and precondition in the compat, conformance, race, format and docs selftests still matches the real tree."

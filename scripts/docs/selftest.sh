#!/usr/bin/env bash
# Controls for the package-documentation gate (issue #526).
#
# #526 is a check that did not exist. Six documentation lanes moved file
# openers to sit immediately above `package`, which is where go/doc reads
# the package overview from, and every gate step stayed green through all
# of it, because nothing in this repository ever assembled a package
# overview and looked at it. `go doc ./core/service` opened with
# "This file is the operator's activity feed" for days.
#
# scripts/docs/check-package-doc.sh is the check that now does. This is the
# proof that it can go red, and it is deliberately paired: the same
# mutation runs past a second, independent check that has to STAY SILENT.
#
#   G2  a comment promoted to sit adjacent to `package`
#       -> check-package-doc.sh goes red and names the file
#   G3  the same mutation, unchanged
#       -> check-comments-only.sh stays green
#
# G3 is not decoration. Without it, "check-package-doc.sh went red" is also
# what you would see from a checker that fires on any edit at all, and the
# two halves of this fix would not be measuring different things. The
# promotion changes what `go doc` prints and changes no token in the file,
# so the pair proves each check answers its own question. G4 is G3's
# positive control, because a checker that never speaks is also silent.
#
# The mutation goes into the real core/service/activity.go and is put back,
# rather than into a copy. That file is the one #526 is named for, and a
# control planted in a synthetic fixture would not notice the day somebody
# promotes its opener again for real. Restoration is a trap, and G5 checks
# it actually happened rather than trusting it.
#
# `bash scripts/docs/selftest.sh --check-anchors` dry-runs the anchors
# against the real tree, building nothing and writing nothing, which is
# what scripts/selftest/check-anchors.sh runs.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

root=$(pwd)
# shellcheck source=scripts/lib/selftest-swap.sh
. "$root/scripts/lib/selftest-swap.sh"
selftest_parse_args "$@"

pkgdoc=$root/scripts/docs/check-package-doc.sh
commentsonly=$root/scripts/docs/check-comments-only.sh

# The file the controls plant into, and the package whose overview it would
# join. Kept here as data so the messages below and the mutations cannot
# drift apart.
SUBJECT=core/service/activity.go
SUBJECT_PACKAGE=core/service

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-docs-selftest.XXXXXX")
pristine=$tmp/activity.go.pristine

restore() {
  if [ -f "$pristine" ]; then
    cp "$pristine" "$root/$SUBJECT"
  fi
}
trap 'restore; rm -rf "$tmp"' EXIT

pass=0
fail=0

# Every verdict goes through one of these two, so the run ends with a count
# it can check rather than with an impression.
ok()  { echo "  ok:   $1"; pass=$((pass + 1)); }
bad() { echo "SELFTEST FAIL: $1" >&2; shift; [ $# -gt 0 ] && printf '%s\n' "$1" | sed 's/^/    /' >&2; fail=$((fail + 1)); }

# ----------------------------------------------------------- the anchors

# A1 is the fix itself: activity.go's opener sits below the imports, where
# it is a file comment. The day it goes back above `package`, this stops
# matching and says so, which is the drift that matters most here.
OPENER_BELOW_IMPORTS=')

// This file is the operator'"'"'s activity feed: the read side of the'

# A2 is where the promotion plants. Anchored on the package clause and the
# import block together, so it can only land in one place.
PACKAGE_CLAUSE='package service

import ('

PROMOTED='// A file opener promoted to package documentation by
// scripts/docs/selftest.sh (issue #526). If you are reading this in a real
// checkout, that self-test did not get to put the file back:
//
//	git checkout -- core/service/activity.go
package service

import ('

# A3 is the token mutation G4 needs: a real identifier, renamed, so the
# comments-only check has something to find.
LISTACTIVITY='func (b *BackupService) ListActivity(ctx context.Context, limit int) ([]ActivityEvent, error) {'
LISTACTIVITY_RENAMED='func (b *BackupService) ListActivityRenamedBySelftest(ctx context.Context, limit int) ([]ActivityEvent, error) {'

echo "==> anchors in $SUBJECT"
swap_dry "$root/$SUBJECT" "$OPENER_BELOW_IMPORTS"
swap_dry "$root/$SUBJECT" "$PACKAGE_CLAUSE"
swap_dry "$root/$SUBJECT" "$LISTACTIVITY"
if [ "$selftest_anchors_stale" -eq 0 ]; then
  ok "all three anchors match exactly once"
else
  bad "an anchor no longer matches the tree it names:" "$selftest_stale_pending
Re-aim it at the code as it is now rather than deleting the control."
fi

if [ "$selftest_dry_run" = 1 ]; then
  echo
  if [ "$fail" -eq 0 ]; then
    echo "==> docs selftest anchors: ok (3 anchors checked)"
    exit 0
  fi
  echo "==> docs selftest anchors: FAILED" >&2
  exit 1
fi

cp "$root/$SUBJECT" "$pristine"

# ------------------------------------------------------------------- G1

echo
echo "==> G1 negative control: the real tree passes both checks"
if bash "$pkgdoc" >"$tmp/out" 2>&1; then
  ok "check-package-doc.sh is green on the unmutated tree, so G2's red means something"
else
  bad "check-package-doc.sh FAILS on the unmutated tree, so its failures say nothing" "$(cat "$tmp/out")"
fi

# G3 asks whether one file's change against HEAD is comments-only, so this
# control has to establish that the file is at HEAD to begin with. A
# developer mid-edit is not a failure of the check; it is a reason its
# answer would be about their edit instead of about the plant.
if git diff --quiet HEAD -- "$SUBJECT"; then
  ok "$SUBJECT matches HEAD, so G3 and G4 will be answering about the plant"
  subject_clean=1
else
  bad "$SUBJECT has uncommitted changes, so G3 and G4 would be measuring those instead" \
    "$(git diff --stat HEAD -- "$SUBJECT")"
  subject_clean=0
fi

# ------------------------------------------------------------------- G2

echo
echo "==> G2 a comment promoted adjacent to \`package\` turns the package-doc check red"
swap "$root/$SUBJECT" "$PACKAGE_CLAUSE" "$PROMOTED"
if [ -n "$selftest_stale_pending" ]; then
  bad "the promotion refused to plant:" "$selftest_stale_pending"
elif bash "$pkgdoc" >"$tmp/out" 2>&1; then
  bad "check-package-doc.sh PASSED with a file opener sitting in the package overview, which is #526 exactly" "$(cat "$tmp/out")"
elif ! grep -qF "$SUBJECT_PACKAGE" "$tmp/out"; then
  bad "it went red but never named the package" "$(cat "$tmp/out")"
elif ! grep -qF 'activity.go' "$tmp/out"; then
  bad "it named the package but not the file that started carrying the comment" "$(cat "$tmp/out")"
else
  ok "red, and it names both $SUBJECT_PACKAGE and activity.go"
fi

# ------------------------------------------------------------------- G3

echo
echo "==> G3 independence: the same mutation leaves the token-level check silent"
if [ "$subject_clean" != 1 ]; then
  bad "skipped, because G1 found $SUBJECT already modified"
elif [ -n "$selftest_stale_pending" ]; then
  bad "skipped, because the promotion above did not plant"
elif bash "$commentsonly" HEAD --only "$SUBJECT" >"$tmp/out" 2>&1; then
  ok "green: the promotion changes what go doc prints and changes no token, so the two checks are asking different questions"
else
  bad "check-comments-only.sh went RED on a pure comment move, so it is not the independent second opinion G2 needs" "$(cat "$tmp/out")"
fi

restore
selftest_stale_pending=""

# ------------------------------------------------------------------- G4

echo
echo "==> G4 positive control for G3: a real token change does turn it red"
swap "$root/$SUBJECT" "$LISTACTIVITY" "$LISTACTIVITY_RENAMED"
if [ "$subject_clean" != 1 ]; then
  bad "skipped, because G1 found $SUBJECT already modified"
elif [ -n "$selftest_stale_pending" ]; then
  bad "the rename refused to plant:" "$selftest_stale_pending"
elif bash "$commentsonly" HEAD --only "$SUBJECT" >"$tmp/out" 2>&1; then
  bad "check-comments-only.sh PASSED a renamed function, so G3's silence proves nothing" "$(cat "$tmp/out")"
elif ! grep -qF "$SUBJECT" "$tmp/out"; then
  bad "it went red but never named the file" "$(cat "$tmp/out")"
else
  ok "red, and it names $SUBJECT"
fi

restore

# ------------------------------------------------------------------- G5

echo
echo "==> G5 the real file is back the way it was"
if git diff --quiet HEAD -- "$SUBJECT"; then
  ok "$SUBJECT matches HEAD again"
elif [ "$subject_clean" != 1 ]; then
  ok "$SUBJECT still carries the local edit G1 found, and nothing this script planted"
else
  bad "$SUBJECT is still modified, so a mutation escaped. Put it back with: git checkout -- $SUBJECT" \
    "$(git diff HEAD -- "$SUBJECT" | head -30)"
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "==> docs selftest: ok ($pass controls)"
  exit 0
fi
echo "==> docs selftest: $fail failed, $pass passed" >&2
exit 1

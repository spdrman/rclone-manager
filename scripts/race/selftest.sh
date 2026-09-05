#!/usr/bin/env bash
# Positive controls for the gate's race detector (issue #417).
#
# `-race` is a check with the same shape as every other one this
# repository has had to learn to distrust: it passes silently, it passes
# when it is not there at all, and the difference between "this tree has
# no data race" and "nobody asked" is invisible in the output. The gate
# ran no `-race` anywhere until #417, and the one test in this repository
# whose own doc says it only means something under the detector
# (service.TestCreateBackupSet_ConcurrentWithReadersDoesNotRace, written
# for PR #155's mandatory review) had therefore never once been run the
# way it says it has to be.
#
# So the detector gets the same treatment the FR-35 compatibility cells
# and the composed conformance cells got in #242: a real data race is
# planted in real product source, in a copy of the working tree, and the
# step has to catch it. Three cells, because catching it is only one of
# the three things that have to be true:
#
#   R1  the unmutated tree is clean under -race. Without this, R2 would
#       also pass against a tree that is red for some unrelated reason.
#   R2  the planted race is caught, and the report names the write that
#       plants it. A bare non-zero exit is not enough: a mutant that
#       failed to compile would give one.
#   R3  the same mutant is INVISIBLE without -race. This is the cell that
#       makes the other two mean something: it proves the detector is
#       what caught the race, not an assertion that would have caught it
#       anyway, which is the whole claim #417 is buying.
#
# Whether the gate still ASKS for the detector is a different question and
# is answered somewhere cheaper: Group K of scripts/tests/ci-local-gate.test.sh
# scans scripts/ci-local.sh for a Go suite that runs without -race, and
# proves that scan can fail. This file is about whether the detector has
# teeth once it is asked.
#
# The plant is anchored to a verbatim copy of product source, tabs and
# all, in the shared way scripts/compat/selftest.sh and
# scripts/conformance/selftest.sh already are, so a refactor that moves
# the code cannot leave this control quietly planting nothing.
# `bash scripts/race/selftest.sh --check-anchors` is that drift check on
# its own, and scripts/selftest/check-anchors.sh (#458) runs it for every
# selftest that has anchors.
#
# Cost: one package, one test, three runs. The mutant is one file deep in
# a leaf package and Go's build cache is content-addressed, so the copies
# rebuild `service` and nothing under it.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

. scripts/lib/selftest-swap.sh
selftest_parse_args "$@"

root=$(pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-race-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# The one test that exercises the shape #417 is about: CreateBackupSet
# hot-reloading {inner, revision} while 150 goroutines read it. Its own
# doc says it proves nothing without the detector, which is exactly why
# it is the vehicle here.
RACE_TEST='^TestCreateBackupSet_ConcurrentWithReadersDoesNotRace$'

# mutant <name> copies the working tree into $tmp/<name> and echoes its
# path. --cached --others --exclude-standard, so an uncommitted gate (the
# usual state while one is being written) is the gate under test.
mutant() {
  local name=$1
  local dir="$tmp/$name"
  if [ "$selftest_dry_run" = 1 ]; then
    printf '%s' "$root"
    return 0
  fi
  mkdir -p "$dir"
  (cd "$root" && git ls-files -z --cached --others --exclude-standard | tar -cf - --null -T -) | (cd "$dir" && tar -xf -)
  printf '%s' "$dir"
}

# race_gate <dir> runs the detector over the racing test in <dir>, the
# same way the gate's own core step runs it over the whole package set.
race_gate() {
  (cd "$1/core" && GOWORK=off go test -race -count=1 -run "$RACE_TEST" ./service/)
}

# plain_gate <dir> is the identical run with the detector off, which is
# what the gate did before #417.
plain_gate() {
  (cd "$1/core" && GOWORK=off go test -count=1 -run "$RACE_TEST" ./service/)
}

# expect_race_caught <label> <dir> <needle>
# Red is not enough. The report has to name the racing access, or a
# mutant that simply broke the build reads as this control passing.
expect_race_caught() {
  local label=$1 dir=$2 needle=$3
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if race_gate "$dir" >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $label. -race PASSED against a planted data race." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
    return 0
  fi
  if ! grep -qF 'WARNING: DATA RACE' "$tmp/out"; then
    echo "SELFTEST FAIL: $label. The run failed, but not with a data race report, so something else broke." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
    return 0
  fi
  if ! grep -qF "$needle" "$tmp/out"; then
    echo "SELFTEST FAIL: $label. A race was reported, but not the one that was planted." >&2
    echo "    expected the report to name: $needle" >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
    return 0
  fi
  echo "  ok (caught):    $label"
  pass=$((pass + 1))
}

# expect_race_missed <label> <dir>
# The other direction, and the reason the other cells mean anything: the
# same tree, the same test, the detector off, and it must go green.
expect_race_missed() {
  local label=$1 dir=$2
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if plain_gate "$dir" >"$tmp/out" 2>&1; then
    echo "  ok (missed):    $label"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $label. The planted race was caught WITHOUT -race, so this corpus does not measure the detector." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

expect_gate_passes() {
  local label=$1 dir=$2
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if race_gate "$dir" >"$tmp/out" 2>&1; then
    echo "  ok (clean):     $label"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $label. -race FAILED against an unmutated tree, so its failures mean nothing." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

# plant_revision_cache_race <dir>
#
# The mutation: memoise the configuration revision in a plain field on
# BackupService, and have every caller of ConfigRevision write it. Two
# anchors, both in service.go, because the field and the write to it are
# in different places.
#
# It is a real mistake rather than a contrived one. Caching a computed
# value in a struct field is the most ordinary optimisation there is, and
# this is the exact defect service.go's own doc records this code having
# had once: "inner and revision were two plain fields written under
# configMu but read by every one of those call sites with no lock at all".
# The mutant compiles, vets, and returns the same string; nothing about it
# is observable except through the memory model, which is what R3 rests
# on. computeConfigRevision returns a fixed 16-character hex digest, so
# even a torn read of that string header cannot produce an out-of-bounds
# slice, which is why the un-instrumented run is not merely usually green
# but reliably so.
#
# # Why it is here rather than in adoptConfig, which is where it started
#
# The first version of this control planted the same class of race in
# adoptConfig: publish by mutating the configState every reader already
# holds instead of swapping in a new one. That is the more evocative shape,
# and it worked on this branch, 10 runs out of 10.
#
# It stopped working the moment this branch was composed with the other
# lanes of its wave, and the reason is worth writing down, because it is
# the same failure this whole file exists to catch. #411's create-path
# repoint check added two journal queries to CreateBackupSet BEFORE it
# reaches adoptConfig. That pushed the planted write past the point where
# the 150 reader goroutines in the racing test had finished, and
# ThreadSanitizer keeps only a few shadow entries per word, so the readers'
# accesses had aged out by the time the write landed. The race was still
# there in the code; the detector simply had nothing left to compare it
# against. Measured: 0 catches in 10 runs composed, 10 in 10 with that
# one new pre-write check short-circuited.
#
# So the old plant's visibility depended on a timing overlap it did not
# control, and any lane adding work ahead of adoptConfig could silently
# make this control prove nothing. The cell did fail loudly rather than go
# green, which is the one thing that went right, but a control that has to
# be re-earned every time somebody edits CreateBackupSet is not a control.
#
# ConfigRevision has no such dependence. The racing test calls it from
# fifty goroutines directly and fifty more through SubmitRunCycle, all
# concurrent with each other by construction rather than by scheduling, so
# a write in there races roughly a hundred ways at once no matter what any
# other code path does first. Verified 10 of 10 on this branch AND 10 of 10
# on the composed tree that broke the old one, with the un-instrumented
# run green 10 of 10 in both.
plant_revision_cache_race() {
  # The field the mutant caches into. Nothing reads it except the method
  # below; it exists so the plant has somewhere unsynchronised to write.
  swap "$1/core/service/service.go" \
'	state atomic.Pointer[configState]

	journal *state.Journal' \
'	state atomic.Pointer[configState]

	// PLANTED DATA RACE (scripts/race/selftest.sh). The memoised
	// revision ConfigRevision writes below, in a plain field, exactly
	// as inner and revision themselves were before #155 made them one
	// atomic pointer.
	plantedRevisionCache string

	journal *state.Journal'

  swap "$1/core/service/service.go" \
'func (b *BackupService) ConfigRevision() string {
	return b.state.Load().revision
}' \
'func (b *BackupService) ConfigRevision() string {
	// PLANTED DATA RACE (scripts/race/selftest.sh). One read turned
	// into a write, on a field every concurrent caller shares.
	b.plantedRevisionCache = b.state.Load().revision
	return b.plantedRevisionCache
}'
}

echo "==> R1 negative control: the racing test is clean under -race on the real tree"
expect_gate_passes "core/service under -race, unmutated" "$root"

echo
echo "==> R2 the planted race is caught, and named"
d=$(mutant revision-cached-in-a-plain-field)
plant_revision_cache_race "$d"
# The write side of the planted race, named in the report's own stack. A
# bare non-zero exit is not enough: a mutant that failed to compile gives
# one too.
expect_race_caught "a revision memoised into a plain field every reader writes" "$d" \
  "core/service.(*BackupService).ConfigRevision"

echo
echo "==> R3 the same mutant is invisible without the detector"
d=$(mutant revision-cached-in-a-plain-field-no-race)
plant_revision_cache_race "$d"
expect_race_missed "the same tree, the same test, -race off" "$d"

echo
selftest_stale_summary
if [ "$fail" -eq 0 ] && [ "$selftest_stale_count" -eq 0 ]; then
  if [ "$selftest_dry_run" = 1 ]; then
    echo "==> race selftest anchors: ok ($selftest_anchors_checked checked)"
  else
    echo "==> race selftest: ok ($pass controls)"
  fi
  exit 0
fi
echo "==> race selftest: $fail failed, $selftest_stale_count stale, $pass passed" >&2
exit 1

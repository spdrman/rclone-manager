#!/usr/bin/env bash
# Positive controls for FR-20's retention apply (issue #602).
#
# core/service/retentionapplyevidence_test.go and
# core/internal/retention/lastknowngoodpresence_test.go are the evidence
# that an apply removes exactly the previewed DELETE set and nothing else.
# An assertion nobody has watched fail is indistinguishable from one that
# cannot fail, and a delete is the assertion where that costs the most:
# the cheapest way to pass "every previewed DELETE is gone, every KEEP
# survives" is to delete nothing, or to plan nothing.
#
# So every claim that evidence makes is mutation-tested here against the
# real tree: a copy of the working tree gets one deliberate violation
# planted in a real product file, the suite runs, and it must fail AND
# name the check whose promise the violation broke. Naming the check, not
# merely failing, is what stops a mutation that broke the build for an
# unrelated reason from reading as a pass. scripts/conformance/selftest.sh
# and scripts/compat/selftest.sh are the models and this is deliberately
# their sibling.
#
# The last control is the one to read first. It plants its violation in
# the TEST, not in the product: retentionEvidenceCompare is made to return
# no complaints at all, and the suite still has to go red, because the
# in-test positive control is what proves the comparison would notice a
# file the plan never named going missing. Without that, this whole file
# would be certifying a guard whose own guard nobody had checked.
#
# Every mutation below is anchored to a verbatim copy of product source,
# tabs and all, which means a refactor over there can leave an anchor here
# naming code that is no longer in the tree. That is the third verdict,
# STALE ANCHOR: the control is skipped, because a tree with nothing
# planted in it would pass and reading that as a pass is the exact failure
# this file exists to rule out, and the run carries on so one run names
# every stale control instead of dying on the first (#458).
#
# `bash scripts/retention/selftest.sh --check-anchors` is that check on
# its own: every anchor against the real tree, building nothing, in about
# a second. scripts/selftest/check-anchors.sh runs it alongside the others.
#
# This is not fast. Each mutant rebuilds core/ and runs a narrowed slice
# of three packages, so budget a few minutes. It needs no Docker and no
# container.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

. scripts/lib/selftest-swap.sh
selftest_parse_args "$@"

root=$(pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-retention-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# mutant <name> copies the working tree into $tmp/<name> and echoes its
# path.
#
# --cached --others --exclude-standard, not plain `git ls-files`: the copy
# has to include files that are present but not yet committed, because the
# suite being tested is itself usually uncommitted while it is being
# written. Copying tracked files only is what produced a self-test
# elsewhere in this repository that silently "caught" every mutation,
# because the check it invoked did not exist in the copy at all.
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

# retention_gate runs the evidence in whichever tree it is called from.
#
# The three packages are the three places the claim lives: core/service is
# the envelope an operator reaches through, core/internal/retention is
# where the deletion and FR-19's confirmation happen, and
# cmd/backup-manager is the verb. -run narrows to the checks a mutation is
# expected to move, per call, so the whole file does not cost a full suite
# per control.
retention_gate() {
  local pattern=${1:-.}
  (cd core && GOWORK=off go test -count=1 -timeout 15m -run "$pattern" ./service/ ./internal/retention/ ./cmd/backup-manager/)
}

# expect_gate_fails <label> <dir> <expected substring> [run pattern]
expect_gate_fails() {
  local label=$1 dir=$2 needle=$3 pattern=${4:-.}
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if (cd "$dir" && retention_gate "$pattern") >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $label. The evidence PASSED against a planted violation." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  elif ! grep -qF "$needle" "$tmp/out"; then
    echo "SELFTEST FAIL: $label. The suite failed, but never named the promise that was broken." >&2
    echo "    expected its output to mention: $needle" >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  else
    echo "  ok (caught): $label"
    echo "      -> $(grep -m1 -F "$needle" "$tmp/out" | sed 's/^[[:space:]]*//' | cut -c1-400)"
    pass=$((pass + 1))
  fi
}

# The negative control. A mutation that turns a suite red proves nothing
# about the mutation if the suite was red already, and the two early
# returns matter for the reason scripts/conformance/selftest.sh gives:
# a control whose anchor no longer matches planted nothing, and under
# --check-anchors nothing is built at all.
expect_gate_passes() {
  local label=$1 dir=$2
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if (cd "$dir" && retention_gate 'TestRetentionApplyEvidence|TestPruneRefusesEveryDelete|TestPruneStillDeletes|TestPruneDeletesWhenTheOperator|TestPruneConfirmsA|TestRun_RetentionApply') >"$tmp/out" 2>&1; then
    echo "  ok (clean):  $label"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $label. The evidence FAILED against an unmutated tree, so its failures mean nothing." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

echo "==> negative control: the retention apply evidence is clean on the real tree"
expect_gate_passes "the retention apply evidence on an unmutated tree" "$root"

echo
echo "==> the apply carries out the plan"

# The cheapest way to pass every "what survived" assertion ever written is
# to delete nothing. This is that engine.
d=$(mutant apply-deletes-nothing)
swap "$d/core/internal/retention/prune.go" \
  '		if err := os.Remove(safePath); err != nil {' \
  '		// PLANTED VIOLATION (scripts/retention/selftest.sh): an apply
		// that confirms a plan and then carries none of it out.
		if err := func(string) error { return nil }(safePath); err != nil {'
expect_gate_fails "an apply that deletes nothing at all" "$d" \
  "is still on disk and the plan marked it DELETE" \
  'TestRetentionApplyEvidence_RemovesExactlyThePreviewedDeleteSet'

echo
echo "==> the apply removes ONLY what the plan named"

# The other direction, and the one an assertion written the obvious way
# cannot see: it only ever looks up the paths the plan already named, so
# an apply that reached past its plan is invisible to it. The planted
# engine takes the whole directory, which is what "positively identified"
# is there to make impossible.
d=$(mutant apply-reaches-past-its-plan)
swap "$d/core/internal/retention/prune.go" \
  '		if err := os.Remove(safePath); err != nil {' \
  '		// PLANTED VIOLATION (scripts/retention/selftest.sh): an apply
		// that clears the directory rather than the artifact, which is
		// the FR-20 guarantee that only a journal record can identify a
		// file to this package at all.
		if err := func(p string) error {
			dir := filepath.Dir(p)
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				return readErr
			}
			for _, e := range entries {
				if !e.IsDir() {
					_ = os.Remove(filepath.Join(dir, e.Name()))
				}
			}
			return nil
		}(safePath); err != nil {'
expect_gate_fails "an apply that removes a file no verdict named" "$d" \
  "was removed and no verdict in the plan named it" \
  'TestRetentionApplyEvidence_RemovesExactlyThePreviewedDeleteSet'

echo
echo "==> FR-20: a symlink at a final path is refused, never followed"

# Stat THROUGH the link rather than at it, which is the naive
# implementation this check exists to rule out and the one a refactor
# would plausibly reach for. Deleting the symlink branch on its own
# changes nothing, and finding that out is what this control is for: the
# IsRegular check immediately below it already refuses anything whose
# Lstat mode is not a regular file, so on the local path the two are
# redundant and only the Lstat/Stat choice is load-bearing.
d=$(mutant symlink-followed)
swap "$d/core/internal/retention/prune.go" \
  '	info, err := os.Lstat(expected)' \
  '	// PLANTED VIOLATION (scripts/retention/selftest.sh): stat through
	// the link rather than at it, so a symlink at an artifact FINAL path
	// reports as the regular file it points at.
	info, err := os.Stat(expected)'
expect_gate_fails "a symlink at an artifact's final path treated as the artifact" "$d" \
  "whose final path is a symlink" \
  'TestRetentionApplyEvidence_ASymlinkAtAnArtifactsPathIsRefusedNotFollowed|TestPruneRefusesSymlinkAtFinalPath'

echo
echo "==> FR-19 and FR-30: the last known good"

# Planted at the DECISION rather than at its application, because
# pruneEvaluate re-checks the protection independently and turns a verdict
# that contradicts it into a REFUSE, which is
# TestPruneRefusesLastKnownGoodEvenIfGFSVerdictLies' whole subject and
# means a Keep flipped to false is caught by that redundancy rather than
# by this evidence. A protection that is never DECIDED has nothing left to
# catch it, and that is the shape where the newest restore point in a set
# nothing else keeps is deleted along with everything else.
d=$(mutant last-known-good-never-decided)
swap "$d/core/internal/retention/lastknowngood.go" \
  '	result.Protected = true
	result.Artifact = newest.artifact' \
  '	// PLANTED VIOLATION (scripts/retention/selftest.sh): FR-19 finds the
	// newest eligible restore point and then protects nothing.
	result.Protected = false
	result.Artifact = newest.artifact'
expect_gate_fails "the newest restore point found and then not protected" "$d" \
  "this backup set's last known good" \
  'TestRetentionApplyEvidence_LastKnownGoodSurvivesATierVerdictThatWouldRemoveIt|TestPruneDecideKeepsLastKnownGoodArtifact'

# And the confirmation that the thing being protected is a copy rather
# than a row (#602). Without it, a set whose newest file was lost out of
# band is emptied under a plan that reports a restore point was kept.
d=$(mutant last-known-good-copy-not-confirmed)
swap "$d/core/internal/retention/prune.go" \
  '	if !lkg.Protected {
		return ""
	}' \
  '	// PLANTED VIOLATION (scripts/retention/selftest.sh): the FR-30
	// confirmation short-circuited, so a last known good whose file is
	// gone still authorises deleting everything else.
	if true {
		return ""
	}'
expect_gate_fails "a last known good confirmed from its journal row alone" "$d" \
  "FR-30" \
  'TestRetentionApplyEvidence_NothingIsRemovedWhenTheLastKnownGoodCopyIsGone|TestPruneRefusesEveryDeleteWhenTheLastKnownGood'

echo
echo "==> the CLI verb's confirmation"

d=$(mutant acknowledge-not-required)
swap "$d/core/cmd/backup-manager/retentionapply.go" \
  '	acknowledge := fs.Bool("acknowledge", false,' \
  '	// PLANTED VIOLATION (scripts/retention/selftest.sh): the
	// confirmation defaulted to given, so a mistyped command line
	// deletes restore points.
	acknowledge := fs.Bool("acknowledge", true,'
expect_gate_fails "an apply verb whose confirmation is assumed rather than asked for" "$d" \
  "want 2" \
  'TestRun_RetentionApplyRefusesWithoutTheAcknowledgement'

echo
echo "==> the guard on the guard"

# The one that is planted in the test rather than in the product, and the
# reason every control above means anything. If the comparison cannot
# fail, an apply that deleted the wrong thing passes every clause in the
# evidence, and this file would be certifying a wall of assertions that
# had quietly stopped asserting.
d=$(mutant comparison-cannot-fail)
swap "$d/core/service/retentionapplyevidence_test.go" \
  'func retentionEvidenceCompare(before, after map[string]retentionEvidenceEntry, wantRemoved []string) []string {
	var complaints []string' \
  'func retentionEvidenceCompare(before, after map[string]retentionEvidenceEntry, wantRemoved []string) []string {
	// PLANTED VIOLATION (scripts/retention/selftest.sh): the whole
	// comparison made vacuous. Every assertion built on it still passes;
	// only its own positive control can see this.
	if true {
		return nil
	}
	var complaints []string'
expect_gate_fails "an exact-set comparison that cannot report anything wrong" "$d" \
  "which makes every other assertion in this file worthless" \
  'TestRetentionApplyEvidence_TheComparisonNoticesWhatItIsAskedTo'

selftest_stale_summary
if [ "$fail" -ne 0 ] || [ "$selftest_stale_count" -ne 0 ]; then
  echo >&2
  echo "FAIL: $((fail + selftest_stale_count)) of $((pass + fail + selftest_stale_count)) retention-apply controls did not behave as required ($fail reached the wrong verdict, $selftest_stale_count could not plant their violation at all)." >&2
  exit 1
fi
echo
if [ "$selftest_dry_run" = 1 ]; then
  echo "OK: all $selftest_anchors_checked retention-apply mutation anchors still name code that is in this tree."
else
  echo "OK: all $pass retention-apply controls behaved as required (every claim was shown to go red against a real planted violation, and shown not to on the real tree)."
fi

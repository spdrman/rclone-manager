#!/usr/bin/env bash
# Positive controls for the UGOS resource-certification harness
# (issue #89, D2.1).
#
# distribution/hwcert decides whether a set of numbers taken off a real
# UGREEN unit is a pass. Nobody will be standing next to it when it does:
# the hardware run happens once, on a device most reviewers cannot reach,
# and whatever the harness prints is what goes in the evidence record. So
# the question worth answering before that day is not "does it pass on the
# fixture", it is "can it fail at all".
#
# Four ways it could stop being able to, each planted in real product
# source in a copy of the working tree, each of which has to turn a
# specific test red:
#
#   H1  a measurement over its threshold is not caught
#   H2  a missing measurement is filled in with a benign default
#   H3  a record for one architecture is accepted as the other's evidence
#   H4  "the app is not running" is read as "the app is idle"
#
# H4 is the one that could not be caught any other way. An absent process
# and an idle process report the same CPU, so a harness that stopped
# checking whether the process was there would certify an app that was
# never running, and every number in the record would be real.
#
# H0 is the negative control: the unmutated package has to be green, or
# the four reds below say nothing about the mutations.
#
# Every plant is anchored to a verbatim copy of product source, tabs and
# all, the same way scripts/race/selftest.sh and its siblings are, so a
# refactor that moves the code cannot leave a control quietly planting
# nothing. `bash scripts/hwcert/selftest.sh --check-anchors` is that drift
# check on its own, and scripts/selftest/check-anchors.sh runs it for
# every selftest that has anchors.
#
# Cost: one package, five short runs, no Docker, no network, no device.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

. scripts/lib/selftest-swap.sh
selftest_parse_args "$@"

root=$(pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-hwcert-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# mutant <name> copies the working tree into $tmp/<name> and echoes its
# path. --cached --others --exclude-standard, so an uncommitted harness
# (the usual state while one is being written) is the harness under test.
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

# hwcert_gate <dir> [run-pattern] runs the harness's own suite.
hwcert_gate() {
  local dir=$1 pattern=${2:-}
  if [ -n "$pattern" ]; then
    (cd "$dir/distribution" && GOWORK=off go test -count=1 -run "$pattern" ./hwcert/)
  else
    (cd "$dir/distribution" && GOWORK=off go test -count=1 ./hwcert/)
  fi
}

# expect_green <label> <dir> is the negative control's verdict.
expect_green() {
  local label=$1 dir=$2
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if hwcert_gate "$dir" >"$tmp/out" 2>&1; then
    echo "  ok (clean):     $label"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $label. The harness suite is red on an unmutated tree, so its reds below mean nothing." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

# expect_red <label> <dir> <run-pattern> <test-name>...
#
# Red is not enough on its own. A mutant that failed to compile is also
# red, and so is one that broke some unrelated assertion, so the named
# tests have to be the ones reported failing.
expect_red() {
  local label=$1 dir=$2 pattern=$3
  shift 3
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if hwcert_gate "$dir" "$pattern" >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $label. The harness PASSED against a planted defect." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
    return 0
  fi
  if grep -qE '^(# |.*\.go:[0-9]+:[0-9]+: )' "$tmp/out" && ! grep -q '^--- FAIL' "$tmp/out"; then
    echo "SELFTEST FAIL: $label. The mutant did not compile, so nothing was measured." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
    return 0
  fi
  local name
  for name in "$@"; do
    if ! grep -qF -- "--- FAIL: $name" "$tmp/out"; then
      echo "SELFTEST FAIL: $label. The suite went red, but $name is not among the tests that failed." >&2
      sed 's/^/    /' "$tmp/out" >&2
      fail=$((fail + 1))
      return 0
    fi
  done
  echo "  ok (caught):    $label"
  pass=$((pass + 1))
}

# H1. A unit slip in the comparison, which is the most ordinary way a
# threshold check stops checking: the arithmetic is still there, the
# report still prints, and nothing is ever outside its budget again.
plant_threshold_that_cannot_be_exceeded() {
  swap "$1/distribution/hwcert/verify.go" \
'			over := m.Direction == LowerIsBetter && value > threshold
			under := m.Direction == HigherIsBetter && value < threshold' \
'			// PLANTED (scripts/hwcert/selftest.sh): a threshold a
			// thousand times looser than the one the procedure
			// documents, which is what a unit slip looks like.
			over := m.Direction == LowerIsBetter && value > threshold*1000
			under := m.Direction == HigherIsBetter && value < threshold/1000'
}

# H2. The benign default. Both halves of it, because a missing number can
# arrive two ways: as a metric the record never carried, and as an input a
# derived ratio needed.
plant_missing_measurement_defaults() {
  swap "$1/distribution/hwcert/verify.go" \
'	v, ok := rec.Measurements[name]
	if !ok {
		return 0, false, "no measurement recorded, and an absent measurement is not a pass"
	}
	return v, true, ""' \
'	// PLANTED (scripts/hwcert/selftest.sh): an absent measurement
	// reads as a zero that happens to be inside every lower_is_better
	// budget.
	return rec.Measurements[name], true, ""'

  swap "$1/distribution/hwcert/verify.go" \
'		switch {
		case !numOK:
			return 0, false, fmt.Sprintf("not derivable: the record has no %s to divide", d.Numerator)
		case !denOK:
			return 0, false, fmt.Sprintf("not derivable: the record has no %s to divide by", d.Denominator)
		case den == 0:
			return 0, false, fmt.Sprintf("not derivable: %s is zero, so the ratio has no value", d.Denominator)
		}
		return num / den, true, ""' \
'		// PLANTED (scripts/hwcert/selftest.sh): a ratio with a
		// missing side falls back to 1, which passes every ratio
		// threshold in the procedure.
		if !numOK || !denOK || den == 0 {
			return 1, true, ""
		}
		return num / den, true, ""'
}

# H3. The architecture guard, both witnesses: the one Verify checks and
# the one Validate checks. Removing either on its own leaves the other
# holding the line, which is the point of having two.
plant_one_architecture_standing_in_for_the_other() {
  swap "$1/distribution/hwcert/verify.go" \
'	if rec.Architecture != arch {
		return nil, fmt.Errorf("this is a %s evidence record and it is being read as %s evidence; an %s pass never stands in for an %s claim",
			rec.Architecture, arch, rec.Architecture, arch)
	}' \
'	// PLANTED (scripts/hwcert/selftest.sh): any record verifies
	// against any architecture the procedure claims.'

  swap "$1/distribution/hwcert/record.go" \
'	if r.Probe.GOARCH == "" {
		add("probe.goarch is empty, so nothing but the operator says which architecture this is")
	} else if r.Architecture != "" && r.Probe.GOARCH != r.Architecture {
		add("probe.goarch is %q and architecture is %q; a probe built for one architecture cannot produce a record for the other", r.Probe.GOARCH, r.Architecture)
	}' \
'	// PLANTED (scripts/hwcert/selftest.sh): the probe binary no longer
	// has to have been built for the architecture it is recording.
	if r.Probe.GOARCH == "" {
		add("probe.goarch is empty, so nothing but the operator says which architecture this is")
	}'
}

# H4. The one that cannot be caught by looking at the numbers. Skip the
# presence check and a window in which the process was never there
# averages to a very good idle CPU figure.
plant_absent_process_reads_as_idle() {
  swap "$1/distribution/hwcert/series.go" \
'	for i, s := range w.Samples {
		if !s.Present {
			return LivenessNotRunning, fmt.Sprintf("sample %d at %.0fs found no process; an absent process uses no CPU, which is not the same fact as an idle one", i, s.AtSeconds)
		}
	}' \
'	// PLANTED (scripts/hwcert/selftest.sh): samples with no process
	// behind them are read as samples of a process using no CPU.
	for _, s := range w.Samples {
		_ = s.Present
	}'
}

echo "==> H0 negative control: the harness suite is green on the real tree"
expect_green "distribution/hwcert, unmutated" "$root"

echo
echo "==> H1 a measurement over its threshold is caught"
d=$(mutant threshold-that-cannot-be-exceeded)
plant_threshold_that_cannot_be_exceeded "$d"
expect_red "a threshold a thousand times looser than the documented one" "$d" \
  'TestAMeasurementOverItsThresholdFails|TestAHigherIsBetterMeasurementUnderItsThresholdFails' \
  TestAMeasurementOverItsThresholdFails \
  TestAHigherIsBetterMeasurementUnderItsThresholdFails

echo
echo "==> H2 a missing measurement fails rather than defaulting to pass"
d=$(mutant missing-measurement-defaults-to-a-pass)
plant_missing_measurement_defaults "$d"
expect_red "an absent measurement read as a zero, and an absent ratio input read as 1" "$d" \
  'TestAMissingMeasurementFailsRatherThanDefaultingToPass' \
  TestAMissingMeasurementFailsRatherThanDefaultingToPass

echo
echo "==> H3 an evidence record for the wrong architecture is refused"
d=$(mutant one-architecture-standing-in-for-the-other)
plant_one_architecture_standing_in_for_the_other "$d"
expect_red "an amd64 record accepted as arm64 evidence" "$d" \
  'TestARecordForTheWrongArchitectureIsRefused|TestARecordCannotClaimAnArchitectureItsWitnessesDisagreeWith' \
  TestARecordForTheWrongArchitectureIsRefused \
  TestARecordCannotClaimAnArchitectureItsWitnessesDisagreeWith

echo
echo "==> H4 an idle app is distinguishable from an app that is not running"
d=$(mutant absent-process-reads-as-idle)
plant_absent_process_reads_as_idle "$d"
expect_red "a window with no process in it read as an idle window" "$d" \
  'TestIdleIsDistinguishableFromNotRunning' \
  TestIdleIsDistinguishableFromNotRunning

echo
selftest_stale_summary
if [ "$fail" -eq 0 ] && [ "$selftest_stale_count" -eq 0 ]; then
  if [ "$selftest_dry_run" = 1 ]; then
    echo "==> hwcert selftest anchors: ok ($selftest_anchors_checked checked)"
  else
    echo "==> hwcert selftest: ok ($pass controls)"
  fi
  exit 0
fi
echo "==> hwcert selftest: $fail failed, $selftest_stale_count stale, $pass passed" >&2
exit 1

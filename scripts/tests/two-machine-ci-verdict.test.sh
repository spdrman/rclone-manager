#!/usr/bin/env bash
# Self-test for scripts/e2e/two-machine-ci.sh, the translation that stands
# between the two-machine proof and a required check on `release` (#575).
#
# The failure this exists to prevent has one shape. The proof says 3 when
# the machine it is running on cannot perform it, scripts/ci-local.sh
# reads that as INCOMPLETE and ledgers it, and a workflow step reading the
# same 3 as "nothing to report" would publish a signed image on the
# strength of a proof that never ran. Docker-in-docker needs a privileged
# container and the connection-cap case needs a kernel that carries an
# iptables connlimit rule, so "this runner cannot" is a live outcome on
# hosted runners and not a hypothetical.
#
# So all three outcomes are pinned here, and the middle one is pinned
# twice: red, and red under a word that is not FAILED. A run that could
# not look must not be mistaken for a run that looked and liked what it
# saw, and it must not be mistaken for a broken product either, because
# the second is triaged by reading the diff and the first is triaged by
# fixing the runner.
#
# Run directly (`bash scripts/tests/two-machine-ci-verdict.test.sh`) or
# let the gate run it: scripts/ci-local.sh invokes it in every run.
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/.." && pwd)"
WRAPPER="$SCRIPTS_DIR/e2e/two-machine-ci.sh"

# Inside the checkout, gitignored, for the reason every other sandbox in
# this repository is: nothing here should need a recursive delete outside
# the working tree to clean up after itself.
SANDBOX="$REPO_ROOT/.two-machine-ci-test"
case "$SANDBOX" in
  /*/.two-machine-ci-test) ;;
  *) echo "refusing to use sandbox path [$SANDBOX]" >&2; exit 1 ;;
esac
rm -rf "$SANDBOX"
mkdir -p "$SANDBOX"
trap 'rm -rf "$SANDBOX"' EXIT

checks=0
failures=0
pass() { checks=$((checks + 1)); printf '    ok   %s\n' "$1"; }
fail() {
  checks=$((checks + 1))
  failures=$((failures + 1))
  printf '    FAIL %s\n' "$1" >&2
  if [ $# -gt 1 ]; then
    printf '%s\n' "$2" | sed 's/^/         | /' >&2
  fi
}

# fake_proof <status> writes a stand-in for the real proof that exits with
# the status given. The wrapper's whole job is reading that number, so a
# stand-in is the honest fixture here: standing up two containers to
# produce a 0 would test docker-in-docker, not the translation.
fake_proof() {
  local path="$SANDBOX/proof-$1.sh"
  cat > "$path" <<EOF
#!/usr/bin/env bash
echo "fake proof, exiting $1"
exit $1
EOF
  chmod +x "$path"
  printf '%s' "$path"
}

# run_wrapper <status> -> prints combined output, returns the wrapper's
# own exit status. GITHUB_ACTIONS is set so the annotations are emitted
# and can be asserted on; GITHUB_STEP_SUMMARY points at a file per case so
# the summary can be read back.
run_wrapper() {
  local proof summary
  proof="$(fake_proof "$1")"
  summary="$SANDBOX/summary-$1.md"
  : > "$summary"
  TWO_MACHINE_PROOF="$proof" \
  GITHUB_ACTIONS=true \
  GITHUB_STEP_SUMMARY="$summary" \
    bash "$WRAPPER" 2>&1
}

echo "==> two-machine CI verdict: three outcomes, and the one that must not read as a pass"

# ---------------------------------------------------------------- case 1
#
# The pass. Without it every assertion below would hold against a wrapper
# that had simply stopped exiting 0 for anything at all, which would make
# the check unmergeable rather than honest.
out="$(run_wrapper 0)"
status=$?
if [ "$status" = 0 ]; then
  pass "a proof that passed leaves the wrapper with 0, so the check can go green"
else
  fail "a proof that passed left the wrapper with $status, want 0" "$out"
fi
case "$out" in
  *PASSED*) pass "and it says PASSED" ;;
  *) fail "a passing proof produced no PASSED verdict" "$out" ;;
esac

# ---------------------------------------------------------------- case 2
#
# The one this file is for. 3 is the proof's "this machine cannot perform
# it", and a zero here is a green required check on the branch that
# publishes, standing on a run that never happened.
out="$(run_wrapper 3)"
status=$?
if [ "$status" = 0 ]; then
  fail "a proof that COULD NOT RUN left the wrapper with 0: a runner that cannot start docker-in-docker would report a green gate on the branch that publishes a signed image" "$out"
elif [ "$status" = 3 ]; then
  pass "a proof that could not run leaves the wrapper with 3: red, and still telling the caller which of the two reds it is"
else
  fail "a proof that could not run left the wrapper with $status, want 3" "$out"
fi
case "$out" in
  *INCOMPLETE*) pass "and it says INCOMPLETE" ;;
  *) fail "a proof that could not run produced no INCOMPLETE verdict" "$out" ;;
esac
# The word matters as much as the colour. INCOMPLETE is triaged by fixing
# the runner and FAILED is triaged by reading the diff, so a wrapper that
# called this one FAILED would send every reader to the wrong place.
case "$out" in
  *FAILED*) fail "a proof that could not run was reported as FAILED, which sends the reader to the diff when the problem is the runner" "$out" ;;
  *) pass "and it does not call it FAILED, because nothing failed: the runner could not look" ;;
esac

# ---------------------------------------------------------------- case 3
#
# The failure, and the control for case 2: if the wrapper turned every
# non-zero into INCOMPLETE, case 2 would pass against a wrapper that could
# no longer report a real defect at all.
out="$(run_wrapper 1)"
status=$?
if [ "$status" = 1 ]; then
  pass "a proof that failed leaves the wrapper with 1"
else
  fail "a proof that failed left the wrapper with $status, want 1" "$out"
fi
case "$out" in
  *FAILED*) pass "and it says FAILED, so a real defect is not filed under a runner limitation" ;;
  *) fail "a failing proof produced no FAILED verdict" "$out" ;;
esac
case "$out" in
  *INCOMPLETE*) fail "a failing proof was also called INCOMPLETE, so the two verdicts are not distinguishable" "$out" ;;
  *) pass "and not INCOMPLETE" ;;
esac

# ---------------------------------------------------------------- case 4
#
# A status the wrapper has no meaning for travels untouched, so "reported
# as a failure" cannot quietly become "rewritten to 1". 127 is the one
# that actually happens: it is what running a proof script that is not
# there produces, and a missing proof must never be a pass.
out="$(run_wrapper 127)"
status=$?
if [ "$status" = 127 ]; then
  pass "a status the wrapper has no meaning for reaches the caller unchanged"
else
  fail "a proof exiting 127 left the wrapper with $status, want 127" "$out"
fi

# ---------------------------------------------------------------- case 5
#
# The summary is where a person reading a red check looks first, so the
# verdict has to be in it rather than only in the log.
proof="$(fake_proof 3)"
summary="$SANDBOX/summary-file.md"
: > "$summary"
TWO_MACHINE_PROOF="$proof" GITHUB_ACTIONS=true GITHUB_STEP_SUMMARY="$summary" \
  bash "$WRAPPER" >/dev/null 2>&1
if grep -q "INCOMPLETE" "$summary"; then
  pass "the verdict is written to the job summary, not only to the log"
else
  fail "GITHUB_STEP_SUMMARY was set and the verdict never reached it" "$(cat "$summary")"
fi

# ---------------------------------------------------------------- case 6
#
# Off a runner the Actions workflow commands are noise. Asserting their
# absence keeps them from leaking into a local run's output, and proves
# the annotation is conditional rather than unconditional and merely
# harmless.
proof="$(fake_proof 3)"
out="$(TWO_MACHINE_PROOF="$proof" bash "$WRAPPER" 2>&1)"
case "$out" in
  *"::error"*) fail "the Actions annotation is emitted off a runner too, so a local run prints workflow commands nothing will read" "$out" ;;
  *) pass "off a runner no Actions workflow command is emitted, and the verdict is still printed" ;;
esac
case "$out" in
  *INCOMPLETE*) pass "and the verdict itself is still there without GITHUB_STEP_SUMMARY set" ;;
  *) fail "with no summary file the verdict disappeared entirely" "$out" ;;
esac

echo ""
if [ "$failures" -eq 0 ]; then
  echo "==> two-machine CI verdict: ok ($checks checks)"
  exit 0
fi
echo "==> two-machine CI verdict: FAILED ($failures of $checks checks)" >&2
exit 1

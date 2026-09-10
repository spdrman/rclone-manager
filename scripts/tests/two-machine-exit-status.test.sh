#!/usr/bin/env bash
# Self-test for the one number scripts/e2e/two-machine-backup.sh says to
# its caller, and for the collision that made it worth pinning.
#
# The chain, end to end: scripts/lib/ci-local-gate.sh uses 3 for "the gate
# performed less than it was asked to" (#160), scripts/ci-local.sh reads a
# 3 from the two-machine proof as exactly that and ledgers it, and
# .husky/pre-commit turns an INCOMPLETE gate into a commit that is allowed
# with a warning. Then issue #551 gave the CLI an exit status of its own
# at 3 (another process is already serving this deployment), and every
# `bm` call in that script runs the real CLI under `set -euo pipefail`. An
# unguarded one meeting that refusal would have ended the script with the
# number that means "this machine could not try", so a failed proof would
# have been ledgered as a skip and the commit would have gone through.
#
# It was latent when it was found: every `bm` that can meet the refusal is
# guarded with `|| die`, and the only unguarded substitution runs
# `version`. That is safety by review. These cases are what makes it
# safety by construction again, which is what it used to be when the CLI
# had no 3 at all.
#
# The proof plants a command that exits 3 at a call site the script does
# NOT guard (the `git rev-parse` the build step starts with), rather than
# at a `bm` call, and that is deliberate: the guarantee worth having is
# about any unguarded 3, not about the one command that happens to be able
# to produce one today.
#
# Run directly (`bash scripts/tests/two-machine-exit-status.test.sh`) or
# let the gate run it: scripts/ci-local.sh invokes it in every run.
#
# PORTED-CHECK HAZARD NOTE
#
# EPIC I / I1.6 (#672) moved the proof this drives from bash to
# scripts/rcmtools/e2e/two_machine_backup.py, behind an exec shim at the old
# path. A port can silently convert a check into one that CANNOT FAIL, and
# this file is the worked example the epic cites, so every assertion it makes
# answers for itself below.
#
# case 1: a machine with no reachable Docker still exits 3
#   hazard in bash:   the proof stops saying "could not run" at all, and the
#                     gate's INCOMPLETE ledger silently stops being fed.
#   hazard in python: STILL EXISTS. cannot_run() raises CannotRun,
#                     harness.finish() maps EXIT_CANNOT_RUN 97 -> 3. Delete
#                     either half and this goes red.
#   held by:          scripts/rcmtools/harness.py finish(), one translation
#                     for every ported script.
#
# case 2: an unguarded command exiting 3 must NOT become the machine verdict
#   hazard in bash:   `set -euo pipefail` propagates a subprocess's status, so
#                     a `bm` call meeting the CLI's own exit 3 (#551) ended the
#                     script with the number that means "this machine could
#                     not try". A failed proof ledgered as a skip, and
#                     .husky/pre-commit lets an INCOMPLETE gate commit.
#   hazard in python: STILL EXISTS, BUT ONLY BY CONSTRUCTION. Python has no
#                     `set -e`; a subprocess status nobody reads is discarded,
#                     so this regression would have become impossible and this
#                     case would have gone VACUOUSLY GREEN -- worse than a
#                     deleted check, because nothing would say it had stopped
#                     watching. harness.sh(check=True) therefore RESTORES the
#                     propagation deliberately: it raises CommandFailed
#                     carrying the status, and finish() decides what the
#                     status means in one place.
#   held by:          harness.sh + harness.finish. PROVEN RED, not assumed:
#                     replacing finish()'s `if status == EXIT_INCOMPLETE`
#                     branch with `if False` fails 2 of this file's 6 checks.
#                     Re-run that mutation if you change either function.
#
# case 3: a status that means nothing here reaches the caller unchanged
#   hazard in bash:   the translation degenerates to "anything that failed
#                     becomes 1", which would make case 2 pass against a
#                     script that had lost every other status it can report.
#   hazard in python: STILL EXISTS. finish() returns the status untouched on
#                     its default branch; collapsing that to EXIT_FAILED
#                     reddens this.
#   held by:          harness.finish()'s final `return status`.
#
# What the fake tools reach, and why that did not change: the stand-ins are
# prepended to PATH, and the Python proof shells out to `docker` and `git`
# through harness.sh, which resolves them on PATH exactly as the shell did.
# The planted `git` is still reached at the build step, and case 2 asserts
# that it was reached rather than assuming it.
#
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/.." && pwd)"
SCRIPT="$SCRIPTS_DIR/e2e/two-machine-backup.sh"

# Inside the checkout, gitignored, for the reason every other sandbox in
# this repository is: nothing here should need a recursive delete outside
# the working tree to clean up after itself.
SANDBOX="$REPO_ROOT/.two-machine-exit-test"
case "$SANDBOX" in
  /*/.two-machine-exit-test) ;;
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

# fake_bin <dir> <name> <status> writes a stand-in for a real tool that
# exits with the status given and prints nothing, into a PATH directory of
# this case's own. Prepended to PATH rather than replacing it: the script needs
# date, dirname, python3 and the rest to be the real ones, and the point
# is to control one command, not to build a machine.
fake_bin() {
  local dir="$1" name="$2" status="$3"
  mkdir -p "$dir"
  cat > "$dir/$name" <<EOF
#!/usr/bin/env bash
exit $status
EOF
  chmod +x "$dir/$name"
}

# run_case <case dir> -> prints the run's combined output, returns its
# exit status. --case plain keeps the run to one case, and E2E_RUN_ID
# keeps the run directory predictable and out of a real run's way.
run_case() {
  local dir="$1"
  (
    cd "$REPO_ROOT" || exit 1
    PATH="$dir/bin:$PATH" E2E_RUN_ID="exit-status-test-$$" bash "$SCRIPT" --case plain 2>&1
  )
}

echo "==> two-machine exit status: the verdict, and what must not be mistaken for it"

# ---------------------------------------------------------------- case 1
#
# The verdict itself still reaches the caller as 3. Without this, every
# assertion below would pass against a script that had simply stopped
# saying "could not run" at all, which would break the gate's ledger in
# the other direction.
c1="$SANDBOX/cannot-run"
fake_bin "$c1/bin" docker 1
out="$(run_case "$c1")"
status=$?
if [ "$status" = 3 ]; then
  pass "a machine with no reachable Docker daemon still exits 3, the status ci-local.sh ledgers"
else
  fail "a machine with no reachable Docker daemon exited $status, want 3" "$out"
fi
case "$out" in
  *"CANNOT RUN"*) pass "and it says CANNOT RUN, so the 3 is the verdict and not something else that happened to be 3" ;;
  *) fail "the run exited without printing the CANNOT RUN verdict" "$out" ;;
esac

# ---------------------------------------------------------------- case 2
#
# The regression. An unguarded command exits 3, which is what a `bm` call
# meeting #551's refusal does, and the script must not hand that to the
# gate as its own "could not run" verdict.
c2="$SANDBOX/planted-three"
fake_bin "$c2/bin" docker 0
fake_bin "$c2/bin" git 3
out="$(run_case "$c2")"
status=$?
case "$out" in
  *"building the image under test"*)
    pass "the planted command really is reached: the run got past the preflight to the build step" ;;
  *)
    fail "the run never reached the step the 3 is planted in, so it proves nothing about an unguarded 3" "$out" ;;
esac
if [ "$status" = 3 ]; then
  fail "an unguarded command exiting 3 left the script with 3, the status the gate reads as 'this machine could not perform the proof': a failed proof would be ledgered as a skip and .husky/pre-commit would allow the commit" "$out"
elif [ "$status" = 1 ]; then
  pass "an unguarded command exiting 3 leaves the script with 1, so the gate reads a failure"
else
  fail "an unguarded command exiting 3 left the script with $status, want 1" "$out"
fi
case "$out" in
  *"reserves 3"*)
    pass "and it says why the status changed, so nobody has to find this file to understand the 1" ;;
  *)
    fail "the run turned a 3 into a 1 without saying so anywhere in its output" "$out" ;;
esac

# ---------------------------------------------------------------- case 3
#
# The control for case 2. If the translation were "anything that failed
# becomes 1", case 2 would pass against a script that had lost every other
# status it can report, so one arbitrary status has to travel untouched.
c3="$SANDBOX/passthrough"
fake_bin "$c3/bin" docker 0
fake_bin "$c3/bin" git 7
out="$(run_case "$c3")"
status=$?
if [ "$status" = 7 ]; then
  pass "a status that means nothing to this script reaches the caller unchanged"
else
  fail "a command exiting 7 left the script with $status, want 7: the translation is rewriting statuses it has no business touching" "$out"
fi

echo ""
if [ "$failures" -eq 0 ]; then
  echo "==> two-machine exit status: ok ($checks checks)"
  exit 0
fi
echo "==> two-machine exit status: FAILED ($failures of $checks checks)" >&2
exit 1

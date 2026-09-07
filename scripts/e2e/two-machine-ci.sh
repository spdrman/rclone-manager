#!/usr/bin/env bash
# What CI runs instead of calling scripts/e2e/two-machine-backup.sh
# directly (issue #575).
#
# The proof has three outcomes and GitHub Actions has two. That gap is the
# whole reason this file exists.
#
# scripts/e2e/two-machine-backup.sh says 0 for "the proof passed", 3 for
# "this machine cannot perform the proof" and anything else for "the proof
# failed". scripts/ci-local.sh reads all three: a 3 goes in its ledger and
# the run ends INCOMPLETE. A workflow step has no ledger. It exits zero or
# it does not, and a step that ran `two-machine-backup.sh` under `set -e`
# without reading the number would turn a runner that cannot start
# docker-in-docker into a green required check on the branch that
# publishes. That is worse than having no check at all, because a green
# check is read as evidence.
#
# So: 3 is red here, and it is red under its own word. INCOMPLETE is not
# FAILED, it does not mean the product is broken, and it must not be
# triaged as a flaky test. It means the runner could not perform the proof
# and nothing was learned, which on a publishing branch is a reason to
# stop.
#
# The exit status keeps the vocabulary rather than flattening it, so
# anything wrapping this in turn can still tell the two apart:
#
#   0            the proof passed
#   3            the proof could not be performed on this machine
#   anything else the proof failed, with the status it failed with
#
# Every argument is passed straight through, so --case and
# --keep-on-failure work here exactly as they do by hand.
#
# Covered by scripts/tests/two-machine-ci-verdict.test.sh, which
# scripts/ci-local.sh runs.
set -uo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"

# The proof itself decides the verdict. TWO_MACHINE_PROOF exists only so
# the self-test can drive all three outcomes without standing up two
# containers, and it is named rather than positional so no ordinary
# invocation can reach it by accident.
proof="${TWO_MACHINE_PROOF:-$repo_root/scripts/e2e/two-machine-backup.sh}"

status=0
bash "$proof" "$@" || status=$?

# say <line>... puts a line on stdout and, on a runner, in the job summary
# too. The summary is what somebody reading a red check sees first, and a
# verdict that lives only in the middle of a scrolled log is a verdict
# most people will not find.
say() {
  printf '%s\n' "$@"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '%s\n' "$@" >>"$GITHUB_STEP_SUMMARY" 2>/dev/null || true
  fi
}

# annotate <level> <message> raises the verdict to the top of the run's
# own UI. ::error:: and ::notice:: are Actions' own vocabulary; off a
# runner they are noise, so they are only emitted on one.
annotate() {
  [ -n "${GITHUB_ACTIONS:-}" ] || return 0
  printf '::%s title=two-machine proof::%s\n' "$1" "$2"
}

echo ""
case "$status" in
  0)
    say "## two-machine proof: PASSED" \
        "" \
        "A fresh install on a throwaway machine pulled a real backup off another" \
        "throwaway machine over a temporary network, and the artifacts match the" \
        "source by digest."
    annotate notice "PASSED: a fresh install pulled a real backup and the bytes match."
    exit 0
    ;;
  3)
    say "## two-machine proof: INCOMPLETE" \
        "" \
        "This runner could not perform the proof, so nothing about this change was" \
        "learned. The run above says which capability was missing: usually a Docker" \
        "daemon that is not there, one that refuses the privileged container" \
        "docker-in-docker needs, or a kernel that will not carry the connection-cap" \
        "case's iptables rule." \
        "" \
        "This is red on purpose and it is not a failing test. The branch this gates" \
        "publishes a signed image on merge, and a proof nobody performed is not" \
        "evidence that anything works. Fix the runner, or re-run once it can."
    annotate error "INCOMPLETE: this runner could not perform the proof, so nothing was learned. Not a product failure, and not a pass."
    exit 3
    ;;
  *)
    say "## two-machine proof: FAILED" \
        "" \
        "The proof ran and did not hold. The run above names the case and the step" \
        "it died on. This is the one test in the tree that covers the claim a user" \
        "actually makes, so a red here is a change that should not be published." \
        "" \
        "Exit status: $status"
    annotate error "FAILED: the two-machine proof ran and did not hold. Exit status $status."
    exit "$status"
    ;;
esac

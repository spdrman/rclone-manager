#!/usr/bin/env bash
# Self-test for the honesty guarantees scripts/ci-local.sh makes about its
# own final line (issue #160).
#
# The defect this pins down: the gate used to treat "this JS workspace has no
# node_modules/" as "print a note and carry on", and then still ended with
# "==> ci-local: ok". node_modules/ is gitignored and every agent on this
# project works in a fresh `git worktree`, so the DEFAULT state of a new
# checkout was "the frontend lint/typecheck/vitest suites and the
# cross-provider conformance suite never ran, and the gate said ok anyway".
# The final line is what this project reads as merge evidence, so a skip that
# is indistinguishable from a pass in both the exit code and the last line is
# the same class of bug as a test that asserts nothing.
#
# Two layers, because neither alone is enough:
#
#   Group A drives scripts/lib/ci-local-gate.sh directly, which is where the
#   skip ledger and the final line live.
#   Group B runs the REAL scripts/ci-local.sh against synthetic checkouts, so
#   the wiring between the two is proved, not assumed.
#   Group C pins the invariant that ci-local.sh has no success line of its
#   own to print.
#
# Every negative assertion here is paired with a positive control, because
# "the gate did not print ok" is also true when the gate fell over for a
# reason that has nothing to do with this fix. Group B's cases therefore
# assert WHY the run failed (the workspace is named, the fix command is
# printed, and no Go step ran), not merely THAT it failed.
#
# Run directly (`bash scripts/tests/ci-local-gate.test.sh`) or let the gate
# run it: scripts/ci-local.sh invokes it in a non-FAST run.
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/.." && pwd)"
GATE_LIB="$SCRIPTS_DIR/lib/ci-local-gate.sh"

# The sandbox lives inside the repository (gitignored) rather than under
# $TMPDIR so that everything this test creates and deletes stays within the
# checkout it belongs to.
SANDBOX="$REPO_ROOT/.ci-local-gate-test"
case "$SANDBOX" in
  /*/.ci-local-gate-test) ;;
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

assert_contains() { # <label> <needle> <haystack>
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1 -- expected output to contain: $2" "$3" ;;
  esac
}

assert_not_contains() { # <label> <needle> <haystack>
  case "$3" in
    *"$2"*) fail "$1 -- expected output NOT to contain: $2" "$3" ;;
    *) pass "$1" ;;
  esac
}

assert_eq() { # <label> <want> <got>
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 -- want [$2], got [$3]"; fi
}

assert_nonzero() { # <label> <status>
  if [ "$2" -ne 0 ]; then pass "$1"; else fail "$1 -- expected a non-zero exit, got 0"; fi
}

# ---------------------------------------------------------------- fixtures

make_tree() { # -> path of a synthetic checkout carrying only the gate scripts
  # mktemp, not a counter: this runs inside $( ), so a counter would increment
  # in the subshell and every caller would get the same directory back. That
  # bug made three cases share one tree and each inherit the last one's
  # workspaces, which is exactly the confound the controls below exist to
  # catch, and it did catch it.
  tree="$(mktemp -d "$SANDBOX/tree.XXXXXX")"
  mkdir -p "$tree/scripts/lib"
  cp "$SCRIPTS_DIR/ci-local.sh" "$tree/scripts/ci-local.sh"
  if [ -f "$GATE_LIB" ]; then cp "$GATE_LIB" "$tree/scripts/lib/"; fi
  printf '%s\n' "$tree"
}

add_workspace() { # <tree> <relpath> <installed|uninstalled> [nolock]
  mkdir -p "$1/$2"
  printf '{"name":"fixture","private":true}\n' >"$1/$2/package.json"
  if [ "${4:-lock}" != nolock ]; then
    printf '{"lockfileVersion":3}\n' >"$1/$2/package-lock.json"
  fi
  if [ "$3" = installed ]; then mkdir -p "$1/$2/node_modules/.fixture"; fi
}

out=""
status=0
run_gate() { # <tree> [VAR=VAL ...]
  gate_tree="$1"
  shift
  # -u on both flags: the caller's own environment must not decide what this
  # test is measuring. A developer running the suite under CI_LOCAL_FAST=1
  # would otherwise silently turn every fail-closed case into a pass.
  out="$(cd "$gate_tree" && env -u CI_LOCAL_FAST -u CI_LOCAL_SKIP_JS "$@" bash scripts/ci-local.sh 2>&1)"
  status=$?
}

FAIL_BANNER='present in this tree but have no installed dependencies'

# ------------------------------------- Group A: the ledger and the summary

echo "==> A. scripts/lib/ci-local-gate.sh"

if [ ! -f "$GATE_LIB" ]; then
  fail "A0 scripts/lib/ci-local-gate.sh exists"
else
  pass "A0 scripts/lib/ci-local-gate.sh exists"

  ws="$SANDBOX/states"
  mkdir -p "$ws"
  add_workspace "$ws" installed installed
  add_workspace "$ws" uninstalled uninstalled
  add_workspace "$ws" nolock uninstalled nolock
  mkdir -p "$ws/absent-dir"

  probe() { ( . "$GATE_LIB" && cd "$ws" && "$@" ); }

  assert_eq "A1 a workspace that is not in the tree reads as absent" \
    absent "$(probe gate_workspace_state not-here)"
  assert_eq "A2 a directory with no package.json reads as absent" \
    absent "$(probe gate_workspace_state absent-dir)"
  assert_eq "A3 package.json with no node_modules reads as uninstalled" \
    uninstalled "$(probe gate_workspace_state uninstalled)"
  assert_eq "A4 package.json with node_modules reads as installed" \
    installed "$(probe gate_workspace_state installed)"
  assert_eq "A5 the fix command for a locked workspace is npm ci" \
    "cd uninstalled && npm ci" "$(probe gate_install_hint uninstalled)"
  assert_eq "A6 the fix command without a lockfile is npm install" \
    "cd nolock && npm install" "$(probe gate_install_hint nolock)"

  # A7 is the positive control for A8: it proves the summary CAN print the
  # success line, so A8's absence assertion is not vacuous.
  clean_summary="$( . "$GATE_LIB" && gate_summary 2>&1 )"
  assert_contains "A7 an empty ledger still prints the success line" \
    'ci-local: ok' "$clean_summary"

  dirty_summary="$( . "$GATE_LIB" && gate_note_skip 'the conformance suite' && gate_summary 2>&1 )"
  assert_not_contains "A8 a non-empty ledger cannot print the success line" \
    'ci-local: ok' "$dirty_summary"
  assert_contains "A9 the summary names what did not run" \
    'the conformance suite' "$dirty_summary"
  assert_contains "A10 the summary says the run is not merge evidence" \
    'not merge evidence' "$dirty_summary"
fi

# ------------------------------- Group B: the real script, real checkouts

echo "==> B. scripts/ci-local.sh against synthetic checkouts"

# B1-B3: each skip site fails closed, independently. apps/common/tests gets
# its own tree with no other workspace in it, so it is proved on its own and
# not assumed to share a code path with ui/shared -- it is the site nobody
# noticed for three phases.
for site in ui/shared apps/common/tests apps/ugos/frontend/upk-proof; do
  tree="$(make_tree)"
  add_workspace "$tree" "$site" uninstalled
  run_gate "$tree"

  assert_nonzero "B[$site] the run fails" "$status"
  assert_not_contains "B[$site] the run does not report success" 'ci-local: ok' "$out"
  assert_contains "B[$site] the failure names the workspace" "$site" "$out"
  assert_contains "B[$site] the failure prints the exact fix command" \
    "cd $site && npm ci" "$out"
  assert_contains "B[$site] the failure explains itself" "$FAIL_BANNER" "$out"
  # The gate must refuse BEFORE it spends twenty minutes on the Go suites,
  # and this is also what tells a real preflight failure apart from the
  # synthetic tree simply having no core/ module to build.
  assert_not_contains "B[$site] nothing ran before the refusal" \
    'core/ go build' "$out"
done

# B4: the control for "absent is not uninstalled". A tree with no JS
# workspace at all must sail through the preflight; apps/ugos/frontend/upk-proof
# genuinely does not exist on main, and an optional component that is not in
# the tree is nothing to check, not a hole.
tree="$(make_tree)"
run_gate "$tree"
assert_not_contains "B4 an absent workspace is not a failure" "$FAIL_BANNER" "$out"
assert_contains "B4 an absent workspace is reported as absent" \
  'ui/shared not present in this tree' "$out"

# B5: the control for "the check keys on node_modules". Same tree shape as
# B1, one directory different, opposite verdict.
tree="$(make_tree)"
add_workspace "$tree" ui/shared installed
run_gate "$tree"
assert_not_contains "B5 an installed workspace is not a failure" "$FAIL_BANNER" "$out"
assert_contains "B5 an installed workspace is reported as installed" \
  'ui/shared installed' "$out"

# B6: skipping stays possible, but only when it is chosen out loud.
tree="$(make_tree)"
add_workspace "$tree" ui/shared uninstalled
run_gate "$tree" CI_LOCAL_SKIP_JS=1
assert_not_contains "B6 CI_LOCAL_SKIP_JS=1 opts out of the refusal" "$FAIL_BANNER" "$out"

# B7: CI_LOCAL_FAST=1 keeps behaving as the documented fast-iteration mode.
tree="$(make_tree)"
add_workspace "$tree" ui/shared uninstalled
run_gate "$tree" CI_LOCAL_FAST=1
assert_not_contains "B7 CI_LOCAL_FAST=1 still runs" "$FAIL_BANNER" "$out"

# --------------------------------- Group C: no second way to claim success

echo "==> C. the success line has exactly one source"

# Comments are exempt: the header explains the rule and has to be able to
# quote the string. Only a line that can actually print counts.
emits_success_line() { grep -v '^[[:space:]]*#' "$1" | grep -q 'ci-local: ok'; }

if emits_success_line "$SCRIPTS_DIR/ci-local.sh"; then
  fail "C1 ci-local.sh prints no success line of its own (it must go through gate_summary)"
else
  pass "C1 ci-local.sh prints no success line of its own"
fi

# C2 is C1's positive control: the same scan, run against a copy that has
# been given exactly the bad habit C1 forbids, must flag it.
mutant="$SANDBOX/mutant.sh"
cp "$SCRIPTS_DIR/ci-local.sh" "$mutant"
printf 'echo "==> ci-local: ok"\n' >>"$mutant"
if emits_success_line "$mutant"; then
  pass "C2 the C1 scan flags a script that does print one"
else
  fail "C2 the C1 scan flags a script that does print one -- the scan cannot fail, so C1 proves nothing"
fi

# ------------------------------------------------------------------ result

echo
if [ "$failures" -eq 0 ]; then
  echo "==> ci-local gate self-test: ok ($checks checks)"
  exit 0
fi
echo "==> ci-local gate self-test: $failures of $checks checks FAILED" >&2
exit 1

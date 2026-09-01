#!/usr/bin/env bash
# Self-test for the honesty guarantees scripts/ci-local.sh makes about its
# own final line and its own exit status (issue #160).
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
# Four layers, because none of them alone is enough:
#
#   Group A drives scripts/lib/ci-local-gate.sh directly, which is where the
#   skip ledger, the exit status and the final line live.
#   Group B runs the REAL scripts/ci-local.sh against synthetic checkouts and
#   proves the preflight refusals.
#   Group C pins the invariant that ci-local.sh has no success line of its
#   own to print.
#   Group D runs the REAL scripts/ci-local.sh all the way to its last line
#   against a complete synthetic checkout, so the wiring between the skip
#   sites and gate_summary is measured rather than assumed. Group B alone
#   never observed that line: every Group B tree dies in the preflight.
#   Group E mutates that wiring on purpose and proves Group D goes red. A
#   review of this branch neutered the four gate_note_skip call sites, and
#   separately rewrote the three `case "$(gate_workspace_state ...)"`
#   subjects to a constant, and the suite stayed green at 37/37 both times.
#   E1 and E2 are those two exact mutations, kept here so they cannot come
#   back unnoticed.
#
# Every negative assertion here is paired with a positive control, because
# "the gate did not print ok" is also true when the gate fell over for a
# reason that has nothing to do with this fix. Group B's cases assert WHY the
# run failed (the workspace is named, the fix command is printed, and no Go
# step ran), and D1 is the control for the whole of Group D: the same fixture,
# fully installed, must reach "ci-local: ok" with status 0, or every
# INCOMPLETE assertion below it would pass against a gate that can never say
# ok at all.
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

# The exit status ci-local.sh uses for "ran, but skipped something".
INCOMPLETE=3

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

# Printed by the stub self-test the synthetic trees carry. A nested run of
# ci-local.sh inside this suite must execute that stub and not the real
# suite, and D1/D7 assert on this marker in both directions.
SELFTEST_STUB='SELFTEST-STUB-RAN'

# Printed by the stub browser-e2e step the synthetic trees carry. Group G
# asserts on this marker in both directions, the same way D1/D7 do for the
# self-test stub.
E2E_STUB='E2E-GATE-STUB-RAN'

make_tree() { # -> path of a synthetic checkout carrying only the gate scripts
  # mktemp, not a counter: this runs inside $( ), so a counter would increment
  # in the subshell and every caller would get the same directory back. That
  # bug made three cases share one tree and each inherit the last one's
  # workspaces, which is exactly the confound the controls below exist to
  # catch, and it did catch it.
  tree="$(mktemp -d "$SANDBOX/tree.XXXXXX")"
  mkdir -p "$tree/scripts/lib" "$tree/bin"
  cp "$SCRIPTS_DIR/ci-local.sh" "$tree/scripts/ci-local.sh"
  if [ -f "$GATE_LIB" ]; then cp "$GATE_LIB" "$tree/scripts/lib/"; fi
  # The Docker probe is a property of the machine, not of the tree, so every
  # synthetic run gets a stub daemon it controls. run_gate puts $tree/bin
  # first on PATH. If a real docker ever shadowed this stub the docker cases
  # below go red rather than quietly measuring the host.
  set_docker "$tree" available
  printf '%s\n' "$tree"
}

set_docker() { # <tree> <available|unavailable>
  if [ "$2" = available ]; then
    printf '#!/bin/sh\nexit 0\n' >"$1/bin/docker"
  else
    printf '#!/bin/sh\nexit 1\n' >"$1/bin/docker"
  fi
  chmod +x "$1/bin/docker"
}

add_workspace() { # <tree> <relpath> <installed|uninstalled> [nolock]
  mkdir -p "$1/$2"
  # Every script the gate invokes on a JS workspace, mapped to true. The
  # fixture is measuring which steps the gate chooses to run, not what they
  # do, so the cheapest script that exists and succeeds is the right one.
  cat >"$1/$2/package.json" <<'PKG'
{
  "name": "fixture",
  "private": true,
  "scripts": {
    "lint": "true",
    "typecheck:providers": "true",
    "eslint": "true",
    "test": "true",
    "build": "true"
  }
}
PKG
  if [ "${4:-lock}" != nolock ]; then
    printf '{"lockfileVersion":3}\n' >"$1/$2/package-lock.json"
  fi
  if [ "$3" = installed ]; then mkdir -p "$1/$2/node_modules/.fixture"; fi
}

add_go_module() { # <tree> <relpath> <module name>
  mkdir -p "$1/$2"
  printf 'module %s\n\ngo 1.21\n' "$3" >"$1/$2/go.mod"
  printf 'package stub\n\n// Stub exists so the module has something to build.\nfunc Stub() int { return 1 }\n' >"$1/$2/stub.go"
}

# make_full_tree -> a synthetic checkout a non-FAST run can complete in full
#
# Everything scripts/ci-local.sh reaches for on the way to gate_summary, in
# its cheapest honest form: two real (tiny) Go modules, the project's own
# golangci config, installed JS workspaces whose scripts are `true`, and
# stubs for the self-test and the four structure proofs. Group B's trees
# deliberately have none of this, which is why no Group B case has ever seen
# the final line.
make_full_tree() {
  tree="$(make_tree)"

  cp "$REPO_ROOT/.golangci.yml" "$tree/.golangci.yml"

  add_go_module "$tree" core stubcore
  add_go_module "$tree" core/internal/stub stubcoreinternal
  rm -f "$tree/core/internal/stub/go.mod"
  # issue #256: the Docker-backed suites step now runs
  # `go run ./cmd/gotestwatch ...` instead of a plain `go test`, so this
  # fixture needs that package to actually exist and resolve, the same
  # reason every other stub in this function exists. The stub does nothing
  # with its arguments (a bare `func main() {}` ignores them entirely),
  # which is enough: this fixture measures which steps the gate chooses to
  # run, not what gotestwatch itself does with real packages.
  mkdir -p "$tree/core/cmd/gotestwatch"
  printf 'package main\n\nfunc main() {}\n' >"$tree/core/cmd/gotestwatch/main.go"
  add_go_module "$tree" apps/common stubcommon
  # The distribution layer became its own Go module in #165, and ci-local.sh
  # builds, vets, tests and lints it like every other module. Without it here
  # the gate dies on `cd distribution` in every full-tree case, which is the
  # same shape of miss the two comments further down record.
  add_go_module "$tree" distribution stubdistribution

  add_workspace "$tree" ui/shared installed
  add_workspace "$tree" apps/common/tests installed

  # A stub self-test, so a nested run terminates on purpose rather than on
  # whatever the fixture happens to lack. ci-local.sh's CI_LOCAL_SELFTEST
  # guard is the second, independent terminator; D7 measures it.
  mkdir -p "$tree/scripts/tests"
  printf '#!/usr/bin/env bash\necho "%s"\nexit 0\n' "$SELFTEST_STUB" \
    >"$tree/scripts/tests/ci-local-gate.test.sh"

  # The other guard suite ci-local.sh runs before the self-test. It arrived
  # with #182 without a stub here, and `bash` on a path that does not exist
  # exits 127 under set -e, so every full-tree case below that step died for
  # a reason unrelated to what it measured, Group D's own control included.
  printf '#!/usr/bin/env bash\nexit 0\n' \
    >"$tree/scripts/tests/record-release-hashes-guards.test.sh"

  # .husky/pre-commit is the one caller that has to act on the exit status
  # rather than read the prose, so Group F drives the real hook in this tree.
  mkdir -p "$tree/.husky"
  cp "$REPO_ROOT/.husky/pre-commit" "$tree/.husky/pre-commit"

  # The three-layer checks and their mutation self-test (#165) join the four
  # structure proofs here: static ones run even under CI_LOCAL_FAST, so every
  # tree that gets as far as them needs all of them present.
  mkdir -p "$tree/scripts/architecture"
  for arch in check-layer-manifest check-core-dependency-rule \
              check-layer-ownership check-ui-shared-provider-imports \
              selftest verify-core-without-apps verify-core-without-distribution \
              verify-ui-shared-without-provider-sdks verify-ugos-removable; do
    printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/architecture/$arch.sh"
  done

  # The performance baseline gate and its own self-test (#165), stubbed for
  # the same reason: this fixture measures which steps the gate chooses to
  # run, and the real ones read a baseline record captured on one host.
  mkdir -p "$tree/scripts/perf"
  for perf in check-baseline selftest; do
    printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/perf/$perf.sh"
  done

  # The /api/v1 contract drift check, the client-path check (#211) and
  # their shared mutation self-test (#166). Same reason again, and the same
  # failure mode if any of them is missing: they run unconditionally, FAST
  # included, so a gate step pointed at a path this fixture does not have
  # exits 127 under `set -e` and every case below it fails for a reason
  # that has nothing to do with what it was measuring.
  mkdir -p "$tree/scripts/api"
  for api in check-contract-drift check-client-paths selftest; do
    printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/api/$api.sh"
  done

  # Stubs for the release-script guard suites the gate runs, for the same
  # reason the four structure proofs above are stubbed: this fixture
  # measures which steps the gate chooses to run, not what those steps do,
  # and the real suites drive Docker-shaped scripts in throwaway git
  # repositories.
  #
  # Not optional. `bash` on a path that does not exist exits 127, the gate
  # runs under `set -e`, and the run dies at that step, so every full-tree
  # case below it (Group D's control included) fails for a reason that has
  # nothing to do with what it is measuring. That is what happened when
  # #174's guard suite was added to ci-local.sh without being added here:
  # 18 of this suite's 94 checks were failing on main before this line
  # existed, and the gate could not reach its own summary.
  for guard in record-release-hashes-guards publish-image-guards; do
    printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/tests/$guard.test.sh"
  done

  # The browser e2e step (#158, #197). Same reason as every stub above, and
  # the same failure mode without it, which this suite has now been bitten
  # by twice: `bash` on a path that does not exist exits 127, the gate runs
  # under `set -e`, and every full-tree case below the step dies for a
  # reason that has nothing to do with what it measures. That is what
  # #174's guard suite did to 18 of these checks before its stub existed.
  #
  # The real script clones a pinned repository, builds a binary, and drives
  # a browser. This fixture measures which steps the gate chooses to run,
  # so the stub prints a marker and succeeds; Group G replaces it with a
  # failing one where it needs the other direction.
  mkdir -p "$tree/scripts/e2e"
  printf '#!/usr/bin/env bash\necho "%s"\nexit 0\n' "$E2E_STUB" \
    >"$tree/scripts/e2e/run-tests-repo-gate.sh"

  printf '%s\n' "$tree"
}

out=""
status=0

# run_hook drives .husky/pre-commit; run_gate drives scripts/ci-local.sh.
# Both share $out/$status, and both scrub the four CI_LOCAL_* variables so
# the caller's own environment cannot decide what a case measures.
run_hook() { # <tree> [VAR=VAL ...]
  hook_tree="$1"
  shift
  out="$(cd "$hook_tree" && env -u CI_LOCAL_FAST -u CI_LOCAL_SKIP_JS \
    -u CI_LOCAL_SKIP_DOCKER -u CI_LOCAL_SELFTEST -u CI_LOCAL_SKIP_E2E \
    PATH="$hook_tree/bin:$PATH" "$@" sh .husky/pre-commit 2>&1)"
  status=$?
}

run_gate() { # <tree> [VAR=VAL ...]
  gate_tree="$1"
  shift
  # A developer running this suite under CI_LOCAL_FAST=1 would otherwise
  # silently turn every fail-closed case into a pass, and the gate exports
  # CI_LOCAL_SELFTEST=1 around this very script, which would otherwise reach
  # every synthetic run as an inherited skip.
  out="$(cd "$gate_tree" && env -u CI_LOCAL_FAST -u CI_LOCAL_SKIP_JS \
    -u CI_LOCAL_SKIP_DOCKER -u CI_LOCAL_SELFTEST -u CI_LOCAL_SKIP_E2E \
    PATH="$gate_tree/bin:$PATH" "$@" bash scripts/ci-local.sh 2>&1)"
  status=$?
}

FAIL_BANNER='present in this tree but have no installed dependencies'
DOCKER_BANNER='The Docker daemon is not reachable'

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

  # A11/A12: the exit status has to say what the last line says. A wrapper
  # (.husky/pre-commit, any scripted merge) reads the status, never the
  # prose, and before this pair both outcomes returned 0.
  ( . "$GATE_LIB" && gate_summary >/dev/null 2>&1 )
  assert_eq "A11 an empty ledger exits 0" 0 "$?"

  ( . "$GATE_LIB" && gate_note_skip 'the conformance suite' && gate_summary >/dev/null 2>&1 )
  assert_eq "A12 a non-empty ledger exits $INCOMPLETE, not 0" "$INCOMPLETE" "$?"

  # A13-A17: the Docker probe. `docker info`, not `command -v docker`: the
  # binary being installed says nothing about the daemon being up, and it is
  # the daemon that decides whether the crash matrix runs or t.Skips.
  dockerbin="$SANDBOX/dockerbin"
  mkdir -p "$dockerbin"
  set_docker_probe() { # <available|unavailable>
    if [ "$1" = available ]; then printf '#!/bin/sh\nexit 0\n' >"$dockerbin/docker"
    else printf '#!/bin/sh\nexit 1\n' >"$dockerbin/docker"; fi
    chmod +x "$dockerbin/docker"
  }

  set_docker_probe available
  assert_eq "A13 a reachable daemon reads as available" \
    available "$( PATH="$dockerbin:$PATH" sh -c ". '$GATE_LIB' && gate_docker_state" )"
  set_docker_probe unavailable
  assert_eq "A14 an unreachable daemon reads as unavailable" \
    unavailable "$( PATH="$dockerbin:$PATH" sh -c ". '$GATE_LIB' && gate_docker_state" )"

  docker_refusal="$( PATH="$dockerbin:$PATH" sh -c ". '$GATE_LIB' && gate_require_docker" 2>&1 )"
  docker_refusal_status=$?
  assert_nonzero "A15 an unreachable daemon refuses the run" "$docker_refusal_status"
  assert_contains "A16 the refusal names the suites that would t.Skip" \
    'crash matrix' "$docker_refusal"

  docker_optout="$( PATH="$dockerbin:$PATH" CI_LOCAL_SKIP_DOCKER=1 \
    sh -c ". '$GATE_LIB' && gate_require_docker && gate_summary" 2>&1 )"
  assert_contains "A17 CI_LOCAL_SKIP_DOCKER=1 ledgers instead of refusing" \
    'ci-local: INCOMPLETE' "$docker_optout"
  assert_contains "A18 the ledgered Docker entry names the suites" \
    'crash matrix' "$docker_optout"

  # A19 is the positive control for A15-A18: with the daemon up, the same
  # call must be a silent success and leave the ledger empty, or A15-A18
  # would pass against a probe that always refuses.
  set_docker_probe available
  docker_ok="$( PATH="$dockerbin:$PATH" sh -c ". '$GATE_LIB' && gate_require_docker && gate_summary" 2>&1 )"
  assert_contains "A19 a reachable daemon leaves the ledger empty" \
    'ci-local: ok' "$docker_ok"
fi

# ------------------------------- Group B: the real script, real checkouts

echo "==> B. scripts/ci-local.sh preflight against synthetic checkouts"

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

# ------------------ Group D: the real script, all the way to its last line

echo "==> D. scripts/ci-local.sh end to end, through gate_summary"

# D1 is the control for everything below it. Same fixture, nothing missing,
# daemon up: the run must reach the success line with status 0. Without this
# case, every INCOMPLETE assertion in D2-D8 would also pass against a gate
# that had lost the ability to say ok at all.
tree="$(make_full_tree)"
run_gate "$tree"
assert_contains "D1 a complete run reports success" 'ci-local: ok' "$out"
assert_eq "D1 a complete run exits 0" 0 "$status"
assert_not_contains "D1 a complete run is not INCOMPLETE" 'ci-local: INCOMPLETE' "$out"
assert_not_contains "D1 a complete run prints no failure marker" 'ci-local: FAILED' "$out"
# Proof the fixture really performed the run rather than short-circuiting
# somewhere convenient: the last three steps before the summary all show up.
assert_contains "D1 the conformance step ran" \
  'cross-provider conformance suite' "$out"
assert_contains "D1 the ui/shared vitest step ran" 'ui/shared tests' "$out"
assert_contains "D1 the nested self-test ran (and was the stub)" "$SELFTEST_STUB" "$out"

# D2: the ui/shared skip site, end to end. Mutation E1 and E2 both make this
# case say ok.
tree="$(make_full_tree)"
rm -rf "$tree/ui/shared/node_modules"
run_gate "$tree" CI_LOCAL_SKIP_JS=1
assert_not_contains "D2 a skipped ui/shared cannot report success" 'ci-local: ok' "$out"
assert_contains "D2 a skipped ui/shared ends INCOMPLETE" 'ci-local: INCOMPLETE' "$out"
assert_contains "D2 the summary names the ui/shared checks" \
  'ui/shared lint, typecheck:providers' "$out"
assert_contains "D2 the summary prints the fix command" 'cd ui/shared && npm ci' "$out"
assert_eq "D2 a skipped ui/shared exits $INCOMPLETE" "$INCOMPLETE" "$status"

# D3: the apps/common/tests skip site, end to end, on its own tree. This is
# the site that was silently skipped on every Phase 3 gate run.
tree="$(make_full_tree)"
rm -rf "$tree/apps/common/tests/node_modules"
run_gate "$tree" CI_LOCAL_SKIP_JS=1
assert_not_contains "D3 a skipped conformance suite cannot report success" \
  'ci-local: ok' "$out"
assert_contains "D3 the summary names the conformance suite" \
  'cross-provider conformance suite' "$out"
assert_eq "D3 a skipped conformance suite exits $INCOMPLETE" "$INCOMPLETE" "$status"

# D4: FAST is a documented, deliberate skip, and it still may not claim
# success. Everything in this tree is installed, so the only thing standing
# between this run and D1's ok is the FAST ledger entry.
tree="$(make_full_tree)"
run_gate "$tree" CI_LOCAL_FAST=1
assert_not_contains "D4 CI_LOCAL_FAST=1 cannot report success" 'ci-local: ok' "$out"
assert_contains "D4 CI_LOCAL_FAST=1 ends INCOMPLETE" 'ci-local: INCOMPLETE' "$out"
assert_contains "D4 the summary names FAST as the reason" 'CI_LOCAL_FAST=1' "$out"
assert_eq "D4 CI_LOCAL_FAST=1 exits $INCOMPLETE" "$INCOMPLETE" "$status"

# D5: a stopped Docker daemon is refused in the preflight. Without this the
# crash matrix, the SFTP integration suite and apps/generic/tests/dockercli
# all t.Skip, go test exits 0, and the run ends ok having tested none of it.
tree="$(make_full_tree)"
set_docker "$tree" unavailable
run_gate "$tree"
assert_nonzero "D5 a stopped daemon fails the run" "$status"
assert_not_contains "D5 a stopped daemon cannot report success" 'ci-local: ok' "$out"
assert_contains "D5 the refusal explains itself" "$DOCKER_BANNER" "$out"
assert_contains "D5 the refusal names the crash matrix" 'crash matrix' "$out"
assert_not_contains "D5 nothing ran before the refusal" 'core/ go build' "$out"

# D6: the out-loud opt-out proceeds and ledgers. The 'core/ go build'
# assertion is the positive control against D5: same tree, same stopped
# daemon, and this time the run really does continue.
tree="$(make_full_tree)"
set_docker "$tree" unavailable
run_gate "$tree" CI_LOCAL_SKIP_DOCKER=1
assert_contains "D6 CI_LOCAL_SKIP_DOCKER=1 proceeds past the preflight" \
  'core/ go build' "$out"
assert_not_contains "D6 CI_LOCAL_SKIP_DOCKER=1 cannot report success" \
  'ci-local: ok' "$out"
assert_contains "D6 the summary names the Docker suites" 'crash matrix' "$out"
assert_eq "D6 CI_LOCAL_SKIP_DOCKER=1 exits $INCOMPLETE" "$INCOMPLETE" "$status"

# D7: the recursion guard. ci-local.sh runs this suite, and this suite runs
# ci-local.sh, so a nested run must refuse to descend again. D1 is the
# positive control: it proves the stub self-test does run when the marker is
# not set, so this absence assertion is about the guard and not about a
# fixture that never had a self-test to run.
tree="$(make_full_tree)"
run_gate "$tree" CI_LOCAL_SELFTEST=1
assert_not_contains "D7 a nested run does not run the self-test again" \
  "$SELFTEST_STUB" "$out"
assert_contains "D7 a nested run says why it stopped" 'not recursing' "$out"
assert_contains "D7 a nested run ledgers the self-test it skipped" \
  'nested inside one' "$out"
assert_eq "D7 a nested run exits $INCOMPLETE" "$INCOMPLETE" "$status"

# D8: the third outcome. A run that actually fails used to print no
# "==> ci-local:" marker at all, so of three outcomes only two were
# greppable and the missing one was the failure. D1 is the control for the
# absence of this marker on a healthy run.
tree="$(make_full_tree)"
printf 'package stub\n\nthis is not go\n' >"$tree/core/stub.go"
run_gate "$tree"
assert_nonzero "D8 a broken step fails the run" "$status"
assert_contains "D8 a failed run prints a verdict line naming the step" \
  'ci-local: FAILED (core/ go build)' "$out"
assert_not_contains "D8 a failed run does not report success" 'ci-local: ok' "$out"

# ------------- Group E: the mutations that stayed green during code review

echo "==> E. Group D goes red when the ledger wiring is removed"

# count_matches <file> <extended regex>
count_matches() { grep -cE "$2" "$1" | tr -d '[:space:]'; }

# E1: the review's first mutation. Prefix every gate_note_skip call site in
# ci-local.sh with `:` so the ledger is never written. Nothing else changes:
# the arms are still there, still reached, still "handling" the skip.
tree="$(make_full_tree)"
script="$tree/scripts/ci-local.sh"
before="$(count_matches "$script" '^[[:space:]]*gate_note_skip ')"
sed -i.bak 's/^\([[:space:]]*\)gate_note_skip /\1: gate_note_skip /' "$script"
rm -f "$script.bak"
after="$(count_matches "$script" '^[[:space:]]*: gate_note_skip ')"
# Without this the mutation could match nothing and E1 would "pass" by
# testing the unmutated script.
assert_eq "E1 the mutation neutered every gate_note_skip call site" "$before" "$after"
rm -rf "$tree/ui/shared/node_modules"
run_gate "$tree" CI_LOCAL_SKIP_JS=1
assert_contains "E1 without the ledger the same run reports success, so D2 has teeth" \
  'ci-local: ok' "$out"

# E2: the review's second mutation. Leave every arm and every
# gate_note_skip intact and change only the case subject, so each frontend
# and conformance block vanishes with no ledger entry. A static scan for
# "every uninstalled) arm calls gate_note_skip" does not catch this one,
# which is why Group D exists in the end-to-end form.
tree="$(make_full_tree)"
script="$tree/scripts/ci-local.sh"
before="$(count_matches "$script" 'case "\$\(gate_workspace_state ')"
sed -i.bak 's/case "\$(gate_workspace_state [^)]*)" in/case "absent" in/' "$script"
rm -f "$script.bak"
after="$(count_matches "$script" '^[[:space:]]*case "absent" in')"
assert_eq "E2 the mutation rewrote every workspace-state dispatch" "$before" "$after"
rm -rf "$tree/ui/shared/node_modules"
run_gate "$tree" CI_LOCAL_SKIP_JS=1
assert_contains "E2 without the dispatch the same run reports success, so D2 has teeth" \
  'ci-local: ok' "$out"

# E3: the summary call itself. Delete it and the run ends with no verdict
# line at all, which is what D1's success assertion is really guarding.
tree="$(make_full_tree)"
script="$tree/scripts/ci-local.sh"
before="$(count_matches "$script" '^gate_summary$')"
assert_eq "E3 ci-local.sh calls gate_summary exactly once, unindented" 1 "$before"
sed -i.bak 's/^gate_summary$/: no summary/' "$script"
rm -f "$script.bak"
run_gate "$tree"
assert_not_contains "E3 without gate_summary the run says nothing, so D1 has teeth" \
  'ci-local: ok' "$out"

# --------------------------- Group F: the one caller that reads the status

echo "==> F. .husky/pre-commit acts on the exit status, not the prose"

# The hook is the reason the exit status matters at all: before this it was
# `set -e; bash scripts/ci-local.sh`, and since an INCOMPLETE run returned 0
# the hook could not tell a full run from one that skipped everything. Three
# outcomes, three behaviours, and F1 is the control for the other two.
tree="$(make_full_tree)"
run_hook "$tree"
assert_eq "F1 the hook passes a complete run" 0 "$status"
assert_contains "F1 the hook ran the gate" 'ci-local: ok' "$out"
assert_not_contains "F1 a complete run gets no INCOMPLETE warning" \
  'allowing this commit on an INCOMPLETE gate run' "$out"

tree="$(make_full_tree)"
run_hook "$tree" CI_LOCAL_FAST=1
assert_eq "F2 the hook still allows an INCOMPLETE run" 0 "$status"
assert_contains "F2 the hook says out loud that it allowed one" \
  'allowing this commit on an INCOMPLETE gate run' "$out"

tree="$(make_full_tree)"
printf 'package stub\n\nthis is not go\n' >"$tree/core/stub.go"
run_hook "$tree"
assert_nonzero "F3 the hook blocks a run that actually failed" "$status"
assert_not_contains "F3 a real failure is not treated as INCOMPLETE" \
  'allowing this commit on an INCOMPLETE gate run' "$out"

# ------------------------- Group G: the browser e2e step is a real signal

echo "==> G. the browser e2e step (#158, #197)"

# G1 is the control for everything below it. The step is not conditional on
# anything but FAST, so a complete run must actually invoke it. Without this
# case, G3's absence assertion would also pass against a gate that had lost
# the step entirely.
tree="$(make_full_tree)"
run_gate "$tree"
assert_contains "G1 a complete run invokes the browser e2e step" \
  'browser e2e + CLI smoke' "$out"
assert_contains "G1 the step really ran (and was the stub)" "$E2E_STUB" "$out"
assert_contains "G1 a complete run still reports success" 'ci-local: ok' "$out"

# G2: the out-loud opt-out ledgers rather than printing a note and carrying
# on. This is #197's first open question answered: a missing browser is
# openable, but only the way a stopped Docker daemon is, and the run that
# opens it says so and cannot be merge evidence.
tree="$(make_full_tree)"
run_gate "$tree" CI_LOCAL_SKIP_E2E=1
assert_not_contains "G2 CI_LOCAL_SKIP_E2E=1 does not run the step" "$E2E_STUB" "$out"
assert_not_contains "G2 CI_LOCAL_SKIP_E2E=1 cannot report success" 'ci-local: ok' "$out"
assert_contains "G2 CI_LOCAL_SKIP_E2E=1 ends INCOMPLETE" 'ci-local: INCOMPLETE' "$out"
assert_contains "G2 the summary names the browser suite" \
  'browser e2e suite and the CLI smoke slice' "$out"
assert_eq "G2 CI_LOCAL_SKIP_E2E=1 exits $INCOMPLETE" "$INCOMPLETE" "$status"

# G3: FAST leaves it out, which the never-merge-on-FAST rule already covers.
# D4 owns the INCOMPLETE half; this owns the "it did not run" half.
tree="$(make_full_tree)"
run_gate "$tree" CI_LOCAL_FAST=1
assert_not_contains "G3 CI_LOCAL_FAST=1 does not run the browser e2e step" "$E2E_STUB" "$out"

# G4: the whole point. A red suite has to refuse the commit, not annotate
# it. The stub is replaced by a failing one, which is the closest a
# synthetic tree can get to a red spec without cloning a repository and
# driving a browser; the real thing is demonstrated in the pull request by
# breaking a spec in a worktree and watching this step go red.
tree="$(make_full_tree)"
printf '#!/usr/bin/env bash\necho "a spec failed"\nexit 1\n' \
  >"$tree/scripts/e2e/run-tests-repo-gate.sh"
run_gate "$tree"
assert_nonzero "G4 a red browser suite fails the run" "$status"
assert_not_contains "G4 a red browser suite cannot report success" 'ci-local: ok' "$out"
assert_contains "G4 the verdict line names the step that failed" \
  'ci-local: FAILED (browser e2e + CLI smoke' "$out"

# G5: the pin is a full sha, checked statically. A branch name or a short
# sha there would let the tests under this gate change without the pin
# changing, which is the one thing the pin exists to stop. Checked against
# the REAL pin file, not a fixture, because the fixture would only prove the
# fixture.
pin="$REPO_ROOT/scripts/e2e/tests-repo.pin"
if [ ! -f "$pin" ]; then
  fail "G5 scripts/e2e/tests-repo.pin exists"
elif grep -qE '^TESTS_REPO_SHA=[0-9a-f]{40}$' "$pin"; then
  pass "G5 the tests-repo pin is a full 40-character sha"
else
  fail "G5 the tests-repo pin is a full 40-character sha"
fi

# G6 is G5's positive control: the same scan against a copy carrying exactly
# the bad habit G5 forbids must flag it.
mutant_pin="$SANDBOX/tests-repo.pin"
printf 'TESTS_REPO_URL=https://example.invalid/x.git\nTESTS_REPO_SHA=main\n' >"$mutant_pin"
if grep -qE '^TESTS_REPO_SHA=[0-9a-f]{40}$' "$mutant_pin"; then
  fail "G6 the G5 scan flags a branch-name pin -- the scan cannot fail, so G5 proves nothing"
else
  pass "G6 the G5 scan flags a branch-name pin"
fi

# ------------------------------------------------------------------ result

echo
if [ "$failures" -eq 0 ]; then
  echo "==> ci-local gate self-test: ok ($checks checks)"
  exit 0
fi
echo "==> ci-local gate self-test: $failures of $checks checks FAILED" >&2
exit 1

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

# Every assertion goes through pass or fail and both count, so the run
# ends with a number rather than an impression. That count is what caught
# this suite's own worst failure: a review neutered the gate's skip
# ledger twice and it stayed green at 37 checks both times, which is why
# Groups D and E exist and why the total is worth reading.
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

# The same, for the two-machine end-to-end backup proof (#356). Group I
# asserts on it in both directions. The real script builds a container
# image and stands up docker-in-docker; this fixture measures which steps
# the gate chooses to run, so the stub prints a marker and succeeds.
TWO_MACHINE_STUB='TWO-MACHINE-STUB-RAN'

# And for the repository-wide gofmt sweep (#417), which Group L watches in
# both directions.
GOFMT_STUB='GOFMT-SWEEP-STUB-RAN'

# The same, for the check that vets and lints the Go files no module owns
# (#417). It lives in scripts/architecture/ but runs near the top of the
# gate with the sweep rather than with the other architecture checks, and
# Group L is where that placement is pinned.
UNOWNED_STUB='UNOWNED-GO-STUB-RAN'

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

# A stub `docker` on the tree's own PATH, in whichever state the case
# needs. The Docker probe is a property of the machine and every case here
# is about what the gate DOES with the answer, so the answer has to be the
# fixture's to choose: measuring the host would make the docker cases pass
# or fail depending on who ran them.
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

# A real module, not a marker directory: the gate runs `go build` and
# `go vet` per module, so a fixture module has to compile. One stub file
# is the cheapest thing that does, and keeping it minimal is what lets a
# full synthetic run finish in seconds.
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
  # And distribution/packaging as a package of that module, because #417
  # gave it a step of its own: it is the one Go suite the gate runs without
  # -race, so `go test ./packaging/` is now a real path the gate walks and
  # a tree without it dies on it, which is the same miss again.
  add_go_module "$tree" distribution/packaging stubpackaging
  rm -f "$tree/distribution/packaging/go.mod"

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
              verify-ui-shared-without-provider-sdks verify-ugos-removable \
              check-unowned-go; do
    printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/architecture/$arch.sh"
  done
  # check-unowned-go prints a marker on top of that, because Group L
  # watches for it in both directions the way Group G watches the browser
  # e2e stub. The real one vets and lints every Go file outside every
  # module, through a throwaway module per directory.
  printf '#!/usr/bin/env bash\necho "%s"\nexit 0\n' "$UNOWNED_STUB" \
    >"$tree/scripts/architecture/check-unowned-go.sh"

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

  # EPIC E's FR-35 compatibility mutation self-test (#242), stubbed for the
  # third time for the third identical reason. The real one copies the tree
  # sixteen times and builds core/ in each copy; this fixture measures which
  # steps the gate chooses to run, not what they do, and a missing path here
  # exits 127 under `set -e` and takes every case below it with it.
  mkdir -p "$tree/scripts/compat"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/compat/selftest.sh"

  # And EPIC E's composed conformance mutation self-test (#242), for the
  # same reason: the real one copies the tree nine times, builds core/ in
  # each copy and stands up MinIO containers.
  mkdir -p "$tree/scripts/conformance"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/conformance/selftest.sh"

  # The race detector's own mutation self-test (#417), stubbed for the
  # seventh time for the seventh identical reason: the real one copies the
  # tree twice, plants a data race in core/service and runs the detector
  # over it, and `bash` on a path that does not exist exits 127 under
  # `set -e` and takes every case below it down with it.
  mkdir -p "$tree/scripts/race"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/race/selftest.sh"

  # The formatting sweep and its own mutation self-test (#417), for the
  # eighth and ninth time for the same reason. The sweep prints a marker
  # because Group L watches for it in both directions; the real one reads
  # every tracked .go file in the repository.
  mkdir -p "$tree/scripts/format"
  printf '#!/usr/bin/env bash\necho "%s"\nexit 0\n' "$GOFMT_STUB" \
    >"$tree/scripts/format/check-gofmt.sh"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$tree/scripts/format/selftest.sh"

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

  # The installer's unit tests (#262), which the gate runs by `cd`-ing into
  # scripts/install. Same reason as every stub above, and the same failure
  # mode without it: a `cd` into a directory this fixture does not have
  # fails, the gate runs under `set -e`, and every full-tree case below the
  # step dies for a reason that has nothing to do with what it measures.
  # This is the third time that has happened here, and it is why the step
  # gets a stub rather than a conditional in ci-local.sh: a step that
  # quietly skips itself when its own file is missing is #160's silent skip
  # wearing a different hat.
  mkdir -p "$tree/scripts/install"
  printf 'import unittest\n\n\nclass Stub(unittest.TestCase):\n    def test_stub(self):\n        pass\n' \
    >"$tree/scripts/install/test_install_docker_host.py"

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

  # The two-machine end-to-end backup proof (#356), stubbed for exactly the
  # reason the four comments above give, and this is the sixth time that
  # lesson has had to be written down here: `bash` on a path that does not
  # exist exits 127, the gate runs under `set -e`, and every full-tree case
  # below the step would die for a reason that has nothing to do with what
  # it measures. The real script builds an image from the working tree and
  # stands up two containers on a temporary network.
  printf '#!/usr/bin/env bash\necho "%s"\nexit 0\n' "$TWO_MACHINE_STUB" \
    >"$tree/scripts/e2e/two-machine-backup.sh"

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
    -u CI_LOCAL_SKIP_TWO_MACHINE \
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
    -u CI_LOCAL_SKIP_TWO_MACHINE \
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

# H1: the installer prerequisite-refusal step (#262) piped its `unittest`
# run through `tail -3` for output tidiness, and this file has no
# `set -o pipefail` (nor could it portably rely on one, being
# #!/usr/bin/env sh): a pipeline's exit status under plain `set -e` is its
# LAST command's, which is `tail` here, and `tail` always succeeds. That
# let the installer's own 55-case test suite fail silently forever -- the
# exact "a refusal nobody has watched work is not a refusal" failure this
# step exists to close, one line away from closing it, caught by review
# before it shipped. This proves the fix: a red installer suite has to
# refuse the commit, not get truncated and ignored, the same shape as G4's
# proof for the browser e2e step.
tree="$(make_full_tree)"
printf 'import unittest\n\n\nclass Stub(unittest.TestCase):\n    def test_stub(self):\n        self.fail("a real installer regression")\n' \
  >"$tree/scripts/install/test_install_docker_host.py"
run_gate "$tree"
assert_nonzero "H1 a red installer suite fails the run" "$status"
assert_not_contains "H1 a red installer suite cannot report success" 'ci-local: ok' "$out"
assert_contains "H1 the verdict line names the step that failed" \
  'ci-local: FAILED (installer prerequisite refusals' "$out"

# ------------- Group I: the two-machine backup proof is a real signal

echo "==> I. the two-machine end-to-end backup proof (#356)"

# I1 is the control for everything below it, and the same control G1 is:
# without it, I3's absence assertion would also pass against a gate that
# had lost the step entirely.
tree="$(make_full_tree)"
run_gate "$tree"
assert_contains "I1 a complete run invokes the two-machine step" \
  'two throwaway machines' "$out"
assert_contains "I1 the step really ran (and was the stub)" "$TWO_MACHINE_STUB" "$out"
assert_contains "I1 a complete run still reports success" 'ci-local: ok' "$out"

# I2: the out-loud opt-out ledgers rather than printing a note and carrying
# on, the same shape as CI_LOCAL_SKIP_DOCKER and CI_LOCAL_SKIP_E2E.
tree="$(make_full_tree)"
run_gate "$tree" CI_LOCAL_SKIP_TWO_MACHINE=1
assert_not_contains "I2 CI_LOCAL_SKIP_TWO_MACHINE=1 does not run the step" "$TWO_MACHINE_STUB" "$out"
assert_not_contains "I2 CI_LOCAL_SKIP_TWO_MACHINE=1 cannot report success" 'ci-local: ok' "$out"
assert_contains "I2 CI_LOCAL_SKIP_TWO_MACHINE=1 ends INCOMPLETE" 'ci-local: INCOMPLETE' "$out"
assert_contains "I2 the summary names the proof that did not run" \
  'two-machine end-to-end backup proof' "$out"
assert_eq "I2 CI_LOCAL_SKIP_TWO_MACHINE=1 exits $INCOMPLETE" "$INCOMPLETE" "$status"

# I3: FAST leaves it out, under the never-merge-on-FAST rule D4 owns.
tree="$(make_full_tree)"
run_gate "$tree" CI_LOCAL_FAST=1
assert_not_contains "I3 CI_LOCAL_FAST=1 does not run the two-machine step" "$TWO_MACHINE_STUB" "$out"

# I4: a failed proof refuses the commit rather than annotating it.
tree="$(make_full_tree)"
printf '#!/usr/bin/env bash\necho "the digests did not match"\nexit 1\n' \
  >"$tree/scripts/e2e/two-machine-backup.sh"
run_gate "$tree"
assert_nonzero "I4 a failed backup proof fails the run" "$status"
assert_not_contains "I4 a failed backup proof cannot report success" 'ci-local: ok' "$out"
assert_contains "I4 the verdict line names the step that failed" \
  'ci-local: FAILED (two throwaway machines' "$out"

# I5 is the case this step has that no other step here has, and the one
# worth the most. "This machine cannot perform the proof" is neither a pass
# nor a failure: no Docker, or a daemon that refuses a privileged container
# so docker-in-docker cannot start. The script says so and exits 3. The
# gate has to LEDGER that, which means neither reporting ok nor dying on
# it, and the distinction is invisible in the exit status alone, which is
# why it is asserted in all three directions.
tree="$(make_full_tree)"
printf '#!/usr/bin/env bash\necho "==> two-machine: CANNOT RUN. no privileged containers here" >&2\nexit 3\n' \
  >"$tree/scripts/e2e/two-machine-backup.sh"
run_gate "$tree"
assert_not_contains "I5 a proof this machine cannot perform cannot report success" 'ci-local: ok' "$out"
assert_contains "I5 it ends INCOMPLETE rather than FAILED" 'ci-local: INCOMPLETE' "$out"
assert_contains "I5 the summary names what could not be performed" \
  'this machine could not perform it' "$out"
assert_eq "I5 it exits $INCOMPLETE" "$INCOMPLETE" "$status"

# I6 is I5's positive control. Without it, I5 would also pass against a
# gate that treated EVERY non-zero status as a ledgered skip, which would
# turn a real digest mismatch into an INCOMPLETE nobody reads. I4 asserts
# the failure half; this asserts that the two statuses are actually told
# apart rather than collapsed.
tree="$(make_full_tree)"
printf '#!/usr/bin/env bash\nexit 3\n' >"$tree/scripts/e2e/two-machine-backup.sh"
run_gate "$tree"
three_status="$status"
tree="$(make_full_tree)"
printf '#!/usr/bin/env bash\nexit 1\n' >"$tree/scripts/e2e/two-machine-backup.sh"
run_gate "$tree"
if [ "$three_status" = "$INCOMPLETE" ] && [ "$status" != "$INCOMPLETE" ]; then
  pass "I6 exit 3 and exit 1 from the proof are told apart"
else
  fail "I6 exit 3 and exit 1 from the proof are told apart, got $three_status and $status"
fi

# I7: every container the proof creates is removed WITH its volumes.
#
# Not a stub test, because the property is in the real script rather than
# in the gate's handling of it. docker:28-dind declares
# VOLUME /var/lib/docker, so each manager machine gets an anonymous host
# volume carrying that machine's whole inner Docker state, and
# `docker rm -f` leaves it behind. Nothing references a leftover, so
# nothing complains, and the shared Docker disk fills silently until an
# install refuses on free space. That is not hypothetical: it is how a
# full run failed, with the installer's own preflight reporting 912 MiB
# free against its 2048 MiB floor on a Docker VM at 98%.
#
# Asserted on the source rather than by running a case, because running
# one costs a container image build and this has to hold for a removal
# site somebody adds later, not only for the two that exist today.
proof_script="$(dirname "$0")/../e2e/two-machine-backup.sh"
if [ ! -f "$proof_script" ]; then
  fail "I7 the two-machine proof script is where this expects it"
else
  # Comment lines are excluded, and the prose above the teardown quotes
  # `docker rm -f` on purpose to explain why it is wrong; a check that
  # cannot tell a command from its own explanation is not a check.
  bare_removals="$(grep -vE '^[[:space:]]*#' "$proof_script" | grep -nE 'docker rm ' | grep -v -- '-fv' || true)"
  if [ -z "$bare_removals" ]; then
    pass "I7 every docker rm in the proof removes the container's volumes too"
  else
    fail "I7 every docker rm in the proof removes the container's volumes too, but these do not:
$bare_removals"
  fi
  assert_contains "I7 the proof says why -v is load-bearing" \
    'VOLUME /var/lib/docker' "$(cat "$proof_script")"
fi

# ------------- Group J: a daemon that dies in the middle of a run (#457)

echo "==> J. the Docker daemon during the run, not only at the preflight"

# Docker Desktop's Resource Saver stops the hypervisor after five idle
# minutes and cold-starts it on the next API call. This gate has several
# Docker-free stretches longer than that, so every run was restarting the VM
# somewhere in the middle, and two runs on 2026-09-04 died of it: one VM came
# up and died 86ms later racing the previous hypervisor over Docker.raw, and
# one `images/create` returned HTTP 500 for 2m31s.
#
# Neither showed up as a failure. The gate probed the daemon once, at the
# top, so a VM that died at minute 18 turned every Docker-backed suite into a
# t.Skip, `go test` exited 0, and the run reported on tests that never ran.
# That is #160's defect again, arriving through the machine rather than
# through the checkout.
#
# This stub is that shape exactly: a daemon that answers the preflight probe
# and then stops answering. It also records every call, which is how the two
# sentinel cases below watch a container they cannot create for real.
set_docker_recording() { # <tree>
  cat >"$1/bin/docker" <<'STUB'
#!/bin/sh
# Records every invocation, and stops answering `docker info` after
# DOCKER_STUB_MAX_INFO of them. Everything else always succeeds, so the
# number of non-info calls the gate makes cannot change what a case measures.
if [ -n "${DOCKER_STUB_LOG:-}" ]; then printf '%s\n' "$*" >>"$DOCKER_STUB_LOG"; fi
if [ "${1:-}" = info ]; then
  count_file="${DOCKER_STUB_LOG:-/dev/null}.info"
  n=$(( $(cat "$count_file" 2>/dev/null || echo 0) + 1 ))
  echo "$n" >"$count_file" 2>/dev/null || true
  if [ "$n" -gt "${DOCKER_STUB_MAX_INFO:-99}" ]; then exit 1; fi
fi
exit 0
STUB
  chmod +x "$1/bin/docker"
}

# J1: the headline. One `docker info` answered, every one after it refused,
# and the run must FAIL by name at the step that needed the daemon.
tree="$(make_full_tree)"
set_docker_recording "$tree"
run_gate "$tree" DOCKER_STUB_LOG="$tree/docker.log" DOCKER_STUB_MAX_INFO=1
assert_nonzero "J1 a daemon that dies mid-run fails the run" "$status"
assert_not_contains "J1 a daemon that dies mid-run cannot report success" \
  'ci-local: ok' "$out"
assert_not_contains "J1 a daemon that dies mid-run is not a ledgered skip" \
  'ci-local: INCOMPLETE' "$out"
assert_contains "J1 the failure names the step that needed the daemon" \
  'ci-local: FAILED (core/ go test -race ./...' "$out"
assert_contains "J1 the failure says the daemon died during the run" \
  'it was at the start of this run' "$out"
assert_contains "J1 the failure points at Resource Saver" 'Resource Saver' "$out"
# The positive control for "this is a mid-run failure and not the preflight
# refusal wearing a different hat": the run really did get past the preflight
# and do work first.
assert_contains "J1 the run got past the preflight before it failed" \
  'core/ go build' "$out"

# J2: J1's control. Same tree, same stub, same everything except that the
# daemon keeps answering. Without this, J1 would also pass against a gate
# that had simply lost the ability to finish at all.
tree="$(make_full_tree)"
set_docker_recording "$tree"
run_gate "$tree" DOCKER_STUB_LOG="$tree/docker.log" DOCKER_STUB_MAX_INFO=99
assert_contains "J2 a daemon that stays up reports success" 'ci-local: ok' "$out"
assert_eq "J2 a daemon that stays up exits 0" 0 "$status"

# J3: the sentinel. Resource Saver measures IDLE, so the fix that does not
# depend on anyone's GUI settings is to never be idle. J2's run is reused
# through its recorded call log: one `docker run` of a sleeping container,
# and one removal of that same container on the way out.
docker_log="$(cat "$tree/docker.log" 2>/dev/null || true)"
assert_contains "J3 the gate starts a sentinel container" \
  'run -d --rm --name ci-local-sentinel-' "$docker_log"
assert_contains "J3 the sentinel just sleeps" 'sleep infinity' "$docker_log"
assert_contains "J3 the sentinel is labelled" \
  '--label rclone-manager-ci-local-sentinel=1' "$docker_log"
assert_contains "J3 the gate removes the sentinel on the way out" \
  'rm -f ci-local-sentinel-' "$docker_log"
# Same container, not just some container of each shape: a start and a
# removal that name different containers is the leak this case is about.
sentinel_started="$(printf '%s\n' "$docker_log" | sed -n 's/.*--name \(ci-local-sentinel-[0-9]*\).*/\1/p' | head -1)"
sentinel_removed="$(printf '%s\n' "$docker_log" | sed -n 's/^rm -f \(ci-local-sentinel-[0-9]*\)$/\1/p' | head -1)"
if [ -n "$sentinel_started" ] && [ "$sentinel_started" = "$sentinel_removed" ]; then
  pass "J3 the container that was started is the container that was removed"
else
  fail "J3 the container that was started is the container that was removed" \
    "started [$sentinel_started], removed [$sentinel_removed]"
fi
# Nothing after the removal, so the sentinel cannot outlive the run.
assert_eq "J3 the removal is the last thing the gate asks docker for" \
  "rm -f $sentinel_started" "$(printf '%s\n' "$docker_log" | tail -1)"

# J4: the removal is not conditional on the run going well. A sentinel that
# survives a failed run is #150's leak wearing a different label, and a
# failed run is the common case for a gate that runs on every commit.
tree="$(make_full_tree)"
set_docker_recording "$tree"
printf 'package stub\n\nthis is not go\n' >"$tree/core/stub.go"
run_gate "$tree" DOCKER_STUB_LOG="$tree/docker.log" DOCKER_STUB_MAX_INFO=99
assert_nonzero "J4 the broken tree fails the run" "$status"
docker_log="$(cat "$tree/docker.log" 2>/dev/null || true)"
assert_contains "J4 a failed run still starts its sentinel" \
  'run -d --rm --name ci-local-sentinel-' "$docker_log"
assert_contains "J4 a failed run still removes its sentinel" \
  'rm -f ci-local-sentinel-' "$docker_log"

# J5: the sentinel's label must NOT be the one core/tests/dockerlease
# sweeps. That sweep removes labelled containers older than fifteen minutes,
# and a full gate run is twenty-five, so sharing the label would delete the
# sentinel out from under the run it exists to protect, at almost exactly the
# halfway point. Read out of the Go source rather than copied here, because
# the failure this guards against is the two constants drifting together.
lease_source="$REPO_ROOT/core/tests/dockerlease/dockerlease.go"
if [ ! -f "$lease_source" ]; then
  fail "J5 core/tests/dockerlease/dockerlease.go is where this expects it"
else
  lease_key="$(sed -n 's/^[[:space:]]*LabelKey = "\(.*\)"$/\1/p' "$lease_source" | head -1)"
  sentinel_key="$(sh -c ". '$GATE_LIB' && printf '%s' \"\$GATE_SENTINEL_LABEL_KEY\"")"
  if [ -n "$lease_key" ] && [ -n "$sentinel_key" ] && [ "$lease_key" != "$sentinel_key" ]; then
    pass "J5 the sentinel does not carry the label dockerlease sweeps"
  else
    fail "J5 the sentinel does not carry the label dockerlease sweeps" \
      "dockerlease LabelKey [$lease_key], sentinel [$sentinel_key]"
  fi
fi

# J6: an interrupt is the other way a sentinel outlives its run, and it is
# not a rare path on a gate that takes twenty-five minutes from a pre-commit
# hook. Driven against the library directly, because run_gate has no way to
# send a signal into the middle of a run.
int_log="$SANDBOX/interrupt-docker.log"
cat >"$SANDBOX/interrupt-docker" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >>"$DOCKER_STUB_LOG"
exit 0
STUB
chmod +x "$SANDBOX/interrupt-docker"
mkdir -p "$SANDBOX/intbin"
cp "$SANDBOX/interrupt-docker" "$SANDBOX/intbin/docker"
cat >"$SANDBOX/interrupted-run.sh" <<'RUN'
set -e
. "$1"
gate_install_traps
gate_start_docker_sentinel >/dev/null
gate_step "core/ tests/crashmatrix under gotestwatch" >/dev/null
kill -INT $$
echo "REACHED-THE-LINE-AFTER-THE-SIGNAL"
RUN
int_out="$(DOCKER_STUB_LOG="$int_log" PATH="$SANDBOX/intbin:$PATH" \
  sh "$SANDBOX/interrupted-run.sh" "$GATE_LIB" 2>&1)"
int_status=$?
assert_eq "J6 an interrupted run exits 130" 130 "$int_status"
assert_not_contains "J6 an interrupted run stops where it was interrupted" \
  'REACHED-THE-LINE-AFTER-THE-SIGNAL' "$int_out"
assert_contains "J6 an interrupted run still prints a verdict naming the step" \
  'ci-local: FAILED (core/ tests/crashmatrix under gotestwatch, interrupted by SIGINT)' "$int_out"
assert_contains "J6 an interrupted run removes its sentinel" \
  'rm -f ci-local-sentinel-' "$(cat "$int_log" 2>/dev/null || true)"

# J7: CI_LOCAL=1 reaches the processes the gate starts. The Docker fixtures
# key on it to tell "this laptop has no Docker", which is an honest skip
# outside the gate, from "the daemon this gate already used has gone away",
# which is a failure. Measured through a child process rather than in this
# shell, because being exported is the whole point.
tree="$(make_full_tree)"
printf '#!/usr/bin/env bash\necho "CI-LOCAL-ENV=[${CI_LOCAL:-unset}]"\nexit 0\n' \
  >"$tree/scripts/perf/selftest.sh"
run_gate "$tree"
assert_contains "J7 the gate exports CI_LOCAL=1 to the steps it runs" \
  'CI-LOCAL-ENV=[1]' "$out"

# J8: the mutation-anchor step (#458). The file is written under separate
# work, so the gate guards on its existence; these two cases are what stops
# that guard from being a permanent silent skip. A tree that has the script
# must run it, and a tree whose script fails must fail the run. The label is
# asserted, not just the exit status: this step goes red when a mutation
# anchor in the compat or conformance selftest has drifted off the code it
# names, and the person reading the verdict line has to know that is what
# broke.
tree="$(make_full_tree)"
mkdir -p "$tree/scripts/selftest"
printf '#!/usr/bin/env bash\necho "ANCHORS-STUB-RAN"\nexit 0\n' \
  >"$tree/scripts/selftest/check-anchors.sh"
run_gate "$tree"
assert_contains "J8 a tree with an anchors script runs it" 'ANCHORS-STUB-RAN' "$out"
assert_contains "J8 the anchors step is announced, and says mutation anchors" \
  'mutation anchors in the compat, conformance and race selftests' "$out"
assert_eq "J8 a passing anchors script leaves the run green" 0 "$status"

tree="$(make_full_tree)"
mkdir -p "$tree/scripts/selftest"
printf '#!/usr/bin/env bash\necho "ANCHORS-STUB-FAILED"\nexit 1\n' \
  >"$tree/scripts/selftest/check-anchors.sh"
run_gate "$tree"
assert_nonzero "J8 a failing anchors script fails the run" "$status"
assert_contains "J8 the failure names the anchors step" \
  'ci-local: FAILED (mutation anchors in the compat, conformance and race selftests' "$out"
# And it fails EARLY: the point of putting a one-second check near the top is
# that nobody waits twenty-five minutes to hear about a broken link.
assert_not_contains "J8 a failing anchors script fails before the Go suites" \
  'core/ go test' "$out"

# J9: the Resource Saver reading, both answers and both ways of having no
# answer. It is a warning and never a refusal, so every one of these paths
# has to return 0: a gate that refused over a Docker Desktop preference
# would be unrunnable on the machine that has it on, which is this one.
rs_dir="$SANDBOX/resource-saver"
mkdir -p "$rs_dir"
printf '{"UseResourceSaver": true, "Other": 1}\n' >"$rs_dir/on.json"
printf '{"UseResourceSaver":false}\n' >"$rs_dir/off.json"
printf '{"SomethingElse":true}\n' >"$rs_dir/nokey.json"
for rs_case in on off nokey missing; do
  case "$rs_case" in
    on) want=on ;;
    off) want=off ;;
    *) want=unknown ;;
  esac
  got="$(GATE_DOCKER_SETTINGS_FILE="$rs_dir/$rs_case.json" \
    sh -c ". '$GATE_LIB' && gate_resource_saver_state")"
  assert_eq "J9 Resource Saver reads $rs_case as $want" "$want" "$got"
  warn_out="$(GATE_DOCKER_SETTINGS_FILE="$rs_dir/$rs_case.json" \
    sh -c ". '$GATE_LIB' && gate_warn_resource_saver" 2>&1)"
  assert_eq "J9 the $rs_case warning never fails the run" 0 $?
  if [ "$rs_case" = on ]; then
    assert_contains "J9 the warning names the setting" 'Resource Saver' "$warn_out"
    assert_contains "J9 the warning says where it lives" \
      'Docker Desktop > Settings > Resources > Advanced' "$warn_out"
  else
    assert_eq "J9 $rs_case prints no warning" "" "$warn_out"
  fi
done

# ------------- Group K: every Go suite in the gate runs under -race (#417)

echo "==> K. the race detector is on, and on everything"

# The gate ran no -race anywhere until #417. That is the same shape as every
# other hole this suite exists for: `go test` exits 0 whether the detector
# looked or not, so "this tree has no data race" and "nobody asked" were the
# same output. Turning it on found two real things in one package on the
# first run, one of them a genuine data race in a dependency.
#
# It is a flag on the steps that already exist rather than a step of its
# own, which is the decision these cases pin down. A separate step can be
# commented out and the suites still run and still report ok; a flag cannot
# be removed without the step going with it. So the assertion is not "there
# is a race step" but "no `go test` in this gate runs without the detector",
# which is a rule a new module cannot be added around by accident.

# race_flag_problems <path to a ci-local.sh> -> one line per invocation
# that runs a Go suite without the detector and without saying why.
#
# It reads command lines only: `GOWORK=off go test` is how every suite in
# this gate is actually invoked, and `cmd/gotestwatch` is the one that runs
# through the progress-bounded wrapper instead. Step headings and comments
# are prose about those commands and are skipped, which matters because at
# least one heading says "go test -timeout" while describing the flag it
# does NOT pass.
#
# A trailing `# no -race: <reason>` on the command line itself is the one
# way out, and it is not a loophole because it is counted: K4 below asserts
# how many lines carry it and which. An exclusion nobody can enumerate is
# how a gate ends up not running the thing it says it runs.
race_flag_problems() {
  awk '
    /^[[:space:]]*#/ { next }
    /GOWORK=off go test/ && !/GOWORK=off go test -race/ {
      if ($0 ~ /# no -race: [^ ]/) { next }
      printf "  a Go suite runs without -race and without a reason, line %d: %s\n", NR, $0
    }
    /cmd\/gotestwatch/ && !/gotestwatch -race/ {
      printf "  the gotestwatch suites run without -race, line %d: %s\n", NR, $0
    }
  ' "$1"
}

# race_flag_exceptions <path> -> one line per deliberate exclusion.
race_flag_exceptions() {
  awk '
    /^[[:space:]]*#/ { next }
    /GOWORK=off go test/ && !/GOWORK=off go test -race/ && /# no -race: [^ ]/ {
      printf "%d: %s\n", NR, $0
    }
  ' "$1"
}

# race_flag_invocations <path> -> how many Go suites the script runs at all.
# The count is the control for the scan: a script this scan finds nothing
# wrong with because it found nothing at all would otherwise read as a pass.
race_flag_invocations() {
  grep -cE '^[^#]*(GOWORK=off go test|cmd/gotestwatch)' "$1" | tr -d '[:space:]'
}

real_gate="$SCRIPTS_DIR/ci-local.sh"

# K1: the real script, as it stands. Every Go suite, detector on.
k1_problems="$(race_flag_problems "$real_gate")"
if [ -z "$k1_problems" ]; then
  pass "K1 every Go suite in scripts/ci-local.sh runs under -race"
else
  fail "K1 every Go suite in scripts/ci-local.sh runs under -race" "$k1_problems"
fi

# K1's control: the scan has something to find. Nine Go suites today (the
# FAST core step, the full core step, gotestwatch, apps/common,
# distribution, distribution/packaging, apps/generic, apps/synology and
# apps/ugos/backend), and the floor is deliberately low so adding or
# removing a module does not fail this, while an empty scan does.
k1_count="$(race_flag_invocations "$real_gate")"
if [ "$k1_count" -ge 5 ]; then
  pass "K1 the scan actually found the gate's Go suites ($k1_count of them)"
else
  fail "K1 the scan actually found the gate's Go suites" \
    "found $k1_count invocations of \`go test\` or gotestwatch in $real_gate, want at least 5; the scan is looking for the wrong shape"
fi

# K2: the mutation. Strip the flag out of a copy and the scan has to say so,
# by name, or K1 is a check that cannot fail.
tree="$(make_full_tree)"
script="$tree/scripts/ci-local.sh"
sed -i.bak 's/go test -race/go test/g; s/gotestwatch -race/gotestwatch/g' "$script"
rm -f "$script.bak"
k2_problems="$(race_flag_problems "$script")"
assert_contains "K2 a gate with the flag stripped out is caught" \
  'a Go suite runs without -race' "$k2_problems"
assert_contains "K2 the gotestwatch suites are caught too" \
  'the gotestwatch suites run without -race' "$k2_problems"

# K3: the other direction, end to end. A static scan of the file says the
# flag is written down; this says a run actually announces it, which is what
# stops the whole thing from passing against a step nobody reaches.
tree="$(make_full_tree)"
run_gate "$tree"
assert_eq "K3 a full run with -race everywhere still reaches ok" 0 "$status"
assert_contains "K3 the core step announces the detector" \
  'core/ go test -race ./...' "$out"
assert_contains "K3 the gotestwatch step announces the detector" \
  'under gotestwatch, -race' "$out"
assert_contains "K3 the other Go modules announce it too" \
  'apps/common go build, vet, test -race' "$out"

# K4: the exclusions, enumerated. One suite is deliberately out
# (distribution/packaging: no goroutine anywhere in it, so nothing to
# detect, and it is the most CPU-bound package here). That is a decision
# somebody made with a number in hand, and this is what keeps the next one
# from being made by accident: the count is pinned, so a second exclusion
# fails here until it is added on purpose.
k4_exceptions="$(race_flag_exceptions "$real_gate")"
k4_count="$(printf '%s' "$k4_exceptions" | grep -c . | tr -d '[:space:]')"
assert_eq "K4 exactly one Go suite in the gate is excluded from -race" 1 "$k4_count"
assert_contains "K4 the exclusion is distribution/packaging" './packaging/' "$k4_exceptions"
assert_contains "K4 the exclusion says why on the line itself" \
  'no goroutine of its own' "$k4_exceptions"

# K4's control: the enumeration notices a new one. Without this, K4's count
# assertion would also pass against a scan that can no longer see any
# exclusion at all.
tree="$(make_full_tree)"
script="$tree/scripts/ci-local.sh"
sed -i.bak 's|^(cd apps/common && \(.*\)go test -race \./\.\.\.)$|(cd apps/common \&\& \1go test ./...) # no -race: a second exclusion nobody decided on|' "$script"
rm -f "$script.bak"
k4_mutant="$(race_flag_exceptions "$script")"
k4_mutant_count="$(printf '%s' "$k4_mutant" | grep -c . | tr -d '[:space:]')"
if [ "$k4_mutant_count" -eq 2 ]; then
  pass "K4 a second exclusion is counted, so the count assertion has teeth"
else
  fail "K4 a second exclusion is counted, so the count assertion has teeth" \
    "the mutation should have produced 2 exclusions, the scan found $k4_mutant_count:
$k4_mutant"
fi

# ------------- Group L: formatting is checked at all (#417)

echo "==> L. the formatting gate, in both halves"

# Formatting was not checked anywhere until #417, and the way that surfaced
# is the reason this group exists. Two Go files in this repository were not
# gofmt-clean, one of them since it was written, and every gate step stayed
# green: `go build`, `go vet` and every linter .golangci.yml enabled are all
# indifferent to layout. A check that was never there and a check that is
# there and looking produce the same output, which is #160's defect arriving
# through a third door.
#
# It is closed in two places, and neither makes the other redundant.
# .golangci.yml enables the gofmt formatter, which covers the five Go
# modules, and scripts/format/check-gofmt.sh sweeps every tracked .go file,
# which is the only thing that reaches the two Go files living outside every
# module and outside go.work. L1 and L2 are the sweep as a gate step; L3 is
# the config, with the mutation that proves the assertion can fail.

# L1: the sweep runs, and a run that has it stays green. The marker is the
# control against L2 passing because the step was never reached at all.
tree="$(make_full_tree)"
run_gate "$tree"
assert_contains "L1 the gate runs the formatting sweep" "$GOFMT_STUB" "$out"
assert_contains "L1 the step says what it covers" \
  'every tracked Go file is gofmt-clean' "$out"
assert_eq "L1 a passing sweep leaves the run green" 0 "$status"

# L2: a tree with unformatted Go in it fails the run, by name, and early.
# Early matters for the same reason it did for the anchors check: this is a
# half-second sweep, and nobody should wait out the Docker-backed suites to
# be told about whitespace.
tree="$(make_full_tree)"
printf '#!/usr/bin/env bash\necho "not gofmt-clean" >&2\nexit 1\n' \
  >"$tree/scripts/format/check-gofmt.sh"
run_gate "$tree"
assert_nonzero "L2 an unformatted tree fails the run" "$status"
assert_not_contains "L2 an unformatted tree cannot report success" 'ci-local: ok' "$out"
assert_contains "L2 the verdict names the formatting step" \
  'ci-local: FAILED (every tracked Go file is gofmt-clean' "$out"
assert_not_contains "L2 the formatting sweep fails before the Go suites" \
  'core/ go test' "$out"

# L4: the other half of the same blind spot. Being outside every module
# does not only cost those two files their formatting: this gate vets and
# lints per module too, so nothing had ever vetted or linted them either.
# The check that does now lives in scripts/architecture/ but runs HERE,
# near the top, and that placement is the thing this cell pins: the other
# architecture checks run after the Go suites, and a Go file nobody checks
# is worth hearing about before twenty minutes of Docker-backed tests.
tree="$(make_full_tree)"
run_gate "$tree"
assert_contains "L4 the gate checks the Go files no module owns" "$UNOWNED_STUB" "$out"
assert_contains "L4 the step says what it covers" \
  'every Go file no module owns still passes go vet and golangci-lint' "$out"
assert_eq "L4 a passing check leaves the run green" 0 "$status"

tree="$(make_full_tree)"
printf '#!/usr/bin/env bash\necho "go vet found something" >&2\nexit 1\n' \
  >"$tree/scripts/architecture/check-unowned-go.sh"
run_gate "$tree"
assert_nonzero "L4 an unvetted Go file fails the run" "$status"
assert_contains "L4 the verdict names the step" \
  'ci-local: FAILED (every Go file no module owns' "$out"
assert_not_contains "L4 it fails before the Go suites" 'core/ go test' "$out"

# L3: the other half. golangci-lint checks formatting only if the config
# asks it to, and the config is a file anybody can trim.
#
# The reading is structural rather than a grep for the word, because
# .golangci.yml mentions gofmt several times in the prose explaining why it
# is enabled, and a scan that matched those would pass against a config that
# had lost the setting and kept the paragraph.
gofmt_formatter_enabled() { # <path to a .golangci.yml>
  python3 "$SCRIPTS_DIR/tests/gofmt-formatter-enabled.py" "$1"
}

if ! python3 -c 'import yaml' 2>/dev/null; then
  fail "L3 .golangci.yml enables the gofmt formatter, and python3 has no yaml module to read it structurally"
else
  if gofmt_formatter_enabled "$REPO_ROOT/.golangci.yml"; then
    pass "L3 .golangci.yml enables the gofmt formatter"
  else
    fail "L3 .golangci.yml enables the gofmt formatter" \
      "formatters.enable does not list gofmt, so golangci-lint checks no formatting in any of the five Go modules"
  fi

  # L3's mutation: drop the setting, keep every word of the prose around it,
  # and the scan has to notice. Without this, L3 would also pass against a
  # scan that can no longer tell the difference.
  cp "$REPO_ROOT/.golangci.yml" "$SANDBOX/golangci-no-formatters.yml"
  perl -0pi -e 's/formatters:\n  enable:\n    - gofmt\n//' "$SANDBOX/golangci-no-formatters.yml"
  if gofmt_formatter_enabled "$SANDBOX/golangci-no-formatters.yml"; then
    fail "L3 the scan notices a config with the formatter removed" \
      "the mutated config still reads as enabling gofmt, so L3 cannot fail"
  else
    pass "L3 the scan notices a config with the formatter removed, so L3 has teeth"
  fi
fi

# ------------------------------------------------------------------ result

echo
if [ "$failures" -eq 0 ]; then
  echo "==> ci-local gate self-test: ok ($checks checks)"
  exit 0
fi
echo "==> ci-local gate self-test: $failures of $checks checks FAILED" >&2
exit 1

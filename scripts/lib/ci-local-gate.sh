#!/usr/bin/env sh
# Helpers that keep scripts/ci-local.sh's last line honest (issue #160).
# Sourced by that script, never executed on its own.
#
# Three jobs, and they are the same job seen from three ends:
#
#   1. Tell "this capability is not part of this tree/machine" apart from
#      "it is part of this tree but nobody made it usable". A JS workspace
#      that is not here is nothing to check; one that is here with no
#      node_modules/ is a hole in the gate. A Docker daemon that is not
#      running is the same shape: the Docker-backed suites still get
#      invoked, they just t.Skip and report ok.
#   2. Keep a ledger of every check that did NOT run, so the run can end with
#      "ci-local: ok" only when that ledger is empty.
#   3. Say the same thing in the exit code that the last line says, because a
#      program reading this gate cannot read prose. ok is 0, INCOMPLETE is
#      GATE_INCOMPLETE (3), an actual failure is whatever failed.
#
# Why this exists: the gate used to treat a missing node_modules/ as "print a
# note and carry on", then finish with "==> ci-local: ok" regardless.
# node_modules/ is gitignored and everyone here works in a fresh
# `git worktree`, so the DEFAULT state of a new checkout was "the frontend
# lint/typecheck/vitest suites and the cross-provider conformance suite never
# ran, and the gate reported success anyway". That last line is what this
# project reads as merge evidence, and a skip that is indistinguishable from a
# pass in both the exit code and the final line is the same class of bug as a
# test that asserts nothing: green because it could not look.
#
# Covered by scripts/tests/ci-local-gate.test.sh, which the gate itself runs.

# The exit status of a run that performed less than it was asked to. Distinct
# from 0 (performed everything) and from 1 (something actually failed), so a
# wrapper can tell all three apart. .husky/pre-commit tolerates this one on
# purpose and says so out loud; an automated merge must not.
GATE_INCOMPLETE=3

# Every check that was asked for and did not run, one per line.
gate_skipped=""

# Set to 1 the moment any "==> ci-local: ..." verdict line has been printed,
# so the exit trap does not print a second, contradictory one.
gate_marked=0

# The heading of the step currently running, so a failure can name it.
gate_last_step="(startup)"

# gate_note_skip <what did not run>
# Record a check the run could not perform. Anything in here makes the final
# line say INCOMPLETE instead of ok, and the exit status GATE_INCOMPLETE
# instead of 0.
gate_note_skip() {
  if [ -z "$gate_skipped" ]; then
    gate_skipped="$1"
  else
    gate_skipped="$gate_skipped
$1"
  fi
}

# gate_step <heading>
# Announce a step and remember it. Every step heading in ci-local.sh goes
# through here so that a run which dies under `set -e` can still print a
# verdict line naming the step that killed it, instead of ending on a bare
# command error with no marker at all.
gate_step() {
  gate_last_step="$1"
  echo "==> $1"
}

# gate_workspace_state <dir>  ->  absent | uninstalled | installed
#
# package.json is the marker of "this workspace is part of this tree", not the
# directory: git cannot track an empty directory, so a workspace that is in the
# tree always arrives with its package.json, and a directory without one is not
# something anyone can install. That is the whole absent-versus-uninstalled
# distinction, and it is why an optional component that simply does not exist
# here (apps/ugos/frontend/upk-proof is not in this tree today) stays a
# non-event rather than becoming a hard failure.
gate_workspace_state() {
  if [ ! -f "$1/package.json" ]; then
    echo absent
  elif [ ! -d "$1/node_modules" ]; then
    echo uninstalled
  else
    echo installed
  fi
}

# gate_install_hint <dir>  ->  the exact command that fixes it
# npm ci only when there is a lockfile to be exact against; npm install
# otherwise, because npm ci fails outright without one.
gate_install_hint() {
  if [ -f "$1/package-lock.json" ]; then
    echo "cd $1 && npm ci"
  else
    echo "cd $1 && npm install"
  fi
}

# gate_require_js_deps <dir>...
# Report the state of every JS workspace the gate knows about, then refuse the
# run if any of them is present-but-uninstalled, unless skipping was chosen out
# loud (CI_LOCAL_SKIP_JS=1) or this is the documented fast-iteration loop
# (CI_LOCAL_FAST=1). Returns 1 to refuse, which under the caller's `set -e`
# ends the run.
#
# Deliberately runs before the Go suites: twenty minutes of Docker-backed tests
# followed by "by the way, you needed npm ci" is a worse gate than a refusal in
# the first second.
gate_require_js_deps() {
  gate_uninstalled=""
  for gate_ws in "$@"; do
    case "$(gate_workspace_state "$gate_ws")" in
      absent)
        echo "==> js deps: $gate_ws not present in this tree"
        ;;
      installed)
        echo "==> js deps: $gate_ws installed"
        ;;
      uninstalled)
        echo "==> js deps: $gate_ws present but NOT installed"
        gate_uninstalled="$gate_uninstalled $gate_ws"
        ;;
    esac
  done

  [ -n "$gate_uninstalled" ] || return 0

  if [ "${CI_LOCAL_FAST:-0}" = "1" ] || [ "${CI_LOCAL_SKIP_JS:-0}" = "1" ]; then
    return 0
  fi

  gate_marked=1
  echo "" >&2
  echo "==> ci-local: FAILED. These JS workspaces are present in this tree but have no installed dependencies:" >&2
  for gate_ws in $gate_uninstalled; do
    echo "        $gate_ws" >&2
    echo "            fix:  $(gate_install_hint "$gate_ws")" >&2
  done
  echo "" >&2
  echo "    Their checks (lint, typecheck, eslint, the vitest suites, the cross-provider" >&2
  echo "    conformance suite, the production build) cannot run, and this gate no longer" >&2
  echo "    reports success for a run it could not perform." >&2
  echo "" >&2
  echo "    Install them, or choose the skip out loud with CI_LOCAL_SKIP_JS=1. A run that" >&2
  echo "    skips them is not merge evidence and will say so." >&2
  return 1
}

# The Docker-backed suites, named once so the refusal, the ledger entry and
# the documentation cannot drift apart.
GATE_DOCKER_SUITES="core/tests/... (the crash matrix), the SFTP integration tests in core/internal/transport/rclone, and apps/generic/tests/dockercli"

# gate_docker_state  ->  available | unavailable
# `docker info` rather than `command -v docker`: the binary being installed
# says nothing about the daemon being up, and it is the daemon that decides
# whether those suites run or t.Skip.
gate_docker_state() {
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    echo available
  else
    echo unavailable
  fi
}

# gate_require_docker
# The same rule as gate_require_js_deps, for the other capability this gate
# silently reported on without having. With the daemon down, every Docker
# test in this repository calls t.Skip, `go test ./...` prints ok per package
# and exits 0, and nothing reaches the ledger. That is issue #160 again in a
# larger and more consequential surface, so refuse by default and make
# CI_LOCAL_SKIP_DOCKER=1 the out-loud opt-out that ledgers instead.
gate_require_docker() {
  if [ "$(gate_docker_state)" = available ]; then
    echo "==> docker: daemon reachable"
    return 0
  fi

  echo "==> docker: daemon NOT reachable"

  if [ "${CI_LOCAL_SKIP_DOCKER:-0}" = "1" ]; then
    gate_note_skip "$GATE_DOCKER_SUITES: the Docker daemon is not reachable, so those tests t.Skip themselves and report ok (CI_LOCAL_SKIP_DOCKER=1)"
    return 0
  fi

  gate_marked=1
  echo "" >&2
  echo "==> ci-local: FAILED. The Docker daemon is not reachable, so these suites would t.Skip and still report ok:" >&2
  echo "        $GATE_DOCKER_SUITES" >&2
  echo "" >&2
  echo "    Those are the highest-consequence tests in this repository, and go test exits 0" >&2
  echo "    whether they ran or not, so a run without the daemon is not merge evidence." >&2
  echo "" >&2
  echo "    Start Docker, or choose the skip out loud with CI_LOCAL_SKIP_DOCKER=1. A run that" >&2
  echo "    skips them ends INCOMPLETE and says so." >&2
  return 1
}

# gate_summary
# The last thing a run prints. "ci-local: ok" is reachable only from an empty
# ledger; anything else says what did not run, refuses the word ok, and
# returns GATE_INCOMPLETE so a caller that never reads the text still knows.
gate_summary() {
  gate_marked=1
  if [ -z "$gate_skipped" ]; then
    echo "==> ci-local: ok"
    return 0
  fi

  echo "==> ci-local: INCOMPLETE. This run is not merge evidence. These checks did not run:"
  printf '%s\n' "$gate_skipped" | sed 's/^/        - /'
  echo "==> For a run that is merge evidence: install every JS workspace, start Docker, and re-run with CI_LOCAL_FAST, CI_LOCAL_SKIP_JS and CI_LOCAL_SKIP_DOCKER unset."
  return "$GATE_INCOMPLETE"
}

# gate_exit_marker <status>
# Installed as ci-local.sh's EXIT trap. Without it a run that dies under
# `set -e` produces no "==> ci-local: ..." line at all, so of the three
# outcomes only two were greppable and the missing one was the failure.
gate_exit_marker() {
  if [ "$1" -eq 0 ]; then
    return 0
  fi
  if [ "$gate_marked" = 1 ]; then
    return 0
  fi
  echo "==> ci-local: FAILED ($gate_last_step)" >&2
}

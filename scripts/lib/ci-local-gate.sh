#!/usr/bin/env sh
# Helpers that keep scripts/ci-local.sh's last line honest (issue #160).
# Sourced by that script, never executed on its own.
#
# Two jobs, and they are the same job seen from two ends:
#
#   1. Tell "this JS workspace is not part of this tree" apart from "this JS
#      workspace is part of this tree but nobody installed its dependencies".
#      The first is nothing to check. The second is a hole in the gate.
#   2. Keep a ledger of every check that did NOT run, so the run can end with
#      "ci-local: ok" only when that ledger is empty.
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

# Every check that was asked for and did not run, one per line.
gate_skipped=""

# gate_note_skip <what did not run>
# Record a check the run could not perform. Anything in here makes the final
# line say INCOMPLETE instead of ok.
gate_note_skip() {
  if [ -z "$gate_skipped" ]; then
    gate_skipped="$1"
  else
    gate_skipped="$gate_skipped
$1"
  fi
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

# gate_summary
# The last thing a run prints. "ci-local: ok" is reachable only from an empty
# ledger; anything else says what did not run and refuses the word ok.
gate_summary() {
  if [ -z "$gate_skipped" ]; then
    echo "==> ci-local: ok"
    return 0
  fi

  echo "==> ci-local: INCOMPLETE. This run is not merge evidence. These checks did not run:"
  printf '%s\n' "$gate_skipped" | sed 's/^/        - /'
  echo "==> For a run that is merge evidence: install every JS workspace and re-run with CI_LOCAL_FAST and CI_LOCAL_SKIP_JS unset."
  return 0
}

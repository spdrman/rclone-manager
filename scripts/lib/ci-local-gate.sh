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

# ---------------------------------------------------------------- Docker

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

# Set to 1 the moment the daemon has answered a probe in this run. It is the
# difference between "this machine has no Docker" and "the daemon this run
# already used has gone away", and the two deserve opposite answers: the
# first is a documented, ledgered opt-out, the second is a failure at the
# step that needed it.
gate_docker_verified=0

# Set to 1 once the Docker suites are in the ledger, so re-probing before
# every Docker-dependent step cannot write the same entry eight times.
gate_docker_skip_noted=0

# gate_note_docker_skip <reason>
# gate_note_skip for the Docker suites, at most once per run.
gate_note_docker_skip() {
  [ "$gate_docker_skip_noted" = 1 ] && return 0
  gate_docker_skip_noted=1
  gate_note_skip "$GATE_DOCKER_SUITES: $1"
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
    gate_docker_verified=1
    echo "==> docker: daemon reachable"
    return 0
  fi

  echo "==> docker: daemon NOT reachable"

  if [ "${CI_LOCAL_SKIP_DOCKER:-0}" = "1" ]; then
    gate_note_docker_skip "the Docker daemon is not reachable, so those tests t.Skip themselves and report ok (CI_LOCAL_SKIP_DOCKER=1)"
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

# gate_recheck_docker <step heading>
# The preflight probe, run again immediately before a step that needs the
# daemon. It costs about 100ms and it exists because a single probe at the
# top of a 25-minute run cannot see a VM that dies at minute 18 (issue #457).
#
# Docker Desktop's Resource Saver stops the hypervisor after five idle
# minutes, and this gate has several Docker-free stretches longer than that
# (the compat, api, perf and architecture self-tests), so the VM gets
# cold-started somewhere in the middle of every run whether anyone asked for
# it or not. Two runs on 2026-09-04 were lost to that restart: one VM came up
# and died 86ms later racing the previous hypervisor over Docker.raw, and one
# `images/create` returned HTTP 500 for 2m31s. Both showed up as t.Skip, not
# as a failure, because a daemon that goes away mid-run is invisible to a
# probe that already ran.
#
# The answer depends on what the run already knows:
#
#   - the daemon answers now                 -> carry on, silently.
#   - it never answered (a FAST run, which
#     does not run the preflight probe, or
#     a ledgered CI_LOCAL_SKIP_DOCKER=1 run) -> ledger once and carry on,
#     because that run already said out loud that it is not merge evidence.
#   - it answered earlier and does not now   -> fail, naming this step.
#
# Returns 1 in that last case, which under the caller's `set -e` ends the run.
gate_recheck_docker() {
  if [ "$(gate_docker_state)" = available ]; then
    return 0
  fi

  if [ "$gate_docker_verified" != 1 ]; then
    gate_note_docker_skip "the Docker daemon was never reachable in this run, so those tests t.Skip themselves and report ok"
    return 0
  fi

  if [ "${CI_LOCAL_SKIP_DOCKER:-0}" = "1" ]; then
    gate_note_docker_skip "the Docker daemon answered at the preflight and had gone away by \"$1\", so those tests t.Skip themselves and report ok (CI_LOCAL_SKIP_DOCKER=1)"
    return 0
  fi

  gate_marked=1
  echo "" >&2
  echo "==> ci-local: FAILED ($1). The Docker daemon is not reachable, and it was at the start of this run." >&2
  echo "" >&2
  echo "    It answered the preflight probe and has gone away since, so it died DURING this" >&2
  echo "    run. Without this check that would not have been a failure at all: these suites" >&2
  echo "        $GATE_DOCKER_SUITES" >&2
  echo "    call t.Skip when the daemon is down, go test exits 0, and the run would have" >&2
  echo "    carried on and reported on tests that never executed." >&2
  echo "" >&2
  echo "    On a Mac the usual cause is Docker Desktop's Resource Saver stopping the" >&2
  echo "    hypervisor during one of this gate's Docker-free stretches and failing to bring" >&2
  echo "    it back (issue #457): turn it off in Docker Desktop > Settings > Resources >" >&2
  echo "    Advanced > Resource Saver. Restart the daemon and re-run." >&2
  return 1
}

# gate_docker_step <heading>
# gate_step for a step that cannot honestly run without the daemon: announce
# it, then prove the daemon is still there before its command runs. One
# function rather than two lines per step, because the guard that gets
# forgotten at one call site is the one that matters.
gate_docker_step() {
  gate_step "$1"
  gate_recheck_docker "$1"
}

# ------------------------------------------------- Resource Saver, sentinel

# Where Docker Desktop keeps its settings on a Mac. Overridable so this
# gate's own self-test can drive both answers without touching the real one,
# and so a machine that keeps it elsewhere can point at it.
GATE_DOCKER_SETTINGS_FILE="${GATE_DOCKER_SETTINGS_FILE:-$HOME/Library/Group Containers/group.com.docker/settings-store.json}"

# gate_resource_saver_state  ->  on | off | unknown
# Read, never written, and never fatal. unknown covers every machine that is
# not a Mac running Docker Desktop, a Docker Desktop too old to have the
# setting, and a settings file this gate cannot parse. Whitespace is stripped
# first so `"UseResourceSaver" : true` reads the same as `"UseResourceSaver":true`.
gate_resource_saver_state() {
  if [ ! -f "$GATE_DOCKER_SETTINGS_FILE" ]; then
    echo unknown
    return 0
  fi
  gate_rs_json="$(tr -d ' \t\r\n' <"$GATE_DOCKER_SETTINGS_FILE" 2>/dev/null || true)"
  case "$gate_rs_json" in
    *'"UseResourceSaver":true'*) echo on ;;
    *'"UseResourceSaver":false'*) echo off ;;
    *) echo unknown ;;
  esac
}

# gate_warn_resource_saver
# A warning, never a refusal. Resource Saver being on is not a reason to
# refuse a run: the sentinel container below makes it harmless, and the
# per-step probes turn what it still breaks into a named failure. But it IS
# the first thing to check when the daemon dies at minute 18, so a run that
# was shaped by it should say so at the top rather than leave the reader to
# find it in a Docker Desktop settings pane.
gate_warn_resource_saver() {
  [ "$(gate_resource_saver_state)" = on ] || return 0
  echo "==> docker: Resource Saver is ON in Docker Desktop."
  echo "        It stops the hypervisor after five idle minutes and cold-starts it on the"
  echo "        next API call, which is how two gate runs died mid-flight (issue #457)."
  echo "        This run keeps a sentinel container up so the daemon is never idle, but the"
  echo "        setting is still worth turning off:"
  echo "        Docker Desktop > Settings > Resources > Advanced > Resource Saver."
}

# The sentinel container ---------------------------------------------------
#
# Resource Saver measures IDLE, so the fix that does not depend on anyone's
# GUI settings is to never be idle: one `alpine sleep infinity` container,
# started after the preflight and removed on the way out, for the whole life
# of the run. It costs a few MB of RAM and no CPU.
#
# Its label is deliberately NOT dockerlease's `rclone-manager-test`: that
# sweep removes labelled containers older than fifteen minutes, and a full
# gate run is twenty-five, so the sentinel would be swept out from under the
# run it exists to protect, at almost exactly the halfway point.
GATE_SENTINEL_LABEL_KEY=rclone-manager-ci-local-sentinel
GATE_SENTINEL_LABEL="$GATE_SENTINEL_LABEL_KEY=1"

# alpine:3.20 rather than alpine:latest: it is the base
# core/internal/transport/rclone's SFTP fixture already builds from, so any
# machine that can run this gate at all has it cached and the sentinel costs
# no pull. CI_LOCAL_SENTINEL_IMAGE overrides it; CI_LOCAL_SENTINEL=0 turns
# the whole thing off.
GATE_SENTINEL_IMAGE="${CI_LOCAL_SENTINEL_IMAGE:-alpine:3.20}"

# The name of the sentinel this run started, empty when there is none. Also
# the flag gate_stop_docker_sentinel keys on, so it is safe to call the stop
# twice (an interrupt handler and then the EXIT trap both do).
gate_sentinel_name=""

# gate_sweep_docker_sentinels
# #150's rule, applied to the sentinel: a run that is SIGKILLed cannot run
# its own trap, so the cleanup that has to work is the one on the way IN.
# A sentinel whose owning gate process is still alive is left alone, because
# several worktrees on this machine share one daemon and run their gates at
# the same time.
gate_sweep_docker_sentinels() {
  gate_sweep_list="$(docker ps -a --filter "label=$GATE_SENTINEL_LABEL" --format '{{.ID}} {{.Names}}' 2>/dev/null || true)"
  [ -n "$gate_sweep_list" ] || return 0
  printf '%s\n' "$gate_sweep_list" | while read -r gate_sweep_id gate_sweep_container; do
    [ -n "$gate_sweep_id" ] || continue
    gate_sweep_pid="${gate_sweep_container##*-}"
    case "$gate_sweep_pid" in
      ''|*[!0-9]*) ;;
      *) if kill -0 "$gate_sweep_pid" 2>/dev/null; then continue; fi ;;
    esac
    echo "==> docker sentinel: removing $gate_sweep_container, left behind by a run that no longer exists"
    docker rm -f "$gate_sweep_id" >/dev/null 2>&1 || true
  done
}

# gate_start_docker_sentinel
# Best-effort by design, exactly like dockerlease.Sweep: it reports what it
# did and never fails the run. A gate that refuses to start because a
# housekeeping container would not come up is worse than the idle daemon it
# was guarding against, and the per-step probes are the check that has teeth.
gate_start_docker_sentinel() {
  if [ "${CI_LOCAL_SENTINEL:-1}" = "0" ]; then
    echo "==> docker sentinel: not started (CI_LOCAL_SENTINEL=0)"
    return 0
  fi
  if [ "$(gate_docker_state)" != available ]; then
    echo "==> docker sentinel: not started, the daemon is not reachable"
    return 0
  fi

  gate_sweep_docker_sentinels

  gate_sentinel_candidate="ci-local-sentinel-$$"
  if docker run -d --rm --name "$gate_sentinel_candidate" \
      --label "$GATE_SENTINEL_LABEL" "$GATE_SENTINEL_IMAGE" \
      sleep infinity >/dev/null 2>&1; then
    gate_sentinel_name="$gate_sentinel_candidate"
    echo "==> docker sentinel: $gate_sentinel_name up, so the daemon is never idle for this run (#457)"
    return 0
  fi

  echo "==> docker sentinel: $GATE_SENTINEL_IMAGE is not here, pulling it once"
  if docker pull "$GATE_SENTINEL_IMAGE" >/dev/null 2>&1 &&
     docker run -d --rm --name "$gate_sentinel_candidate" \
       --label "$GATE_SENTINEL_LABEL" "$GATE_SENTINEL_IMAGE" \
       sleep infinity >/dev/null 2>&1; then
    gate_sentinel_name="$gate_sentinel_candidate"
    echo "==> docker sentinel: $gate_sentinel_name up, so the daemon is never idle for this run (#457)"
    return 0
  fi

  echo "==> docker sentinel: could not start one. The run continues without it, and every" >&2
  echo "    Docker-dependent step still probes the daemon before it runs." >&2
  return 0
}

# gate_stop_docker_sentinel
# Idempotent, silent about failure, and called from every exit path there is.
# A sentinel that outlives the gate is its own bug: it is the leak #150 was
# about, wearing a different label.
gate_stop_docker_sentinel() {
  [ -n "$gate_sentinel_name" ] || return 0
  gate_sentinel_going="$gate_sentinel_name"
  gate_sentinel_name=""
  docker rm -f "$gate_sentinel_going" >/dev/null 2>&1 || true
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

# gate_on_exit <status>
# The one EXIT handler, and the reason it exists rather than a second
# `trap ... EXIT` line: a trap is set, not appended. A second EXIT trap
# anywhere in ci-local.sh would silently REPLACE `gate_exit_marker`, and the
# thing it replaced is the only marker a failed run prints. So everything
# that has to happen on the way out composes here, in order: drop the
# sentinel container first (it is a real resource, and the marker is only
# text), then print the verdict.
gate_on_exit() {
  gate_stop_docker_sentinel
  gate_exit_marker "$1"
}

# gate_on_signal <signal name> <exit status>
# Ctrl-C is not a rare path on a gate that runs for twenty-five minutes from
# a pre-commit hook, and it is the path that used to leak containers (#150).
# The sentinel must not survive it, so INT/TERM/HUP get a handler that cleans
# up and then exits, which runs gate_on_exit too. The verdict line still says
# FAILED, with the step and the signal in the parentheses, because an
# interrupted run is not a run that performed anything and a fourth verdict
# word nobody greps for would be worse than the one they already do.
gate_on_signal() {
  gate_marked=1
  gate_stop_docker_sentinel
  echo "" >&2
  echo "==> ci-local: FAILED ($gate_last_step, interrupted by SIG$1)" >&2
  exit "$2"
}

# gate_install_traps
# Every trap ci-local.sh needs, installed from one place so that adding a
# handler later means editing this function rather than adding a `trap` line
# that quietly unsets the one before it.
gate_install_traps() {
  trap 'gate_on_exit $?' EXIT
  trap 'gate_on_signal INT 130' INT
  trap 'gate_on_signal TERM 143' TERM
  trap 'gate_on_signal HUP 129' HUP
}

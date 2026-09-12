#!/usr/bin/env bash
# HELP-START
# Three machines on private networks, a real deployment, and a browser that
# has to talk to it over the wire.
#
# # The blind spot this closes
#
# suites/web-ui over in backupdproject/backupd-tests starts `npm run dev`
# and the app it drives resolves createMockApi out of
# ui/shared/src/api/mock.ts. So every case in that suite is a claim about a
# component rendering correctly GIVEN a fixture, and not one of them can go
# red on anything in the path a user actually meets:
#
#     browser -> serve-ui -> reverse proxy -> serve -> SQLite
#
# That is not a theoretical gap. The Activity page errored in the browser on
# a real NAS while the server answered HTTP 200 with valid JSON in twenty
# milliseconds, and it went unnoticed, because nothing that watches the
# server can see it and nothing that mocks the client can either: a mock
# answers with the shape the client already expects. This script stands up
# the whole path so a browser can fail on it.
#
# # The three machines
#
#   CLIENT    the browser and the Playwright runner, both inside the
#             container (scripts/e2e/client-machine.Dockerfile). It is on
#             the edge network and nothing else, so the only thing it can
#             reach is the product's front door.
#
#   RCLONE-MANAGER  the real product image, built from THIS working tree,
#             running the deployment container/compose.yaml describes.
#
#   VPS       a real sshd holding real files (atmoz/sftp, through
#             scripts/e2e/source-machine.Dockerfile, the same definition
#             two-machine-backup.sh and core/tests/machines use), playing
#             the machine being backed up.
#
# Three machines, four containers, and the difference is worth being plain
# about. The product IS two containers: `/backupd-web serve` (the engine: local
# authentication, /api/v1, the scheduler, SQLite) and `/backupd-web serve-ui`
# (the static bundle plus a reverse proxy to the engine). Collapsing them
# into one would delete the reverse-proxy hop, and that hop is half of what
# this suite exists to cover. So "the backupd machine" here is the
# product's own split, unchanged.
#
# # Why this drops docker-in-docker, and what that gives up
#
# scripts/e2e/two-machine-backup.sh runs its manager machine as
# docker-in-docker and installs onto it with the real installer. That is
# the right shape for what it proves, and its own comment says why this one
# cannot borrow it: the dind daemon is a network namespace of its own, so
# 8080 in there is not 8080 out here, and a sibling client container has no
# route to it at all. The two ways out were to publish the inner port back
# out of the dind container onto the shared network, or to drop dind and
# run the product image directly on the network. This takes the second.
#
# Publishing back out would work, and what it would produce is a browser
# talking to a port forwarded by a daemon running inside a daemon, which is
# a hop that exists nowhere in production. Every timeout and every reset on
# it would be a question about the test rig. The thing under test here is
# an HTTP path, so the fewer inventions between the browser and the server
# the better.
#
# What that gives up is stated rather than quietly dropped: THIS SCRIPT
# DOES NOT TEST THE INSTALLER. It tests the running product.
# two-machine-backup.sh remains the test that scripts/install/
# install_docker_host.py takes a bare machine to a working deployment, and
# it must stay that way. A green run here says nothing about whether an
# operator can install this.
#
# # Three networks, not one
#
#     client ──edge── web-ui ──internal── engine ──backhaul── vps
#
# One flat network would have been fewer lines. It would also let a spec
# reach the engine directly and pass while proving nothing about the proxy,
# and it would put the client on the same wire as an engine that runs with
# TRUST_FORWARDED_HEADERS=true, which container/compose.yaml goes to some
# length to explain is only safe because serve-ui is its sole possible
# peer. Both of those are the topology doing work, so the topology is the
# real one. Nothing is published to the host on any of them.
#
# The name `backupd` lives on the edge network only, and it resolves
# to the UI container: from outside the deployment that IS backupd,
# and it is the address the browser is given. Inside the deployment the
# containers keep compose's own names. One name per network, so nothing
# ever resolves to two things.
#
# # Seeded state, generated per run, committed never
#
# The VPS holds three files of known content. The deployment is configured
# with a backup set pointing at it over SSH, a cycle is run so the journal
# and the catalogue have real rows in them, and an administrator is
# enrolled. The SSH keypair is generated INSIDE a container and lives in a
# Docker volume that is destroyed at teardown, so the private half never
# touches this host's filesystem. The administrator's password is generated
# per run, reaches the product down a pipe into stdin, and is printed here
# because a suite that has to sign in needs it. Both are dead the moment
# this script returns.
#
# # Handing it to a suite
#
# The client container gets these, and they are the whole contract:
#
#   RM_BASE_URL          http://backupd:8080
#   RM_ADMIN_USERNAME    the enrolled administrator
#   RM_ADMIN_PASSWORD    its password, generated this run
#   RM_BACKUP_SET        the seeded set's name
#   RM_ARTIFACTS_DIR     /artifacts, mounted out to the host
#
# A Playwright config that sees RM_BASE_URL must use it as `baseURL` and
# must NOT start a web server: there is one, it is another container, and
# `npm run dev` in here would serve the mock this whole script exists to
# get away from.
#
# # Running it
#
#   scripts/e2e/three-machine-web-ui.sh
#       stand the stack up and run the built-in browser check.
#
#   scripts/e2e/three-machine-web-ui.sh --suite ../backupd-tests/suites/web-ui
#       stand it up and run that directory's Playwright suite inside the
#       client container. The suite's own node_modules is not used: the
#       image's is, because the checkout's was built for this host.
#
#   scripts/e2e/three-machine-web-ui.sh --keep-up
#       stand it up, print how to drive it, and leave it running. For
#       working on the suite without paying the setup cost per attempt.
#       Tear it down afterwards with the line it prints.
#
#   --artifacts DIR   where traces, screenshots and reports land. Outside
#                     this repository by default, and never removed by the
#                     teardown, because a failing run whose evidence was
#                     deleted with the stack is worse than no evidence.
#   --image REF       skip the build and use an already-built product image.
#   --keep-on-failure leave a failed stack up for reading.
#   --front-proxy-tls put an ordinary TLS + HTTP/2 reverse proxy in front of
#                     serve-ui, so the browser reaches the stack the way a
#                     real NAS's front door does (h2 over TLS) rather than
#                     the plain HTTP/1.1 this rig otherwise uses. The
#                     reproduction for backupd#730. RM_SEED_CYCLES=N
#                     additionally runs N backup cycles to enlarge the feed.
#   --break-engine    hand the suite the ability to take the ENGINE away
#                     mid-session, leaving serve-ui up, so the browser
#                     meets a front door that cannot reach the service
#                     behind it. The reproduction for backupd#795.
#
#                     The engine is NOT stopped up front, and that is the
#                     whole design. serve-ui proxies all of /api/v1,
#                     /auth/session included, so a stack that starts
#                     broken never gets a browser past the login page and
#                     the Activity page is never reached. The reported NAS
#                     failed the other way round: a loaded, signed-in app
#                     whose engine went away underneath it. So the break
#                     happens while the suite holds a live session.
#
#                     The client container gets two more variables:
#
#                       RM_ENGINE_UNREACHABLE=1   branch on this
#                       RM_ENGINE_CONTROL         a directory under
#                                                 /artifacts, described
#                                                 below
#
#                     and a watcher on THIS host owns the docker socket
#                     the client deliberately does not have. The protocol
#                     is four empty files in that directory:
#
#                       write "stop"    -> the engine container is stopped
#                                          and "stopped" appears
#                       write "start"   -> it is started, and "started"
#                                          appears only once its own
#                                          healthcheck passes
#
#                     A request file is removed as it is picked up, so one
#                     request is never acknowledged by the leavings of the
#                     last, and "start" against an engine that is already
#                     running is a no-op that still acknowledges. A suite
#                     that deletes an ack before asking again is doing the
#                     right thing and is expected to.
#
#                     Before handing over, the break is REHEARSED: the
#                     engine is stopped, the edge network is asked for
#                     /api/v1/activity and has to come back 502 with an
#                     X-Correlation-Id on it, and the engine is started
#                     again. A mode that cannot demonstrate the fault it
#                     exists to produce fails here rather than handing a
#                     suite a healthy stack to pass against.
#
# The exit status is the client container's, not the teardown's. A run that
# tore down cleanly after a red suite is a red run.
#
# # Cost
#
# About forty seconds of setup on the machine this was written on once the
# product image is built, and the product image build is minutes on a cold
# cache because it compiles the Go binaries and the UI bundle. --image and
# --keep-up both exist to stop paying that per attempt.
# HELP-END

set -euo pipefail

# --help reads this file and the run cd's to the repository root, so both
# paths are settled here, from the same dirname, before that happens. Same
# reasoning as two-machine-backup.sh, and the same shape.
self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

# ---------------------------------------------------------------- output

step() { echo ""; echo "==> three-machine: $*"; }
note() { echo "    $*"; }

die() {
  echo "" >&2
  echo "==> three-machine: FAILED. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit 1
}

# A capability this machine does not have is not a failure and is not a
# pass either. It leaves as 3, which is what scripts/lib/ci-local-gate.sh
# reads as INCOMPLETE.
#
# It travels to `finish` as its own number rather than as 3, for the reason
# two-machine-backup.sh spells out at length: the CLI under test has its own
# meaning for exit 3 (#551, another process is already serving this
# deployment), several commands below are that CLI, and a run that met that
# refusal must not be reported as a machine that never tried.
EXIT_CANNOT_RUN=97
cannot_run() {
  echo "" >&2
  echo "==> three-machine: CANNOT RUN. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit "$EXIT_CANNOT_RUN"
}

# ------------------------------------------------------------------ help
#
# The block between the two markers at the top of this file, never a range
# of line numbers (#514): markers move with the text they delimit, and a
# comment inserted above one leaves the rendered help unchanged.
render_help() {
  awk '
    /^# HELP-END$/   { closed = 1; inside = 0; next }
    /^# HELP-START$/ { opened = 1; inside = 1; next }
    inside           { sub(/^# ?/, ""); print }
    END              { if (!opened || !closed) exit 1 }
  ' "$self" || die "the help block is missing from $self" \
    "--help renders the lines between the HELP-START and HELP-END markers," \
    "and this file has lost one or both of them."
}

# --------------------------------------------------------------- options

suite_dir=""
artifacts_dir=""
keep_on_failure=0
keep_up=0
prebuilt_image="${RM_PRODUCT_IMAGE:-}"
# backupd#730 reproduction: put an ordinary TLS + HTTP/2 reverse
# proxy in front of serve-ui, so the browser reaches the stack the way it
# reaches a real NAS (h2 over TLS) rather than the plain HTTP/1.1 the rig
# otherwise uses. Off by default; the default rig is unchanged.
front_proxy="${RM_FRONT_PROXY_TLS:-0}"
# backupd#795 reproduction: let the suite take the engine away while the
# browser holds a live session, with serve-ui left up in front of it.
# Off by default; every line it adds is behind this flag, so a default run
# is the run it was before.
break_engine="${RM_BREAK_ENGINE:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --suite) suite_dir="${2:-}"; shift 2 ;;
    --suite=*) suite_dir="${1#--suite=}"; shift ;;
    --artifacts) artifacts_dir="${2:-}"; shift 2 ;;
    --artifacts=*) artifacts_dir="${1#--artifacts=}"; shift ;;
    --image) prebuilt_image="${2:-}"; shift 2 ;;
    --image=*) prebuilt_image="${1#--image=}"; shift ;;
    --keep-on-failure) keep_on_failure=1; shift ;;
    --keep-up) keep_up=1; shift ;;
    --front-proxy-tls) front_proxy=1; shift ;;
    --break-engine) break_engine=1; shift ;;
    -h|--help) render_help; exit 0 ;;
    *) die "unknown option $1" \
           "Usage: $0 [--suite DIR] [--artifacts DIR] [--image REF] [--front-proxy-tls] [--break-engine] [--keep-up] [--keep-on-failure]" ;;
  esac
done

if [ -n "$suite_dir" ]; then
  [ -d "$suite_dir" ] || die "--suite $suite_dir is not a directory."
  suite_dir="$(cd "$suite_dir" && pwd)"
  [ -f "$suite_dir/playwright.config.ts" ] || [ -f "$suite_dir/playwright.config.js" ] \
    || die "--suite $suite_dir has no playwright.config.ts in it." \
           "That is the directory the client container runs \`npx playwright test\` in, so it has to be the suite root."
fi

# ------------------------------------------------------------ identities

run_id="${E2E_RUN_ID:-$$-$(date +%s)-${RANDOM}}"
label="backupd-e2e=three-machine-web-ui"

net_edge="rm-webui-edge-$run_id"
net_internal="rm-webui-internal-$run_id"
net_backhaul="rm-webui-backhaul-$run_id"

c_source="rm-webui-vps-$run_id"
c_engine="rm-webui-engine-$run_id"
c_web="rm-webui-web-$run_id"
c_client="rm-webui-client-$run_id"
c_proxy="rm-webui-proxy-$run_id"

# The deployment's state, in Docker volumes rather than host directories,
# and the reason is the SSH key. core/internal/transport/rclone/ssh.go
# refuses a key_file whose mode is not exactly 0600 and refuses any
# ancestor directory that is group- or world-writable, which is the right
# rule and is also the rule a bind mount cannot reliably satisfy: file
# ownership across a Docker Desktop share is not the host's, and a mode
# that survives on Linux does not survive there. A volume is the same
# filesystem the container writes with, so 0600 means 0600. It also means
# the private key never lands on this host at all.
v_keys="rm-webui-keys-$run_id"
v_config="rm-webui-config-$run_id"
v_state="rm-webui-state-$run_id"
v_backups="rm-webui-backups-$run_id"

# The payload is the exception, and deliberately: the harness generates it
# and has to be able to digest it from outside, and it holds nothing
# secret. It lives in a run directory OUTSIDE this repository, so nothing
# this script writes can be committed by accident.
tmp_root="${TMPDIR:-/tmp}"
tmp_root="${tmp_root%/}/backupd-e2e-web-ui"
run_dir="$tmp_root/$run_id"

# Artifacts outlive the stack on purpose, so this is not $run_dir. Outside
# the repository by default because a Playwright trace of a sign-in carries
# the password that was typed into it, and this repository is not where
# credential material goes, dead or not.
[ -n "$artifacts_dir" ] || artifacts_dir="$tmp_root/$run_id-artifacts"

# backupd#795's control channel, and it lives UNDER the artifacts
# directory rather than beside it for one reason: that directory is
# already bind-mounted into the client container, and the client must not
# be given anything else. A suite that could reach the Docker socket could
# stop the engine itself, and would then be a suite that can do anything
# to this host; four empty files in a directory it already has is the
# whole capability it needs.
engine_control="$artifacts_dir/engine-control"
engine_control_in_client="/artifacts/engine-control"
engine_watcher_pid=""

product_image="${prebuilt_image:-backupd-web-ui-e2e:$run_id}"
source_image="backupd-e2e-source:1"
client_image="backupd-e2e-client:1"
proxy_image="backupd-e2e-proxy:1"

source_dockerfile="$repo_root/scripts/e2e/source-machine.Dockerfile"
client_dockerfile="$repo_root/scripts/e2e/client-machine.Dockerfile"
proxy_dockerfile="$repo_root/scripts/e2e/proxy-machine.Dockerfile"
[ -r "$source_dockerfile" ] || die "the VPS machine Dockerfile is missing at $source_dockerfile."
[ -r "$client_dockerfile" ] || die "the client machine Dockerfile is missing at $client_dockerfile."
[ "$front_proxy" != 1 ] || [ -r "$proxy_dockerfile" ] \
  || die "the front-proxy machine Dockerfile is missing at $proxy_dockerfile."

sftp_user="backupuser"
sftp_uid=1001

# The uid/gid the product runs as, matching container/compose.yaml's own
# PUID/PGID defaults. The volumes above are chowned to it before anything
# starts, because a distroless image has no shell and no root step to fix
# an ownership problem from the inside.
app_uid=1000
app_gid=1000

backup_set="e2e/vps"

# What the client is told. `backupd` is an alias on the edge network
# and on no other, so it resolves to exactly one container from exactly one
# place, seen from the client: the UI container normally, or the TLS/HTTP-2
# front proxy when --front-proxy-tls is set (which then upstreams to the UI
# container, aliased `origin` on the same edge network).
# When the front proxy is in play the browser and any node fetch reach the
# stack over TLS with a self-signed leaf, so the probes and the client are
# told to accept it: this rig is exercising the h2/transport path, not
# certificate trust. front_proxy_probe_env is used UNQUOTED on purpose so
# the empty default expands to no argument at all.
if [ "$front_proxy" = 1 ]; then
  base_url="https://backupd"
  edge_web_alias="origin"
  front_proxy_probe_env="-e NODE_NO_WARNINGS=1 -e NODE_TLS_REJECT_UNAUTHORIZED=0"
else
  base_url="http://backupd:8080"
  edge_web_alias="backupd"
  front_proxy_probe_env=""
fi

# Generated here, printed below, never written to a file by this script. It
# reaches `auth create-admin` down a pipe into stdin and reaches the client
# container as an environment variable, which is what the product's own
# documentation prescribes for both. Twelve characters is the product's own
# minimum (the enrolment form enforces it), so this comfortably clears it.
admin_user="e2e-operator"
admin_pass="e2e-$(openssl rand -hex 16)"

# ------------------------------------------------------------- teardown

created_containers=()
created_networks=()
created_volumes=()
created_images=()
teardown_done=0

teardown() {
  local status="${1:-0}"
  [ "$teardown_done" = 1 ] && return
  teardown_done=1

  # The watcher first, and before the --keep-up return below rather than
  # after it: it is a process on THIS host holding the Docker socket, and
  # a run that leaves the stack up on purpose still must not leave a loop
  # behind that stops a container somebody is reading. Killed even when
  # the containers are kept.
  stop_engine_watcher

  if [ "$keep_up" = 1 ] || { [ "$keep_on_failure" = 1 ] && [ "$status" != 0 ]; }; then
    echo "" >&2
    if [ "$keep_up" = 1 ]; then
      echo "==> three-machine: --keep-up, so the stack is still running:" >&2
    else
      echo "==> three-machine: --keep-on-failure, so the failed stack is left up for reading:" >&2
    fi
    for c in "${created_containers[@]:-}"; do [ -n "$c" ] && echo "        container $c" >&2; done
    for n in "${created_networks[@]:-}"; do [ -n "$n" ] && echo "        network   $n" >&2; done
    for v in "${created_volumes[@]:-}"; do [ -n "$v" ] && echo "        volume    $v" >&2; done
    echo "" >&2
    echo "    Tear it down with:" >&2
    echo "        docker rm -fv ${created_containers[*]:-} && docker network rm ${created_networks[*]:-} && docker volume rm ${created_volumes[*]:-}" >&2
    return
  fi

  echo ""
  echo "==> three-machine: tearing down"
  for c in "${created_containers[@]:-}"; do
    [ -n "$c" ] && docker rm -fv "$c" >/dev/null 2>&1 || true
  done
  # After the containers, never before: a network with an endpoint on it
  # cannot be removed, and "network is in use" at teardown time is how a
  # network survives a run.
  for n in "${created_networks[@]:-}"; do
    [ -n "$n" ] && docker network rm "$n" >/dev/null 2>&1 || true
  done
  # And the volumes after the networks, for the same reason one step
  # further out: a volume with a container still attached is not removable
  # either, and these hold the run's SSH private key.
  for v in "${created_volumes[@]:-}"; do
    [ -n "$v" ] && docker volume rm "$v" >/dev/null 2>&1 || true
  done
  for i in "${created_images[@]:-}"; do
    [ -n "$i" ] && docker image rm -f "$i" >/dev/null 2>&1 || true
  done
  remove_run_dir
  # $artifacts_dir is NOT removed here, and that is the point of it.
}

# remove_run_dir deletes what this run wrote, by name, and then removes the
# directories. Deliberately not a recursive delete: the files are a known,
# short list, and `rm -rf` on a path built from variables is how a script
# eventually deletes the wrong thing. An unexpected extra file leaves the
# directory behind, which is visible rather than silent.
remove_run_dir() {
  [ -d "$run_dir" ] || return 0
  rm -f "$run_dir/upload/payload.bin" "$run_dir/upload/schema.sql" "$run_dir/upload/notes.txt" 2>/dev/null || true
  rm -f "$run_dir/authorized_keys/engine.pub" 2>/dev/null || true
  rmdir "$run_dir/upload" "$run_dir/authorized_keys" "$run_dir" 2>/dev/null || true
  rmdir "$tmp_root" 2>/dev/null || true
}

# Three traps, not one. A bash trap on INT or TERM runs the handler and then
# RESUMES the script, so `trap teardown EXIT INT TERM` would tear everything
# down and then carry on against containers that are no longer there. These
# exit, which fires the EXIT trap too; teardown_done makes the second call a
# no-op. The status is passed in rather than read from $?, because inside a
# signal handler $? is the status of whatever the signal interrupted.
finish() {
  local status="${1:-0}"
  teardown "$status"
  case "$status" in
    "$EXIT_CANNOT_RUN") exit 3 ;;
    3)
      echo "" >&2
      echo "==> three-machine: FAILED. A command exited 3." >&2
      echo "    This script reserves 3 for the gate's \"this machine could not perform the proof\" verdict and never" >&2
      echo "    produces it itself, so a 3 here came from something with its own meaning for that status: most likely" >&2
      echo "    the CLI refusing a configuration write because the engine is already serving this deployment (#551)." >&2
      echo "    Reported as a failure, which is what a proof that did not finish is." >&2
      exit 1 ;;
    *) exit "$status" ;;
  esac
}
trap 'finish $?' EXIT
trap 'teardown 130; exit 130' INT
trap 'teardown 143; exit 143' TERM

# ------------------------------------------------------------- utilities

# wait_or_die <seconds> <what it is waiting for> <command...>
# Every wait in this script goes through here, so none of them can be the
# one that hangs a cold machine forever, and every timeout says what it was
# waiting for rather than only that it waited.
wait_or_die() {
  local budget="$1" what="$2"
  shift 2
  local deadline=$(( $(date +%s) + budget ))
  until "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      die "timed out after ${budget}s waiting for $what."
    fi
    sleep 1
  done
}

# ------------------------------------------- the engine control channel
#
# backupd#795. Only ever used with --break-engine, and every line of it is
# inert without that flag.
#
# engine_is_live is the same healthcheck the startup wait uses, asked of
# the engine's own listener from inside its container: "docker start
# returned" is not the same fact as "the engine is serving again", and a
# suite told the second when only the first is true fails on a race it
# cannot see.
engine_is_live() {
  docker exec "$c_engine" /backupd-web healthcheck --url http://127.0.0.1:8080/health/live >/dev/null 2>&1
}

# engine_watcher_loop is what the client container cannot do for itself.
# It has no Docker socket, deliberately: the thing under test is a browser
# on an edge network, and a browser that can stop containers is not one.
# So the capability is held here and exposed as four empty files.
#
# Ordering inside each branch is the contract, not tidiness. The request
# file is removed BEFORE the work, so a second request can never be
# acknowledged by the first one's leftovers, and the ack is written AFTER
# it, so a suite that sees "started" can rely on the engine answering.
# `docker start` against a running container is a no-op that exits 0,
# which makes a redundant heal (a test that heals mid-run and again in a
# finally) acknowledge rather than fail.
engine_watcher_loop() {
  while :; do
    if [ -e "$engine_control/stop" ]; then
      rm -f "$engine_control/stop"
      docker stop "$c_engine" >/dev/null 2>&1 || true
      rm -f "$engine_control/started"
      : > "$engine_control/stopped"
    elif [ -e "$engine_control/start" ]; then
      rm -f "$engine_control/start"
      docker start "$c_engine" >/dev/null 2>&1 || true
      # Bounded, and it gives up rather than hanging: a watcher that
      # never acknowledges is read by the suite as its own 180s timeout,
      # which is a worse message than the ack arriving against an engine
      # that is still coming up. The engine's own startup wait above
      # allows 180s from cold; this allows 120s from an image and a
      # database that are already warm.
      local waited=0
      while [ "$waited" -lt 120 ] && ! engine_is_live; do
        sleep 1
        waited=$(( waited + 1 ))
      done
      rm -f "$engine_control/stopped"
      : > "$engine_control/started"
    fi
    sleep 0.25
  done
}

start_engine_watcher() {
  mkdir -p "$engine_control"
  # The state the stack is actually in when the suite is handed it. The
  # suite is not expected to read it before asking for anything - it
  # removes the ack it is about to wait for first - but a directory whose
  # contents describe the world is easier to debug than an empty one.
  rm -f "$engine_control/stop" "$engine_control/start" "$engine_control/stopped"
  : > "$engine_control/started"
  engine_watcher_loop &
  engine_watcher_pid=$!
}

stop_engine_watcher() {
  [ -n "$engine_watcher_pid" ] || return 0
  kill "$engine_watcher_pid" >/dev/null 2>&1 || true
  wait "$engine_watcher_pid" 2>/dev/null || true
  engine_watcher_pid=""
}

# The VPS container, which is alpine and has coreutils. The product's
# containers do not, which is what the three functions below are for.
sha256_of() {  # sha256_of <container> <path>
  docker exec "$1" sha256sum "$2" | awk '{print $1}'
}

# toolbox runs a shell command in a throwaway container that has a shell,
# which is the VPS image this run already built. Everything the harness
# needs to ask about the DEPLOYMENT's own volumes goes through it, because
# the product's runtime image is distroless: no shell, no coreutils, nothing
# to ask with. `docker exec <engine> sha256sum` does not answer wrongly, it
# fails with "executable file not found", which is a confusing way to learn
# whether a backup landed.
#
# --entrypoint sh, and that part is load-bearing rather than tidy.
# atmoz/sftp's entrypoint generates a fresh pair of host keys and narrates
# it, randomart and all, on STDOUT before it runs the command it was given.
# Harmless when the container IS the VPS, and fatal when the container is
# being asked a question: a command substitution around it comes back with
# forty lines of ASCII art and the answer on the end.
toolbox() {  # toolbox <extra docker run args...> -- <shell command>
  local args=()
  while [ "${1:-}" != "--" ]; do args+=("$1"); shift; done
  shift
  docker run --rm --label "$label" --entrypoint sh "${args[@]}" "$source_image" -c "$1"
}

sha256_in_volume() {  # sha256_in_volume <volume> <path within it>
  toolbox -v "$1:/v:ro" -- "sha256sum '/v/$2' 2>/dev/null | awk '{print \$1}'"
}

exists_in_volume() {  # exists_in_volume <volume> <path within it>
  toolbox -v "$1:/v:ro" -- "test -f '/v/$2'"
}

list_volume() {  # list_volume <volume> <directory within it>
  toolbox -v "$1:/v:ro" -- "ls -1 '/v/$2' 2>/dev/null" | tr '\n' ' '
}

# A one-shot product container on the deployment's own volumes, for the
# three commands that run while nothing is serving: creating the backup set,
# enrolling the administrator, and the seeding cycle. The first two are
# REFUSED beside a running engine (#571, and the auth store's own
# process-lifetime flock) and the third loses a SQLite lock race with it,
# which is measured rather than assumed and is written up where it happens.
# All three are things an operator does before first start, so this is the
# honest order rather than a workaround.
oneshot() {  # oneshot <network, or "none"> <command...>
  local net="$1"; shift
  docker run --rm -i \
    --network "$net" \
    --label "$label" \
    --user "$app_uid:$app_gid" \
    -v "$v_config:/etc/backupd/config" \
    -v "$v_state:/data/state" \
    -v "$v_backups:/data/backups" \
    -v "$v_keys:/etc/backupd/keys:ro" \
    -e TMPDIR=/tmp \
    "$product_image" "$@"
}

# ------------------------------------------------------------- preflight

step "preflight"
for tool in docker openssl; do
  command -v "$tool" >/dev/null 2>&1 \
    || cannot_run "$tool is not on PATH, and this stack needs it."
done
docker info >/dev/null 2>&1 \
  || cannot_run "the Docker daemon is not reachable." \
                "Start Docker and re-run. Reporting a pass for a run that never happened is the one thing this must not do."
note "docker daemon reachable"

if [ -n "$prebuilt_image" ]; then
  docker image inspect "$prebuilt_image" >/dev/null 2>&1 \
    || die "--image $prebuilt_image is not on this machine, and this script will not pull one." \
           "A published tag would test somebody else's build, which is #342. Build it, or drop --image."
fi

mkdir -p "$run_dir/upload" "$run_dir/authorized_keys" "$artifacts_dir"
# The client container runs as this host's own uid so the traces and
# screenshots it writes are owned by the person who has to read them, so
# this directory only has to be writable by that same uid.
chmod 700 "$run_dir" "$artifacts_dir"

# ---------------------------------------------------------- the images

if [ -z "$prebuilt_image" ]; then
  step "building the product image from this working tree"
  version="$(git rev-parse --short HEAD)"
  commit="$(git rev-parse HEAD)"
  if ! git diff --quiet HEAD 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
    commit="$commit-dirty"
  fi
  note "VERSION=$version COMMIT=$commit"
  docker build \
    -f container/Dockerfile \
    --build-arg "VERSION=$version" \
    --build-arg "COMMIT=$commit" \
    -t "$product_image" \
    . \
    || die "could not build the product image from this working tree." \
           "Everything below tests that image, so there is nothing to fall back to."
  created_images+=("$product_image")
else
  note "using the product image already on this machine: $product_image"
fi

step "building the VPS and client machine images"
docker build -q -t "$source_image" -f "$source_dockerfile" "$(dirname "$source_dockerfile")" >/dev/null \
  || die "could not build the VPS machine image from $source_dockerfile."
# The client image's context is scripts/e2e, because web-ui-smoke.mjs is
# copied into it and the browser has to be able to find @playwright/test
# next to that file.
docker build -q -t "$client_image" -f "$client_dockerfile" "$(dirname "$client_dockerfile")" >/dev/null \
  || die "could not build the client machine image from $client_dockerfile." \
         "It is the pinned Playwright image plus the runner: if the pull failed, this machine has no route to mcr.microsoft.com."
note "VPS machine:    $source_image"
note "client machine: $client_image"
if [ "$front_proxy" = 1 ]; then
  docker build -q -t "$proxy_image" -f "$proxy_dockerfile" "$(dirname "$proxy_dockerfile")" >/dev/null \
    || die "could not build the front-proxy machine image from $proxy_dockerfile."
  created_images+=("$proxy_image")
  note "front proxy:    $proxy_image (TLS + HTTP/2, #730 reproduction)"
fi

# ------------------------------------------------------------- payload

step "seeding the VPS's files"
# Deterministic bytes rather than /dev/urandom, so a digest mismatch can be
# reasoned about rather than only observed.
head -c 3145728 /dev/zero \
  | openssl enc -aes-256-ctr -pbkdf2 -pass pass:backupd-e2e-web-ui -nosalt 2>/dev/null \
  > "$run_dir/upload/payload.bin" \
  || die "could not generate the payload."
printf 'CREATE TABLE artifacts (id text primary key);\n' > "$run_dir/upload/schema.sql"
printf 'the browser suite runs against this machine, not against a mock.\n' > "$run_dir/upload/notes.txt"
# The sshd container runs as its own uid and has to read these, and this
# directory is a per-run path under the system temp directory that teardown
# removes by name.
chmod 644 "$run_dir/upload"/*
chmod 755 "$run_dir/upload"
note "3 files: payload.bin (3 MiB), schema.sql, notes.txt"

# ------------------------------------------------------------- volumes

step "creating the deployment's volumes and its SSH key"
for v in "$v_keys" "$v_config" "$v_state" "$v_backups"; do
  docker volume create --label "$label" "$v" >/dev/null \
    || die "could not create the volume $v."
  created_volumes+=("$v")
done

# The keypair is generated INSIDE a container and stays in a volume this
# script destroys, so the private half never touches this host. The public
# half comes back out through stdout, because that is not a secret and the
# VPS has to be told to accept it.
#
# 0600 on the key and 0700 on the directory holding it are not hygiene
# theatre: core/internal/transport/rclone/ssh.go refuses anything else, by
# design, and refuses a group- or world-writable ancestor as well.
toolbox -v "$v_keys:/keys" -- "
    set -e
    ssh-keygen -q -t ed25519 -N '' -C 'backupd e2e' -f /keys/id_ed25519 </dev/null
    chown -R $app_uid:$app_gid /keys
    chmod 700 /keys
    chmod 600 /keys/id_ed25519
    chmod 644 /keys/id_ed25519.pub
  " >/dev/null \
  || die "could not generate the deployment's SSH keypair inside the key volume."

engine_pubkey="$(toolbox -v "$v_keys:/keys:ro" -- "cat /keys/id_ed25519.pub")"
[ -n "$engine_pubkey" ] || die "the generated keypair has no public half."
printf '%s\n' "$engine_pubkey" > "$run_dir/authorized_keys/engine.pub"
chmod 644 "$run_dir/authorized_keys/engine.pub"
note "keypair generated in the volume $v_keys, public half ${engine_pubkey:0:38}..."

# The config, state and backup volumes start out owned by root, and the
# product runs as $app_uid with no shell and no root step to fix that from
# the inside. Chowned here, once, with the machine image that is already
# local rather than a pull.
toolbox -v "$v_config:/c" -v "$v_state:/s" -v "$v_backups:/b" \
  -- "chown $app_uid:$app_gid /c /s /b && chmod 755 /c /s /b" >/dev/null \
  || die "could not give the deployment's volumes to $app_uid:$app_gid."
note "config, state and backup volumes belong to $app_uid:$app_gid"

# ------------------------------------------------------------ networks

step "creating the three private networks"
for n in "$net_edge" "$net_internal" "$net_backhaul"; do
  docker network create --label "$label" "$n" >/dev/null \
    || die "could not create the network $n."
  created_networks+=("$n")
done
note "edge     $net_edge      (client <-> the product's front door)"
note "internal $net_internal  (serve-ui <-> the engine)"
note "backhaul $net_backhaul  (the engine <-> the VPS)"

# ---------------------------------------------------------- the VPS

step "starting the VPS"
# No host keys are mounted in: atmoz/sftp generates its own on first start,
# and `backup-set create --trust-host-key` below pins whatever it presents.
# two-machine-backup.sh mounts a fixed pair because core/tests/sftpfixture
# wants a known_hosts line settled in advance; nothing here does, and a
# keypair not generated is a keypair not left on this host.
docker run -d \
  --name "$c_source" \
  --network "$net_backhaul" \
  --network-alias vps \
  --label "$label" \
  -v "$run_dir/authorized_keys:/home/$sftp_user/.ssh/keys:ro" \
  -v "$run_dir/upload:/home/$sftp_user/upload" \
  "$source_image" "$sftp_user::$sftp_uid:$sftp_uid:upload" >/dev/null \
  || die "could not start the VPS."
created_containers+=("$c_source")

wait_or_die 120 "the VPS's sshd to start listening" \
  docker exec "$c_source" sh -c 'nc -w 2 127.0.0.1 22 </dev/null 2>/dev/null | grep -q ^SSH-'
note "$c_source is serving SSH on the backhaul network as \"vps\""

# The digests every assertion below is against, read from the VPS itself
# rather than from the copy this script wrote. What has to match is the
# bytes the machine being backed up is actually serving.
want_payload="$(sha256_of "$c_source" "/home/$sftp_user/upload/payload.bin")"
want_schema="$(sha256_of "$c_source" "/home/$sftp_user/upload/schema.sql")"
want_notes="$(sha256_of "$c_source" "/home/$sftp_user/upload/notes.txt")"
note "payload.bin on the VPS is sha256 $want_payload"

# ------------------------------------------- configure the deployment

step "creating the backup set, before anything is serving"
# --trust-host-key probes the VPS now and trusts what answers, which is why
# the VPS is up first. The alternative, --known-hosts-line, would need a
# key this script had settled in advance, and it settles none.
oneshot "$net_backhaul" \
  /backupd backup-set create "$backup_set" \
    --config /etc/backupd/config \
    --host vps \
    --user "$sftp_user" \
    --ssh-key-file /etc/backupd/keys/id_ed25519 \
    --trust-host-key \
    --remote-path /upload \
    --local-path /data/backups/vps \
    --completion-strategy rename \
    --read-only \
    --state-database /data/state/state.db \
  || die "creating the backup set failed." \
         "This ran with nothing serving, on the deployment's own volumes, so a refusal here is about the arguments or the VPS, not about #571."
note "backup set $backup_set points at the VPS over SSH, read-only"

step "enrolling the administrator"
# Down a pipe into stdin, never on a command line and never in a file:
# `auth create-admin --password-stdin` is the product's own way to make an
# administrator without a browser, and it is what the deployment's first-run
# flow would otherwise do interactively.
printf '%s' "$admin_pass" | oneshot none \
  /backupd-web auth create-admin --username "$admin_user" --password-stdin \
  || die "could not enrol the administrator."
note "administrator $admin_user enrolled, password generated this run"

step "running one backup cycle, so the pages have something real to render"
# BEFORE the engine starts, and this order is load-bearing rather than
# stylistic. `backupd run` beside a serving engine is two processes writing the
# same SQLite journal, and the loser gets SQLITE_BUSY: measured here, a
# cycle run that way came back with
#
#   lifecycle: transfer: recording TRANSFERRED: state: update artifact:
#   database is locked (5) (SQLITE_BUSY)
#   e2e/vps backed nothing up this cycle: 2 walked, 0 got through
#
# on the second attempt, having got clean through on the first. Nothing
# about the deployment needs the engine up for this: seeding is setup, and
# the other two setup steps already run against a stopped deployment for
# the same family of reason (#571, and the auth store's own flock). So this
# joins them rather than racing them.
#
# It is also the proof that the engine can reach the VPS over SSH, and a
# stronger one than a banner grab: it authenticates with the generated key,
# lists a directory, pulls three files and verifies them.
oneshot "$net_backhaul" \
  /backupd run --config /etc/backupd/config \
  || die "the backup cycle exited non-zero, so the deployment could not pull from the VPS." \
         "Everything the browser is about to look at would be empty, and a suite passing against empty tables proves nothing."

# Optionally run more cycles to grow the durable activity journal past the
# size a three-file seed produces. #730's throw is on the AUTHENTICATED
# /api/v1/activity payload, and a larger one is likelier to cross whatever
# streaming/framing threshold a plain seed never reaches. Default 1 leaves
# the base rig byte-identical; the verify below still holds because the
# files on the VPS do not change between cycles.
seed_cycles="${RM_SEED_CYCLES:-1}"
if [ "$seed_cycles" -gt 1 ]; then
  step "running $((seed_cycles - 1)) more backup cycle(s) to enlarge the activity journal"
  i=1
  while [ "$i" -lt "$seed_cycles" ]; do
    oneshot "$net_backhaul" /backupd run --config /etc/backupd/config \
      || die "seed cycle $((i + 1)) of $seed_cycles exited non-zero."
    i=$((i + 1))
  done
  note "$seed_cycles cycles run; the activity feed holds more than the first three events"
fi

for pair in "payload.bin:$want_payload" "schema.sql:$want_schema" "notes.txt:$want_notes"; do
  name="${pair%%:*}"
  want="${pair##*:}"
  exists_in_volume "$v_backups" "vps/$name" \
    || die "$name never landed in the deployment's backup volume." \
           "The volume holds: $(list_volume "$v_backups" vps)"
  got="$(sha256_in_volume "$v_backups" "vps/$name")"
  [ "$got" = "$want" ] \
    || die "$name landed with different bytes from the VPS's." "VPS:  $want" "here: $got" \
           "A file that exists is not a backup, and a page rendering a row about it would be rendering a lie."
done
note "three artifacts landed and every one matches the VPS by sha256"

# --------------------------------------------------------- the engine

step "starting the engine (/backupd-web serve)"
# The environment is container/compose.yaml's own for this service, and the
# values that differ from it differ for a reason written beside them.
docker run -d \
  --name "$c_engine" \
  --network "$net_internal" \
  --network-alias engine \
  --label "$label" \
  --user "$app_uid:$app_gid" \
  -e TMPDIR=/tmp \
  -e LISTEN_ADDR=":8080" \
  -e PUBLIC_BASE_URL="$base_url" \
  -e TRUST_FORWARDED_HEADERS="true" \
  -v "$v_config:/etc/backupd/config" \
  -v "$v_state:/data/state" \
  -v "$v_backups:/data/backups" \
  -v "$v_keys:/etc/backupd/keys:ro" \
  "$product_image" /backupd-web serve --profile=generic >/dev/null \
  || die "could not start the engine."
created_containers+=("$c_engine")

# The engine also needs the backhaul network, and it is attached after the
# start rather than at it because `docker run` takes one --network. The
# alias is the name the backup set was created against.
docker network connect --alias manager "$net_backhaul" "$c_engine" \
  || die "could not put the engine on the backhaul network, so it has no route to the VPS."

wait_or_die 180 "the engine to report itself live" \
  docker exec "$c_engine" /backupd-web healthcheck --url http://127.0.0.1:8080/health/live
note "$c_engine is serving on the internal network as \"engine\", and is on the backhaul network as \"manager\""

# --------------------------------------------------------- the UI host

step "starting the UI host (/backupd-web serve-ui)"
docker run -d \
  --name "$c_web" \
  --network "$net_internal" \
  --network-alias web-ui \
  --label "$label" \
  --user "$app_uid:$app_gid" \
  --read-only \
  --tmpfs "/tmp:size=16m,mode=1777" \
  -e TMPDIR=/tmp \
  -e LISTEN_ADDR=":8080" \
  -e UPSTREAM_ADDR="http://engine:8080" \
  "$product_image" /backupd-web serve-ui --profile=generic >/dev/null \
  || die "could not start the UI host."
created_containers+=("$c_web")

# And the edge network, where it answers to the name the browser is given.
# This is the ONLY container on that network besides the client, so a spec
# that tried to reach the engine directly would find nothing at all, which
# is the point of there being three networks rather than one.
docker network connect --alias "$edge_web_alias" "$net_edge" "$c_web" \
  || die "could not put the UI host on the edge network, so the client would have nothing to talk to."

wait_or_die 180 "the UI host to answer its own listener" \
  docker exec "$c_web" /backupd-web healthcheck
note "$c_web is serving on the edge network as \"$edge_web_alias\", proxying to \"engine\""

if [ "$front_proxy" = 1 ]; then
  step "starting the TLS + HTTP/2 front proxy (backupd#730 reproduction)"
  # An ordinary reverse proxy in front of serve-ui, taking the edge-network
  # name the client is given and upstreaming to serve-ui's "origin" alias.
  # This is the hop a real NAS has and the plain-HTTP rig did not: the
  # browser now negotiates HTTP/2 over TLS instead of HTTP/1.1 in the clear,
  # which is the transport the client request is identical to every other
  # page's on yet #730 says only /api/v1/activity fails over.
  docker run -d \
    --name "$c_proxy" \
    --network "$net_edge" \
    --network-alias backupd \
    --label "$label" \
    "$proxy_image" >/dev/null \
    || die "could not start the front proxy."
  created_containers+=("$c_proxy")

  proxy_up=0
  for _ in $(seq 1 30); do
    if toolbox --network "$net_edge" -- 'nc -z -w 3 backupd 443'; then
      proxy_up=1; break
    fi
    sleep 1
  done
  [ "$proxy_up" = 1 ] \
    || die "the front proxy never accepted TLS on 443 on the edge network."
  note "$c_proxy terminates TLS + HTTP/2 as \"backupd\", upstream to \"$edge_web_alias\""
fi

# ============================================ the two reachability proofs

step "proving the wire, both hops"

# The engine's own network position, borrowed. --network container: shares
# that container's namespace and its resolver, so this is the route the
# engine has and not a route that resembles it. The engine's image is
# distroless and has no ssh client of its own to ask with.
ssh_banner="$(toolbox --network "container:$c_engine" -- \
  'nc -w 5 vps 22 </dev/null 2>/dev/null | head -1' || true)"
case "$ssh_banner" in
  SSH-*) note "from the engine's network namespace, vps:22 answers \"$ssh_banner\"" ;;
  *) die "from the engine's network namespace, vps:22 did not answer with an SSH banner (got: ${ssh_banner:-nothing})." ;;
esac

# And from the client's, the page the browser is about to open. Run in a
# throwaway on the edge network from the client image itself, so what is
# proven reachable is reachable from the thing that will do the reaching.
login_probe="$(docker run --rm --label "$label" --network "$net_edge" \
  -e "RM_BASE_URL=$base_url" $front_proxy_probe_env "$client_image" \
  node -e '
    (async () => {
      const r = await fetch(process.env.RM_BASE_URL + "/");
      const body = await r.text();
      const title = (body.match(/<title[^>]*>([^<]*)<\/title>/i) || [, ""])[1];
      console.log(r.status + " " + (r.headers.get("content-type") || "") + " " + body.length + " bytes, <title>" + title + "</title>");
    })().catch((e) => { console.log("no answer: " + e.message); process.exitCode = 1; });
  ' 2>&1 || true)"
case "$login_probe" in
  "200 text/html"*) note "from the client's network, GET $base_url/ answers $login_probe" ;;
  *) die "the client could not fetch the Web UI from $base_url/ (got: ${login_probe:-nothing})." \
         "This is the hop the whole stack exists for, so nothing below is worth running until it works." ;;
esac

# ========================================= the break, rehearsed (#795)

if [ "$break_engine" = 1 ]; then
  step "--break-engine: rehearsing the fault before handing the stack over"

  # api_probe <what it is for> -> "<status> <correlation id or ->"
  #
  # /api/v1/activity unauthenticated, from the edge network, which is the
  # exact route and the exact position the browser will fail from. The
  # request carries no session, so a working stack refuses it 401 FROM
  # THE ENGINE; a stack whose engine is gone is answered 502 by serve-ui
  # itself. Those two numbers are the whole proof, and telling them apart
  # is why this probe asks for an API route rather than for the bundle:
  # the bundle is served by serve-ui either way and says nothing about
  # the hop behind it.
  api_probe() {
    docker run --rm --label "$label" --network "$net_edge" \
      -e "RM_BASE_URL=$base_url" $front_proxy_probe_env "$client_image" \
      node -e '
        (async () => {
          const r = await fetch(process.env.RM_BASE_URL + "/api/v1/activity");
          console.log(r.status + " " + (r.headers.get("x-correlation-id") || "-"));
        })().catch((e) => { console.log("no-answer " + e.message); process.exitCode = 1; });
      ' 2>&1 || true
  }

  healthy_probe="$(api_probe)"
  case "$healthy_probe" in
    401\ *) note "with the engine up, GET /api/v1/activity answers $healthy_probe" ;;
    *) die "with the engine up, GET /api/v1/activity answered \"${healthy_probe:-nothing}\", want a 401 from the engine." \
           "The break below is only meaningful against a stack that was working, so this refuses to rehearse on one that was not." ;;
  esac

  docker stop "$c_engine" >/dev/null || die "could not stop the engine for the rehearsal."
  broken_probe="$(api_probe)"
  case "$broken_probe" in
    502\ -) die "with the engine stopped, GET /api/v1/activity answered 502 with no X-Correlation-Id." \
                "The browser's banner would then have nothing to quote and the proxy_error line in serve-ui's log nothing to be joined by (#795)." ;;
    502\ *) note "with the engine stopped, GET /api/v1/activity answers $broken_probe, from serve-ui rather than the engine" ;;
    *) docker start "$c_engine" >/dev/null 2>&1 || true
       die "with the engine stopped, GET /api/v1/activity answered \"${broken_probe:-nothing}\", want 502 from serve-ui." \
           "Either serve-ui went down with the engine, in which case this mode is testing something else entirely, or something is answering for it." ;;
  esac

  docker start "$c_engine" >/dev/null || die "could not start the engine again after the rehearsal."
  wait_or_die 180 "the engine to come back up after the rehearsal" engine_is_live
  healed_probe="$(api_probe)"
  case "$healed_probe" in
    401\ *) note "and with it back, $healed_probe again: the break is reversible, which the recovery half of the suite needs" ;;
    *) die "after restarting the engine, GET /api/v1/activity answered \"${healed_probe:-nothing}\", want 401 again." \
           "A break that cannot be undone leaves no way to assert that the pages recover." ;;
  esac

  start_engine_watcher
  note "the control channel is live at $engine_control (write \"stop\" or \"start\", wait for \"stopped\" or \"started\")"
fi

# ------------------------------------------------------- hand it over

step "the stack is up"
note "RM_BASE_URL       $base_url"
note "RM_ADMIN_USERNAME $admin_user"
note "RM_ADMIN_PASSWORD $admin_pass"
note "RM_BACKUP_SET     $backup_set"
note "RM_ARTIFACTS_DIR  /artifacts, mounted from $artifacts_dir"

client_env=(
  -e "RM_BASE_URL=$base_url"
  -e "RM_ADMIN_USERNAME=$admin_user"
  -e "RM_ADMIN_PASSWORD=$admin_pass"
  -e "RM_BACKUP_SET=$backup_set"
  -e "RM_ARTIFACTS_DIR=/artifacts"
  -e "RM_CHROMIUM_NO_SANDBOX=${RM_CHROMIUM_NO_SANDBOX:-0}"
  -e "HOME=/tmp"
)
# Over the self-signed front proxy the browser and any node fetch in the
# suite must accept the leaf; RM_IGNORE_HTTPS switches on Playwright's
# ignoreHTTPSErrors in web-ui-smoke.mjs, and NODE_TLS_REJECT_UNAUTHORIZED
# covers a suite that fetches from node. Appended after the array literal so
# an empty case never expands to a stray argument.
if [ "$front_proxy" = 1 ]; then
  client_env+=(-e "RM_IGNORE_HTTPS=1" -e "NODE_TLS_REJECT_UNAUTHORIZED=0" -e "NODE_NO_WARNINGS=1")
fi
# backupd#795. The flag the suite branches on, and the directory it drives
# the break from. Same appended-after-the-literal shape as the block
# above, and for the same reason: off, neither variable exists at all, so
# a spec that reads RM_ENGINE_UNREACHABLE gets undefined and asserts the
# healthy feed.
if [ "$break_engine" = 1 ]; then
  client_env+=(-e "RM_ENGINE_UNREACHABLE=1" -e "RM_ENGINE_CONTROL=$engine_control_in_client")
  note "RM_ENGINE_UNREACHABLE 1"
  note "RM_ENGINE_CONTROL     $engine_control_in_client, watched on this host at $engine_control"
fi

client_run=(
  docker run
  --name "$c_client"
  --network "$net_edge"
  --label "$label"
  # Chromium's renderers put their shared buffers in /dev/shm, and Docker's
  # 64 MiB default is not enough for a 1440x900 page: the browser dies
  # mid-run with a page crash and no useful message. --shm-size rather than
  # --ipc=host, because sharing the host's IPC namespace to work around a
  # size default is a much larger hole than the size default is a problem.
  --shm-size=1g
  # Chromium leaves zombies when its parent is pid 1 and pid 1 is a test
  # runner rather than an init. A run that ends with a few hundred defunct
  # processes still exits, and then the next one has no pids left.
  --init
  # The invoking user's uid, so the traces and screenshots below land on
  # the host owned by whoever has to read them. The image's /suite is
  # a+rwX for exactly this: the uid has no account in that image.
  --user "$(id -u):$(id -g)"
  "${client_env[@]}"
  -v "$artifacts_dir:/artifacts"
)

if [ -n "$suite_dir" ]; then
  # The suite is MOUNTED rather than copied into the image, and the choice
  # is about which of the two changes more often. The specs change every
  # time somebody works on them and the runtime changes when a lockfile
  # moves, so mounting keeps the image stable across a day of spec edits
  # and keeps the rebuild for the thing that actually needs one. It costs
  # self-containment: this image alone does not carry a suite, and a run on
  # a machine without the tests checkout has nothing to mount.
  #
  # The anonymous volume over node_modules is what makes it work. Docker
  # seeds a volume from the image's own content at that path, so /suite is
  # the checkout and /suite/node_modules is the image's linux install, and
  # the checkout's darwin-built one is never in the picture.
  client_run+=(-v "$suite_dir:/suite" -v "/suite/node_modules" -w /suite)
  client_run+=("$client_image" npx playwright test)
  [ "$keep_up" = 1 ] || note "running the suite at $suite_dir inside the client container"
else
  client_run+=(-w /suite "$client_image" node smoke.mjs)
  [ "$keep_up" = 1 ] || note "running the built-in browser check (pass --suite DIR to run a Playwright suite instead)"
fi

if [ "$keep_up" = 1 ]; then
  step "--keep-up, so nothing is run and nothing is torn down"
  echo ""
  echo "    Drive it with:"
  echo ""
  printf '        '
  sep=""
  for word in "${client_run[@]}"; do
    printf '%s' "$sep"
    printf '%q' "$word"
    sep=" "
  done
  printf '\n'
  echo ""
  echo "    The teardown line is printed below. Nothing here is published to a host port,"
  echo "    so the only way to reach the UI is from a container on $net_edge."
  if [ "$break_engine" = 1 ]; then
    echo ""
    echo "    --break-engine's watcher is NOT left running: it holds this host's Docker socket,"
    echo "    and a loop outliving the script that started it is a loop nobody owns. Break the"
    echo "    engine by hand instead, which is all the watcher does:"
    echo ""
    echo "        docker stop $c_engine     # serve-ui stays up and answers 502"
    echo "        docker start $c_engine    # and the pages recover"
  fi
  exit 0
fi

step "the client machine drives the browser"
created_containers+=("$c_client")
client_status=0
"${client_run[@]}" || client_status=$?

echo ""
if [ "$client_status" = 0 ]; then
  echo "==> three-machine: PASSED. A real browser signed in to a real deployment and read real data."
else
  echo "==> three-machine: FAILED. The client container exited $client_status." >&2
  echo "    Its evidence is at $artifacts_dir, and the teardown never removes that." >&2
fi
echo "    artifacts: $artifacts_dir"

# The client's status, deliberately, and not the teardown's. The EXIT trap
# runs teardown and then passes this through untouched, so a run that tore
# down cleanly after a red suite is still a red run.
exit "$client_status"

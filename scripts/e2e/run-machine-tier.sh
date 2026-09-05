#!/usr/bin/env bash
# HELP-START
# The Go machine tier, run from inside a manager machine (issue #451).
#
# Rom's ask for #447 was two containers on a dedicated network, one of them
# the rclone-manager machine. scripts/e2e/two-machine-backup.sh makes that
# literally true for the installer. For the Go machine tier it was not: the
# test process runs on the host and reaches the source over a published
# loopback port, because on Docker Desktop for macOS a host process cannot
# sit on a bridge network. core/tests/machines was written with a seam for
# this (Source.Addr answers 127.0.0.1:<published> on the host and
# source:22 inside the network, chosen by RCLONE_MANAGER_MACHINES_NETWORK),
# and this script is the other side of it.
#
# What it stands up:
#
#   * a MANAGER machine: a Go toolchain with a docker client, this
#     repository mounted, and the Go build and module caches mounted, on
#     the dedicated network, playing the NAS;
#   * a dedicated NETWORK, labelled the way core/tests/dockerlease labels
#     everything so a killed run is reclaimed;
#   * the SOURCE machines, created by the harness inside the manager, one
#     per test, from scripts/e2e/source-machine.Dockerfile.
#
# Then it runs the machine-tier packages inside the manager with
# RCLONE_MANAGER_MACHINES_NETWORK set, so nothing publishes a port and every
# address a test uses is the address a real manager would use.
#
# It runs them under core/cmd/gotestwatch rather than under a bare
# `go test`, which is the same wrapper scripts/ci-local.sh puts them under
# and for the same reason (#256): a machine-tier package's wall clock tracks
# real machine load, and a fixed -timeout chosen on a quiet machine kills a
# run that is still making progress. Being a drop-in for that step is the
# point, so it takes --race too.
#
# # The docker socket question, answered
#
# The manager container gets the docker socket. There were two ways to do
# this and the other one does not work: the harness creates its machines
# per test, with per-test key material and per-test host keys, so a driver
# cannot pre-create them and hand them over. Anything short of the socket
# would mean rewriting the harness to take machines it did not make, which
# is the opposite of #447.
#
# two-machine-backup.sh refuses the socket for its own manager, and that
# refusal is still right there and does not apply here. Its manager runs
# the real INSTALLER, so a mounted socket would put the product's own
# containers on the developer's host and the installer would be installing
# onto the machine the script is running on, which is the one thing that
# test exists not to do. This manager runs the test harness. The harness is
# orchestration, not the product, and what this script is proving is where
# the manager sits on the network, not what its daemon can see.
#
# Two consequences worth knowing before reading a failure here. The
# containers the harness creates are SIBLINGS of the manager on the host's
# daemon rather than children of it, which is fine because they join the
# same network by name. And a bind mount the harness asks for is resolved
# by the host's daemon against the HOST's filesystem, which is why this
# script mounts the repository at the same absolute path inside the manager
# as it has outside: core/tests/.run/<test> then means one directory to
# both sides. Mount it anywhere else and the source machines come up with
# empty key directories and refuse every login.
#
# # Not root, and not opted out of
#
# core/internal/testenv refuses to run as root rather than skipping the
# permission-bit tests, which is deliberate (#456's shape: a skip deletes
# coverage from a run that goes on saying ok). So the manager runs as the
# invoking user's uid and gid. The docker socket is root:root 0660, so the
# container is given gid 0 as a SUPPLEMENTARY group, which is how
# docker-outside-of-docker has always granted socket access. euid is still
# not zero, so the refusal is satisfied honestly rather than opted out of.
#
# # Cost, measured on this machine rather than guessed
#
# On the 4 CPU / 4 GB Docker Desktop VM this gate runs against, arm64
# native, running every machine package:
#
#   warm, --race, under gotestwatch:  169s wall, 164s inside, compile 3s
#   the compile alone, both caches empty, --race:  45s
#
#   cold, --race, under gotestwatch: 217s wall, 169s inside, compile 45s
#
# and, measured before this took --race and gotestwatch, on four packages
# under a plain `go test`: 210s wall cold and 120s warm, so about 76s of
# compile.
#
# #451 asked for the cold figure because a cold compile of rclone's module
# graph took over six minutes in CI with no cache, and that is what the
# "run every test on two machines" option was rejected on. It does not
# reproduce here, for two reasons worth knowing before either number is
# quoted again: only the packages the tier imports get compiled, not the
# whole product, and this builds arm64 natively. DOCKER_DEFAULT_PLATFORM is
# linux/amd64 on this machine, and with it in force the manager was built
# emulated and the compile was measuring qemu, which is why the platform is
# named from the daemon's own architecture below.
#
# The 45 seconds is worth its own line, because it is exactly gotestwatch's
# unmeasured floor. A cold compile inside the watched window does not merely
# risk tripping the watchdog, it lands on the boundary, and under any load
# at all it goes over. That is why the compile is its own step above the
# watched run rather than inside it.
#
# The caches are volumes rather than being thrown away with the container
# precisely so the warm number is the one a gate pays.
#
# # Hygiene
#
# The manager and the network are torn down on success, on failure and on
# interrupt. Everything carries a per-run id, nothing publishes a host
# port, and the harness reclaims its own machines. Two of these can run at
# once.
# HELP-END
# Everything below is one brace group, and that is not a style choice.
#
# bash reads a script from the file incrementally, by byte offset, so
# editing a script while it is running makes it resume mid-token: this one
# ran for 159 seconds, passed every package, and then died with "syntax
# error near unexpected token `('" in a branch that was never taken, purely
# because the file had been edited underneath it. A run this long is
# exactly the kind somebody edits while it works. Wrapping the body forces
# bash to parse all of it before executing any of it.
{
set -euo pipefail

# --help reads this file, and the line below this one leaves the directory
# the script was invoked from, so a relative $0 would stop resolving. Both
# paths are settled here, from the same dirname, before that happens.
self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

step() { echo ""; echo "==> machine-tier: $*"; }
note() { echo "    $*"; }

die() {
  echo "" >&2
  echo "==> machine-tier: FAILED. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit 1
}

# A capability this machine does not have is not a failure and is not a
# pass either. Exit 3, the same status scripts/lib/ci-local-gate.sh uses
# for INCOMPLETE and the same one two-machine-backup.sh gives, so a caller
# can tell "could not run it" from "ran it and it failed" without parsing
# prose.
EXIT_CANNOT_RUN=3
cannot_run() {
  echo "" >&2
  echo "==> machine-tier: CANNOT RUN. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit "$EXIT_CANNOT_RUN"
}

# ------------------------------------------------------------------ help
#
# --help prints the header block between the two markers at the top of
# this file, and deliberately not a range of line numbers (#514). The old
# form was `sed -n '2,84p' "$0"`, so the help an operator read was a set of
# coordinates rather than a piece of text: inserting a comment above the
# boundary rewrote it, deleting one truncated it, and nothing anywhere
# would have noticed either. Both had already happened by the time #514
# was written. Markers move with the text they delimit, and
# scripts/tests/e2e-help.test.sh pins the rendered result, so a reword is
# a decision now rather than an accident.
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

# Every package that reaches a machine, which is the gate's own
# gotestwatch list plus tests/machines. The harness package is not on the
# gate's list because it is a harness rather than a machine-tier package
# and runs in the plain `go test` step, but it is exactly the package whose
# own #161, #243 and #456 proofs are worth running in this placement too.
packages="./tests/machines/... ./tests/machinegate/... ./tests/sftpintegration/... ./tests/miniointegration/... ./tests/conformance/... ./tests/crashmatrix/..."
keep=0
run_filter=""
verbose=""
race=""
while [ $# -gt 0 ]; do
  case "$1" in
    --packages) packages="${2:-}"; shift 2 ;;
    --packages=*) packages="${1#--packages=}"; shift ;;
    --run) run_filter="${2:-}"; shift 2 ;;
    --run=*) run_filter="${1#--run=}"; shift ;;
    # `go test -v` inside the manager. Worth having because the numbers
    # these tests measure are printed with t.Logf, and t.Logf on a PASSING
    # test is invisible without it: reading a measurement out of this
    # placement is otherwise only possible by making it fail.
    -v|--verbose) verbose="-v"; shift ;;
    # `go test -race`, which is what the gate runs the machine tier with.
    # Off by default here because a race build costs compile time and the
    # cost figures in this header were measured without it; on when this
    # driver is standing in for the gate's own step.
    --race) race="-race"; shift ;;
    # Leaves the manager container and the network up after a FAILING run,
    # for reading. Never the default.
    --keep-on-failure) keep=1; shift ;;
    -h|--help)
      render_help
      exit 0 ;;
    *) die "unknown option $1" "Usage: $0 [--packages '<go test patterns>'] [--run <regexp>] [-v] [--race] [--keep-on-failure]" ;;
  esac
done

# ------------------------------------------------------------- identities

run_id="${MACHINE_TIER_RUN_ID:-$$-$(date +%s)-${RANDOM}}"
label_key="rclone-manager-test"
label="$label_key=1"
net="rclone-manager-machines-driver-$run_id"
manager="rclone-manager-machine-tier-$run_id"
manager_image="rclone-manager-machine-tier:1"

source_dockerfile="$repo_root/scripts/e2e/source-machine.Dockerfile"
manager_dockerfile="$repo_root/scripts/e2e/manager-machine.Dockerfile"

# The caches are named volumes rather than host directories on purpose. A
# host directory would be written by the container's uid and then be in the
# way of an ordinary `go build` on the host; a volume is the manager
# machine's own disk, which is what it would be on a real one.
build_cache="rclone-manager-machine-tier-gocache"
mod_cache="rclone-manager-machine-tier-gomodcache"

# ---------------------------------------------------------------- teardown

failed=0
cleanup() {
  local status=$?
  if [ "$status" -ne 0 ] && [ "$keep" = "1" ]; then
    echo "" >&2
    echo "    --keep-on-failure: leaving $manager and $net up." >&2
    echo "    docker exec -it $manager bash" >&2
    echo "    docker rm -f $manager && docker network rm $net" >&2
    return
  fi
  docker rm -f "$manager" >/dev/null 2>&1 || true
  # The harness removes its own machines, but a killed run may not have,
  # and a network with an endpoint left on it cannot be removed. Anything
  # still on this network is ours by construction: the name carries this
  # run's id.
  local stragglers
  stragglers="$(docker ps -aq --filter "network=$net" 2>/dev/null || true)"
  if [ -n "$stragglers" ]; then
    # shellcheck disable=SC2086
    docker rm -f $stragglers >/dev/null 2>&1 || true
  fi
  docker network rm "$net" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# ------------------------------------------------------------- capability

step "checking this machine can run the tier"
command -v docker >/dev/null 2>&1 \
  || cannot_run "docker is not on PATH." "The machine tier is two containers on a network; there is nothing to stand them up with."
docker info >/dev/null 2>&1 \
  || cannot_run "the docker daemon is not answering." "Start Docker and run this again."

[ -r "$source_dockerfile" ] \
  || die "the source machine Dockerfile is missing at $source_dockerfile." \
         "It is the one definition of the simulated VPS, shared with core/tests/machines and two-machine-backup.sh."
[ -r "$manager_dockerfile" ] \
  || die "the manager machine Dockerfile is missing at $manager_dockerfile."

# The Go version comes from core/go.mod rather than from a constant here,
# because a bumped go directive that this script did not follow would
# compile the tier against an older toolchain and say nothing.
go_version="$(awk '/^go [0-9]/ { print $2; exit }' core/go.mod)"
[ -n "$go_version" ] \
  || die "could not read the go directive out of core/go.mod, so there is no toolchain version to build the manager machine with."
note "toolchain: go $go_version (from core/go.mod)"

# The manager machine has to be the host daemon's own architecture. On this
# Mac the build picked amd64 unprompted and every compile inside it ran
# under emulation, which turns "measure the cost" into measuring qemu.
daemon_arch="$(docker info --format '{{.Architecture}}' 2>/dev/null || true)"
case "$daemon_arch" in
  aarch64|arm64) platform="linux/arm64" ;;
  x86_64|amd64)  platform="linux/amd64" ;;
  *) die "the docker daemon reports an architecture this script does not know how to name for --platform: ${daemon_arch:-unknown}." \
         "Without an explicit platform the manager machine can be built for the wrong one and every compile inside it runs emulated." ;;
esac
note "platform: $platform (daemon reports $daemon_arch)"

uid="$(id -u)"
gid="$(id -g)"
[ "$uid" != "0" ] \
  || die "this script is running as root, and the manager machine would be too." \
         "core/internal/testenv refuses to run the permission-bit tests as root rather than skipping them, which is deliberate: a skip there deletes coverage from a run that goes on saying ok. Run this as an ordinary user."

# ------------------------------------------------------------ the machines

step "building the manager machine image"
docker build -q -t "$manager_image" \
  --platform "$platform" \
  --build-arg "GO_VERSION=$go_version" \
  -f "$manager_dockerfile" "$(dirname "$manager_dockerfile")" >/dev/null \
  || die "could not build the manager machine image from $manager_dockerfile."
note "manager machine: $manager_image"

step "creating the dedicated network"
docker network create --label "$label" "$net" >/dev/null \
  || cannot_run "could not create the network $net." \
                "Docker's default address pool is about thirty networks wide; if it is full, reclaim the leaked ones (docker network prune) and try again."
note "network: $net"

step "preparing the manager machine's caches"
# A named volume is created root-owned, and the manager runs as an ordinary
# user, so the first run inside it dies at "failed to initialize build
# cache: permission denied" before it compiles a line. One rootful
# throwaway container fixes the ownership once; it is not the manager and
# it runs nothing of this repository's.
docker run --rm --platform "$platform" \
  -v "$build_cache:/gocache" -v "$mod_cache:/gomodcache" \
  alpine:3.20 chown -R "$uid:$gid" /gocache /gomodcache >/dev/null \
  || die "could not give the manager machine's caches to uid $uid." \
         "They are named volumes and docker creates them root-owned; without this the toolchain inside cannot write a single object."
note "caches: $build_cache and $mod_cache, owned by $uid:$gid"

step "starting the manager machine on it"
# The repository at the SAME absolute path inside as out, because the bind
# mounts the harness asks for are resolved by the host's daemon against the
# host's filesystem. See the header.
docker run -d --name "$manager" \
  --platform "$platform" \
  --label "$label" \
  --network "$net" \
  --network-alias manager \
  --user "$uid:$gid" \
  --group-add 0 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$repo_root:$repo_root" \
  -v "$build_cache:/gocache" \
  -v "$mod_cache:/gomodcache" \
  -w "$repo_root/core" \
  -e GOCACHE=/gocache \
  -e GOMODCACHE=/gomodcache \
  -e GOFLAGS=-mod=mod \
  -e GOWORK=off \
  -e HOME=/tmp \
  -e "RCLONE_MANAGER_MACHINES_NETWORK=$net" \
  -e "CI_LOCAL=${CI_LOCAL:-}" \
  -e "CI_LOCAL_SKIP_DOCKER=${CI_LOCAL_SKIP_DOCKER:-}" \
  "$manager_image" sleep infinity >/dev/null \
  || die "could not start the manager machine."

# An /etc/passwd entry for the uid the manager runs as. Without one,
# ssh-keygen exits 255 with "No user exists for uid <n>" and the first
# machine the harness tries to stand up has no host key. The uid is the
# invoking user's, chosen so the repository bind mount and the caches are
# writable, and no base image can be expected to have an entry for it.
docker exec -u 0:0 "$manager" sh -c \
  "getent group $gid >/dev/null || echo 'machinetier:x:$gid:' >> /etc/group; \
   getent passwd $uid >/dev/null || echo 'machinetier:x:$uid:$gid:machine tier:/tmp:/bin/sh' >> /etc/passwd" \
  || die "could not give uid $uid a passwd entry inside the manager machine." \
         "ssh-keygen refuses to run for a uid with no entry, so no machine would get a host key."

# The manager has to be able to reach the daemon before anything is worth
# running, and it is the one thing that is not obvious from a `go test`
# failure ten minutes later.
docker exec "$manager" docker version >/dev/null 2>&1 \
  || die "the manager machine cannot talk to the docker daemon through the mounted socket." \
         "The socket is root:root 0660 and the container runs as $uid:$gid with gid 0 as a supplementary group; if that has changed, the harness inside cannot create a single machine."
# And buildx, separately, because a CLI without it silently falls back to
# the legacy builder and the harness's own image build is watched through
# --progress=plain, which the legacy builder rejects. The failure that
# produces reads as "unknown flag" from inside a Go test, twenty lines deep.
docker exec "$manager" docker buildx version >/dev/null 2>&1 \
  || die "the manager machine's docker client has no buildx plugin." \
         "Without it the CLI falls back to the legacy builder, which does not understand --progress=plain, and the harness cannot build a source machine."
note "manager: $manager (uid $uid, on $net, socket reachable)"

# ---------------------------------------------------------------- the run

step "compiling the tier inside the manager machine"
# Before the watched run, not inside it.
#
# gotestwatch bounds a run by the pace of its own test events, and its
# unmeasured floor is 45 seconds: nothing has been observed yet, so that is
# all it has to go on. A cold compile inside this container takes longer
# than that and emits no test event while it works, so the first thing the
# watchdog sees is 45 seconds of silence and it kills a run that was
# building normally. That is not a gotestwatch bug. In scripts/ci-local.sh
# the gotestwatch step is preceded by a whole `go test ./...` step, so the
# build cache is warm by the time anything is watched; this driver had no
# such step, and this is it.
#
# `-run ^$` compiles and links every test binary and runs no test in it, so
# nothing here starts a container.
compile_started="$(date +%s)"
docker exec "$manager" go test $race -count=1 -run '^$' $packages >/dev/null \
  || die "the machine-tier packages do not compile inside the manager machine." \
         "Nothing below can run, and this is a build failure rather than a test one."
note "compiled in $(( $(date +%s) - compile_started ))s"

step "running the machine tier inside the manager machine, under gotestwatch"
note "packages: $packages"
[ -n "$run_filter" ] && note "filter:   -run $run_filter"
note "no port is published by any source or medium: RCLONE_MANAGER_MACHINES_NETWORK=$net"

started="$(date +%s)"
set +e
# shellcheck disable=SC2086
docker exec "$manager" go run ./cmd/gotestwatch $race -count=1 $verbose ${run_filter:+-run "$run_filter"} $packages
status=$?
set -e
elapsed=$(( $(date +%s) - started ))

if [ "$status" -ne 0 ]; then
  failed=1
  die "the machine tier failed inside the manager machine after ${elapsed}s (exit $status)." \
      "That is a product or harness failure, not a capability one: the manager was up, on the network, and talking to the daemon."
fi

step "PASSED in ${elapsed}s"
note "every source and medium was reached by its network alias, with nothing published"

# The brace group above is half the protection against this file being
# edited while it runs; this exit is the other half. bash parses the whole
# group before executing any of it, but once the group is done it goes back
# to the file for whatever comes next, at the byte offset it saved. If the
# file grew in the meantime that offset now points into the middle of a
# line, and bash tries to run the fragment: this script passed every
# package, printed PASSED, and then exited 127 on a word out of its own
# header comment. Exiting here means there is never a next read.
exit 0
}

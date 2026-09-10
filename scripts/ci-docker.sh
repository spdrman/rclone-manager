#!/usr/bin/env bash
#
# Run anything in this repository in a container instead of on this machine.
#
#     bash scripts/ci-docker.sh                    # the whole gate
#     bash scripts/ci-docker.sh --fast             # flags pass to ci-local.sh
#     bash scripts/ci-docker.sh --shell            # a prompt in that environment
#     bash scripts/ci-docker.sh --exec go test ./core/...
#     bash scripts/ci-docker.sh --exec python3 -m unittest discover scripts/install
#
# --exec is the general form and the gate is one use of it. Nothing here
# should need the host's go, node, npm, python or linker: a toolchain that
# comes from whatever the laptop updated overnight cannot be the thing a
# red result is measured against.
#
# WHY THIS EXISTS. scripts/ci-local.sh is the gate and it is not changed
# by any of this: the same script runs, with the same steps, and its
# verdict is still the verdict. What changes is what it runs ON. The
# toolchain moves from "whatever this laptop has today" to the pinned set
# in scripts/ci/Dockerfile, which is the difference between a red run
# meaning something and a red run meaning macOS shipped a new SDK
# overnight. See that file for the four separate environment failures in
# one evening that produced it.
#
# WHY THE HOST'S DAEMON, AND NOT ONE INSIDE. Docker-out-of-docker: the
# socket is mounted and every container the gate starts is a SIBLING on
# this machine's daemon, not a child of a daemon in here. Two reasons,
# and the first is decisive. The two-machine proof needs a privileged
# container; running it under a nested daemon means asking for privilege
# twice and getting a different topology than the one the proof is about.
# The second is that sibling containers are exactly what the gate creates
# today, so nothing about how the proof runs changes.
#
# WHY THE REPOSITORY IS MOUNTED AT ITS OWN PATH. That is what makes
# docker-out-of-docker work at all. The gate takes REPO_ROOT from `pwd`
# and hands paths under it to `docker run -v`, and those are interpreted
# by the HOST daemon. Mounted anywhere else, every one of them would
# name a directory that does not exist out there, and the failure would
# be an empty mount rather than an error. Same path inside and out, and
# they resolve to the same bytes.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"

# A git worktree keeps its objects somewhere else, and .git is a file
# pointing at them. Without this mount every git call in the gate fails
# in a checkout that looks perfectly normal from the outside.
git_common="$(cd "$repo" && git rev-parse --git-common-dir 2>/dev/null || true)"
case "$git_common" in
  /*) : ;;
  "") git_common="" ;;
  *)  git_common="$repo/$git_common" ;;
esac

# arm64 by default, and stated rather than derived. DOCKER_DEFAULT_PLATFORM
# is linux/amd64 in at least one shell around here, and inheriting that
# would put every run under qemu: correct, and slow enough that nobody
# would do it twice. Deriving it from `uname -m` instead would be quietly
# wrong the first time this runs somewhere else, so the default is the
# declared preference and RCM_CI_PLATFORM is how you say otherwise.
unset DOCKER_DEFAULT_PLATFORM
platform="${RCM_CI_PLATFORM:-linux/arm64}"
host_arch="$(uname -m)"
case "$host_arch:$platform" in
  arm64:linux/arm64|aarch64:linux/arm64|x86_64:linux/amd64|amd64:linux/amd64) : ;;
  *) printf '==> ci-docker: %s on a %s host, so this runs emulated. RCM_CI_PLATFORM overrides it.\n' \
       "$platform" "$host_arch" >&2 ;;
esac

# Tagged by the Dockerfile's own hash AND the uid it is built for, so a
# changed Dockerfile rebuilds and two users on one machine never collide
# over an image whose passwd entry names only one of them.
ci_uid="$(id -u)"; ci_gid="$(id -g)"
image="rclone-manager-ci:$(shasum -a 256 "$here/ci/Dockerfile" | cut -c1-12)-u${ci_uid}"

# Named volumes, and the reason is the same failure story as the image.
# The Go build cache on the host was wiped by three concurrent `go clean
# -cache` runs in the middle of a gate, and entries vanished mid-compile.
# In here the caches belong to the gate, so nothing else on the machine
# can take them away, and a second run is fast rather than cold.
# Keyed by uid. A named volume takes its ownership from whoever first
# wrote it, so one created by a root run is unwritable to a run as the
# host user, and Go reports it as "permission denied" on a cache file
# rather than as anything to do with ownership.
vol_prefix="rcm-ci-u$(id -u)"
declare -a mounts=(
  -v "/var/run/docker.sock:/var/run/docker.sock"
  -v "$repo:$repo"
  -v "${vol_prefix}-gocache:/ci/.cache/go-build"
  -v "${vol_prefix}-gomodcache:/ci/go/pkg/mod"
  -v "${vol_prefix}-npm:/ci/.npm"
  -v "${vol_prefix}-xdg:/ci/.cache/rclone-manager-tests-gate"
)

# The host's own uid, not root, and group 0 for the socket.
#
# Root ignores file modes, and one installer test sets a directory
# unwritable to check the installer refuses rather than raising: as root
# it cannot set the case up and skips itself, which is coverage lost with
# nobody deciding to lose it. Group 0 because the daemon socket arrives
# inside as root:root 0660, so the group bit is the way in without being
# root. Docker Desktop maps writes on the bind mount back to the host
# user whatever uid makes them, so the repository stays owned by you.
mounts+=(--user "$ci_uid:$ci_gid" --group-add 0)
[ -n "$git_common" ] && [ -d "$git_common" ] && mounts+=(-v "$git_common:$git_common")

# node_modules never comes from the host, and this is not an
# optimisation. The host is macOS: ui/shared/node_modules holds darwin
# binaries for esbuild and rollup, and a linux container cannot execute
# them. Masking each workspace with its own volume gives the container a
# linux install of the same lockfile, and leaves the host's alone for
# whatever the developer runs outside this.
declare -a js_workspaces=("ui/shared" "apps/common/tests")
for ws in "${js_workspaces[@]}"; do
  [ -d "$repo/$ws" ] || continue
  mounts+=(-v "${vol_prefix}-nm-$(printf '%s' "$ws" | tr '/' '-'):$repo/$ws/node_modules")
done

say() { printf '==> ci-docker: %s\n' "$*"; }

if ! docker image inspect "$image" >/dev/null 2>&1; then
  say "building the toolchain image ($image, $platform)"
  docker build --platform "$platform" \
    --build-arg "CI_UID=$ci_uid" --build-arg "CI_GID=$ci_gid" \
    -t "$image" "$here/ci"
else
  say "toolchain image $image is already built"
fi

# npm ci inside the container, and only when the lockfile it was last run
# for has changed. The stamp lives in the volume rather than beside the
# lockfile, because the lockfile is on the host and shared with every
# other checkout.
bootstrap='
set -eu
for ws in '"${js_workspaces[*]}"'; do
  [ -d "$ws" ] || continue
  lock="$ws/package-lock.json"
  [ -f "$lock" ] || continue
  want="$(sha256sum "$lock" | cut -d" " -f1)"
  stamp="$ws/node_modules/.ci-docker-lockfile"
  if [ -f "$stamp" ] && [ "$(cat "$stamp")" = "$want" ]; then
    echo "==> ci-docker: $ws dependencies are current"
    continue
  fi
  echo "==> ci-docker: installing $ws dependencies (lockfile changed or volume empty)"
  ( cd "$ws" && npm ci )
  printf "%s" "$want" > "$stamp"
done
'

case "${1:-}" in
  --shell)
    shift
    say "a shell in $image ($platform)"
    exec docker run --rm -it --platform "$platform" "${mounts[@]}" -w "$repo" "$image" bash
    ;;
  --exec)
    # Deliberately NOT preceded by the dependency bootstrap. --exec is for
    # one command, `go test` and `python3 -m unittest` are most of them,
    # and neither needs node_modules; paying an npm ci to find out a Go
    # test passes is how a runner stops being used. Ask for it with
    # --deps when the command does need them.
    shift
    [ "$#" -gt 0 ] || { echo "ci-docker: --exec needs a command" >&2; exit 2; }
    exec docker run --rm --platform "$platform" "${mounts[@]}" -w "$repo" "$image" "$@"
    ;;
  --deps)
    shift
    say "installing dependencies if the lockfiles moved"
    docker run --rm --platform "$platform" "${mounts[@]}" -w "$repo" "$image" bash -c "$bootstrap"
    [ "$#" -gt 0 ] || exit 0
    exec docker run --rm --platform "$platform" "${mounts[@]}" -w "$repo" "$image" "$@"
    ;;
esac

say "installing dependencies if the lockfiles moved"
docker run --rm --platform "$platform" "${mounts[@]}" -w "$repo" "$image" bash -c "$bootstrap"

say "running scripts/ci-local.sh in $image ($platform)"
exec docker run --rm --platform "$platform" "${mounts[@]}" -w "$repo" "$image" \
  bash scripts/ci-local.sh "$@"

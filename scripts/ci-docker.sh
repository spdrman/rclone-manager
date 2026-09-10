#!/usr/bin/env bash
#
# Run the gate in a container instead of on this machine.
#
#     bash scripts/ci-docker.sh            # the whole gate
#     bash scripts/ci-docker.sh --fast     # whatever flags ci-local.sh takes
#     bash scripts/ci-docker.sh --shell    # a prompt in the same environment
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

# Native, never emulated. DOCKER_DEFAULT_PLATFORM is set to linux/amd64
# in at least one shell around here, and inheriting it would run the
# whole gate under qemu: correct, and slow enough that nobody would run
# it twice. The gate is a correctness gate, so it wants this machine's
# own architecture.
unset DOCKER_DEFAULT_PLATFORM
arch="$(uname -m)"
case "$arch" in
  arm64|aarch64) platform="linux/arm64" ;;
  x86_64|amd64)  platform="linux/amd64" ;;
  *) echo "ci-docker: unknown host architecture $arch" >&2; exit 2 ;;
esac

image="rclone-manager-ci:$(shasum -a 256 "$here/ci/Dockerfile" | cut -c1-12)"

# Named volumes, and the reason is the same failure story as the image.
# The Go build cache on the host was wiped by three concurrent `go clean
# -cache` runs in the middle of a gate, and entries vanished mid-compile.
# In here the caches belong to the gate, so nothing else on the machine
# can take them away, and a second run is fast rather than cold.
vol_prefix="rcm-ci"
declare -a mounts=(
  -v "/var/run/docker.sock:/var/run/docker.sock"
  -v "$repo:$repo"
  -v "${vol_prefix}-gocache:/root/.cache/go-build"
  -v "${vol_prefix}-gomodcache:/go/pkg/mod"
  -v "${vol_prefix}-npm:/root/.npm"
  -v "${vol_prefix}-xdg:/root/.cache/rclone-manager-tests-gate"
)
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
  docker build --platform "$platform" -t "$image" "$here/ci"
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

if [ "${1:-}" = "--shell" ]; then
  shift
  say "opening a shell in the gate environment"
  exec docker run --rm -it --platform "$platform" "${mounts[@]}" -w "$repo" "$image" bash
fi

say "installing dependencies if the lockfiles moved"
docker run --rm --platform "$platform" "${mounts[@]}" -w "$repo" "$image" bash -c "$bootstrap"

say "running scripts/ci-local.sh in $image"
exec docker run --rm --platform "$platform" "${mounts[@]}" -w "$repo" "$image" \
  bash scripts/ci-local.sh "$@"

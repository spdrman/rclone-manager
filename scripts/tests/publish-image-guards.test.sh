#!/usr/bin/env bash
# An automated control for the refusals in
# scripts/release/publish-image.sh. Issue #88 (B5.2).
#
# The script it drives is the one step in this repository that does
# something irreversible: it puts bytes in a public registry under a
# semantic version. Every other release artifact can be regenerated after
# the fact, and a bad registry tag cannot. So its refusals get a control
# for the same reason #174's generator got one in
# record-release-hashes-guards.test.sh, only more so: this one runs once,
# on the day it matters, and nobody will be watching it work for the first
# time.
#
# Structure copied from that file on purpose. Throwaway `git init`
# repository per refusal, the real script, the GUARDS_ONLY=1 seam that
# stops immediately after the guard block and before the first Docker
# command, and an assertion on the exit code AND the distinct message.
# Every refusal exits 2, so an exit-code-only assertion cannot tell "the
# tree is dirty" from "there is a private key sitting in it", and those
# call for very different reactions.
#
# Positive controls, because every assertion here is a negative one:
#
#   * a clean checkout whose HEAD is the commit the manifest records
#     reaches the seam, or these tests would pass equally against a script
#     that refuses everything;
#   * guard 6's two arms are driven through the same `go` stub at exit 0
#     and exit 1, so a stub that was not on PATH, or a script that ignored
#     the exit status, fails the pair.
set -uo pipefail

unset GIT_INDEX_FILE GIT_DIR GIT_WORK_TREE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR GIT_PREFIX

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="${REPO_ROOT}/scripts/release/publish-image.sh"

failures=0
current=""
tmpdirs=()

cleanup() {
  for d in ${tmpdirs+"${tmpdirs[@]}"}; do
    [ -n "$d" ] && rm -rf "$d"
  done
}
trap cleanup EXIT

fail() {
  failures=$((failures + 1))
  echo "FAIL: ${current}: $1" >&2
  if [ -n "${2:-}" ]; then
    echo "--- script output ---" >&2
    echo "$2" >&2
    echo "---------------------" >&2
  fi
}

# new_repo prints the path of a throwaway repository shaped like this one
# from the script's point of view: the two JSON files it reads, the paths
# its dirty check looks at, and a committed HEAD.
#
# manifest_commit is written AFTER the commit, so it can name the real
# HEAD. That leaves container/release-manifest.json modified, which is
# deliberate and harmless: the dirty guard looks at core, apps, ui and
# container/Dockerfile, and the manifest is none of them. If that ever
# changes, the positive control below stops reaching the seam and says so.
new_repo() {
  local dir
  dir="$(mktemp -d)"
  tmpdirs+=("$dir")
  git -C "$dir" init -q -b main
  git -C "$dir" config user.email t@example.invalid
  git -C "$dir" config user.name t
  mkdir -p "$dir/core" "$dir/apps/common/packaging" "$dir/ui" "$dir/container" "$dir/provenance"
  printf 'package main\n' >"$dir/core/main.go"
  printf 'ui\n' >"$dir/ui/marker"
  printf 'FROM scratch\n' >"$dir/container/Dockerfile"
  cat >"$dir/apps/common/packaging/canonical.json" <<'JSON'
{ "image": { "reference": "ghcr.io/spdrman/backup-manager:1.0.0", "published": false } }
JSON
  printf '{ "version": "test", "commit": "0000000000000000000000000000000000000000" }\n' \
    >"$dir/container/release-manifest.json"
  printf '{}\n' >"$dir/provenance/sbom.spdx.json"
  git -C "$dir" add -A
  git -C "$dir" commit -qm base
  echo "$dir"
}

# pin_manifest_to_head rewrites the manifest so its commit is this
# repository's HEAD, which is what guard 2 requires.
pin_manifest_to_head() {
  local dir="$1"
  printf '{ "version": "test", "commit": "%s" }\n' "$(git -C "$dir" rev-parse HEAD)" \
    >"$dir/container/release-manifest.json"
}

# stub_go writes a `go` onto PATH that exits with a fixed code, so guard
# 6's "the provenance bundle is stale" branch can be driven both ways
# without a Go toolchain or a real module in the fixture.
stub_go() {
  local dir="$1" code="$2" bin
  bin="${dir}/.stubbin"
  mkdir -p "$bin"
  cat >"${bin}/go" <<EOF
#!/usr/bin/env bash
exit ${code}
EOF
  chmod +x "${bin}/go"
  echo "$bin"
}

run_guards() {
  local dir="$1"
  shift
  local out rc
  out="$(cd "$dir" && env GUARDS_ONLY=1 "$@" bash "$SCRIPT" 2>&1)"
  rc=$?
  echo "$rc"
  echo "$out"
}

expect() {
  local rc="$1" out="$2" want_rc="$3" want="$4"
  if [ "$rc" != "$want_rc" ]; then
    fail "exit ${rc}, want ${want_rc}" "$out"
    return
  fi
  case "$out" in
    *"$want"*) ;;
    *) fail "output does not contain: ${want}" "$out" ;;
  esac
}

refute() {
  local out="$1" unwanted="$2"
  case "$out" in
    *"$unwanted"*) fail "output contains, and must not: ${unwanted}" "$out" ;;
  esac
}

split_rc() { echo "$1" | head -n1; }
split_out() { echo "$1" | tail -n +2; }

# --- positive control, first, so the refusals below mean something ------
current="a clean checkout at the commit the manifest records reaches the seam"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "every guard passed"
expect "$rc" "$out" 0 "Would publish ghcr.io/spdrman/backup-manager:1.0.0"

# --- guard 1: the files it reads are not there --------------------------
current="canonical.json missing"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
rm "$repo/apps/common/packaging/canonical.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "apps/common/packaging/canonical.json is not readable"

current="canonical.json present but carrying no reference"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
printf '{ "image": { "published": false } }\n' >"$repo/apps/common/packaging/canonical.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "no image reference in"

# --- guard 2: HEAD is not the commit the manifest describes -------------
current="HEAD is not the commit the release manifest records"
repo="$(new_repo)" # the fixture's manifest still pins all-zeroes
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "and HEAD is"
expect "$rc" "$out" 2 "in the registry"

current="a manifest that pins no commit at all"
repo="$(new_repo)"
printf '{ "version": "test" }\n' >"$repo/container/release-manifest.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "pins no commit"
refute "$out" "and HEAD is"

# --- guard 3: a waived manifest must never be published -----------------
current="a manifest stamped unsafe_local_build"
repo="$(new_repo)"
printf '{ "unsafe_local_build": true, "version": "test", "commit": "%s" }\n' \
  "$(git -C "$repo" rev-parse HEAD)" >"$repo/container/release-manifest.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "unsafe_local_build"
expect "$rc" "$out" 2 "public registry"

# --- guard 4: a dirty tree is not the release ---------------------------
current="a working tree dirty in a path the image is built from"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
printf 'uncommitted\n' >>"$repo/core/main.go"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "the working tree is dirty"

# --- guard 5: key material on disk --------------------------------------
current="an untracked private key beside the script"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
printf 'not a real key\n' >"$repo/cosign.key"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "private key material is present in the working tree"
expect "$rc" "$out" 2 "cosign.key"

current="a tracked .pem is refused too, not only an untracked .key"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
printf 'not a real key\n' >"$repo/release-signing.pem"
git -C "$repo" add release-signing.pem
git -C "$repo" commit -qm "oops"
pin_manifest_to_head "$repo"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "private key material is present in the working tree"
expect "$rc" "$out" 2 "release-signing.pem"

current="COSIGN_KEY_FILE is refused even with no key on disk"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1 COSIGN_KEY_FILE=/tmp/nope.key)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "COSIGN_KEY_FILE is set"
refute "$out" "present in the working tree"

# --- guard 6: the SBOM has to describe this tree ------------------------
current="no SBOM in the tree at all"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
rm "$repo/provenance/sbom.spdx.json"
r="$(run_guards "$repo")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "is not in the tree, so there is no SBOM to attest"

current="the provenance check failing is a refusal"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
stub="$(stub_go "$repo" 1)"
r="$(run_guards "$repo" "PATH=${stub}:${PATH}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "are not what this tree generates"

current="the same stub at exit 0 reaches the seam (control for the pair above)"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
stub="$(stub_go "$repo" 0)"
r="$(run_guards "$repo" "PATH=${stub}:${PATH}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "every guard passed"
refute "$out" "are not what this tree generates"

if [ "$failures" -gt 0 ]; then
  echo "publish-image guards: ${failures} failing assertion(s)" >&2
  exit 1
fi
echo "publish-image guards: ok"

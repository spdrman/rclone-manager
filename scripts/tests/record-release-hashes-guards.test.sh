#!/usr/bin/env bash
# Issue #174, PR #182 review M1: an automated control for the refusals in
# scripts/release/record-release-hashes.sh.
#
# Those refusals are the half of #174's fix that keeps the net empty. The
# net itself (distribution/packaging's release-manifest checks) is tested
# exhaustively; the generator was exercised once by hand at release time,
# after two Docker cross-builds, and its result was recorded in a pull
# request description. Two of its refusals have no downstream net at all:
# a manifest generated from a dirty tree, or with COMMIT hand-set, pins a
# perfectly reachable commit whose binary_sha256 values describe
# different bytes, and nothing downstream can tell.
#
# So this drives the real script, in throwaway `git init` repositories,
# one per refusal, asserting exit code 2 AND the distinct message. The
# exit code alone proves nothing: all five refusals exit 2, so an
# exit-code-only assertion cannot tell "the tree is dirty" from "git
# could not decide", which is exactly the confusion M4 was filed about.
#
# The script stops at the GUARDS_ONLY=1 seam, immediately after the guard
# block and before the first Docker build, so this runs in about a second
# on any machine and needs no Docker daemon.
#
# Positive controls, because every assertion here is a negative one:
#
#   * a clean checkout at a commit on the reachable ref must pass all
#     five guards and reach the seam, or these tests would pass just as
#     happily against a script that refuses everything;
#   * the two ancestry branches are driven through the same `git` stub at
#     two different exit codes, 1 and 128, and each must produce the
#     other's message and not its own. A stub that was not on PATH, or a
#     script that treated every non-zero exit the same, fails that pair.

set -uo pipefail

# git sets these when this runs from the pre-commit hook, and a relative
# GIT_INDEX_FILE resolved inside a throwaway repository is how you get
# "index file open failed". ci-local.sh already unsets them; do it again
# so this is safe to run standalone.
unset GIT_INDEX_FILE GIT_DIR GIT_WORK_TREE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR GIT_PREFIX

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="${REPO_ROOT}/scripts/release/record-release-hashes.sh"
REAL_GIT="$(command -v git)"
WELL_FORMED_UNKNOWN_SHA="0123456789abcdef0123456789abcdef01234567"

failures=0
current=""
tmpdirs=()

# Every case works in its own throwaway git repository, and this removes
# them from an EXIT trap so a case that dies partway through still cleans
# up after itself. The ${tmpdirs+...} expansion is not decoration: under
# set -u an empty array is an unbound variable, and a run that refuses
# before creating anything would otherwise fail inside its own cleanup.
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

# new_repo prints the path of a throwaway repository holding the paths
# the guards look at: core/, apps/, ui/ and container/Dockerfile.
new_repo() {
  local dir
  dir="$(mktemp -d)"
  tmpdirs+=("$dir")
  git -C "$dir" init -q -b main
  git -C "$dir" config user.email t@example.invalid
  git -C "$dir" config user.name t
  mkdir -p "$dir/core" "$dir/apps" "$dir/ui" "$dir/container"
  printf 'package main\n' >"$dir/core/main.go"
  printf 'apps\n' >"$dir/apps/marker"
  printf 'ui\n' >"$dir/ui/marker"
  printf 'FROM scratch\n' >"$dir/container/Dockerfile"
  git -C "$dir" add -A
  git -C "$dir" commit -qm "base"
  echo "$dir"
}

# stub_git writes a `git` onto PATH that answers merge-base
# --is-ancestor with a fixed exit code and forwards everything else to
# the real git. Exit 1 is git saying no; 128 is git saying it could not
# decide, which is what a shallow clone or a missing object produces and
# which no fixture can create on demand.
stub_git() {
  local dir="$1" code="$2"
  mkdir -p "$dir/stubbin"
  cat >"$dir/stubbin/git" <<STUB
#!/usr/bin/env bash
for arg in "\$@"; do
  if [ "\$arg" = "--is-ancestor" ]; then
    echo "fatal: Not a valid commit name (stubbed exit ${code})" >&2
    exit ${code}
  fi
done
exec "${REAL_GIT}" "\$@"
STUB
  chmod +x "$dir/stubbin/git"
  echo "$dir/stubbin"
}

# run_guards runs the real script inside a repository with GUARDS_ONLY=1.
# It prints the exit code on the first line and the combined output
# after it, so a caller reads both from one invocation.
run_guards() {
  local dir="$1"
  shift
  local out rc
  out="$(cd "$dir" && env GUARDS_ONLY=1 "$@" bash "$SCRIPT" 2>&1)"
  rc=$?
  echo "$rc"
  echo "$out"
}

# expect asserts the exit code AND the message. Every refusal in the
# script under test exits with the same code, so a code-only assertion
# cannot tell one refusal from another, and the message is the half that
# says which guard fired. The code is checked first, because when it is
# wrong the script went somewhere else entirely and the message assertion
# would only report the symptom.
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

# refute asserts an absence, so it passes on its own against a run that
# died early. Every use of it sits beside an expect that pins what did
# happen.
refute() {
  local out="$1" unwanted="$2"
  case "$out" in
    *"$unwanted"*) fail "output contains, and must not: ${unwanted}" "$out" ;;
  esac
}

# The two halves of what a guarded run returns: a bash function has one
# output channel, so the exit code rides on the first line and everything
# the script printed follows it.
split_rc() { echo "$1" | head -n1; }
split_out() { echo "$1" | tail -n +2; }

# --- positive control, first, so the refusals below mean something -----
current="a clean checkout at a commit on the reachable ref passes every guard"
repo="$(new_repo)"
r="$(run_guards "$repo" REACHABLE_FROM=main)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "every guard passed"
expect "$rc" "$out" 0 "Would write container/release-manifest.json"

# --- refusal 1: COMMIT names nothing ------------------------------------
current="COMMIT that names no commit here"
repo="$(new_repo)"
r="$(run_guards "$repo" REACHABLE_FROM=main "COMMIT=${WELL_FORMED_UNKNOWN_SHA}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "does not name a commit in this repository"

# --- refusal 2: COMMIT is not HEAD --------------------------------------
current="COMMIT that resolves but is not HEAD"
repo="$(new_repo)"
base="$(git -C "$repo" rev-parse HEAD)"
printf 'second\n' >>"$repo/core/main.go"
git -C "$repo" commit -qam "second"
r="$(run_guards "$repo" REACHABLE_FROM=main "COMMIT=${base}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "is not HEAD"

# --- refusal 3: dirty tree ----------------------------------------------
current="a working tree dirty in a path the image is built from"
repo="$(new_repo)"
printf 'uncommitted\n' >>"$repo/core/main.go"
r="$(run_guards "$repo" REACHABLE_FROM=main)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "the working tree is dirty"

# --- refusal 4: the reachable ref does not resolve ----------------------
current="REACHABLE_FROM that does not resolve"
repo="$(new_repo)"
r="$(run_guards "$repo")" # the default origin/main does not exist here
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "cannot resolve origin/main"

# --- refusal 5: the commit is not on the reachable ref ------------------
current="a commit that is not an ancestor of the reachable ref"
repo="$(new_repo)"
git -C "$repo" checkout -q -b feature
printf 'feature\n' >>"$repo/core/main.go"
git -C "$repo" commit -qam "feature work"
r="$(run_guards "$repo" REACHABLE_FROM=main)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "is not an ancestor of main"
refute "$out" "could not decide"

# --- M4: git could not decide is not git saying no ----------------------
current="git exiting 128 is reported as undecidable, not as a no"
repo="$(new_repo)"
stub="$(stub_git "$repo" 128)"
r="$(run_guards "$repo" REACHABLE_FROM=main "PATH=${stub}:${PATH}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "could not decide"
expect "$rc" "$out" 2 "exited 128"
refute "$out" "is not an ancestor of"

current="the same stub at exit 1 still reports a plain no (control for the pair above)"
repo="$(new_repo)"
stub="$(stub_git "$repo" 1)"
r="$(run_guards "$repo" REACHABLE_FROM=main "PATH=${stub}:${PATH}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "is not an ancestor of main"
refute "$out" "could not decide"

# --- M3: the waiver is loud and does not default to the tracked path ----
current="UNSAFE_LOCAL_BUILD=1 waives the guards but writes somewhere gitignored"
repo="$(new_repo)"
printf 'uncommitted\n' >>"$repo/core/main.go" # would fail the dirty guard
r="$(run_guards "$repo" UNSAFE_LOCAL_BUILD=1 "COMMIT=${WELL_FORMED_UNKNOWN_SHA}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "Would write container/.generated/release-manifest.local.json"
expect "$rc" "$out" 0 "unsafe_local_build"
refute "$out" "Would write container/release-manifest.json"

current="UNSAFE_LOCAL_BUILD=1 still honours an explicit OUT"
repo="$(new_repo)"
r="$(run_guards "$repo" UNSAFE_LOCAL_BUILD=1 "OUT=${repo}/explicit.json")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "Would write ${repo}/explicit.json"

if [ "$failures" -gt 0 ]; then
  echo "record-release-hashes guards: ${failures} failing assertion(s)" >&2
  exit 1
fi
echo "record-release-hashes guards: ok"

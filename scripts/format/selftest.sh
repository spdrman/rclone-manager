#!/usr/bin/env bash
# Positive controls for the formatting gate (issue #417).
#
# Formatting joined this repository's checks because it was missing, and it
# was missing in the way that is hardest to see: two Go files were not
# gofmt-clean, and every gate step stayed green, because `go build`,
# `go vet` and every linter that was enabled are all indifferent to layout.
# A check that was never there and a check that is there and looking are
# the same output, which is the same argument #242 made for the
# compatibility cells and #417 made for the race detector. So the same
# treatment: plant real unformatted code and require each half of the gate
# to go red.
#
# There are two halves, and they are not redundant.
#
#   .golangci.yml's gofmt formatter    covers the five Go modules
#   scripts/format/check-gofmt.sh      covers every tracked .go file
#
# The second exists because golangci-lint is invoked per module and two Go
# files live outside every module and outside go.work
# (scripts/api/gen-bindings.go, scripts/architecture/ownership.go). No
# per-module lint run can see them, and the unformatted one of the pair was
# gen-bindings.go. F2 below is the standing precondition for that claim,
# and F4 is the cell the per-module half cannot make.
#
# The mutations plant NEW files rather than editing existing source, so
# there is no verbatim copy of product code here to drift, and this file
# does not use scripts/lib/selftest-swap.sh. What CAN drift is the
# structural fact F2 pins: the day somebody adds scripts/go.mod, those two
# files stop being outside every module, this script's whole reason changes
# and the comments naming them go stale. That is what --check-anchors
# checks, in the same shape and the same half-second as every other
# anchor, and scripts/selftest/check-anchors.sh runs it.
#
# `bash scripts/format/selftest.sh --check-anchors` is that check alone,
# building nothing.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

root=$(pwd)
check=$root/scripts/format/check-gofmt.sh
config=$root/.golangci.yml

dry_run=0
for arg in "$@"; do
  case "$arg" in
    --check-anchors) dry_run=1 ;;
    -h|--help)
      echo "usage: $0 [--check-anchors]"
      echo
      echo "  --check-anchors  check only the structural precondition this"
      echo "                   corpus rests on, building nothing."
      exit 0
      ;;
    *)
      echo "$0: unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-format-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok()   { echo "  ok:   $1"; pass=$((pass + 1)); }
bad()  { echo "SELFTEST FAIL: $1" >&2; shift; [ $# -gt 0 ] && printf '%s\n' "$1" | sed 's/^/    /' >&2; fail=$((fail + 1)); }

# The two files the other two comments name. Kept here as data so F2 and
# those comments cannot drift apart silently.
OUTSIDE_EVERY_MODULE="scripts/api/gen-bindings.go
scripts/architecture/ownership.go"

# module_root_of <path relative to the repo root> -> the directory holding
# the go.mod that owns it, or "" when no directory above it has one.
module_root_of() {
  local dir
  dir=$(dirname "$1")
  while [ "$dir" != "." ] && [ "$dir" != "/" ]; do
    if [ -f "$dir/go.mod" ]; then
      printf '%s' "$dir"
      return 0
    fi
    dir=$(dirname "$dir")
  done
  [ -f "go.mod" ] && printf '.'
  return 0
}

# unformatted_go <path> writes a Go file that compiles and that gofmt
# disagrees with. Badly spaced rather than mangled on purpose: the point is
# that this file is perfectly valid, builds, vets and lints clean, and is
# only wrong about layout, which is exactly why nothing caught the two real
# ones.
unformatted_go() {
  printf 'package %s\n\n// Sum adds two numbers.\nfunc Sum(a int,b int) int {\n\treturn a+b\n}\n' "$2" >"$1"
}

# synthetic_repo builds a throwaway git work tree shaped like this one: a
# repository root with no go.mod of its own, a Go module in a
# subdirectory, and a scripts/ directory outside every module. Same shape,
# so the cells below exercise check-gofmt.sh's real code path (git ls-files
# inside a git work tree) rather than a second implementation written for
# the test.
synthetic_repo() {
  local dir="$tmp/$1"
  mkdir -p "$dir/core" "$dir/scripts"
  printf 'module stubcore\n\ngo 1.21\n' >"$dir/core/go.mod"
  printf 'package core\n\n// Stub exists so the module has something to build.\nfunc Stub() int { return 1 }\n' >"$dir/core/stub.go"
  printf 'package main\n\nfunc main() {}\n' >"$dir/scripts/tool.go"
  git -C "$dir" init -q
  git -C "$dir" add -A
  printf '%s' "$dir"
}

# ---------------------------------------------------------------- F2 first

echo "==> F2 the two Go files outside every module are still outside every module"
f2_problems=""
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  if [ ! -f "$rel" ]; then
    f2_problems="$f2_problems
  $rel is gone, so the reason check-gofmt.sh exists no longer names it"
    continue
  fi
  owner=$(module_root_of "$rel")
  if [ -n "$owner" ]; then
    f2_problems="$f2_problems
  $rel is now inside the module at $owner, so a per-module golangci-lint run does reach it"
  fi
done <<EOF
$OUTSIDE_EVERY_MODULE
EOF
if [ -z "$f2_problems" ]; then
  ok "both files are outside every module, so only the sweep can reach them"
else
  bad "the structural precondition this corpus rests on has moved:" "$f2_problems
Update scripts/format/check-gofmt.sh's header, .golangci.yml's comment and
OUTSIDE_EVERY_MODULE in this file together, rather than deleting the cell."
fi

if [ "$dry_run" = 1 ]; then
  echo
  if [ "$fail" -eq 0 ]; then
    echo "==> format selftest anchors: ok (1 precondition checked)"
    exit 0
  fi
  echo "==> format selftest anchors: FAILED" >&2
  exit 1
fi

# ---------------------------------------------------------------------- F1

echo
echo "==> F1 negative control: the real tree is clean"
if bash "$check" >"$tmp/out" 2>&1; then
  ok "check-gofmt.sh passes on the real tree, so the cells below mean something"
else
  bad "check-gofmt.sh FAILS on the unmutated tree, so its failures say nothing" "$(cat "$tmp/out")"
fi

# ---------------------------------------------------------------------- F3

echo
echo "==> F3 an unformatted file inside a module is caught"
d=$(synthetic_repo inside-a-module)
unformatted_go "$d/core/bad.go" core
git -C "$d" add -A
if bash "$check" "$d" >"$tmp/out" 2>&1; then
  bad "the sweep PASSED with an unformatted file in a module" "$(cat "$tmp/out")"
elif ! grep -qF 'core/bad.go' "$tmp/out"; then
  bad "the sweep failed but never named the file" "$(cat "$tmp/out")"
else
  ok "caught, and named: core/bad.go"
fi

# ---------------------------------------------------------------------- F4

echo
echo "==> F4 an unformatted file OUTSIDE every module is caught"
d=$(synthetic_repo outside-every-module)
unformatted_go "$d/scripts/bad.go" main
git -C "$d" add -A
# The precondition for this cell, in the fixture rather than in the real
# tree: a scripts/ that turned out to be inside a module would make this
# the same cell as F3 wearing a different name.
if [ -f "$d/scripts/go.mod" ] || [ -f "$d/go.mod" ]; then
  bad "the fixture's scripts/ is inside a module, so F4 is not measuring what it says"
elif bash "$check" "$d" >"$tmp/out" 2>&1; then
  bad "the sweep PASSED with an unformatted file outside every module, which is the one thing golangci-lint cannot see" "$(cat "$tmp/out")"
elif ! grep -qF 'scripts/bad.go' "$tmp/out"; then
  bad "the sweep failed but never named the file" "$(cat "$tmp/out")"
else
  ok "caught, and named: scripts/bad.go, which no per-module lint run reaches"
fi

# ---------------------------------------------------------------------- F5

echo
echo "==> F5 a newly added, uncommitted file is caught, because that is the pre-commit state"
d=$(synthetic_repo staged-not-committed)
unformatted_go "$d/core/added.go" core
git -C "$d" add "core/added.go"
if bash "$check" "$d" >"$tmp/out" 2>&1; then
  bad "the sweep PASSED on a staged file, so the pre-commit hook would let it land" "$(cat "$tmp/out")"
elif ! grep -qF 'core/added.go' "$tmp/out"; then
  bad "the sweep failed but never named the staged file" "$(cat "$tmp/out")"
else
  ok "caught, and named: core/added.go"
fi

# ---------------------------------------------------------------------- F6

echo
echo "==> F6 .golangci.yml's own formatter turns an unformatted module red"
if ! command -v golangci-lint >/dev/null 2>&1; then
  bad "golangci-lint is not on PATH, so the half of this gate that lives in .golangci.yml cannot be measured"
else
  d="$tmp/golangci"
  mkdir -p "$d"
  printf 'module fmtprobe\n\ngo 1.21\n' >"$d/go.mod"
  unformatted_go "$d/bad.go" fmtprobe
  if (cd "$d" && golangci-lint run --config "$config" ./...) >"$tmp/out" 2>&1; then
    bad "golangci-lint PASSED an unformatted file with this repository's own config" "$(cat "$tmp/out")"
  elif ! grep -qF '(gofmt)' "$tmp/out"; then
    bad "golangci-lint failed, but not on formatting, so something else broke" "$(cat "$tmp/out")"
  else
    ok "red, and it says (gofmt)"
  fi
  # And the other direction, which is what stops the cell above from
  # passing against a config that rejects everything.
  gofmt -w "$d/bad.go"
  if (cd "$d" && golangci-lint run --config "$config" ./...) >"$tmp/out" 2>&1; then
    ok "green once the same file is formatted"
  else
    bad "golangci-lint FAILS on a formatted file, so the cell above proves nothing" "$(cat "$tmp/out")"
  fi
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "==> format selftest: ok ($pass controls)"
  exit 0
fi
echo "==> format selftest: $fail failed, $pass passed" >&2
exit 1

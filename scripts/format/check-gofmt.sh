#!/usr/bin/env bash
# Is every Go file in this repository gofmt-clean? (issue #417)
#
# Two of them were not, and the interesting part is not the whitespace. It
# is that nothing anywhere noticed, for as long as those files have
# existed. `go build` and `go vet` are indifferent to layout, none of the
# linters .golangci.yml enabled looks at it, and the gate's own step
# headings say "build, vet, test" and "golangci-lint", so every one of them
# was green on a file no tool had an opinion about. That is this
# repository's recurring shape, arriving through formatting this time: a
# check that is silently not checking.
#
# .golangci.yml now enables the gofmt formatter, which closes it for the
# five Go modules. This script is the other half, and it is not redundant
# with that one. golangci-lint is invoked per module (`cd core && ...`,
# `cd apps/common && ...`), and two Go files in this repository live
# outside every module and outside go.work:
#
#   scripts/api/gen-bindings.go        (run by scripts/api/lib.sh)
#   scripts/architecture/ownership.go  (run by the layer-ownership check)
#
# No per-module lint run has ever been able to see either of them, and the
# unformatted one of the pair was gen-bindings.go. They are compiled, by
# the `go run` that invokes them, so a syntax error would surface; nothing
# else about them is checked by anything. This sweep at least holds them to
# the same formatting as the rest of the tree.
#
# Tracked files only, through `git ls-files`, which is the same set a
# reviewer sees and keeps node_modules/, build output and throwaway
# worktrees out without a list of exclusions to maintain.
#
# Exit code contract, which is all a gate step needs from it:
#
#   0        every tracked Go file is gofmt-clean
#   non-zero at least one is not, and the run printed which
#
# Costs about half a second on the whole repository, which is why it sits
# near the top of the gate next to scripts/selftest/check-anchors.sh
# rather than at minute 20 behind the Go suites.
#
# Takes an optional directory: a git work tree to check instead of this
# one. That is what scripts/format/selftest.sh drives, so the mutation
# controls and the real run go down the same code path rather than the
# controls exercising a second implementation.
set -uo pipefail

root=${1:-}
if [ -n "$root" ]; then
  if [ ! -d "$root" ]; then
    echo "check-gofmt: $root is not a directory" >&2
    exit 2
  fi
  cd "$root" || exit 2
fi

toplevel=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "check-gofmt: ${root:-$PWD} is not inside a git work tree, and this reads the tracked file list" >&2
  exit 2
}
cd "$toplevel" || exit 2

files=$(git ls-files -z '*.go' | xargs -0 gofmt -l 2>/dev/null)

if [ -z "$files" ]; then
  count=$(git ls-files '*.go' | grep -c .)
  echo "OK: all $count tracked Go files are gofmt-clean."
  exit 0
fi

echo "FAIL: these tracked Go files are not gofmt-clean:" >&2
printf '%s\n' "$files" | sed 's/^/    /' >&2
echo "" >&2
echo "    Fix them with:" >&2
printf '%s\n' "$files" | tr '\n' ' ' | sed 's/^/        gofmt -w /' >&2
echo "" >&2
echo "" >&2
echo "    Nothing in this repository looked at formatting until #417, which is how two" >&2
echo "    files stayed unformatted indefinitely while every gate step reported ok." >&2
exit 1

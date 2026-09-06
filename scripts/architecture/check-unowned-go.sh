#!/usr/bin/env bash
# Is every Go file in this repository owned by a module, and if not, is it
# checked anyway? (issue #417)
#
# "Unowned" here means exactly one thing: no directory at or above the file
# contains a go.mod. Two files in this repository are unowned today:
#
#   scripts/api/gen-bindings.go        (run by scripts/api/lib.sh)
#   scripts/architecture/ownership.go  (run by check-layer-ownership.sh)
#
# That is not a defect on its own. They are single-file `package main`
# programs that this repository's own scripts `go run`, and giving them a
# module would put a sixth entry in go.work and a new row in the layer
# manifest to buy very little.
#
# What IS a defect is what follows from it. This gate lints per module
# (`cd core && golangci-lint run ...`, `cd apps/common && ...`) and vets per
# module the same way, so an unowned file is reached by none of it. Both of
# these have therefore never been vetted or linted by anything, ever, in a
# repository whose gate otherwise vets and lints everything. That is how one
# of the pair came to be the only unformatted Go file in the tree
# (scripts/format/check-gofmt.sh is the sweep that found it): nothing was
# looking, so nothing said anything.
#
# So this looks. `go vet` needs no module at all when it is given file
# paths, which is the whole reason this can exist without a scripts/go.mod.
# golangci-lint does need one, so each unowned directory is copied into a
# throwaway module in $TMPDIR and linted there against this repository's own
# .golangci.yml. Both files are standard-library only, so that copy resolves
# offline with no go.sum and no network; GOPROXY=off below makes a file that
# stops being standard-library-only fail immediately and say so, rather than
# hanging on a fetch.
#
# Paths in the output are rewritten back to the real ones, because a
# complaint about /var/folders/.../T/rclone-manager-unowned.XXXX/bad.go is a
# complaint nobody can act on.
#
# Exit code contract, which is all a gate step needs from it:
#
#   0        every unowned Go file passes go vet and golangci-lint
#            (including the case where there are none)
#   non-zero at least one does not, and the run printed which and why
#
# About a second. It runs near the top of the gate with the gofmt sweep
# rather than with the other architecture checks, for the reason that
# applies to all of them: a check this cheap should not be reached at
# minute 20, and a Go file nobody checks is worth hearing about before the
# Docker-backed suites start.
#
# Takes an optional directory: a git work tree to check instead of this
# one, which is what scripts/architecture/selftest.sh drives so the
# mutation controls and the real run go down the same code path.
set -uo pipefail

root_arg=${1:-}
if [ -n "$root_arg" ]; then
  if [ ! -d "$root_arg" ]; then
    echo "check-unowned-go: $root_arg is not a directory" >&2
    exit 2
  fi
  cd "$root_arg" || exit 2
fi

toplevel=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "check-unowned-go: ${root_arg:-$PWD} is not inside a git work tree, and this reads the tracked file list" >&2
  exit 2
}
cd "$toplevel" || exit 2

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-unowned.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# module_owner <path> -> the directory holding the go.mod that owns it, or
# "" when nothing at or above it has one.
module_owner() {
  local dir
  dir=$(dirname "$1")
  while :; do
    if [ -f "$dir/go.mod" ]; then
      printf '%s' "$dir"
      return 0
    fi
    [ "$dir" = "." ] && break
    dir=$(dirname "$dir")
  done
  return 0
}

# Every tracked .go file no module owns, grouped by the directory it sits
# in, because files in one directory are one package and vetting them
# separately would report every reference between them as undefined.
unowned_dirs=""
unowned_count=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  [ -n "$(module_owner "$f")" ] && continue
  unowned_count=$((unowned_count + 1))
  d=$(dirname "$f")
  case "
$unowned_dirs" in
    *"
$d"*) ;;
    *) unowned_dirs="$unowned_dirs
$d" ;;
  esac
done <<EOF
$(git ls-files '*.go')
EOF

if [ "$unowned_count" -eq 0 ]; then
  echo "OK: every tracked Go file is inside a module, so the per-module vet and lint steps reach all of them."
  exit 0
fi

goversion=$(go env GOVERSION 2>/dev/null | sed 's/^go//')
[ -n "$goversion" ] || goversion=1.21

status=0
checked=0

for d in $unowned_dirs; do
  files=$(git ls-files "$d/*.go" | while IFS= read -r f; do
    [ "$(dirname "$f")" = "$d" ] && [ -z "$(module_owner "$f")" ] && printf '%s\n' "$f"
  done)
  [ -n "$files" ] || continue

  # go vet, straight at the file paths. No module, no go.work, no copy.
  if ! go vet $files >"$tmp/vet.out" 2>&1; then
    echo "FAIL: go vet found something in $d, which no module owns and nothing else vets:" >&2
    sed 's/^/    /' "$tmp/vet.out" >&2
    echo "" >&2
    status=1
  fi

  # golangci-lint needs a module, so it gets a throwaway one.
  modkey="mod-$(printf '%s' "$d" | tr '/' '-')"
  mod="$tmp/$modkey"
  mkdir -p "$mod"
  printf 'module unownedcheck\n\ngo %s\n' "$goversion" >"$mod/go.mod"
  # shellcheck disable=SC2086
  cp $files "$mod/" || { echo "check-unowned-go: could not copy $d into a throwaway module" >&2; exit 2; }
  if ! (cd "$mod" && GOFLAGS=-mod=mod GOPROXY=off golangci-lint run --config "$toplevel/.golangci.yml" ./...) >"$tmp/lint.out" 2>&1; then
    echo "FAIL: golangci-lint found something in $d, which no module owns and nothing else lints:" >&2
    # golangci-lint reports paths relative to wherever it was invoked
    # from, which from a throwaway module under $TMPDIR is a long climb of
    # "../" back out. Anything ending in the throwaway module's own
    # directory name is that prefix, whether it arrived absolute or
    # relative, so strip it and put the real directory back.
    sed -E "s|[^[:space:]]*/$modkey/|$d/|g; s|^|    |" "$tmp/lint.out" >&2
    echo "" >&2
    if grep -q 'GOPROXY=off\|cannot find module\|missing go.sum' "$tmp/lint.out"; then
      echo "    That looks like a dependency this check cannot resolve. It copies each unowned" >&2
      echo "    directory into a throwaway module and lints it offline, which works because" >&2
      echo "    every unowned file here is standard-library only. One that is not needs a real" >&2
      echo "    module rather than this." >&2
      echo "" >&2
    fi
    status=1
  fi

  checked=$((checked + 1))
done

if [ "$status" -ne 0 ]; then
  echo "    These files are outside every module and outside go.work, so no per-module vet or" >&2
  echo "    lint step in this gate can see them. This check is the only thing that does." >&2
  exit 1
fi

echo "OK: $unowned_count Go file(s) in $checked director(ies) are owned by no module, and all of them pass go vet and golangci-lint."
exit 0

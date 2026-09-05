#!/usr/bin/env bash
# Is every Go change between a ref and this work tree a comment change and
# nothing else? (issue #526)
#
# #526's fix moves prose. It touches around thirty files and changes no
# behaviour, and "changes no behaviour" is the kind of claim a reviewer
# cannot check by reading, because the diffs are large, the moved blocks
# are long, and a real edit hidden in the middle of one would look like
# more of the same.
#
# So it gets proven instead. Every changed Go file is scanned at both
# revisions with go/scanner in its default mode, which does not emit
# comments at all, and the two token streams have to be identical. That is
# the compiler's own view of the file with the prose taken out: same
# tokens, same program.
#
# On its own that would be too weak in one specific way, and #526's fix
# walks straight into it. A `//go:build` line is a comment to the scanner
# and a build constraint to the toolchain, so a header moved from above it
# to below it changes which platforms compile the file while the token
# stream stays byte-identical. core/service/lock_unix.go and
# core/service/lock_other.go are exactly that shape and both were in the
# fix. So core/cmd/docguard prints every //go: directive and // +build line
# alongside the tokens, with which side of the package clause it sits on,
# and this compares those too.
#
# A file that exists at only one of the two revisions has no token stream
# to compare, so this cannot speak for it either way. It is named in the
# output rather than passed over silently: the reader has to be able to see
# the edge of what was proven, or a branch that added a whole file of new
# behaviour alongside the comment moves would read as "comments-only" on
# the strength of a check that never looked at it.
#
# Exit code contract:
#
#   0        every Go file present at BOTH revisions differs only in
#            comments; any added or deleted file is named in the output
#   non-zero at least one differs in tokens or in directives, and the run
#            printed which
#
# usage: check-comments-only.sh <ref> [work-tree] [--only <path>]...
#
#   ref        what to compare against, usually origin/main
#   work-tree  a git work tree to check instead of this one
#   --only     restrict the comparison to one path, repeatable. That is
#              what scripts/docs/selftest.sh drives: its controls plant one
#              mutation in one real file, and a whole-tree comparison would
#              also be answering for whatever else the person running the
#              gate happens to have uncommitted.
set -uo pipefail

ref=""
root=""
only=""
want_only=0
for arg in "$@"; do
  if [ "$want_only" = 1 ]; then
    only="$only$arg
"
    want_only=0
    continue
  fi
  case "$arg" in
    --only) want_only=1 ;;
    -h|--help)
      echo "usage: $0 <ref> [work-tree] [--only <path>]..."
      exit 0
      ;;
    -*)
      echo "$0: unknown argument: $arg" >&2
      exit 2
      ;;
    *)
      if [ -z "$ref" ]; then ref=$arg; else root=$arg; fi
      ;;
  esac
done

if [ "$want_only" = 1 ]; then
  echo "$0: --only needs a path" >&2
  exit 2
fi

if [ -z "$ref" ]; then
  echo "usage: $0 <ref> [work-tree] [--only <path>]..." >&2
  exit 2
fi

if [ -n "$root" ]; then
  if [ ! -d "$root" ]; then
    echo "check-comments-only: $root is not a directory" >&2
    exit 2
  fi
  cd "$root" || exit 2
fi

toplevel=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "check-comments-only: ${root:-$PWD} is not inside a git work tree" >&2
  exit 2
}
cd "$toplevel" || exit 2

if ! git rev-parse --verify --quiet "$ref^{commit}" >/dev/null; then
  echo "check-comments-only: $ref is not a commit in this repository" >&2
  exit 2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-comments-only.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# Built once. `go run` per file would be a compile per file, and this is
# meant to be cheap enough to run on every branch.
guard=$tmp/docguard
if ! (cd core && go build -o "$guard" ./cmd/docguard) >"$tmp/build" 2>&1; then
  echo "check-comments-only: could not build core/cmd/docguard" >&2
  sed 's/^/    /' "$tmp/build" >&2
  exit 2
fi

if [ -n "$only" ]; then
  # git diff still decides what actually changed, so --only narrows the
  # question and never answers it: a path that is named and unchanged is
  # reported as such rather than counted as agreement.
  changed=$(printf '%s' "$only" | grep -c . >/dev/null; git diff --name-only "$ref" -- $(printf '%s' "$only" | tr '\n' ' '))
  named=$(printf '%s' "$only" | grep -c .)
  if [ -z "$changed" ]; then
    echo "check-comments-only: none of the $named path(s) named with --only differ from $ref," >&2
    echo "                     so there is nothing here to prove anything about." >&2
    exit 2
  fi
else
  changed=$(git diff --name-only "$ref" -- '*.go')
fi

same=0
added=0
removed=0
problems=""
outside=""

while IFS= read -r path; do
  [ -n "$path" ] || continue
  before=$tmp/before.go
  after=$tmp/after.go
  if ! git show "$ref:$path" >"$before" 2>/dev/null; then
    added=$((added + 1))
    outside="$outside
      $path (added, so it has no earlier token stream to differ from)"
    continue
  fi
  if [ ! -f "$path" ]; then
    removed=$((removed + 1))
    outside="$outside
      $path (deleted, so it has no current token stream)"
    continue
  fi
  cp "$path" "$after"
  if ! "$guard" tokens "$before" >"$tmp/before.tok" 2>"$tmp/err"; then
    problems="$problems
  $path could not be scanned at $ref: $(cat "$tmp/err")"
    continue
  fi
  if ! "$guard" tokens "$after" >"$tmp/after.tok" 2>"$tmp/err"; then
    problems="$problems
  $path could not be scanned in the work tree: $(cat "$tmp/err")"
    continue
  fi
  if diff -q "$tmp/before.tok" "$tmp/after.tok" >/dev/null 2>&1; then
    same=$((same + 1))
    continue
  fi
  problems="$problems
  $path differs in more than comments:
$(diff "$tmp/before.tok" "$tmp/after.tok" | head -20 | sed 's/^/      /')"
done <<CHANGED
$changed
CHANGED

total=$(printf '%s\n' "$changed" | grep -c .)

compared=$((total - added - removed))

if [ -z "$problems" ]; then
  echo "OK: all $compared of $total changed Go files present at both revisions have the same"
  echo "    token stream and the same //go: directives as at $ref, so those changes are"
  echo "    comments and nothing else."
  if [ -n "$outside" ]; then
    echo ""
    echo "    $added added and $removed deleted, which this cannot speak for either way:"
    printf '%s\n' "$outside"
  fi
  exit 0
fi

echo "FAIL: the change against $ref is not comments-only." >&2
printf '%s\n' "$problems" >&2
echo "" >&2
echo "    $same of $compared comparable files differ only in comments." >&2
if [ -n "$outside" ]; then
  echo "    $added added and $removed deleted, which this cannot speak for either way:" >&2
  printf '%s\n' "$outside" >&2
fi
exit 1

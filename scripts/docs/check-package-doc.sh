#!/usr/bin/env bash
# Does every package's `go doc` overview still say what it is recorded as
# saying, and is it still assembled from the same files? (issue #526)
#
# A comment sitting immediately above `package` is the package doc. Not
# "looks like it", is it: go/doc collects every one of them across a
# package and concatenates them in sorted file order. Six documentation
# lanes put a per-file opener there, so `go doc ./core/service` opened with
# "This file is the operator's activity feed", activity.go introducing
# itself, with the real overview several topics down. core/internal/state
# went from one overview to eleven stacked openers the same way.
#
# Every gate step was green throughout. `go build` and `go vet` do not care
# where a comment sits, none of the linters .golangci.yml enables looks at
# package documentation, and the campaign's own reviewers were reading
# diffs of individual files, where an opener above `package` looks exactly
# like an opener below the imports. The overview only exists once the files
# are put together, and nothing was putting them together.
#
# So this does, and records two things per package rather than one:
#
#   the digest of the assembled doc   catches text that changed
#   the files that carry a comment    catches a file that started carrying
#     adjacent to `package`             one, which is the defect itself
#
# The second is the durable half. A promoted opener always shows up as a
# new carrier, in a line a reviewer can read, before anybody has to think
# about what the digest means.
#
# Size is deliberately not one of them. go/doc concatenates in sorted
# order, so a promoted opener can land BEFORE the real overview and lead
# it, which reads as a replacement and can leave the header shorter than it
# was. A lane in this campaign measured header length with
# `awk '/^(func|type|var|const) /{exit}'`, the awk exited on the lane's own
# prose where "type" had wrapped to column 0, and it reported a 283-line
# header as 9 lines. The value here comes from go/doc's Package.Doc, the
# same value `go doc` prints, through core/cmd/docguard.
#
# Exit code contract, which is all a gate step needs from it:
#
#   0        every package matches scripts/docs/package-doc.baseline
#   non-zero at least one does not, and the run printed which and how
#
# A deliberate documentation change is meant to fail this once. Read what
# it says, confirm the new carrier list is the one you meant, and record it:
#
#   bash scripts/docs/check-package-doc.sh --update
#
# --against <ref> asks the other question, and it is the one #526 was
# actually closed with: is every package overview in this tree the same TEXT
# it was at some earlier commit? It extracts that commit, assembles both
# sides through the same go/doc call, and prints a unified diff per package
# that moved. The baseline above records digests, which tell a reviewer that
# something changed; this shows them what.
#
# Takes an optional directory: a git work tree to check instead of this
# one. That is what scripts/docs/selftest.sh drives, so the mutation
# controls and the real run go down the same code path rather than the
# controls exercising a second implementation.
set -uo pipefail

update=0
against=""
want_against=0
root=""
for arg in "$@"; do
  if [ "$want_against" = 1 ]; then
    against=$arg
    want_against=0
    continue
  fi
  case "$arg" in
    --update) update=1 ;;
    --against) want_against=1 ;;
    -h|--help)
      echo "usage: $0 [--update | --against <ref>] [work-tree]"
      echo
      echo "  --update         record the tree as it is now, instead of checking it"
      echo "  --against <ref>  compare every package overview's TEXT against that"
      echo "                   commit, and print a diff per package that moved"
      exit 0
      ;;
    -*)
      echo "$0: unknown argument: $arg" >&2
      exit 2
      ;;
    *) root=$arg ;;
  esac
done

if [ -n "$root" ]; then
  if [ ! -d "$root" ]; then
    echo "check-package-doc: $root is not a directory" >&2
    exit 2
  fi
  cd "$root" || exit 2
fi

toplevel=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "check-package-doc: ${root:-$PWD} is not inside a git work tree" >&2
  exit 2
}
cd "$toplevel" || exit 2

if [ "$want_against" = 1 ]; then
  echo "$0: --against needs a commit" >&2
  exit 2
fi

if [ -n "$against" ] && [ "$update" = 1 ]; then
  echo "$0: --update and --against ask different questions; pick one" >&2
  exit 2
fi

baseline=scripts/docs/package-doc.baseline

# --against is its own path from here. It compares two trees to each other
# rather than either of them to the recorded digests, so it shares the
# reader (core/cmd/docguard) and nothing else.
if [ -n "$against" ]; then
  if ! git rev-parse --verify --quiet "$against^{commit}" >/dev/null; then
    echo "check-package-doc: $against is not a commit in this repository" >&2
    exit 2
  fi
  atmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-package-doc-against.XXXXXX")
  trap 'rm -rf "$atmp"' EXIT
  mkdir -p "$atmp/tree"
  if ! git archive "$against" | tar -x -C "$atmp/tree"; then
    echo "check-package-doc: could not extract $against" >&2
    exit 2
  fi
  if ! (cd core && go build -o "$atmp/docguard" ./cmd/docguard) >"$atmp/build" 2>&1; then
    echo "check-package-doc: could not build core/cmd/docguard" >&2
    sed 's/^/    /' "$atmp/build" >&2
    exit 2
  fi
  # The same binary reads both trees, so a difference is a difference in the
  # documentation and never a difference in how it was read.
  "$atmp/docguard" -full packages "$atmp/tree" >"$atmp/before" || exit 2
  "$atmp/docguard" -full packages . >"$atmp/after" || exit 2
  if diff -q "$atmp/before" "$atmp/after" >/dev/null 2>&1; then
    echo "OK: every package overview is byte-identical to $against."
    exit 0
  fi
  echo "These package overviews differ from $against:"
  echo ""
  diff -u "$atmp/before" "$atmp/after" | sed 's/^/    /'
  echo ""
  echo "    Each hunk is inside a === <path> <package> block, which is that package's"
  echo "    assembled go/doc overview and nothing else."
  exit 1
fi

nowfile=$(mktemp "${TMPDIR:-/tmp}/rclone-manager-package-doc.XXXXXX")
trap 'rm -f "$nowfile"' EXIT

(cd core && go run ./cmd/docguard packages ..) >"$nowfile"
status=$?
if [ "$status" -ne 0 ]; then
  echo "check-package-doc: docguard could not read the tree" >&2
  exit 2
fi

if [ "$update" = 1 ]; then
  cp "$nowfile" "$baseline"
  echo "OK: recorded $(grep -c . "$baseline") packages in $baseline."
  exit 0
fi

if [ ! -f "$baseline" ]; then
  echo "check-package-doc: $baseline is missing. Record it with --update." >&2
  exit 2
fi

if diff -q "$baseline" "$nowfile" >/dev/null 2>&1; then
  echo "OK: all $(grep -c . "$baseline") package overviews match $baseline, carriers included."
  exit 0
fi

echo "FAIL: a package overview is not what $baseline records." >&2
echo "" >&2

# Report per package rather than as a raw diff of digest lines, because a
# reviewer holding a changed sha256 has been told nothing. Naming the file
# that started or stopped carrying a comment adjacent to `package` is the
# whole finding in one line.
python3 - "$baseline" "$nowfile" >&2 <<'PY' 
import sys

baseline_path, now_path = sys.argv[1:3]
now = {}
for line in open(now_path).read().splitlines():
    if not line.strip():
        continue
    path, name, digest, carriers = line.split("\t")
    now[(path, name)] = (digest, carriers.split(","))

was = {}
for line in open(baseline_path).read().splitlines():
    if not line.strip():
        continue
    path, name, digest, carriers = line.split("\t")
    was[(path, name)] = (digest, carriers.split(","))

for key in sorted(set(was) | set(now)):
    path, name = key
    if was.get(key) == now.get(key):
        continue
    if key not in was:
        print("    %s (%s) is a package the baseline has never seen." % (path, name))
        print("        carriers: %s" % ", ".join(now[key][1]))
        continue
    if key not in now:
        print("    %s (%s) is in the baseline and not in the tree." % (path, name))
        continue
    old_digest, old_carriers = was[key]
    new_digest, new_carriers = now[key]
    print("    %s (%s)" % (path, name))
    gained = [f for f in new_carriers if f not in old_carriers]
    lost = [f for f in old_carriers if f not in new_carriers]
    if gained:
        print("        these files now carry a comment adjacent to `package`, so their")
        print("        text is part of the package overview: %s" % ", ".join(gained))
    if lost:
        print("        these files no longer carry one: %s" % ", ".join(lost))
    if old_digest != new_digest and not gained and not lost:
        print("        the same files carry it, and the text changed.")
    print("        see it with: go doc ./%s" % path)
PY

echo "" >&2
echo "    A file opener belongs BELOW the imports, where it is a file comment and a" >&2
echo "    reader still meets it on opening the file. Only the package's overview" >&2
echo "    belongs above \`package\`." >&2
echo "" >&2
echo "    If the change is deliberate, record it:" >&2
echo "        bash scripts/docs/check-package-doc.sh --update" >&2
exit 1

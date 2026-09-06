#!/usr/bin/env bash
# Completeness guard for the three-layer manifest (issue #165).
#
# Every other check in this directory reads scripts/architecture/layers.conf
# to decide which layer a path is in. A manifest with a hole in it makes all
# of them fail open: an unclassified directory is checked by nothing, and
# nothing says so. Phase 4 already learned this lesson on the conformance
# matrix, where omitting a capability had to fail rather than shrink the
# matrix; this is the same guard for the layer split.
#
# It fails when:
#   - any tracked file is not classified by some manifest entry;
#   - any manifest entry names a path that does not exist, or one whose shape
#     leaves the repository (absolute, or carrying a ".." segment);
#   - any entry uses a layer or kind outside the declared vocabulary;
#   - a kind is set on a non-distribution layer, or missing on a
#     distribution one;
#   - any of the three product layers is empty, so the split cannot pass
#     vacuously by classifying everything as infrastructure.
#
# Static, no worktree: it inspects the working tree so a contributor sees
# the failure before committing, unlike the deletion proofs next to it,
# which necessarily run against HEAD.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

manifest=$(arch::manifest)
if [ ! -f "$manifest" ]; then
  echo "FAIL: $manifest does not exist, so no layer is declared at all." >&2
  exit 1
fi

fail=0
# note records the failure and keeps going, so one run reports every
# violation rather than the first one. That matters more here than it
# looks: these findings arrive in batches (a manifest edit that misses
# several files, a new module that imports across two layers at once), and
# a check that stops at the first turns one fix into several runs.
note() { echo "$@" >&2; fail=1; }

# ---- vocabulary and shape -------------------------------------------------

while read -r layer kind path; do
  case "$layer" in
    core|platform|distribution|infrastructure) ;;
    *) note "FAIL: unknown layer \"$layer\" for $path (expected core, platform, distribution or infrastructure)." ;;
  esac

  if [ "$layer" = "distribution" ]; then
    case "$kind" in
      adapter|canonical) ;;
      *) note "FAIL: distribution entry $path has kind \"$kind\"; a distribution path must be \"adapter\" or \"canonical\", because verify-core-without-distribution.sh deletes exactly the adapter ones." ;;
    esac
  else
    if [ "$kind" != "-" ]; then
      note "FAIL: $layer entry $path has kind \"$kind\"; kind is meaningful only on the distribution layer, so it must be \"-\" here."
    fi
  fi

  # Shape before existence. An entry that escapes the repository can satisfy
  # the existence test below by pointing at something real outside the tree,
  # and it is invisible to the completeness guard further down because
  # arch::classify only ever matches entries against tracked files. It is not
  # invisible to verify-core-without-distribution.sh, which deletes every
  # entry marked "distribution adapter".
  if problem=$(arch::manifest_path_problem "$path"); then
    note "FAIL: manifest entry \"$path\" $problem. Every check here joins a manifest path onto a directory, and verify-core-without-distribution.sh hands the adapter ones to rm -rf, so an entry that leaves the repository is a delete nobody reviewed."
    continue
  fi

  if [ ! -e "$path" ]; then
    note "FAIL: manifest entry $path does not exist. A stale entry silently narrows every check that reads this file."
  fi
done < <(arch::manifest_rows)

# ---- no layer may be empty ------------------------------------------------

for layer in core platform distribution; do
  if [ -z "$(arch::layer_paths "$layer")" ]; then
    note "FAIL: the $layer layer has no paths. Three layers that are not all populated is two layers with a comment."
  fi
done

if [ -z "$(arch::layer_paths distribution adapter)" ]; then
  note "FAIL: no distribution path is marked \"adapter\", so verify-core-without-distribution.sh would delete nothing and pass vacuously."
fi

# ---- every tracked file is classified -------------------------------------

# git ls-files rather than a filesystem walk: the question is what the
# repository contains, not what happens to be lying in the working tree
# (a node_modules, a build output, a scratch file).
unclassified=""
count=0
while IFS= read -r file; do
  count=$((count + 1))
  if ! arch::classify "$file" >/dev/null; then
    unclassified="${unclassified}
  ${file}"
  fi
done < <(git ls-files)

if [ -n "$unclassified" ]; then
  note "FAIL: tracked file(s) belong to no layer. Classify them in $manifest:$unclassified"
fi

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "  The layers and what each owns: docs/architecture/layers.md" >&2
  exit 1
fi

echo "OK: all $count tracked files are classified by $manifest, and every entry exists."

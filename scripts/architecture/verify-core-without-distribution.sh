#!/usr/bin/env bash
# EPIC B #81's dependency rule, the literal half (issue #165):
#
#   "The core and generic application must build and pass tests if the
#    distribution adapter tree is deleted."
#
# verify-core-without-apps.sh already proves the narrower, older claim that
# core/ alone stands with all of apps/ gone. This proves the Phase 6 one,
# which is different in two ways: it deletes the distribution ADAPTER tree
# specifically, wherever those paths happen to live today, and it requires
# the generic application to survive too, not only core.
#
# It deletes and rebuilds rather than scanning imports, for the same reason
# lib.sh gives: a static scan can be fooled by a path built at runtime, a
# go:embed, or a test fixture reaching across the boundary. An actual
# deletion cannot.
#
# What it deletes is not hard-coded here. It is every path
# scripts/architecture/layers.conf marks "distribution adapter", so adding
# an adapter without declaring it fails check-layer-manifest.sh rather than
# silently escaping this proof.
#
# What it deliberately does NOT delete is the distribution paths marked
# "canonical": container/ (the canonical image and Compose runtime) and
# distribution/packaging (the shared metadata and the conformance suite).
# #81's claim is about the adapter tree, not about the canonical runtime the
# adapters wrap, and the generic application legitimately depends on that
# runtime existing. distribution/packaging's own tests are not run here for
# the mirror-image reason: they check the adapters, so with the adapters
# deleted they SHOULD fail, and running them would prove nothing about core.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

wt=""
cleanup() { [ -n "$wt" ] && arch::cleanup_worktree "$wt"; }
trap cleanup EXIT

arch::make_worktree wt

if [ ! -d "$wt/core" ]; then
  echo "FAIL: core/ module does not exist yet." >&2
  exit 1
fi

if [ ! -f "$wt/$(arch::manifest)" ]; then
  echo "FAIL: $(arch::manifest) does not exist at HEAD." >&2
  echo "  This check builds a throwaway worktree of HEAD, so a manifest that exists only in your working tree is invisible to it." >&2
  echo "  Commit the manifest first, then re-run." >&2
  exit 1
fi

adapters=$(arch::layer_paths distribution adapter "$wt")
if [ -z "$adapters" ]; then
  echo "FAIL: scripts/architecture/layers.conf marks no path \"distribution adapter\", so this check would delete nothing and pass without proving anything." >&2
  exit 1
fi

echo "==> deleting the distribution adapter tree in a throwaway worktree"

# The operand of the only rm -rf this stack adds comes out of an editable
# text file, so it gets checked twice before anything is removed: once on its
# shape (arch::manifest_path_problem, shared with check-layer-manifest.sh so
# a contributor sees the refusal before committing), and once on its outcome,
# by resolving the path and requiring the result to still be inside the
# throwaway worktree. The shape test alone would trust the input; symlinks
# mean the input is not the whole story. "${wt:?}" guarded the variable that
# cannot realistically be empty and nothing guarded the one a typo controls.
wt_real=$(cd "$wt" && pwd -P)

count=0
skipped=0
while IFS= read -r path; do
  [ -n "$path" ] || continue

  if problem=$(arch::manifest_path_problem "$path"); then
    echo "FAIL: $(arch::manifest) marks \"$path\" as a distribution adapter, but that entry $problem." >&2
    echo "  This check deletes every adapter path, so it refuses to run against a manifest entry it cannot vouch for." >&2
    exit 1
  fi

  target="$wt_real/$path"
  if [ ! -e "$target" ]; then
    # check-layer-manifest.sh is what reports a stale entry, against the
    # working tree, where a contributor can act on it. Here the entry is
    # simply not present at HEAD, so there is nothing to delete and nothing
    # this check can usefully say about it.
    echo "    (not present at HEAD, nothing to delete: $path)"
    skipped=$((skipped + 1))
    continue
  fi

  parent=$(cd "$(dirname "$target")" 2>/dev/null && pwd -P) || parent=""
  if [ -z "$parent" ]; then
    echo "FAIL: cannot resolve the parent directory of adapter path \"$path\" inside the throwaway worktree." >&2
    exit 1
  fi
  resolved="$parent/$(basename "$target")"
  case "$resolved" in
    "$wt_real"/*) ;;
    *)
      echo "FAIL: adapter path \"$path\" resolves to $resolved, which is outside the throwaway worktree $wt_real." >&2
      echo "  Refusing to delete it. A manifest entry may only name something inside the repository." >&2
      exit 1
      ;;
  esac

  echo "    rm -rf $path"
  rm -rf -- "$resolved"
  count=$((count + 1))
done <<EOF
$adapters
EOF
echo "    ($count adapter path(s) deleted, $skipped not present at HEAD)"

if [ "$count" -eq 0 ]; then
  echo "FAIL: no adapter path was actually deleted, so this check proved nothing." >&2
  echo "  Every path $(arch::manifest) marks \"distribution adapter\" is missing at HEAD." >&2
  exit 1
fi

# GOWORK=off throughout: the repo-root go.work lists sibling modules for
# local development convenience, and apps/synology's go.mod is one of the
# files just deleted. Without this, `go build` would walk up to go.work and
# fail on the missing module, which is a workspace-tooling artifact rather
# than the thing this check exists to prove.
for module in core apps/common apps/generic; do
  echo "==> go build ./... ($module, with the distribution adapter tree deleted)"
  (cd "$wt/$module" && GOWORK=off go build ./...)

  echo "==> go vet ./... ($module, with the distribution adapter tree deleted)"
  (cd "$wt/$module" && GOWORK=off go vet ./...)
done

# core/'s own test suite is deliberately not re-run here.
# verify-core-without-apps.sh already runs it with ALL of apps/ deleted,
# which is a strictly stronger deletion than this one (every adapter path
# under apps/ is gone there too), so running it again would cost the
# Docker-backed crash matrix and the SFTP integration suite a second time
# to prove something already proven. What is genuinely new in this check is
# the two application modules, so those do run their tests.
for module in apps/common apps/generic; do
  echo "==> go test ./... ($module, with the distribution adapter tree deleted)"
  (cd "$wt/$module" && GOWORK=off go test ./...)
done

echo "OK: core/, apps/common and apps/generic build and pass their tests with the distribution adapter tree deleted entirely."

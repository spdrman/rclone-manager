# Shared helpers for the EPIC-B repository-structure checks (issue #106,
# Work Package 1.1, spec §7.1 "Dependency enforcement").
#
# Each check in this directory proves a boundary claim ("core/ builds with
# apps/ deleted", "ui/shared builds with the provider SDK dirs gone", "a
# single provider app is removable") by actually checking out the current
# commit into a throwaway git worktree, deleting the directory the claim
# says shouldn't matter, and running the real build/test command there. A
# static import scan can be fooled by a path built at runtime or a test
# fixture reaching across the boundary; an actual deletion cannot.
#
# Not meant to be executed directly — sourced by the verify-*.sh scripts
# next to it.

set -euo pipefail

# arch::warn_if_dirty
# Every check in this file proves a property of `HEAD` (the last commit),
# not of the working tree: it builds its throwaway worktree from `HEAD`,
# so an uncommitted change — including one that violates the very boundary
# being checked — is invisible to it. Warn loudly rather than let a
# developer running this locally against a dirty tree mistake "OK" for
# "my mid-edit state is safe to commit."
arch::warn_if_dirty() {
  if [ -n "$(git status --porcelain)" ]; then
    echo "WARNING: working tree has uncommitted changes; this check only proves the property against the last commit (HEAD), not your uncommitted edits." >&2
  fi
}

# arch::make_worktree <out_var>
# Creates a detached worktree of HEAD in a fresh temp directory and writes
# its path into the caller's named variable. Pair with arch::cleanup_worktree.
arch::make_worktree() {
  local __outvar=$1
  local dir
  arch::warn_if_dirty
  dir=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-arch-check.XXXXXX")
  # --detach: we are proving a property of the current tree, not making a
  # branch anyone will commit on.
  git worktree add --quiet --detach "$dir" HEAD >/dev/null
  printf -v "$__outvar" '%s' "$dir"
}

# arch::cleanup_worktree <dir>
# Removes a worktree created by arch::make_worktree, including the case
# where the check's own `rm -rf` inside it already deleted files git
# expects to find.
arch::cleanup_worktree() {
  local dir=$1
  git worktree remove --force "$dir" >/dev/null 2>&1 || rm -rf "$dir"
  git worktree prune >/dev/null 2>&1 || true
}

# --------------------------------------------------------------------------
# The three-layer manifest (issue #165). See scripts/architecture/layers.conf
# for the format and docs/architecture/layers.md for the prose.
#
# Before Phase 6 the repository had two boundaries, core and "a provider
# app", and the checks in this directory hard-coded the provider list. The
# manifest replaces that with one declared classification every check reads,
# so adding a platform or moving a packaging artifact updates one file
# instead of four scripts that can drift apart.
# --------------------------------------------------------------------------

# arch::manifest prints the manifest's path, relative to the repository root.
arch::manifest() { printf '%s' "scripts/architecture/layers.conf"; }

# arch::manifest_rows prints the manifest with comments and blank lines
# stripped, one "<layer> <kind> <path>" row per line, space-separated.
arch::manifest_rows() {
  local root=${1:-.}
  sed 's/#.*//' "$root/$(arch::manifest)" |
    awk 'NF >= 3 { print $1, $2, $3 }'
}

# arch::layer_paths <layer> [<kind>] [<root>]
# Prints the manifest paths in a layer, optionally narrowed to one kind.
# A kind of "" or "any" means every kind.
arch::layer_paths() {
  local want_layer=$1 want_kind=${2:-any} root=${3:-.}
  arch::manifest_rows "$root" | awk -v l="$want_layer" -v k="$want_kind" \
    '$1 == l && (k == "any" || k == "" || $2 == k) { print $3 }'
}

# arch::classify <path> [<root>]
# Prints "<layer> <kind>" for a repository-relative path, choosing the
# LONGEST matching manifest path so a specific entry beats the directory it
# sits in. Prints nothing and returns 1 when no entry matches, which is what
# check-layer-manifest.sh turns into a completeness failure.
arch::classify() {
  local path=$1 root=${2:-.}
  arch::manifest_rows "$root" | awk -v p="$path" '
    {
      entry = $3
      if (p == entry || index(p, entry "/") == 1) {
        if (length(entry) > best_len) { best_len = length(entry); best = $1 " " $2 }
      }
    }
    END { if (best_len > 0) { print best; exit 0 } exit 1 }
  '
}

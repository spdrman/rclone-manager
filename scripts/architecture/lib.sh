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

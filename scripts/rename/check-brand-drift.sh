#!/usr/bin/env bash
# The brand-drift guard (#794): no NEW old-brand identifier enters this tree.
#
# Issue #794 renamed the runtime identifiers this project inherited from its
# two previous names -- rclone-manager's `RM_` environment variables and
# backup-manager's `bm_` cookies -- to `BACKUPD_` / `backupd_`. That rename
# is a one-off edit; this file is the part that makes it stay done. Without
# it the next `RM_SOMETHING` somebody adds by copying a neighbouring line
# is invisible until the third rename, which is how this repository got two
# brands' worth of prefixes in the first place.
#
# WHAT IT LOOKS FOR: the creation of an identifier carrying an old brand
# prefix, in any tracked source file:
#
#   RM_[A-Z]     rclone-manager environment variables
#   BM_[A-Z]     backup-manager environment variables (none exist today)
#   bm_[a-z]     backup-manager cookies, container and helper names
#   rbm_[a-z]    the never-used third variant, blocked before it exists
#
# Each pattern is anchored on a non-identifier character to its left, so
# `CONFIRM_DELETE` is not an `RM_` variable and rclone's `ibm_signer.go` is
# not a `bm_` cookie. `rbm_` needs a pattern of its own for exactly that
# reason: the `r` in front of it is an identifier character, so the `bm_`
# pattern cannot see it.
#
# WHAT IT ALLOWS: three lists, and they mean three different things.
#
#   ALIASES     the deprecated aliases #794 deliberately KEEPS for one
#               release so an upgrade does not break. Allowed anywhere in
#               the tree, because an alias has to be minted, read, tested
#               and documented, and pinning it to a file list would turn
#               every one of those into a gate failure.
#   PENDING     identifiers that are still on `main` and are being deleted
#               by #794 itself, with no alias. Allowed anywhere, and
#               expected to disappear: when one does, this script says so
#               and the entry should be deleted.
#   PREEXISTING old-brand identifiers that are out of #794's scope, pinned
#               as token+path pairs. Pinning the path is the point: the
#               occurrences that exist stay green, and the same name
#               appearing in a new file is a creation and goes red.
#
# A list entry that matches nothing left in the tree is reported and does
# NOT fail the run: the three rename branches of #794 land by deleting
# occurrences, and a guard that goes red the moment the thing it guards is
# fixed is a guard nobody keeps. Deleting the reported line is the fix.
#
# Shell and `git grep`, deliberately, where most of scripts/ is Python
# now (EPIC I, #672): the whole check is one pattern sweep over tracked
# files plus three fixed lists, it has to run in the pre-commit path, and
# `git grep` is the one tool that already knows what "tracked source" means.
#
# Registered in scripts/ci-local.sh (the gate .husky/pre-commit runs) and in
# .github/workflows/ci.yml. scripts/rename/selftest.sh is the proof it can
# still go red; scripts/ci-local.sh runs that too.
#
# Exit code contract, which is all a gate step needs:
#
#   0        every occurrence found is on a list
#   1        at least one is not, and the run printed file, line and name
#
# NOTE: the repository root comes from `git rev-parse --show-toplevel` of
# the CURRENT WORKING DIRECTORY, the same as the scripts/architecture
# checks, which is what lets the self-test point this at a throwaway tree.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# The kept deprecated aliases (#794). Exactly the identifiers the rename
# branches keep working for one release, and nothing else. Each one is
# primary nowhere: the preferred name is BACKUPD_DEBUG / backupd_session /
# backupd_csrf, and these stay only so an in-place upgrade keeps reading the
# operator's existing environment and keeps existing browser sessions valid.
#
# When the deprecation window closes, the alias and its line here go
# together, and this script reports the line as unused the moment the alias
# is gone.
aliases="$(
  cat <<'EOF'
RM_DEBUG
bm_session
bm_csrf
EOF
)"

# Still on `main`, deleted (not aliased) by #794's own branches.
#
# RM_SIGNAL_EXIT_CHILD_MODE is test-internal: it exists so
# core/internal/transport/rclone/signalexit_test.go can re-enter its own
# test binary as a child process. Nothing outside that file has ever set
# it, so it is a straight rename to BACKUPD_SIGNAL_EXIT_CHILD_MODE with no
# alias and no deprecation window. It is here rather than in the alias list
# above because those two states are not the same claim, and the difference
# is load bearing: an alias is kept on purpose, this is in transit. Delete
# this line once the env branch of #794 has merged -- the run that first
# sees it gone will say so.
pending="$(
  cat <<'EOF'
RM_SIGNAL_EXIT_CHILD_MODE
EOF
)"

# Out of #794's scope, pinned to the files they already live in.
#
# Two groups, and neither is this rename's to fix:
#
#   * The RM_* names are the ENVIRONMENT CONTRACT of another repository.
#     scripts/bdtools/e2e/run_tests_repo_gate.py and
#     scripts/e2e/three-machine-web-ui.sh set them for the suites in
#     backupdproject/backupd-tests, pinned at scripts/e2e/tests-repo.pin;
#     that repository reads them by these names. Renaming them here alone
#     breaks the e2e gate, so it is a two-repository change with a pin bump
#     in the middle, not a line in this one. The .help.txt golden, the
#     README and the pin's own ledger prose carry the same names because
#     they describe that interface.
#   * bm_stopped and bm_routed are helper METHOD names in the two-machine
#     backup proof (scripts/bdtools/e2e/two_machine_backup.py): "run the
#     CLI on the stopped machine" and "...through the routed one". They are
#     internal to that file and rename cleanly, but they are not a runtime
#     identifier anybody upgrades across, so they belong to whoever next
#     touches that proof rather than to #794.
#
# Every line is <token> <path>, one occurrence-site per line. Adding one of
# these names to a file that is not listed is a creation, and this guard
# treats it as one.
preexisting="$(
  cat <<'EOF'
RM_ADMIN_PASSWORD scripts/e2e/three-machine-web-ui.sh
RM_ADMIN_PASSWORD scripts/e2e/web-ui-smoke.mjs
RM_ADMIN_PASSWORD scripts/tests/testdata/three-machine-web-ui.help.txt
RM_ADMIN_USERNAME scripts/e2e/three-machine-web-ui.sh
RM_ADMIN_USERNAME scripts/e2e/web-ui-smoke.mjs
RM_ADMIN_USERNAME scripts/tests/testdata/three-machine-web-ui.help.txt
RM_ARTIFACTS_DIR scripts/e2e/three-machine-web-ui.sh
RM_ARTIFACTS_DIR scripts/e2e/web-ui-smoke.mjs
RM_ARTIFACTS_DIR scripts/tests/testdata/three-machine-web-ui.help.txt
RM_BACKUP_SET scripts/e2e/three-machine-web-ui.sh
RM_BACKUP_SET scripts/e2e/web-ui-smoke.mjs
RM_BACKUP_SET scripts/tests/testdata/three-machine-web-ui.help.txt
RM_BASE_URL scripts/bdtools/e2e/run_tests_repo_gate.py
RM_BASE_URL scripts/e2e/tests-repo.pin
RM_BASE_URL scripts/e2e/three-machine-web-ui.sh
RM_BASE_URL scripts/e2e/web-ui-smoke.mjs
RM_BASE_URL scripts/tests/testdata/three-machine-web-ui.help.txt
RM_BINARY scripts/bdtools/e2e/run_tests_repo_gate.py
RM_CHROMIUM_NO_SANDBOX scripts/e2e/three-machine-web-ui.sh
RM_CHROMIUM_NO_SANDBOX scripts/e2e/web-ui-smoke.mjs
RM_COMMIT scripts/bdtools/e2e/run_tests_repo_gate.py
RM_FRONT_PROXY_TLS scripts/e2e/three-machine-web-ui.sh
RM_IGNORE_HTTPS scripts/e2e/three-machine-web-ui.sh
RM_IGNORE_HTTPS scripts/e2e/web-ui-smoke.mjs
RM_MODE scripts/bdtools/e2e/run_tests_repo_gate.py
RM_MODE scripts/e2e/tests-repo.pin
RM_PRODUCT_IMAGE scripts/e2e/three-machine-web-ui.sh
RM_SEED_CYCLES scripts/e2e/README.md
RM_SEED_CYCLES scripts/e2e/three-machine-web-ui.sh
RM_SEED_CYCLES scripts/tests/testdata/three-machine-web-ui.help.txt
RM_SOURCE_DIR scripts/bdtools/e2e/run_tests_repo_gate.py
RM_UI_DIR .github/workflows/nightly-e2e.yml
RM_UI_DIR scripts/bdtools/e2e/run_tests_repo_gate.py
RM_UI_DIR scripts/e2e/tests-repo.pin
bm_routed scripts/bdtools/e2e/two_machine_backup.py
bm_stopped scripts/bdtools/e2e/two_machine_backup.py
EOF
)"

# Not source, so not this guard's business. Every one of these either
# RECORDS history (which is the one place an old name is supposed to keep
# appearing) or is machine-written from something this guard does scan.
#
#   CHANGELOG.md                      the record of the rename itself
#   **/go.sum                         module hashes, and rclone's own
#                                     ibm_* backends live in there
#   **/node_modules/**                vendored third-party JS
#   provenance/**                     released-artifact records: SBOM,
#                                     checksums, third-party licences
#   core/apicontract/contract.gen.go  generated from api/v1/openapi.json,
#   ui/shared/src/api/generated/**    which IS scanned, so a cookie name in
#                                     the contract is still caught -- at
#                                     the source rather than twice more in
#                                     its DO-NOT-EDIT copies
#
# And the last two are this file and its self-test, which are the only
# files in the tree whose JOB is to write these names down: the allowlist
# above is a list of old-brand identifiers, and the self-test plants them on
# purpose. Scanning either would report the guard's own contents as drift.
# Named file by file rather than as scripts/rename/** so that a third file
# added in this directory is scanned like anything else.
excluded_paths=(
  ':!CHANGELOG.md'
  ':!**/go.sum'
  ':!**/node_modules/**'
  ':!provenance/**'
  ':!core/apicontract/contract.gen.go'
  ':!ui/shared/src/api/generated/**'
  ':!scripts/rename/check-brand-drift.sh'
  ':!scripts/rename/selftest.sh'
)

# The left anchor both patterns share: start of line, or a character that
# cannot be part of an identifier. POSIX ERE has no \b (git grep would need
# --perl-regexp, which is a build-time option this script will not depend
# on), and this is the thing \b would have been for.
boundary='(^|[^A-Za-z0-9_])'
scan_re="$boundary(RM_[A-Z]|BM_[A-Z]|bm_[a-z]|rbm_[a-z])"
token_re="$boundary(RM_[A-Z][A-Za-z0-9_]*|BM_[A-Z][A-Za-z0-9_]*|bm_[a-z][A-Za-z0-9_]*|rbm_[a-z][A-Za-z0-9_]*)"

# -I so a binary file is never scanned, -n because a finding has to name a
# line somebody can open. `|| true` because git grep exits 1 for "no
# matches", which is a clean tree and not an error.
hits="$(git grep -nIE "$scan_re" -- . "${excluded_paths[@]}" || true)"

listed() {
  printf '%s\n' "$2" | grep -Fxq -- "$1"
}

violations=""
found_aliases=""
found_pending=""
found_preexisting=""
allowed_count=0

while IFS= read -r hit; do
  [ -n "$hit" ] || continue

  path="${hit%%:*}"
  rest="${hit#*:}"
  lineno="${rest%%:*}"
  text="${rest#*:}"

  # One line can carry several distinct names (a compose file mapping two
  # variables, a test table naming both cookies), and the same name twice;
  # sort -u makes the report one finding per name per line.
  while IFS= read -r token; do
    [ -n "$token" ] || continue

    if listed "$token" "$aliases"; then
      found_aliases="$found_aliases$token
"
      allowed_count=$((allowed_count + 1))
    elif listed "$token" "$pending"; then
      found_pending="$found_pending$token
"
      allowed_count=$((allowed_count + 1))
    elif listed "$token $path" "$preexisting"; then
      found_preexisting="$found_preexisting$token $path
"
      allowed_count=$((allowed_count + 1))
    else
      violations="$violations  $path:$lineno: $token
"
    fi
  done < <(printf '%s\n' "$text" |
    grep -oE "$token_re" |
    sed -E 's/^[^A-Za-z0-9_]//' |
    sort -u)
done < <(printf '%s\n' "$hits")

# The list entries nothing matched any more. Reported, never fatal: see the
# header. Printed before the verdict so a red run does not bury them.
stale=""
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! listed "$entry" "$found_aliases"; then
    stale="$stale  alias      $entry
"
  fi
done < <(printf '%s\n' "$aliases")

while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! listed "$entry" "$found_pending"; then
    stale="$stale  pending    $entry
"
  fi
done < <(printf '%s\n' "$pending")

while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! listed "$entry" "$found_preexisting"; then
    stale="$stale  pre-existing  $entry
"
  fi
done < <(printf '%s\n' "$preexisting")

if [ -n "$stale" ]; then
  echo "check-brand-drift: these allowlist entries no longer match anything in the tree:"
  printf '%s' "$stale"
  echo "check-brand-drift: that identifier is gone, so delete the line above from scripts/rename/check-brand-drift.sh. Not a failure."
fi

if [ -n "$violations" ]; then
  echo "check-brand-drift: FAILED: old-brand identifier created outside the allowlist (#794):" >&2
  printf '%s' "$violations" >&2
  cat >&2 <<'EOF'
check-brand-drift: this project has been renamed twice. RM_ is
  rclone-manager's prefix and bm_ is backup-manager's; both were replaced by
  BACKUPD_ / backupd_ in #794, and a new identifier must use the new prefix.
  Name it BACKUPD_<THING> (environment) or backupd_<thing> (cookie).
  If the name above is not new -- a file moved, or an old-brand identifier
  this rename does not own -- add it to the pre-existing list in
  scripts/rename/check-brand-drift.sh, with the reason, in the same shape as
  the entries already there.
EOF
  exit 1
fi

echo "check-brand-drift: ok ($allowed_count allowlisted occurrence(s), no new RM_/BM_/bm_/rbm_ identifier)"

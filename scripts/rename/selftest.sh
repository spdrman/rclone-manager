#!/usr/bin/env bash
# Mutation self-test for the brand-drift guard (#794).
#
# The guard (scripts/rename/check-brand-drift.sh) is green on this tree, and
# a check that is green on the only tree anybody runs it against has proven
# nothing: the pattern could be misanchored, the allowlist could be swallowing
# everything, or `git grep` could be looking at no files at all. So this
# plants the drift the guard exists to catch, in a throwaway git repository
# per case, and requires it to go red -- and plants the things it must NOT
# catch (CONFIRM_DELETE, rclone's ibm_signer.go, the kept deprecated aliases)
# and requires it to stay green.
#
# Throwaway repositories rather than mutant copies of this tree, which is
# where this differs from scripts/architecture/selftest.sh and the five other
# anchored selftests: those plant a violation INTO a verbatim copy of product
# source, so their plants have to be anchored and
# scripts/selftest/check-anchors.sh watches them for drift. Nothing here
# copies product source. Every case writes the three or four lines it is
# about, so there is no anchor to drift and nothing for that aggregator to
# check -- the guard's own allowlist is the only thing coupled to the real
# tree, and the guard reports its own dead entries.
#
# Every case runs even after one has failed; the tally at the end is the
# result, and one run names every broken control rather than the first.
#
# Run directly (`bash scripts/rename/selftest.sh`) or let the gate run it:
# scripts/ci-local.sh invokes it next to the guard itself.
set -uo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
guard="$repo_root/scripts/rename/check-brand-drift.sh"

checks=0
failures=0

pass() {
  checks=$((checks + 1))
  echo "  ok   $1"
}

fail() {
  checks=$((checks + 1))
  failures=$((failures + 1))
  echo "  FAIL $1"
  if [ -n "${2:-}" ]; then
    printf '%s\n' "$2" | sed 's/^/         /'
  fi
}

# A throwaway repository with one commit, so `git grep` has tracked files to
# look at. Nothing here shares state with any other case.
new_repo() {
  tree="$(mktemp -d)"
  (
    cd "$tree"
    git init -q
    mkdir -p core
    printf 'package core\n\nconst Level = "BACKUPD_DEBUG"\n' >core/level.go
    git add -A
    git -c user.email=selftest@example.invalid -c user.name=selftest \
      commit -q -m "base"
  )
  echo "$tree"
}

# run_guard <tree> -> exit status in $status, combined output in $out
run_guard() {
  out="$(cd "$1" && bash "$guard" 2>&1)"
  status=$?
}

# green <label> <tree>: the guard must accept this tree.
green() {
  run_guard "$2"
  if [ "$status" -eq 0 ]; then
    pass "$1"
  else
    fail "$1 (expected exit 0, got $status)" "$out"
  fi
  rm -rf "$2"
}

# red <label> <tree> <substring the report must name>
red() {
  run_guard "$2"
  if [ "$status" -eq 0 ]; then
    fail "$1 (the guard accepted it)" "$out"
  elif ! printf '%s' "$out" | grep -Fq -- "$3"; then
    fail "$1 (went red, but never named '$3')" "$out"
  else
    pass "$1"
  fi
  rm -rf "$2"
}

# commit <tree> <path> <content>: a tracked file, because the guard scans
# what git tracks and nothing else.
commit() {
  tree="$1"
  mkdir -p "$tree/$(dirname "$2")"
  printf '%s\n' "$3" >"$tree/$2"
  (
    cd "$tree"
    git add -A
    git -c user.email=selftest@example.invalid -c user.name=selftest \
      commit -q -m "case"
  )
}

echo "==> brand-drift guard: mutation self-test (#794)"

# The control. A tree with nothing old-brand in it at all must be green, and
# every allowlist entry in the guard is dead here, which is the other half of
# the control: dead entries are REPORTED and do not fail the run, because
# #794's own branches land by deleting the occurrences they name.
tree="$(new_repo)"
run_guard "$tree"
if [ "$status" -ne 0 ]; then
  fail "control: a tree with no old-brand identifier is green (exit $status)" "$out"
elif ! printf '%s' "$out" | grep -Fq "no longer match anything"; then
  fail "control: dead allowlist entries are reported" "$out"
else
  pass "control: clean tree green, dead allowlist entries reported not fatal"
fi
rm -rf "$tree"

# The four prefixes, one case each. This is the whole point of the guard: a
# new identifier carrying a name this project no longer has.
tree="$(new_repo)"
commit "$tree" core/env.go 'package core

const Debug = "RM_NEW_THING"'
red "a new RM_ environment variable goes red" "$tree" "RM_NEW_THING"

tree="$(new_repo)"
commit "$tree" core/env.go 'package core

const Debug = "BM_NEW_THING"'
red "a new BM_ environment variable goes red" "$tree" "BM_NEW_THING"

tree="$(new_repo)"
commit "$tree" core/cookie.go 'package core

const Name = "bm_new_cookie"'
red "a new bm_ cookie goes red" "$tree" "bm_new_cookie"

# `rbm_` is behind an identifier character, so the bm_ pattern cannot see it
# and it needs one of its own. This is the case that proves it has one.
tree="$(new_repo)"
commit "$tree" core/cookie.go 'package core

const Name = "rbm_new_cookie"'
red "a new rbm_ identifier goes red" "$tree" "rbm_new_cookie"

# The report has to be openable. A guard that says "something somewhere" is
# a guard somebody disables.
tree="$(new_repo)"
commit "$tree" apps/thing/env.go 'package thing

const A = "RM_LOCATED"'
red "the report names file and line" "$tree" "apps/thing/env.go:3:"

# A commit is not the only state the gate runs in: .husky/pre-commit runs it
# with the change in the INDEX and nothing committed. `git add -N` is that
# state, and a guard that could not see it would pass every commit it was
# asked about.
tree="$(new_repo)"
mkdir -p "$tree/core"
printf 'package core\n\nconst A = "RM_STAGED_ONLY"\n' >"$tree/core/staged.go"
(cd "$tree" && git add -N core/staged.go)
red "a staged-but-uncommitted creation goes red" "$tree" "RM_STAGED_ONLY"

# The kept deprecated aliases (#794), which are allowed ANYWHERE rather than
# pinned to a file list: an alias has to be minted, read, tested and
# documented, and every one of those is a new file sooner or later.
tree="$(new_repo)"
commit "$tree" apps/common/auth/compat_test.go 'package auth

// RM_DEBUG is the deprecated alias of BACKUPD_DEBUG.
const legacyEnv = "RM_DEBUG"
const legacySession = "bm_session"
const legacyCSRF = "bm_csrf"'
green "the three kept aliases stay green in a file that did not exist" "$tree"

# Anchoring. Every one of these contains the guard pattern as a SUBSTRING and
# none of them is an old-brand identifier: the left anchor is the only thing
# standing between this guard and a wall of false positives that gets it
# deleted. ibm_signer.go is not hypothetical -- it is rclone's own S3 backend,
# named in distribution/packaging/compliance.json on this tree.
tree="$(new_repo)"
commit "$tree" core/anchor.go 'package core

const Confirm = "CONFIRM_DELETE"
const Form = "FORM_BASE_URL"
const Ids = "PLATFORM_IDS"
const Vendor = "backend/s3/ibm_signer.go"
const Alarm = "ALARM_STATE"
const Key = "rm-debug"'
green "substring lookalikes stay green (CONFIRM_, FORM_, ibm_signer, rm-debug)" "$tree"

# The new prefix, which is what a violation is supposed to become.
tree="$(new_repo)"
commit "$tree" core/new.go 'package core

const Debug = "BACKUPD_DEBUG"
const Session = "backupd_session"'
green "the replacement BACKUPD_/backupd_ names are green" "$tree"

# The out-of-scope names are pinned to their files on purpose, and this is
# the half of that decision that does work: RM_BASE_URL is the pinned
# environment contract of backupdproject/backupd-tests, and it is still a
# creation when it turns up somewhere new.
tree="$(new_repo)"
commit "$tree" core/service/copy.go 'package service

const Base = "RM_BASE_URL"'
red "a pre-existing name in a file it is not pinned to goes red" "$tree" "RM_BASE_URL"

# The excluded paths. A changelog that records the rename, a released-artifact
# record and a DO-NOT-EDIT binding are the places an old name is SUPPOSED to
# keep appearing, and the binding is checked at its source (api/v1/openapi.json)
# instead.
tree="$(new_repo)"
commit "$tree" CHANGELOG.md '- renamed RM_EXCLUDED_ONE to BACKUPD_EXCLUDED_ONE'
commit "$tree" provenance/sbom.spdx.json '{"note": "RM_EXCLUDED_TWO"}'
commit "$tree" core/apicontract/contract.gen.go 'package apicontract

// Code generated by gen-bindings.go. DO NOT EDIT.
const Cookie = "bm_excluded_three"'
commit "$tree" ui/shared/src/api/generated/contract.ts 'export const COOKIE = "bm_excluded_four";'
green "history records and generated bindings are out of scope" "$tree"

# And the same names in a file that is NOT excluded, so the case above is
# measuring the exclusion rather than the pattern failing to match its own
# content.
tree="$(new_repo)"
commit "$tree" docs/notes.md '- renamed RM_EXCLUDED_ONE to BACKUPD_EXCLUDED_ONE'
red "the same text outside an excluded path goes red" "$tree" "RM_EXCLUDED_ONE"

# The guard excludes ITSELF and this file, because both write these names
# down for a living. Two cases, because the exclusion is named file by file
# rather than by directory, and the second one is what makes that decision
# worth anything.
tree="$(new_repo)"
commit "$tree" scripts/rename/check-brand-drift.sh '# RM_SELF_LISTED bm_self_listed'
commit "$tree" scripts/rename/selftest.sh '# RM_SELF_PLANTED bm_self_planted'
green "the guard and its self-test are not scanned for their own contents" "$tree"

tree="$(new_repo)"
commit "$tree" scripts/rename/other.sh '# RM_NEIGHBOUR'
red "a third file in scripts/rename is scanned like anything else" "$tree" "RM_NEIGHBOUR"

echo "==> brand-drift guard self-test: $checks checks, $failures failure(s)"
[ "$failures" -eq 0 ] || exit 1

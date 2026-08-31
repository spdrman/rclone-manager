#!/usr/bin/env sh
# Full local mirror of .github/workflows/ci.yml and rclone-upgrade-gate.yml,
# run entirely on this machine. GitHub Actions no longer auto-triggers on
# this repo (see the "on:" blocks in .github/workflows/*.yml, all switched
# to workflow_dispatch-only) — this script is the actual gate now. The
# pre-commit hook runs it on every commit; `--admin` merges rely on it
# having been green, not on any GitHub-side check.
#
# Comprehensive on purpose, job-for-job with ci.yml, which means it is NOT
# fast: the full core/ test suite (including the Docker-backed crash matrix
# and SFTP integration tests), two cross-compiles, three separate frontend
# installs/builds, and the dependency-rules worktree-deletion proofs. Needs
# a running Docker daemon for the full run, and now says so instead of
# quietly reporting on suites that skipped themselves.
#
# Set CI_LOCAL_FAST=1 for a quick iteration loop. It skips core/'s
# ./tests/... (the crash matrix and the SFTP integration tests), both
# cross-compiles, the ui/shared and upk-proof production builds, the
# apps/common/tests conformance suite, the structure proofs and this gate's
# own self-test. It does NOT skip apps/generic, whose own tests bring a
# compose stack up, so a FAST run is not a Docker-free run. A FAST run
# always ends INCOMPLETE; never rely on it before a merge.
#
# The JS workspaces (ui/shared, apps/common/tests, and
# apps/ugos/frontend/upk-proof where it exists) need their dependencies
# installed before this script can check them. node_modules/ is gitignored,
# so a fresh clone or a new `git worktree` has none, and until issue #160 a
# full run quietly skipped every check that needed them and still finished
# with "ci-local: ok". It no longer does. A full run now refuses to start
# until every JS workspace that is in the tree is installed, and names the
# command that fixes each one. Set CI_LOCAL_SKIP_JS=1 to leave them out on
# purpose: that run, like a FAST one, ends with INCOMPLETE instead of ok,
# because it is not merge evidence.
#
# The Docker daemon is the same story with a bigger blast radius: with it
# down, the crash matrix, the SFTP integration suite and the whole
# apps/generic/tests/dockercli package call t.Skip, go test still exits 0,
# and nothing would reach the ledger. A full run refuses to start without
# the daemon; CI_LOCAL_SKIP_DOCKER=1 is the out-loud opt-out and ends
# INCOMPLETE.
#
# Three outcomes, three exit statuses, so a wrapper does not have to parse
# prose: 0 for "ci-local: ok", 3 for "ci-local: INCOMPLETE", and whatever
# failed for "ci-local: FAILED".
#
# A workspace that is not in the tree at all is a different thing from an
# uninstalled one, and is never a failure. apps/ugos/backend and
# apps/ugos/frontend/upk-proof are optional components that are not in this
# tree today; when a component is absent its checks are inapplicable, not
# skipped, and the run can still legitimately be ok.

set -e

export PATH="/opt/homebrew/bin:$PATH"

# Running as a git hook (pre-commit) means git has GIT_INDEX_FILE (a path
# relative to the repo root, e.g. ".git/index"), GIT_DIR, GIT_WORK_TREE, etc.
# set in the environment for the in-progress commit. The dependency-rules
# scripts below do `git worktree add` from inside a subshell that `cd`s
# elsewhere first, and a relative GIT_INDEX_FILE resolved against the wrong
# cwd is exactly how you get "fatal: .git/index: index file open failed:
# Not a directory" — a real failure this script hit and had to trace back to
# here. Unset them so every git command in this script (and anything it
# shells out to) resolves the repository fresh, the same as it would run
# standalone outside a hook.
unset GIT_INDEX_FILE GIT_DIR GIT_WORK_TREE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR GIT_PREFIX

FAST="${CI_LOCAL_FAST:-0}"

# Resolved from this script's own location, not the working directory. Every
# other path here is cwd-relative because .husky/pre-commit always invokes
# this from the repo root, but the one file that decides whether a run may
# call itself ok should not depend on that holding.
. "$(cd "$(dirname "$0")" && pwd)/lib/ci-local-gate.sh"

# Every exit from here on carries a verdict line. Without this, a run that
# died under `set -e` printed no "==> ci-local: ..." marker at all, so the
# one outcome a reader most needs to grep for was the one with no marker.
trap 'gate_exit_marker $?' EXIT

if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "==> golangci-lint not found. Install it (brew install golangci-lint) and re-run." >&2
  exit 1
fi

# Captured once, from repo root (where this script is always invoked from,
# per .husky/pre-commit), so every module below can reference the one
# project-wide .golangci.yml with an absolute path instead of working out
# its own relative depth back to the repo root.
REPO_ROOT="$(pwd)"

# Before anything slow: a capability this gate reports on but does not have
# is a hole in the gate, not a footnote. Refuse here rather than after twenty
# minutes of Docker-backed Go tests.
gate_require_js_deps ui/shared apps/common/tests apps/ugos/frontend/upk-proof

if [ "$FAST" != "1" ]; then
  gate_require_docker
fi

if [ "$FAST" = "1" ]; then
  gate_note_skip "core/ ./tests/... (the Docker-backed crash matrix and the SFTP integration tests), the cross-compiles, the upk-proof and ui/shared production builds, the apps/common/tests cross-provider conformance suite, the repository-structure dependency rules and this gate's own self-test (CI_LOCAL_FAST=1)"
fi

gate_step "core/ go build"
(cd core && GOWORK=off go build ./...)

gate_step "core/ go vet"
(cd core && GOWORK=off go vet ./...)

gate_step "core/ golangci-lint"
(cd core && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

if [ "$FAST" = "1" ]; then
  gate_step "core/ go test ./internal/... (CI_LOCAL_FAST=1: skipping ./tests/... Docker suites)"
  (cd core && GOWORK=off go test ./internal/...)
else
  gate_step "core/ go test ./... (full suite, including Docker-backed crash matrix + SFTP integration)"
  (cd core && GOWORK=off go test ./...)
fi

gate_step "apps/common go build, vet, test"
(cd apps/common && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

gate_step "apps/common golangci-lint"
(cd apps/common && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

if [ -f apps/generic/go.mod ]; then
  gate_step "apps/generic go build, vet, test"
  (cd apps/generic && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

  gate_step "apps/generic golangci-lint"
  (cd apps/generic && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)
fi

if [ -f apps/synology/go.mod ]; then
  gate_step "apps/synology go build, vet, test"
  (cd apps/synology && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

  gate_step "apps/synology golangci-lint"
  (cd apps/synology && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)
fi

if [ -f apps/ugos/backend/go.mod ]; then
  gate_step "apps/ugos/backend go build, vet, test"
  (cd apps/ugos/backend && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

  gate_step "apps/ugos/backend golangci-lint"
  (cd apps/ugos/backend && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

  if [ "$FAST" != "1" ]; then
    gate_step "apps/ugos/backend cross-compile linux/amd64"
    (cd apps/ugos/backend && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./...)
  fi
fi

if [ "$FAST" != "1" ]; then
  case "$(gate_workspace_state apps/ugos/frontend/upk-proof)" in
    installed)
      gate_step "apps/ugos/frontend/upk-proof typecheck, build"
      (cd apps/ugos/frontend/upk-proof && npm run --silent build)
      ;;
    uninstalled)
      # Only reachable under CI_LOCAL_SKIP_JS=1; the preflight refuses the
      # run otherwise. Ledgered either way, so the final line cannot say ok.
      gate_note_skip "apps/ugos/frontend/upk-proof typecheck and build ($(gate_install_hint apps/ugos/frontend/upk-proof))"
      ;;
  esac
fi

if [ "$FAST" != "1" ]; then
  gate_step "UGREEN cross-compile (amd64)"
  (cd core && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOWORK=off go build -o /dev/null ./...)

  gate_step "UGREEN cross-compile (arm64)"
  (cd core && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOWORK=off go build -o /dev/null ./...)
fi

case "$(gate_workspace_state ui/shared)" in
  installed)
    gate_step "ui/shared typecheck"
    (cd ui/shared && npm run --silent lint)

    gate_step "ui/shared typecheck every provider"
    (cd ui/shared && npm run --silent typecheck:providers)

    gate_step "ui/shared eslint"
    (cd ui/shared && npm run --silent eslint)

    gate_step "ui/shared tests"
    (cd ui/shared && npm test --silent)

    if [ "$FAST" != "1" ]; then
      gate_step "ui/shared build"
      (cd ui/shared && npm run --silent build)
    fi
    ;;
  uninstalled)
    # Reachable under CI_LOCAL_FAST=1 or CI_LOCAL_SKIP_JS=1 only.
    gate_note_skip "ui/shared lint, typecheck:providers, eslint, the vitest suite and the production build ($(gate_install_hint ui/shared))"
    ;;
esac

if [ "$FAST" != "1" ]; then
  case "$(gate_workspace_state apps/common/tests)" in
    installed)
      gate_step "apps/common/tests typecheck"
      (cd apps/common/tests && npm run --silent lint)

      gate_step "apps/common/tests eslint"
      (cd apps/common/tests && npm run --silent eslint)

      gate_step "cross-provider conformance suite (apps/common/tests)"
      (cd apps/common/tests && npm test --silent)
      ;;
    uninstalled)
      # The site nobody noticed: this is the cross-provider conformance
      # suite, and it was skipped on every Phase 3 gate run.
      gate_note_skip "apps/common/tests lint, eslint and the cross-provider conformance suite ($(gate_install_hint apps/common/tests))"
      ;;
  esac
fi

if [ "$FAST" != "1" ]; then
  gate_step "release-manifest generator guards (#174)"
  bash scripts/tests/record-release-hashes-guards.test.sh

  # The self-test runs this very script against synthetic checkouts, so
  # without a marker the recursion terminates only by whatever the fixture
  # happens to lack. CI_LOCAL_SELFTEST makes that an enforced invariant
  # instead, and a nested run says out loud that it stopped.
  if [ "${CI_LOCAL_SELFTEST:-0}" = "1" ]; then
    echo "==> gate self-test: already inside one, not recursing (CI_LOCAL_SELFTEST=1)"
    gate_note_skip "this gate's own self-test, because this run is itself nested inside one (CI_LOCAL_SELFTEST=1)"
  else
    gate_step "gate self-test (scripts/tests/ci-local-gate.test.sh)"
    CI_LOCAL_SELFTEST=1 bash scripts/tests/ci-local-gate.test.sh
  fi

  gate_step "repository-structure dependency rules (§7.1)"
  bash scripts/architecture/check-core-dependency-rule.sh
  bash scripts/architecture/verify-core-without-apps.sh
  bash scripts/architecture/verify-ui-shared-without-provider-sdks.sh
  bash scripts/architecture/verify-ugos-removable.sh
fi

# rclone-upgrade-gate.yml's own point was never to fully automate FR-2, only
# to enforce compilation/tests (already covered above) and print a reminder
# for the one step that's inherently manual: a human reading the upstream
# release notes before approving. Keep printing that reminder locally too.
if git diff --cached --name-only 2>/dev/null | grep -qE '^core/go\.(mod|sum)$'; then
  echo "==> core/go.mod or core/go.sum changed: docs/rclone-upgrade.md (FR-2) requires reading the rclone release notes between the old and new pinned version before this merges. This script cannot verify that happened."
fi

# Never a bare echo: "ok" has to be earned by an empty skip ledger.
gate_summary

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
# a running Docker daemon for the full run.
#
# Set CI_LOCAL_FAST=1 to skip the slow Docker-backed suites, cross-compiles,
# and full frontend build for a quick iteration loop. Never rely on FAST
# mode before a merge — run this unset (or =0) at least once first.

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

if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "==> golangci-lint not found. Install it (brew install golangci-lint) and re-run." >&2
  exit 1
fi

# Captured once, from repo root (where this script is always invoked from,
# per .husky/pre-commit), so every module below can reference the one
# project-wide .golangci.yml with an absolute path instead of working out
# its own relative depth back to the repo root.
REPO_ROOT="$(pwd)"

echo "==> core/ go build"
(cd core && GOWORK=off go build ./...)

echo "==> core/ go vet"
(cd core && GOWORK=off go vet ./...)

echo "==> core/ golangci-lint"
(cd core && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

if [ "$FAST" = "1" ]; then
  echo "==> core/ go test ./internal/... (CI_LOCAL_FAST=1: skipping ./tests/... Docker suites)"
  (cd core && GOWORK=off go test ./internal/...)
else
  echo "==> core/ go test ./... (full suite, including Docker-backed crash matrix + SFTP integration)"
  (cd core && GOWORK=off go test ./...)
fi

echo "==> apps/common go build, vet, test"
(cd apps/common && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

echo "==> apps/common golangci-lint"
(cd apps/common && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

if [ -f apps/generic/go.mod ]; then
  echo "==> apps/generic go build, vet, test"
  (cd apps/generic && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

  echo "==> apps/generic golangci-lint"
  (cd apps/generic && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)
fi

if [ -f apps/synology/go.mod ]; then
  echo "==> apps/synology go build, vet, test"
  (cd apps/synology && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

  echo "==> apps/synology golangci-lint"
  (cd apps/synology && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)
fi

if [ -f apps/ugos/backend/go.mod ]; then
  echo "==> apps/ugos/backend go build, vet, test"
  (cd apps/ugos/backend && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

  echo "==> apps/ugos/backend golangci-lint"
  (cd apps/ugos/backend && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

  if [ "$FAST" != "1" ]; then
    echo "==> apps/ugos/backend cross-compile linux/amd64"
    (cd apps/ugos/backend && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./...)
  fi
fi

if [ "$FAST" != "1" ] && [ -d apps/ugos/frontend/upk-proof ]; then
  if [ -d apps/ugos/frontend/upk-proof/node_modules ]; then
    echo "==> apps/ugos/frontend/upk-proof typecheck, build"
    (cd apps/ugos/frontend/upk-proof && npm run --silent build)
  else
    echo "==> apps/ugos/frontend/upk-proof has no node_modules, skipping (run 'npm install' there)"
  fi
fi

if [ "$FAST" != "1" ]; then
  echo "==> UGREEN cross-compile (amd64)"
  (cd core && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOWORK=off go build -o /dev/null ./...)

  echo "==> UGREEN cross-compile (arm64)"
  (cd core && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOWORK=off go build -o /dev/null ./...)
fi

if [ -d ui/shared ]; then
  if [ ! -d ui/shared/node_modules ]; then
    echo "==> ui/shared has no node_modules, skipping frontend checks (run 'cd ui/shared && npm install')"
  else
    echo "==> ui/shared typecheck"
    (cd ui/shared && npm run --silent lint)

    echo "==> ui/shared typecheck every provider"
    (cd ui/shared && npm run --silent typecheck:providers)

    echo "==> ui/shared eslint"
    (cd ui/shared && npm run --silent eslint)

    echo "==> ui/shared tests"
    (cd ui/shared && npm test --silent)

    if [ "$FAST" != "1" ]; then
      echo "==> ui/shared build"
      (cd ui/shared && npm run --silent build)
    fi
  fi
fi

if [ "$FAST" != "1" ] && [ -d apps/common/tests ]; then
  if [ -d apps/common/tests/node_modules ]; then
    echo "==> apps/common/tests typecheck"
    (cd apps/common/tests && npm run --silent lint)

    echo "==> apps/common/tests eslint"
    (cd apps/common/tests && npm run --silent eslint)

    echo "==> cross-provider conformance suite (apps/common/tests)"
    (cd apps/common/tests && npm test --silent)
  else
    echo "==> apps/common/tests has no node_modules, skipping (run 'npm install' there)"
  fi
fi

if [ "$FAST" != "1" ]; then
  echo "==> release-manifest generator guards (#174)"
  bash scripts/tests/record-release-hashes-guards.test.sh

  echo "==> repository-structure dependency rules (§7.1)"
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

echo "==> ci-local: ok"

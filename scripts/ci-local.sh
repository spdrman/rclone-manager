#!/usr/bin/env sh
# Full local mirror of .github/workflows/ci.yml and rclone-upgrade-gate.yml,
# run entirely on this machine. GitHub Actions no longer auto-triggers on
# this repo (see the "on:" blocks in .github/workflows/*.yml, all switched
# to workflow_dispatch-only) — this script is the actual gate now. The
# pre-commit hook runs it on every commit; `--admin` merges rely on it
# having been green, not on any GitHub-side check.
#
# Comprehensive on purpose, job-for-job with ci.yml, which means it is NOT
# fast: the full core/ test suite (including the Docker-backed crash matrix,
# SFTP integration and MinIO integration tests), two cross-compiles, three separate frontend
# installs/builds, and the dependency-rules worktree-deletion proofs. Needs
# a running Docker daemon for the full run, and now says so instead of
# quietly reporting on suites that skipped themselves.
#
# Set CI_LOCAL_FAST=1 for a quick iteration loop. It skips core/'s
# ./tests/... (the crash matrix, the SFTP integration tests and the MinIO
# integration tests), both
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
# down, the crash matrix, the SFTP and MinIO integration suites and the whole
# apps/generic/tests/dockercli package call t.Skip, go test still exits 0,
# and nothing would reach the ledger. A full run refuses to start without
# the daemon; CI_LOCAL_SKIP_DOCKER=1 is the out-loud opt-out and ends
# INCOMPLETE.
#
# A missing Playwright Chromium is the third instance of the same shape,
# and gets the same answer: the browser e2e step refuses and names the
# install command, and CI_LOCAL_SKIP_E2E=1 is the out-loud opt-out that
# ledgers. See scripts/e2e/run-tests-repo-gate.sh, which is where that
# suite now runs from (#158 moved it to spdrman/rclone-manager-tests, #197
# is why it runs at all).
#
# The two-machine end-to-end backup proof (#356) is the fourth, with one
# extra state. Docker being absent is the same shape as everything above.
# A Docker daemon that is present and refuses a PRIVILEGED container is a
# capability question too: the manager machine is docker-in-docker, because
# mounting the host socket instead would install onto the developer's own
# host rather than onto a fresh machine. The script says CANNOT RUN and
# exits 3 for both, this gate ledgers that, and CI_LOCAL_SKIP_TWO_MACHINE=1
# is the out-loud opt-out.
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

# The marker every process this gate starts can read: "you are running inside
# the local gate", as opposed to a developer running one suite by hand. The
# Docker fixtures key on it to tell "this laptop has no Docker" (skip, which
# is honest outside the gate) from "the daemon this gate already used has
# gone away" (a failure, because the gate refused to start without it). The
# name is load-bearing in core/tests; do not rename it here alone.
export CI_LOCAL=1

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

# Every exit from here on carries a verdict line, and drops anything this run
# started. Without the marker, a run that died under `set -e` printed no
# "==> ci-local: ..." line at all, so the one outcome a reader most needs to
# grep for was the one with no marker. Installed through one function rather
# than as `trap ... EXIT` here, because a trap is set and not appended: a
# second EXIT trap later in this file would silently replace this one. See
# gate_install_traps.
gate_install_traps

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

# Not a refusal, and deliberately not one: Resource Saver being on is normal,
# the sentinel below makes it harmless, and the per-step probes turn what it
# still breaks into a named failure. It is printed because it is the first
# thing to check when the daemon dies at minute 18 (#457).
gate_warn_resource_saver

# The daemon is now known good (or the run said out loud that it is not merge
# evidence). Keep it that way: Docker Desktop's Resource Saver measures IDLE,
# and this gate has several Docker-free stretches longer than its five-minute
# timer, so every run used to cold-start the VM somewhere in the middle. One
# container that sleeps for the life of the run means the daemon is never
# idle, on any machine, without depending on a GUI setting (#457).
gate_start_docker_sentinel

if [ "$FAST" = "1" ]; then
  gate_note_skip "core/ ./tests/... (the Docker-backed crash matrix, the SFTP integration tests, the MinIO integration tests and the composed conformance scenario), the cross-compiles, the upk-proof and ui/shared production builds, the apps/common/tests cross-provider conformance suite, the browser e2e suite and CLI smoke slice from rclone-manager-tests, the repository-structure dependency rules and this gate's own self-test (CI_LOCAL_FAST=1)"
fi

# The documentation anchor check, added under separate work. Guarded on the
# file rather than assumed, because this branch and the branch that writes it
# are in flight at the same time; once it has landed the guard can go and the
# step becomes unconditional, which is the shape every other step here has
# for #160's reason (a step that quietly skips itself when its own file is
# missing is a silent skip wearing a different hat). It runs here, before the
# first Go build, because it costs about a second and it is the check most
# likely to be broken by the edit being committed.
if [ -f scripts/selftest/check-anchors.sh ]; then
  gate_step "documentation anchors resolve (scripts/selftest/check-anchors.sh)"
  bash scripts/selftest/check-anchors.sh
else
  echo "==> anchors: scripts/selftest/check-anchors.sh is not in this tree yet, nothing to run"
fi

gate_step "core/ go build"
(cd core && GOWORK=off go build ./...)

gate_step "core/ go vet"
(cd core && GOWORK=off go vet ./...)

gate_step "core/ golangci-lint"
(cd core && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

if [ "$FAST" = "1" ]; then
  gate_docker_step "core/ go test ./internal/... (CI_LOCAL_FAST=1: skipping ./tests/... Docker suites)"
  (cd core && GOWORK=off go test ./internal/...)
else
  # tests/crashmatrix, tests/sftpintegration and tests/miniointegration
  # run separately, under cmd/gotestwatch instead of `go test`'s own
  # default -timeout (10m per package). All three drive real Docker work
  # through a real subprocess (tests/crashmatrix's own harness, a real
  # rclone transfer against the SFTP fixture container, or a real S3
  # round trip against the MinIO one), so their wall-clock time tracks
  # real machine load rather than a fixed budget; issue #256 is a real
  # gate run hitting go test's fixed 10m default under load. On a machine
  # that has to PULL a fixture image first, the pull alone can eat most
  # of that budget. gotestwatch bounds them with a no-progress window
  # derived from this run's own measured pace instead (issue #247's
  # reasoning, one layer out; see core/cmd/gotestwatch/doc.go), so there
  # is no fixed number to outgrow.
  gate_docker_step "core/ go test ./... (excluding tests/crashmatrix + tests/sftpintegration + tests/miniointegration + tests/conformance, run next)"
  (cd core && GOWORK=off go test $(GOWORK=off go list ./... | grep -vE '/tests/(crashmatrix|sftpintegration|miniointegration|conformance)$'))

  gate_docker_step "core/ tests/crashmatrix + tests/sftpintegration + tests/miniointegration + tests/conformance under gotestwatch (issue #256: no fixed go test -timeout)"
  (cd core && GOWORK=off go run ./cmd/gotestwatch -count=1 ./tests/crashmatrix/... ./tests/sftpintegration/... ./tests/miniointegration/... ./tests/conformance/...)
fi

gate_step "apps/common go build, vet, test"
(cd apps/common && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

gate_step "apps/common golangci-lint"
(cd apps/common && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

gate_step "distribution go build, vet, test"
(cd distribution && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...)

gate_step "distribution golangci-lint"
(cd distribution && GOWORK=off golangci-lint run --config "$REPO_ROOT/.golangci.yml" ./...)

if [ -f apps/generic/go.mod ]; then
  gate_docker_step "apps/generic go build, vet, test"
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

# The browser e2e signal (#158, #197). Until this step existed, the
# Playwright suite had no automated execution anywhere: nightly-e2e.yml's
# schedule is commented out, every workflow here is workflow_dispatch-only,
# and this script never invoked it. So it ran when somebody remembered to,
# which is how a deterministically red spec sat on main through four merges
# and was dismissed twice as an ordering flake.
#
# The suite itself no longer lives in this repository; it is Suite B of
# spdrman/rclone-manager-tests, pinned by scripts/e2e/tests-repo.pin. What
# it runs against is not the pin's own build, it is THIS working tree's
# ui/shared, on a port the harness picks and proves free. The same step
# also runs that repository's CLI smoke slice against a backup-manager
# built from this tree, which is a black-box signal this repository has
# never had at all.
#
# Non-FAST only, so CI_LOCAL_FAST=1 skips it under the existing
# never-merge-on-FAST rule. CI_LOCAL_SKIP_E2E=1 is the separate out-loud
# opt-out for a machine with no browser, and it ledgers, so that run ends
# INCOMPLETE and says which check it left out.
if [ "$FAST" != "1" ]; then
  if [ "${CI_LOCAL_SKIP_E2E:-0}" = "1" ]; then
    gate_note_skip "the browser e2e suite and the CLI smoke slice from rclone-manager-tests, which are the only automated execution either of them gets (CI_LOCAL_SKIP_E2E=1)"
  else
    gate_step "browser e2e + CLI smoke, from rclone-manager-tests at the pinned sha (#197)"
    bash scripts/e2e/run-tests-repo-gate.sh
  fi
fi

# The two-machine end-to-end backup proof (#356). Everything between
# "nothing installed" and "an artifact is on disk" was proven in pieces and
# nowhere joined up, so the one claim a user actually makes (a fresh
# install can be pointed at a machine and pull a backup off it) had no
# test anywhere. This is that test: two throwaway containers on a temporary
# network, the real installer, a backup set created through the CLI, and
# the artifact compared to the source by digest.
#
# Non-FAST only, like the browser suite, and for the same reason: it costs
# a container image build plus about a minute per case. CI_LOCAL_SKIP_TWO_MACHINE=1
# is the separate out-loud opt-out, and it ledgers.
#
# Three outcomes, not two, which is the part that matters. The script exits
# 3 when this machine CANNOT perform the proof (no Docker, or a daemon that
# refuses a privileged container so docker-in-docker cannot start), and
# that is neither a pass nor a failure: it is ledgered, exactly as a
# missing browser or a stopped daemon is, so the run ends INCOMPLETE and
# says which proof it could not perform. Reporting ok for a backup nobody
# proved would be the single worst version of #160.
if [ "$FAST" != "1" ]; then
  if [ "${CI_LOCAL_SKIP_TWO_MACHINE:-0}" = "1" ]; then
    gate_note_skip "the two-machine end-to-end backup proof (#356), which is the only test anywhere that a fresh install can pull a real backup off a real machine (CI_LOCAL_SKIP_TWO_MACHINE=1)"
  else
    gate_docker_step "two throwaway machines, one temporary network, one real backup (#356)"
    # Not under `set -e`: exit 3 is a verdict this script has to READ, and
    # `set -e` would end the run on it before the ledger ever saw it.
    set +e
    bash scripts/e2e/two-machine-backup.sh
    two_machine_status=$?
    set -e
    case "$two_machine_status" in
      0) ;;
      3)
        gate_note_skip "the two-machine end-to-end backup proof (#356): this machine could not perform it, and said why above"
        ;;
      *)
        exit "$two_machine_status"
        ;;
    esac
  fi
fi

# The static layer checks (issue #165) run even in FAST mode: none of them
# builds, installs or deletes anything, so together they cost seconds, and
# they are the ones a mid-refactor edit is most likely to break.
gate_step "three-layer boundaries, static checks (§7.1, #165)"
bash scripts/architecture/check-layer-manifest.sh
bash scripts/architecture/check-core-dependency-rule.sh
bash scripts/architecture/check-layer-ownership.sh
bash scripts/architecture/check-ui-shared-provider-imports.sh

# The installer's refusals (#262). Standard library only and no Docker, so
# this is a couple of seconds and runs in FAST mode too. It is here rather
# than nowhere for the reason #160 exists: scripts/deploy's own Python
# tests have never been wired into this gate, so they have never run on a
# commit, and a refusal nobody has watched work is not a refusal. Every
# assertion in it is about the installer saying no, which is exactly the
# behaviour nobody exercises until the day it matters on a NAS they cannot
# debug.
gate_step "installer prerequisite refusals (#262)"
# Not piped through `tail`: this file has no `set -o pipefail` (and could
# not portably rely on one, being #!/usr/bin/env sh), so a pipeline's exit
# status under plain `set -e` is its LAST command's -- `tail`, which always
# succeeds. Piping this would let the installer's own test suite fail
# silently forever, the exact "a refusal nobody has watched work is not a
# refusal" failure this step exists to close, one line away from closing it.
(cd scripts/install && python3 -m unittest test_install_docker_host)

gate_step "performance baseline present, and its gate can fail (#165)"
bash scripts/perf/check-baseline.sh
bash scripts/perf/selftest.sh

# ci.yml's api-contract job, mirrored here because ci.yml is
# workflow_dispatch-only and therefore runs on no commit: without these two
# lines the byte-for-byte binding comparison, the implementation-type leak
# scan and all 15 of their mutation controls had never executed on a commit
# at all (#166, PR #194 review M1). Unconditional, FAST included: together
# they take under 20 seconds, and check-contract-drift.sh is the only thing
# that catches a hand edit to a generated binding that keeps its digest
# intact.
gate_step "/api/v1 bindings match the contract, and no implementation type leaks (#166)"
bash scripts/api/check-contract-drift.sh

# The other half of #166's "generated from it or mechanically validated
# against it": ui/shared/src/api/client.ts is hand-written on top of the
# generated module, so its request paths are string literals that the
# binding comparison above cannot see. Fourteen of them named operations
# neither the contract nor the router had, and four of the six shipped
# pages failed against a real backend while every suite stayed green
# (#211). Static, so it costs about a second and needs no npm install.
gate_step "every /api/v1 path the shared client builds is a declared operation (#211)"
bash scripts/api/check-client-paths.sh

gate_step "the /api/v1 contract gates can actually fail (mutation self-test)"
bash scripts/api/selftest.sh

if [ "$FAST" != "1" ]; then
  gate_step "release-manifest generator guards (#174)"
  bash scripts/tests/record-release-hashes-guards.test.sh

  # The publish script is the one step in this repository that does
  # something irreversible, and it runs once, on the day it matters. Its
  # six refusals get the same treatment #174's five got: driven against
  # the real script in throwaway repositories, asserting the distinct
  # message rather than only the exit code, through a seam that stops
  # before the first Docker command (#88).
  gate_step "image-publish guards (#88)"
  bash scripts/tests/publish-image-guards.test.sh

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

  gate_step "architecture rules can actually fail (mutation self-test)"
  bash scripts/architecture/selftest.sh

  # EPIC E's FR-35 compatibility gate, shown to fire (#242). core/tests/compat
  # is a wall of "nothing about a medium-free deployment moved" assertions,
  # and every one of them is the shape this repository keeps finding passing
  # for the wrong reason. This runs each cell against a real planted
  # violation in a copy of the tree, including the two the EPIC E spec's own
  # section 4 table names by hand. It costs a few minutes because every
  # mutant builds core/ and backup-manager and runs a real capture; that is
  # the price of the corpus meaning anything.
  gate_step "the FR-35 compatibility cells can actually fail (mutation self-test, #242)"
  bash scripts/compat/selftest.sh

  # EPIC E's composed conformance scenario, shown to fire (#242). Same
  # argument as the block above, applied to the other half of that issue:
  # core/tests/conformance runs the three-tier chain against MinIO and
  # watches FR-30's standing invariant at every event that could falsify it,
  # and a watcher nobody has watched catch something is a watcher that
  # certifies nothing. This plants nine violations in real product files,
  # including the gate line's own, and each one has to turn the suite red
  # AND name the promise it broke.
  gate_docker_step "the composed conformance cells can actually fail (mutation self-test, #242)"
  bash scripts/conformance/selftest.sh

  gate_step "repository-structure dependency rules (§7.1), by actual deletion"
  bash scripts/architecture/check-core-dependency-rule.sh
  bash scripts/architecture/verify-core-without-apps.sh
  bash scripts/architecture/verify-core-without-distribution.sh
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

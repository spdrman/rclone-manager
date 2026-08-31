#!/usr/bin/env bash
# The replacement e2e signal (issues #158 and #197).
#
# Until this existed, the browser suite had no automated execution at all:
# nightly-e2e.yml's schedule was commented out, GitHub Actions is
# workflow_dispatch-only here, and scripts/ci-local.sh never invoked
# Playwright. So the suite ran when somebody remembered to run it, which is
# how a deterministically red spec sat on main through four merges and got
# dismissed twice as an ordering flake (#172, then #197).
#
# This script is what ci-local.sh calls instead, on every non-FAST run, and
# since ci-local.sh runs under `set -e` from .husky/pre-commit, a red suite
# refuses the commit. It does two things:
#
#   1. the CLI smoke slice (55 of Suite A's 60 cases) against a
#      backup-manager built from THIS working tree;
#   2. the browser suite against THIS working tree's ui/shared.
#
# Both come from spdrman/rclone-manager-tests at the sha in tests-repo.pin,
# so the tests are versioned independently of the product and a new test
# cannot break in-flight work here until the pin is bumped. What is under
# test is never the pin's own idea of a build: it is the tree being
# committed. Those are different things and the run says which is which.
#
# Costs, measured on this machine: about 11 seconds for the smoke slice and
# about 22 for the browser suite, against a gate that already runs
# Docker-backed crash matrices for minutes. The first run at a new pin also
# clones the tests repository and installs its Playwright, which is a minute
# or two, once per pin.
#
# Capability refusals follow gate_require_docker's shape rather than
# inventing a new one: a missing browser is a hard failure that names the
# command that fixes it, and CI_LOCAL_SKIP_E2E=1 is the out-loud opt-out
# that ledgers the skip in ci-local.sh so the run ends INCOMPLETE and cannot
# be merge evidence. That answers #197's first open question with this
# repository's own precedent: Docker is the higher-consequence capability
# and it is refuse-by-default with a ledgered opt-out, so a browser gets the
# same shape and not a weaker one.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

pin_file="scripts/e2e/tests-repo.pin"

die() {
  echo "" >&2
  echo "==> e2e gate: FAILED. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  echo "" >&2
  echo "    Fix it, or choose the skip out loud with CI_LOCAL_SKIP_E2E=1. A run that" >&2
  echo "    skips it ends INCOMPLETE and is not merge evidence." >&2
  exit 1
}

[ -f "$pin_file" ] || die "there is no $pin_file, so this gate does not know which tests to run."

# shellcheck source=/dev/null
. "$pin_file"
TESTS_REPO_URL="${TESTS_REPO_URL:-}"
TESTS_REPO_SHA="${TESTS_REPO_SHA:-}"
if ! [[ "$TESTS_REPO_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  die "$pin_file does not carry a full 40-character commit sha (TESTS_REPO_SHA=${TESTS_REPO_SHA:-unset})." \
      "A short sha or a branch name would let the tests under this gate change without the pin changing," \
      "which is the whole thing the pin exists to stop."
fi
[ -n "$TESTS_REPO_URL" ] || die "$pin_file does not carry TESTS_REPO_URL."

for tool in git go node npm; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH, and this gate needs it."
done

# ------------------------------------------------- the pinned tests checkout
#
# Keyed by sha, so a populated directory is immutable and two concurrent
# gate runs (this machine carries ~50 worktrees of this repository) never
# fight over one working tree. The clone and its npm install both happen in
# a scratch sibling that is renamed into place only once it is complete, so
# a half-finished or interrupted attempt can never be mistaken for a good
# checkout.
#
# A losing or failed scratch directory is left where it is rather than
# deleted. This path is outside any workspace directory, and leaking one
# directory under a cache is a far cheaper mistake than a recursive delete
# there.
cache_root="${XDG_CACHE_HOME:-$HOME/.cache}/rclone-manager-tests-gate"
checkout="$cache_root/$TESTS_REPO_SHA"

if [ ! -f "$checkout/.complete" ]; then
  echo "==> e2e gate: fetching rclone-manager-tests at ${TESTS_REPO_SHA:0:12}"
  mkdir -p "$cache_root"
  scratch="$cache_root/scratch.$$.$(date +%s)"
  mkdir -p "$scratch"
  (
    cd "$scratch"
    git init -q .
    git remote add origin "$TESTS_REPO_URL"
    git fetch -q --depth 1 origin "$TESTS_REPO_SHA"
    git checkout -q --detach FETCH_HEAD
  ) || die "could not fetch $TESTS_REPO_SHA from $TESTS_REPO_URL." \
           "Scratch directory left at $scratch for inspection." \
           "If the sha is on an unpushed branch, push it before pinning it."

  echo "==> e2e gate: installing the browser suite's dependencies (once per pin)"
  (cd "$scratch/suites/web-ui" && npm ci --no-audit --no-fund >/dev/null) \
    || die "npm ci failed in the pinned tests checkout at $scratch/suites/web-ui."

  : >"$scratch/.complete"
  # Losing this race is not an error: the winner published the same sha.
  mv "$scratch" "$checkout" 2>/dev/null || true
  [ -f "$checkout/.complete" ] || die "could not publish the pinned tests checkout to $checkout." \
                                      "Scratch directory left at $scratch."
fi

# ------------------------------------------------------- the build under test
#
# The tests repository refuses to run against a build that will not say which
# commit it is, so the -ldflags here are load-bearing rather than cosmetic:
# without them the binary reports "commit none" and the identity handshake
# aborts the run. That is the handshake doing its job, not a problem to work
# around.
#
# Under a pre-commit hook HEAD is the parent commit and the tree carries the
# staged change, so the build genuinely is HEAD plus something. It says so
# with a -dirty suffix, which the handshake tolerates against a clean pin.
work="$repo_root/.e2e-gate"
mkdir -p "$work"
head_sha="$(git rev-parse HEAD)"
build_commit="$head_sha"
if ! git diff --quiet HEAD 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
  build_commit="$head_sha-dirty"
fi

echo "==> e2e gate: building backup-manager from this working tree"
(cd core && GOWORK=off go build \
  -ldflags "-X main.version=$(git rev-parse --short HEAD) -X main.commit=$build_commit" \
  -o "$work/backup-manager" ./cmd/backup-manager)

echo "==> e2e gate: Suite A smoke slice, against that binary"
RM_MODE=local \
RM_BINARY="$work/backup-manager" \
RM_COMMIT="$head_sha" \
RM_SOURCE_DIR="$repo_root" \
  make -C "$checkout" smoke

# ------------------------------------------------------------ the browser half
#
# ui/shared has to be installed, which ci-local.sh's own preflight already
# refuses without, so reaching here with it missing means this script was run
# standalone. Say so rather than letting `npm run dev` fail sixty seconds
# later inside a webServer timeout.
[ -d ui/shared/node_modules ] \
  || die "ui/shared has no installed dependencies, so its dev server cannot start." \
         "Fix it with: cd ui/shared && npm ci"

# A browser this machine does not have is the one capability question #197
# left open. Refuse, name the fix, and let ci-local.sh ledger the opt-out.
if ! (cd "$checkout/suites/web-ui" && node -e '
const { chromium } = require("playwright-core");
require("node:fs").accessSync(chromium.executablePath());
' >/dev/null 2>&1); then
  die "Playwright has no installed Chromium on this machine, so the browser suite cannot run." \
      "Fix it with: cd $checkout/suites/web-ui && npx playwright install chromium"
fi

# The suite's own unit test of its port helper comes with it. In the old
# home ui/shared's vitest ran it; nothing else does now, and it is the
# thing that stops an E2E_PORT typo becoming port 0 or NaN.
echo "==> e2e gate: the browser suite's own unit tests"
(cd "$checkout/suites/web-ui" && npm run --silent unit)

# One port, chosen here, and handed to the suite through E2E_PORT.
#
# Not decoration, and not the same thing as letting the suite derive its
# own. Playwright re-evaluates playwright.config.ts inside every worker
# process, so anything the config COMPUTES has to come out the same in the
# runner and in each worker. The suite's default derivation probes for a
# free port, and by the time a worker probes, the runner's own Vite is
# already holding the one the runner picked, so the worker can walk to the
# next slot and end up with a baseURL nothing is listening on. That is
# exactly what happened on the first full gate run here: the runner said
# 5930 and one worker navigated to 5931 and got ERR_CONNECTION_REFUSED, one
# test out of 165.
#
# E2E_PORT is read from the environment rather than computed, so the runner
# and every worker read the same number. The residual race (something else
# grabs the port between this probe and Vite's bind) is loud rather than
# silent: --strictPort makes Vite refuse to slide, and reuseExistingServer
# is false, so a lost race fails to start instead of testing somebody
# else's server.
e2e_port="$(node -e '
const { createServer } = require("node:net");
const s = createServer();
s.on("error", () => process.exit(1));
s.listen({ host: "127.0.0.1", port: 0, exclusive: true }, () => {
  const port = s.address().port;
  s.close(() => process.stdout.write(String(port)));
});
')"
[ -n "$e2e_port" ] || die "could not obtain a free port for the browser suite."

echo "==> e2e gate: Suite B browser suite on port $e2e_port, against this working tree's ui/shared"
(cd "$checkout/suites/web-ui" && RM_UI_DIR="$repo_root/ui/shared" E2E_PORT="$e2e_port" npm run --silent e2e)

#!/usr/bin/env bash
# The replacement e2e signal (issues #158 and #197).
#
# Until this existed, the browser suite had no automated execution at all:
# nightly-e2e.yml's schedule was commented out, no workflow here triggered
# on anything, and scripts/ci-local.sh never invoked
# Playwright. So the suite ran when somebody remembered to run it, which is
# how a deterministically red spec sat on main through four merges and got
# dismissed twice as an ordering flake (#172, then #197).
#
# This script is what ci-local.sh calls instead, on every non-FAST run, and
# since ci-local.sh runs under `set -e` from .husky/pre-commit, a red suite
# refuses the commit. It does two things:
#
#   1. the CLI smoke slice (55 of Suite A's 60 cases) against a
#      rbm built from THIS working tree;
#   2. the browser suite against a real deployment built from THIS working
#      tree, over the wire, through scripts/e2e/three-machine-web-ui.sh
#      (#687: this used to run RM_UI_DIR against ui/shared's Vite dev
#      server over a mock API, until the tests repository dropped
#      RM_UI_DIR and moved Suite B onto RM_BASE_URL against a real
#      deployment; that rig already existed in this file's own worktree,
#      unwired to this gate, which is the other half of what #687 fixes).
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

echo "==> e2e gate: building rbm from this working tree"
(cd core && GOWORK=off go build \
  -ldflags "-X main.version=$(git rev-parse --short HEAD) -X main.commit=$build_commit" \
  -o "$work/rbm" ./cmd/backup-manager)

echo "==> e2e gate: Suite A smoke slice, against that binary"
RM_MODE=local \
RM_BINARY="$work/rbm" \
RM_COMMIT="$head_sha" \
RM_SOURCE_DIR="$repo_root" \
  make -C "$checkout" smoke

# ------------------------------------------------------------ the browser half
#
# #687: this used to start ui/shared's own Vite dev server over
# createMockApi and drive it through RM_UI_DIR, so a case's pass or fail
# was a claim about a component rendering given a fixture, and nothing in
# it could go red on the path an operator actually meets (browser ->
# serve-ui -> reverse proxy -> serve -> SQLite). The tests repository
# retired RM_UI_DIR along with that suite (rclone-manager-tests#65) in
# favour of RM_BASE_URL against a real deployment, and
# scripts/e2e/three-machine-web-ui.sh is that deployment: three private
# Docker networks, the product's own two containers built from this
# working tree, a real sshd standing in for the machine being backed up,
# and a client container carrying the browser and the Playwright runner
# together, so nothing here needs a browser installed on the host.
#
# Its own exit code carries the same three-outcome vocabulary
# two-machine-backup.sh uses: 0 passed, 3 is CANNOT RUN (a capability
# this machine does not have, most often no reachable Docker daemon,
# already required above ci-local.sh's own gate_require_docker), anything
# else failed. 3 is translated into the named refusal below rather than
# left to `set -e` so a capability gap still reads as "fix this" and not
# as an unexplained nonzero.
echo "==> e2e gate: the browser suite's own unit tests"
(cd "$checkout/suites/web-ui" && npm run --silent unit)

echo "==> e2e gate: Suite B browser suite, against a real deployment built from this working tree, over the wire"
webui_status=0
bash scripts/e2e/three-machine-web-ui.sh --suite "$checkout/suites/web-ui" || webui_status=$?
if [ "$webui_status" = 3 ]; then
  die "three-machine-web-ui.sh could not perform the proof on this machine (exit 3)." \
      "Its own output above names the missing capability, most likely Docker."
elif [ "$webui_status" != 0 ]; then
  exit "$webui_status"
fi

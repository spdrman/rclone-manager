#!/usr/bin/env bash
# EPIC-B WP1.1 RED plan: "ui/shared/ builds with provider SDK directories
# removed" (docs/EPIC-B-multi-nas.md §69 WP1.1, §11), extended by issue #165
# to Phase 6's full platform list and to a static import scan. Two halves:
# the fast import scan (check-ui-shared-provider-imports.sh, run first so a
# violation reports in seconds rather than after an npm ci) and the deletion
# proof, which removes every NAS-vendor provider directory in a throwaway
# worktree and installs and builds ui/shared there.
#
# The check itself is
# scripts/bdtools/architecture/verify_ui_shared_without_provider_sdks.py now
# (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml run
#     `bash scripts/architecture/verify-ui-shared-without-provider-sdks.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs;
#   * scripts/architecture/selftest.sh drives THIS path from inside each
#     mutant copy of the tree, so the shim is exercised by every control.
#
# `exec`, so the exit status is the check's own.
#
# NOTE: no `cd` here, deliberately. The check resolves its target tree with
# `git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY, exactly
# as the bash original's `cd "$(git rev-parse --show-toplevel)"` did, which
# is what lets the self-test point it at a mutant copy.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/architecture/verify_ui_shared_without_provider_sdks.py" "$@"

#!/usr/bin/env bash
# The shared UI's provider-SDK import scan: no module specifier under
# ui/shared/src names a provider directory or a provider SDK, across all ten
# Phase 6 platforms including the four with no directory to delete yet
# (issue #165).
#
# The check itself is
# scripts/bdtools/architecture/check_ui_shared_provider_imports.py now
# (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml run
#     `bash scripts/architecture/check-ui-shared-provider-imports.sh`;
#   * scripts/architecture/verify-ui-shared-without-provider-sdks.sh runs it
#     first, so a violation reports in seconds rather than after an npm ci;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs;
#   * scripts/architecture/selftest.sh drives THIS path from inside each
#     mutant copy of the tree, so the shim is exercised by every control.
#
# The scan still reads module SPECIFIERS, not whole lines: ui/shared
# legitimately names every platform in prose and in its PlatformId union
# (platform differences are capability data, per EPIC B #81), and a
# line-level match would either fire on all of that or be watered down until
# it fired on nothing.
#
# `exec`, so the exit status is the check's own.
#
# NOTE: no `cd` here, deliberately. The check resolves its target tree with
# `git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY, exactly
# as the bash original's `cd "$(git rev-parse --show-toplevel)"` did, which
# is what lets the self-test point it at a mutant copy.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/architecture/check_ui_shared_provider_imports.py" "$@"

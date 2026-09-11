#!/usr/bin/env bash
# Positive controls for the /api/v1 contract gates (#166), at the path
# everything already names.
#
# The controls themselves are scripts/bdtools/api/selftest.py now (EPIC I,
# I1.6 / #672). This file stays because the path is load bearing:
#
#   * scripts/ci-local.sh runs `bash scripts/api/selftest.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs;
#   * check-contract-drift.sh's own gate-wiring rule (section 3) greps
#     scripts/ci-local.sh for `bash scripts/api/selftest.sh` by name, so the
#     path itself is part of what is being enforced.
#
# `exec`, so the exit status is the self-test's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/api/selftest.py" "$@"

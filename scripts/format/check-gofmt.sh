#!/usr/bin/env bash
# Issue #417's formatting sweep, at the path everything already names.
#
# The sweep itself is scripts/rcmtools/format/check_gofmt.py now (EPIC I,
# I1.6 / #672). This file stays because the path is load bearing in three
# places that are not this port's to move:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml both run
#     `bash scripts/format/check-gofmt.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this
#     literal path to drive the gate step it stubs;
#   * scripts/rcmtools/format/selftest.py drives THIS path rather than
#     the module directly, so the shim itself is exercised by every
#     control.
#
# `exec`, so the exit status is the sweep's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/format/check_gofmt.py" "$@"

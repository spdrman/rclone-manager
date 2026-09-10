#!/usr/bin/env bash
# The performance gate's own positive controls (#165), at the path
# everything already names.
#
# The controls themselves are scripts/rcmtools/perf/selftest.py now (EPIC I,
# I1.6 / #672). This file stays because the path is load bearing:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml both run
#     `bash scripts/perf/selftest.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs, and separately to prove CI_LOCAL
#     is exported to every step the gate runs.
#
# `exec`, so the exit status is the self-test's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/perf/selftest.py" "$@"

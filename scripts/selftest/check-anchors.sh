#!/usr/bin/env bash
# Every anchored selftest's `--check-anchors`, in one run, at the path
# everything already names.
#
# The aggregation itself is scripts/bdtools/selftest/check_anchors.py
# now (EPIC I, I1.6 / #672 / #458). This file stays because the path is
# load bearing:
#
#   * scripts/ci-local.sh runs `bash scripts/selftest/check-anchors.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a stand-in at this
#     literal path.
#
# `exec`, so the exit status is the aggregator's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/selftest/check_anchors.py" "$@"

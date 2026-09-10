#!/usr/bin/env bash
# The FR-35 compatibility gate's own positive controls (#242), at the path
# everything already names.
#
# The controls themselves are scripts/rcmtools/compat/selftest.py now
# (EPIC I, I1.6 / #672). This file stays because the path is load bearing:
#
#   * scripts/ci-local.sh runs `bash scripts/compat/selftest.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a stand-in at this
#     literal path to drive the gate step it stubs;
#   * scripts/rcmtools/selftest/check_anchors.py and
#     scripts/selftest/check-anchors.sh both run it with --check-anchors
#     for the same reason they run every other anchored selftest that way;
#   * core/cmd/backup-manager/usagepins_test.go, core/tests/compat's own
#     capture_api.go/capture_upgrade.go/compat_test.go, README.md and
#     docs/storage-mediums.md/docs/conformance/epic-e-matrix.md all name
#     this literal path in their own comments and prose.
#
# `exec`, so the exit status is the self-test's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/compat/selftest.py" "$@"

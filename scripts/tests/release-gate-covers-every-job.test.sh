#!/usr/bin/env bash
# The check itself is scripts/rcmtools/tests/release_gate_covers_every_job.py
# now (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing in three places that are not this port's to move:
#
#   * scripts/ci-local.sh runs `bash scripts/tests/release-gate-covers-every-job.test.sh`
#     in every run;
#   * .github/workflows/ci.yml runs it again in release-gate;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs.
#
# `exec`, so the exit status and the checks/failures tally are the port's
# own and not a copy of it.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/tests/release_gate_covers_every_job.py" \
  "$repo_root/.github/workflows/ci.yml"

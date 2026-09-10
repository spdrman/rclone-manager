#!/usr/bin/env bash
# Positive controls for the architecture checks (#165), at the path
# everything already names.
#
# The controls themselves are scripts/rcmtools/architecture/selftest.py now
# (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing in three places that are not this port's to move:
#
#   * scripts/ci-local.sh runs `bash scripts/architecture/selftest.sh` in a
#     non-FAST run;
#   * .github/workflows/ci.yml runs it again, and installs golangci-lint
#     specifically so this file's check-unowned-go controls report "the
#     linter says no" rather than "the binary is missing" (#575);
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs.
#
# `exec`, so the exit status is the self-test's own. That matters more here
# than anywhere else in this directory: this is the file that decides
# whether the other ten checks can still fail, so a wrapper that flattened
# its status would report every architecture rule as proven while proving
# none of them.
#
# NOTE: no `cd` here, deliberately. The self-test resolves its target tree
# with `git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY,
# exactly as the bash original's `cd "$(git rev-parse --show-toplevel)"`
# did. It then copies that tree per mutation and runs the checks from
# inside each copy, so the target must come from the caller's cwd and never
# from this file's location.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/rcmtools/architecture/selftest.py" "$@"

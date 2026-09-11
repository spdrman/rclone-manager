#!/usr/bin/env bash
# The CI wrapper around the two-machine proof (#575), at the path CI names.
#
# The wrapper itself is scripts/bdtools/e2e/two_machine_ci.py now (EPIC I,
# I1.6 / #672). This file stays because .github/workflows/ci.yml runs
# `bash scripts/e2e/two-machine-ci.sh --case all` and
# scripts/tests/two-machine-ci-verdict.test.sh drives this literal path with
# TWO_MACHINE_PROOF pointing at a stand-in.
#
# `exec`, so the three-outcome exit status (0 passed, 3 could not be
# performed, anything else failed) is the wrapper's own and not a copy of it.
# Flattening that number here would put a green required check on the branch
# that publishes a signed image, standing on a run that never happened, which
# is the entire reason the wrapper exists.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/e2e/two_machine_ci.py" "$@"

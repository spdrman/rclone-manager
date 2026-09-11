#!/usr/bin/env bash
# The /api/v1 contract drift gate (#166), at the path everything already
# names.
#
# The gate itself is scripts/bdtools/api/check_contract_drift.py now (EPIC
# I, I1.6 / #672). This file stays because the path is load bearing in three
# places that are not this port's to move:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml both run
#     `bash scripts/api/check-contract-drift.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path (`printf '#!/usr/bin/env bash\nexit 0\n' > .../check-contract-drift.sh`)
#     to drive the gate step it stubs;
#   * scripts/bdtools/api/selftest.py drives THIS path (from inside each
#     mutant copy of the tree) rather than the module directly, so the shim
#     itself is exercised by every control.
#
# That is why this is a real script and not a symlink: a fixture that
# overwrites the path must overwrite a stand-in, not the port underneath it.
#
# `exec`, so the exit status is the gate's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/api/check_contract_drift.py" "$@"

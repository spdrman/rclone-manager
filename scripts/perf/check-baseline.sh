#!/usr/bin/env bash
# Phase 6's performance gate (#165), at the path everything already names.
#
# The gate itself is scripts/bdtools/perf/check_baseline.py now (EPIC I,
# I1.6 / #672: sixty scripts across twelve domains, each reinventing the same
# four helpers, consolidated onto scripts/bdtools). This file stays because
# the path is load bearing in three places that are not this port's to move:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml both run
#     `bash scripts/perf/check-baseline.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path (`printf '#!/usr/bin/env bash\nexit 0\n' > .../check-baseline.sh`)
#     to drive the gate step it stubs;
#   * scripts/bdtools/perf/selftest.py drives THIS path rather than the
#     module directly, so the shim itself is exercised by every control.
#
# That is why this is a real script and not a symlink: a fixture that
# overwrites the path must overwrite a stand-in, not the port underneath it.
#
# `exec`, so the exit status is the gate's own. See bdtools.harness for
# where that status is decided.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/perf/check_baseline.py" "$@"

#!/usr/bin/env bash
# The suite itself is scripts/rcmtools/tests/two_machine_exit_status.py now
# (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing in three places that are not this port's to move:
#
#   * scripts/ci-local.sh runs `bash scripts/tests/two-machine-exit-status.test.sh`
#     in every run;
#   * .github/workflows/ci.yml runs it again in two-machine-e2e;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path (`printf '#!/usr/bin/env bash\nexit 0\n' > .../two-machine-exit-status.test.sh`)
#     to drive the gate step it stubs.
#
# `exec`, so the exit status and the checks/failures tally are the port's
# own and not a copy of it.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/tests/two_machine_exit_status.py" "$@"

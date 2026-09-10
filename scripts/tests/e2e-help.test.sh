#!/usr/bin/env bash
# The suite itself is scripts/rcmtools/tests/e2e_help.py now (EPIC I, I1.6 /
# #672 / #697, carrying the #662 fix from fix/663-e2e). This file stays
# because the path is load bearing in two places that are not this port's
# to move:
#
#   * scripts/ci-local.sh runs `bash scripts/tests/e2e-help.test.sh` in
#     every run, FAST included;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path (`printf '#!/usr/bin/env bash\nexit 0\n' > .../e2e-help.test.sh`)
#     to drive the gate step it stubs.
#
# `exec`, so the exit status and the checks/failures tally are the port's
# own and not a copy of it.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/tests/e2e_help.py" "$@"

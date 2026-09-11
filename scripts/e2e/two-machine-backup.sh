#!/usr/bin/env bash
# The two-machine backup proof (#356), at the path everything already names.
#
# The proof itself is scripts/bdtools/e2e/two_machine_backup.py now (EPIC I,
# I1.6 / #672: sixty scripts across twelve domains, each reinventing the same
# four helpers, consolidated onto scripts/bdtools). This file stays because
# the path is load bearing in four places that are not this port's to move:
#
#   * scripts/ci-local.sh execs it and reads its exit status;
#   * .github/workflows/ci.yml reaches it through scripts/e2e/two-machine-ci.sh;
#   * scripts/tests/two-machine-exit-status.test.sh drives it with stand-in
#     tools on PATH;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path (`printf '#!/usr/bin/env bash\nexit 3\n' > .../two-machine-backup.sh`)
#     to drive the gate's INCOMPLETE ledger.
#
# That last one is why this is a real script and not a symlink: the test
# overwrites the path, and a symlink there would have it overwrite the port
# instead of a stand-in.
#
# `exec`, so the exit status is the proof's own and not a copy of it. The
# whole exit-code contract (0 pass, 3 this machine could not perform the
# proof, anything else failed) depends on that number arriving here
# untouched; see bdtools.harness for where it is decided.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/e2e/two_machine_backup.py" "$@"

#!/usr/bin/env bash
# The suite itself is scripts/rcmtools/tests/record_release_hashes_guards.py
# now (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing in three places that are not this port's to move:
#
#   * scripts/ci-local.sh runs `bash scripts/tests/record-release-hashes-guards.test.sh`
#     in a non-FAST run, before the gate self-test;
#   * .github/workflows/ci.yml runs it again;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path (`printf '#!/usr/bin/env bash\nexit 0\n' > .../record-release-hashes-guards.test.sh`)
#     to drive the gate step it stubs -- arriving with #182 without one made
#     `bash` on a missing path exit 127 under `set -e` and kill every
#     full-tree case below it for an unrelated reason.
#
# `exec`, so the exit status is the port's own and not a copy of it.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/tests/record_release_hashes_guards.py" "$@"

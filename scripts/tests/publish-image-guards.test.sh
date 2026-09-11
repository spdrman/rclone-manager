#!/usr/bin/env bash
# The suite itself is scripts/bdtools/tests/publish_image_guards.py now
# (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing in two places that are not this port's to move:
#
#   * scripts/ci-local.sh runs `bash scripts/tests/publish-image-guards.test.sh`
#     in a non-FAST run;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path (`printf '#!/usr/bin/env bash\nexit 0\n' > .../publish-image-guards.test.sh`)
#     to drive the gate step it stubs.
#
# `exec`, so the exit status is the port's own and not a copy of it.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/tests/publish_image_guards.py" "$@"

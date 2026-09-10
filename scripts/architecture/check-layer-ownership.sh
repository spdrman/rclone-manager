#!/usr/bin/env bash
# The layer-ownership check: nothing outside core may DECLARE a core-owned
# concept -- lifecycle state, retention, validation, catalog or backup
# policy (issue #165).
#
# The check itself is scripts/rcmtools/architecture/check_layer_ownership.py
# now (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml run
#     `bash scripts/architecture/check-layer-ownership.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs;
#   * scripts/architecture/selftest.sh drives THIS path from inside each
#     mutant copy of the tree, so the shim is exercised by every control.
#
# The scanning is still scripts/architecture/ownership.go, unported and
# invoked with `GOWORK=off go run` from the module: the thing being detected
# is a Go DECLARATION, and grep cannot tell one from a comment. The Python
# is the wrapper that decides WHAT to scan -- every runtime-platform and
# distribution path in the layer manifest -- so adding a platform or an
# adapter brings it under the rule automatically.
#
# `exec`, so the exit status is the check's own.
#
# NOTE: no `cd` here, deliberately. The check resolves its target tree with
# `git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY, exactly
# as the bash original's `cd "$(git rev-parse --show-toplevel)"` did, which
# is what lets the self-test point it at a mutant copy.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/rcmtools/architecture/check_layer_ownership.py" "$@"

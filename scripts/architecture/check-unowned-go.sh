#!/usr/bin/env bash
# Is every Go file in this repository owned by a module, and if not, is it
# checked anyway? (issue #417)
#
# The check itself is scripts/bdtools/architecture/check_unowned_go.py now
# (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml run
#     `bash scripts/architecture/check-unowned-go.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs, and its Group L watches this
#     step's marker in both directions;
#   * scripts/architecture/selftest.sh drives THIS path from inside each
#     mutant copy of the tree, so the shim is exercised by every control.
#
# The reasoning that is still true and still load bearing: "unowned" means
# no directory at or above the file contains a go.mod, which today is
# scripts/api/gen-bindings.go and scripts/architecture/ownership.go. That
# is not a defect on its own -- but this gate vets and lints PER MODULE, so
# nothing in it has ever reached either file, which is how one of the pair
# came to be the only unformatted Go file in the tree. The check DISCOVERS
# that set rather than naming it, so a third unowned file added tomorrow is
# covered the day it lands. `.github/workflows/ci.yml` installs
# golangci-lint specifically so this reports "the linter says no" rather
# than "the binary is missing" (#575).
#
# Takes an optional directory: a git work tree to check instead of this
# one, which is what scripts/architecture/selftest.sh drives.
#
# `exec`, so the exit status is the check's own.
#
# NOTE: no `cd` here, deliberately. The check resolves its target tree with
# `git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY, exactly
# as the bash original's `cd "$(git rev-parse --show-toplevel)"` did, which
# is what lets the self-test point it at a mutant copy.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/architecture/check_unowned_go.py" "$@"

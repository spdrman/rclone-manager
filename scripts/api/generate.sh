#!/usr/bin/env bash
# Regenerate the /api/v1 bindings from the authoritative contract (#166), at
# the path everything already names.
#
# The generator invocation itself is scripts/rcmtools/api/generate.py now
# (EPIC I, I1.6 / #672). This file stays because the path is load bearing:
#
#   * scripts/api/selftest.sh (also a shim now) drives it as a subprocess,
#     from inside a mutant copy of the tree, to prove check-contract-drift.sh
#     catches a contract change nobody regenerated for;
#   * docs/api/contract.md and this repository's Go test failure messages
#     ("Run scripts/api/generate.sh") both name this literal path as the
#     operator command;
#   * scripts/api/lib.sh, which used to be sourced here, is gone: nothing
#     sources it once generate.sh, check-contract-drift.sh and
#     check-client-paths.sh are all shims.
#
# `exec`, so the exit status is the generator's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/api/generate.py" "$@"

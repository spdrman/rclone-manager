#!/usr/bin/env bash
# The release-manifest binary-parity proof (#260), at the path everything
# already names.
#
# The proof itself is scripts/bdtools/release/verify_manifest_parity.py now
# (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing in places that are not this port's to move:
#
#   * .github/workflows/release.yml runs
#     `bash scripts/release/verify-manifest-parity.sh` before the push;
#   * docs/deployment.md and docs/release-branch.md tell an operator to run
#     it standalone to check a tree before cutting a release.
#
# `exec`, so the exit status is the proof's own: exit 2 is "the recorded
# hashes do not describe what this tree builds, and nothing has been
# pushed", and a wrapper that returned 0 there would publish the build the
# manifest does not describe.
#
# NOTE: no `cd` here, deliberately -- see the same note in
# record-release-hashes.sh. The target tree comes from `git rev-parse
# --show-toplevel` of the CURRENT WORKING DIRECTORY, not from this file's
# location.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/release/verify_manifest_parity.py" "$@"

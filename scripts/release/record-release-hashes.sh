#!/usr/bin/env bash
# The release-manifest generator (#82/B4.1, #174), at the path everything
# already names.
#
# The generator itself is scripts/rcmtools/release/record_release_hashes.py
# now (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing in places that are not this port's to move:
#
#   * docs/deployment.md, docs/release-branch.md,
#     docs/compliance/release-provenance.md and
#     docs/conformance/phase-4-matrix.md all tell an operator to run
#     `bash scripts/release/record-release-hashes.sh`, and
#     container/release-manifest.json's own `note` field names it, as does
#     the refusal text in verify-manifest-parity;
#   * scripts/rcmtools/tests/record_release_hashes_guards.py drives THIS
#     path (from inside a throwaway repository per refusal) rather than the
#     module directly, so the shim itself is exercised by every one of the
#     suite's eleven controls.
#
# That is why this is a real script and not a symlink: a fixture that
# overwrites the path must overwrite a stand-in, not the port underneath it.
#
# `exec`, so the exit status is the generator's own. In particular exit 2 --
# every #174 refusal -- must not be flattened, and the guard suite asserts
# the code before the message.
#
# NOTE: no `cd` here, deliberately. The generator resolves its target tree
# with `git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY,
# exactly as the bash original did, which is what lets the guard suite point
# it at a throwaway repository. Resolving the target from this file's own
# location instead would silently check the developer's real workspace; that
# is the defect this port was caught on.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/rcmtools/release/record_release_hashes.py" "$@"

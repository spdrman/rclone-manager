#!/usr/bin/env bash
# The canonical image push, sign and SBOM attestation (#88/B5.2), at the
# path everything already names.
#
# The script itself is scripts/bdtools/release/publish_image.py now (EPIC
# I, I1.6 / #672 / #697). This file stays because the path is load bearing
# in places that are not this port's to move:
#
#   * .github/workflows/release.yml runs
#     `bash scripts/release/publish-image.sh` as the publish step;
#   * docs/compliance/release-provenance.md, docs/release-branch.md and
#     docs/install.md all name it, the last of them for its own words about
#     a registry tag being a mutable pointer;
#   * scripts/bdtools/tests/publish_image_guards.py drives THIS path (from
#     inside a throwaway repository per refusal, through the GUARDS_ONLY=1
#     and DRY_RUN=1 seams) rather than the module directly, so the shim
#     itself is exercised by every one of the suite's twenty-one controls.
#
# That is why this is a real script and not a symlink: a fixture that
# overwrites the path must overwrite a stand-in, not the port underneath it.
#
# `exec`, so the exit status is the script's own. This is the one step in
# this repository that does something irreversible, all nine of its
# refusals exit 2, and the guard suite asserts the code before the message.
#
# NOTE: no `cd` here, deliberately -- see the same note in
# record-release-hashes.sh. The target tree comes from `git rev-parse
# --show-toplevel` of the CURRENT WORKING DIRECTORY, not from this file's
# location.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/release/publish_image.py" "$@"

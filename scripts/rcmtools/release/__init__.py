"""The release pipeline's three operator scripts (EPIC I, I1.6 / #672).

Ported off `scripts/release/*.sh` (issue #672's release domain). Three
programs, one for each stage of cutting a release, previously three bash
files sharing no code and each reimplementing its own `sha256_of`:

  * `verify_manifest_parity` -- proves `container/release-manifest.json`
    describes the image this tree builds, by building it and hashing the
    binaries out of it (#260).
  * `record_release_hashes` -- builds both architectures, hashes the two
    shipped binaries out of each, and writes the manifest (#82/B4.1,
    #174).
  * `publish_image` -- pushes the multi-architecture image, reads its
    registry digests back, signs it and attests its SBOM (#88/B5.2).

This domain's specific hazard, stated once because it binds every guard
in all three modules: a publish-time guard that stops guarding fails as
a BAD RELEASE, not a red build. `publish_image`'s guards run against a
throwaway `git init` repository in
`scripts/rcmtools/release/test_guards.py`, through the same
`GUARDS_ONLY=1`/`DRY_RUN=1` seam the bash scripts used, because a refusal
exercised for the first time at release time is a refusal nobody has
watched work.

The three bash scripts and their two guard test files
(`scripts/tests/record-release-hashes-guards.test.sh`,
`scripts/tests/publish-image-guards.test.sh`) are deleted, not kept as
shims: both guard tests invoke the scripts at their literal
`scripts/release/*.sh` paths with an env-var seam, which are real
callers rather than `scripts/tests/ci-local-gate.test.sh` fabrications,
so there is no fabrication list entry keeping a bash exec-shim alive
here. Every caller -- `.github/workflows/release.yml`, `ci-local.sh`,
the two guard tests, and the docs that give the exact operator command
-- is updated to invoke the Python entry point instead.
"""

from __future__ import annotations

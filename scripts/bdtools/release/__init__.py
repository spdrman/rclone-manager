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
a BAD RELEASE, not a red build. Those guards run against a throwaway
`git init` repository per refusal, in
`scripts/bdtools/tests/record_release_hashes_guards.py` and
`scripts/bdtools/tests/publish_image_guards.py`, through the same
`GUARDS_ONLY=1`/`DRY_RUN=1` seams the bash scripts used, because a
refusal exercised for the first time at release time is a refusal
nobody has watched work.

RECONCILIATION NOTE (#697). This domain was ported twice, by two
routes, and the difference was a real disagreement about one fact:

  * one route kept `scripts/tests/{record-release-hashes,publish-image}-guards.test.sh`
    as bash exec shims, on the evidence that
    `scripts/tests/ci-local-gate.test.sh` FABRICATES files at those two
    literal paths;
  * the other deleted them, on the evidence that the guard tests are
    real callers of `scripts/release/*.sh` through an env-var seam, not
    fabrications.

Both facts are true, about different files. `ci-local-gate.test.sh`
fabricates the two GUARD TEST paths (see the loop over
`record-release-hashes-guards publish-image-guards` in its synthetic-tree
fixture), and those guard tests really do drive the three release
SCRIPTS. So every bash path here stays as a real `exec` shim -- the
guard tests' and the fixture's, deleting either of which turns a live
fixture red -- and the suites drive the shim rather than the module, so
the shim is itself exercised by all thirty-two controls. That is the
same shape as `scripts/bdtools/api/selftest.py` driving
`scripts/api/check-contract-drift.sh`.

Nothing calls a second Python copy of those suites: the two under
`scripts/bdtools/tests/` are the only ones, and no assertion from
either route was dropped.
"""

from __future__ import annotations

"""Issue #458: does every anchored selftest's mutation anchor still name
code that is actually in the tree?

`scripts/compat/selftest.sh`, `scripts/conformance/selftest.sh`,
`scripts/race/selftest.sh`, `scripts/format/selftest.sh`,
`scripts/docs/selftest.sh` and `scripts/retention/selftest.sh` each plant
deliberate violations to prove their own gate can go red. Every plant is
anchored to a verbatim copy of product source living in a file the
author of the product change never opens, so a refactor drifts the
anchor and the mutation stops planting anything.

One entry point:

  * `check_anchors` -- every one of the six `--check-anchors` above, in
    one run, dry-running each and reporting every stale one rather than
    stopping at the first.

Ported from `scripts/selftest/check-anchors.sh` under EPIC I (#672 /
#458). `scripts/selftest/check-anchors.sh` stays as a real, runnable
shim: `scripts/ci-local.sh` runs it by that literal path, and
`scripts/tests/ci-local-gate.test.sh` fabricates a stand-in there.
"""

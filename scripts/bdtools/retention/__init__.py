"""FR-20's retention apply (issue #602): the domain that decides what
this product deletes.

One entry point:

  * `selftest` -- positive controls for the apply evidence in
    `core/service/retentionapplyevidence_test.go` and
    `core/internal/retention/lastknowngoodpresence_test.go`. Every claim
    that evidence makes is mutation-tested here against a real copy of
    the working tree: a deliberate violation is planted in a real
    product file, the narrowed gate runs, and it must fail AND name the
    check whose promise the violation broke.

There is no `check-*.py` here because retention has no standing gate of
its own outside the Go test suite; `core/service/retentionapplyevidence_test.go`
and its siblings ARE the gate, and this package's only job is proving
they can go red.

Ported from `scripts/retention/selftest.sh` under EPIC I (#672 / #602).
`scripts/retention/selftest.sh` stays as a real, runnable shim:
`scripts/ci-local.sh` runs `bash scripts/retention/selftest.sh` by that
literal path, and `scripts/tests/ci-local-gate.test.sh` fabricates a
stand-in there.
"""

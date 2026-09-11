"""Issue #417's formatting gate: is every Go file in this repository
gofmt-clean, and does the gate actually notice when one is not?

Two entry points:

  * `check_gofmt`  -- the sweep itself, over every tracked `.go` file via
    `git ls-files`, closing the blind spot `.golangci.yml`'s own gofmt
    formatter cannot reach: two Go files in this repository live outside
    every module and outside `go.work`
    (`scripts/api/gen-bindings.go`, `scripts/architecture/ownership.go`),
    so no per-module `golangci-lint` run has ever been able to see them.
  * `selftest`     -- positive controls: the real tree clean, an
    unformatted file planted inside a synthetic module, one outside every
    module, one staged but uncommitted, and (when `golangci-lint` is on
    PATH) `.golangci.yml`'s own formatter shown to fire and to clear.

Ported from `scripts/format/*.sh` under EPIC I (#672 / #662).
`scripts/format/check-gofmt.sh` and `scripts/format/selftest.sh` stay as
real, runnable shims: `scripts/tests/ci-local-gate.test.sh` fabricates a
stand-in at both literal paths.
"""

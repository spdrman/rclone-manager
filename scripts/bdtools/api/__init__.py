"""The /api/v1 contract gates (issue #166), under EPIC I (#672 / #662).

Three entry points, all reading the same three constants from `lib`:

  * `generate`             -- regenerates the two checked-in bindings from
    `api/v1/openapi.json`. Run this after editing the contract.
  * `check_contract_drift` -- the drift gate: the checked-in bindings match
    what the contract generates, no implementation type reaches the public
    schema, the gate that gates actually runs these checks, and the
    contract's `security` blocks agree with its own
    `x-authenticated`/`x-csrf-required` extensions.
  * `check_client_paths`   -- the other half of #166: every request path
    `ui/shared/src/api/client.ts` builds is a declared operation, checked
    statically with no npm install (issue #211).
  * `selftest`             -- mutation-tests every rule the two gates above
    enforce against the real tree, plus the Go handler-drift controls that
    need `go test` and cannot be a shell/Python scan.

Ported from `scripts/api/*.sh` under EPIC I (#672). `scripts/api/gen-bindings.go`
stays exactly where it is and exactly what it is: a standalone, module-less
Go program (like `scripts/architecture/ownership.go` beside it), invoked with
`GOWORK=off go run`, never imported. Porting a generator that produces Go
source to Python would still need `go build` to validate its own output, so
nothing is gained and a second implementation of the contract-to-Go mapping
is exactly the kind of drift this domain exists to prevent.

`scripts/api/generate.sh`, `scripts/api/check-contract-drift.sh`,
`scripts/api/check-client-paths.sh` and `scripts/api/selftest.sh` all stay as
real, runnable shims (`exec python3 .../bdtools/api/<module>.py "$@"`), not
symlinks: `scripts/tests/ci-local-gate.test.sh` fabricates stand-ins at the
three check paths' literal locations, and `scripts/api/selftest.sh` itself
drives `scripts/api/generate.sh` as a subprocess (one of its own mutants
regenerates through the shim). `scripts/api/lib.sh` is deleted: it was
sourced only by the three files above, and once those are shims nothing
sources it.
"""

"""The suites that hold every other contract in this repository (#672, #697).

`scripts/tests/*.test.sh` is not a domain with a `check-*.sh` of its own:
each file here IS the check, driving another script (`scripts/ci-local.sh`,
an `scripts/e2e/*` wrapper, a `scripts/release/*` guard) from the outside
and asserting on its exit status and output. `ci-local-gate.test.sh` alone
carries 207 checks and `e2e-help.test.sh` 29; between them they hold the
gate that every other change in this repository has to pass.

`testkit` is the one file shared across every module here: the
pass/fail/tally vocabulary every `*.test.sh` file reinvented byte-for-byte
(`checks=0; failures=0; pass() { ... }; fail() { ... }`), and the
subprocess-capture helper that reads a status the way `out="$(cmd)";
status=$?` did. It is intentionally NOT `bdtools.harness`: harness's own
docstring says a domain widens it with a real caller in the same change,
and `sh(check=True)`'s propagate-or-raise shape is wrong for a test
driver, which has to read an EXPECTED-nonzero status rather than treat it
as a refusal.

Ported from `scripts/tests/*.test.sh` under EPIC I (#672 / #697). Standard
library only, Python 3.8 or newer, for the reason
`scripts/bdtools/__init__.py` gives.
"""

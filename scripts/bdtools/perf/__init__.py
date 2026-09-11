"""The Phase 6 performance baseline and gate (issue #165, EPIC B #81's
performance contract).

Two entry points, both driven by `docs/perf/gate.json`:

  * `check_baseline`  -- the gate itself, in presence mode (is a complete
    baseline checked in for the designated host?) and compare mode (does a
    freshly captured record beat it?). This is the one `scripts/ci-local.sh`
    and `.github/workflows/ci.yml` run.
  * `selftest`        -- positive controls, mutating a real baseline record
    one property at a time and requiring the gate to refuse each mutation.

`capture_baseline` is a third module in here but not, on its own terms, a
"check": it drives two Go test harnesses and an optional `docker build` to
produce the record the two above read, and it is never run by an automated
gate (`docs/perf/README.md` says why: EPIC B #81 wants a dedicated stable
benchmark environment, and a shared CI runner is not that environment).

`scripts/perf/hostid.sh` stays exactly where it is and exactly what it is:
a bash library, SOURCED (not executed) both by the two perf shell scripts
this replaces and directly by
`apps/generic/tests/dockercli/imagesize_test.go`, which shells out to
`bash -c "source scripts/perf/hostid.sh && perf::host_id"` for the reason
its own comment gives -- the host-id slug is a rule, and a second
implementation of a rule in a second language is a second thing to keep in
step. `capture_baseline.py` below calls the same one implementation the
same way, rather than adding a third.

Ported from `scripts/perf/*.sh` under EPIC I (#672 / #662). Standard
library only, Python 3.8 or newer, for the reason
`scripts/bdtools/__init__.py` gives. Unlike the bash it replaces, none of
these three needs `jq` for its own JSON handling -- `check_baseline` and
`selftest` parse and emit JSON with the standard library -- though
`capture_baseline` still requires it transitively, because
`scripts/perf/hostid.sh`'s `perf::host_json` shells out to it.
"""

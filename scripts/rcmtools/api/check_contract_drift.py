#!/usr/bin/env python3
"""The /api/v1 contract drift gate (issue #166).

`api/v1/openapi.json` is the authoritative definition of the boundary. Four
things can drift away from it, and this script catches all of them:

  1. A GENERATED FILE that was hand-edited. Both bindings carry a DO NOT
     EDIT banner, but a banner is a request, not a check. This regenerates
     into a temporary directory and compares byte for byte, the same
     generate-then-diff shape `docs/conformance/phase-4-matrix.md` already
     uses.

  2. An IMPLEMENTATION TYPE reaching the public schema. #81's standing
     constraint forbids rclone, SQLite, filesystem and provider SDK types on
     the public API, both because it leaks implementation detail and because
     it widens what an external integrator or a store reviewer can reach.
     If a public shape leaks one, the contract is wrong, not this check.

  3. THE GATE ITSELF NOT RUNNING. Both rules above were wired only into
     `.github/workflows/ci.yml`, which runs only on a pull request into
     `release` (#575), so `scripts/ci-local.sh` has to run them too or they
     run on nothing bound for `main` (PR #194 review, M1). A check nothing
     invokes is indistinguishable from a check that does not exist, so the
     invocation is checked here too.

  4. THE CONTRACT DISAGREEING WITH ITSELF. Every operation declares whether
     it needs a session and the double-submit token TWICE: once in the
     standard OpenAPI `security` block an external consumer or an
     off-the-shelf generator reads, and once in the `x-authenticated` and
     `x-csrf-required` extensions this repository's own generator obeys.
     Nothing compared them, and they had already parted on fifteen of
     forty-six operations (PR #546 review). A constant declared twice with
     nothing comparing the two is a constant that drifts, which is this
     repository's own rule about the wire, turned on the document.

What this deliberately does NOT check is whether the Go HANDLERS still match
the bindings; that needs reflection over unexported types, so it lives in
`apps/common/webhost/contract_test.go` and `apps/common/auth/local/contract_test.go`
and runs under `go test`. The two halves are independent: a handler can
drift with the bindings intact, and a binding can be hand-edited with the
handlers intact.

`selftest.py` mutation-tests every rule below against the real tree.

# PORTED-CHECK HAZARD NOTE

the schema scan cannot pass having verified nothing
  hazard in bash:   `identifiers=$(python3 ... )`; an empty result is still
                    a zero-exit command substitution under `set -e`, so
                    nothing forced `[ -z "$identifiers" ]` to be checked --
                    a contract with no `components/schemas` and no `paths`
                    (e.g. one gutted by a bad merge) would make the forbidden-
                    pattern grep match nothing and print "ok: 0 public-schema
                    identifiers carry no ... type", which reads as a pass.
  hazard in python: STILL EXISTS in the same shape: `identifiers` is a plain
                    Python list, and an empty one iterates zero times in the
                    same way an empty bash loop does. The port keeps the
                    same explicit `if not identifiers` guard before the scan
                    counts anything.
  held by:          the `if not identifiers` check in `body()`, and
                    `selftest.py`'s `contract-with-no-operations` control.
  held by (bash predecessor): the same explicit `[ -z "$identifiers" ]`
                    check that predates this port.

the contract's security blocks cannot silently compare nothing
  hazard in bash:   the embedded `python3 - "$API_CONTRACT" <<'PYSEC'` block
                    ran as a SEPARATE process whose exit status `set -e`
                    would only see through the `if ... then ... else` around
                    it -- so its own `sys.exit(1)` on `checked == 0` had to
                    be threaded back through that `if`, and a refactor that
                    turned it into a bare `python3 ... ; note ...` (dropping
                    the `if`) would have `set -e` kill the whole script on
                    any refusal, which is worse than a silent pass but still
                    not the intended "accumulate and report" behaviour.
  hazard in python: GONE as a subprocess-boundary problem: this port runs
                    the same logic in-process, as a plain function returning
                    a list of problems, with no exit status to relay across
                    a process boundary at all. The `checked == 0` guard is
                    now just another entry appended to that list.
  held by:          the `if checked == 0` branch in
                    `_security_cross_check()`, and `selftest.py`'s
                    `contract-with-no-security-at-all` control.

a mutation that fails the build is not silently read as "no drift"
  hazard in bash:   `api::generate` is invoked as `if ! api::generate ...;
                    then note FAIL; exit 1; fi` -- a `go run` that fails for
                    an UNRELATED reason (a missing Go toolchain, say) reads
                    exactly the same as a contract the generator refuses to
                    read, and the script exits 1 either way with a message
                    that says "the generator refused", so this is not
                    actually a hazard distinguishable in the bash: both
                    paths were already fused into one exit.
  hazard in python: not applicable; `lib.generate()` returns a
                    `CompletedProcess` and this port checks `.returncode`
                    the same way, fused the same way.
  held by:          `selftest.py`'s `contract-unparseable` control, which
                    plants a contract the generator cannot read at all and
                    requires the message to say "the generator refused".
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness
from rcmtools.api import lib

PROGRAM = "check-contract-drift"

# The identifiers a public schema is made of: schema names, property names,
# enum values, path segments and operation ids. Descriptions are excluded on
# purpose - a schema is allowed to SAY "this is not an rclone remote", and a
# check that could not tell the two apart would be watered down until it
# fired on nothing, which is how the ADMIN_PASSWORD scanner in this
# repository failed before.
_SKIP_KEYS = ("description", "summary", "why", "title", "$comment")

# Case-insensitive, and deliberately without a word boundary: \b never
# matches between an underscore and a letter, which is how a previous
# scanner in this repository missed ADMIN_PASSWORD. A substring match is
# what catches rclone_remote, sqlite_path and RcloneConfig alike.
_FORBIDDEN_PATTERN = re.compile(
    r"rclone|sqlite|sqlite3|\.db\b|dsn|/var/lib|/etc/|gorm|database/sql|"
    r"synology_api|truenas_api|ugos_sdk|dsm_api|unraid_plugin|omv_rpc",
    re.IGNORECASE,
)

_VERBS = ("get", "put", "post", "delete", "options", "head", "patch", "trace")

# The pairs the security cross-check compares. csrf and csrfCookie are the
# two halves of one double-submit requirement and are always required
# together, so both are read against the same extension: a document that
# asked for the header without naming the cookie a client has to echo would
# be describing a requirement nobody can satisfy, which is the state this
# whole rule exists to have caught.
_PAIRS = (("session", "x-authenticated"), ("csrf", "x-csrf-required"), ("csrfCookie", "x-csrf-required"))


def _walk_identifiers(node: Any, path: str, out: list[str]) -> None:
    if isinstance(node, dict):
        for key, value in node.items():
            if key in _SKIP_KEYS:
                continue
            out.append(f"{path}/{key}\t{key}")
            _walk_identifiers(value, f"{path}/{key}", out)
    elif isinstance(node, list):
        for i, value in enumerate(node):
            _walk_identifiers(value, f"{path}[{i}]", out)
    elif isinstance(node, str):
        out.append(f"{path}\t{node}")


def _security_cross_check(doc: dict[str, Any]) -> tuple[list[str], str | None]:
    """(problems, ok-message). Exactly one of the two is meaningful."""
    schemes = set((doc.get("components") or {}).get("securitySchemes") or {})

    problems: list[str] = []
    checked = 0

    for path, item in (doc.get("paths") or {}).items():
        for verb, op in item.items():
            if verb.lower() not in _VERBS:
                continue
            where = f"{verb.upper()} {path} ({op.get('operationId', '?')})"
            security = op.get("security")
            # Fail closed on a shape this rule cannot decide rather than
            # skipping it. A list of alternatives is legal OpenAPI and this
            # contract has never declared one, so meeting one means this
            # rule needs rewriting, not relaxing, and a silent skip here
            # would be indistinguishable from a comparison that passed.
            if not isinstance(security, list) or len(security) != 1 or not isinstance(security[0], dict):
                problems.append(
                    f"{where}: security is {security!r}. This rule reads exactly one requirement set per "
                    "operation, which is all this contract has ever declared; anything else needs the rule "
                    "rewritten rather than skipped."
                )
                continue
            required = security[0]
            unknown = sorted(set(required) - schemes)
            if unknown:
                problems.append(
                    f"{where}: names security scheme(s) {', '.join(unknown)}, which "
                    "components.securitySchemes does not declare."
                )
            checked += 1

            for scheme, extension in _PAIRS:
                declared = scheme in required
                extended = op.get(extension) is True
                if declared != extended:
                    problems.append(
                        f"{where}: security {'declares' if declared else 'does not declare'} {scheme!r}, and "
                        f"{extension} is {op.get(extension)!r}. These are two declarations of one fact, and "
                        "both are read: an external consumer implements `security`, and "
                        "scripts/api/gen-bindings.go obeys the extension. Say the same thing in both."
                    )

    if checked == 0:
        return (
            [
                "no operation declared a security requirement at all, so this rule compared nothing "
                "and would pass vacuously"
            ],
            None,
        )

    if problems:
        return problems, None

    return (
        [],
        f"ok: {checked} operation(s) say the same thing about a session and about CSRF in `security` and "
        "in their extensions",
    )


def _compare(problems: list[str], checked_in: Path, regenerated: Path, language: str) -> None:
    if not checked_in.is_file():
        msg = f"FAIL: {checked_in} is missing. Run scripts/api/generate.sh."
        print(msg, file=sys.stderr)
        problems.append(msg)
        return
    diff = subprocess.run(["diff", "-u", str(checked_in), str(regenerated)], capture_output=True, text=True)
    if diff.returncode != 0:
        msg = f"FAIL: the checked-in {language} binding does not match what {lib.API_CONTRACT} generates."
        print(msg, file=sys.stderr)
        print(file=sys.stderr)
        print(f"  If you edited {checked_in} by hand, do not: it is generated output.", file=sys.stderr)
        print("  If you edited the contract, run scripts/api/generate.sh and commit the result.", file=sys.stderr)
        print(file=sys.stderr)
        for line in diff.stdout.splitlines():
            print(f"    {line}", file=sys.stderr)
        problems.append(msg)
        return
    print(f"  ok: {checked_in} matches the contract")


def body() -> int:
    if not lib.API_CONTRACT.is_file():
        harness.die(f"{lib.API_CONTRACT} does not exist, so /api/v1 has no authoritative definition at all.")

    problems: list[str] = []

    with tempfile.TemporaryDirectory(prefix="rclone-manager-api-drift.") as tmp_str:
        tmp = Path(tmp_str)
        gen_go, gen_ts = tmp / "contract.gen.go", tmp / "contract.ts"

        print(f"==> regenerating the bindings from {lib.API_CONTRACT}")
        proc = lib.generate(gen_go, gen_ts)
        if proc.returncode != 0:
            # Nothing below can mean anything without generated output to
            # compare against, so this one is fatal rather than accumulated.
            harness.die(
                f"the generator refused {lib.API_CONTRACT}:",
                *(proc.stdout or "").splitlines(),
                *(proc.stderr or "").splitlines(),
            )
        for line in (proc.stdout or "").splitlines():
            print(f"    {line}")

        # ---- 1. the checked-in bindings are what the contract generates ---
        _compare(problems, lib.API_GO_BINDING, gen_go, "Go")
        _compare(problems, lib.API_TS_BINDING, gen_ts, "TypeScript")

    # ---- 2. no implementation type reaches the public schema --------------
    print("==> scanning the public schema for implementation types")
    doc = json.loads(lib.API_CONTRACT.read_text())
    identifiers: list[str] = []
    _walk_identifiers(doc.get("components", {}).get("schemas", {}), "components/schemas", identifiers)
    _walk_identifiers(doc.get("paths", {}), "paths", identifiers)

    if not identifiers:
        msg = "FAIL: the schema scan found no identifiers at all, so it verified nothing."
        print(msg, file=sys.stderr)
        problems.append(msg)
    else:
        hits = [line for line in identifiers if _FORBIDDEN_PATTERN.search(line)]
        if hits:
            msg = (
                "FAIL: an implementation type reached the public schema. #81's standing constraint forbids "
                "rclone, SQLite, filesystem and provider SDK types on /api/v1, and the fix is the contract, "
                "not this check:"
            )
            print(msg, file=sys.stderr)
            for h in hits:
                print(f"    {h}", file=sys.stderr)
            problems.append(msg)
        print(
            f"  ok: {len(identifiers)} public-schema identifiers carry no rclone, SQLite, filesystem or "
            "provider SDK type"
        )

    # ---- 3. the gate that actually gates runs both of these ----------------
    print("==> the pre-commit gate runs these checks")
    gate = Path("scripts/ci-local.sh")
    if not gate.is_file():
        msg = f"FAIL: {gate} does not exist, so nothing can be said about whether these checks run on a commit."
        print(msg, file=sys.stderr)
        problems.append(msg)
    else:
        gate_text = gate.read_text()
        for invoked in (
            "scripts/api/check-contract-drift.sh",
            "scripts/api/check-client-paths.sh",
            "scripts/api/selftest.sh",
        ):
            if re.search(rf"^[ \t]*bash {re.escape(invoked)}", gate_text, re.MULTILINE):
                print(f"  ok: {gate} runs {invoked}")
            else:
                msg = (
                    f"FAIL: {gate} does not run {invoked}. .github/workflows/ci.yml runs only on a pull "
                    "request into `release` (#575), so a check that lives only there runs on nothing bound "
                    f"for main. Add `bash {invoked}` to {gate}."
                )
                print(msg, file=sys.stderr)
                problems.append(msg)

    # ---- 4. the contract does not disagree with itself ---------------------
    print("==> the contract's security blocks agree with its own extensions")
    sec_problems, sec_ok = _security_cross_check(doc)
    if sec_problems:
        msg = "FAIL: the contract's security blocks disagree with its own x-authenticated/x-csrf-required extensions."
        print(msg, file=sys.stderr)
        for p in sec_problems:
            print(f"    {p}", file=sys.stderr)
        problems.append(msg)
    else:
        print(f"  {sec_ok}")

    if problems:
        harness.die("How to regenerate, and what a drift failure means: docs/api/contract.md")

    print(f"OK: the /api/v1 bindings match {lib.API_CONTRACT}, and no implementation type reaches the public schema.")
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    if argv:
        print(f"usage: {sys.argv[0]}", file=sys.stderr)
        return harness.EXIT_USAGE
    return harness.finish(body)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

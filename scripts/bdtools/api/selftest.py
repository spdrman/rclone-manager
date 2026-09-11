#!/usr/bin/env python3
"""Positive controls for the /api/v1 contract gates (issue #166).

Every rule those gates enforce is a negative assertion: "the generated
bindings match the contract", "no implementation type reaches the public
schema", "no handler shape drifted", "no route exists outside the
contract", "no error code is emitted that the registry does not know". A
negative assertion that has never been seen to fail is indistinguishable
from one that cannot fail, and this repository has been bitten by exactly
that twice: a scanner whose `\\b` never matched between `_` and `p`, and a
self-test that "caught" every mutation because the check script was
missing from the copy it ran in.

So each rule is mutation-tested against the REAL tree: a copy of the
working tree gets one deliberate violation planted in a real file, the
check runs, and it must fail AND print the message that names the planted
reason. Asserting the message, not merely the exit code, is what stops a
check that failed for an unrelated reason from reading as a pass.

The Go controls run `go test`, which is slower than the static checks but
is the only thing that can prove a HANDLER drift is caught: that check needs
reflection over unexported types and cannot be a shell scan.

Not covered here, on purpose: the TypeScript side's own consumption checks
(`ui/shared/src/api/contract.conformance.test.ts`). Those need an installed
npm workspace, which this script deliberately does not build; they run in
the ordinary `npm test` job. What IS covered here is the TypeScript DRIFT
rule, which is the one CI has to fail on, in both directions: a hand-edited
generated module, and a contract change nobody regenerated.

Ported from `scripts/api/selftest.sh`. Every control below runs the
mutant's OWN copy of `scripts/api/check-contract-drift.sh` /
`check-client-paths.sh` (now shims onto this package), by relative path
with the mutant directory as `cwd` -- exactly as the bash drove
`bash scripts/api/check-contract-drift.sh` from inside the copy. That is
what makes a mutation self-contained: the copied shim execs the copied
`bdtools/api/check_contract_drift.py`, which resolves its own path at
runtime, so nothing here can accidentally check the real tree instead of
the mutant.

# PORTED-CHECK HAZARD NOTE

a check invoked against a copy that is missing the check itself is not caught
  hazard in bash:   `mutant()` copies `git ls-files -z --cached --others
                    --exclude-standard`, which is every tracked AND
                    untracked file -- not `git ls-files` alone. A version of
                    this file that copied only tracked files would silently
                    "catch" every mutation whenever the check under test was
                    itself uncommitted, because `bash scripts/api/check-...`
                    would fail with "No such file or directory" inside the
                    copy, exit non-zero, and read as the gate correctly
                    refusing the plant.
  hazard in python: STILL EXISTS in the same shape: `_mutant()` below is a
                    straight port of the same `--cached --others
                    --exclude-standard` file list, and a future edit that
                    swapped it for `git ls-files` alone would reproduce the
                    exact bug this comment warns about, silently.
  held by:          `_mutant()`'s literal `--cached --others
                    --exclude-standard` argument list, unchanged from the
                    bash, and every `expect_check_fails` call insisting on
                    the PLANTED message rather than merely a non-zero exit
                    -- a copy missing the check entirely fails with a
                    shell/Python "No such file" message, not the planted
                    one, so that failure mode is itself distinguishable
                    from a real catch even if the copy were ever wrong.

a subprocess status this file does not read is not a status this file can be fooled by
  hazard in bash:   none beyond the general one `harness.py`'s own
                    docstring describes: `expect_check_fails` ran
                    `(cd "$dir" && "$@")` under the OUTER script's
                    `set -euo pipefail`, but inside an `if`, so the inner
                    command's exit status was always read deliberately
                    rather than propagated by accident -- this file's own
                    hazard is the mutant-copy one above, not this one.
  hazard in python: not applicable; `subprocess.run(..., check=False)`
                    below reads `.returncode` explicitly, the same
                    deliberate read.
  held by:          `_run()`'s explicit `.returncode` check.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any, Callable

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness

PROGRAM = "api selftest"


class Tally:
    def __init__(self) -> None:
        self.passed = 0
        self.failed = 0


def _mutant(root: Path, tmp: Path, name: str) -> Path:
    """Copy `root`'s tracked AND untracked (not ignored) files into `tmp/name`.

    `--cached --others --exclude-standard`, not plain `git ls-files`: the
    copy has to include files that are present but not yet committed,
    because the checks being tested are themselves usually uncommitted
    while they are being written. Copying tracked files only is what
    produced a self-test elsewhere in this repository that silently
    "caught" every mutation, because the check script it invoked did not
    exist in the copy at all.
    """
    dest = tmp / name
    dest.mkdir(parents=True)
    files = harness.sh_out(
        ["git", "-C", str(root), "ls-files", "--cached", "--others", "--exclude-standard"]
    ).splitlines()
    for rel in files:
        src = root / rel
        dst = dest / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        if src.is_symlink():
            os.symlink(os.readlink(src), dst)
        elif src.is_file():
            shutil.copy2(src, dst)
    harness.sh(["git", "init", "-q"], cwd=dest)
    harness.sh(["git", "add", "-A"], cwd=dest)
    harness.sh(
        [
            "git",
            "-c",
            "user.email=selftest@example.invalid",
            "-c",
            "user.name=selftest",
            "commit",
            "-q",
            "-m",
            "selftest baseline",
        ],
        cwd=dest,
    )
    return dest


def _run(argv: list[str], cwd: Path) -> tuple[int, str]:
    proc = subprocess.run(argv, cwd=cwd, capture_output=True, text=True)
    return proc.returncode, (proc.stdout or "") + (proc.stderr or "")


def _expect_check_fails(tally: Tally, label: str, cwd: Path, expect: str, argv: list[str]) -> None:
    rc, out = _run(argv, cwd)
    if rc == 0:
        print(f"SELFTEST FAIL: {label}. The check PASSED against a planted violation.", file=sys.stderr)
        for line in out.splitlines():
            print(f"    {line}", file=sys.stderr)
        tally.failed += 1
    elif expect not in out:
        print(f"SELFTEST FAIL: {label}. The check failed, but not for the planted reason.", file=sys.stderr)
        print(f"    expected its output to contain: {expect}", file=sys.stderr)
        for line in out.splitlines():
            print(f"    {line}", file=sys.stderr)
        tally.failed += 1
    else:
        print(f"  ok (caught): {label}")
        tally.passed += 1


def _write(path: Path, text: str) -> None:
    path.write_text(text)


def _sed_replace(path: Path, old: str, new: str) -> None:
    text = path.read_text()
    if old not in text:
        raise ValueError(f"anchor not found in {path}: {old!r}")
    path.write_text(text.replace(old, new, 1))


def _json_edit(path: Path, fn: Callable[[dict[str, Any]], None]) -> None:
    doc = json.loads(path.read_text())
    fn(doc)
    path.write_text(json.dumps(doc, indent=2) + "\n")


def body(root: Path) -> int:
    tally = Tally()

    with tempfile.TemporaryDirectory(prefix="backupd-api-selftest.") as tmp_str:
        tmp = Path(tmp_str)

        drift = ["bash", "scripts/api/check-contract-drift.sh"]
        client_paths = ["bash", "scripts/api/check-client-paths.sh"]

        print("==> negative control: the gates are clean on the real tree")
        rc, out = _run(drift, root)
        if rc != 0:
            harness.die(
                "check-contract-drift FAILED on the unmutated tree, so every control below is meaningless:", out
            )
        print("  ok (clean): check-contract-drift")
        rc, out = _run(client_paths, root)
        if rc != 0:
            harness.die("check-client-paths FAILED on the unmutated tree, so every control below is meaningless:", out)
        print("  ok (clean): check-client-paths")

        print()
        print("==> generated output is generated, not edited")

        d = _mutant(root, tmp, "go-binding-hand-edited")
        _sed_replace(d / "core/apicontract/contract.gen.go", 'json:"config_revision"', 'json:"configRevision"')
        _expect_check_fails(
            tally, "a hand edit to the generated Go binding", d, "the checked-in Go binding does not match", drift
        )

        d = _mutant(root, tmp, "ts-binding-hand-edited")
        _sed_replace(d / "ui/shared/src/api/generated/contract.ts", "plan_id: string;", "plan_id?: string;")
        _expect_check_fails(
            tally,
            "a hand edit to the generated TypeScript binding",
            d,
            "the checked-in TypeScript binding does not match",
            drift,
        )

        d = _mutant(root, tmp, "ts-binding-missing")
        (d / "ui/shared/src/api/generated/contract.ts").unlink()
        _expect_check_fails(tally, "a deleted TypeScript binding", d, "is missing. Run scripts/api/generate.sh", drift)

        d = _mutant(root, tmp, "contract-changed-without-regenerating")
        text = (d / "api/v1/openapi.json").read_text()
        text = text.replace('"known_hosts_line"', '"known_hosts"')
        (d / "api/v1/openapi.json").write_text(text)
        _expect_check_fails(
            tally,
            "a contract change nobody regenerated the bindings for",
            d,
            "the checked-in TypeScript binding does not match",
            drift,
        )
        _expect_check_fails(
            tally,
            "the same change, caught by go test's own digest check",
            d,
            "but the generated bindings were made from",
            [
                "bash",
                "-c",
                "cd apps/common && go test -count=1 "
                "-run TestContract_TheBindingsWereGeneratedFromThisContract ./webhost/",
            ],
        )

        print()
        print("==> the public schema carries no implementation type")

        d = _mutant(root, tmp, "schema-leaks-sqlite")

        def _leak_sqlite(doc: dict[str, Any]) -> None:
            schema = doc["components"]["schemas"]["VersionResponse"]
            schema["properties"]["local_sqlite_path"] = {"type": "string"}
            schema["required"].append("local_sqlite_path")

        _json_edit(d / "api/v1/openapi.json", _leak_sqlite)
        _expect_check_fails(
            tally,
            "a SQLite implementation type on a public schema",
            d,
            "an implementation type reached the public schema",
            drift,
        )

        d = _mutant(root, tmp, "schema-leaks-provider-sdk")

        def _leak_sdk(doc: dict[str, Any]) -> None:
            doc["components"]["schemas"]["CapabilitiesResponse"]["properties"]["ugos_sdk_handle"] = {"type": "string"}
            doc["components"]["schemas"]["CapabilitiesResponse"]["required"].append("ugos_sdk_handle")

        _json_edit(d / "api/v1/openapi.json", _leak_sdk)
        _expect_check_fails(
            tally,
            "a provider SDK type on a public schema",
            d,
            "an implementation type reached the public schema",
            drift,
        )

        print()
        print("==> the contract says the same thing about a session and a token twice")

        d = _mutant(root, tmp, "contract-security-drops-the-csrf-scheme")

        def _drop_csrf(doc: dict[str, Any]) -> None:
            del doc["paths"]["/backup-sets/{source}/{set}"]["delete"]["security"][0]["csrf"]

        _json_edit(d / "api/v1/openapi.json", _drop_csrf)
        _expect_check_fails(
            tally,
            "an operation whose security block forgot the CSRF token it requires",
            d,
            "security does not declare 'csrf', and x-csrf-required is True",
            drift,
        )

        d = _mutant(root, tmp, "contract-authenticated-extension-flipped")

        def _flip_auth(doc: dict[str, Any]) -> None:
            doc["paths"]["/auth/logout"]["post"]["x-authenticated"] = False

        _json_edit(d / "api/v1/openapi.json", _flip_auth)
        _expect_check_fails(
            tally,
            "an operation whose x-authenticated no longer matches its security block",
            d,
            "security declares 'session', and x-authenticated is False",
            drift,
        )

        d = _mutant(root, tmp, "contract-security-shape-this-rule-cannot-read")

        def _unreadable_security(doc: dict[str, Any]) -> None:
            del doc["paths"]["/backup-sets"]["post"]["security"]

        _json_edit(d / "api/v1/openapi.json", _unreadable_security)
        _expect_check_fails(
            tally,
            "an operation whose security block this rule cannot read",
            d,
            "This rule reads exactly one requirement set per operation",
            drift,
        )

        d = _mutant(root, tmp, "contract-with-no-security-at-all")
        verbs = ("get", "put", "post", "delete", "options", "head", "patch", "trace")

        def _strip_all_security(doc: dict[str, Any]) -> None:
            for item in doc["paths"].values():
                for verb, op in item.items():
                    if verb in verbs:
                        op.pop("security", None)

        _json_edit(d / "api/v1/openapi.json", _strip_all_security)
        _expect_check_fails(
            tally,
            "a contract with no security block left to compare anything against",
            d,
            "would pass vacuously",
            drift,
        )

        print()
        print("==> a gate that inspected nothing refuses")

        d = _mutant(root, tmp, "contract-with-no-operations")
        _json_edit(d / "api/v1/openapi.json", lambda doc: doc.__setitem__("paths", {}))
        _expect_check_fails(
            tally, "a contract that declares no operations at all", d, "would produce nothing and pass vacuously", drift
        )

        d = _mutant(root, tmp, "contract-unparseable")
        (d / "api/v1/openapi.json").write_text("not json at all\n")
        _expect_check_fails(tally, "a contract the generator cannot read", d, "the generator refused", drift)

        print()
        print("==> the gate that gates actually runs these checks")

        d = _mutant(root, tmp, "ci-local-without-the-drift-gate")
        text = (d / "scripts/ci-local.sh").read_text()
        lines = [
            ln
            for ln in text.splitlines(keepends=True)
            if "bash scripts/api/check-contract-drift.sh" not in ln
            or not ln.lstrip().startswith("bash scripts/api/check-contract-drift.sh")
        ]
        (d / "scripts/ci-local.sh").write_text("".join(lines))
        _expect_check_fails(
            tally,
            "the drift gate dropped from the pre-commit gate",
            d,
            "does not run scripts/api/check-contract-drift.sh",
            drift,
        )

        d = _mutant(root, tmp, "ci-local-without-the-client-path-gate")
        text = (d / "scripts/ci-local.sh").read_text()
        lines = [
            ln
            for ln in text.splitlines(keepends=True)
            if not ln.lstrip().startswith("bash scripts/api/check-client-paths.sh")
        ]
        (d / "scripts/ci-local.sh").write_text("".join(lines))
        _expect_check_fails(
            tally,
            "the client-path gate dropped from the pre-commit gate",
            d,
            "does not run scripts/api/check-client-paths.sh",
            drift,
        )

        d = _mutant(root, tmp, "ci-local-without-the-selftest")
        text = (d / "scripts/ci-local.sh").read_text()
        lines = [
            ln for ln in text.splitlines(keepends=True) if not ln.lstrip().startswith("bash scripts/api/selftest.sh")
        ]
        (d / "scripts/ci-local.sh").write_text("".join(lines))
        _expect_check_fails(
            tally, "this self-test dropped from the pre-commit gate", d, "does not run scripts/api/selftest.sh", drift
        )

        print()
        print("==> every /api/v1 path the shared client builds is a declared operation (#211)")

        d = _mutant(root, tmp, "client-path-the-contract-does-not-declare")
        _sed_replace(d / "ui/shared/src/api/client.ts", '"/validators"', '"/validator-catalog"')
        _expect_check_fails(
            tally,
            "a client path the contract does not declare",
            d,
            "are not operations api/v1/openapi.json declares",
            client_paths,
        )

        d = _mutant(root, tmp, "client-verb-the-contract-does-not-declare")
        _sed_replace(d / "ui/shared/src/api/client.ts", 'method: "PATCH"', 'method: "PUT"')
        _expect_check_fails(
            tally, "the right path under a verb the contract does not declare", d, "on this path, not PUT", client_paths
        )

        d = _mutant(root, tmp, "contract-renamed-an-operation-the-client-calls")
        _json_edit(
            d / "api/v1/openapi.json",
            lambda doc: doc["paths"].__setitem__("/preferences", doc["paths"].pop("/settings")),
        )
        _expect_check_fails(
            tally,
            "a contract rename that left the client calling the old path",
            d,
            "are not operations api/v1/openapi.json declares",
            client_paths,
        )

        d = _mutant(root, tmp, "client-method-that-makes-no-request")
        _sed_replace(
            d / "ui/shared/src/api/client.ts", 'logout: () => post("/auth/logout")', "logout: () => Promise.resolve()"
        )
        _expect_check_fails(
            tally,
            "a client method whose request this gate cannot find",
            d,
            "makes no request() or post() call this gate could find",
            client_paths,
        )

        d = _mutant(root, tmp, "client-path-that-is-entirely-interpolated")
        _sed_replace(
            d / "ui/shared/src/api/client.ts",
            'request<WireSettingsResponse>("/settings")',
            'request<WireSettingsResponse>("/" + settingsPath)',
        )
        _expect_check_fails(
            tally,
            "a request path with no literal segment left to check",
            d,
            "which is entirely interpolated",
            client_paths,
        )

        d = _mutant(root, tmp, "client-path-not-rooted-at-slash")
        _sed_replace(
            d / "ui/shared/src/api/client.ts",
            'request<WireSettingsResponse>("/settings")',
            "request<WireSettingsResponse>(settingsPath)",
        )
        _expect_check_fails(
            tally, 'a request path that is not rooted at "/" at all', d, "which is not rooted at", client_paths
        )

        d = _mutant(root, tmp, "client-with-a-second-fetch")
        with (d / "ui/shared/src/api/client.ts").open("a") as f:
            f.write('\nexport const strayRequest = () => fetch("/api/v1/anything");\n')
        _expect_check_fails(
            tally,
            "a request that bypasses the one fetch this gate reasons about",
            d,
            "which is only equivalent to what reaches the network",
            client_paths,
        )

        d = _mutant(root, tmp, "client-base-path-moved")
        _sed_replace(d / "ui/shared/src/api/client.ts", 'const BASE = "/api/v1";', 'const BASE = "/api/v2";')
        _expect_check_fails(
            tally, "a client whose base path is no longer the one this gate checks", d, 'not "/api/v1"', client_paths
        )

        d = _mutant(root, tmp, "client-paths-against-an-empty-contract")
        _json_edit(d / "api/v1/openapi.json", lambda doc: doc.__setitem__("paths", {}))
        _expect_check_fails(
            tally,
            "a contract with no operations, which would blame the client for everything",
            d,
            "declares no operations at all",
            client_paths,
        )

        print()
        print("==> the generator refuses a shape it would otherwise drop")

        d = _mutant(root, tmp, "contract-oneof-on-a-named-schema")

        def _oneof(doc: dict[str, Any]) -> None:
            doc["components"]["schemas"]["SubmitOperationConflict"] = {
                "oneOf": [
                    {"$ref": "#/components/schemas/ConfigRevisionStaleResponse"},
                    {"$ref": "#/components/schemas/ErrorResponse"},
                ]
            }

        _json_edit(d / "api/v1/openapi.json", _oneof)
        _expect_check_fails(
            tally,
            "a named schema using oneOf, which both bindings would drop",
            d,
            "would be dropped from both bindings without a word",
            drift,
        )

        print()
        print("==> Go handlers still match the contract (go test)")

        d = _mutant(root, tmp, "handler-field-renamed")
        _sed_replace(
            d / "apps/common/webhost/handlers_system.go",
            'ConfigRevision string `json:"config_revision"`',
            'ConfigRevision string `json:"revision"`',
        )
        _expect_check_fails(
            tally,
            "a handler response field renamed without the contract",
            d,
            "is in the handler type but not in the contract",
            ["bash", "-c", "cd apps/common && go test -count=1 -run TestContract ./webhost/"],
        )

        d = _mutant(root, tmp, "handler-field-added")
        _sed_replace(
            d / "apps/common/webhost/handlers_system.go",
            '\tReady bool `json:"ready"`',
            '\tReady bool `json:"ready"`\n\tSQLitePath string `json:"sqlite_path"`',
        )
        _expect_check_fails(
            tally,
            "a handler response field added without the contract",
            d,
            "is in the handler type but not in the contract",
            ["bash", "-c", "cd apps/common && go test -count=1 -run TestContract ./webhost/"],
        )

        d = _mutant(root, tmp, "route-outside-the-contract")
        _sed_replace(
            d / "apps/common/webhost/router.go",
            '\t\tr.Get("/validators", h.listValidators)',
            '\t\tr.Get("/validators", h.listValidators)\n\t\tr.Get("/rclone/remotes", h.listValidators)',
        )
        _expect_check_fails(
            tally,
            "a route the contract does not declare",
            d,
            "which the contract does not declare",
            ["bash", "-c", "cd apps/common && go test -count=1 -run TestContract ./webhost/"],
        )

        d = _mutant(root, tmp, "unregistered-error-code")
        _sed_replace(
            d / "apps/common/webhost/handlers_operations.go",
            'writeError(w, http.StatusNotFound, "OPERATION_NOT_FOUND"',
            'writeError(w, http.StatusNotFound, "OPERATION_VANISHED"',
        )
        _expect_check_fails(
            tally,
            "an error code no registry knows",
            d,
            "which api/v1/openapi.json does not register as a wire code",
            ["bash", "-c", "cd apps/common && go test -count=1 -run TestContract ./webhost/"],
        )

        d = _mutant(root, tmp, "contract-path-no-client-can-build")

        def _identity_spans_segments(doc: dict[str, Any]) -> None:
            item = doc["paths"].pop("/backups/{source}/{set}/{name}")
            for op in item.values():
                op["parameters"] = [
                    {
                        "name": "id",
                        "in": "path",
                        "required": True,
                        "schema": {"type": "string"},
                        "description": "The three-part identity.",
                    }
                ] + [p for p in op.get("parameters", []) if p.get("in") != "path"]
            doc["paths"]["/backups/{id}"] = item

        _json_edit(d / "api/v1/openapi.json", _identity_spans_segments)
        subprocess.run(["bash", "scripts/api/generate.sh"], cwd=d, capture_output=True, text=True)
        _expect_check_fails(
            tally,
            "a path template no caller holding the resource's identity can fill",
            d,
            "which is not an instance of it",
            [
                "bash",
                "-c",
                "cd apps/common && go test -count=1 "
                "-run TestContract_ThePublishedTemplateIsTheShapeTheseRoutesAreDrivenAt ./webhost/",
            ],
        )
        _expect_check_fails(
            tally,
            "a path built from that template, driven at the real router",
            d,
            "want 200 and that id back",
            [
                "bash",
                "-c",
                "cd apps/common && go test -count=1 "
                "-run TestContract_APathBuiltFromTheContractReachesTheResourceItNames ./webhost/",
            ],
        )

        d = _mutant(root, tmp, "profile-changes-backup-semantics")
        path = d / "apps/common/webhost/handlers_backupsets.go"
        text = path.read_text()
        text = text.replace(
            "func (h *handlers) listBackupSets(w http.ResponseWriter, r *http.Request) {",
            'func (h *handlers) listBackupSets(w http.ResponseWriter, r *http.Request) {\n'
            '\tif h.platform.ID() == "ugos" {\n'
            "\t\twriteJSON(w, http.StatusOK, listBackupSetsResponse{})\n"
            "\t\treturn\n\t}",
            1,
        )
        path.write_text(text)
        _expect_check_fails(
            tally,
            "a runtime profile changing what a backup endpoint means",
            d,
            "differs between the generic and ugos profiles",
            ["bash", "-c", "cd apps/common && go test -count=1 -run TestProfileParity ./webhost/"],
        )

    if tally.failed:
        harness.die(
            f"{tally.failed} of {tally.passed + tally.failed} API-contract controls did not behave as required."
        )

    print()
    print(
        f"OK: all {tally.passed} API-contract controls behaved as required (every rule was shown to fire against "
        "a real planted violation, and shown not to fire on the real tree)."
    )
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    root = harness.repo_root(Path(__file__).resolve())
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main())

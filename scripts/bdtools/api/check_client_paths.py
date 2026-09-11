#!/usr/bin/env python3
"""The /api/v1 CLIENT PATH gate (issue #211).

`check_contract_drift.py` next to this file proves the two GENERATED
bindings still match `api/v1/openapi.json`. That is only half of what issue
#166 asked for. `ui/shared/src/api/client.ts` is hand-written on top of the
generated module and imports nothing but types and the error-code registry
from it: every request path it builds is a string literal, and until this
script nothing compared those literals to the contract at all.

What that cost, measured against a real engine on main (#211): fourteen
(method, path) pairs the client asked for that neither the contract nor
`apps/common/webhost/router.go` had, so four of the six shipped pages failed
outright with "The backup service returned an unexpected response." Every
suite in the repository was green while that was true, because the browser
tests run against `createMockApi`, which implements whatever the client asks
for and is therefore green by construction.

So this script reads `client.ts` STATICALLY -- no npm install, no bundler,
no running browser -- reduces every request path it builds back to a
(method, path) pattern, and requires each one to be an operation
`api/v1/openapi.json` declares.

Three properties matter more than the check itself:

  1. It FAILS CLOSED. A path expression this script cannot reduce is a
     failure, never a skip, and every method on the exported client object
     must produce at least one request. A silent skip here would be
     indistinguishable from a pass, which is exactly how the drift this
     gate exists to catch survived four PRs.

  2. It refuses to pass VACUOUSLY. A client with no calls in it, or a
     contract with no operations in it, is a failure rather than an empty
     comparison that trivially succeeds.

  3. It has NO allowlist. `ui/shared/src/api/contract.conformance.test.ts`
     already pinned these fourteen paths as recorded debt, exactly, and
     that is why the suite stayed green: an allowlist turns a gate into a
     ledger. There is deliberately no way to exempt a path here. If a
     client path is not in the contract, either the contract gains the
     operation or the client stops calling it.

`selftest.py` mutation-tests every rule below against the real tree.

# PORTED-CHECK HAZARD NOTE

a path expression that fans out past the candidate cap is a refusal, not silence
  hazard in bash:   the embedded python ran as a heredoc under `python3 -`;
                     a `ValueError` from `reduce_expr` propagated as an
                     uncaught exception, printed a traceback to stderr and
                     exited non-zero -- which the outer `set -euo pipefail`
                     script then reported honestly as a failure (a non-zero
                     `python3 -` under a pipeline with no explicit exit
                     check), but with a Python traceback instead of this
                     gate's own message, so a legitimate refusal read like a
                     crash.
  hazard in python: GONE as a process-boundary problem, same shape as
                     check_contract_drift's hazard note: this port runs the
                     same `reduce_expr` in-process and the `except
                     ValueError` in `calls_in`'s caller already converts it
                     to a `fail(...)` entry, exactly as the embedded script
                     did -- the only thing this port removes is the second,
                     accidental failure mode (a bare traceback) that a
                     `python3 - <<'PY'` heredoc could produce if the
                     `except` around it were ever dropped upstream of this
                     port.
  held by:           the `try/except ValueError` around `reduce_expr` in
                     `_requests_from_members`, and `selftest.py`'s
                     controls under "the generator refuses a shape it would
                     otherwise drop" exercise the same `reduce_expr` code
                     path via `check_contract_drift.py`, not this file, but
                     both scripts share the ValueError-refuses-not-crashes
                     contract.

a client path this gate cannot read is a failure, never a silent skip
  hazard in bash:    none distinguishable from python: the embedded script
                     already refused (via `fail(...)` + `sys.exit(1)`)
                     rather than skip, in the same process that scanned the
                     file, so there was no shell-level `set -e` boundary for
                     a subprocess status to be lost across.
  hazard in python:  not applicable; same in-process refusal, unchanged.
  held by:           the `client-method-that-makes-no-request`,
                     `client-path-that-is-entirely-interpolated` and
                     `client-path-not-rooted-at-slash` controls in
                     `selftest.py`, unchanged from the bash `selftest.sh`.
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path
from typing import Iterator

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness
from bdtools.api import lib

PROGRAM = "check-client-paths"

API_CLIENT = Path("ui/shared/src/api/client.ts")

OPEN = {"(": ")", "[": "]", "{": "}"}
CLOSE = {")": "(", "]": "[", "}": "{"}

PLACEHOLDER = "{}"
MAX_CANDIDATES = 64

_VERBS = ("get", "put", "post", "delete", "options", "head", "patch", "trace")


def strip_comments(src: str) -> str:
    out: list[str] = []
    i, n = 0, len(src)
    while i < n:
        c = src[i]
        if c == "/" and i + 1 < n and src[i + 1] == "/":
            while i < n and src[i] != "\n":
                i += 1
            continue
        if c == "/" and i + 1 < n and src[i + 1] == "*":
            i += 2
            while i + 1 < n and not (src[i] == "*" and src[i + 1] == "/"):
                i += 1
            i += 2
            continue
        if c in "'\"`":
            quote = c
            out.append(c)
            i += 1
            while i < n:
                if src[i] == "\\":
                    out.append(src[i : i + 2])
                    i += 2
                    continue
                out.append(src[i])
                if src[i] == quote:
                    i += 1
                    break
                i += 1
            continue
        out.append(c)
        i += 1
    return "".join(out)


def scan(text: str, start: int = 0, end: int | None = None) -> Iterator[tuple[int, str, int, bool]]:
    """Yields (index, char, depth, in_string) over text[start:end]."""
    if end is None:
        end = len(text)
    depth = 0
    i = start
    while i < end:
        c = text[i]
        if c in "'\"`":
            quote = c
            j = i + 1
            while j < end:
                if text[j] == "\\":
                    j += 2
                    continue
                if text[j] == quote:
                    break
                j += 1
            yield (i, c, depth, True)
            i = j + 1
            continue
        if c in OPEN:
            yield (i, c, depth, False)
            depth += 1
            i += 1
            continue
        if c in CLOSE:
            depth -= 1
            yield (i, c, depth, False)
            i += 1
            continue
        yield (i, c, depth, False)
        i += 1


def match_bracket(text: str, open_index: int) -> int:
    want = OPEN[text[open_index]]
    for i, c, depth, in_string in scan(text, open_index):
        if in_string:
            continue
        if i == open_index:
            continue
        if c == want and depth == 0:
            return i
    return -1


def split_top_level(expr: str, sep: str) -> list[str]:
    parts, last = [], 0
    for i, c, depth, in_string in scan(expr):
        if in_string or depth != 0:
            continue
        if c == sep:
            parts.append(expr[last:i])
            last = i + 1
    parts.append(expr[last:])
    return parts


def string_literal(expr: str) -> str | None:
    """The value of expr if it is exactly one plain string literal, else None."""
    e = expr.strip()
    if len(e) < 2 or e[0] not in "'\"" or e[-1] != e[0]:
        return None
    body = e[1:-1]
    if e[0] in body.replace("\\" + e[0], ""):
        return None
    return body.replace("\\" + e[0], e[0])


def find_ternary(expr: str) -> tuple[int, int] | None:
    """(question, colon) indices of a top-level ternary, or None."""
    q = None
    for i, c, depth, in_string in scan(expr):
        if in_string or depth != 0:
            continue
        if c == "?" and q is None:
            if expr[i : i + 2] in ("?.", "??") or (i > 0 and expr[i - 1] == "?"):
                continue
            q = i
            continue
        if c == ":" and q is not None:
            return (q, i)
    return None


def body() -> int:
    failures: list[str] = []

    def fail(msg: str) -> None:
        failures.append(msg)

    if not lib.API_CONTRACT.is_file():
        harness.die(f"{lib.API_CONTRACT} does not exist.")
    if not API_CLIENT.is_file():
        harness.die(f"{API_CLIENT} does not exist.")

    print(f"==> reducing every request path in {API_CLIENT}")

    raw = API_CLIENT.read_text(encoding="utf-8")
    src = strip_comments(raw)

    base_match = re.search(r'const\s+BASE\s*=\s*"([^"]*)"', src)
    if not base_match:
        fail(
            f"{API_CLIENT} no longer declares `const BASE = ...`, so this gate cannot tell what its paths "
            "are relative to."
        )
    else:
        base = base_match.group(1)
        if base != "/api/v1":
            fail(
                f'{API_CLIENT}\'s BASE is "{base}", not "/api/v1". Either the API moved (update this gate and '
                "the contract's `servers` entry together) or that is the bug."
            )

    fetch_calls = list(re.finditer(r"\bfetch\s*\(", src))
    if len(fetch_calls) != 1:
        fail(
            f"{API_CLIENT} makes {len(fetch_calls)} fetch() calls. This gate reduces the paths passed to its "
            "request()/post() helpers, which is only equivalent to what reaches the network while exactly one "
            "fetch() exists and it is the one inside request(). Route the new call through request(), or teach "
            "this gate about it."
        )
    else:
        tail = src[fetch_calls[0].end() : fetch_calls[0].end() + 40]
        if not re.match(r"\s*BASE\s*\+\s*path\b", tail):
            fail(
                f"{API_CLIENT}'s single fetch() no longer reads `fetch(BASE + path`, so a path this gate "
                "reduced is no longer the URL that is requested."
            )

    post_helper = re.search(
        r"const\s+post\s*=\s*\([^)]*\)\s*=>\s*request<[^>]*>\(\s*path\s*,\s*\{\s*method:\s*\"POST\"", src
    )
    if not post_helper:
        fail(
            f'{API_CLIENT}\'s `post` helper no longer reads `request<...>(path, {{ method: "POST" ...`, so '
            "this gate can no longer assume a post() call is a POST."
        )

    # -------------------------------------------------------------------
    # Named path helpers, e.g. retentionPath(source, set).
    # -------------------------------------------------------------------
    candidates: dict[str, str] = {}
    for m in re.finditer(r"\bconst\s+([A-Za-z_$][\w$]*)\s*=\s*\(", src):
        open_paren = m.end() - 1
        close_paren = match_bracket(src, open_paren)
        if close_paren < 0:
            continue
        after = src[close_paren + 1 :]
        arrow = re.match(r"\s*(?::[^=]*)?=>", after)
        if not arrow:
            continue
        body_start = close_paren + 1 + arrow.end()
        body_end = None
        for i, c, depth, in_string in scan(src, body_start):
            if in_string or depth != 0:
                continue
            if c == ";":
                body_end = i
                break
        if body_end is None:
            continue
        candidates[m.group(1)] = src[body_start:body_end].strip()

    helpers: dict[str, str] = {}

    def reduce_expr(expr: str, seen: tuple[str, ...] = ()) -> set[str]:
        expr = expr.strip()
        if not expr:
            return {""}

        if expr.startswith("(") and match_bracket(expr, 0) == len(expr) - 1:
            return reduce_expr(expr[1:-1], seen)

        ternary = find_ternary(expr)
        if ternary:
            q, colon = ternary
            return reduce_expr(expr[q + 1 : colon], seen) | reduce_expr(expr[colon + 1 :], seen)

        terms = split_top_level(expr, "+")
        if len(terms) > 1:
            out = {""}
            for term in terms:
                piece = reduce_expr(term, seen)
                out = {a + b for a in out for b in piece}
                if len(out) > MAX_CANDIDATES:
                    raise ValueError(f"path expression fans out past {MAX_CANDIDATES} candidates")
            return out

        lit = string_literal(expr)
        if lit is not None:
            return {lit}

        if expr.startswith("`") and expr.endswith("`"):
            return {re.sub(r"\$\{[^}]*\}", PLACEHOLDER, expr[1:-1])}

        call = re.match(r"([A-Za-z_$][\w$]*)\s*\(", expr)
        if call and call.group(1) in helpers:
            name = call.group(1)
            if name in seen:
                raise ValueError(f"path helper {name}() is recursive")
            return reduce_expr(helpers[name], (*seen, name))

        return {PLACEHOLDER}

    for _ in range(len(candidates) + 1):
        progressed = False
        for name, cbody in candidates.items():
            if name in helpers:
                continue
            try:
                reduced = reduce_expr(cbody)
            except ValueError:
                continue
            if reduced and all(value.startswith("/") for value in reduced):
                helpers[name] = cbody
                progressed = True
        if not progressed:
            break

    # -------------------------------------------------------------------
    # Every request()/post() call inside the exported client object.
    # -------------------------------------------------------------------
    client_decl = re.search(r"export\s+const\s+httpApi\s*:\s*BackupdApi\s*=\s*\{", src)
    if not client_decl:
        fail(
            f"{API_CLIENT} no longer exports `const httpApi: BackupdApi = {{`, so this gate cannot "
            "find the requests to check."
        )
        for f in failures:
            print("FAIL: " + f, file=sys.stderr)
        return harness.EXIT_FAILED

    obj_open = client_decl.end() - 1
    obj_close = match_bracket(src, obj_open)
    if obj_close < 0:
        fail(f"{API_CLIENT}'s httpApi object literal is unbalanced; this gate refuses to guess where it ends.")
        for f in failures:
            print("FAIL: " + f, file=sys.stderr)
        return harness.EXIT_FAILED

    members: list[tuple[str, str]] = []
    for chunk in split_top_level(src[obj_open + 1 : obj_close], ","):
        if not chunk.strip():
            continue
        head = split_top_level(chunk, ":")
        if len(head) < 2:
            fail(f"this gate could not read `{chunk.strip()[:60]}` as a `name: expression` member of httpApi.")
            continue
        members.append((head[0].strip(), ":".join(head[1:])))

    if not members:
        fail("httpApi declares no methods at all, so this gate compared nothing and would pass vacuously.")

    def calls_in(mbody: str) -> list[tuple[str, str]]:
        found = []
        for m in re.finditer(r"\b(request|post)\b", mbody):
            i = m.end()
            j = i
            while j < len(mbody) and mbody[j].isspace():
                j += 1
            if j < len(mbody) and mbody[j] == "<":
                depth = 0
                while j < len(mbody):
                    if mbody[j] == "<":
                        depth += 1
                    elif mbody[j] == ">":
                        depth -= 1
                        if depth == 0:
                            j += 1
                            break
                    j += 1
                while j < len(mbody) and mbody[j].isspace():
                    j += 1
            if j >= len(mbody) or mbody[j] != "(":
                continue
            close = match_bracket(mbody, j)
            if close < 0:
                raise ValueError(f"unbalanced argument list after {m.group(1)}(")
            args = split_top_level(mbody[j + 1 : close], ",")
            verb = "POST" if m.group(1) == "post" else "GET"
            if m.group(1) == "request" and len(args) > 1:
                method = re.search(r'method\s*:\s*"([A-Za-z]+)"', args[1])
                if method:
                    verb = method.group(1).upper()
            found.append((verb, args[0]))
        return found

    requests: list[tuple[str, str, str]] = []
    for name, mbody in members:
        try:
            found = calls_in(mbody)
        except ValueError as exc:
            fail(f"{name}: {exc}")
            continue
        if not found:
            fail(
                f"{name} makes no request() or post() call this gate could find. A client method whose path "
                "this gate cannot read is not checked at all, which is the one outcome it must never produce "
                "silently."
            )
            continue
        for verb, path_expr in found:
            try:
                path_candidates = sorted(reduce_expr(path_expr))
            except ValueError as exc:
                fail(f"{name}: {exc}")
                continue
            for candidate in path_candidates:
                path = candidate.split("?")[0]
                if not path.startswith("/"):
                    fail(
                        f'{name} builds the request path {candidate!r}, which is not rooted at "/". Every '
                        'path in this file is relative to BASE, so a path that does not start with "/" '
                        "cannot be checked against the contract."
                    )
                    continue
                if path.strip("/") == "" or set(path.split("/")) <= {"", PLACEHOLDER}:
                    fail(
                        f"{name} builds the request path {candidate!r}, which is entirely interpolated. This "
                        "gate cannot say anything about a path with no literal segments, so it refuses rather "
                        "than passing it."
                    )
                    continue
                if (name, verb, path) not in requests:
                    requests.append((name, verb, path))

    if not requests:
        fail("this gate extracted no request paths at all, so it verified nothing.")

    # -------------------------------------------------------------------
    # What the contract declares.
    # -------------------------------------------------------------------
    contract = json.loads(lib.API_CONTRACT.read_text())

    declared: dict[tuple[str, str], str] = {}
    for path, item in (contract.get("paths") or {}).items():
        normalised = re.sub(r"\{[^}]*\}", PLACEHOLDER, path)
        for verb, operation in item.items():
            if verb.lower() not in _VERBS:
                continue
            declared[(verb.upper(), normalised)] = operation.get("operationId", "?")

    if not declared:
        fail(
            f"{lib.API_CONTRACT} declares no operations at all, so every client path would be reported for "
            "the wrong reason."
        )

    # -------------------------------------------------------------------
    # The comparison.
    # -------------------------------------------------------------------
    if failures:
        for f in failures:
            print("FAIL: " + f, file=sys.stderr)
        return harness.EXIT_FAILED

    violations = [(name, verb, path) for name, verb, path in requests if (verb, path) not in declared]

    print(
        f"  read {len(requests)} request path(s) from {len(members)} client method(s) against "
        f"{len(declared)} declared operation(s)"
    )

    if violations:
        print(file=sys.stderr)
        print(
            f"FAIL: {len(violations)} request path(s) in {API_CLIENT} are not operations {lib.API_CONTRACT} "
            "declares. A real backend answers each of these with a 404 or a 405, and the shared Web UI turns "
            'that into "The backup service returned an unexpected response." Either add the operation to the '
            "contract (then scripts/api/generate.sh, then a handler in apps/common/webhost/router.go), or stop "
            "calling it:",
            file=sys.stderr,
        )
        print(file=sys.stderr)
        width = max(len(v) + 1 + len(p) for _, v, p in violations)
        for name, verb, path in sorted(violations, key=lambda v: (v[1], v[2])):
            near = sorted(m for (m, p) in declared if p == path and m != verb)
            hint = f"  (the contract declares {', '.join(near)} on this path, not {verb})" if near else ""
            print(f"    {verb + ' ' + path:<{width}}  <- httpApi.{name}{hint}", file=sys.stderr)
        print(file=sys.stderr)
        return harness.EXIT_FAILED

    print(f"OK: every /api/v1 path {API_CLIENT} builds is an operation {lib.API_CONTRACT} declares.")
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

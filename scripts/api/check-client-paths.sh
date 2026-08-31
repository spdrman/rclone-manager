#!/usr/bin/env bash
# The /api/v1 CLIENT PATH gate (issue #211).
#
# check-contract-drift.sh next to this file proves the two GENERATED
# bindings still match api/v1/openapi.json. That is only half of what
# issue #166 asked for. ui/shared/src/api/client.ts is hand-written on top
# of the generated module and imports nothing but types and the error-code
# registry from it: every request path it builds is a string literal, and
# until this script nothing compared those literals to the contract at all.
#
# What that cost, measured against a real engine on main (#211): fourteen
# (method, path) pairs the client asked for that neither the contract nor
# apps/common/webhost/router.go had, so four of the six shipped pages
# failed outright with "The backup service returned an unexpected
# response." Every suite in the repository was green while that was true,
# because the browser tests run against createMockApi, which implements
# whatever the client asks for and is therefore green by construction.
#
# So this script reads client.ts STATICALLY - no npm install, no bundler,
# no running browser - reduces every request path it builds back to a
# (method, path) pattern, and requires each one to be an operation
# api/v1/openapi.json declares.
#
# Three properties matter more than the check itself:
#
#   1. It FAILS CLOSED. A path expression this script cannot reduce is a
#      failure, never a skip, and every method on the exported client
#      object must produce at least one request. A silent skip here would
#      be indistinguishable from a pass, which is exactly how the drift
#      this gate exists to catch survived four PRs.
#
#   2. It refuses to pass VACUOUSLY. A client with no calls in it, or a
#      contract with no operations in it, is a failure rather than an
#      empty comparison that trivially succeeds.
#
#   3. It has NO allowlist. ui/shared/src/api/contract.conformance.test.ts
#      already pinned these fourteen paths as recorded debt, exactly, and
#      that is why the suite stayed green: an allowlist turns a gate into
#      a ledger. There is deliberately no way to exempt a path here. If a
#      client path is not in the contract, either the contract gains the
#      operation or the client stops calling it.
#
# scripts/api/selftest.sh mutation-tests every rule below against the real
# tree.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/api/lib.sh

command -v python3 >/dev/null 2>&1 || {
  echo "check-client-paths: python3 is required (it reduces the client's path expressions); scripts/api/check-contract-drift.sh already depends on it" >&2
  exit 1
}

API_CLIENT="ui/shared/src/api/client.ts"

if [ ! -f "$API_CONTRACT" ]; then
  echo "FAIL: $API_CONTRACT does not exist, so /api/v1 has no authoritative definition to check the client against." >&2
  exit 1
fi
if [ ! -f "$API_CLIENT" ]; then
  echo "FAIL: $API_CLIENT does not exist. This gate checks the shared Web UI's request paths against the contract; if that file moved, move this check with it rather than letting it pass on an absent client." >&2
  exit 1
fi

echo "==> reducing every request path in $API_CLIENT"
python3 - "$API_CLIENT" "$API_CONTRACT" <<'PY'
import json
import re
import sys

client_path, contract_path = sys.argv[1], sys.argv[2]

failures = []


def fail(msg):
    failures.append(msg)


# ---------------------------------------------------------------------------
# 1. Read the client with comments removed.
# ---------------------------------------------------------------------------
#
# Comments are stripped rather than tolerated because this file's comments
# quote paths ("GET /api/v1/validators", "/backup-sets/{source}/{set}/
# retention/...") and a scanner that read them would both invent calls that
# are not made and, worse, be satisfiable by writing a comment.

def strip_comments(src):
    out = []
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
                    out.append(src[i:i + 2])
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


raw = open(client_path, encoding="utf-8").read()
src = strip_comments(raw)


# ---------------------------------------------------------------------------
# Small scanning helpers, all string- and depth-aware.
# ---------------------------------------------------------------------------

OPEN = {"(": ")", "[": "]", "{": "}"}
CLOSE = {")": "(", "]": "[", "}": "{"}


def scan(text, start=0, end=None):
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


def match_bracket(text, open_index):
    """Index of the bracket closing the one at open_index."""
    want = OPEN[text[open_index]]
    for i, c, depth, in_string in scan(text, open_index):
        if in_string:
            continue
        if i == open_index:
            continue
        if c == want and depth == 0:
            return i
    return -1


def split_top_level(expr, sep):
    parts, last = [], 0
    for i, c, depth, in_string in scan(expr):
        if in_string or depth != 0:
            continue
        if c == sep:
            parts.append(expr[last:i])
            last = i + 1
    parts.append(expr[last:])
    return parts


def string_literal(expr):
    """The value of expr if it is exactly one plain string literal, else None."""
    e = expr.strip()
    if len(e) < 2 or e[0] not in "'\"" or e[-1] != e[0]:
        return None
    body = e[1:-1]
    if e[0] in body.replace("\\" + e[0], ""):
        return None
    return body.replace("\\" + e[0], e[0])


# ---------------------------------------------------------------------------
# 2. The base path, and the single fetch that every request goes through.
# ---------------------------------------------------------------------------

base_match = re.search(r'const\s+BASE\s*=\s*"([^"]*)"', src)
if not base_match:
    fail("client.ts no longer declares `const BASE = ...`, so this gate cannot tell what its paths are relative to.")
    base = None
else:
    base = base_match.group(1)
    if base != "/api/v1":
        fail('client.ts\'s BASE is "%s", not "/api/v1". Either the API moved (update this gate and the contract\'s `servers` entry together) or that is the bug.' % base)

fetch_calls = [m for m in re.finditer(r"\bfetch\s*\(", src)]
if len(fetch_calls) != 1:
    fail(
        "client.ts makes %d fetch() calls. This gate reduces the paths passed to its request()/post() helpers, "
        "which is only equivalent to what reaches the network while exactly one fetch() exists and it is the one "
        "inside request(). Route the new call through request(), or teach this gate about it." % len(fetch_calls)
    )
else:
    tail = src[fetch_calls[0].end():fetch_calls[0].end() + 40]
    if not re.match(r"\s*BASE\s*\+\s*path\b", tail):
        fail("client.ts's single fetch() no longer reads `fetch(BASE + path`, so a path this gate reduced is no longer the URL that is requested.")

post_helper = re.search(r"const\s+post\s*=\s*\([^)]*\)\s*=>\s*request<[^>]*>\(\s*path\s*,\s*\{\s*method:\s*\"POST\"", src)
if not post_helper:
    fail("client.ts's `post` helper no longer reads `request<...>(path, { method: \"POST\" ...`, so this gate can no longer assume a post() call is a POST.")


# ---------------------------------------------------------------------------
# 3. Named path helpers, e.g. retentionPath(source, set).
# ---------------------------------------------------------------------------
#
# Every `const NAME = (...) => ...;` in the file is a CANDIDATE here; the
# ones that qualify are decided below, after reduce_expr exists, by whether
# their bodies reduce to paths rooted at "/". That is what separates
# retentionPath (a path builder) from post (a verb wrapper), without this
# gate having to hardcode either name.

candidates = {}
for m in re.finditer(r"\bconst\s+([A-Za-z_$][\w$]*)\s*=\s*\(", src):
    open_paren = m.end() - 1
    close_paren = match_bracket(src, open_paren)
    if close_paren < 0:
        continue
    after = src[close_paren + 1:]
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

helpers = {}


# ---------------------------------------------------------------------------
# 4. Reduce a path expression to the set of concrete patterns it can build.
# ---------------------------------------------------------------------------
#
# Every interpolated value becomes "{}", so "/backup-sets/" + id reduces to
# "/backup-sets/{}" and the contract's "/backup-sets/{id}" normalises to
# the same thing. A ternary yields BOTH branches rather than one guess, so
# listArtifacts' optional query string is reduced to the two URLs it can
# actually build and both are checked.

PLACEHOLDER = "{}"
MAX_CANDIDATES = 64


def find_ternary(expr):
    """(question, colon) indices of a top-level ternary, or None."""
    q = None
    for i, c, depth, in_string in scan(expr):
        if in_string or depth != 0:
            continue
        if c == "?" and q is None:
            if expr[i:i + 2] in ("?.", "??") or (i > 0 and expr[i - 1] == "?"):
                continue
            q = i
            continue
        if c == ":" and q is not None:
            return (q, i)
    return None


def reduce_expr(expr, seen=()):
    expr = expr.strip()
    if not expr:
        return {""}

    if expr.startswith("(") and match_bracket(expr, 0) == len(expr) - 1:
        return reduce_expr(expr[1:-1], seen)

    # The conditional operator binds looser than "+", so it has to be split
    # off FIRST. Splitting on "+" first cuts `a ? "x" + f(b) : ""` in half
    # down the middle of its own consequent, and both halves then reduce to
    # a bare placeholder - which is how listArtifacts' optional query string
    # first reduced to "/backups{}{}" here instead of the two URLs it really
    # builds.
    ternary = find_ternary(expr)
    if ternary:
        q, colon = ternary
        return reduce_expr(expr[q + 1:colon], seen) | reduce_expr(expr[colon + 1:], seen)

    terms = split_top_level(expr, "+")
    if len(terms) > 1:
        out = {""}
        for term in terms:
            piece = reduce_expr(term, seen)
            out = {a + b for a in out for b in piece}
            if len(out) > MAX_CANDIDATES:
                raise ValueError("path expression fans out past %d candidates" % MAX_CANDIDATES)
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
            raise ValueError("path helper %s() is recursive" % name)
        return reduce_expr(helpers[name], seen + (name,))

    # An identifier, a call this gate does not know, a member expression:
    # one interpolated value, whatever it evaluates to.
    return {PLACEHOLDER}


# A helper qualifies as a PATH helper only if its body reduces to paths
# that are all rooted at "/". Registered by fixpoint rather than in one
# pass, because one helper may be written in terms of another
# (retentionPath is backupSetPath plus a tail), and the composed one only
# becomes reducible once the composed-of one is known.
#
# A helper that never qualifies is simply not registered, and a call to it
# then reduces to a bare placeholder, which section 5 below refuses as an
# entirely-interpolated path. That is the fail-closed direction: an
# expression this gate cannot follow is a failure, never a skip.
for _ in range(len(candidates) + 1):
    progressed = False
    for name, body in candidates.items():
        if name in helpers:
            continue
        try:
            reduced = reduce_expr(body)
        except ValueError:
            continue
        if reduced and all(value.startswith("/") for value in reduced):
            helpers[name] = body
            progressed = True
    if not progressed:
        break

# ---------------------------------------------------------------------------
# 5. Every request()/post() call inside the exported client object.
# ---------------------------------------------------------------------------

client_decl = re.search(r"export\s+const\s+httpApi\s*:\s*BackupManagerApi\s*=\s*\{", src)
if not client_decl:
    fail("client.ts no longer exports `const httpApi: BackupManagerApi = {`, so this gate cannot find the requests to check.")
    print("\n".join("FAIL: " + f for f in failures), file=sys.stderr)
    sys.exit(1)

obj_open = client_decl.end() - 1
obj_close = match_bracket(src, obj_open)
if obj_close < 0:
    fail("client.ts's httpApi object literal is unbalanced; this gate refuses to guess where it ends.")
    print("\n".join("FAIL: " + f for f in failures), file=sys.stderr)
    sys.exit(1)

members = []
for chunk in split_top_level(src[obj_open + 1:obj_close], ","):
    if not chunk.strip():
        continue
    head = split_top_level(chunk, ":")
    if len(head) < 2:
        fail("this gate could not read `%s` as a `name: expression` member of httpApi." % chunk.strip()[:60])
        continue
    members.append((head[0].strip(), ":".join(head[1:])))

if not members:
    fail("httpApi declares no methods at all, so this gate compared nothing and would pass vacuously.")


def calls_in(body):
    """(verb, path-expression) for every request()/post() call in body."""
    found = []
    for m in re.finditer(r"\b(request|post)\b", body):
        i = m.end()
        # Optional type arguments: request<WireBackupSet>( ... )
        j = i
        while j < len(body) and body[j].isspace():
            j += 1
        if j < len(body) and body[j] == "<":
            depth = 0
            while j < len(body):
                if body[j] == "<":
                    depth += 1
                elif body[j] == ">":
                    depth -= 1
                    if depth == 0:
                        j += 1
                        break
                j += 1
            while j < len(body) and body[j].isspace():
                j += 1
        if j >= len(body) or body[j] != "(":
            continue
        close = match_bracket(body, j)
        if close < 0:
            raise ValueError("unbalanced argument list after %s(" % m.group(1))
        args = split_top_level(body[j + 1:close], ",")
        verb = "POST" if m.group(1) == "post" else "GET"
        if m.group(1) == "request" and len(args) > 1:
            method = re.search(r'method\s*:\s*"([A-Za-z]+)"', args[1])
            if method:
                verb = method.group(1).upper()
        found.append((verb, args[0]))
    return found


requests = []
for name, body in members:
    try:
        found = calls_in(body)
    except ValueError as exc:
        fail("%s: %s" % (name, exc))
        continue
    if not found:
        fail(
            "%s makes no request() or post() call this gate could find. A client method whose path this gate cannot "
            "read is not checked at all, which is the one outcome it must never produce silently." % name
        )
        continue
    for verb, path_expr in found:
        try:
            candidates = sorted(reduce_expr(path_expr))
        except ValueError as exc:
            fail("%s: %s" % (name, exc))
            continue
        for candidate in candidates:
            path = candidate.split("?")[0]
            if not path.startswith("/"):
                fail(
                    "%s builds the request path %r, which is not rooted at \"/\". Every path in this file is relative "
                    "to BASE, so a path that does not start with \"/\" cannot be checked against the contract." % (name, candidate)
                )
                continue
            if path.strip("/") == "" or set(path.split("/")) <= {"", PLACEHOLDER}:
                fail(
                    "%s builds the request path %r, which is entirely interpolated. This gate cannot say anything "
                    "about a path with no literal segments, so it refuses rather than passing it." % (name, candidate)
                )
                continue
            # Deduplicated: both branches of listArtifacts' optional query
            # string reduce to the same path once the query is dropped, and
            # reporting one violation twice reads as two.
            if (name, verb, path) not in requests:
                requests.append((name, verb, path))

if not requests:
    fail("this gate extracted no request paths at all, so it verified nothing.")


# ---------------------------------------------------------------------------
# 6. What the contract declares.
# ---------------------------------------------------------------------------

with open(contract_path, encoding="utf-8") as f:
    contract = json.load(f)

VERBS = ("get", "put", "post", "delete", "options", "head", "patch", "trace")

declared = {}
for path, item in (contract.get("paths") or {}).items():
    normalised = re.sub(r"\{[^}]*\}", PLACEHOLDER, path)
    for verb, operation in item.items():
        if verb.lower() not in VERBS:
            continue
        declared[(verb.upper(), normalised)] = operation.get("operationId", "?")

if not declared:
    fail("%s declares no operations at all, so every client path would be reported for the wrong reason." % contract_path)


# ---------------------------------------------------------------------------
# 7. The comparison.
# ---------------------------------------------------------------------------

if failures:
    for f in failures:
        print("FAIL: " + f, file=sys.stderr)
    sys.exit(1)

violations = [(name, verb, path) for name, verb, path in requests if (verb, path) not in declared]

print("  read %d request path(s) from %d client method(s) against %d declared operation(s)"
      % (len(requests), len(members), len(declared)))

if violations:
    print(file=sys.stderr)
    print(
        "FAIL: %d request path(s) in %s are not operations %s declares. A real backend answers each of these with a "
        "404 or a 405, and the shared Web UI turns that into \"The backup service returned an unexpected response.\" "
        "Either add the operation to the contract (then scripts/api/generate.sh, then a handler in "
        "apps/common/webhost/router.go), or stop calling it:" % (len(violations), client_path, contract_path),
        file=sys.stderr,
    )
    print(file=sys.stderr)
    width = max(len(v) + 1 + len(p) for _, v, p in violations)
    for name, verb, path in sorted(violations, key=lambda v: (v[1], v[2])):
        near = sorted(m for (m, p) in declared if p == path and m != verb)
        hint = "  (the contract declares %s on this path, not %s)" % (", ".join(near), verb) if near else ""
        print("    %-*s  <- httpApi.%s%s" % (width, verb + " " + path, name, hint), file=sys.stderr)
    print(file=sys.stderr)
    sys.exit(1)

print("OK: every /api/v1 path %s builds is an operation %s declares." % (client_path, contract_path))
PY

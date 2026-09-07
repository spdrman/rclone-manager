#!/usr/bin/env bash
# The /api/v1 contract drift gate (issue #166).
#
# api/v1/openapi.json is the authoritative definition of the boundary. Two
# things can drift away from it, and this script catches both:
#
#   1. A GENERATED FILE that was hand-edited. Both bindings carry a DO NOT
#      EDIT banner, but a banner is a request, not a check. This
#      regenerates into a temporary directory and compares byte for byte,
#      the same generate-then-diff shape docs/conformance/phase-4-matrix.md
#      already uses.
#
#   2. An IMPLEMENTATION TYPE reaching the public schema. #81's standing
#      constraint forbids rclone, SQLite, filesystem and provider SDK types
#      on the public API, both because it leaks implementation detail and
#      because it widens what an external integrator or a store reviewer
#      can reach. If a public shape leaks one, the contract is wrong, not
#      this check.
#
#   3. THE GATE ITSELF NOT RUNNING. Both rules above were wired only into
#      .github/workflows/ci.yml, which ran on nothing at all then and runs
#      only on a pull request into `release` now (#575), so for
#      four PRs they ran on no commit at all while .husky/pre-commit's
#      scripts/ci-local.sh had never heard of scripts/api (PR #194 review,
#      M1). A check nothing invokes is indistinguishable from a check that
#      does not exist, so the invocation is now checked here too.
#
#   4. THE CONTRACT DISAGREEING WITH ITSELF. Every operation declares
#      whether it needs a session and the double-submit token TWICE: once
#      in the standard OpenAPI `security` block an external consumer or an
#      off-the-shelf generator reads, and once in the x-authenticated and
#      x-csrf-required extensions this repository's own generator obeys.
#      Nothing compared them, and they had already parted on fifteen of
#      forty-six operations: all fifteen mutating, all fifteen behind
#      requireCSRF at runtime, and the published document silent about it,
#      so the requirement could not be implemented from the document that
#      defines it (PR #546 review). A constant declared twice with nothing
#      comparing the two is a constant that drifts, which is this
#      repository's own rule about the wire, turned on the document.
#
# What it deliberately does NOT check is whether the Go HANDLERS still
# match the bindings; that needs reflection over unexported types, so it
# lives in apps/common/webhost/contract_test.go and
# apps/common/auth/local/contract_test.go and runs under `go test`. The two
# halves are independent: a handler can drift with the bindings intact, and
# a binding can be hand-edited with the handlers intact.
#
# scripts/api/selftest.sh mutation-tests every rule below against the real
# tree.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/api/lib.sh

command -v python3 >/dev/null 2>&1 || {
  echo "check-contract-drift: python3 is required (it reads the contract's identifiers); scripts/deploy already depends on it" >&2
  exit 1
}

fail=0
# note prints and records in one call, which is what lets this script keep
# checking after the first problem. A gate that exits on the first failure
# tells you about one drifted binding per run, and the two bindings drift
# together far more often than separately.
note() { echo "$@" >&2; fail=1; }

if [ ! -f "$API_CONTRACT" ]; then
  echo "FAIL: $API_CONTRACT does not exist, so /api/v1 has no authoritative definition at all." >&2
  exit 1
fi

# ---- 1. the checked-in bindings are what the contract generates ----------

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-api-drift.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

echo "==> regenerating the bindings from $API_CONTRACT"
if ! api::generate "$tmp/contract.gen.go" "$tmp/contract.ts" >"$tmp/gen.log" 2>&1; then
  note "FAIL: the generator refused $API_CONTRACT:"
  sed 's/^/    /' "$tmp/gen.log" >&2
  # Nothing below can mean anything without generated output to compare
  # against, so this one is fatal rather than accumulated.
  exit 1
fi
sed 's/^/    /' "$tmp/gen.log"

# compare says which of the two mistakes was made, because they have
# opposite fixes and the wrong guess undoes the change: a hand edit to the
# generated file is reverted by regenerating, and a contract change is
# completed by it. Both remedies are printed rather than the reader being
# asked to work out which case they are in from a diff.
compare() {
  local checked_in=$1 regenerated=$2 language=$3
  if [ ! -f "$checked_in" ]; then
    note "FAIL: $checked_in is missing. Run scripts/api/generate.sh."
    return
  fi
  if ! diff -u "$checked_in" "$regenerated" >"$tmp/diff" 2>&1; then
    note "FAIL: the checked-in $language binding does not match what $API_CONTRACT generates."
    note ""
    note "  If you edited $checked_in by hand, do not: it is generated output."
    note "  If you edited the contract, run scripts/api/generate.sh and commit the result."
    note ""
    sed 's/^/    /' "$tmp/diff" >&2
    return
  fi
  echo "  ok: $checked_in matches the contract"
}

compare "$API_GO_BINDING" "$tmp/contract.gen.go" "Go"
compare "$API_TS_BINDING" "$tmp/contract.ts" "TypeScript"

# ---- 2. no implementation type reaches the public schema ------------------

# The identifiers a public schema is made of: schema names, property names,
# enum values, path segments and operation ids. Descriptions are excluded on
# purpose - a schema is allowed to SAY "this is not an rclone remote", and a
# check that could not tell the two apart would be watered down until it
# fired on nothing, which is how the ADMIN_PASSWORD scanner in this
# repository failed before.
echo "==> scanning the public schema for implementation types"
identifiers=$(python3 - "$API_CONTRACT" <<'PY'
import json, sys

with open(sys.argv[1]) as f:
    doc = json.load(f)

out = []

def walk(node, path):
    if isinstance(node, dict):
        for key, value in node.items():
            if key in ("description", "summary", "why", "title", "$comment"):
                continue
            out.append(f"{path}/{key}\t{key}")
            walk(value, f"{path}/{key}")
    elif isinstance(node, list):
        for i, value in enumerate(node):
            walk(value, f"{path}[{i}]")
    elif isinstance(node, str):
        out.append(f"{path}\t{node}")

walk(doc.get("components", {}).get("schemas", {}), "components/schemas")
walk(doc.get("paths", {}), "paths")
print("\n".join(out))
PY
)

if [ -z "$identifiers" ]; then
  note "FAIL: the schema scan found no identifiers at all, so it verified nothing."
fi

# Case-insensitive, and deliberately without a word boundary: \b never
# matches between an underscore and a letter, which is how a previous
# scanner in this repository missed ADMIN_PASSWORD. A substring match is
# what catches rclone_remote, sqlite_path and RcloneConfig alike.
forbidden_pattern='rclone|sqlite|sqlite3|\.db\b|dsn|/var/lib|/etc/|gorm|database/sql|synology_api|truenas_api|ugos_sdk|dsm_api|unraid_plugin|omv_rpc'
if hits=$(printf '%s\n' "$identifiers" | grep -inE "$forbidden_pattern" || true); [ -n "$hits" ]; then
  note "FAIL: an implementation type reached the public schema. #81's standing constraint forbids rclone, SQLite, filesystem and provider SDK types on /api/v1, and the fix is the contract, not this check:"
  printf '%s\n' "$hits" | sed 's/^/    /' >&2
fi

count=$(printf '%s\n' "$identifiers" | grep -c . || true)
echo "  ok: $count public-schema identifiers carry no rclone, SQLite, filesystem or provider SDK type"

# ---- 3. the gate that actually gates runs both of these -------------------

# scripts/ci-local.sh is what .husky/pre-commit runs, and its own header
# claims it "mirrors ci.yml job-for-job, not just a fast subset of it".
# This is that claim, for these two scripts, turned into something that
# fails rather than something a reader has to trust. It reads for the
# literal invocation rather than any mention, so a line that only names
# the script in a comment does not satisfy it.
echo "==> the pre-commit gate runs these checks"
gate="scripts/ci-local.sh"
if [ ! -f "$gate" ]; then
  note "FAIL: $gate does not exist, so nothing can be said about whether these checks run on a commit."
else
  for invoked in scripts/api/check-contract-drift.sh scripts/api/check-client-paths.sh scripts/api/selftest.sh; do
    if grep -qE "^[[:space:]]*bash $invoked" "$gate"; then
      echo "  ok: $gate runs $invoked"
    else
      note "FAIL: $gate does not run $invoked. .github/workflows/ci.yml runs only on a pull request into \`release\` (#575), so a check that lives only there runs on nothing bound for main. Add \`bash $invoked\` to $gate."
    fi
  done
fi

# ---- 4. the contract does not disagree with itself -----------------------

# security vs x-authenticated / x-csrf-required, per operation.
#
# The two say the same thing to different readers, and only one of them is
# obeyed here: gen-bindings.go decodes `security` and reads nothing out of
# it, so that block can say anything at all and both generated bindings
# stay byte for byte identical. That makes the OpenAPI half invisible to
# every other check in this file, which is how it came to be wrong about a
# third of the document while every gate stayed green.
echo "==> the contract's security blocks agree with its own extensions"
if python3 - "$API_CONTRACT" >"$tmp/security" 2>&1 <<'PYSEC'
import json, sys

VERBS = ("get", "put", "post", "delete", "options", "head", "patch", "trace")

with open(sys.argv[1]) as f:
    doc = json.load(f)

schemes = set((doc.get("components") or {}).get("securitySchemes") or {})

# The pairs this rule compares. csrf and csrfCookie are the two halves of
# one double-submit requirement and are always required together, so both
# are read against the same extension: a document that asked for the header
# without naming the cookie a client has to echo would be describing a
# requirement nobody can satisfy, which is the state this whole rule exists
# to have caught.
PAIRS = (("session", "x-authenticated"), ("csrf", "x-csrf-required"), ("csrfCookie", "x-csrf-required"))

problems = []
checked = 0

for path, item in (doc.get("paths") or {}).items():
    for verb, op in item.items():
        if verb.lower() not in VERBS:
            continue
        where = "%s %s (%s)" % (verb.upper(), path, op.get("operationId", "?"))
        security = op.get("security")
        # Fail closed on a shape this rule cannot decide rather than
        # skipping it. A list of alternatives is legal OpenAPI and this
        # contract has never declared one, so meeting one means this rule
        # needs rewriting, not relaxing, and a silent skip here would be
        # indistinguishable from a comparison that passed.
        if not isinstance(security, list) or len(security) != 1 or not isinstance(security[0], dict):
            problems.append("%s: security is %r. This rule reads exactly one requirement set per operation, which "
                            "is all this contract has ever declared; anything else needs the rule rewritten rather "
                            "than skipped." % (where, security))
            continue
        required = security[0]
        unknown = sorted(set(required) - schemes)
        if unknown:
            problems.append("%s: names security scheme(s) %s, which components.securitySchemes does not declare."
                            % (where, ", ".join(unknown)))
        checked += 1

        for scheme, extension in PAIRS:
            declared = scheme in required
            extended = op.get(extension) is True
            if declared != extended:
                problems.append(
                    "%s: security %s %r, and %s is %r. These are two declarations of one fact, and both are read: an "
                    "external consumer implements `security`, and scripts/api/gen-bindings.go obeys the extension. "
                    "Say the same thing in both."
                    % (where, "declares" if declared else "does not declare", scheme, extension, op.get(extension)))

if checked == 0:
    print("no operation declared a security requirement at all, so this rule compared nothing and would pass vacuously")
    sys.exit(1)

if problems:
    for p in problems:
        print(p)
    sys.exit(1)

print("ok: %d operation(s) say the same thing about a session and about CSRF in `security` and in their extensions" % checked)
PYSEC
then
  sed 's/^/  /' "$tmp/security"
else
  note "FAIL: the contract's security blocks disagree with its own x-authenticated/x-csrf-required extensions."
  sed 's/^/    /' "$tmp/security" >&2
fi

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "  How to regenerate, and what a drift failure means: docs/api/contract.md" >&2
  exit 1
fi

echo "OK: the /api/v1 bindings match $API_CONTRACT, and no implementation type reaches the public schema."

#!/usr/bin/env bash
# Positive controls for the /api/v1 contract gates (issue #166).
#
# Every rule those gates enforce is a negative assertion: "the generated
# bindings match the contract", "no implementation type reaches the public
# schema", "no handler shape drifted", "no route exists outside the
# contract", "no error code is emitted that the registry does not know". A
# negative assertion that has never been seen to fail is indistinguishable
# from one that cannot fail, and this repository has been bitten by exactly
# that twice: a scanner whose `\b` never matched between `_` and `p`, and a
# self-test that "caught" every mutation because the check script was
# missing from the copy it ran in.
#
# So each rule is mutation-tested against the REAL tree: a copy of the
# working tree gets one deliberate violation planted in a real file, the
# check runs, and it must fail AND print the message that names the planted
# reason. Asserting the message, not merely the exit code, is what stops a
# check that failed for an unrelated reason from reading as a pass.
#
# The Go controls run `go test`, which is slower than the static checks
# but is the only thing that can prove a HANDLER drift is caught: that
# check needs reflection over unexported types and cannot be a shell scan.
#
# Not covered here, on purpose: the TypeScript side's own consumption
# checks (ui/shared/src/api/contract.conformance.test.ts). Those need an
# installed npm workspace, which this script deliberately does not build;
# they run in the ordinary `npm test` job. What IS covered here is the
# TypeScript DRIFT rule, which is the one CI has to fail on, in both
# directions: a hand-edited generated module, and a contract change nobody
# regenerated.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

root=$(pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-api-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# mutant <name> copies the working tree into $tmp/<name> and echoes its
# path. --cached --others --exclude-standard, not plain `git ls-files`: the
# copy has to include files that are present but not yet committed, because
# the checks being tested are themselves usually uncommitted while they are
# being written. Copying tracked files only is what produced a self-test
# elsewhere in this repository that silently "caught" every mutation,
# because the check script it invoked did not exist in the copy at all.
mutant() {
  local name=$1
  local dir="$tmp/$name"
  mkdir -p "$dir"
  (cd "$root" && git ls-files -z --cached --others --exclude-standard | tar -cf - --null -T -) | (cd "$dir" && tar -xf -)
  git -C "$dir" init -q
  git -C "$dir" add -A
  git -C "$dir" -c user.email=selftest@example.invalid -c user.name=selftest commit -q -m "selftest baseline"
  printf '%s' "$dir"
}

# expect_check_fails <label> <dir> <expected-substring> <command...>
expect_check_fails() {
  local label=$1 dir=$2 expect=$3; shift 3
  if (cd "$dir" && "$@") >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $label. The check PASSED against a planted violation." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  elif ! grep -qF "$expect" "$tmp/out"; then
    echo "SELFTEST FAIL: $label. The check failed, but not for the planted reason." >&2
    echo "    expected its output to contain: $expect" >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  else
    echo "  ok (caught): $label"
    pass=$((pass + 1))
  fi
}

expect_check_passes() {
  local label=$1 dir=$2; shift 2
  if (cd "$dir" && "$@") >"$tmp/out" 2>&1; then
    echo "  ok (clean):  $label"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $label. The check FAILED against an unmutated tree, so its failures mean nothing." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

drift() { bash scripts/api/check-contract-drift.sh; }
gotest() { (cd apps/common && go test -count=1 ./webhost/ ./auth/local/); }

echo "==> negative control: the gates are clean on the real tree"
expect_check_passes "check-contract-drift" "$root" bash scripts/api/check-contract-drift.sh

echo
echo "==> generated output is generated, not edited"

d=$(mutant go-binding-hand-edited)
# A plausible hand edit rather than a syntax error: someone "fixing" a
# field name in the generated Go instead of in the contract.
sed -i.bak 's/json:"config_revision"/json:"configRevision"/' "$d/apps/common/webhost/apicontract/contract.gen.go"
rm -f "$d/apps/common/webhost/apicontract/contract.gen.go.bak"
expect_check_fails "a hand edit to the generated Go binding" "$d" \
  "the checked-in Go binding does not match" bash scripts/api/check-contract-drift.sh

d=$(mutant ts-binding-hand-edited)
sed -i.bak 's/plan_id: string;/plan_id?: string;/' "$d/ui/shared/src/api/generated/contract.ts"
rm -f "$d/ui/shared/src/api/generated/contract.ts.bak"
expect_check_fails "a hand edit to the generated TypeScript binding" "$d" \
  "the checked-in TypeScript binding does not match" bash scripts/api/check-contract-drift.sh

d=$(mutant ts-binding-missing)
rm -f "$d/ui/shared/src/api/generated/contract.ts"
expect_check_fails "a deleted TypeScript binding" "$d" \
  "is missing. Run scripts/api/generate.sh" bash scripts/api/check-contract-drift.sh

d=$(mutant contract-changed-without-regenerating)
# The other direction, and the one issue #166 names explicitly: the
# contract changes and nobody regenerates, so the TypeScript client is
# still compiling against the old shape.
sed -i.bak 's/"known_hosts_line"/"known_hosts"/g' "$d/api/v1/openapi.json"
rm -f "$d/api/v1/openapi.json.bak"
expect_check_fails "a contract change nobody regenerated the bindings for" "$d" \
  "the checked-in TypeScript binding does not match" bash scripts/api/check-contract-drift.sh
# The same mutation, caught by the ordinary `go test` run rather than by the
# dedicated job. Both matter: scripts/ci-local.sh runs the tests, and a
# contributor should not have to know this gate exists to be told about it.
expect_check_fails "the same change, caught by go test's own digest check" "$d" \
  "but the generated bindings were made from" bash -c 'cd apps/common && go test -count=1 -run TestContract_TheBindingsWereGeneratedFromThisContract ./webhost/'

echo
echo "==> the public schema carries no implementation type"

d=$(mutant schema-leaks-sqlite)
# Deliberately underscore-prefixed. A `\bsqlite` rule would never fire
# between "_" and "s", which is the exact shape of the scanner bug this
# repository already shipped once.
python3 - "$d/api/v1/openapi.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    doc = json.load(f)
schema = doc["components"]["schemas"]["VersionResponse"]
schema["properties"]["local_sqlite_path"] = {"type": "string"}
schema["required"].append("local_sqlite_path")
with open(sys.argv[1], "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
expect_check_fails "a SQLite implementation type on a public schema" "$d" \
  "an implementation type reached the public schema" bash scripts/api/check-contract-drift.sh

d=$(mutant schema-leaks-provider-sdk)
python3 - "$d/api/v1/openapi.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    doc = json.load(f)
doc["components"]["schemas"]["CapabilitiesResponse"]["properties"]["ugos_sdk_handle"] = {"type": "string"}
doc["components"]["schemas"]["CapabilitiesResponse"]["required"].append("ugos_sdk_handle")
with open(sys.argv[1], "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
expect_check_fails "a provider SDK type on a public schema" "$d" \
  "an implementation type reached the public schema" bash scripts/api/check-contract-drift.sh

echo
echo "==> a gate that inspected nothing refuses"

d=$(mutant contract-with-no-operations)
python3 - "$d/api/v1/openapi.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    doc = json.load(f)
doc["paths"] = {}
with open(sys.argv[1], "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
expect_check_fails "a contract that declares no operations at all" "$d" \
  "would produce nothing and pass vacuously" bash scripts/api/check-contract-drift.sh

d=$(mutant contract-unparseable)
printf 'not json at all\n' > "$d/api/v1/openapi.json"
expect_check_fails "a contract the generator cannot read" "$d" \
  "the generator refused" bash scripts/api/check-contract-drift.sh

echo
echo "==> the gate that gates actually runs these checks"

# M1 (PR #194 review): these controls are the reason the two lines in
# scripts/ci-local.sh cannot quietly go missing again. Removing either
# invocation is a one-line edit that leaves every other check green, and
# it is exactly the edit that made three controls inert for four PRs.
d=$(mutant ci-local-without-the-drift-gate)
sed -i.bak '/^[[:space:]]*bash scripts\/api\/check-contract-drift.sh/d' "$d/scripts/ci-local.sh"
rm -f "$d/scripts/ci-local.sh.bak"
expect_check_fails "the drift gate dropped from the pre-commit gate" "$d" \
  "does not run scripts/api/check-contract-drift.sh" bash scripts/api/check-contract-drift.sh

d=$(mutant ci-local-without-the-selftest)
sed -i.bak '/^[[:space:]]*bash scripts\/api\/selftest.sh/d' "$d/scripts/ci-local.sh"
rm -f "$d/scripts/ci-local.sh.bak"
expect_check_fails "this self-test dropped from the pre-commit gate" "$d" \
  "does not run scripts/api/selftest.sh" bash scripts/api/check-contract-drift.sh

echo
echo "==> the generator refuses a shape it would otherwise drop"

# submitOperation's 409 body is a oneOf (M2): its three codes do not all
# use the same shape. Neither binding generates a type from an error body,
# so oneOf is safe THERE and nowhere else - and "nowhere else" has to be
# enforced, because objectSchemaNames skips a oneOf-only schema in silence,
# which would delete a shape from both bindings without a diff.
d=$(mutant contract-oneof-on-a-named-schema)
python3 - "$d/api/v1/openapi.json" <<'PY2'
import json, sys
with open(sys.argv[1]) as f:
    doc = json.load(f)
doc["components"]["schemas"]["SubmitOperationConflict"] = {
    "oneOf": [
        {"$ref": "#/components/schemas/ConfigRevisionStaleResponse"},
        {"$ref": "#/components/schemas/ErrorResponse"},
    ]
}
with open(sys.argv[1], "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY2
expect_check_fails "a named schema using oneOf, which both bindings would drop" "$d" \
  "would be dropped from both bindings without a word" bash scripts/api/check-contract-drift.sh

echo
echo "==> Go handlers still match the contract (go test)"

d=$(mutant handler-field-renamed)
sed -i.bak 's/ConfigRevision string `json:"config_revision"`/ConfigRevision string `json:"revision"`/' "$d/apps/common/webhost/handlers_system.go"
rm -f "$d/apps/common/webhost/handlers_system.go.bak"
expect_check_fails "a handler response field renamed without the contract" "$d" \
  "is in the handler type but not in the contract" bash -c 'cd apps/common && go test -count=1 -run TestContract ./webhost/'

d=$(mutant handler-field-added)
sed -i.bak 's/\tReady bool `json:"ready"`/\tReady bool `json:"ready"`\n\tSQLitePath string `json:"sqlite_path"`/' "$d/apps/common/webhost/handlers_system.go"
rm -f "$d/apps/common/webhost/handlers_system.go.bak"
expect_check_fails "a handler response field added without the contract" "$d" \
  "is in the handler type but not in the contract" bash -c 'cd apps/common && go test -count=1 -run TestContract ./webhost/'

d=$(mutant route-outside-the-contract)
sed -i.bak 's|\t\tr.Get("/validators", h.listValidators)|\t\tr.Get("/validators", h.listValidators)\n\t\tr.Get("/rclone/remotes", h.listValidators)|' "$d/apps/common/webhost/router.go"
rm -f "$d/apps/common/webhost/router.go.bak"
expect_check_fails "a route the contract does not declare" "$d" \
  "which the contract does not declare" bash -c 'cd apps/common && go test -count=1 -run TestContract ./webhost/'

d=$(mutant unregistered-error-code)
sed -i.bak 's|writeError(w, http.StatusNotFound, "OPERATION_NOT_FOUND"|writeError(w, http.StatusNotFound, "OPERATION_VANISHED"|' "$d/apps/common/webhost/handlers_operations.go"
rm -f "$d/apps/common/webhost/handlers_operations.go.bak"
expect_check_fails "an error code no registry knows" "$d" \
  "which api/v1/openapi.json does not register as a wire code" bash -c 'cd apps/common && go test -count=1 -run TestContract ./webhost/'

d=$(mutant profile-changes-backup-semantics)
# A runtime profile reaching into a backup response is the fork this whole
# phase exists to prevent, so the parity test has to catch it rather than
# only comparing statuses.
python3 - "$d/apps/common/webhost/handlers_backupsets.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
s = s.replace(
    "func (h *handlers) listBackupSets(w http.ResponseWriter, r *http.Request) {",
    "func (h *handlers) listBackupSets(w http.ResponseWriter, r *http.Request) {\n\tif h.platform.ID() == \"ugos\" {\n\t\twriteJSON(w, http.StatusOK, listBackupSetsResponse{})\n\t\treturn\n\t}",
    1,
)
open(p, "w").write(s)
PY
expect_check_fails "a runtime profile changing what a backup endpoint means" "$d" \
  "differs between the generic and ugos profiles" bash -c 'cd apps/common && go test -count=1 -run TestProfileParity ./webhost/'

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "FAIL: $fail of $((pass + fail)) API-contract controls did not behave as required." >&2
  exit 1
fi
echo
echo "OK: all $pass API-contract controls behaved as required (every rule was shown to fire against a real planted violation, and shown not to fire on the real tree)."

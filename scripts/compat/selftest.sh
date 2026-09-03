#!/usr/bin/env bash
# Positive controls for EPIC E's FR-35 compatibility gate (issue #242).
#
# core/tests/compat is a wall of negative assertions: "config validation
# did not move", "the artifact rows are what the migrations left", "the
# CLI prints what it printed", "the contract lost nothing". A negative
# assertion nobody has watched fail is indistinguishable from one that
# cannot fail, and this repository has now found fifteen checks that
# passed for the wrong reason, three of them on the day this gate was
# written: a rule that could never fire because settled rows counted as
# throughput, a mutation that disabled two mechanisms at once so a budget
# looked safe, and a guard that matched a flag in the comment explaining
# the flag rather than in the command.
#
# So every cell in that corpus is mutation-tested against the real tree
# here: a copy of the working tree gets one deliberate violation planted
# in a real file, the gate runs, and it must fail AND name the cell whose
# promise the violation broke. Naming the cell, not merely failing, is
# what stops a mutation that broke the build for an unrelated reason from
# reading as a pass.
#
# Two of the violations below are the ones docs/EPIC-E-alternative-storage.md
# section 4 names by hand:
#
#   "Compatibility (FR-35): a migration variant that rewrites
#    retention_tier during backfill; the golden retention suite must fail
#    it."
#
# and the source-safety family's shape, applied to the one destructive
# path that exists in this tree today: prune deleting a file it could not
# stat instead of refusing.
#
# This is not fast. Each mutant builds core/ and backup-manager and runs a
# real capture, so budget a few minutes. It is the only thing standing
# between "the compatibility suite is green" and "the compatibility suite
# is green because it cannot go red".
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

root=$(pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-compat-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# mutant <name> copies the working tree into $tmp/<name> and echoes its
# path.
#
# --cached --others --exclude-standard, not plain `git ls-files`: the copy
# has to include files that are present but not yet committed, because the
# gate being tested is itself usually uncommitted while it is being
# written. Copying tracked files only is what produced a self-test
# elsewhere in this repository that silently "caught" every mutation,
# because the check it invoked did not exist in the copy at all.
mutant() {
  local name=$1
  local dir="$tmp/$name"
  mkdir -p "$dir"
  (cd "$root" && git ls-files -z --cached --others --exclude-standard | tar -cf - --null -T -) | (cd "$dir" && tar -xf -)
  printf '%s' "$dir"
}

# compat_gate runs the FR-35 suite in whichever tree it is called from.
compat_gate() {
  (cd core && GOWORK=off go test -count=1 ./tests/compat/)
}

# expect_cell_fails <label> <dir> <cell-name>
#
# The cell name is the expected substring, so a mutation that broke the
# build, or broke a different promise, does not read as this control
# passing.
expect_cell_fails() {
  local label=$1 dir=$2 cell=$3
  if (cd "$dir" && compat_gate) >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $label. The FR-35 gate PASSED against a planted violation." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  elif ! grep -qF "$cell" "$tmp/out"; then
    echo "SELFTEST FAIL: $label. The gate failed, but never named the cell whose promise was broken." >&2
    echo "    expected its output to mention: $cell" >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  else
    echo "  ok (caught): $label -> $cell"
    pass=$((pass + 1))
  fi
}

expect_gate_passes() {
  local label=$1 dir=$2
  if (cd "$dir" && compat_gate) >"$tmp/out" 2>&1; then
    echo "  ok (clean):  $label"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $label. The gate FAILED against an unmutated tree, so its failures mean nothing." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

# plant_migration <dir> <body>
#
# Writes <body> as the NEXT migration in <dir>, whichever number that is.
#
# These controls used to hardcode 0007, which worked right up until 0007
# was taken: EPIC E's placements migration landed on main and every
# migration-shaped control started failing for "migrations 0007_placements
# and 0007_compat_selftest both claim version 7" instead of for the
# violation it planted. The gate still went red, which is why
# expect_cell_fails insists on the output naming the cell rather than
# accepting any red at all, and that insistence is the only reason this
# was caught instead of read as five passes.
plant_migration() {
  local dir=$1 body=$2 next
  next=$(( $(ls "$dir"/core/migrations/[0-9]*.sql \
             | sed 's|.*/||; s|_.*||; s|^0*||' \
             | sort -n | tail -1) + 1 ))
  printf '%s\n' "$body" > "$dir/core/migrations/$(printf '%04d' "$next")_compat_selftest_planted_violation.sql"
}

echo "==> negative control: the FR-35 gate is clean on the real tree"
expect_gate_passes "core/tests/compat on an unmutated tree" "$root"

echo
echo "==> the spec's own planted violation for FR-35"

d=$(mutant backfill-rewrites-retention-tier)
# docs/EPIC-E-alternative-storage.md section 4, verbatim: "a migration
# variant that rewrites retention_tier during backfill". Written as the
# next migration in sequence, which is exactly where the placements
# migration will land, so this is the mistake in the place it would
# actually be made.
plant_migration "$d" '-- PLANTED VIOLATION (scripts/compat/selftest.sh). A backfill that helps
-- itself to a column it does not own.
UPDATE artifacts SET retention_tier = '\'''\'';'
expect_cell_fails "a backfill that rewrites retention_tier" "$d" "10-upgraded-artifact-rows"

d=$(mutant backfill-rewrites-discovered-at)
# The same shape aimed at the field FR-32 protects: an artifact's
# retention bucketing must be invariant under anything a migration or a
# move does to it, so rewriting discovery time has to move the verdicts.
plant_migration "$d" '-- PLANTED VIOLATION (scripts/compat/selftest.sh). Discovery time is
-- journal truth (FR-32); nothing may re-derive it from anywhere else.
--
-- The value moves the artifact into a different calendar month on
-- purpose. A rewrite that lands in the same bucket changes the row and
-- not the decision, and the decision is what this control is aimed at:
-- the first draft shifted the timestamp by one minute, the verdicts did
-- not move, and the control failed for being too gentle rather than the
-- gate failing for being too weak.
UPDATE artifacts SET discovered_at = '\''2026-08-29T09:00:00Z'\'' WHERE artifact_name = '\''monthly-only.dump'\'';'
expect_cell_fails "a backfill that rewrites the journal's discovery timestamp" "$d" "11-upgraded-retention-verdicts"

echo
echo "==> the schema an upgrade inherits"

d=$(mutant migration-drops-an-index)
plant_migration "$d" '-- PLANTED VIOLATION (scripts/compat/selftest.sh). An upgrade may add to
-- the schema; it may not take away from it.
DROP INDEX idx_artifacts_state;'
expect_cell_fails "a migration that drops an index an upgrade inherits" "$d" "03-migrated-schema"

d=$(mutant migration-adds-a-column-to-artifacts)
# FR-29 decided placements live in a new table "rather than new columns on
# the artifact row". This is that decision being quietly reversed.
plant_migration "$d" '-- PLANTED VIOLATION (scripts/compat/selftest.sh). FR-29 put placements in
-- their own table on purpose.
ALTER TABLE artifacts ADD COLUMN placement_medium TEXT NOT NULL DEFAULT '\''local'\'';'
expect_cell_fails "a placement column bolted onto the artifact row" "$d" "02-artifact-rows-after-migration"

echo
echo "==> the decisions a medium-free deployment already makes"

d=$(mutant absent-medium-stops-meaning-local)
python3 - "$d/core/internal/config/config.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = '''func (t RetentionTier) EffectiveMedium() string {
	if t.Medium == "" {
		return MediumLocal
	}
	return t.Medium
}'''
new = '''func (t RetentionTier) EffectiveMedium() string {
	return t.Medium
}'''
assert old in s, "EffectiveMedium no longer has the shape this control mutates"
open(p, "w").write(s.replace(old, new))
PY
expect_cell_fails "a tier with no medium key resolving to no medium at all" "$d" "01-config-validation"

d=$(mutant last-known-good-protection-dropped)
python3 - "$d/core/internal/retention/lastknowngood.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = '''	if !lkg.Protected {
		return verdicts
	}'''
new = '''	if true {
		return verdicts
	}'''
assert old in s, "ApplyLastKnownGood no longer has the shape this control mutates"
open(p, "w").write(s.replace(old, new, 1))
PY
expect_cell_fails "FR-19 protection silently not composed onto the verdicts" "$d" "04-retention-verdicts"

d=$(mutant prune-deletes-what-it-cannot-stat)
python3 - "$d/core/internal/retention/prune.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = '''	info, err := os.Lstat(expected)
	if err != nil {
		return "", fmt.Errorf("retention: prune: refusing %s: cannot stat %q: %w", rec.Artifact, expected, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {'''
new = '''	info, err := os.Lstat(expected)
	if err != nil {
		return expected, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {'''
assert old in s, "the prune stat refusal no longer has the shape this control mutates"
open(p, "w").write(s.replace(old, new, 1))
PY
expect_cell_fails "prune promoting a file it could not stat from REFUSE to DELETE" "$d" "05-prune-verdicts"

echo
echo "==> what an operator sees in a terminal"

d=$(mutant cli-renders-a-placement-line-unconditionally)
# FR-35 allows an additive CLI column only when a non-local placement
# exists. This is that column rendered on a deployment that has none,
# which is the single most likely way EPIC E breaks this clause.
python3 - "$d/core/cmd/backup-manager/artifacts.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = '''	if rec.RetentionTier != "" {
		fmt.Printf("retention_tier:      %s\\n", rec.RetentionTier)
	}'''
new = '''	fmt.Printf("placements:          local\\n")
	if rec.RetentionTier != "" {
		fmt.Printf("retention_tier:      %s\\n", rec.RetentionTier)
	}'''
assert old in s, "printArtifactDetail no longer has the shape this control mutates"
open(p, "w").write(s.replace(old, new, 1))
PY
expect_cell_fails "an additive CLI line rendered with no non-local placement anywhere" "$d" "06-cli-surfaces"

d=$(mutant cli-retention-line-reshaped)
python3 - "$d/core/cmd/backup-manager/retention.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = '''			fmt.Printf("  %-6s %-40s tiers=%v\\n", decision, v.Artifact.Name, v.Tiers)'''
new = '''			fmt.Printf("  %-6s %-40s medium=local tiers=%v\\n", decision, v.Artifact.Name, v.Tiers)'''
assert old in s, "the retention preview line no longer has the shape this control mutates"
open(p, "w").write(s.replace(old, new, 1))
PY
expect_cell_fails "the retention preview line growing a medium column for a local-only deployment" "$d" "07-cli-retention-preview"

d=$(mutant cli-usage-line-reworded)
# The usage block is compared additively so a new subcommand does not
# force a regeneration. This is the other direction: a line an operator
# already reads, quietly reworded.
python3 - "$d/core/cmd/backup-manager/main.go" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
old = "  reconcile                                      run FR-17 reconciliation for every backup set"
assert old in s, "the usage block no longer has the line this control rewords"
open(p, "w").write(s.replace(old, "  reconcile                                      reconcile every backup set", 1))
PYEOF
expect_cell_fails "a usage line reworded under an operator who already read it" "$d" \
  "06b-cli-usage-block"

echo
echo "==> what the /api/v1 contract already promises"

d=$(mutant contract-drops-a-response-property)
python3 - "$d/api/v1/openapi.json" <<'PY'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
schema = doc["components"]["schemas"]["Artifact"]
assert "retention_tier" in schema["properties"], "the Artifact schema no longer has the property this control removes"
del schema["properties"]["retention_tier"]
with open(p, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
expect_cell_fails "a response property taken off the contract" "$d" "08-api-contract-promises"

d=$(mutant contract-changes-a-property-type)
python3 - "$d/api/v1/openapi.json" <<'PY'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
schema = doc["components"]["schemas"]["Artifact"]
schema["properties"]["size_bytes"] = {"type": "string"}
with open(p, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
expect_cell_fails "a response property whose type changed under a client" "$d" "08-api-contract-promises"

d=$(mutant contract-adds-a-required-request-field)
# The one break additive-only cannot see on its own: adding something is
# an addition, and this addition breaks every client already sending the
# old shape.
python3 - "$d/api/v1/openapi.json" <<'PY'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
schema = doc["components"]["schemas"]["ApplyRetentionRequest"]
schema.setdefault("properties", {})["acknowledge_medium_disclosure"] = {"type": "boolean"}
schema.setdefault("required", []).append("acknowledge_medium_disclosure")
with open(p, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
expect_cell_fails "a new required field on a request every existing client already sends" "$d" "09-api-request-requirements"

d=$(mutant contract-adds-a-required-query-parameter)
# The parameter-shaped version of the same blind spot: additive-only sees
# a new line and waves it through, and every caller that was not already
# sending it starts getting a 400.
python3 - "$d/api/v1/openapi.json" <<'PYEOF'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
op = doc["paths"]["/activity"]["get"]
op.setdefault("parameters", []).append(
    {"name": "medium", "in": "query", "required": True, "schema": {"type": "string"}}
)
with open(p, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PYEOF
expect_cell_fails "a new required query parameter on an operation clients already call" "$d" \
  "09-api-request-requirements"

echo
echo "==> the numbers FR-34 refuses to invent"

d=$(mutant contract-serves-a-cost-figure)
python3 - "$d/api/v1/openapi.json" <<'PYEOF'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
doc["components"]["schemas"]["Artifact"]["properties"]["estimated_restore_cost_usd"] = {"type": "number"}
with open(p, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PYEOF
expect_cell_fails "a cost figure on a public schema" "$d" \
  "no surface renders a cost figure or an invented ETA"

d=$(mutant contract-serves-a-restore-eta)
python3 - "$d/api/v1/openapi.json" <<'PYEOF'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
props = doc["components"]["schemas"]["Artifact"]["properties"]
props["restore_eta_seconds"] = {"type": "integer"}
props["restore_percent_complete"] = {"type": "integer"}
with open(p, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PYEOF
expect_cell_fails "an invented restore ETA and a percentage S3 never reports" "$d" \
  "no surface renders a cost figure or an invented ETA"

echo
echo "==> the invariant a regenerated corpus cannot silence"

d=$(mutant backfill-rewrites-retention-tier-and-corpus-regenerated)
plant_migration "$d" '-- PLANTED VIOLATION (scripts/compat/selftest.sh). The same backfill as
-- above, with the corpus obligingly re-captured around it, which is what
-- somebody in a hurry does to a red golden file.
UPDATE artifacts SET retention_tier = '\'''\'';'
(cd "$d/core" && COMPAT_UPDATE=1 GOWORK=off go test -count=1 -run TestMediumFreeSurfacesAreUnchanged ./tests/compat/ >/dev/null 2>&1) || true
expect_cell_fails "the same backfill, with the corpus regenerated to accept it" "$d" \
  "differs between a fresh install and an in-place upgrade"

echo
echo "==> the matrix cannot claim a suite that is not there"

d=$(mutant matrix-cites-a-suite-that-does-not-exist)
python3 - "$d/docs/conformance/epic-e-matrix.md" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
old = "`core/tests/compat`, twelve cells"
assert old in s, "the P2.7 row no longer has the shape this control mutates"
open(p, "w").write(s.replace(old, "`core/tests/there-is-no-such-suite`", 1))
PYEOF
expect_cell_fails "a PASS row citing a suite this repository does not have" "$d" \
  "name something this repository does not have"

d=$(mutant matrix-has-nothing-blocked)
python3 - "$d/docs/conformance/epic-e-matrix.md" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
# The optimistic edit: every blocked row quietly promoted to PASS.
out = re.sub(r"\| BLOCKED \(#[0-9, #]+\) \|", "| PASS |", s)
assert out != s, "no BLOCKED row left to promote"
open(p, "w").write(out)
PYEOF
expect_cell_fails "every blocked row promoted to PASS" "$d" \
  "no row in the matrix is BLOCKED"

echo
echo "==> the gate's own corpus"

d=$(mutant corpus-cell-deleted)
# A cell quietly dropped from the corpus is how a gate shrinks to nothing
# one commit at a time.
python3 - "$d/core/tests/compat/testdata/medium-free-surfaces.json" <<'PY'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
del doc["cells"]["05-prune-verdicts"]
with open(p, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
expect_cell_fails "a corpus cell deleted rather than satisfied" "$d" "05-prune-verdicts"

d=$(mutant corpus-cell-emptied)
python3 - "$d/core/tests/compat/testdata/medium-free-surfaces.json" <<'PY'
import json, sys
p = sys.argv[1]
doc = json.load(open(p))
doc["cells"]["06-cli-surfaces"]["lines"] = []
with open(p, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
expect_cell_fails "a corpus cell emptied so it passes whatever the product does" "$d" "06-cli-surfaces"

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "FAIL: $fail of $((pass + fail)) FR-35 compatibility controls did not behave as required." >&2
  exit 1
fi
echo
echo "OK: all $pass FR-35 compatibility controls behaved as required (every cell was shown to go red against a real planted violation, and shown not to on the real tree)."

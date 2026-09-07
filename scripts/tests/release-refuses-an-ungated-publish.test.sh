#!/usr/bin/env bash
# The release gate only gates while somebody configured branch protection.
# This is the half that does not depend on a settings page (issue #575).
#
# `release gate` in .github/workflows/ci.yml is the required check on a
# pull request into `release`, and its sibling test
# release-gate-covers-every-job.test.sh keeps it covering every job. Both
# of those are about the pull request. Neither is about the other ways a
# commit reaches `release`: a direct push, an administrator merging past a
# red check, a ruleset somebody edited. All three publish, because
# .github/workflows/release.yml triggers on the push and pushes a signed
# image to a public registry, and until the `require-release-gate` job
# existed none of them met a check at all.
#
# That job asks GitHub whether `release gate` passed on the commit being
# published, or on the head that commit merged in, and refuses `publish`
# if the answer is anything else. This drives it.
#
# It does not pattern-match the YAML. It pulls the job's own script out of
# the workflow and runs it with a stand-in `gh` on PATH, once per outcome
# worth having, for the reason release_publish_gate_test.go gives about
# the `decide` script next door: a guard asserted by grep survives being
# rewritten into something that no longer works. GitHub Actions is the
# only thing that ever runs this workflow, so a test that executes the
# script is the only place these refusals are ever performed.
#
# Nine behaviours, and the shape of the workflow around them:
#
#   1. a merge whose head passed the gate publishes;
#   2. a commit nothing checked does not;
#   3. a gate that concluded `failure` does not;
#   4. a gate that concluded `skipped` does not, which is the case branch
#      protection itself gets wrong: a skipped required check counts as
#      satisfied, so this is the one reading that has to be made here;
#   5. a green check run posted by some other app does not, because
#      anything holding checks:write can write one called `release gate`;
#   6. a query that could not be answered refuses rather than assumes;
#   7. `Release-Gate-Override: <why>` publishes anyway and says so;
#   8. the same trailer with no reason refuses, because the reason is the
#      whole audit trail;
#   9. a run that publishes nothing does not ask GitHub anything at all.
#
# Run directly (`bash scripts/tests/release-refuses-an-ungated-publish.test.sh`)
# or let the gate run it: scripts/ci-local.sh invokes it in every run.
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/.." && pwd)"
WORKFLOW="$REPO_ROOT/.github/workflows/release.yml"
JOB="require-release-gate"

echo "==> release publish gate: only a commit the release gate passed publishes"

checks=0
failures=0
pass() { checks=$((checks + 1)); printf '    ok   %s\n' "$1"; }
fail() {
  checks=$((checks + 1))
  failures=$((failures + 1))
  printf '    FAIL %s\n' "$1" >&2
  if [ $# -gt 1 ] && [ -n "$2" ]; then
    printf '%s\n' "$2" | sed 's/^/         | /' >&2
  fi
}

# Inside the checkout and gitignored, like every other sandbox here:
# nothing in this repository should need a recursive delete outside the
# working tree to clean up after itself.
SANDBOX="$REPO_ROOT/.release-gate-test"
case "$SANDBOX" in
  /*/.release-gate-test) ;;
  *) echo "refusing to use sandbox path [$SANDBOX]" >&2; exit 1 ;;
esac
rm -rf "$SANDBOX"
mkdir -p "$SANDBOX"
trap 'rm -rf "$SANDBOX"' EXIT

# --- the script, pulled out of the workflow ------------------------------
#
# Parsed with the standard library rather than a YAML module, the same way
# release-gate-covers-every-job.test.sh does it, so this runs on any
# machine with python3 and needs nothing installed. The positive control
# below is what makes that safe: an extractor that quietly stopped
# matching would hand every case an empty script, and an empty script
# exits 0, which would report a clean sweep of refusals that never
# happened.
SCRIPT="$SANDBOX/require-release-gate.sh"
python3 - "$WORKFLOW" "$JOB" "$SCRIPT" <<'PY'
import re
import sys

workflow, job, out = sys.argv[1], sys.argv[2], sys.argv[3]
lines = open(workflow, encoding="utf-8").read().splitlines()

# Only inside the `jobs:` mapping. `on:` has two-space keys of its own
# (`push:`, `workflow_dispatch:`), and a parser that counts those as jobs
# is a parser whose answers about jobs are not about jobs.
in_jobs = False
in_job = False
body = []
run_indent = None
for line in lines:
    if re.match(r"^jobs:\s*$", line):
        in_jobs = True
        continue
    if in_jobs and line and not line.startswith(" ") and not line.startswith("#"):
        in_jobs = False
        continue
    if not in_jobs:
        continue
    m = re.match(r"^  ([A-Za-z0-9_.-]+):\s*$", line)
    if m:
        in_job = m.group(1) == job
        continue
    if not in_job:
        continue
    if run_indent is None:
        if re.match(r"^\s+run:\s*\|\s*$", line):
            run_indent = len(line) - len(line.lstrip(" "))
        continue
    if line.strip() == "":
        body.append("")
        continue
    indent = len(line) - len(line.lstrip(" "))
    if indent <= run_indent:
        break
    body.append(line[run_indent + 2:])

open(out, "w", encoding="utf-8").write("\n".join(body) + "\n")
print(len(body))
PY
extracted_lines="$(wc -l < "$SCRIPT" 2>/dev/null | tr -d ' ')"
if [ "${extracted_lines:-0}" -lt 80 ]; then
  fail "the extractor pulled ${extracted_lines:-0} lines out of ${JOB} in release.yml, which is far less than that job has ever been: it has stopped matching, and every case below would run an empty script and pass" "$(head -5 "$SCRIPT" 2>/dev/null)"
  echo "==> release publish gate: FAILED (the script under test could not be read)" >&2
  exit 1
fi
pass "the ${JOB} script came out of release.yml (${extracted_lines} lines)"

# --- the stand-in gh -----------------------------------------------------
#
# It answers from a per-case fixture directory and exits 1 when there is no
# fixture, which is also case 6: an unreachable API has to refuse. Every
# call is logged so a case can assert what was asked as well as what came
# back.
STUB_BIN="$SANDBOX/bin"
mkdir -p "$STUB_BIN"
cat > "$STUB_BIN/gh" <<'STUB'
#!/usr/bin/env bash
if [ "${1:-}" != "api" ]; then
  printf 'stub gh: expected `gh api`, got: %s\n' "$*" >&2
  exit 64
fi
path=""
for a in "$@"; do path="$a"; done
printf '%s\n' "$path" >> "$GH_CALLS"
key="$(printf '%s' "$path" | tr -c 'A-Za-z0-9' '_')"
if [ -f "$GH_FIXTURES/$key.json" ]; then
  cat "$GH_FIXTURES/$key.json"
  exit 0
fi
printf 'stub gh: HTTP 404: no fixture for %s\n' "$path" >&2
exit 1
STUB
chmod +x "$STUB_BIN/gh"

REPO="spdrman/rclone-manager"
MERGE="1111111111111111111111111111111111111111"
BASE="2222222222222222222222222222222222222222"
HEAD="3333333333333333333333333333333333333333"

FIX=""
CALLS=""

# new_case starts a fresh fixture directory and call log.
new_case() {
  FIX="$SANDBOX/fixtures-$1"
  CALLS="$SANDBOX/calls-$1"
  rm -rf "$FIX"
  mkdir -p "$FIX"
  : > "$CALLS"
}

# fixture <api path> reads the JSON body on stdin and files it under the
# path the script will ask for.
fixture() {
  local key
  key="$(printf '%s' "$1" | tr -c 'A-Za-z0-9' '_')"
  cat > "$FIX/$key.json"
}

# commit_fixture <sha> <message> [parent...]
commit_fixture() {
  local sha="$1" message="$2"; shift 2
  local parents="" p
  for p in "$@"; do
    [ -n "$parents" ] && parents="${parents},"
    parents="${parents}{\"sha\":\"${p}\"}"
  done
  python3 - "$sha" "$message" "$parents" <<'PY' | fixture "repos/${REPO}/commits/${sha}"
import json, sys
sha, message, parents = sys.argv[1], sys.argv[2], sys.argv[3]
print(json.dumps({
    "sha": sha,
    "commit": {"message": message},
    "parents": json.loads("[" + parents + "]") if parents else [],
}))
PY
}

# gate_fixture <sha> <conclusion> [app slug] [status]
gate_fixture() {
  local sha="$1" conclusion="$2" slug="${3:-github-actions}" status="${4:-completed}"
  python3 - "$conclusion" "$slug" "$status" <<'PY' | fixture "repos/${REPO}/commits/${sha}/check-runs?per_page=100&page=1"
import json, sys
conclusion, slug, status = sys.argv[1], sys.argv[2], sys.argv[3]
runs = [
    {"id": 1, "name": "core/ build, vet, test", "status": "completed", "conclusion": "success",
     "started_at": "2026-09-05T10:00:00Z", "app": {"slug": "github-actions"}},
    {"id": 2, "name": "release gate", "status": status,
     "conclusion": None if conclusion == "none" else conclusion,
     "started_at": "2026-09-05T10:30:00Z", "app": {"slug": slug},
     "html_url": "https://github.com/spdrman/rclone-manager/runs/2"},
]
print(json.dumps({"total_count": len(runs), "check_runs": runs}))
PY
}

# no_gate_fixture <sha> answers the check-runs query with a commit that has
# other checks and no `release gate` among them, which is what a commit
# nobody ran ci.yml against looks like.
no_gate_fixture() {
  fixture "repos/${REPO}/commits/${1}/check-runs?per_page=100&page=1" <<'JSON'
{"total_count": 0, "check_runs": []}
JSON
}

OUT=""
STATUS=0
SUMMARY=""

# run_gate <publish> <sha> runs the extracted script the way Actions would.
run_gate() {
  SUMMARY="$SANDBOX/summary"
  : > "$SUMMARY"
  OUT="$(
    PATH="$STUB_BIN:$PATH" \
    GH_FIXTURES="$FIX" \
    GH_CALLS="$CALLS" \
    PUBLISH="$1" \
    GITHUB_REPOSITORY="$REPO" \
    GITHUB_SHA="$2" \
    GITHUB_STEP_SUMMARY="$SUMMARY" \
    GH_TOKEN="unused-by-the-stub" \
    bash "$SCRIPT" 2>&1
  )"
  STATUS=$?
}

# expect <name> <want status> <substring> asserts the exit code and the
# distinct message, never the exit code alone: every refusal here exits 1,
# so a status-only assertion cannot tell "nothing checked this" from "the
# API did not answer", and those are triaged completely differently.
expect() {
  local name="$1" want_status="$2" want_text="$3"
  if [ "$STATUS" -ne "$want_status" ]; then
    fail "$name: exit ${STATUS}, want ${want_status}" "$OUT"
    return
  fi
  case "$OUT" in
    *"$want_text"*) pass "$name" ;;
    *) fail "$name: the exit code is right and the message is not; want a mention of [${want_text}]" "$OUT" ;;
  esac
}

refute_text() {
  case "$OUT" in
    *"$2"*) fail "$1: the output says [$2] and should not" "$OUT" ;;
    *) pass "$1" ;;
  esac
}

# 1. The release that is supposed to work. This is the positive control for
#    everything below it: without it, a script that refused every input
#    would pass every other case in this file.
new_case merged-and-green
commit_fixture "$MERGE" "Merge pull request #600 from spdrman/main" "$BASE" "$HEAD"
no_gate_fixture "$MERGE"
gate_fixture "$HEAD" success
run_gate true "$MERGE"
expect "a merge whose head passed the gate publishes" 0 "release gate: PASSED"
refute_text "and it publishes because the gate passed, not because anything was overridden" "OVERRIDDEN"

# 2. The case the job exists for.
new_case direct-push
commit_fixture "$MERGE" "Bump the version" "$BASE"
no_gate_fixture "$MERGE"
run_gate true "$MERGE"
expect "a commit nothing ever checked does not publish" 1 "nothing checked the commit this run would publish"
expect "and it says where it looked and found nothing" 1 "no \`release gate\` check run at all"

# 3. A gate that ran and went red.
new_case gate-failed
commit_fixture "$MERGE" "Merge pull request #601 from spdrman/main" "$BASE" "$HEAD"
no_gate_fixture "$MERGE"
gate_fixture "$HEAD" failure
run_gate true "$MERGE"
expect "a gate that concluded failure does not publish" 1 "concluded \`failure\`"

# 4. A gate that was skipped, which branch protection reads as a pass.
new_case gate-skipped
commit_fixture "$MERGE" "Merge pull request #602 from spdrman/main" "$BASE" "$HEAD"
no_gate_fixture "$MERGE"
gate_fixture "$HEAD" skipped
run_gate true "$MERGE"
expect "a gate that was skipped does not publish, whatever branch protection made of it" 1 "concluded \`skipped\`"

# 5. A green check run of the right name from the wrong writer.
new_case forged-check-run
commit_fixture "$MERGE" "Merge pull request #603 from spdrman/main" "$BASE" "$HEAD"
no_gate_fixture "$MERGE"
gate_fixture "$HEAD" success some-other-app
run_gate true "$MERGE"
expect "a green \`release gate\` posted by another app does not publish" 1 "rather than by GitHub Actions"

# 6. The API not answering. Fail closed, and say which query failed.
new_case api-unreachable
run_gate true "$MERGE"
expect "a query that could not be answered refuses rather than assumes" 1 "release gate could not be read"

# 7. The escape hatch.
new_case overridden
commit_fixture "$MERGE" "Cut 0.4.1
The registry outage took Actions down for the day and this fixes a
data-losing bug.

Release-Gate-Override: Actions is down and 0.4.0 loses backups" "$BASE" "$HEAD"
no_gate_fixture "$MERGE"
no_gate_fixture "$HEAD"
run_gate true "$MERGE"
expect "a commit carrying the override trailer publishes" 0 "release gate: OVERRIDDEN"
expect "and the reason is in the output rather than only in somebody's head" 0 "Actions is down and 0.4.0 loses backups"
expect "and it is annotated on the run, not merely logged" 0 "::warning title=release gate overridden::"

# 8. The override with nothing behind it.
new_case override-without-reason
commit_fixture "$MERGE" "Cut 0.4.2

Release-Gate-Override: yes" "$BASE" "$HEAD"
no_gate_fixture "$MERGE"
no_gate_fixture "$HEAD"
run_gate true "$MERGE"
expect "an override with no reason behind it refuses" 1 "release gate override carries no reason"

# 9. A run that publishes nothing does not gate anything, and proves it by
#    never asking. The call log is the assertion: a script that queried and
#    ignored the answer would pass a check on the exit code alone.
new_case dry-run
run_gate false "$MERGE"
expect "a run that publishes nothing is not gated" 0 "release gate: nothing to gate"
if [ -s "$CALLS" ]; then
  fail "a run that publishes nothing still queried GitHub" "$(cat "$CALLS")"
else
  pass "and it asked GitHub nothing at all"
fi

# 10. The publish decision arriving as something this job does not
#     recognise. Reading it as "not publishing" is the shape of every
#     defect in this area: quiet, green, and looking at nothing.
new_case unknown-publish
run_gate "" "$MERGE"
expect "a publish decision that is neither true nor false refuses" 1 "which is neither \`true\` nor \`false\`"

# 11. The stale trailer. A commit whose gate passed publishes on the gate,
#     so a trailer left behind from an earlier cut is noise rather than a
#     standing permission.
new_case stale-trailer
commit_fixture "$MERGE" "Cut 0.4.3

Release-Gate-Override: this was true three releases ago" "$BASE" "$HEAD"
no_gate_fixture "$MERGE"
gate_fixture "$HEAD" success
run_gate true "$MERGE"
expect "a passing gate publishes on the gate even when a stale trailer is sitting there" 0 "release gate: PASSED"
refute_text "and a stale trailer is not read as an override" "OVERRIDDEN"

# 12. The summary is what a reader sees on the run page, so a refusal that
#     only reaches stdout is a refusal nobody reads.
new_case summary-written
commit_fixture "$MERGE" "Bump the version" "$BASE"
no_gate_fixture "$MERGE"
run_gate true "$MERGE"
if grep -q "release gate: REFUSED" "$SUMMARY" 2>/dev/null; then
  pass "the refusal reaches the run summary and not only the log"
else
  fail "the refusal never reached GITHUB_STEP_SUMMARY, so the run page says nothing about why the release stopped" "$(cat "$SUMMARY" 2>/dev/null)"
fi

# --- the wiring around the script ---------------------------------------
#
# The script above can be perfect and gate nothing if `publish` does not
# wait for it, which is one word to delete and invisible in a diff that
# also touches the job.
python3 - "$WORKFLOW" "$JOB" "$SCRIPT" "$REPO_ROOT/.github/workflows/ci.yml" <<'PY'
import re
import sys

workflow, job, script, ci = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
lines = open(workflow, encoding="utf-8").read().splitlines()

jobs = []
publish_needs = []
gate_permissions = []
id_token_jobs = []
current = None
in_jobs = False

for line in lines:
    if re.match(r"^jobs:\s*$", line):
        in_jobs = True
        continue
    if in_jobs and line and not line.startswith(" ") and not line.startswith("#"):
        in_jobs = False
        continue
    if not in_jobs:
        continue
    m = re.match(r"^  ([A-Za-z0-9_.-]+):\s*$", line)
    if m:
        current = m.group(1)
        jobs.append(current)
        continue
    if current is None:
        continue
    if current == "publish":
        m = re.match(r"^    needs:\s*\[(.*)\]\s*$", line)
        if m:
            publish_needs = [n.strip() for n in m.group(1).split(",") if n.strip()]
    if current == job and re.match(r"^      (contents|checks|packages|id-token):", line):
        gate_permissions.append(line.strip())
    if re.match(r"^\s+id-token:\s*write\s*$", line):
        id_token_jobs.append(current)

failures = []
checks = 0


def check(ok, ok_text, bad_text):
    global checks
    checks += 1
    if ok:
        print("    ok   " + ok_text)
    else:
        failures.append(bad_text)
        print("    FAIL " + bad_text, file=sys.stderr)


check(
    len(jobs) >= 4 and "decide" in jobs and "publish" in jobs,
    f"the parser found {len(jobs)} jobs in release.yml, so it is reading the file",
    f"the parser found only {len(jobs)} jobs in release.yml ({jobs}): it has stopped matching, and every comparison below would pass against nothing",
)
check(
    job in jobs,
    f"{job} is one of them",
    f"{job} is not in release.yml. Nothing in this repository then checks that a commit reaching `release` was ever gated, and the only thing standing in front of a signed image is a branch protection setting no file here can read",
)
check(
    job in publish_needs,
    f"publish waits for {job}",
    f"publish does not wait for {job} (it needs {publish_needs}), so the gate check runs beside the publish instead of in front of it and a refusal arrives after the image is already in the registry",
)
check(
    "id-token: write" not in gate_permissions and "packages: write" not in gate_permissions,
    f"{job} holds no id-token and no packages scope, so it cannot publish or sign anything",
    f"{job} carries {gate_permissions}. The job that decides whether a release may publish must not be able to publish one",
)
check(
    id_token_jobs == ["publish"],
    "publish is still the only job that holds id-token: write, so the signing identity is unchanged",
    f"id-token: write is held by {id_token_jobs}. GitHub builds the Fulcio certificate SAN from the workflow and ref of the run that asks for the token, so a second job minting one signs under an identity the documented `cosign verify` command does not pin, which is #510",
)

# The name of the check run is a literal in release.yml and the `name:` of
# a job in ci.yml, in two files, with nothing holding them together. That
# is exactly the shape of #510, where the signing identity and the trigger
# that produces it were separate strings and one of them moved: the
# symptom there was a genuinely signed image reported as unverifiable, and
# the symptom here is a release refused because the job it looks for was
# renamed, or worse, a rename that quietly leaves the search looking for
# nothing.
gate_name = ""
m = re.search(r'^\s*GATE = "([^"]+)"\s*$', open(script, encoding="utf-8").read(), re.MULTILINE)
if m:
    gate_name = m.group(1)

ci_job_names = re.findall(r'^    name:\s*(.+?)\s*$', open(ci, encoding="utf-8").read(), re.MULTILINE)

check(
    gate_name != "",
    f"the job looks for a check run named {gate_name!r}",
    "the extracted script declares no GATE name, so this test cannot tell which check run it hunts for and the comparison below would be against an empty string",
)
check(
    len(ci_job_names) >= 10,
    f"ci.yml declares {len(ci_job_names)} job names, so the parser is reading it",
    f"only {len(ci_job_names)} job names came out of ci.yml, which is fewer than it has ever had: the parser has stopped matching and the comparison below would pass against nothing",
)
check(
    gate_name in ci_job_names,
    f"and ci.yml has a job called {gate_name!r} to produce it",
    f"release.yml looks for a check run named {gate_name!r} and no job in ci.yml is called that (the names there are {ci_job_names}). "
    "GitHub names a check run after the job's `name:`, so the search finds nothing on every commit, and a search that finds nothing refuses every release. Rename one and rename the other",
)

print()
if failures:
    print(f"==> release publish gate wiring: FAILED ({len(failures)} of {checks} checks)", file=sys.stderr)
    sys.exit(1)
print(f"==> release publish gate wiring: ok ({checks} checks)")
PY
wiring=$?
if [ "$wiring" -ne 0 ]; then
  failures=$((failures + 1))
fi

echo
if [ "$failures" -gt 0 ]; then
  echo "==> release publish gate: FAILED (${failures} failing assertion(s) of ${checks})" >&2
  exit 1
fi
echo "==> release publish gate: ok (${checks} behaviour checks, plus the wiring above)"

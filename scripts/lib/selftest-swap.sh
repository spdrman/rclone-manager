#!/usr/bin/env bash
# Shared anchored-mutation helpers for scripts/compat/selftest.sh and
# scripts/conformance/selftest.sh. Source it; do not run it.
#
# Both selftests prove a gate can go red by planting a real violation in a
# copy of the working tree. Every plant is anchored to a verbatim copy of
# product source, tabs and all, sitting in a script the author of the
# product change never opens. Refactor the anchored code and the anchor
# drifts.
#
# An anchor that no longer matches has to refuse to plant. A mutation that
# silently planted nothing reads as a clean pass, which is the exact failure
# these files exist to rule out, so the refusal is not the bug. The bug was
# that the refusal killed the run: `set -e` plus a python `sys.exit` took
# the whole script down on the first stale anchor, so every control after it
# never executed and drift surfaced one control per 25-minute gate run.
# Issue #458 has the three times that happened this campaign, and the second
# fix hid the third.
#
# So a stale anchor is a third verdict now. swap records the complaint and
# returns; the verdict function for that control turns it into STALE ANCHOR,
# counts it, and moves on; the summary names every stale control found in
# the one run.
#
# And because the anchors are invisible to the person whose refactor breaks
# them, swap_dry checks an anchor without planting anything. That is what
# --check-anchors runs: every anchor in both selftests against the real
# tree, building nothing, in about a second, so drift fails at the top of
# the gate instead of at minute 25.

# selftest_dry_run is 1 when the caller only wants anchors checked. The
# selftests set it from --check-anchors via selftest_parse_args.
: "${selftest_dry_run:=0}"

# Anchors seen, anchors that did not match, and the controls they belong to.
selftest_anchors_checked=0
selftest_anchors_stale=0
selftest_stale_count=0
selftest_stale_controls=""

# Complaints raised since the last verdict. Every swap for a control runs
# before that control's verdict, so a plain accumulator is enough.
selftest_stale_pending=""

# selftest_parse_args "$@" understands the one flag both selftests take.
selftest_parse_args() {
  local arg
  for arg in "$@"; do
    case "$arg" in
      --check-anchors) selftest_dry_run=1 ;;
      -h|--help)
        echo "usage: $0 [--check-anchors]"
        echo
        echo "  --check-anchors  dry-run every mutation anchor against the real"
        echo "                   tree, build nothing, and report the stale ones."
        exit 0
        ;;
      *)
        echo "$0: unknown argument: $arg" >&2
        echo "usage: $0 [--check-anchors]" >&2
        exit 2
        ;;
    esac
  done
}

# selftest_note_stale <text> files one complaint against the control being
# set up right now.
selftest_note_stale() {
  selftest_anchors_stale=$((selftest_anchors_stale + 1))
  selftest_stale_pending="${selftest_stale_pending}$1
"
}

# _selftest_anchor <mode> <file> <old> [new]
#
# mode is "plant" or "dry". Both insist the anchor is present exactly once:
# absent means the code it names was refactored, and twice means the plant
# would land in two places, so the mutant would no longer be the one
# violation the control describes. Either way the control cannot run, and
# either way the answer is to fix the anchor, not to guess.
_selftest_anchor() {
  local mode=$1 file=$2 old=$3 new=${4:-} out status=0
  selftest_anchors_checked=$((selftest_anchors_checked + 1))
  out=$(python3 - "$mode" "$file" "$old" "$new" <<'PY'
import os
import sys

mode, path, old, new = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

try:
    src = open(path).read()
except OSError as err:
    sys.exit("%s: %s" % (path, err))

n = src.count(old)
if n == 1:
    if mode == "plant":
        open(path, "w").write(src.replace(old, new, 1))
    sys.exit(0)


def quoted(text):
    return "\n".join("      | " + line for line in text.split("\n"))


head = os.path.relpath(path) if os.path.isabs(path) else path
if n > 1:
    sys.exit(
        "%s contains this anchor %d times, so the plant would land in more "
        "than one place:\n%s" % (head, n, quoted(old))
    )

# Where it stopped matching. Reporting the first anchor line that is no
# longer there turns "somebody refactored something" into a line number the
# person holding the diff recognises.
lines = old.split("\n")
kept = 0
while kept < len(lines) and "\n".join(lines[: kept + 1]) in src:
    kept += 1

detail = "%s no longer contains this anchor:\n%s" % (head, quoted(old))
if kept == 0:
    detail += "\n      not even its first line survives, so the whole block moved or went away."
else:
    detail += "\n      its first %d line(s) are still there; it stops matching at:\n%s" % (
        kept,
        quoted(lines[kept]),
    )
sys.exit(detail)
PY
  ) || status=$?
  if [ "$status" -ne 0 ]; then
    selftest_note_stale "$out"
  fi
}

# swap <file> <old> <new> replaces one exact string in a mutant tree.
#
# Under --check-anchors it only looks, so the same call site is both the
# mutation and its own drift check and the two cannot disagree.
swap() {
  if [ "$selftest_dry_run" = 1 ]; then
    _selftest_anchor dry "$1" "$2"
  else
    _selftest_anchor plant "$1" "$2" "$3"
  fi
}

# swap_dry <file> <old> asks whether an anchor is still there, and never
# writes.
swap_dry() {
  _selftest_anchor dry "$1" "$2"
}

# mutate_py <file> runs the python mutation on its stdin against <file>,
# for the handful of mutants that reshape json or run a regex rather than
# swapping one literal. Their asserts stay theirs; this only stops a failing
# one from taking the rest of the run with it, and skips them under
# --check-anchors, which builds nothing and must not write to the real tree.
mutate_py() {
  local file=$1 out status=0
  if [ "$selftest_dry_run" = 1 ]; then
    cat >/dev/null
    return 0
  fi
  out=$(python3 - "$file" 2>&1) || status=$?
  if [ "$status" -ne 0 ]; then
    selftest_note_stale "$file: the mutation refused to plant:
$(printf '%s\n' "$out" | sed 's/^/      | /')"
  fi
}

# selftest_stale_verdict <label> is the third verdict. It returns 0 when
# something refused to plant for this control, which means the caller must
# NOT run its gate: a tree with no violation in it would pass, and reading
# that as a pass is the whole thing being guarded against.
selftest_stale_verdict() {
  local label=$1
  if [ -z "$selftest_stale_pending" ]; then
    return 1
  fi
  echo "STALE ANCHOR: $label. Nothing was planted, so this control did not run." >&2
  printf '%s' "$selftest_stale_pending" | sed 's/^/    /' >&2
  selftest_stale_count=$((selftest_stale_count + 1))
  selftest_stale_controls="${selftest_stale_controls}  - ${label}
"
  selftest_stale_pending=""
  return 0
}

# selftest_anchors_only <label> is true when this run is only checking
# anchors, so the caller returns before building anything.
selftest_anchors_only() {
  if [ "$selftest_dry_run" = 1 ]; then
    echo "  ok (anchors): $1"
    return 0
  fi
  return 1
}

# selftest_stale_summary prints every stale control from this one run, which
# is the point: one run, the whole list, not one per gate.
selftest_stale_summary() {
  if [ "$selftest_stale_count" -eq 0 ]; then
    return 0
  fi
  echo >&2
  echo "STALE ANCHORS: $selftest_stale_count control(s) could not plant their violation, out of $selftest_anchors_checked anchor(s) checked." >&2
  printf '%s' "$selftest_stale_controls" >&2
  echo "Each one names a verbatim copy of product source that has since been refactored." >&2
  echo "Re-anchor it to the code as it is now. Run 'bash scripts/selftest/check-anchors.sh' to check them all in seconds." >&2
}

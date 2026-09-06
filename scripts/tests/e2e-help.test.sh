#!/usr/bin/env bash
# The two end-to-end drivers' --help is operator-visible text, and this pins
# it (issue #514).
#
# What it is guarding against. Both scripts used to render their help by
# reading their own header BY LINE NUMBER:
#
#   sed -n '2,110p' "$0"     two-machine-backup.sh
#   sed -n '2,84p'  "$0"     run-machine-tier.sh
#
# so the help an operator read was a set of coordinates rather than a piece
# of text. Inserting a comment above the boundary rewrote it and deleting one
# truncated it, silently, and by the time #514 was written both had already
# drifted: two-machine-backup.sh ended on a bare section heading with the
# section missing, and run-machine-tier.sh ended mid-sentence, on "so about
# 76s of". Nothing anywhere rendered either script's help, so nothing could
# have said so.
#
# FR-35 clause 4 is the rule this is an instance of: nothing may reword a
# line an operator already reads. core/tests/compat enforces that for the
# CLI, byte for byte, and these two surfaces had none of it. So the rendered
# text is now pinned against a golden the same way, and a reword fails here
# until somebody updates the golden on purpose.
#
# Four things get asserted, because pinning the text alone would not have
# caught the defect that produced #514:
#
#   A  the rendered help is byte for byte the golden, from a foreign working
#      directory, for both --help and -h.
#   B  neither script addresses its help by line number any more, and both
#      carry exactly one HELP-START and one HELP-END marker.
#   C  a comment inserted into the header ABOVE the block leaves the rendered
#      help unchanged. That is the property that did not hold before, and it
#      is the only one here that is about the shape rather than the content.
#   D  the controls. C is also true of a mutation that never landed and of a
#      renderer that prints nothing at all, so: the same insertion applied to
#      a script that renders by line number MUST change its help, a reword
#      inside the block MUST be seen, and a block with its markers removed
#      MUST refuse out loud rather than print an empty help and exit 0.
#
# Run directly (`bash scripts/tests/e2e-help.test.sh`); it costs about a
# second and touches nothing but its own temporary directory. scripts/ci-local.sh
# runs it with the other static checks, in FAST runs too, for the same reason.
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/../.." && pwd)"
GOLDEN_DIR="$SCRIPTS_DIR/testdata"

# The two drivers, and the golden each one's help is pinned against.
SUBJECTS="two-machine-backup run-machine-tier"

checks=0
failures=0
tmpdirs=()

cleanup() {
  for d in ${tmpdirs+"${tmpdirs[@]}"}; do
    [ -n "$d" ] && rm -rf "$d"
  done
}
trap cleanup EXIT

pass() { checks=$((checks + 1)); printf '    ok   %s\n' "$1"; }
fail() {
  checks=$((checks + 1))
  failures=$((failures + 1))
  printf '    FAIL %s\n' "$1" >&2
  if [ $# -gt 1 ]; then
    printf '%s\n' "$2" | sed 's/^/         | /' >&2
  fi
}

# render prints a script's help. Deliberately from `/` rather than from the
# repository root: the drivers cd to the repository root before they parse
# their arguments, so a $0 the shell left relative stops resolving there, and
# that is not hypothetical either. Before #514 this exact call printed an
# empty help and still exited 0.
render() { # <script path> [flag]
  (cd / && bash "$1" "${2:---help}" 2>&1)
}

# sandbox_copy prints the path of a throwaway checkout holding one driver at
# the path it has here, so the copy's own `dirname "$0"/../..` still lands on
# a directory it can cd into.
sandbox_copy() { # <subject>
  local dir
  dir="$(mktemp -d)"
  tmpdirs+=("$dir")
  mkdir -p "$dir/scripts/e2e"
  cp "$REPO_ROOT/scripts/e2e/$1.sh" "$dir/scripts/e2e/$1.sh"
  printf '%s\n' "$dir/scripts/e2e/$1.sh"
}

echo "==> e2e driver --help (#514)"

# ------------------------------------ A: the rendered text is what it was

for subject in $SUBJECTS; do
  script="$REPO_ROOT/scripts/e2e/$subject.sh"
  golden="$GOLDEN_DIR/$subject.help.txt"

  if [ ! -f "$script" ]; then
    fail "A $subject.sh is where this expects it"
    continue
  fi
  if [ ! -f "$golden" ]; then
    fail "A $subject's help is pinned" "no golden at $golden"
    continue
  fi

  actual="$(render "$script")"
  status=$?

  if [ "$status" -eq 0 ]; then
    pass "A $subject --help exits 0"
  else
    fail "A $subject --help exits 0, got $status" "$actual"
  fi

  # A golden somebody emptied would otherwise make the comparison below
  # pass against a renderer that prints nothing, which is one of the two
  # failure modes this whole file exists for.
  if [ "$(wc -l <"$golden")" -ge 20 ]; then
    pass "A $subject's golden is a real help text rather than an empty file"
  else
    fail "A $subject's golden is a real help text rather than an empty file" \
      "$golden has $(wc -l <"$golden") lines"
  fi

  # Both sides go through a command substitution, so a trailing blank line
  # is stripped from each rather than from only one of them: diffing the
  # golden FILE against the rendered STRING reports a phantom last-line
  # difference on every real failure, which is noise on top of the one line
  # somebody actually needs to read.
  expected="$(cat "$golden")"
  if [ "$actual" = "$expected" ]; then
    pass "A $subject --help is byte for byte its golden"
  else
    fail "A $subject --help is byte for byte its golden" \
      "$(diff <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") | head -40)
--
This is operator-visible text under FR-35 clause 4. If the reword is
deliberate, update the golden and say so in the commit:

  bash scripts/e2e/$subject.sh --help > scripts/tests/testdata/$subject.help.txt

If it is not deliberate, the help block in scripts/e2e/$subject.sh has moved
underneath somebody, which is the whole of #514."
  fi

  short="$(render "$script" -h)"
  if [ "$short" = "$actual" ]; then
    pass "A $subject -h renders the same help as --help"
  else
    fail "A $subject -h renders the same help as --help" \
      "$(diff <(printf '%s\n' "$actual") <(printf '%s\n' "$short") | head -20)"
  fi
done

# ---------------------------------- B: nothing addresses help by line number

for subject in $SUBJECTS; do
  script="$REPO_ROOT/scripts/e2e/$subject.sh"
  [ -f "$script" ] || continue
  source_text="$(cat "$script")"

  # The exact shape #514 is about: a range of two line numbers over the
  # script's own file. Anchored on the digits rather than on the whole old
  # command, so the same idea written with different numbers, a different
  # variable for the file, or awk instead of sed is still caught.
  #
  # Comment lines are dropped from the result, because both scripts quote the
  # command they used to use in the prose explaining why they no longer do,
  # and a check that cannot tell a command from its own explanation is not a
  # check. The filter runs on the match rather than on the file so the line
  # numbers it reports are still this file's own.
  by_number="$(grep -nE "(sed|awk|head|tail)[^|]*['\"]?[0-9]+,[0-9]+p" "$script" \
    | grep -vE '^[0-9]+:[[:space:]]*#' || true)"
  if [ -z "$by_number" ]; then
    pass "B $subject renders no part of itself by line number"
  else
    fail "B $subject renders no part of itself by line number" "$by_number"
  fi

  for marker in "# HELP-START" "# HELP-END"; do
    count="$(grep -cxF "$marker" "$script" || true)"
    if [ "$count" = "1" ]; then
      pass "B $subject carries exactly one $marker"
    else
      fail "B $subject carries exactly one $marker, found $count"
    fi
  done

  case "$source_text" in
    *render_help*) pass "B $subject renders its help through render_help" ;;
    *) fail "B $subject renders its help through render_help" ;;
  esac
done

# --------------- C: an edit above the block does not rewrite the help

INSERTED='# An unrelated implementation note, added later, above the help block.'

for subject in $SUBJECTS; do
  script="$REPO_ROOT/scripts/e2e/$subject.sh"
  [ -f "$script" ] || continue
  before="$(render "$script")"

  copy="$(sandbox_copy "$subject")"
  # After the shebang, so it lands above the block rather than inside it.
  # This is the edit that used to silently truncate the help by a line.
  awk -v note="$INSERTED" 'NR == 1 { print; print note; next } { print }' \
    "$copy" >"$copy.new" && mv "$copy.new" "$copy"

  after="$(render "$copy")"
  if [ "$after" = "$before" ]; then
    pass "C $subject: a comment added above the help block does not change --help"
  else
    fail "C $subject: a comment added above the help block does not change --help" \
      "$(diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") | head -20)"
  fi
done

# ------------------------------------------------- D: the controls for C

# D1. C is also true of an insertion that never landed, and of a harness that
# renders the wrong file. The same edit, applied by the same code, to a script
# that renders its help the way both drivers used to, has to CHANGE its help.
# Without this, C would pass against a mutation that did nothing at all.
control_dir="$(mktemp -d)"
tmpdirs+=("$control_dir")
mkdir -p "$control_dir/scripts/e2e"
control="$control_dir/scripts/e2e/by-line-number.sh"
cat >"$control" <<'CONTROL'
#!/usr/bin/env bash
# first help line
# second help line
# third help line
set -euo pipefail
sed -n '2,4p' "$0" | sed 's/^# \{0,1\}//'
CONTROL
control_before="$(cd / && bash "$control" 2>&1)"
awk -v note="$INSERTED" 'NR == 1 { print; print note; next } { print }' \
  "$control" >"$control.new" && mv "$control.new" "$control"
control_after="$(cd / && bash "$control" 2>&1)"
if [ "$control_before" != "$control_after" ]; then
  pass "D1 the same insertion DOES change a help rendered by line number, so C has teeth"
else
  fail "D1 the same insertion DOES change a help rendered by line number, so C has teeth" \
    "the control script's help was [$control_before] before and after, so C is measuring nothing"
fi

# D2. The other half. C and A would both survive a render_help that printed a
# constant, so a reword INSIDE the block has to be seen. This is the assertion
# the issue asked to be proven red by hand, kept here so it stays proven.
for subject in $SUBJECTS; do
  script="$REPO_ROOT/scripts/e2e/$subject.sh"
  [ -f "$script" ] || continue
  before="$(render "$script")"

  copy="$(sandbox_copy "$subject")"
  awk '
    /^# HELP-START$/ { print; getline line; print line " REWORDED"; next }
    { print }
  ' "$copy" >"$copy.new" && mv "$copy.new" "$copy"

  after="$(render "$copy")"
  case "$after" in
    *REWORDED*) reworded=1 ;;
    *) reworded=0 ;;
  esac
  if [ "$after" != "$before" ] && [ "$reworded" = "1" ]; then
    pass "D2 $subject: a word changed inside the block does change --help"
  else
    fail "D2 $subject: a word changed inside the block does change --help" \
      "the mutated copy rendered help that is neither different nor carries the reword, so A is pinning something that cannot move"
  fi
done

# D3. And the loud-refusal half. A renderer that answers an absent block with
# an empty help and exit 0 is #160's silent skip wearing a different hat: the
# gate, the operator and this suite would all read it as a help that happens
# to be short. Both drivers refuse instead.
for subject in $SUBJECTS; do
  script="$REPO_ROOT/scripts/e2e/$subject.sh"
  [ -f "$script" ] || continue

  copy="$(sandbox_copy "$subject")"
  grep -vxF -e '# HELP-START' -e '# HELP-END' "$copy" >"$copy.new" \
    && mv "$copy.new" "$copy"

  out="$(render "$copy")"
  status=$?
  if [ "$status" -ne 0 ]; then
    pass "D3 $subject refuses to render a help block whose markers are gone"
  else
    fail "D3 $subject refuses to render a help block whose markers are gone" \
      "it exited 0 and printed ${#out} bytes"
  fi
  case "$out" in
    *"help block is missing"*)
      pass "D3 $subject says what is missing rather than printing nothing" ;;
    *)
      fail "D3 $subject says what is missing rather than printing nothing" "$out" ;;
  esac
done

# ------------------------------------------------------------------ result

echo
if [ "$failures" -eq 0 ]; then
  echo "==> e2e driver --help: ok ($checks checks)"
  exit 0
fi
echo "==> e2e driver --help: $failures of $checks checks FAILED" >&2
exit 1

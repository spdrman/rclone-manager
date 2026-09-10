#!/usr/bin/env bash
# The end-to-end drivers' --help is operator-visible text, and this pins it
# (issue #514).
#
# What it is guarding against. The two that existed when #514 was written
# used to render their help by reading their own header BY LINE NUMBER:
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
#
# PORTED-CHECK HAZARD NOTE
#
# EPIC I / I1.6 (#672) moved both drivers to scripts/rcmtools/e2e/*.py behind
# exec shims at their old scripts/e2e/*.sh paths. A port can silently convert
# a check into one that cannot fail, so each assertion answers for itself.
#
# A  the rendered help is byte for byte the golden
#   hazard in bash:   somebody rewords operator-visible text without deciding
#                     to (FR-35 clause 4).
#   hazard in python: STILL EXISTS, and grew a second half. The block moved
#                     into a `#` comment block in the .py file rather than
#                     into the module docstring, precisely so render_help
#                     keeps ONE rule (strip a leading `# `) instead of
#                     guessing. A docstring would have forced six heading
#                     lines in each golden to change for no operator-visible
#                     reason, and a golden diff nobody can read is a golden
#                     nobody checks.
#   held by:          the byte comparison, plus the new shim-parity check:
#                     the old path must render the same help, because that is
#                     the path ci-local.sh, ci.yml and operators still name.
#
# B  no help is addressed by line number; the markers are unique
#   hazard in bash:   `sed -n '2,110p' "$0"` -- help as coordinates, silently
#                     rewritten by any edit above the boundary. This is #514.
#   hazard in python: STILL EXISTS. The grep runs on the help-owning file
#                     whatever language it is in, and a Python renderer
#                     slicing __doc__ by index would be the same defect.
#   held by:          the same grep, repointed at subject_file.
#
# C  a comment inserted above the block does not change the help
#   hazard in bash:   the boundary moves and the help silently truncates.
#   hazard in python: STILL EXISTS. A comment between a Python shebang and
#                     the docstring is legal and leaves the docstring first,
#                     so the identical one-line mutation is still the honest
#                     one.
#   held by:          C, whose teeth are D1.
#
# D3 an absent block is refused out loud, not answered with an empty help
#   hazard in bash:   awk prints nothing and exits 0; the gate, the operator
#                     and this suite all read it as a short help.
#   hazard in python: STILL EXISTS AND GREW A NEW WAY TO GO VACUOUS. D3 asks
#                     only for a non-zero exit, and a ported driver imports
#                     rcmtools before it parses argv -- so a sandbox missing
#                     the package exits non-zero on an ImportError and D3
#                     PASSES having measured nothing. This is why render_help
#                     reads the FILE rather than __doc__ (a docstring reader
#                     cannot tell deleted markers from an absent docstring),
#                     why sandbox_copy copies the package, and why check S
#                     below exists at all.
#   held by:          S (the unmutated sandbox must render, identically),
#                     plus D3's second assertion that the refusal SAYS
#                     "help block is missing" rather than merely failing.
#
# S  is new, and has no bash counterpart: the port created the hazard it
#    closes. Noted rather than left as an unexplained extra check.
#
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/../.." && pwd)"
GOLDEN_DIR="$SCRIPTS_DIR/testdata"

# The drivers, and the golden each one's help is pinned against.
#
# A subject is a NAME, not a path, because EPIC I / I1.6 moved the drivers to
# scripts/rcmtools/e2e/*.py and left an exec shim at each old scripts/e2e/*.sh
# path (ci-local.sh, ci.yml and scripts/tests/ci-local-gate.test.sh all still
# name those). The help lives with the driver, not with the shim, so this
# suite follows the driver: subject_file says where the help-owning file is
# and subject_interp says what runs it. Both are python3 today; the table
# stays a table so a subject that is still bash does not need this file
# restructured to keep being checked. three-machine-web-ui (#687) is that
# subject: unported, so its help-owning file and its bash entry point are
# the same file.
SUBJECTS="two-machine-backup run-machine-tier three-machine-web-ui"

subject_file() { # <subject> -> path, relative to the repository root
  case "$1" in
    two-machine-backup)   printf '%s\n' "scripts/rcmtools/e2e/two_machine_backup.py" ;;
    run-machine-tier)     printf '%s\n' "scripts/rcmtools/e2e/run_machine_tier.py" ;;
    three-machine-web-ui) printf '%s\n' "scripts/e2e/three-machine-web-ui.sh" ;;
    *) return 1 ;;
  esac
}

subject_interp() { # <subject> -> the interpreter its help-owning file needs
  case "$1" in
    two-machine-backup|run-machine-tier) printf '%s\n' "python3" ;;
    three-machine-web-ui)                printf '%s\n' "bash" ;;
    *) return 1 ;;
  esac
}

# subject_shim prints the old scripts/e2e path that must still be a runnable
# entry point, or nothing where there is no longer any reason for one.
#
# Not every ported driver keeps its old path, and which ones do is a fact
# about who NAMES the path rather than a matter of consistency.
# two-machine-backup.sh is still exec'd by scripts/ci-local.sh, reached by
# .github/workflows/ci.yml through two-machine-ci.sh, driven by
# scripts/tests/two-machine-exit-status.test.sh, and FABRICATED at that
# literal path by scripts/tests/ci-local-gate.test.sh. run-machine-tier.sh
# was named by nothing that runs it once its callers were repointed, so it
# was deleted rather than left as a file whose only purpose is to be found.
# three-machine-web-ui is neither: it was never ported, so subject_file
# above already points straight at its one and only entry point, and its
# "shim" is that same file. The branch this feeds still exercises
# something real for it -- A's "renders identically through $shim" check
# runs the identical file twice, through the same interpreter, which is a
# weaker check than a real shim's but not a vacuous one: a driver that
# reads argv from anything other than its own invocation (a stray cwd
# assumption, say) would still fail it.
subject_shim() { # <subject> -> path relative to the repository root, or ""
  case "$1" in
    two-machine-backup)   printf '%s\n' "scripts/e2e/two-machine-backup.sh" ;;
    three-machine-web-ui) printf '%s\n' "scripts/e2e/three-machine-web-ui.sh" ;;
    *) printf '%s\n' "" ;;
  esac
}

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
# empty help and still exited 0. The ported drivers resolve their own root
# from __file__ for the same reason, and this is what proves it.
render() { # <interpreter> <script path> [flag]
  (cd / && "$1" "$2" "${3:---help}" 2>&1)
}

# sandbox_copy prints the path of a throwaway checkout holding one driver at
# the path it has here, so the copy's own root resolution still lands on a
# directory it can cd into.
#
# It copies the rcmtools PACKAGE too, and that is not convenience. A ported
# driver does `sys.path.insert(...); from rcmtools import harness` before it
# looks at argv, so a sandbox holding only the driver file fails on an
# ImportError before render_help is ever called. C and D2 would then fail for
# a reason that is not what they measure, and D3 -- which only asks for a
# non-zero exit -- would PASS on the ImportError, pinning nothing at all.
# That is the same vacuous-green hazard I1.6 exists to keep watch for, so the
# sandbox is proven to render before anything mutates it (check S below).
sandbox_copy() { # <subject>
  local dir file
  dir="$(mktemp -d)"
  tmpdirs+=("$dir")
  file="$(subject_file "$1")"
  mkdir -p "$dir/$(dirname "$file")"
  # Only a ported (python3) subject needs the package staged: an unported
  # bash subject such as three-machine-web-ui (#687) is a complete program
  # on its own, and staging a package it never imports would be copying
  # for its own sake -- and, for a subject whose file does not live under
  # scripts/rcmtools/e2e, a `cp` into a directory the mkdir above never
  # had reason to create.
  if [ "$(subject_interp "$1")" = "python3" ]; then
    mkdir -p "$dir/scripts/rcmtools/e2e"
    cp "$REPO_ROOT/scripts/rcmtools/__init__.py" "$dir/scripts/rcmtools/__init__.py"
    cp "$REPO_ROOT/scripts/rcmtools/harness.py" "$dir/scripts/rcmtools/harness.py"
    cp "$REPO_ROOT/scripts/rcmtools/e2e/__init__.py" "$dir/scripts/rcmtools/e2e/__init__.py"
  fi
  cp "$REPO_ROOT/$file" "$dir/$file"
  printf '%s\n' "$dir/$file"
}

echo "==> e2e driver --help (#514)"

# ------------------------------------ A: the rendered text is what it was

for subject in $SUBJECTS; do
  script="$REPO_ROOT/$(subject_file "$subject")"
  interp="$(subject_interp "$subject")"
  shim="$(subject_shim "$subject")"
  golden="$GOLDEN_DIR/$subject.help.txt"

  if [ ! -f "$script" ]; then
    fail "A $subject's help-owning file is where this expects it" "no file at $script"
    continue
  fi
  if [ ! -f "$golden" ]; then
    fail "A $subject's help is pinned" "no golden at $golden"
    continue
  fi

  actual="$(render "$interp" "$script")"
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

If it is not deliberate, the help block in $(subject_file "$subject") has moved
underneath somebody, which is the whole of #514."
  fi

  short="$(render "$interp" "$script" -h)"
  if [ "$short" = "$actual" ]; then
    pass "A $subject -h renders the same help as --help"
  else
    fail "A $subject -h renders the same help as --help" \
      "$(diff <(printf '%s\n' "$actual") <(printf '%s\n' "$short") | head -20)"
  fi

  # The old path is what ci-local.sh, ci.yml and an operator's muscle memory
  # all still name, and I1.6 kept a real exec shim there rather than moving
  # it. A shim that forwarded arguments but not the help would be a silent
  # regression for every one of those callers, so the two are compared
  # rather than assumed equal.
  #
  # The no-shim branch has to ASK. It used to print "and has none to drift"
  # from the case statement above and nothing else -- a claim about the
  # filesystem that never looked at it, so no state of the tree could have
  # failed it. That is the defect this file's own header exists to police
  # (#662), and a standard that does not apply to itself is not a standard.
  # Restoring scripts/e2e/run-machine-tier.sh -- the stale 449-line driver
  # that README, docs/architecture/test-tiers.md and manager-machine.Dockerfile
  # were all repointed away from -- used to keep this green.
  if [ -z "$shim" ]; then
    stale="scripts/e2e/$subject.sh"
    if [ ! -e "$REPO_ROOT/$stale" ]; then
      pass "A $subject needs no scripts/e2e entry point, and has none at $stale to drift"
    else
      fail "A $subject needs no scripts/e2e entry point, and has none to drift" \
        "$stale exists. subject_shim names no shim for $subject, so nothing runs that file and nothing
compares its help against the driver's: it can drift arbitrarily far from
$(subject_file "$subject") and this suite would never say so. Either delete it, or give $subject a
shim entry in subject_shim so the branch above compares the two."
    fi
  elif [ -f "$REPO_ROOT/$shim" ]; then
    through_shim="$(render bash "$REPO_ROOT/$shim")"
    if [ "$through_shim" = "$actual" ]; then
      pass "A $subject --help renders identically through $shim"
    else
      fail "A $subject --help renders identically through $shim" \
        "$(diff <(printf '%s\n' "$actual") <(printf '%s\n' "$through_shim") | head -20)"
    fi
  else
    fail "A $subject still has an entry point at $shim" \
      "scripts/ci-local.sh execs that literal path and scripts/tests/ci-local-gate.test.sh fabricates a stand-in at it"
  fi
done

# ---------------------------------- B: nothing addresses help by line number

for subject in $SUBJECTS; do
  script="$REPO_ROOT/$(subject_file "$subject")"
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

  # The renderer is rcmtools.harness.render_help now, one copy for every
  # domain rather than one per script, so the name is what is checked and
  # not where it is defined.
  case "$source_text" in
    *render_help*) pass "B $subject renders its help through render_help" ;;
    *) fail "B $subject renders its help through render_help" ;;
  esac
done

# ------------- S: the sandbox itself renders, before anything mutates it
#
# C, D2 and D3 all measure a MUTATED sandbox copy against the real thing, so
# every one of them is only as good as the sandbox. A ported driver imports
# rcmtools before it looks at argv, and a sandbox missing the package fails
# on an ImportError: C and D2 would then fail for a reason that is not what
# they measure, and D3 -- which asks only for a non-zero exit -- would PASS
# on that ImportError while pinning nothing at all.
#
# So the unmutated copy is required to render, and to render EXACTLY what the
# original does. This check has no counterpart in the bash this file used to
# drive, where a one-file copy was a complete program; it is here because the
# port created the hazard.
for subject in $SUBJECTS; do
  script="$REPO_ROOT/$(subject_file "$subject")"
  interp="$(subject_interp "$subject")"
  [ -f "$script" ] || continue

  copy="$(sandbox_copy "$subject")"
  sandbox_help="$(render "$interp" "$copy")"
  sandbox_status=$?
  if [ "$sandbox_status" -eq 0 ] && [ "$sandbox_help" = "$(render "$interp" "$script")" ]; then
    pass "S $subject: an unmutated sandbox copy renders the same help, so C/D2/D3 measure what they claim"
  else
    fail "S $subject: an unmutated sandbox copy renders the same help, so C/D2/D3 measure what they claim" \
      "exit $sandbox_status from $copy
$sandbox_help"
  fi
done

# --------------- C: an edit above the block does not rewrite the help

INSERTED='# An unrelated implementation note, added later, above the help block.'

for subject in $SUBJECTS; do
  script="$REPO_ROOT/$(subject_file "$subject")"
  interp="$(subject_interp "$subject")"
  [ -f "$script" ] || continue
  before="$(render "$interp" "$script")"

  copy="$(sandbox_copy "$subject")"
  # After the shebang, so it lands above the block rather than inside it.
  # This is the edit that used to silently truncate the help by a line. A
  # comment between a Python shebang and the module docstring is still legal
  # and still leaves the docstring first, so the same one-line insertion is
  # the honest mutation on both kinds of subject.
  awk -v note="$INSERTED" 'NR == 1 { print; print note; next } { print }' \
    "$copy" >"$copy.new" && mv "$copy.new" "$copy"

  after="$(render "$interp" "$copy")"
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
  script="$REPO_ROOT/$(subject_file "$subject")"
  interp="$(subject_interp "$subject")"
  [ -f "$script" ] || continue
  before="$(render "$interp" "$script")"

  copy="$(sandbox_copy "$subject")"
  awk '
    /^# HELP-START$/ { print; getline line; print line " REWORDED"; next }
    { print }
  ' "$copy" >"$copy.new" && mv "$copy.new" "$copy"

  after="$(render "$interp" "$copy")"
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
  script="$REPO_ROOT/$(subject_file "$subject")"
  interp="$(subject_interp "$subject")"
  [ -f "$script" ] || continue

  copy="$(sandbox_copy "$subject")"
  grep -vxF -e '# HELP-START' -e '# HELP-END' "$copy" >"$copy.new" \
    && mv "$copy.new" "$copy"

  out="$(render "$interp" "$copy")"
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

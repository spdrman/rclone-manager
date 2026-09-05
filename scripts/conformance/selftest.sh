#!/usr/bin/env bash
# Positive controls for EPIC E's composed conformance scenario (issue #242).
#
# core/tests/conformance is a wall of assertions about a chain that mostly
# works: the artifact ends up on the medium the chain named, the invariant
# held at every instant, the crash cells converged, the bytes hash right.
# An assertion nobody has watched fail is indistinguishable from one that
# cannot fail, and this repository has now found more than fifteen checks
# that passed for the wrong reason.
#
# So every claim that suite makes is mutation-tested here against the real
# tree: a copy of the working tree gets one deliberate violation planted in
# a real product file, the suite runs, and it must fail AND name the check
# whose promise the violation broke. Naming the check, not merely failing,
# is what stops a mutation that broke the build for an unrelated reason
# from reading as a pass. scripts/compat/selftest.sh is the same discipline
# for the FR-35 half, and this is deliberately its sibling.
#
# Two of the violations below are named by the conformance matrix itself:
#
#   P2.1: "an engine that lets both placements be non-verified at the same
#          instant. The harness has to observe it at that instant, which is
#          why 'continuously' is in the gate line and why sampling would
#          not do."
#
#   P2.2 / V1: "a mutation that issues the source delete before VERIFIED is
#          durably recorded."
#
# They are the same planted edit here, because in this engine they are the
# same mistake seen from two sides.
#
# This is not fast. Each mutant rebuilds core/ and runs a suite that stands
# up MinIO containers, so budget ten minutes. It is the only thing standing
# between "the composed scenario is green" and "the composed scenario is
# green because it cannot go red".
#
# Every mutation below is anchored to a verbatim copy of product source,
# tabs and all, which means a refactor over there can leave an anchor here
# naming code that is no longer in the tree. #437 did exactly that to the
# crash-matrix anchor. That is the third verdict, STALE ANCHOR: the control
# is skipped, because a tree with nothing planted in it would pass and
# reading that as a pass is the exact failure this file exists to rule out,
# and the run carries on so one run names every stale control instead of
# dying on the first (#458).
#
# `bash scripts/conformance/selftest.sh --check-anchors` is that check on
# its own: every anchor against the real tree, building nothing, in about a
# second. scripts/selftest/check-anchors.sh runs it for both selftests.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# swap, the STALE ANCHOR verdict and --check-anchors all live in the shared
# library, because scripts/compat/selftest.sh needs exactly the same three
# things and the two drifted apart once already (#458).
. scripts/lib/selftest-swap.sh
selftest_parse_args "$@"

root=$(pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-conformance-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# mutant <name> copies the working tree into $tmp/<name> and echoes its
# path.
#
# --cached --others --exclude-standard, not plain `git ls-files`: the copy
# has to include files that are present but not yet committed, because the
# suite being tested is itself usually uncommitted while it is being
# written. Copying tracked files only is what produced a self-test
# elsewhere in this repository that silently "caught" every mutation,
# because the check it invoked did not exist in the copy at all.
mutant() {
  local name=$1
  local dir="$tmp/$name"
  # Under --check-anchors nothing is planted, so there is nothing to copy
  # into and no reason to spend a tar per control. Every swap reads the real
  # tree instead, which is the tree whose drift is being looked for.
  if [ "$selftest_dry_run" = 1 ]; then
    printf '%s' "$root"
    return 0
  fi
  mkdir -p "$dir"
  (cd "$root" && git ls-files -z --cached --others --exclude-standard | tar -cf - --null -T -) | (cd "$dir" && tar -xf -)
  printf '%s' "$dir"
}

# conformance_gate runs the composed suite in whichever tree it is called
# from. -run narrows it to the checks a mutation is expected to move, per
# call, so a ten-minute self-test does not become an hour one.
conformance_gate() {
  local pattern=${1:-.}
  (cd core && GOWORK=off go test -count=1 -timeout 30m -run "$pattern" ./tests/conformance/)
}

# unit_gate runs one non-composed package, for the two halves of a claim
# that do not live in tests/conformance. It is here rather than in a
# separate script because the matrix row it serves is one row: a claim
# checked in two places needs both falsifications in one place, or the
# second one quietly stops being run.
unit_gate() {
  local pkg=$1 pattern=${2:-.}
  (cd core && GOWORK=off go test -count=1 -run "$pattern" "$pkg")
}

# expect_unit_check_fails <label> <dir> <expected substring> <package> <run pattern>
expect_unit_check_fails() {
  local label=$1 dir=$2 needle=$3 pkg=$4 pattern=$5
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if (cd "$dir" && unit_gate "$pkg" "$pattern") >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $label. $pkg PASSED against a planted violation." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  elif ! grep -qF "$needle" "$tmp/out"; then
    echo "SELFTEST FAIL: $label. The package failed, but never named the promise that was broken." >&2
    echo "    expected its output to mention: $needle" >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  else
    echo "  ok (caught): $label"
    echo "      -> $(grep -m1 -F "$needle" "$tmp/out" | sed 's/^[[:space:]]*//' | cut -c1-400)"
    pass=$((pass + 1))
  fi
}

# expect_check_fails <label> <dir> <expected substring> [run pattern]
expect_check_fails() {
  local label=$1 dir=$2 needle=$3 pattern=${4:-.}
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if (cd "$dir" && conformance_gate "$pattern") >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $label. The composed suite PASSED against a planted violation." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  elif ! grep -qF "$needle" "$tmp/out"; then
    echo "SELFTEST FAIL: $label. The suite failed, but never named the promise that was broken." >&2
    echo "    expected its output to mention: $needle" >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  else
    echo "  ok (caught): $label"
    echo "      -> $(grep -m1 -F "$needle" "$tmp/out" | sed 's/^[[:space:]]*//' | cut -c1-400)"
    pass=$((pass + 1))
  fi
}

expect_gate_passes() {
  local label=$1 dir=$2
  if selftest_stale_verdict "$label"; then
    return 0
  fi
  if selftest_anchors_only "$label"; then
    return 0
  fi
  if (cd "$dir" && conformance_gate) >"$tmp/out" 2>&1; then
    echo "  ok (clean):  $label"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $label. The suite FAILED against an unmutated tree, so its failures mean nothing." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

# swap <file> <old> <new> replaces one exact string and REFUSES if the old
# text is not there, which is the whole point: a planted violation that
# silently planted nothing is the "green mutation" failure this file exists
# to rule out, and it has happened here before. It lives in
# scripts/lib/selftest-swap.sh now, along with what a refusal costs: the
# control is reported STALE ANCHOR and skipped, and the run carries on to
# the rest instead of dying here (#458).

echo "==> negative control: the composed suite is clean on the real tree"
expect_gate_passes "core/tests/conformance on an unmutated tree" "$root"

echo
echo "==> the matrix's own falsification for the phase 2 exit gate (P2.1, P2.2, V1)"

# The source delete issued before the destination has been verified. It is
# one edit in two places because the ordering is enforced twice: the phase
# table has no edge, and the driver has no case. Mutating only one of them
# produces a refused write rather than the bug, which would be a mutation
# that "passed" for the wrong reason.
d=$(mutant source-delete-before-verified)
swap "$d/core/internal/placement/phases.go" \
  '	{From: Copied, To: Verifying},' \
  '	{From: Copied, To: Verifying},
	// PLANTED VIOLATION (scripts/conformance/selftest.sh).
	{From: Copied, To: SourceDeletePending},'
swap "$d/core/internal/placement/engine.go" \
  '		case Copied:
			next, err = e.startVerify(ctx, mv)' \
  '		case Copied:
			// PLANTED VIOLATION (scripts/conformance/selftest.sh).
			next, err = e.intendSourceDelete(ctx, mv)'
# The third edit is what makes this a real violation rather than a refused
# write. intendSourceDelete names the phase it is leaving, and without this
# the mutant stalls at the copy phase and the suite goes red for "the write
# expected a different phase", which is a mutation that broke the build
# rather than one that broke the promise. The first run of this control did
# exactly that.
swap "$d/core/internal/placement/engine.go" \
  '		MoveID: mv.ID, From: state.MoveVerified, To: state.MoveSourceDeletePending,' \
  '		MoveID: mv.ID, From: mv.Phase, To: state.MoveSourceDeletePending,'
expect_check_fails "the source delete issued before the destination is verified" "$d" \
  "FR-30's standing invariant did not hold" \
  'TestTheThreeTierChainEndToEnd|TestTheCrashMatrixAgainstARealS3Endpoint'

echo
echo "==> FR-27's home rule"

# Chain order is load-bearing: the FIRST selecting tier names the home, so
# an artifact claimed by daily and by monthly lives on daily's medium. A
# rule that took the last one instead would quietly send every warm
# artifact offsite.
d=$(mutant home-medium-takes-the-last-tier)
swap "$d/core/internal/retention/homemedium.go" \
  '		return t.EffectiveMedium(), true, nil
	}
	return "", false, nil' \
  '		// PLANTED VIOLATION (scripts/conformance/selftest.sh): keep
		// going, so the LAST selecting tier wins instead of the first.
		medium, hasHome = t.EffectiveMedium(), true
	}
	return medium, hasHome, nil'
expect_check_fails "the home rule taking the last selecting tier instead of the first" "$d" \
  'want "local"' \
  'TestTheThreeTierChainEndToEnd'

echo
echo "==> the bucketing invariant across a move (P2.3, V2's composed shape)"

# FR-32: nothing a medium reports may reach a retention decision. The
# composed shape of that is a move changing where an artifact is bucketed,
# and the value most likely to do it is the one the move itself writes,
# which is when the destination copy was verified.
d=$(mutant bucketing-reads-when-the-copy-was-verified)
swap "$d/core/internal/retention/bucketkey.go" \
  '	r := gfsDiscoveryInstant(rec)
	discovered = gfsPlacement{date: gfsCivilDateIn(r, loc), occurred: r}' \
  '	r := gfsDiscoveryInstant(rec)
	// PLANTED VIOLATION (scripts/conformance/selftest.sh): let where the
	// bytes are decide when they were produced.
	for _, pl := range rec.Placements {
		if pl.Status == state.PlacementActive && pl.VerifiedAt != nil {
			r = *pl.VerifiedAt
		}
	}
	discovered = gfsPlacement{date: gfsCivilDateIn(r, loc), occurred: r}'
expect_check_fails "bucketing derived from when a copy was verified" "$d" \
  "changed its retention verdict" \
  'TestAMoveDoesNotChangeARetentionVerdict'

echo
echo "==> the engine's refusals"

# Medium to medium goes through a local staging copy (#429). The chain's
# second hop is the composed run of it, so putting the old outright refusal
# back has to turn that check red: the artifact stays on the monthly medium
# and never reaches the annual one.
d=$(mutant medium-to-medium-refused-again)
swap "$d/core/internal/placement/engine.go" \
  '		if err := e.canStage(rec.Artifact); err != nil {' \
  '		if err := fmt.Errorf("PLANTED VIOLATION (scripts/conformance/selftest.sh)"); err != nil {'
expect_check_fails "the medium-to-medium hop refused instead of staged" "$d" \
  "want DONE (refusal:" \
  'TestTheChainsSecondHopIsMediumToMedium'

# The staging copy is removed at the end of the copy phase. Leave it and
# every hop between two mediums costs a permanent artifact-sized file on
# the backup set's own disk, which is the disk the next hop's size check is
# about. Nothing else about the move changes, so only a check that looks
# for the leftover can see it.
d=$(mutant staging-copy-left-behind)
swap "$d/core/internal/placement/staging.go" \
  '	rmErr := e.Local.Remove(ctx, staged)' \
  '	// PLANTED VIOLATION (scripts/conformance/selftest.sh): the upload
	// landed; the staging copy stays.
	var rmErr error'
expect_check_fails "a staged hop that never removes its staging copy" "$d" \
  "the staging copy is still at" \
  'TestTheChainsSecondHopIsMediumToMedium'

# The other end of the same ordering: the source delete is durably
# intended, and then it actually happens. A move that records the intent
# and leaves the file behind reads as a completed move everywhere except
# on the disk, and every artifact quietly costs two copies for ever.
d=$(mutant source-copy-never-removed)
swap "$d/core/internal/placement/engine.go" \
  '	if t.localPath != "" {
		if e.Local == nil {
			return fmt.Errorf("no local store is configured")
		}
		return e.Local.Remove(ctx, t.localPath)
	}' \
  '	if t.localPath != "" {
		if e.Local == nil {
			return fmt.Errorf("no local store is configured")
		}
		// PLANTED VIOLATION (scripts/conformance/selftest.sh): the journal
		// says the source is gone; the file stays.
		return nil
	}'
expect_check_fails "a completed move that never removes the source copy" "$d" \
  'still has a local copy after a completed move to "annual_s3"' \
  'TestTheThreeTierChainEndToEnd'

# A copy that fails leaves its reason on the move row. Without it the row
# reads COPYING with an empty error for as long as the failure lasts, and
# the only account of what went wrong lives in a cycle report that is gone.
d=$(mutant failed-copy-records-no-reason)
swap "$d/core/internal/placement/engine.go" \
  '		noted, noteErr := e.step(ctx, mv, Copying, Copying, wrapped.Error())
		if noteErr != nil {
			return mv, fmt.Errorf("%w (and the reason could not be recorded on the move row: %v)", wrapped, noteErr)
		}
		return noted, wrapped' \
  '		// PLANTED VIOLATION (scripts/conformance/selftest.sh).
		return mv, wrapped'
expect_check_fails "a failed copy that records no reason on the move row" "$d" \
  "the move row carries no error at all" \
  'TestAFailedCopyLeavesItsReasonOnTheMoveRow'

echo
echo "==> prune meeting an artifact the chain has moved (P2.4, V6)"

# #239's medium-aware prune is in this tree, so what these mutations break
# is no longer "refuse until it is built". It is the shape of the delete:
# the decision is retention's, taken without ever reading the medium
# (FR-32), and the evidence is placement's, taken at the moment of the
# delete (FR-16). The composed scenario is the only place either half runs
# against an object a real move really put on a real bucket.

# An artifact whose copy is on a medium sent down FR-20's local path
# instead. That is wrong in the most expensive way available: the local
# file is not there any more, so the verdict is about a path nothing owns
# while the object survives unreferenced.
d=$(mutant prune-takes-the-local-path-for-a-medium-copy)
swap "$d/core/internal/retention/prune.go" \
  '	if loc.OnMedium() {
		return pruneEvaluateOnMedium(rec, keepVerdict, lkg, loc.Medium)
	}' \
  '	// PLANTED VIOLATION (scripts/conformance/selftest.sh).
	if false {
		return pruneEvaluateOnMedium(rec, keepVerdict, lkg, loc.Medium)
	}'
expect_check_fails "an artifact on a medium sent down FR-20's local path" "$d" \
  "is REFUSE, want DELETE" \
  'TestPruneRemovesAnArtifactsOnlyCopyFromAMedium'

# A DELETE reported without anything being asked to make it. The verdict
# reads exactly the same and the object is still there, which is the
# failure mode a dry run cannot show and only the apply can.
d=$(mutant prune-apply-never-asks-the-pruner)
swap "$d/core/internal/retention/prune.go" \
  '			if err := medium.DeleteFromMedium(ctx, rec, verdicts[i].Medium); err != nil {
				verdicts[i].Action = PruneRefuse
				verdicts[i].Reason = fmt.Sprintf("nothing was removed from %q: %v", verdicts[i].Medium, err)
			}
			continue' \
  '			// PLANTED VIOLATION (scripts/conformance/selftest.sh): report the
			// delete without making it.
			_ = medium
			_ = rec
			continue'
expect_check_fails "a medium delete reported but never made" "$d" \
  "PruneApply asked the medium pruner for []" \
  'TestPruneRemovesAnArtifactsOnlyCopyFromAMedium'

# V6's own planted violation, from the matrix: something else is at the key
# by the time prune applies. FR-16 says compare before deleting, and this
# is the comparison being run and then ignored.
d=$(mutant fr16-recheck-result-ignored)
swap "$d/core/internal/placement/reclaim.go" \
  '	if !existence.Passed {
		return refuse("%s", existence.Detail)
	}' \
  '	// PLANTED VIOLATION (scripts/conformance/selftest.sh): an object that
	// is there is an object worth deleting.
	_ = existence'
expect_check_fails "the FR-16 identity re-check run and its answer ignored" "$d" \
  "and prune's verdict is DELETE, not REFUSE" \
  'TestPruneRefusesAnObjectThatIsNoLongerTheOneTheJournalRecorded'

# The other half of the same row: FR-30 asks that the mandatory dry-run
# NAME the medium for every proposed deletion, and that surface is the
# CLI's, not the composed suite's. "DELETE 40 artifacts" reads very
# differently when half of them are objects in a bucket, and an operator
# confirming a plan is confirming this text.
d=$(mutant dry-run-does-not-name-the-medium)
swap "$d/core/cmd/backup-manager/retention.go" \
  '	case loc.Status == retention.LocationConfirmed && loc.Medium != config.MediumLocal:
		return " medium=" + loc.Medium' \
  '	case loc.Status == retention.LocationConfirmed && loc.Medium != config.MediumLocal:
		// PLANTED VIOLATION (scripts/conformance/selftest.sh).
		return ""'
expect_unit_check_fails "a dry-run that does not say where a deletion would happen" "$d" \
  "does not say where its deletion would happen" \
  ./cmd/backup-manager/ 'TestRun_RetentionNamesWhereADeletionWouldHappen'

echo
echo "==> the archive gate"

# A retention tier bound to an archive-class medium is refused at LOAD,
# before a daemon starts (#442). Take that away and the config validates,
# the deployment runs, and every cycle plans a move the engine then refuses
# for ever; before #437 it also paid for two uploads and three deletes a
# cycle on a class that bills a 180-day minimum for each discarded copy.
#
# The engine's own plan-time refusal is still there and is still the second
# line of defence, for a journal written by an older build or a bucket that
# grew a lifecycle rule. It is unreachable from this suite now, because the
# config it needs no longer loads, so its falsification lives in
# internal/placement's archiveupload_test.go instead.
d=$(mutant archive-tier-accepted-at-load)
swap "$d/core/internal/config/validate.go" \
  '	class, ok := archived[t.Medium]
	if !ok {
		return
	}' \
  '	// PLANTED VIOLATION (scripts/conformance/selftest.sh): accept the
	// pairing and let the operator find out per cycle.
	class, ok := archived[t.Medium]
	if true || !ok {
		_ = class
		return
	}'
expect_check_fails "a retention tier on an archive class accepted at load" "$d" \
  "the config loaded with the annual tier on a GLACIER medium" \
  'TestAnArchiveClassTierIsRefusedAtLoad'

# An archived copy tops out at existence, because reading one fails. Let it
# claim content and the composed scenario's whole argument about why the
# annual tier is blocked stops being true.
d=$(mutant archived-copy-claims-content)
swap "$d/core/internal/placement/gate.go" \
  '	case archive.RequiresRestore, archive.Restoring:
		return Existence' \
  '	case archive.RequiresRestore, archive.Restoring:
		// PLANTED VIOLATION (scripts/conformance/selftest.sh).
		return Content'
expect_check_fails "an archived copy allowed to claim content class" "$d" \
  "now has a ceiling of" \
  'TestNoConfigurableVerificationCanBeAchievedOnAnArchiveClass'

# The standing invariant means content class unless the operator opted into
# attested. Admitting existence would make an archived copy an acceptable
# SOLE copy, which is the failure the whole EPIC is built to prevent.
d=$(mutant invariant-admits-existence)
swap "$d/core/internal/placement/engine.go" \
  '	if len(sufficient) == 0 {
		sufficient = []Class{Content}
	}' \
  '	if len(sufficient) == 0 {
		// PLANTED VIOLATION (scripts/conformance/selftest.sh).
		sufficient = []Class{Content, Existence}
	}'
expect_check_fails "the standing invariant admitting existence class" "$d" \
  "satisfies FR-30's standing invariant" \
  'TestAnArchiveClassCopyCannotSatisfyTheStandingInvariant'

echo
echo "==> the crash matrix's convergence"

# The destination is re-read and re-hashed before the source is deleted,
# on the fresh path and the restart path alike. Trust the upload instead
# and the two hostile-endpoint cells stop protecting anything: the local
# copy goes and what survives is whatever the endpoint felt like keeping.
#
# It takes BOTH return paths, and that is not tidiness. #437 split
# verifyCopy in two, an ungated Verify and a gated VerifyWithAccess, and a
# mutation that took only one of them would leave the other still reading
# the bytes back, which is a mutation that proves nothing. The anchor this
# used to carry was the pre-split one, and it stopped matching: the swap
# refused to plant, which is exactly what it is for.
#
# The three `_ =` lines are load-bearing too. Without them the mutant does
# not compile, and a mutant that does not compile fails the suite for a
# reason that has nothing to do with the promise.
d=$(mutant destination-trusted-instead-of-verified)
swap "$d/core/internal/placement/engine.go" \
  '	if !gated {
		res, err := Verify(ctx, e.Store, medium, candidate, want, e.now())
		return res, want, err
	}
	// observe spends a restore-status call only for a class that needs
	// one, so a STANDARD destination costs exactly what it cost before.
	obs := e.observe(ctx, medium, mv.DestinationKey, medium.StorageClass)
	res, err := VerifyWithAccess(ctx, e.Store, medium, candidate, want, obs, e.now())
	return res, want, err' \
  '	// PLANTED VIOLATION (scripts/conformance/selftest.sh): trust the
	// upload rather than reading it back, on both paths.
	_ = gated
	_ = medium
	_ = candidate
	return Result{Passed: true, Class: want, Detail: "planted"}, want, nil'
expect_check_fails "a destination trusted instead of verified" "$d" \
  "does not hold the artifact's bytes" \
  'TestTheCrashMatrixAgainstARealS3Endpoint'

echo
echo "==> the continuity claim itself"

# The sampler in sampler_test.go is the control that gives "continuously"
# its meaning, and a sampler that secretly looked continuously would make
# the comparison vacuous while leaving it green. So the control gets
# mutation-tested too: make the sampler look at every event and the
# comparison must refuse to conclude anything.
d=$(mutant sampler-secretly-watches-continuously)
swap "$d/core/tests/conformance/watcher_test.go" \
  '	mv, err := j.inner.AdvanceMove(ctx, a)
	j.w.observe(fmt.Sprintf("after the journal wrote phase %s", a.To))
	return mv, err' \
  '	mv, err := j.inner.AdvanceMove(ctx, a)
	j.w.observe(fmt.Sprintf("after the journal wrote phase %s", a.To))
	// PLANTED VIOLATION (scripts/conformance/selftest.sh): the sampler
	// looks at every event too, so the comparison compares nothing.
	if j.w.sampler != nil {
		j.w.sampler.sample(j.w.t, "at every event")
	}
	return mv, err'
swap "$d/core/tests/conformance/watcher_test.go" \
  '	// destroyed is the set of locators this run has watched being
	// deleted and has not seen rewritten.
	destroyed map[string]bool' \
  '	// destroyed is the set of locators this run has watched being
	// deleted and has not seen rewritten.
	destroyed map[string]bool
	sampler   *sampler'
swap "$d/core/tests/conformance/sampler_test.go" \
  '	sm := newSampler(w.journal, []model.ArtifactID{summer.id})

	sm.sample(t, "before the cycle")
	wa.observe("before the cycle")

	plant := &earlyRelease{read: w.journal, target: summer.id}' \
  '	sm := newSampler(w.journal, []model.ArtifactID{summer.id})
	wa.sampler = sm

	sm.sample(t, "before the cycle")
	wa.observe("before the cycle")

	plant := &earlyRelease{read: w.journal, target: summer.id}'
expect_check_fails "a sampler that secretly looks continuously" "$d" \
  "the sampler saw the breach" \
  'TestTheWatcherCatchesABreachASamplerWouldMiss'

# And the other direction: a planted breach that plants nothing must be
# refused rather than read as agreement between the two judgements.
d=$(mutant planted-breach-plants-nothing)
swap "$d/core/tests/conformance/sampler_test.go" \
  '	a.Placements = append(a.Placements, src.Update().WithStatus(state.PlacementDeletePending))
	j.fired++' \
  '	// PLANTED VIOLATION (scripts/conformance/selftest.sh): plant nothing.
	_ = src'
expect_check_fails "a planted breach that plants nothing" "$d" \
  "the planted breach never fired" \
  'TestTheWatcherCatchesABreachASamplerWouldMiss'

echo
if [ "$selftest_dry_run" = 1 ]; then
  echo "==> $selftest_anchors_checked anchors checked, $selftest_anchors_stale stale"
else
  echo "==> $pass passed, $fail failed, $selftest_stale_count stale"
fi
selftest_stale_summary
if [ "$fail" -ne 0 ] || [ "$selftest_stale_count" -ne 0 ]; then
  exit 1
fi

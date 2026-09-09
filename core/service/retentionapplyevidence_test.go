// Evidence that a retention apply removes exactly the previewed DELETE
// set, on a real directory, with a control proving the comparison can
// fail (issue #602).
//
// # Why this file exists beside the ones already here
//
// retention_test.go covers the envelope: single-use plan ids, the
// staleness refusal, the busy refusal, the concurrency races. Every one
// of those cases asserts about ONE seeded artifact, and asserts it by
// name: "a.dump is still there", "a.dump is gone". That is the right
// shape for a claim about the envelope and the wrong shape for the claim
// this issue is about, because naming the file you look at is exactly
// how you stop seeing the one you did not.
//
// The claim here is about the whole directory. Everything under the
// backup set's root is fingerprinted before the apply and again after,
// the two snapshots are subtracted, and the difference must be exactly
// the set of paths the preview marked DELETE: not a superset, not a
// subset, with every survivor byte-identical. A delete is the one
// operation a stub cannot prove anything about, so nothing here is
// mocked; retentionEvidenceTree walks real directory entries and hashes
// real bytes.
//
// # The vacuity problem, and the control that answers it
//
// A whole-tree comparison has one failure mode that reads exactly like a
// pass. If the fixture ages nothing out, the previewed DELETE set is
// empty, and "every previewed DELETE is gone" plus "every previewed KEEP
// survives" are both true of an apply that did absolutely nothing. This
// repository has been caught by that shape before and answers it the same
// way each time (scripts/conformance/selftest.sh, scripts/compat/
// selftest.sh): a check nobody has watched fail is indistinguishable from
// one that cannot fail.
//
// So the comparison is a pure function that RETURNS its complaints
// (retentionEvidenceCompare) rather than a helper that calls t.Errorf,
// and TestRetentionApplyEvidence_TheComparisonNoticesWhatItIsAskedTo is
// the control that feeds it three perturbed after-states, taken from the
// same real run, and requires it to complain about each one by name:
//
//   - a file the preview never mentioned goes missing anyway. This is the
//     one the issue names, and it is the one an "exactly this set"
//     assertion written the obvious way misses completely, because the
//     obvious way only ever looks up the paths it already has.
//   - a previewed DELETE is still on disk, which is the "the apply did
//     nothing" reading.
//   - an empty plan, which the comparison refuses outright rather than
//     answering true about.
//
// The mutation half, where a violation is planted in the real product
// source and this suite has to go red naming the promise, is
// scripts/retention/selftest.sh, for the same reason the compat and
// conformance controls live in scripts rather than in _test.go: it costs
// a build per control and it has to run against a copy of the tree.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// retentionEvidenceEntry is one directory entry as this evidence reads
// it: enough to tell "gone", "still here" and "still the same bytes"
// apart, and nothing else.
//
// Digest carries the content hash for a regular file and the LINK TARGET
// for a symlink, which is deliberate rather than an overload of one
// field. FR-20 refuses a symlink at an artifact's final path outright and
// never resolves it, so the fact this evidence has to be able to state is
// that the link is still a link pointing where it pointed, which is a
// claim about the target text and not about whatever bytes are at the end
// of it.
type retentionEvidenceEntry struct {
	Kind   string
	Size   int64
	Digest string
}

func (e retentionEvidenceEntry) String() string {
	return fmt.Sprintf("%s size=%d digest=%s", e.Kind, e.Size, e.Digest)
}

// retentionEvidenceTree fingerprints every entry under root, keyed by the
// path relative to root.
//
// Lstat semantics throughout (filepath.WalkDir does not follow symlinks),
// because a walk that followed one would describe a tree this backup set
// does not own and would report the target's bytes as if they were the
// artifact's.
func retentionEvidenceTree(t *testing.T, root string) map[string]retentionEvidenceEntry {
	t.Helper()

	out := map[string]retentionEvidenceEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		switch {
		case d.Type()&os.ModeSymlink != 0:
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}
			out[rel] = retentionEvidenceEntry{Kind: "symlink", Digest: target}
		case d.IsDir():
			out[rel] = retentionEvidenceEntry{Kind: "dir"}
		default:
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sum := sha256.Sum256(b)
			out[rel] = retentionEvidenceEntry{Kind: "file", Size: info.Size(), Digest: hex.EncodeToString(sum[:])}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// retentionEvidenceCompare is the whole assertion, as a pure function
// that returns what is wrong rather than a helper that fails a test.
//
// That shape is the point. A helper that called t.Errorf could only ever
// be exercised by a run that was already failing, so nothing could ask it
// "would you notice"; this one can be handed a deliberately perturbed
// after-state and required to complain, which is what the control below
// does. Same reason internal/retention's own PruneDecide is separate from
// PruneApply: the thing that decides is worth being able to call without
// the thing that acts.
//
// wantRemoved is the set of paths, relative to the backup-set root, that
// the preview marked DELETE. An empty one is refused rather than answered
// true about: with nothing named for deletion, every other clause here is
// vacuously satisfied by an apply that did nothing at all, and returning
// "no complaints" for that would be this function reporting a pass it did
// not establish.
func retentionEvidenceCompare(before, after map[string]retentionEvidenceEntry, wantRemoved []string) []string {
	var complaints []string

	if len(wantRemoved) == 0 {
		return []string{
			"the plan named no artifact for deletion, so this comparison certifies nothing: " +
				"every clause below is vacuously true of an apply that removed nothing at all. " +
				"Fix the fixture so the plan actually selects something, rather than reading this as a pass",
		}
	}

	want := map[string]bool{}
	for _, p := range wantRemoved {
		if _, ok := before[p]; !ok {
			complaints = append(complaints, fmt.Sprintf(
				"the plan marked %q DELETE and it was not in the tree before the apply, so this comparison was never about a real file", p))
			continue
		}
		want[p] = true
	}

	// `is` rather than `now`, which is this package's own clock function
	// (service.go) and would be shadowed for the rest of this loop.
	for path, was := range before {
		is, stillThere := after[path]
		switch {
		case !stillThere && !want[path]:
			// The clause the issue is about. An assertion written the
			// obvious way (look up each previewed DELETE, check it is
			// gone) can never reach this line at all, because it only
			// ever visits paths the plan already named.
			complaints = append(complaints, fmt.Sprintf(
				"%q was removed and no verdict in the plan named it; a retention apply may only ever remove what the preview an operator read marked DELETE (it was %s)", path, was))
		case !stillThere:
			// Removed, and the plan said so. Nothing to say.
		case want[path]:
			complaints = append(complaints, fmt.Sprintf(
				"%q is still on disk and the plan marked it DELETE, so the apply did not carry out the plan that was confirmed (it is %s)", path, is))
		case was != is:
			complaints = append(complaints, fmt.Sprintf(
				"%q survived the apply but is not the same file: it was %s and is now %s", path, was, is))
		}
	}

	for path, is := range after {
		if _, existed := before[path]; !existed {
			complaints = append(complaints, fmt.Sprintf(
				"%q appeared during the apply; a retention apply removes, and creates nothing (it is %s)", path, is))
		}
	}

	sort.Strings(complaints)
	return complaints
}

// retentionEvidenceFixture is one backup set on a real directory, its
// journal, and the service that decides about it.
type retentionEvidenceFixture struct {
	bs      config.BackupSet
	journal *state.Journal
	svc     *BackupService
	root    string
}

// retentionEvidenceChain is a chain with a live daily tier of seven days
// and last-known-good protection OFF, so what a tier keeps is decided by
// the artifact's own age and by nothing else.
//
// Seven days rather than one, because this fixture wants both outcomes in
// the same tree: a plan whose DELETE set is empty proves nothing (see
// retentionEvidenceCompare) and so does one whose KEEP set is, since
// "nothing else was touched" is the half of the claim that needs
// survivors to be about.
func retentionEvidenceChain(protectLastKnownGood bool) config.Retention {
	protect := protectLastKnownGood
	return config.Retention{
		Timezone:     "UTC",
		WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "daily", Granularity: config.GranularityDay, Keep: 7},
		},
		ProtectLastKnownGood: &protect,
	}
}

// newRetentionEvidenceFixture builds the backup set, the journal and the
// service, and returns them with the root already created.
func newRetentionEvidenceFixture(t *testing.T, ret config.Retention) *retentionEvidenceFixture {
	t.Helper()
	root := t.TempDir()
	bs := retentionTestBackupSet(t, root)
	journal := openTestJournal(t)
	return &retentionEvidenceFixture{
		bs:      bs,
		journal: journal,
		svc:     New(retentionTestConfig(bs, ret), journal, nil, nil),
		root:    root,
	}
}

// seedDaysAgo puts one COMPLETE artifact on disk and in the journal,
// dated whole days back from the instant this test runs.
//
// Whole days back from now, rather than a fixed calendar date, for the
// reason core/tests/compat's own retention cell gives: nothing here can
// pin this binary's clock, so a fixture anchored on a literal date drifts
// out of every tier window as the date passes and the test starts
// certifying something else. A single seven-day daily tier plus offsets
// of 1, 2, 40 and 400 days gives the same two-in, two-out split on every
// day of the year.
func (f *retentionEvidenceFixture) seedDaysAgo(t *testing.T, name string, days int, content string) model.ArtifactID {
	t.Helper()
	at := now().Add(-time.Duration(days) * 24 * time.Hour)
	return seedCompleteArtifact(t, context.Background(), f.journal, f.bs, name, at, content)
}

// previewedDeletes reads a plan's DELETE verdicts back as paths relative
// to the backup-set root, which is the vocabulary retentionEvidenceTree
// speaks.
//
// Only the verdicts whose medium is local: a DELETE on a storage medium
// removes an object and leaves the local tree alone, so counting one as a
// path this tree should have lost would make the comparison wrong in the
// one deployment shape that has mediums. Nothing in this file declares
// one, so this returns every DELETE it sees today; it is written this way
// so it keeps meaning what it says when something does.
func previewedDeletes(plan RetentionPlan) []string {
	var out []string
	for _, v := range plan.Verdicts {
		if v.Action == "DELETE" && (v.Medium == "" || v.Medium == config.MediumLocal) {
			out = append(out, v.Artifact)
		}
	}
	sort.Strings(out)
	return out
}

// previewedActions is every verdict as action -> artifact names, for the
// preconditions each test states about its own fixture before it trusts
// anything the apply did.
func previewedActions(plan RetentionPlan) map[string][]string {
	out := map[string][]string{}
	for _, v := range plan.Verdicts {
		out[v.Action] = append(out[v.Action], v.Artifact)
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}

// TestRetentionApplyEvidence_RemovesExactlyThePreviewedDeleteSet is the
// claim issue #602 asks for, on a real tree.
//
// The tree deliberately holds more than the artifacts under decision:
//
//   - unmanaged-by-anything.txt, which no journal row mentions. FR-20's
//     own package doc says nothing here ever lists a directory to find
//     something to delete, so a file the journal does not know about must
//     survive an apply untouched. This is also the file the control below
//     makes disappear, to prove the comparison would notice.
//   - a .partial, which is FR-12's in-flight marker and never a
//     restorable artifact.
//   - a nested directory, so "the walk actually descends" is not an
//     assumption.
func TestRetentionApplyEvidence_RemovesExactlyThePreviewedDeleteSet(t *testing.T) {
	ctx := context.Background()
	f := newRetentionEvidenceFixture(t, retentionEvidenceChain(false))

	f.seedDaysAgo(t, "one-day-old.dump", 1, "inside the daily window, keep me")
	f.seedDaysAgo(t, "two-days-old.dump", 2, "inside the daily window, keep me too")
	f.seedDaysAgo(t, "forty-days-old.dump", 40, "outside every window")
	f.seedDaysAgo(t, "four-hundred-days-old.dump", 400, "outside every window as well")

	// Three things retention was never told about, in the same directory
	// it is about to delete from.
	writeEvidenceFile(t, filepath.Join(f.root, "unmanaged-by-anything.txt"), "no journal row mentions this")
	writeEvidenceFile(t, filepath.Join(f.root, "still-arriving.dump.partial"), "an in-flight transfer")
	if err := os.MkdirAll(filepath.Join(f.root, "nested"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeEvidenceFile(t, filepath.Join(f.root, "nested", "deep.txt"), "a file one level down")

	plan, err := f.svc.PreviewRetention(ctx, f.bs.ID.Source, f.bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	actions := previewedActions(plan)
	wantRemoved := previewedDeletes(plan)
	if len(wantRemoved) != 2 {
		t.Fatalf("precondition: the preview marked %d artifact(s) DELETE, want exactly 2 (the 40- and 400-day-old ones). Verdicts: %+v", len(wantRemoved), plan.Verdicts)
	}
	if len(actions["KEEP"]) != 2 {
		t.Fatalf("precondition: the preview marked %d artifact(s) KEEP, want exactly 2. Without survivors, \"nothing else was touched\" is a claim about an empty set. Verdicts: %+v", len(actions["KEEP"]), plan.Verdicts)
	}

	before := retentionEvidenceTree(t, f.root)

	applied, err := f.svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: f.bs.ID.Source, Set: f.bs.ID.Set, Actor: "evidence",
	})
	if err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}

	after := retentionEvidenceTree(t, f.root)

	for _, complaint := range retentionEvidenceCompare(before, after, wantRemoved) {
		t.Errorf("the apply did not remove exactly the previewed DELETE set: %s", complaint)
	}

	// The applied plan has to agree with what the disk says, or the
	// operator's receipt describes a different run from the one that
	// happened.
	if got := previewedDeletes(applied); !equalStrings(got, wantRemoved) {
		t.Errorf("the applied plan reports DELETE for %v and the preview reported %v; an apply reports what it did, and these are two different claims", got, wantRemoved)
	}
}

// TestRetentionApplyEvidence_TheComparisonNoticesWhatItIsAskedTo is the
// positive control, and it is the reason anything above means anything.
//
// It runs the same real apply, then hands the comparison three
// after-states it has to complain about. Perturbing the SNAPSHOT rather
// than the disk is deliberate: what is under test here is the assertion,
// not the product, and a control that deleted a real file would be
// testing os.Remove.
func TestRetentionApplyEvidence_TheComparisonNoticesWhatItIsAskedTo(t *testing.T) {
	ctx := context.Background()
	f := newRetentionEvidenceFixture(t, retentionEvidenceChain(false))

	f.seedDaysAgo(t, "one-day-old.dump", 1, "inside the daily window")
	f.seedDaysAgo(t, "forty-days-old.dump", 40, "outside every window")
	planted := filepath.Join(f.root, "unmanaged-by-anything.txt")
	writeEvidenceFile(t, planted, "no journal row mentions this")

	plan, err := f.svc.PreviewRetention(ctx, f.bs.ID.Source, f.bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	wantRemoved := previewedDeletes(plan)
	if len(wantRemoved) != 1 {
		t.Fatalf("precondition: the preview marked %d artifact(s) DELETE, want exactly 1. Verdicts: %+v", len(wantRemoved), plan.Verdicts)
	}

	before := retentionEvidenceTree(t, f.root)
	if _, err := f.svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: f.bs.ID.Source, Set: f.bs.ID.Set, Actor: "evidence",
	}); err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	after := retentionEvidenceTree(t, f.root)

	if complaints := retentionEvidenceCompare(before, after, wantRemoved); len(complaints) != 0 {
		t.Fatalf("the control needs a clean run to perturb, and this one was not clean: %v", complaints)
	}

	// 1. The one the issue names: a file the preview never mentioned goes
	//    missing anyway. An apply that reached outside its plan looks
	//    exactly like this, and an assertion that only ever visits the
	//    paths the plan named cannot see it.
	t.Run("a file no verdict named going missing", func(t *testing.T) {
		perturbed := copyEvidenceTree(after)
		delete(perturbed, "unmanaged-by-anything.txt")
		requireComplaintNaming(t, retentionEvidenceCompare(before, perturbed, wantRemoved), "unmanaged-by-anything.txt",
			"a retention apply that removed a file the journal never knew about")
	})

	// 2. The apply-did-nothing reading, which is what every clause above
	//    is vacuously true of when the plan is empty and silently true of
	//    when the delete never happened.
	t.Run("a previewed DELETE still on disk", func(t *testing.T) {
		perturbed := copyEvidenceTree(after)
		perturbed[wantRemoved[0]] = before[wantRemoved[0]]
		requireComplaintNaming(t, retentionEvidenceCompare(before, perturbed, wantRemoved), wantRemoved[0],
			"an apply that confirmed a plan and then carried none of it out")
	})

	// 3. A survivor whose bytes moved. "Present afterwards" is a weaker
	//    claim than "present and the same file", and the difference is
	//    the whole of what a restore point is for.
	t.Run("a survivor whose bytes changed", func(t *testing.T) {
		perturbed := copyEvidenceTree(after)
		victim := "one-day-old.dump"
		was, ok := perturbed[victim]
		if !ok {
			t.Fatalf("the control expected %q to have survived; the tree after the apply holds %v", victim, sortedKeys(perturbed))
		}
		was.Digest = "0000000000000000000000000000000000000000000000000000000000000000"
		perturbed[victim] = was
		requireComplaintNaming(t, retentionEvidenceCompare(before, perturbed, wantRemoved), victim,
			"a KEEP whose bytes were rewritten under it")
	})

	// 4. The vacuity refusal itself, which is the reason this comparison
	//    is a function with a return value at all.
	t.Run("an empty plan is refused rather than answered true", func(t *testing.T) {
		complaints := retentionEvidenceCompare(before, before, nil)
		if len(complaints) == 0 {
			t.Fatal("the comparison reported no complaints for a plan that named nothing to delete, against a tree nothing happened to. That is the shape every assertion in this file is vacuously true of, so it has to be a refusal and not a pass.")
		}
		if !strings.Contains(strings.Join(complaints, "\n"), "certifies nothing") {
			t.Errorf("the comparison refused an empty plan without saying why; complaints were %v", complaints)
		}
	})
}

// TestRetentionApplyEvidence_LastKnownGoodSurvivesATierVerdictThatWouldRemoveIt
// is FR-19 on a real tree, through the whole envelope.
//
// The fixture is the shape that makes the protection load-bearing rather
// than incidental: EVERY artifact is older than the live tier's window,
// so GFS alone selects none of them and the plan would otherwise empty
// the backup set. FR-19 keeps the newest one, which is the last restore
// point this deployment has, and it is kept on disk and byte-identical.
//
// The same fixture with protection off is run first, as this test's own
// control: without it, "the newest file survived" could be true because
// the tier kept it, because nothing was deleted at all, or because the
// fixture never selected anything, and those are three different worlds.
func TestRetentionApplyEvidence_LastKnownGoodSurvivesATierVerdictThatWouldRemoveIt(t *testing.T) {
	ctx := context.Background()

	// The control: the same ages, the same chain, protection OFF. Every
	// artifact is selected for deletion, the newest included.
	unprotected := newRetentionEvidenceFixture(t, retentionEvidenceChain(false))
	unprotected.seedDaysAgo(t, "newest.dump", 40, "the newest restore point")
	unprotected.seedDaysAgo(t, "older.dump", 90, "an older one")
	unprotectedPlan, err := unprotected.svc.PreviewRetention(ctx, unprotected.bs.ID.Source, unprotected.bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention (protection off): %v", err)
	}
	if got := previewedDeletes(unprotectedPlan); !equalStrings(got, []string{"newest.dump", "older.dump"}) {
		t.Fatalf("the control fixture does not put FR-19 under any pressure: with protection OFF the plan marks %v DELETE, and this test only means something when that includes newest.dump. Verdicts: %+v", got, unprotectedPlan.Verdicts)
	}

	// The claim: identical in every respect except protect_last_known_good.
	protected := newRetentionEvidenceFixture(t, retentionEvidenceChain(true))
	protected.seedDaysAgo(t, "newest.dump", 40, "the newest restore point")
	protected.seedDaysAgo(t, "older.dump", 90, "an older one")

	plan, err := protected.svc.PreviewRetention(ctx, protected.bs.ID.Source, protected.bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention (protection on): %v", err)
	}
	wantRemoved := previewedDeletes(plan)
	if !equalStrings(wantRemoved, []string{"older.dump"}) {
		t.Fatalf("with FR-19 protection on, the plan marks %v DELETE; want exactly [older.dump], because newest.dump is this backup set's last known good. Verdicts: %+v", wantRemoved, plan.Verdicts)
	}

	before := retentionEvidenceTree(t, protected.root)
	if _, err := protected.svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: protected.bs.ID.Source, Set: protected.bs.ID.Set, Actor: "evidence",
	}); err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	after := retentionEvidenceTree(t, protected.root)

	for _, complaint := range retentionEvidenceCompare(before, after, wantRemoved) {
		t.Errorf("FR-19: %s", complaint)
	}

	// Said again directly, because this is the one artifact whose
	// survival is the whole point and a comparison over a set can be read
	// past.
	kept, ok := after["newest.dump"]
	if !ok {
		t.Fatal("FR-30: the last-known-good restore point was removed, which leaves this backup set with no confirmed readable copy of anything at all")
	}
	if kept != before["newest.dump"] {
		t.Errorf("the last-known-good restore point survived but is not the same file: it was %s and is now %s", before["newest.dump"], kept)
	}
}

// TestRetentionApplyEvidence_StalePlanLeavesTheTreeExactlyAsItWas is the
// staleness refusal stated as a claim about the directory.
//
// settings_gate_test.go and retention_test.go already pin the refusal
// itself, at the HTTP boundary and at this one, and each asserts that the
// one artifact it seeded is still there. This asserts that NOTHING under
// the root moved: not the artifact the stale plan named, not the ones it
// did not, and not the files no verdict was ever about. "Refused after
// deleting half the plan" produces exactly the error message those tests
// look for.
func TestRetentionApplyEvidence_StalePlanLeavesTheTreeExactlyAsItWas(t *testing.T) {
	ctx := context.Background()
	f := newRetentionEvidenceFixture(t, retentionEvidenceChain(false))

	f.seedDaysAgo(t, "one-day-old.dump", 1, "inside the daily window")
	f.seedDaysAgo(t, "forty-days-old.dump", 40, "outside every window")
	f.seedDaysAgo(t, "four-hundred-days-old.dump", 400, "outside every window as well")
	writeEvidenceFile(t, filepath.Join(f.root, "unmanaged-by-anything.txt"), "no journal row mentions this")

	plan, err := f.svc.PreviewRetention(ctx, f.bs.ID.Source, f.bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if len(previewedDeletes(plan)) != 2 {
		t.Fatalf("precondition: the plan has to select something for its refusal to be about anything; it marked %v DELETE", previewedDeletes(plan))
	}

	// The configuration moves out from under the plan: a new artifact
	// lands, which is what a cycle finishing between the preview and the
	// apply looks like.
	f.seedDaysAgo(t, "just-arrived.dump", 0, "a cycle finished in between")

	before := retentionEvidenceTree(t, f.root)

	_, err = f.svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: f.bs.ID.Source, Set: f.bs.ID.Set, Actor: "evidence",
	})
	if !errors.Is(err, ErrRetentionPlanStale) {
		t.Fatalf("ApplyRetentionPlan = %v, want ErrRetentionPlanStale (RETENTION_PLAN_STALE)", err)
	}

	after := retentionEvidenceTree(t, f.root)
	if diff := describeEvidenceDifference(before, after); diff != "" {
		t.Errorf("a stale plan was refused and the tree changed anyway, so the refusal happened after something was already destroyed:\n%s", diff)
	}
}

// TestRetentionApplyEvidence_ASymlinkAtAnArtifactsPathIsRefusedNotFollowed
// is FR-20's positively-identified removal, through the whole envelope
// and on a real filesystem.
//
// The artifact is recorded COMPLETE in the journal, its final path holds
// a symlink instead of a regular file, and the link points at a file
// OUTSIDE the backup-set root. internal/retention refuses a symlink at a
// final path outright rather than resolving it, so what has to be true
// afterwards is both halves: the link is still there, and the file it
// points at was never touched. A resolve-then-delete implementation
// passes "the artifact is gone" and destroys somebody else's file.
func TestRetentionApplyEvidence_ASymlinkAtAnArtifactsPathIsRefusedNotFollowed(t *testing.T) {
	ctx := context.Background()
	f := newRetentionEvidenceFixture(t, retentionEvidenceChain(false))

	f.seedDaysAgo(t, "forty-days-old.dump", 40, "outside every window")

	// Somewhere else entirely, which the link will point into.
	outside := t.TempDir()
	victim := filepath.Join(outside, "not-ours.dump")
	writeEvidenceFile(t, victim, "a file this backup set does not own")

	// The seeded artifact's final path becomes a symlink to it. The
	// journal row is untouched, so this is exactly the anomaly FR-20
	// describes: the journal says a managed artifact lives here, and what
	// lives here is a link.
	swapped := f.seedDaysAgo(t, "symlinked.dump", 41, "about to be replaced by a link")
	swappedPath := filepath.Join(f.root, "symlinked.dump")
	if err := os.Remove(swappedPath); err != nil {
		t.Fatalf("Remove(%s): %v", swappedPath, err)
	}
	if err := os.Symlink(victim, swappedPath); err != nil {
		t.Fatalf("Symlink(%s -> %s): %v", swappedPath, victim, err)
	}

	plan, err := f.svc.PreviewRetention(ctx, f.bs.ID.Source, f.bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	actions := previewedActions(plan)
	if !contains(actions["REFUSE"], swapped.Name) {
		t.Fatalf("the preview did not REFUSE %s, whose final path is a symlink; it said %+v", swapped.Name, plan.Verdicts)
	}
	wantRemoved := previewedDeletes(plan)
	if !equalStrings(wantRemoved, []string{"forty-days-old.dump"}) {
		t.Fatalf("precondition: the plan marks %v DELETE; want exactly [forty-days-old.dump], so the run has a real deletion beside the refusal", wantRemoved)
	}

	before := retentionEvidenceTree(t, f.root)
	if _, err := f.svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: f.bs.ID.Source, Set: f.bs.ID.Set, Actor: "evidence",
	}); err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	after := retentionEvidenceTree(t, f.root)

	for _, complaint := range retentionEvidenceCompare(before, after, wantRemoved) {
		t.Errorf("FR-20: %s", complaint)
	}

	// The half a same-root comparison cannot see, because the file is not
	// under the root at all.
	if _, err := os.Lstat(victim); err != nil {
		t.Errorf("FR-20: the apply followed the symlink and removed %s, a file outside this backup set's root that no journal row ever named: %v", victim, err)
	}
}

// TestRetentionApplyEvidence_ARecordedPathOutsideTheRootIsRefusedNotFollowed
// is the traversal-shaped half of FR-20, reached the way a real deployment
// could reach it.
//
// model.NewArtifactID refuses a name carrying a separator, so the crafted
// "../secret" artifact name internal/retention's own unit tests build by
// hand is not constructible from here. The shape that IS constructible
// through the public journal API is the other one FR-20 names: a row whose
// recorded local path is not the path this backup set's root and this
// artifact's name compute. A hand-edited row, a restored database from
// another host, a bug in an older build, all produce it, and every one of
// them points the delete at a file this backup set does not own.
//
// The refusal is "positively identified" doing its job: the journal knows
// this artifact by a path, that path is not the one containment was proven
// about, and the two are not reconciled by guessing.
func TestRetentionApplyEvidence_ARecordedPathOutsideTheRootIsRefusedNotFollowed(t *testing.T) {
	ctx := context.Background()
	f := newRetentionEvidenceFixture(t, retentionEvidenceChain(false))

	f.seedDaysAgo(t, "forty-days-old.dump", 40, "outside every window")

	// A file somewhere else entirely, and a journal row for a managed
	// artifact of this backup set claiming that is where it lives.
	outside := t.TempDir()
	elsewhere := filepath.Join(outside, "someone-elses.dump")
	writeEvidenceFile(t, elsewhere, "a file this backup set does not own")
	strayed := seedCompleteArtifactAt(t, ctx, f.journal, f.bs, "strayed.dump", now().Add(-41*24*time.Hour), elsewhere)

	plan, err := f.svc.PreviewRetention(ctx, f.bs.ID.Source, f.bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	actions := previewedActions(plan)
	if !contains(actions["REFUSE"], strayed.Name) {
		t.Fatalf("the preview did not REFUSE %s, whose journal row records a local path outside this backup set's root; it said %+v", strayed.Name, plan.Verdicts)
	}
	wantRemoved := previewedDeletes(plan)
	if !equalStrings(wantRemoved, []string{"forty-days-old.dump"}) {
		t.Fatalf("precondition: the plan marks %v DELETE; want exactly [forty-days-old.dump], so this run has a real deletion beside the refusal", wantRemoved)
	}

	before := retentionEvidenceTree(t, f.root)
	if _, err := f.svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: f.bs.ID.Source, Set: f.bs.ID.Set, Actor: "evidence",
	}); err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	after := retentionEvidenceTree(t, f.root)

	for _, complaint := range retentionEvidenceCompare(before, after, wantRemoved) {
		t.Errorf("FR-20: %s", complaint)
	}
	if _, err := os.Lstat(elsewhere); err != nil {
		t.Errorf("FR-20: the apply followed a journal row pointing outside this backup set's root and removed %s: %v", elsewhere, err)
	}
}

// TestRetentionApplyEvidence_NothingIsRemovedWhenTheLastKnownGoodCopyIsGone
// is issue #602's own finding, at the boundary an operator reaches.
//
// FR-19's protection is computed from journal rows, so it goes on
// protecting the newest row after the file behind it has gone, while every
// older artifact is still selected for deletion on its age alone. Applying
// that plan empties the backup set, and the plan the operator confirmed
// says a restore point was kept. internal/retention refuses the whole pass
// now (pruneLastKnownGoodUnconfirmed); this is that refusal seen from
// here, on a real directory, which is where the cost of getting it wrong
// would have been paid.
func TestRetentionApplyEvidence_NothingIsRemovedWhenTheLastKnownGoodCopyIsGone(t *testing.T) {
	ctx := context.Background()
	f := newRetentionEvidenceFixture(t, retentionEvidenceChain(true))

	f.seedDaysAgo(t, "newest.dump", 40, "the newest restore point")
	f.seedDaysAgo(t, "older.dump", 90, "the only one still readable")

	// The newest artifact's file goes, out of band, with its journal row
	// left exactly as it was. Nothing in this product notices until FR-17
	// reconciliation runs.
	if err := os.Remove(filepath.Join(f.root, "newest.dump")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	plan, err := f.svc.PreviewRetention(ctx, f.bs.ID.Source, f.bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if got := previewedDeletes(plan); len(got) != 0 {
		t.Errorf("FR-30: the preview marks %v DELETE while this backup set's last known good has no readable copy. An operator confirming this plan is told a restore point is kept and gets an empty backup set. Verdicts: %+v", got, plan.Verdicts)
	}

	before := retentionEvidenceTree(t, f.root)
	if _, err := f.svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: f.bs.ID.Source, Set: f.bs.ID.Set, Actor: "evidence",
	}); err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	after := retentionEvidenceTree(t, f.root)

	if diff := describeEvidenceDifference(before, after); diff != "" {
		t.Errorf("FR-30: the apply changed this backup set while its last known good had no readable copy:\n%s", diff)
	}
	if len(after) == 0 {
		t.Error("FR-30: the backup set has no confirmed readable copy of anything at all after the apply")
	}
}

// --------------------------------------------------------------- helpers

// seedCompleteArtifactAt is seedCompleteArtifact with the recorded local
// path chosen by the caller instead of computed from the backup set's
// root, which is the only way to build the disagreement FR-20's
// "positively identified" check is about from outside internal/retention.
func seedCompleteArtifactAt(t *testing.T, ctx context.Context, journal *state.Journal, bs config.BackupSet, name string, discoveredAt time.Time, localPath string) model.ArtifactID {
	t.Helper()

	artifact, err := model.NewArtifactID(bs.ID, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%q): %v", name, err)
	}
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "discover-" + name,
		From:       "",
		To:         string(lifecycle.Discovered),
		OccurredAt: discoveredAt,
		RemotePath: "/backups/" + name,
	}); err != nil {
		t.Fatalf("RecordTransition(discover %s): %v", name, err)
	}
	lp := localPath
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "complete-" + name,
		From:       string(lifecycle.Discovered),
		To:         string(lifecycle.Complete),
		OccurredAt: discoveredAt,
		LocalPath:  &lp,
		Transfer:   &state.TransferResult{BytesTransferred: 1, Checksummed: true},
	}); err != nil {
		t.Fatalf("RecordTransition(complete %s): %v", name, err)
	}
	return artifact
}

func writeEvidenceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func copyEvidenceTree(in map[string]retentionEvidenceEntry) map[string]retentionEvidenceEntry {
	out := make(map[string]retentionEvidenceEntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// requireComplaintNaming is the control's own assertion: the comparison
// has to complain, and it has to name the path, because a complaint about
// something else is a red run for the wrong reason and reads identically.
func requireComplaintNaming(t *testing.T, complaints []string, path, what string) {
	t.Helper()
	if len(complaints) == 0 {
		t.Fatalf("the comparison reported nothing wrong about %s. It would therefore have passed against %s, which makes every other assertion in this file worthless.", path, what)
	}
	if !strings.Contains(strings.Join(complaints, "\n"), path) {
		t.Fatalf("the comparison complained, and about something other than %q, so it would have failed for the wrong reason against %s. Complaints: %v", path, what, complaints)
	}
}

// describeEvidenceDifference renders every way two snapshots differ, for
// the refusal cases whose whole claim is that there is no difference.
func describeEvidenceDifference(before, after map[string]retentionEvidenceEntry) string {
	var lines []string
	for path, was := range before {
		is, ok := after[path]
		switch {
		case !ok:
			lines = append(lines, fmt.Sprintf("  removed: %s (was %s)", path, was))
		case is != was:
			lines = append(lines, fmt.Sprintf("  changed: %s (was %s, is %s)", path, was, is))
		}
	}
	for path, is := range after {
		if _, ok := before[path]; !ok {
			lines = append(lines, fmt.Sprintf("  appeared: %s (%s)", path, is))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func sortedKeys(m map[string]retentionEvidenceEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

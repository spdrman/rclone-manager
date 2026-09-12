// The mutation-during-read harness (K0.4).
//
// One property, asserted once per way a source can move under a reader:
// every capture ends in a DETERMINISTIC outcome, and that outcome is either
// a bounded retry that settled or a capture that is not verified and makes
// the run incomplete. There is no third option, and in particular there is
// no path on which bytes this manager could not prove coherent are recorded
// as though they were.
//
// The cases are the ones a real source actually produces:
//
//   - the same number of bytes with different content, timestamp restored
//     (rsync --times, tar -p, an editor, a restore of the source itself);
//   - a change finer than a one-second timestamp;
//   - a write landing in the middle of our read, which is what tears a file;
//   - a rename onto the path, which is how every careful writer publishes;
//   - a rename away and a delete, which is an ordinary event in a live tree.
//
// The first case is the one with teeth, because no amount of metadata sees
// it. It is the case kopia v0.23.1's cached-entry heuristic gets wrong by
// construction (snapshot/upload/upload.go:694-720, metadataEquals: mtime,
// mode, owner, size, and no content), and it is why this package exists
// above the engine rather than trusting the engine's own answer.

package sourceconsistency

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// A file nobody touches is captured once, on the first attempt, and its
// digest is the digest of its content. This is the baseline the other cases
// are deviations from; without it, a harness that reported "incomplete" for
// everything would pass every test below.
func TestQuietFileIsCapturedOnTheFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "quiet", "the content")

	got := Reader{Source: OSSource{Root: dir}}.Capture(context.Background(), "quiet")

	if got.Outcome != OutcomeStable {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeStable)
	}
	if !got.Verified() {
		t.Fatal("a file nothing touched is not verified")
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	if got.Size != int64(len("the content")) {
		t.Errorf("size = %d, want %d", got.Size, len("the content"))
	}
	if got.Digest != sha256Hex([]byte("the content")) {
		t.Errorf("digest = %q, want the digest of the content", got.Digest)
	}
	if got.DigestAlg != DigestAlgorithm {
		t.Errorf("digest algorithm = %q, want %q", got.DigestAlg, DigestAlgorithm)
	}
}

// A write landing in the middle of the read tears the file. Size and mtime
// both move, so the reader's own post-read stat sees it; the retry is
// bounded and the second attempt, against a source now holding still,
// settles. What must never happen is the first attempt's half-old,
// half-new bytes being recorded as a verified capture.
func TestWriteDuringReadIsRetriedAndNeverRecordedTorn(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "torn", strings.Repeat("a", 4096))

	src := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1,
		afterChunk:   func() { rewrite(t, path, strings.Repeat("b", 8192)) },
	}

	got := Reader{Source: src, ChunkSize: 64}.Capture(context.Background(), "torn")

	if got.Outcome != OutcomeRetried {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeRetried)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one torn, one clean)", got.Attempts)
	}
	if got.Digest != sha256Hex([]byte(strings.Repeat("b", 8192))) {
		t.Error("the recorded digest is not the digest of the settled content")
	}
	if got.Reason == "" {
		t.Error("a retried capture has to say what moved")
	}
}

// A source that will not hold still exhausts the bounded retry and the
// capture is NOT verified. This is the case the acceptance criterion is
// really about: the alternative implementation - keep retrying - is an
// unbounded loop on a busy log file, and the other alternative - accept the
// last attempt - is silently storing a torn file.
func TestASourceThatNeverSettlesMarksTheRunIncomplete(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "busy", strings.Repeat("a", 4096))

	grown := 4096
	src := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1000,
		afterChunk: func() {
			grown += 4096
			rewrite(t, path, strings.Repeat("b", grown))
		},
	}

	got := Reader{Source: src, ChunkSize: 64, MaxAttempts: 3}.Capture(context.Background(), "busy")

	if got.Outcome != OutcomeIncomplete {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeIncomplete)
	}
	if got.Verified() {
		t.Fatal("a capture that never settled reports itself verified")
	}
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want exactly the 3 it was allowed", got.Attempts)
	}

	run := &Run{Mode: model.ModeLiveBestEffort}
	run.Record(got)
	if run.Complete() {
		t.Fatal("a run holding an unverified capture reports itself complete")
	}
	if len(run.IncompleteReasons()) != 1 {
		t.Errorf("IncompleteReasons() = %v, want exactly one", run.IncompleteReasons())
	}
}

// The adversarial case: same size, different content, modification time put
// back. No metadata moved, so nothing derived from metadata can see it. The
// defence is the confirm read, and this test is the proof that it works -
// the torn first read and the coherent second read disagree on the digest,
// which is a mutation whatever the timestamps say.
func TestSameSizeContentChangeWithRestoredMtimeIsCaughtByTheConfirmRead(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "sneaky", strings.Repeat("a", 4096))

	src := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1,
		afterChunk:   func() { rewritePreservingMetadata(t, path, strings.Repeat("b", 4096)) },
	}

	r := Reader{Source: src, ChunkSize: 64, ConfirmDigest: true}
	got := r.Capture(context.Background(), "sneaky")

	if got.Outcome != OutcomeRetried {
		t.Fatalf("outcome = %q (%s), want %q: the confirm read is the only thing that can see this", got.Outcome, got.Reason, OutcomeRetried)
	}
	if got.Digest != sha256Hex([]byte(strings.Repeat("b", 4096))) {
		t.Error("the recorded digest is not the digest of the settled content")
	}

	// And the honest half: without the confirm read the same mutation is
	// invisible. This is asserted rather than left implicit because it is
	// the exact cost of choosing the strong preset on a weak source, and
	// the ADR publishes it.
	rewritePreservingMetadata(t, path, strings.Repeat("a", 4096))
	blind := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1,
		afterChunk:   func() { rewritePreservingMetadata(t, path, strings.Repeat("b", 4096)) },
	}
	unconfirmed := Reader{Source: blind, ChunkSize: 64}.Capture(context.Background(), "sneaky")
	if unconfirmed.Outcome != OutcomeStable {
		t.Fatalf("without a confirm read the outcome was %q (%s); this test's premise is that metadata cannot see this mutation", unconfirmed.Outcome, unconfirmed.Reason)
	}
}

// A change finer than one second. On a filesystem with nanosecond
// timestamps the reader's own window check sees it; the point of the test is
// that the reader compares timestamps at full resolution rather than at the
// resolution a backend claims, because the claim is about what the backend
// REPORTS across runs and has nothing to do with the two stats this reader
// takes seconds apart.
func TestSubSecondChangeInsideTheReadWindowIsDetected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "subsecond", strings.Repeat("a", 4096))

	if !subSecondTimestampsAvailable(t, path) {
		t.Skip("this filesystem rounds modification times to the second, so a sub-second change cannot be staged on it")
	}

	src := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1,
		afterChunk:   func() { bumpModTime(t, path, 3*time.Millisecond) },
	}

	got := Reader{Source: src, ChunkSize: 64}.Capture(context.Background(), "subsecond")

	if got.Outcome != OutcomeRetried {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeRetried)
	}
	if !strings.Contains(got.Reason, "modification time") {
		t.Errorf("reason = %q, want it to name the modification time", got.Reason)
	}
}

// A rename ONTO the path is how every careful writer publishes a file, so it
// is not an error - but the bytes read through the old descriptor are the
// old file's, and recording them under the new file's stat would be a
// fabrication. The reader notices that the path no longer names what it
// opened and retries, and the retry captures the new file.
func TestRenameOntoThePathIsRetriedAndCapturesTheNewFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "published", "the old content")
	replacement := writeFile(t, dir, "published.tmp", "the new content")

	src := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1,
		afterOpen: func() {
			if err := os.Rename(replacement, path); err != nil {
				t.Fatalf("rename: %v", err)
			}
		},
	}

	got := Reader{Source: src, ChunkSize: 4}.Capture(context.Background(), "published")

	if got.Outcome != OutcomeRetried {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeRetried)
	}
	if got.Digest != sha256Hex([]byte("the new content")) {
		t.Error("the retry did not capture the file that is now at the path")
	}
}

// A rename AWAY and a delete are the same fact to a reader: the path no
// longer names anything. Both are ordinary in a live tree and neither is
// verified, because this manager did not capture a file that is not there.
// The run says so rather than quietly containing one fewer file than the
// operator's selection.
func TestRenameAwayAndDeleteLeaveAVanishedCaptureAndAnIncompleteRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(t *testing.T, dir, path string)
	}{
		{
			name: "renamed away",
			remove: func(t *testing.T, dir, path string) {
				if err := os.Rename(path, filepath.Join(dir, "elsewhere")); err != nil {
					t.Fatalf("rename: %v", err)
				}
			},
		},
		{
			name: "deleted",
			remove: func(t *testing.T, _, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeFile(t, dir, "going", strings.Repeat("a", 4096))

			src := &hookSource{
				inner:        OSSource{Root: dir},
				untilAttempt: 1000,
				afterChunk:   func() { tc.remove(t, dir, path) },
			}

			got := Reader{Source: src, ChunkSize: 64}.Capture(context.Background(), "going")

			if got.Outcome != OutcomeVanished {
				t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeVanished)
			}
			if got.Verified() {
				t.Fatal("a file that is no longer there reports itself verified")
			}

			run := &Run{Mode: model.ModeLiveBestEffort}
			run.Record(got)
			if run.Complete() {
				t.Fatal("a run that could not capture a selected file reports itself complete")
			}
		})
	}
}

// A file that was gone before the reader ever looked is the same outcome,
// reached without an open. Worth its own case because the code path is
// different (the first stat fails rather than the last one) and a harness
// that only handled the mid-read variant would report "incomplete" here
// with a reason about reading a file it never opened.
func TestAFileGoneBeforeTheReadIsVanishedNotUnreadable(t *testing.T) {
	dir := t.TempDir()

	got := Reader{Source: OSSource{Root: dir}}.Capture(context.Background(), "never-existed")

	if got.Outcome != OutcomeVanished {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeVanished)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: there is nothing to retry", got.Attempts)
	}
}

// Retries are bounded by attempts, but a cancelled context has to stop the
// reader immediately rather than spend its whole allowance on a run nobody
// is waiting for. The outcome is still deterministic and still not verified.
func TestACancelledContextStopsTheCaptureUnverified(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "quiet", strings.Repeat("a", 4096))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := Reader{Source: OSSource{Root: dir}, ChunkSize: 64}.Capture(ctx, "quiet")

	if got.Verified() {
		t.Fatalf("outcome = %q: a cancelled capture reports itself verified", got.Outcome)
	}
	if got.Outcome != OutcomeIncomplete {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeIncomplete)
	}
}

// The path a capture is asked for is untrusted the same way every remote
// name in this codebase is untrusted: it comes off a directory listing. A
// Source rooted at the operator's selection must refuse to read outside it
// rather than following "../" into the rest of the host.
func TestOSSourceRefusesToLeaveItsRoot(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside-the-root")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	src := OSSource{Root: dir}
	if _, err := src.Stat("../outside-the-root"); err == nil {
		t.Fatal("Stat followed a path out of the root")
	}
	if _, err := src.Open("../outside-the-root"); err == nil {
		t.Fatal("Open followed a path out of the root")
	}

	got := Reader{Source: src}.Capture(context.Background(), "../outside-the-root")
	if got.Verified() {
		t.Fatalf("outcome = %q: a path out of the root was captured as verified", got.Outcome)
	}
}

// Whether a confirm read happens is a decision about the mode and the
// policy, not a knob a caller sets by hand at each call site. Under a mode
// that guarantees a point in time there is nothing to confirm: the source
// cannot change, and a second full read would double the I/O of every run
// for no information.
func TestConfirmReadIsRequiredExactlyWhereItBuysSomething(t *testing.T) {
	always := model.VerificationPolicyFor(model.TrustWeak, model.PresetConservative)
	sampled := model.VerificationPolicyFor(model.TrustWeak, model.PresetStrong)

	for _, tc := range []struct {
		mode model.ConsistencyMode
		pol  model.VerificationPolicy
		want bool
	}{
		{model.ModeLiveBestEffort, always, true},
		{model.ModeExternallyQuiesced, always, true},
		{model.ModeExternalSnapshot, always, false},
		{model.ModeLiveBestEffort, sampled, false},
	} {
		if got := ConfirmReadRequired(tc.mode, tc.pol); got != tc.want {
			t.Errorf("ConfirmReadRequired(%s, %s) = %v, want %v", tc.mode, tc.pol.Mode, got, tc.want)
		}
	}

	r := NewReader(OSSource{Root: t.TempDir()}, model.ModeLiveBestEffort, always)
	if !r.ConfirmDigest {
		t.Error("NewReader did not arm the confirm read for a live source under an always-verify policy")
	}
	if r.MaxAttempts <= 0 {
		t.Error("NewReader left the retry bound unset, which is an unbounded retry")
	}
}

// A mutation seen under a mode that promised it could not happen is the
// promise being broken, and the run reports it separately from its own
// completeness. An operator who quiesced the wrong service has a working
// backup and a false belief, and the second one is what this tells them.
func TestMutationsUnderAPromisedModeAreReportedAsContractViolations(t *testing.T) {
	moved := Capture{Path: "moved", Outcome: OutcomeRetried, Attempts: 2, Reason: "size changed"}

	live := &Run{Mode: model.ModeLiveBestEffort}
	live.Record(moved)
	if got := live.ContractViolations(); len(got) != 0 {
		t.Errorf("live_best_effort reported %v as a violation; a live source changing is the mode, not a fault", got)
	}
	if !live.Complete() {
		t.Error("a run whose only capture settled on retry is not complete")
	}

	quiesced := &Run{Mode: model.ModeExternallyQuiesced}
	quiesced.Record(moved)
	if got := quiesced.ContractViolations(); len(got) != 1 {
		t.Fatalf("ContractViolations() = %v, want exactly one", got)
	}
	if !quiesced.Complete() {
		t.Error("the capture settled, so the run is complete even though the quiesce claim was false")
	}
}

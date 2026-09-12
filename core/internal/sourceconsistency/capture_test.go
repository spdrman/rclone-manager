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
//   - a rename away and a delete, which is an ordinary event in a live tree;
//   - a source that stops answering, which is what a hung remote is.
//
// The first case is the one with teeth, because no amount of metadata sees
// it. It is the case kopia v0.23.1's cached-entry heuristic gets wrong by
// construction (snapshot/upload/upload.go:694-720, metadataEquals: mtime,
// mode, owner, size, and no content), and it is why this package exists
// above the engine rather than trusting the engine's own answer.

package sourceconsistency

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// liveRun is a run under the mode that promises the least and the policy
// that trusts the least, which is the right baseline for the completeness
// assertions below: they are about what the captures prove, not about what
// the operator arranged.
func liveRun(mode model.ConsistencyMode) *Run {
	return NewRun(mode, model.TrustWeak, model.VerificationPolicyFor(model.TrustWeak, model.PresetConservative))
}

// A file nobody touches is captured once, on the first attempt, and its
// digest is the digest of its content. This is the baseline the other cases
// are deviations from; without it, a harness that reported "incomplete" for
// everything would pass every test below.
func TestQuietFileIsCapturedOnTheFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "quiet", "the content")

	r := NewReader(OSSource{Root: dir}, model.ModeLiveBestEffort, ReaderOptions{})
	got := r.Capture(context.Background(), "quiet")

	if got.Outcome != OutcomeStable {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeStable)
	}
	if !got.Verified() {
		t.Fatal("a file nothing touched is not verified")
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	if got.Kind != KindRegular {
		t.Errorf("kind = %q, want %q", got.Kind, KindRegular)
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

	r := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64})
	got := r.Capture(context.Background(), "torn")

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

	r := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64, MaxAttempts: 3})
	got := r.Capture(context.Background(), "busy")

	if got.Outcome != OutcomeIncomplete {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeIncomplete)
	}
	if got.Verified() {
		t.Fatal("a capture that never settled reports itself verified")
	}
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want exactly the 3 it was allowed", got.Attempts)
	}

	run := liveRun(model.ModeLiveBestEffort)
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

	r := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64})
	if !r.ConfirmsDigest() {
		t.Fatal("a live source's reader did not arm the confirm read; nothing below can be detected without it")
	}

	got := r.Capture(context.Background(), "sneaky")

	if got.Outcome != OutcomeRetried {
		t.Fatalf("outcome = %q (%s), want %q: the confirm read is the only thing that can see this", got.Outcome, got.Reason, OutcomeRetried)
	}
	if got.Digest != sha256Hex([]byte(strings.Repeat("b", 4096))) {
		t.Error("the recorded digest is not the digest of the settled content")
	}

	// And the honest half: the ONE mode that switches the confirm read off
	// is the one that claims the source cannot change at all, and this is
	// what a false claim costs. It is asserted rather than left implicit
	// because it is now the whole residual risk of the design, and the ADR
	// publishes it.
	rewritePreservingMetadata(t, path, strings.Repeat("a", 4096))
	blind := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1,
		afterChunk:   func() { rewritePreservingMetadata(t, path, strings.Repeat("b", 4096)) },
	}

	frozen := NewReader(blind, model.ModeExternalSnapshot, ReaderOptions{ChunkSize: 64})
	if frozen.ConfirmsDigest() {
		t.Fatal("a frozen image's reader armed the confirm read; a second pass over an image that cannot change buys nothing")
	}

	unconfirmed := frozen.Capture(context.Background(), "sneaky")
	if unconfirmed.Outcome != OutcomeStable {
		t.Fatalf("without a confirm read the outcome was %q (%s); this test's premise is that metadata cannot see this mutation", unconfirmed.Outcome, unconfirmed.Reason)
	}
}

// The confirm read is armed by the MODE and by nothing else, and this is the
// case that says why. Under a policy that lets metadata skip content - the
// sampled policy a weak source gets under the trust-metadata preset - most
// paths are not read at all, and the ones that ARE read are the sample: the
// only thing standing between that source and a silently stale restore
// point. An earlier version of this package switched the confirm read off
// for exactly those paths, on the theory that an operator who asked to read
// less should not be charged twice, which left the sample unable to detect
// the one mutation it exists to catch.
//
// So: a torn in-place rewrite with a restored timestamp, under a policy that
// may skip content, on a path that was read. It must never be banked as
// verified content.
func TestAPathReadUnderASkipPolicyIsStillDigestConfirmed(t *testing.T) {
	pol := model.VerificationPolicyFor(model.TrustWeak, model.PresetTrustMetadata)
	if !pol.MetadataMaySkipContent() {
		t.Fatalf("this test's premise is a policy that may skip content; %q may not", pol.Mode)
	}

	dir := t.TempDir()
	path := writeFile(t, dir, "sampled", strings.Repeat("a", 4096))

	settling := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1,
		afterChunk:   func() { rewritePreservingMetadata(t, path, strings.Repeat("b", 4096)) },
	}

	r := NewReader(settling, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64})
	got := r.Capture(context.Background(), "sampled")

	if got.Outcome != OutcomeRetried {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeRetried)
	}
	if got.Digest == sha256Hex([]byte(strings.Repeat("a", 4096))) {
		t.Fatal("the torn first read was banked as the capture's content under a metadata-may-skip policy")
	}
	if got.Digest != sha256Hex([]byte(strings.Repeat("b", 4096))) {
		t.Errorf("digest = %q, want the digest of the settled content", got.Digest)
	}

	// And the same mutation on every attempt: not verified, and the run says
	// so. The alternative - a capture whose digest is a torn read - is a
	// restore point that reports itself whole.
	rewritePreservingMetadata(t, path, strings.Repeat("a", 4096))
	relentless := &hookSource{
		inner:        OSSource{Root: dir},
		untilAttempt: 1000,
		afterChunk: func() {
			if strings.HasPrefix(readAll(t, path), "a") {
				rewritePreservingMetadata(t, path, strings.Repeat("b", 4096))
			} else {
				rewritePreservingMetadata(t, path, strings.Repeat("a", 4096))
			}
		},
	}

	never := NewReader(relentless, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64, MaxAttempts: 3}).
		Capture(context.Background(), "sampled")

	if never.Verified() {
		t.Fatalf("outcome = %q (%s): a file rewritten under every attempt was banked as verified", never.Outcome, never.Reason)
	}
	if never.Outcome != OutcomeIncomplete {
		t.Fatalf("outcome = %q (%s), want %q", never.Outcome, never.Reason, OutcomeIncomplete)
	}

	run := liveRun(model.ModeLiveBestEffort)
	run.Record(never)
	if run.Complete() {
		t.Fatal("a run holding a file it could not prove reports itself complete")
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

	r := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64})
	got := r.Capture(context.Background(), "subsecond")

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

	r := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 4})
	got := r.Capture(context.Background(), "published")

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

			r := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64})
			got := r.Capture(context.Background(), "going")

			if got.Outcome != OutcomeVanished {
				t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeVanished)
			}
			if got.Verified() {
				t.Fatal("a file that is no longer there reports itself verified")
			}

			run := liveRun(model.ModeLiveBestEffort)
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

	r := NewReader(OSSource{Root: dir}, model.ModeLiveBestEffort, ReaderOptions{})
	got := r.Capture(context.Background(), "never-existed")

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

	r := NewReader(OSSource{Root: dir}, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64})
	got := r.Capture(ctx, "quiet")

	if got.Verified() {
		t.Fatalf("outcome = %q: a cancelled capture reports itself verified", got.Outcome)
	}
	if got.Outcome != OutcomeUnreadable {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeUnreadable)
	}
}

// A source that stops answering mid-read is the case a bound on attempts
// cannot help with: the reader is not retrying, it is waiting, and nothing
// it knows about will ever come back. This is an ordinary remote failure -
// an SSH session whose TCP connection is black-holed, an object store
// holding a request open - and the only thing that gets the run back is the
// context reaching the read.
//
// So the read I/O takes a context, and this test is what holds it there: a
// source whose Read blocks until ctx is done, cancelled from outside, and a
// Capture that returns promptly with an unverified outcome. Without the
// context in Read there is nothing for a test to assert, because Capture
// never returns at all.
func TestACancelledContextUnblocksAHungRead(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hung", strings.Repeat("a", 4096))

	src := &blockingSource{inner: OSSource{Root: dir}, opened: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan Capture, 1)
	go func() {
		r := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{ChunkSize: 64})
		done <- r.Capture(ctx, "hung")
	}()

	select {
	case <-src.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader never opened the file")
	}

	select {
	case got := <-done:
		t.Fatalf("Capture returned %q (%s) before the context was cancelled; this test's premise is a read that never returns on its own", got.Outcome, got.Reason)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case got := <-done:
		if got.Verified() {
			t.Fatalf("outcome = %q: a capture abandoned mid-read reports itself verified", got.Outcome)
		}
		if got.Outcome != OutcomeUnreadable {
			t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeUnreadable)
		}
		if !strings.Contains(got.Reason, context.Canceled.Error()) {
			t.Errorf("reason = %q, want it to name the cancellation", got.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled context did not unblock a hung read; the context is not reaching the source's I/O")
	}
}

// A symlink has no content this reader captures, and saying so is a
// different answer from failing to capture it.
//
// The bug this pins is specific: Stat lstats the path (a symlink is a
// symlink) and a read through it would fstat the TARGET, so the reader's own
// identity check saw two different objects, retried three times and reported
// every symlink in the tree as an incomplete capture. That is a false alarm
// on an ordinary tree and it would hide the real ones.
//
// The answer is to name what is there and leave the decision where it
// belongs: whether a symlink is stored, followed or skipped is the backend
// capability matrix's symlink_semantics decision, not a content reader's.
func TestASymlinkIsNamedNotReadThroughAndDoesNotFailTheRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "target", "the target's content")
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("this platform will not create a symlink: %v", err)
	}

	r := NewReader(OSSource{Root: dir}, model.ModeLiveBestEffort, ReaderOptions{})
	got := r.Capture(context.Background(), "link")

	if got.Outcome != OutcomeNotAFile {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeNotAFile)
	}
	if got.Kind != KindSymlink {
		t.Errorf("kind = %q, want %q", got.Kind, KindSymlink)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a symlink is not a file that failed to hold still", got.Attempts)
	}
	if got.Verified() {
		t.Error("a symlink reports itself as verified content")
	}
	if got.Digest != "" || got.Size != 0 {
		t.Errorf("the reader read through the symlink: digest %q, size %d", got.Digest, got.Size)
	}
	if got.Digest == sha256Hex([]byte("the target's content")) {
		t.Error("the capture holds the target's content, which is the symlink decision made by accident")
	}

	run := liveRun(model.ModeExternallyQuiesced)
	run.Record(got)
	if !run.Complete() {
		t.Fatalf("a symlink made the run incomplete: %v", run.IncompleteReasons())
	}
	if got := run.ContractViolations(); len(got) != 0 {
		t.Errorf("a symlink was reported as a mutation under a promised mode: %v", got)
	}

	// A directory reaches the same answer by the same route, which is what
	// makes the outcome about "there is no content here" rather than about
	// symlinks specifically.
	if dirCapture := r.Capture(context.Background(), "."); dirCapture.Outcome != OutcomeNotAFile || dirCapture.Kind != KindDir {
		t.Errorf("a directory gave outcome %q kind %q (%s), want %q/%q", dirCapture.Outcome, dirCapture.Kind, dirCapture.Reason, OutcomeNotAFile, KindDir)
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
	ctx := context.Background()
	if _, err := src.Stat(ctx, "../outside-the-root"); err == nil {
		t.Fatal("Stat followed a path out of the root")
	}
	if _, err := src.Open(ctx, "../outside-the-root"); err == nil {
		t.Fatal("Open followed a path out of the root")
	}

	got := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{}).Capture(ctx, "../outside-the-root")
	if got.Verified() {
		t.Fatalf("outcome = %q: a path out of the root was captured as verified", got.Outcome)
	}
}

// A source with no root at all reads whatever the process's working
// directory happens to contain, because filepath.Join("", "x") is "x" and
// the lexical containment check would then pass every path in the run. The
// refusal is by sentinel error so a caller can tell a misconfigured source
// from a hostile path.
func TestOSSourceWithNoRootRefusesEveryPath(t *testing.T) {
	src := OSSource{}
	ctx := context.Background()

	if _, err := src.Stat(ctx, "go.mod"); !errors.Is(err, ErrNoRoot) {
		t.Errorf("Stat error = %v, want %v", err, ErrNoRoot)
	}
	if _, err := src.Open(ctx, "go.mod"); !errors.Is(err, ErrNoRoot) {
		t.Errorf("Open error = %v, want %v", err, ErrNoRoot)
	}

	got := NewReader(src, model.ModeLiveBestEffort, ReaderOptions{}).Capture(ctx, "go.mod")
	if got.Verified() {
		t.Fatalf("outcome = %q: a rootless source captured a path as verified", got.Outcome)
	}
	if got.Outcome != OutcomeUnreadable {
		t.Errorf("outcome = %q (%s), want %q", got.Outcome, got.Reason, OutcomeUnreadable)
	}
}

// Whether a confirm read happens is a decision about the mode, and about
// nothing else. It is off under exactly one mode - the one that says the
// source cannot change while it is read - and on everywhere else, including
// under every policy that lets metadata skip content, because a path that
// IS read has to be proven whatever made it eligible to be skipped.
func TestConfirmReadIsRequiredUnlessTheSourceCannotChange(t *testing.T) {
	for _, tc := range []struct {
		mode model.ConsistencyMode
		want bool
	}{
		{model.ModeLiveBestEffort, true},
		{model.ModeExternallyQuiesced, true},
		{model.ModeExternalSnapshot, false},
		{model.ConsistencyMode("something new"), true},
	} {
		if got := ConfirmReadRequired(tc.mode); got != tc.want {
			t.Errorf("ConfirmReadRequired(%s) = %v, want %v", tc.mode, got, tc.want)
		}
	}

	live := NewReader(OSSource{Root: t.TempDir()}, model.ModeLiveBestEffort, ReaderOptions{})
	if !live.ConfirmsDigest() {
		t.Error("a live source's reader did not arm the confirm read")
	}
	if live.MaxAttempts() <= 0 {
		t.Error("NewReader left the retry bound unset, which is an unbounded retry")
	}

	frozen := NewReader(OSSource{Root: t.TempDir()}, model.ModeExternalSnapshot, ReaderOptions{})
	if frozen.ConfirmsDigest() {
		t.Error("a frozen image's reader armed a second pass that cannot see anything")
	}
}

// A mutation seen under a mode that promised it could not happen is the
// promise being broken, and the run reports it separately from its own
// completeness. An operator who quiesced the wrong service has a working
// backup and a false belief, and the second one is what this tells them.
//
// An I/O failure is NOT such a mutation, and that is the other half of this
// case: a file this manager could not read says nothing about whether
// anybody wrote to the source, so it makes the run incomplete and leaves the
// quiesce claim alone. Folding the two together reported a permission error
// as evidence that the operator's arrangement had failed.
func TestMutationsUnderAPromisedModeAreReportedAsContractViolations(t *testing.T) {
	moved := Capture{Path: "moved", Outcome: OutcomeRetried, Attempts: 2, Reason: "size changed"}
	unreadable := Capture{Path: "locked", Outcome: OutcomeUnreadable, Attempts: 1, Reason: "permission denied"}

	live := liveRun(model.ModeLiveBestEffort)
	live.Record(moved)
	if got := live.ContractViolations(); len(got) != 0 {
		t.Errorf("live_best_effort reported %v as a violation; a live source changing is the mode, not a fault", got)
	}
	if !live.Complete() {
		t.Error("a run whose only capture settled on retry is not complete")
	}

	quiesced := liveRun(model.ModeExternallyQuiesced)
	quiesced.Record(moved)
	if got := quiesced.ContractViolations(); len(got) != 1 {
		t.Fatalf("ContractViolations() = %v, want exactly one", got)
	}
	if !quiesced.Complete() {
		t.Error("the capture settled, so the run is complete even though the quiesce claim was false")
	}

	quiesced.Record(unreadable)
	if got := quiesced.ContractViolations(); len(got) != 1 {
		t.Errorf("ContractViolations() = %v: an unreadable file was reported as a mutation", got)
	}
	if quiesced.Complete() {
		t.Error("a run holding a file it could not read reports itself complete")
	}
}

// A run report is the record of what a run proved. A caller that was handed
// the run's own slices could append to that record, or shorten it, after the
// run was over - and the bug that produces is a report that says something
// the run never observed, which is the one thing this package exists to
// prevent.
func TestRunAccessorsDoNotHandOutTheRunsOwnState(t *testing.T) {
	run := liveRun(model.ModeExternallyQuiesced)
	run.Record(Capture{Path: "a", Outcome: OutcomeRetried, Attempts: 2, Reason: "size changed"})
	run.Record(Capture{Path: "b", Outcome: OutcomeIncomplete, Attempts: 3, Reason: "never settled"})

	captures := run.Captures()
	captures[0].Path = "not-a"
	captures[0].Outcome = OutcomeStable

	reasons := run.IncompleteReasons()
	reasons[0] = "nothing to see here"

	violations := run.ContractViolations()
	violations[0] = "nothing to see here"

	if got := run.Captures(); got[0].Path != "a" || got[0].Outcome != OutcomeRetried {
		t.Errorf("a caller rewrote the run's captures: %+v", got[0])
	}
	if got := run.IncompleteReasons(); !strings.HasPrefix(got[0], "b: ") {
		t.Errorf("a caller rewrote the run's incomplete reasons: %v", got)
	}
	if got := run.ContractViolations(); !strings.HasPrefix(got[0], "a ") {
		t.Errorf("a caller rewrote the run's contract violations: %v", got)
	}
	if run.Mode() != model.ModeExternallyQuiesced {
		t.Errorf("Mode() = %q, want %q", run.Mode(), model.ModeExternallyQuiesced)
	}
	if run.Trust() != model.TrustWeak {
		t.Errorf("Trust() = %q, want %q", run.Trust(), model.TrustWeak)
	}
	if run.Policy().Mode != model.VerifyAlways {
		t.Errorf("Policy().Mode = %q, want %q", run.Policy().Mode, model.VerifyAlways)
	}
}

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Issue #662, defect 3: the dead end. This is why #662 is a bug report and
// not a support question.
//
// Once defect 1 has written an empty record over a good file, the product
// has no way out of the state it put itself in. Measured on the deployment,
// each verb in turn:
//
//	retry               FAILED -> DISCOVERED, the cycle hits the FR-12
//	                    final-name collision, FAILED again. Loops.
//	validate            "... is FAILED, not a durable restore point
//	                    (COMMITTED, REMOTE_DELETE_PENDING, COMPLETE or
//	                    REMOTE_RETAINED)"
//	quarantine reinstate unreachable: RetryQuarantinedIngestion accepts only
//	                    QUARANTINED/QUARANTINED_LOST, and retry has already
//	                    moved the artifact to FAILED
//	reconcile           re-derives the same verdict from the same records
//
// The remedy that fits is `quarantine reinstate` -- "I looked at the file,
// it is good, believe it again" -- and it is exactly the one the natural
// first move takes away. On a NAS, which is what this product is for, the
// only remaining option was moving files by hand from a shell that is not
// supposed to exist.
//
// # What these tests do NOT decide
//
// Nothing here names a remedy. The maintainer may widen `validate` to
// accept the FAILED-after-collision case, may make `retry` non-destructive
// of the `reinstate` option, may have the collision refusal name the verb
// that resolves it, or may fix defect 1 so the record is never empty in the
// first place. Every one of those turns these green. What is pinned is the
// property the issue asks for: every state a documented verb can reach has
// an exit, and a verb must not silently take away a recovery option that
// was available before it.
//
// # The fence
//
// The FR-12 collision refusal is correct and must not be weakened; that is
// pinned next door, in internal/lifecycle's TestTransfer_StillRefusesA
// StrayFileAtTheFinalName. A "fix" that overwrote the file would satisfy
// this file and must fail that one.

// deadEndFixture is issue #662's measured state: a good 294-byte file at
// the artifact's final name, a journal row that records it as zero bytes
// with the sha256 of nothing, and the artifact quarantined.
type deadEndFixture struct {
	svc      *Service
	journal  Journal
	artifact model.ArtifactID
	localDir string
	final    string

	// tr is the fake remote the fixture's artifact was discovered from.
	// The cases about the remote-hash comparison need it: that comparison
	// is the only evidence in the recovery path the local file did not
	// itself produce, so a test about it has to be able to say what the
	// remote holds.
	tr *fakeTransport

	// diskSize and diskHash are what is really on disk, measured. Every
	// failure message quotes them, because the whole defect is that the
	// product never does.
	diskSize int64
	diskHash string
}

// payload662 is 294 bytes, the size of dpkg.diversions.5.gz in the issue.
var payload662 = func() []byte {
	b := make([]byte, 294)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}()

// sha256OfNothing is the checksum the empty read-back recorded over that
// 294-byte file, in both the journal and the sidecar manifest.
const sha256OfNothing = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// newDeadEndFixture walks one artifact along real lifecycle edges to
// QUARANTINED, with the record the empty read-back would have left and the
// file the copy actually wrote.
//
// honestRecord chooses between the two rows: false is #662's, where the
// transfer result says 0 bytes and the recorded hash is the sha256 of
// nothing; true records what the file really is. The second is the positive
// control for every case in this file, and it is not decoration: "no verb
// resolves this" is trivially true of a fixture nothing could resolve, so
// the same fixture with the record honest has to be resolvable today.
func newDeadEndFixture(t *testing.T, honestRecord bool) deadEndFixture {
	t.Helper()
	ctx := context.Background()
	localDir := t.TempDir()

	bs := testBackupSet(t, localDir)
	// The field artifact is a rotated dpkg file, and its backup set is
	// declared read_only, which is why reconciliation reached it through
	// the REMOTE_RETAINED row rather than COMMITTED.
	bs.Include = []string{"*.gz"}
	bs.ReadOnly = true

	source := transport.Source{ID: "662-nas"}
	tr := newFakeTransport()
	tr.put("dpkg.diversions.5.gz", string(payload662), epoch.Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	final, err := lifecycle.FinalArtifactPath(localDir, rec.Artifact)
	if err != nil {
		t.Fatalf("FinalArtifactPath: %v", err)
	}
	mustWriteFile(t, final, string(payload662))
	sum := sha256.Sum256(payload662)
	diskHash := hex.EncodeToString(sum[:])
	diskSize := int64(len(payload662))
	if diskSize == 0 {
		t.Fatal("fixture wrote an empty file; a test about a good file under an empty record needs a good file")
	}

	recordedSize := int64(0)
	recordedHash := sha256OfNothing
	if honestRecord {
		recordedSize = diskSize
		recordedHash = diskHash
	}

	// Every edge below is one the FR-10 table declares, walked in order,
	// so the row this file argues about is one the real pipeline can
	// actually produce. REMOTE_RETAINED is the read-only lineage, and it
	// is the state reconciliation quarantined from in the field.
	steps := []struct {
		from, to  lifecycle.State
		localPath *string
		transfer  *state.TransferResult
		hashes    *state.HashUpdate
	}{
		{from: lifecycle.Discovered, to: lifecycle.Transferring},
		{from: lifecycle.Transferring, to: lifecycle.Transferred, transfer: &state.TransferResult{BytesTransferred: recordedSize}},
		{from: lifecycle.Transferred, to: lifecycle.Verifying},
		{from: lifecycle.Verifying, to: lifecycle.Verified, hashes: &state.HashUpdate{Hash: recordedHash, Alg: string(transport.SHA256)}},
		{from: lifecycle.Verified, to: lifecycle.Committing},
		{from: lifecycle.Committing, to: lifecycle.Committed, localPath: &final},
		{from: lifecycle.Committed, to: lifecycle.RemoteRetained},
		{from: lifecycle.RemoteRetained, to: lifecycle.Quarantined},
	}
	for i, s := range steps {
		if _, err := journal.RecordTransition(ctx, state.Transition{
			Artifact:   rec.Artifact,
			Key:        fmt.Sprintf("662-fixture-%d-%s", i, s.to),
			From:       string(s.from),
			To:         string(s.to),
			LocalPath:  s.localPath,
			Transfer:   s.transfer,
			Hashes:     s.hashes,
			Detail:     "issue #662 fixture",
			OccurredAt: epoch.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("fixture: %s -> %s: %v", s.from, s.to, err)
		}
	}

	if got := stateOf662(t, journal, rec.Artifact); got != string(lifecycle.Quarantined) {
		t.Fatalf("precondition: fixture is %s, want QUARANTINED", got)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("precondition: the good file is not at the final name: %v", err)
	}

	return deadEndFixture{
		svc: svc, journal: journal, artifact: rec.Artifact,
		localDir: localDir, final: final, tr: tr, diskSize: diskSize, diskHash: diskHash,
	}
}

// TestIssue662_SomeDocumentedVerbResolvesAnEmptyRecordOverAGoodFile is issue
// #662's defect 3.
//
// It walks the verbs in the order the operator did, records what each one
// answered, and asks only that ONE of them ended with the artifact back at
// a durable restore point with its file intact. Which one is not this
// test's business.
func TestIssue662_SomeDocumentedVerbResolvesAnEmptyRecordOverAGoodFile(t *testing.T) {
	cases := []struct {
		name         string
		honestRecord bool
	}{
		{
			// #662's measured state.
			name:         "an empty record over a good file at the final name",
			honestRecord: false,
		},
		{
			// The positive control: the identical fixture, quarantined
			// the same way, with the record describing the file. Today
			// `quarantine reinstate` resolves this in one call, which is
			// what proves the walk below can succeed at all and that the
			// case above fails for the defect rather than for the
			// fixture.
			name:         "control: the same quarantined artifact with an honest record",
			honestRecord: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDeadEndFixture(t, tc.honestRecord)
			ctx := context.Background()

			var transcript []string
			note := func(format string, args ...any) {
				transcript = append(transcript, fmt.Sprintf(format, args...))
			}
			resolved := func() bool {
				return durable(lifecycle.State(stateOf662(t, fx.journal, fx.artifact)))
			}

			// 1. Look before deciding. Writes nothing either way.
			if res, err := fx.svc.RevalidateQuarantined(ctx, fx.artifact); err != nil {
				note("quarantine revalidate: refused: %v", err)
			} else {
				note("quarantine revalidate: checked=%v passed=%v: %s", res.Checked, res.Passed, res.Reason)
			}

			// 2. The verb that fits: keep the local copy, trust it again.
			if res, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "issue #662"); err != nil {
				note("quarantine reinstate: refused: %v", err)
			} else {
				note("quarantine reinstate: reinstated=%v passed=%v: %s", res.Reinstated, res.Passed, res.Reason)
			}
			if resolved() {
				return
			}

			// 3. The operator-facing check.
			if _, err := fx.svc.ValidateArtifact(ctx, fx.artifact, ValidateOptions{}); err != nil {
				note("validate: refused: %v", err)
			} else {
				note("validate: ran")
			}
			if resolved() {
				return
			}

			// 4. The natural first move, and the one-way door. Two cycles,
			//    because #662's own measurement is that it loops: the
			//    second is what shows the first was not merely slow.
			if err := fx.svc.RetryQuarantinedIngestion(ctx, fx.artifact); err != nil {
				note("quarantine retry: refused: %v", err)
			} else {
				note("quarantine retry: %s -> %s", lifecycle.Quarantined, stateOf662(t, fx.journal, fx.artifact))
			}
			for cycle := 1; cycle <= 2; cycle++ {
				fx.svc.RunCycle(ctx)
				note("cycle %d: artifact is %s; last failure: %s",
					cycle, stateOf662(t, fx.journal, fx.artifact), failureReason662(t, fx.svc, fx.artifact))
				if resolved() {
					return
				}
			}

			// 5. Everything the FAILED state still offers.
			if err := fx.svc.RetryFailedIngestion(ctx, fx.artifact, "issue #662"); err != nil {
				note("retry: refused: %v", err)
			} else {
				note("retry: FAILED -> %s", stateOf662(t, fx.journal, fx.artifact))
			}
			if _, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "issue #662"); err != nil {
				note("quarantine reinstate (again): refused: %v", err)
			} else {
				note("quarantine reinstate (again): accepted")
			}
			if _, err := fx.svc.ValidateArtifact(ctx, fx.artifact, ValidateOptions{}); err != nil {
				note("validate (again): refused: %v", err)
			}
			fx.svc.RunCycle(ctx)
			note("reconcile + cycle: artifact is %s", stateOf662(t, fx.journal, fx.artifact))
			if resolved() {
				return
			}

			// The file has to still be there, or this is a different
			// complaint entirely and the transcript below is misleading.
			onDisk, statErr := os.Stat(fx.final)
			if statErr != nil {
				t.Fatalf("the good file vanished during the walk (%v); this test can no longer say anything about #662", statErr)
			}
			if onDisk.Size() != fx.diskSize {
				t.Fatalf("the file at %s changed size during the walk: %d -> %d", fx.final, fx.diskSize, onDisk.Size())
			}

			t.Errorf(
				"#662 defect 3: no documented verb resolves an empty record written over a good file.\n"+
					"  artifact: %s, now %s\n"+
					"  on disk:  %s is %d bytes, sha256 %s, untouched throughout\n"+
					"  %s\n"+
					"Every exit is refused, and the one verb that fits the situation -- reinstate, \"I looked, the "+
					"file is good, believe it\" -- is the one the natural first move takes away. An operator on a "+
					"NAS has no shell to finish this by hand.",
				fx.artifact, stateOf662(t, fx.journal, fx.artifact),
				fx.final, fx.diskSize, fx.diskHash,
				strings.Join(transcript, "\n  "))
		})
	}
}

// TestIssue662_RetryFromQuarantineMustNotForfeitARecoveryOption is the
// one-way door on its own, because it is the half of defect 3 that a fix
// for the record could leave standing.
//
// From QUARANTINED an operator has verbs that accept the artifact. After
// `retry` has moved it to DISCOVERED and the cycle has bounced it off the
// FR-12 collision into FAILED, those same verbs refuse it on state grounds
// alone. The move cost a recovery option and returned nothing: the artifact
// is no nearer resolution than before.
//
// The contract asserted is a disjunction, so it goes green on any honest
// fix: after `retry`, EITHER the artifact is resolved, OR every verb that
// accepted it before still accepts it.
func TestIssue662_RetryFromQuarantineMustNotForfeitARecoveryOption(t *testing.T) {
	cases := []struct {
		name string
		// collision is whether a file already occupies the artifact's
		// final name. #662's state has one, put there by the commit that
		// recorded zero bytes; the control has none, so `retry` does what
		// it says and the artifact resolves.
		collision bool
	}{
		{name: "retry into an FR-12 collision, which is #662's state", collision: true},
		{name: "control: retry with the final name free, so the retry actually works", collision: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDeadEndFixture(t, false)
			ctx := context.Background()
			if !tc.collision {
				// The same quarantined artifact with nothing in the way:
				// a retry can genuinely re-fetch, and a verb set that
				// shrinks is fine because the artifact is resolved.
				if err := os.Remove(fx.final); err != nil {
					t.Fatalf("clearing the final name for the control: %v", err)
				}
			}

			before := applicableVerbs662(t, fx)
			if len(before) == 0 {
				t.Fatal("no verb accepts the artifact even before retry; this test cannot measure a forfeiture that has already happened")
			}
			if !before["quarantine reinstate"] {
				t.Fatalf("precondition: reinstate does not accept a QUARANTINED artifact; verbs = %v", sortedVerbs662(before))
			}

			if err := fx.svc.RetryQuarantinedIngestion(ctx, fx.artifact); err != nil {
				t.Fatalf("quarantine retry: %v", err)
			}
			fx.svc.RunCycle(ctx)

			after := applicableVerbs662(t, fx)
			nowAt := lifecycle.State(stateOf662(t, fx.journal, fx.artifact))
			if durable(nowAt) {
				return // retry did its job; nothing was forfeited.
			}

			var lost []string
			for verb := range before {
				if !after[verb] {
					lost = append(lost, verb)
				}
			}
			if len(lost) > 0 {
				t.Errorf(
					"#662 defect 3, the one-way door: `retry` took away %v and resolved nothing.\n"+
						"  artifact: %s, was QUARANTINED, now %s\n"+
						"  accepted before: %v\n"+
						"  accepted after:  %v\n"+
						"  on disk:  %s is %d bytes, sha256 %s\n"+
						"  last failure: %s\n"+
						"`retry` is the natural first move and it is the only door out of the one state that "+
						"offers the verb this situation needs. Moving through it must not cost an option while "+
						"leaving the artifact exactly as stuck.",
					sortedVerbs662Set(lost), fx.artifact, nowAt,
					sortedVerbs662(before), sortedVerbs662(after),
					fx.final, fx.diskSize, fx.diskHash,
					failureReason662(t, fx.svc, fx.artifact))
			}
		})
	}
}

// applicableVerbs662 reports which recovery verbs accept the artifact where
// it currently stands, as opposed to refusing it because of its state.
//
// "Accepts" is deliberately not "succeeds": `quarantine reinstate` that
// runs its checks and reports a failing verdict has accepted the artifact
// and given the operator an answer, which is a different thing from
// ErrNotQuarantined, where the verb will not look at all. That distinction
// is the whole of the one-way door: what `retry` costs is not a verb that
// worked, it is a verb that was willing to.
//
// Only verbs that write nothing when they refuse are probed, so measuring
// the set does not change it. RetryQuarantinedIngestion is the one that
// moves the row, and it is excluded for that reason rather than overlooked:
// it is the move under test, not part of the measurement.
func applicableVerbs662(t *testing.T, fx deadEndFixture) map[string]bool {
	t.Helper()
	ctx := context.Background()
	out := map[string]bool{}

	stateRefusal := func(err error) bool {
		return errors.Is(err, ErrNotQuarantined) ||
			errors.Is(err, ErrNotFailed) ||
			errors.Is(err, ErrQuarantineIrrecoverable) ||
			strings.Contains(err.Error(), "not a durable restore point")
	}

	if _, err := fx.svc.RevalidateQuarantined(ctx, fx.artifact); err == nil || !stateRefusal(err) {
		out["quarantine revalidate"] = true
	}
	if _, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "probe"); err == nil || !stateRefusal(err) {
		out["quarantine reinstate"] = true
	}
	if _, err := fx.svc.ValidateArtifact(ctx, fx.artifact, ValidateOptions{}); err == nil || !stateRefusal(err) {
		out["validate"] = true
	}

	// The probes above must not have moved the artifact; if one did, the
	// measurement is not a measurement.
	if got := stateOf662(t, fx.journal, fx.artifact); !map[string]bool{
		string(lifecycle.Quarantined):     true,
		string(lifecycle.QuarantinedLost): true,
		string(lifecycle.Failed):          true,
		string(lifecycle.Discovered):      true,
		string(lifecycle.Committed):       true,
		string(lifecycle.RemoteRetained):  true,
		string(lifecycle.Complete):        true,
	}[got] {
		t.Fatalf("probing the verbs left the artifact at %s; the probe is not read-only", got)
	}
	return out
}

func sortedVerbs662(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return sortedVerbs662Set(out)
}

func sortedVerbs662Set(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// stateOf662 reads the artifact's state out of the journal, which is the
// only thing `rbm artifacts` shows an operator.
func stateOf662(t *testing.T, j Journal, artifact model.ArtifactID) string {
	t.Helper()
	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Journal.Get(%s): %v", artifact, err)
	}
	return rec.State
}

// failureReason662 quotes the product's own recorded failure sentence, so a
// transcript reads like the one in the issue rather than like a paraphrase.
func failureReason662(t *testing.T, svc *Service, artifact model.ArtifactID) string {
	t.Helper()
	detail, err := svc.GetArtifactDetail(context.Background(), artifact)
	if err != nil {
		return fmt.Sprintf("(could not read the artifact's detail: %v)", err)
	}
	if detail.FailureReason == "" {
		return "(none recorded)"
	}
	return detail.FailureReason
}

// walkToFailed662 puts the fixture's artifact exactly where issue #662's
// operator found it: FAILED, because `retry` sent the row back to
// DISCOVERED and the next cycle met FR-12's collision guard on the
// artifact's own durable copy.
//
// Every step is a verb or a cycle, never a hand-written journal row, so
// what the cases below argue about is a state the product really reaches.
func walkToFailed662(t *testing.T, fx deadEndFixture) {
	t.Helper()
	ctx := context.Background()
	if err := fx.svc.RetryQuarantinedIngestion(ctx, fx.artifact); err != nil {
		t.Fatalf("quarantine retry: %v", err)
	}
	fx.svc.RunCycle(ctx)
	if got := stateOf662(t, fx.journal, fx.artifact); got != string(lifecycle.Failed) {
		t.Fatalf("precondition: the artifact is %s after the collision cycle, want FAILED", got)
	}
	if rec := mustRecord662(t, fx); rec.LocalPath != fx.final {
		t.Fatalf("precondition: the row's local path is %q, want the occupied final name %q", rec.LocalPath, fx.final)
	}
}

func mustRecord662(t *testing.T, fx deadEndFixture) state.Record {
	t.Helper()
	rec, err := fx.journal.Get(context.Background(), fx.artifact)
	if err != nil {
		t.Fatalf("Journal.Get(%s): %v", fx.artifact, err)
	}
	return rec
}

// recordedBytes662 is what the journal row says was transferred, which is
// the number reconciliation compares the file against and the number #662
// is about.
func recordedBytes662(rec state.Record) int64 {
	if rec.Transfer == nil {
		return -1
	}
	return rec.Transfer.BytesTransferred
}

// TestIssue662_RetryRefusesToRecordAByteCountItCouldNotMeasure is review
// finding A against #662's own remedy.
//
// completeIngestionInPlace reads the whole file to hash it and then
// measures that same file, and the measurement is what replaces the empty
// read-back's zero in the journal. The two observations are separated by
// one remote round trip, so a transient local fault can land between them.
// When it did, the repair fell back to a zero byte count and wrote it
// beside the correct content sha256 with Checksummed: true -- which is
// bit for bit the row shape #662 was filed about, over a file the same
// call had just read end to end, produced by the code that exists to
// repair it. The next reconciliation pass then condemns the file on it.
//
// The declines above this point in the function return an empty state
// having written nothing, and they mean "this is not the in-place case".
// This is not one of those: the file was hashed a moment ago, so the case
// is established and the measurement broke. That has to surface as a
// failure rather than be papered over with a zero, because the operator
// can retry a refusal and cannot un-write a journal row.
func TestIssue662_RetryRefusesToRecordAByteCountItCouldNotMeasure(t *testing.T) {
	fx := newDeadEndFixture(t, false)
	ctx := context.Background()
	walkToFailed662(t, fx)

	rowsBefore := transitionRows(t, fx.journal)
	before := mustRecord662(t, fx)

	// The fault, injected in the one window that matters: the file stays
	// present and byte-identical, and only the path to it stops being
	// traversable, so the local stat fails for a reason a NAS produces
	// (a permission flap, an unmount, an SMB reconnect) rather than
	// because the backup is gone.
	restore := func() { _ = os.Chmod(fx.localDir, 0o755) }
	t.Cleanup(restore)
	fx.tr.afterRemoteHash = func() {
		if err := os.Chmod(fx.localDir, 0o000); err != nil {
			t.Fatalf("arming the stat failure: %v", err)
		}
		if _, err := os.Stat(fx.final); err == nil {
			t.Fatal("os.Stat still succeeds through a 0000 directory here, so this case is not reproducing a stat failure and would pass for the wrong reason")
		}
	}

	retryErr := fx.svc.RetryFailedIngestion(ctx, fx.artifact, "issue #662")
	restore()
	fx.tr.afterRemoteHash = nil

	// Without this the whole case is about something else. The premise is
	// a good file that was never in doubt.
	onDisk, statErr := os.Stat(fx.final)
	if statErr != nil {
		t.Fatalf("the good file is gone after the retry (%v); this case can no longer say anything about a measurement that failed over a file that was there", statErr)
	}
	if onDisk.Size() != fx.diskSize {
		t.Fatalf("the file at %s changed size during the retry: %d -> %d", fx.final, fx.diskSize, onDisk.Size())
	}

	after := mustRecord662(t, fx)

	// The row, not the return value. Both the fixed and the unfixed code
	// hand back an error here -- the unfixed one only after it has
	// already written the row -- so what the operator is left with is the
	// only thing that discriminates.
	if recordedBytes662(after) == 0 && strings.EqualFold(after.LocalHash, fx.diskHash) {
		t.Errorf(
			"#662 review finding A: the recovery verb wrote #662's own row shape.\n"+
				"  artifact: %s, now %s\n"+
				"  recorded: %d bytes, sha256 %s, checksummed=%v\n"+
				"  on disk:  %s is %d bytes, sha256 %s, present and unchanged throughout\n"+
				"  retry returned: %v\n"+
				"A zero byte count beside a content hash of 294 real bytes, with the checksummed flag set, is "+
				"exactly the record issue #662 was filed about, and reconciliation condemns the file on it. The "+
				"measurement failed; the repair must say so, not substitute a zero it did not measure.",
			fx.artifact, after.State,
			recordedBytes662(after), after.LocalHash, after.Transfer != nil && after.Transfer.Checksummed,
			fx.final, onDisk.Size(), fx.diskHash,
			retryErr)
	}
	if rows := transitionRows(t, fx.journal); rows != rowsBefore {
		t.Errorf("state_transitions grew from %d to %d rows on a retry whose own measurement of the file failed; "+
			"a repair that could not measure what it was repairing must write nothing (#662 finding A)", rowsBefore, rows)
	}
	if recordedBytes662(after) != recordedBytes662(before) || after.LocalHash != before.LocalHash {
		t.Errorf("the row changed from %d bytes/%s to %d bytes/%s although the measurement behind the change failed (#662 finding A)",
			recordedBytes662(before), before.LocalHash, recordedBytes662(after), after.LocalHash)
	}

	// And it has to be reported, because an operator who is told nothing
	// retries into the same window forever.
	if retryErr == nil {
		t.Fatal("retry reported success although it could not measure the file it was repairing (#662 finding A)")
	}
	if !strings.Contains(retryErr.Error(), fx.final) {
		t.Errorf("retry error = %q, want it to name the file it could not measure (%s)", retryErr, fx.final)
	}
}

// differentPayload662 is 294 bytes, the same size as payload662, and
// byte-for-byte different from it. The cases below need a file that is
// wrong in the one way #662's own fixture never is: present, the right
// size, and NOT the remote object.
var differentPayload662 = func() []byte {
	b := make([]byte, 294)
	for i := range b {
		b[i] = byte('z' - i%26)
	}
	return b
}()

// TestIssue662_MismatchedFinalNameContentIsNotReinstated is review finding
// B's first case: the s.Transport.RemoteHash comparison in
// completeIngestionInPlace is the only observation in the recovery path
// the local file did not itself produce, and it is the one thing #662's
// own regression suite never disturbed the file enough to exercise.
//
// The fixture reaches FAILED exactly as #662's operator did -- quarantine
// retry, then a cycle meeting FR-12's collision guard on the artifact's
// own durable copy, via walkToFailed662 -- and only after that does
// something #662 never modeled: the bytes at the final name stop being
// the remote object (a bad disk, a wrong file swapped in by hand,
// anything other than what committed them). If the comparison is a
// no-op, that difference is invisible and the artifact is trusted right
// back into service on a file that is not its remote copy.
func TestIssue662_MismatchedFinalNameContentIsNotReinstated(t *testing.T) {
	fx := newDeadEndFixture(t, false)
	walkToFailed662(t, fx)

	// Swap the content at the final name for 294 different bytes, same
	// size, so a size-only check could not tell the two apart.
	mustWriteFile(t, fx.final, string(differentPayload662))
	sum := sha256.Sum256(differentPayload662)
	mismatchedHash := hex.EncodeToString(sum[:])
	if strings.EqualFold(mismatchedHash, fx.diskHash) {
		t.Fatal("precondition: the swapped-in bytes hash the same as the remote object; this case needs them to differ")
	}

	ctx := context.Background()
	err := fx.svc.RetryFailedIngestion(ctx, fx.artifact, "issue #662: mismatched content probe")
	if err != nil {
		t.Fatalf("RetryFailedIngestion: %v (a mismatch must fall through to the ordinary retry, which succeeds)", err)
	}

	got := stateOf662(t, fx.journal, fx.artifact)
	if got != string(lifecycle.Discovered) {
		t.Fatalf(
			"#662 review finding B: state = %s after a retry over a final name whose content does NOT match the "+
				"remote object. Want DISCOVERED (the ordinary retry, falling through because the comparison "+
				"failed) -- %s means the artifact was reinstated in place on a file that is not its remote copy.",
			got, got)
	}

	after := mustRecord662(t, fx)
	if after.Transfer != nil && after.Transfer.Checksummed {
		t.Errorf("#662 review finding B: the row carries a content-verified placement (Checksummed=true) over a "+
			"final name whose content does not match the remote object %s", fx.artifact)
	}
}

// newStrayFileFixture is review finding B's second case: rec.LocalPath !=
// final, the guard at quarantineactions.go's line 488, equally unwatched.
//
// Discovered -> Transferring -> Failed is a transient pre-commit failure
// (FR-10's own table: machine.go's {From: Transferring, To: Failed}), so
// no Committing -> Committed transition ever set LocalPath on this row.
// A file that is byte-identical to the remote object is placed at the
// artifact's final name anyway -- exactly what a stray file left over
// from an unrelated write, or a name collision, looks like from the
// outside. Only the durable commit is allowed to make that file this
// artifact's own copy; matching content is not enough on its own,
// which is the property this fixture isolates.
func newStrayFileFixture(t *testing.T) deadEndFixture {
	t.Helper()
	ctx := context.Background()
	localDir := t.TempDir()

	bs := testBackupSet(t, localDir)
	bs.Include = []string{"*.gz"}
	bs.ReadOnly = true

	source := transport.Source{ID: "662-nas"}
	tr := newFakeTransport()
	tr.put("dpkg.diversions.5.gz", string(payload662), epoch.Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	final, err := lifecycle.FinalArtifactPath(localDir, rec.Artifact)
	if err != nil {
		t.Fatalf("FinalArtifactPath: %v", err)
	}
	mustWriteFile(t, final, string(payload662))
	sum := sha256.Sum256(payload662)
	diskHash := hex.EncodeToString(sum[:])
	diskSize := int64(len(payload662))

	steps := []struct{ from, to lifecycle.State }{
		{lifecycle.Discovered, lifecycle.Transferring},
		{lifecycle.Transferring, lifecycle.Failed},
	}
	for i, s := range steps {
		if _, err := journal.RecordTransition(ctx, state.Transition{
			Artifact:   rec.Artifact,
			Key:        fmt.Sprintf("stray-662-%d-%s", i, s.to),
			From:       string(s.from),
			To:         string(s.to),
			Detail:     "issue #662 stray-file fixture: a transient pre-commit failure, no LocalPath ever recorded",
			OccurredAt: epoch.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("fixture: %s -> %s: %v", s.from, s.to, err)
		}
	}

	if got := stateOf662(t, journal, rec.Artifact); got != string(lifecycle.Failed) {
		t.Fatalf("precondition: fixture is %s, want FAILED", got)
	}
	fx := deadEndFixture{
		svc: svc, journal: journal, artifact: rec.Artifact,
		localDir: localDir, final: final, tr: tr, diskSize: diskSize, diskHash: diskHash,
	}
	if rec := mustRecord662(t, fx); rec.LocalPath == final {
		t.Fatal("precondition: the row's local path is already the final name; this fixture needs them to differ")
	}
	return fx
}

// TestIssue662_StrayFileAtFinalNameIsNotTrustedAsTheArtifactsOwnCopy is
// review finding B's second case: completeIngestionInPlace's
// rec.LocalPath != final guard (quarantineactions.go:488) is equally
// unwatched by #662's existing suite. A file that hashes to the remote
// object sits at the final name, but the row never recorded committing to
// it, so it must not be trusted as this artifact's durable copy.
func TestIssue662_StrayFileAtFinalNameIsNotTrustedAsTheArtifactsOwnCopy(t *testing.T) {
	fx := newStrayFileFixture(t)
	ctx := context.Background()

	err := fx.svc.RetryFailedIngestion(ctx, fx.artifact, "issue #662: stray-file probe")
	if err != nil {
		t.Fatalf("RetryFailedIngestion: %v (an unrecorded file at the final name must fall through to the ordinary retry, which succeeds)", err)
	}

	got := stateOf662(t, fx.journal, fx.artifact)
	if got != string(lifecycle.Discovered) {
		t.Fatalf(
			"#662 review finding B: state = %s after a retry where the row never recorded committing to the "+
				"file at the final name. Want DISCOVERED (the ordinary retry, falling through because "+
				"rec.LocalPath != final) -- %s means content alone was enough to reinstate an artifact the row "+
				"never claimed that file for.",
			got, got)
	}

	after := mustRecord662(t, fx)
	if after.Transfer != nil && after.Transfer.Checksummed {
		t.Errorf("#662 review finding B: the row carries a content-verified placement (Checksummed=true) for %s "+
			"although the row never recorded committing to the file at the final name", fx.artifact)
	}
}

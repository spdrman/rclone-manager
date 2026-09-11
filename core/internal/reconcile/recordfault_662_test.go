package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/internal/lifecycle"
	"github.com/spdrman/backupd/core/internal/model"
	"github.com/spdrman/backupd/core/internal/state"
	"github.com/spdrman/backupd/core/internal/transport"
)

// Issue #662, defect 2: reconciliation condemned a file it never opened.
//
// The measured transition, verbatim from the deployment's journal:
//
//	QUARANTINED  dpkg.diversions.5.gz
//	  FR-17 / issue #315: reconciliation found the durable local copy of a
//	  retained (read-only-source) artifact invalid: recorded remote size 294
//	  disagrees with recorded transfer size 0
//
// Read the operands. "recorded remote size" and "recorded transfer size"
// are both fields of the same journal row, and expectedLocalSize returns an
// error the moment they differ. checkLocalFinal turns that error straight
// into invalid(), which quarantines. The file is stat'ed a few lines
// earlier and its size is in hand; it is never consulted for this verdict,
// and the hash check below never runs because the size check errored first.
//
// So an intact 294-byte backup was quarantined on the strength of two
// numbers disagreeing with each other. It was byte-identical to four
// sibling rotations of the same content, and the operator had to prove that
// by hand, outside the product, on a NAS that exists so there is no shell.
//
// What these cases pin, and deliberately no more: a disagreement BETWEEN
// TWO RECORDS is a record fault, not a verdict about a file. The file is
// the tie-breaker and it is already open. The two cases split on how much
// the fix is being told:
//
//   - The first has no recorded local hash, so the record disagreement is
//     the only thing wrong and the file agrees exactly with the remote
//     identity captured at discovery. There is no honest reading in which
//     that artifact is invalid, so it asserts localValid outright.
//   - The second is the field row in full, mis-recorded hash and all. Here
//     a fix might legitimately still refuse the artifact, so it asserts
//     only that the verdict was reached BY READING THE FILE: an invalid
//     reason has to name what was measured on disk. That is the sentence
//     the issue asks for ("which of the two it actually inspected"), and it
//     goes green whether the maintainer decides to trust the file or to
//     keep refusing it for a reason it can defend.
//
// Each case carries its own positive control, the same fixture with the
// record fault absent, because "does not quarantine" is satisfiable by a
// reconciliation that has stopped quarantining anything at all.
//
// The end-to-end case at the bottom drives the real Reconcile pass, so the
// claim is about the journal an operator reads rather than about an
// unexported helper's return value.

// payload662 is 294 bytes, the size of the artifact #662 was filed about.
var payload662 = func() []byte {
	b := make([]byte, 294)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}()

// sha256OfNothing is the sha256 of no bytes: the checksum the empty
// read-back left in the journal and the sidecar manifest of a 294-byte
// file. Written out rather than computed so it can be matched against the
// issue by eye.
const sha256OfNothing = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// writeGoodLocalCopy puts the real bytes on disk and returns the path and
// their sha256, so every assertion below has a disk-side operand that was
// measured rather than assumed.
func writeGoodLocalCopy(t *testing.T) (path, hash string, size int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "dpkg.diversions.5.gz")
	if err := os.WriteFile(path, payload662, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256(payload662)
	return path, hex.EncodeToString(sum[:]), int64(len(payload662))
}

// TestIssue662_ARecordDisagreementIsNotAVerdictAboutTheFile is issue
// #662's defect 2 at the function that decides it.
func TestIssue662_ARecordDisagreementIsNotAVerdictAboutTheFile(t *testing.T) {
	localPath, diskHash, diskSize := writeGoodLocalCopy(t)
	if diskSize == 0 {
		t.Fatal("fixture wrote an empty file; nothing below could distinguish a record fault from a real one")
	}

	// A record shaped exactly like the one #662 measured, minus whichever
	// fault the case is holding out.
	record := func(transferSize int64, localHash string) state.Record {
		remoteSize := diskSize
		rec := state.Record{
			Artifact:   testArtifact(t, "dpkg.diversions.5.gz"),
			RemotePath: "backups/dpkg.diversions.5.gz",
			LocalPath:  localPath,
			State:      string(lifecycle.RemoteRetained),
			Remote:     state.RemoteIdentity{Size: &remoteSize},
			Transfer:   &state.TransferResult{BytesTransferred: transferSize},
		}
		if localHash != "" {
			rec.LocalHash = localHash
			rec.LocalHashAlg = string(transport.SHA256)
		}
		return rec
	}

	t.Run("the record disagreement is the only fault", func(t *testing.T) {
		got := checkLocalFinal(record(0, ""))
		if got.Verdict != localValid {
			t.Errorf(
				"#662 defect 2: reconciliation called a %d-byte file invalid without ever reading it.\n"+
					"  verdict: %s\n"+
					"  reason:  %s\n"+
					"  on disk: %s is %d bytes, sha256 %s, exactly the size the remote identity records\n"+
					"Both operands of that reason are journal fields. A recorded remote size disagreeing with a "+
					"recorded transfer size is a fault in the RECORDS; the file is stat'ed three lines earlier and "+
					"is never asked.",
				diskSize, got.Verdict, got.Reason, localPath, diskSize, diskHash)
		}
	})

	t.Run("control: the same file with the records agreeing", func(t *testing.T) {
		// The instrument's own proof. If this does not pass, the case
		// above is failing on the fixture rather than on the defect.
		got := checkLocalFinal(record(diskSize, ""))
		if got.Verdict != localValid {
			t.Fatalf("control: verdict = %s (%s), want %s; the fixture itself is wrong, not the product",
				got.Verdict, got.Reason, localValid)
		}
	})

	t.Run("the field row in full, with the empty read-back's hash recorded too", func(t *testing.T) {
		// #662's actual journal row: remote 294, transfer 0, and a local
		// hash that is the sha256 of nothing. A fix may legitimately
		// still refuse this artifact; what it may not do is refuse it
		// without looking.
		//
		// The evidence demanded is the file's own sha256, and only that.
		// A size would not do: today's reason already contains "294",
		// read out of the remote-identity RECORD, so a size-based check
		// here would pass on a sentence that proves the file was never
		// touched. A digest of the bytes is something no amount of
		// bookkeeping can produce, and it is already the product's own
		// idiom for this: checkLocalFinal's hash branch says "local final
		// file %s hash %s does not match the %s hash recorded at
		// verification, %s". The control below is that branch firing.
		got := checkLocalFinal(record(0, sha256OfNothing))
		if got.Verdict == localValid {
			return // trusting the file is one of the correct answers.
		}
		if !strings.Contains(got.Reason, diskHash) {
			t.Errorf(
				"#662 defect 2: reconciliation refused the durable copy without naming anything it measured on disk.\n"+
					"  verdict: %s\n"+
					"  reason:  %s\n"+
					"  on disk: %s is %d bytes, sha256 %s\n"+
					"Every number in that reason came out of the journal. An operator cannot tell from it whether "+
					"the file was inspected or only the bookkeeping was, and in the field it was only the "+
					"bookkeeping.",
				got.Verdict, got.Reason, localPath, diskSize, diskHash)
		}
	})

	t.Run("control: an invalid verdict that did read the file names what it read", func(t *testing.T) {
		// The instrument's proof for the case above. A record whose sizes
		// agree (so the record-fault branch is out of the way) over a
		// file whose content does not match the recorded hash: the
		// product reaches its hash branch, hashes the file, and puts the
		// measured digest in the sentence. That is the shape the case
		// above asks for, and it exists today.
		other := filepath.Join(t.TempDir(), "dpkg.diversions.5.gz")
		if err := os.WriteFile(other, append([]byte("x"), payload662[1:]...), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		otherSum := sha256.Sum256(append([]byte("x"), payload662[1:]...))
		otherHash := hex.EncodeToString(otherSum[:])
		if otherHash == diskHash {
			t.Fatal("control fixture is byte-identical to the good copy; it cannot demonstrate a hash mismatch")
		}

		rec := record(diskSize, diskHash)
		rec.LocalPath = other

		got := checkLocalFinal(rec)
		if got.Verdict != localInvalid {
			t.Fatalf("control: verdict = %s (%s), want %s", got.Verdict, got.Reason, localInvalid)
		}
		if !strings.Contains(got.Reason, otherHash) {
			t.Fatalf("control: reason %q does not name the digest it measured (%s); the assertion above is unsatisfiable as written",
				got.Reason, otherHash)
		}
	})

	t.Run("control: a file that really is wrong", func(t *testing.T) {
		// The fence on all three cases above. A reconciliation that
		// stopped quarantining anything would satisfy every one of them
		// and must fail this: a file whose bytes genuinely do not match
		// the record is exactly what FR-17 exists to catch.
		short := filepath.Join(t.TempDir(), "dpkg.diversions.5.gz")
		if err := os.WriteFile(short, payload662[:100], 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		rec := record(diskSize, "")
		rec.LocalPath = short

		got := checkLocalFinal(rec)
		if got.Verdict != localInvalid {
			t.Fatalf(
				"FR-17: a 100-byte file under a record that says %d bytes was called %s (%s), want %s; "+
					"reconciliation must still catch a copy that is genuinely wrong",
				diskSize, got.Verdict, got.Reason, localInvalid)
		}
	})
}

// TestIssue662_ReconcileDoesNotQuarantineAnIntactCopyOverARecordFault is
// the same defect one layer out, so the claim is about the journal an
// operator reads rather than about an unexported verdict.
//
// REMOTE_RETAINED is the state the field artifact was in: its backup set is
// declared read_only, so the manager never deletes the remote and
// reconcileRemoteRetained is the handler that ran. Its quarantine detail is
// the sentence quoted in the issue.
func TestIssue662_ReconcileDoesNotQuarantineAnIntactCopyOverARecordFault(t *testing.T) {
	cases := []struct {
		name string
		// transferSize is what the transfer step recorded. 0 is the
		// empty read-back #662 measured; the control records the truth.
		transferSize int64
	}{
		{name: "an empty read-back's transfer record over an intact 294-byte copy", transferSize: 0},
		{name: "control: the same intact copy with an honest transfer record", transferSize: int64(len(payload662))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			j := openTestJournal(t)
			artifact := testArtifact(t, "dpkg.diversions.5.gz")
			localPath, diskHash, diskSize := writeGoodLocalCopy(t)

			driveTo(t, j, driveParams{
				artifact:  artifact,
				remote:    state.RemoteIdentity{Size: &diskSize},
				localPath: localPath,
				transfer:  &state.TransferResult{BytesTransferred: tc.transferSize},
				stopAt:    lifecycle.RemoteRetained,
			})
			if before := stateOf662(t, j, artifact); before != string(lifecycle.RemoteRetained) {
				t.Fatalf("precondition: fixture is %s, want REMOTE_RETAINED", before)
			}

			report, err := Reconcile(ctx, Deps{Journal: j, Transport: &fakeTransport{}}, testSource, testSet(t))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if len(report.Findings) == 0 {
				t.Fatalf("Reconcile examined nothing at all; a pass with no findings cannot prove anything about this artifact")
			}

			after := stateOf662(t, j, artifact)
			if after == string(lifecycle.Quarantined) {
				t.Errorf(
					"#662 defect 2: reconciliation quarantined an intact durable copy over a disagreement between two of its own records.\n"+
						"  %s: REMOTE_RETAINED -> %s\n"+
						"  finding: %s\n"+
						"  on disk: %s is %d bytes, sha256 %s, matching the remote size the same row records\n"+
						"The file was never opened for this decision. A record/disk disagreement is a record fault "+
						"until the file has been read and found wanting.",
					artifact, after, findingReason662(report, artifact), localPath, diskSize, diskHash)
			}
		})
	}
}

// stateOf662 reads one artifact's current state out of the journal, which
// is the only thing an operator can see.
func stateOf662(t *testing.T, j *state.Journal, artifact model.ArtifactID) string {
	t.Helper()
	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Journal.Get(%s): %v", artifact, err)
	}
	return rec.State
}

// findingReason662 pulls the reason Reconcile reported for one artifact, so
// a failure message quotes the product's own sentence rather than a
// paraphrase of it.
func findingReason662(r Report, artifact model.ArtifactID) string {
	for _, f := range r.Findings {
		if f.Artifact == artifact {
			return fmt.Sprintf("%s -> %s: %s", f.From, f.To, f.Reason)
		}
	}
	return "(no finding reported for this artifact)"
}

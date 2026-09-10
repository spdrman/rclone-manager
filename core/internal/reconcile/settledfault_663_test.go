package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Two follow-on defects in #662's own fix, both about a fault that is
// computed and then not delivered.
//
// The fix stopped quarantining an intact copy over a disagreement between
// two of its own records, and left a sentence behind saying so. Neither of
// the two things that sentence exists for actually happened:
//
//  1. Nobody was told. Every localValid handler appends the clause to a
//     noAction finding (From == To), and the only non-test consumer of
//     Report.Findings prints a finding when `f.Changed() ||
//     f.NeedsInvestigation`. A no-action finding is neither, and no
//     journal transition is written for a converged row either, so the
//     product answered "reconciliation complete; no unresolved findings"
//     over a row that contradicts itself.
//  2. Nothing was checked. On the branch where the file's size agrees with
//     the remote identity, settleRecordContradiction returned valid before
//     checkLocalFinal's hash branch could run, and it repairs nothing, so
//     the row takes that same branch on every pass forever. FR-17's
//     content check was therefore permanently disabled for exactly the
//     rows that had already proven their bookkeeping was unreliable.
//
// (2) is not fixed by consulting rec.LocalHash: the recorded transfer size
// and the recorded local hash are one observation twice (#662 recorded 0
// bytes AND the sha256 of nothing, from one empty read), so half of a
// self-contradictory record cannot check the other half. The operand used
// is the discovery-time remote digest, rec.Remote.Hash, which the local
// read-back cannot have written. It is not always recorded, and the case
// below named "no remote digest was recorded" is the one that pins what
// happens then: a content check that silently degrades to no content check
// is the whole subject of this defect, so the degradation has to be in the
// sentence an operator reads.

// corrupt663 returns payload662's bytes with every one of them changed, so
// a file built from it has the right size and the wrong content: the one
// shape a size comparison cannot catch and a digest can.
func corrupt663() []byte {
	b := make([]byte, len(payload662))
	for i := range payload662 {
		b[i] = payload662[i] ^ 0xff
	}
	return b
}

func writeLocalBytes663(t *testing.T, b []byte) (path, hash string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "dpkg.diversions.5.gz")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256(b)
	return path, hex.EncodeToString(sum[:])
}

// TestIssue663_ContentIsStillCheckedOnAContradictoryRow is defect (2).
//
// Every case here is the #662 record fault (remote size 294, transfer size
// 0) over a 294-byte file, which is the branch that returned early. They
// differ only in what the row recorded about the REMOTE object's content
// and in whether the bytes on disk are the right ones.
func TestIssue663_ContentIsStillCheckedOnAContradictoryRow(t *testing.T) {
	goodPath, goodHash := writeLocalBytes663(t, payload662)
	badPath, badHash := writeLocalBytes663(t, corrupt663())
	size := int64(len(payload662))
	if badHash == goodHash {
		t.Fatal("fixture: the corrupted copy hashes to the same value as the good one; no case below could tell them apart")
	}

	// The field row, with the remote identity's own content record as the
	// only variable. rec.LocalHash is deliberately the sha256 of nothing
	// on every case: it is the empty read-back's own output, and no case
	// here may be satisfied by trusting it.
	record := func(localPath, remoteHash, remoteHashAlg string) state.Record {
		remoteSize := size
		return state.Record{
			Artifact:     testArtifact(t, "dpkg.diversions.5.gz"),
			RemotePath:   "backups/dpkg.diversions.5.gz",
			LocalPath:    localPath,
			State:        string(lifecycle.RemoteRetained),
			Remote:       state.RemoteIdentity{Size: &remoteSize, Hash: remoteHash, HashAlg: remoteHashAlg},
			Transfer:     &state.TransferResult{BytesTransferred: 0},
			LocalHash:    sha256OfNothing,
			LocalHashAlg: string(transport.SHA256),
		}
	}

	t.Run("a corrupted copy under a contradictory row is still refused", func(t *testing.T) {
		got := checkLocalFinal(record(badPath, goodHash, string(transport.SHA256)))
		if got.Verdict != localInvalid {
			t.Errorf(
				"#663: FR-17's content check is disabled for every self-contradictory row.\n"+
					"  verdict: %s\n"+
					"  reason:  %s\n"+
					"  on disk: %s is %d bytes and hashes to %s\n"+
					"  recorded for the remote object at discovery: %s\n"+
					"The file's size agrees with the remote identity, so settleRecordContradiction returns valid "+
					"and checkLocalFinal's hash branch never runs. Every byte of this file is wrong. Nothing "+
					"repairs the row, so this branch is taken again on every later pass, forever.",
				got.Verdict, got.Reason, badPath, size, badHash, goodHash)
			return
		}
		if !strings.Contains(got.Reason, badHash) {
			t.Errorf("#663: the refusal does not name the digest it measured (%s); reason: %s", badHash, got.Reason)
		}
		if !strings.Contains(got.Reason, goodHash) {
			t.Errorf("#663: the refusal does not name the remote digest it compared against (%s); reason: %s", goodHash, got.Reason)
		}
		if strings.Contains(got.Reason, sha256OfNothing) {
			t.Errorf("#663: the refusal leans on the recorded local hash, which is the empty read-back's own output; reason: %s", got.Reason)
		}
	})

	t.Run("a good copy under a contradictory row is content-checked, and says so", func(t *testing.T) {
		got := checkLocalFinal(record(goodPath, goodHash, string(transport.SHA256)))
		if got.Verdict != localValid {
			t.Fatalf("verdict = %s (%s), want %s: the bytes match the digest the remote object was recorded with",
				got.Verdict, got.Reason, localValid)
		}
		if !strings.Contains(got.Reason, goodHash) {
			t.Errorf(
				"#663: the copy was accepted without the reason recording that its CONTENT was checked.\n"+
					"  reason: %s\n"+
					"An operator cannot tell this sentence apart from one that only compared two sizes, and on a "+
					"row whose own bookkeeping is known to be wrong that is the difference that matters.",
				got.Reason)
		}
	})

	t.Run("no remote digest was recorded: still valid, and the sentence says it was not content-checked", func(t *testing.T) {
		got := checkLocalFinal(record(goodPath, "", ""))
		if got.Verdict != localValid {
			t.Fatalf("verdict = %s (%s), want %s: a backend that reports no hash is FR-16's expected case, not a fault",
				got.Verdict, got.Reason, localValid)
		}
		if !strings.Contains(got.Reason, "has not been content-verified") {
			t.Errorf(
				"#663: the content check degraded to nothing and the operator is not told.\n"+
					"  reason: %s\n"+
					"No digest was recorded for the remote object at discovery, so this copy was accepted on a size "+
					"comparison alone. A content check that silently degrades to none is the whole subject of this "+
					"defect: the degradation must be in the sentence, not only in the code.",
				got.Reason)
		}
	})

	t.Run("an unsupported remote digest algorithm degrades the same way, out loud", func(t *testing.T) {
		got := checkLocalFinal(record(goodPath, "0badc0de", "crc32"))
		if got.Verdict != localValid {
			t.Fatalf("verdict = %s (%s), want %s: an algorithm this build cannot compute is not evidence against the file",
				got.Verdict, got.Reason, localValid)
		}
		if !strings.Contains(got.Reason, "has not been content-verified") {
			t.Errorf("#663: an uncomputable remote digest silently became no content check at all; reason: %s", got.Reason)
		}
	})
}

// TestIssue663_TheSettledFaultReachesAFindingAnOperatorSees is defect (1)
// at the layer that decides it.
//
// The clause is computed correctly and then dropped: it rides a noAction
// finding, and `cmd/backup-manager/reconcile.go` prints a finding only when
// `f.Changed() || f.NeedsInvestigation`.
func TestIssue663_TheSettledFaultReachesAFindingAnOperatorSees(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stopAt lifecycle.State
	}{
		{name: "REMOTE_RETAINED, the state the field artifact was in", stopAt: lifecycle.RemoteRetained},
		{name: "COMMITTED", stopAt: lifecycle.Committed},
		{name: "COMPLETE", stopAt: lifecycle.Complete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			j := openTestJournal(t)
			artifact := testArtifact(t, "dpkg.diversions.5.gz")
			localPath, diskHash, diskSize := writeGoodLocalCopy(t)
			remoteSize := diskSize

			driveTo(t, j, driveParams{
				artifact:  artifact,
				remote:    state.RemoteIdentity{Size: &remoteSize},
				localPath: localPath,
				transfer:  &state.TransferResult{BytesTransferred: 0},
				stopAt:    tc.stopAt,
			})

			report, err := Reconcile(ctx, Deps{Journal: j, Transport: &fakeTransport{}}, testSource, testSet(t))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			var found *Finding
			for i := range report.Findings {
				if report.Findings[i].Artifact == artifact {
					found = &report.Findings[i]
				}
			}
			if found == nil {
				t.Fatalf("Reconcile reported no finding at all for %s", artifact)
			}

			if !found.Changed() && !found.NeedsInvestigation {
				t.Errorf(
					"#663: the settled record fault reaches no operator.\n"+
						"  finding: %s -> %s (Changed=%v, NeedsInvestigation=%v)\n"+
						"  reason:  %s\n"+
						"  on disk: %s is %d bytes, sha256 %s\n"+
						"cmd/backup-manager/reconcile.go prints a finding only when f.Changed() || "+
						"f.NeedsInvestigation, and no journal transition is written for a converged row, so this "+
						"row's self-contradiction is computed, formatted, and then discarded. The product printed "+
						"\"reconciliation complete; no unresolved findings\" over it.",
					found.From, found.To, found.Changed(), found.NeedsInvestigation, found.Reason,
					localPath, diskSize, diskHash)
			}
			if !strings.Contains(found.Reason, "journal row needs repair") {
				t.Errorf("#663: the finding's reason does not carry the record fault: %s", found.Reason)
			}
			if !strings.Contains(found.Reason, "disagrees with recorded transfer size") {
				t.Errorf("#663: the finding's reason does not name the two figures that contradict each other: %s", found.Reason)
			}
		})
	}
}

// TestIssue663_AnUncontradictedRowIsStillSilent is the fence on the case
// above. NeedsInvestigation is what `rbm reconcile` prints on, so setting
// it for a row with nothing wrong would turn every healthy artifact into a
// line of output on every pass, which is the same defect as printing
// nothing: an operator who is told about everything is told about nothing.
func TestIssue663_AnUncontradictedRowIsStillSilent(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := testArtifact(t, "dpkg.diversions.5.gz")
	localPath, _, diskSize := writeGoodLocalCopy(t)
	remoteSize := diskSize

	driveTo(t, j, driveParams{
		artifact:  artifact,
		remote:    state.RemoteIdentity{Size: &remoteSize},
		localPath: localPath,
		transfer:  &state.TransferResult{BytesTransferred: diskSize},
		stopAt:    lifecycle.RemoteRetained,
	})

	report, err := Reconcile(ctx, Deps{Journal: j, Transport: &fakeTransport{}}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, f := range report.Findings {
		if f.Artifact != artifact {
			continue
		}
		if f.NeedsInvestigation {
			t.Errorf("a row with no record fault was flagged for investigation; reason: %s", f.Reason)
		}
	}
}

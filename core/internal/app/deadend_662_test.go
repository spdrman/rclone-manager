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
		localDir: localDir, final: final, diskSize: diskSize, diskHash: diskHash,
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

// These cover what the FR-15 delete gate does when an artifact's durable
// copy is not on local disk at all, which is the shape EPIC E's move engine
// leaves behind.
//
// They are in their own file because the assertion is about the WORDING of a
// refusal rather than about the refusal happening. The gate was already
// refusing this case correctly, and the fault issue #434 found was that it
// said "no local final path is recorded", which describes a lost file and
// sends an operator hunting for one. A copy sitting safely in a bucket is
// not a lost file, and a message that cannot tell the two apart turns a
// correct refusal into a wasted afternoon.
//
// So these tests read the refusal text. That is unusual here and it is the
// point: the message is the product surface in this case, since the gate's
// behaviour is unchanged either way.
package lifecycle

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TestDeleteRemote_RefusesWhenTheDurableCopyIsOnAMedium is issue #434's
// pin for the pre-delete gate.
//
// The gate was already refusing this shape, and refusing is right: FR-15
// requires the local copy to be confirmed before the remote source is
// destroyed, and a copy on a medium is one this gate cannot read. What
// was wrong is the reason. "No local final path is recorded for this
// artifact" describes a lost file, and an operator reading it goes
// looking for one. The fixture is the shape a completed move leaves: a
// GONE local placement with no file behind it, and an ACTIVE, content
// verified copy on a medium. FR-30 lets only COMPLETE artifacts move, so
// a COMMITTED artifact in this shape should never exist; the gate's job
// when it does is to refuse and say so, not to guess.
func TestDeleteRemote_RefusesWhenTheDurableCopyIsOnAMedium(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath := filepath.Join(t.TempDir(), "backup.final") // never written: the move deleted it
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
		&state.TransferResult{BytesTransferred: 10}, nil, Committed)

	at := time.Now().UTC()
	for _, p := range []state.PlacementUpdate{
		{Medium: state.MediumLocal, Location: localPath, Status: state.PlacementGone},
		{Medium: "cold_offsite", Location: "rclone-manager/production/pg/" + artifact.Name, Size: &size,
			Hash: "0000000000000000000000000000000000000000000000000000000000000000", HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementActive},
	} {
		p := p
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact: artifact, Key: artifact.String() + ":placement:" + p.Medium,
			From: string(Committed), To: string(Committed), OccurredAt: at, Placement: &p,
		}); err != nil {
			t.Fatalf("recording the %s placement: %v", p.Medium, err)
		}
	}

	tp := &deleteTransport{}
	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})

	refusal := requireRefusal(t, err, "local file")
	if !strings.Contains(refusal.Reason, `"cold_offsite"`) {
		t.Errorf("refusal.Reason = %q, want it to name the medium the durable copy is on", refusal.Reason)
	}
	if strings.Contains(refusal.Reason, "no local final path") {
		t.Errorf("refusal.Reason = %q reads a moved artifact as a lost file", refusal.Reason)
	}
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0: a source is never deleted against a copy this gate cannot read", tp.deleteCalls)
	}
	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(Committed) {
		t.Errorf("journal state changed to %q, want it left at COMMITTED", rec.State)
	}
}

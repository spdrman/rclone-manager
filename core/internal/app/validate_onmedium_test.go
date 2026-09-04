package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TestValidateArtifact_RefusesWhenTheDurableCopyIsOnAMedium is issue
// #434's pin for the operator door.
//
// `validate <id>` against a moved artifact used to return a failed verdict
// ("no local final path is recorded in the journal") and route the
// artifact COMPLETE -> QUARANTINED_LOST, over a copy that was on the
// medium, verified. The copy being somewhere this command cannot read is
// a fact about where it is, not a verdict about the artifact, so it gets
// the same treatment an unresolved validator does: a refusal that says
// where the copy is, and an artifact left exactly as it was.
func TestValidateArtifact_RefusesWhenTheDurableCopyIsOnAMedium(t *testing.T) {
	f := newCommittedFixture(t)
	ctx := context.Background()

	// The shape a completed move leaves: the local file deleted, its
	// placement GONE, and the only ACTIVE placement a content-verified
	// copy on a medium. The fixture's local placement is the one
	// lifecycle's own Commit wrote, so it is retired under its own
	// location rather than one this test made up.
	before, err := f.journal.Get(ctx, f.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	local, ok := before.LocalPlacement()
	if !ok {
		t.Fatalf("the fixture has no ACTIVE local placement; placements = %+v", before.Placements)
	}
	if err := os.Remove(local.Location); err != nil {
		t.Fatalf("removing the local copy the move would have deleted: %v", err)
	}
	size := int64(len("payload for validate"))
	for _, p := range []state.PlacementUpdate{
		{Medium: state.MediumLocal, Location: local.Location, Status: state.PlacementGone},
		{Medium: "cold_offsite", Location: "rclone-manager/production/pg/" + f.artifact.Name, Size: &size,
			Hash: before.LocalHash, HashAlg: before.LocalHashAlg,
			VerificationClass: state.VerificationContent, Status: state.PlacementActive},
	} {
		p := p
		if _, err := f.journal.RecordTransition(ctx, state.Transition{
			Artifact: f.artifact, Key: f.artifact.String() + ":placement:" + p.Medium,
			From: string(lifecycle.Complete), To: string(lifecycle.Complete), OccurredAt: epoch, Placement: &p,
		}); err != nil {
			t.Fatalf("recording the %s placement: %v", p.Medium, err)
		}
	}

	_, err = f.svc.ValidateArtifact(ctx, f.artifact)
	if err == nil {
		t.Fatal("ValidateArtifact returned a verdict for an artifact it has no local copy to check; want a refusal")
	}
	if !strings.Contains(err.Error(), `"cold_offsite"`) {
		t.Errorf("err = %v, want it to name the medium the durable copy is on", err)
	}
	if strings.Contains(err.Error(), "no local final path") {
		t.Errorf("err = %v reads a moved artifact as a lost file", err)
	}

	after, err := f.journal.Get(ctx, f.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.Complete) {
		t.Fatalf("journal state = %q after a refused validate, want COMPLETE left untouched", after.State)
	}
	if _, found, err := f.journal.LastTransition(ctx, f.artifact, string(lifecycle.Complete), string(lifecycle.QuarantinedLost)); err != nil {
		t.Fatalf("LastTransition: %v", err)
	} else if found {
		t.Error("a COMPLETE -> QUARANTINED_LOST transition was written for an artifact whose copy is on a medium")
	}
}

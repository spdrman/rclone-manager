package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// committedFixture is one artifact driven through the real pipeline all
// the way to COMPLETE, ready for ValidateArtifact tests to act on.
type committedFixture struct {
	svc      *Service
	journal  Journal
	artifact model.ArtifactID
	localDir string
}

func newCommittedFixture(t *testing.T) committedFixture {
	t.Helper()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "validate-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload for validate", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.processArtifact(ctx, source, bs, rec)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("precondition failed: journal state = %q, want %q", final.State, lifecycle.Complete)
	}

	return committedFixture{svc: svc, journal: journal, artifact: rec.Artifact, localDir: localDir}
}

// TestValidateArtifact_PassesOnUnchangedFile proves a clean validate
// leaves the artifact exactly where it was (COMPLETE), reports Passed,
// and writes nothing to the journal (no same-state audit write, unlike
// internal/revalidate.Run's own due-ness bookkeeping; see ValidateArtifact's
// doc for why).
func TestValidateArtifact_PassesOnUnchangedFile(t *testing.T) {
	fx := newCommittedFixture(t)
	ctx := context.Background()

	before, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	result, err := fx.svc.ValidateArtifact(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if !result.Checked || !result.Passed {
		t.Errorf("result = %+v, want Checked && Passed", result)
	}
	if result.NewState != "" {
		t.Errorf("NewState = %q, want empty (a pass must not move the artifact)", result.NewState)
	}

	after, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("UpdatedAt changed from %s to %s: a passing validate must not write to the journal", before.UpdatedAt, after.UpdatedAt)
	}
	if after.State != string(lifecycle.Complete) {
		t.Errorf("State = %q, want %q", after.State, lifecycle.Complete)
	}
}

// TestValidateArtifact_QuarantinesOnCorruption proves a validate against a
// local final file that has since been corrupted (its content no longer
// matches the hash recorded at VERIFIED) routes the artifact to
// QUARANTINED_LOST, mirroring internal/reconcile's and
// internal/revalidate's own COMPLETE -> QUARANTINED_LOST routing for
// exactly this finding.
func TestValidateArtifact_QuarantinesOnCorruption(t *testing.T) {
	fx := newCommittedFixture(t)
	ctx := context.Background()

	localFinal := filepath.Join(fx.localDir, "backup.dump")
	if err := os.WriteFile(localFinal, []byte("corrupted after the fact"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	result, err := fx.svc.ValidateArtifact(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if result.Passed {
		t.Fatalf("result = %+v, want Passed = false", result)
	}
	if result.NewState != lifecycle.QuarantinedLost {
		t.Errorf("NewState = %q, want %q", result.NewState, lifecycle.QuarantinedLost)
	}

	after, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.QuarantinedLost) {
		t.Errorf("journal State = %q, want %q", after.State, lifecycle.QuarantinedLost)
	}
}

// TestValidateArtifact_RefusesArtifactNotYetDurable proves ValidateArtifact
// refuses an artifact that has not reached a durable restore point yet
// (still DISCOVERED here), rather than guessing what "validate" should
// mean for it.
func TestValidateArtifact_RefusesArtifactNotYetDurable(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "not-durable-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)

	if _, err := svc.ValidateArtifact(ctx, rec.Artifact); err == nil {
		t.Error("ValidateArtifact on a DISCOVERED artifact = nil error, want a refusal")
	}
}

// TestParseArtifactID_RoundTrips proves ParseArtifactID inverts
// model.ArtifactID.String(), the form `validate <artifact-id>` takes its
// one positional argument in.
func TestParseArtifactID_RoundTrips(t *testing.T) {
	set := mustSetID(t, "production", "postgres-primary")
	id, err := model.NewArtifactID(set, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	parsed, err := ParseArtifactID(id.String())
	if err != nil {
		t.Fatalf("ParseArtifactID(%q): %v", id.String(), err)
	}
	if parsed != id {
		t.Errorf("ParseArtifactID(%q) = %+v, want %+v", id.String(), parsed, id)
	}
}

func TestParseArtifactID_RejectsMalformedInput(t *testing.T) {
	for _, s := range []string{"", "no-slashes-here", "only/two"} {
		if _, err := ParseArtifactID(s); err == nil {
			t.Errorf("ParseArtifactID(%q) = nil error, want an error", s)
		}
	}
}

// TestValidateArtifact_RefusesWhenTheNamedValidatorWasNeverResolved is
// issue #164's review finding M4. The invariant that a Validation naming
// a ValidatorID with a nil Command must never read as "no validator
// configured" was enforced in internal/lifecycle's verify path and
// nowhere else, so FR-14's operator-triggered `validate` reported an
// artifact as passing without ever running the validator its backup set
// names. That is the same fail-open outcome FR-13 exists to prevent,
// reached through the other door.
//
// The pass at the top is this test's positive control, and it is what
// makes the refusal below mean something: the identical call, over the
// identical artifact, differing only in whether the backup set names a
// validator nothing resolved.
func TestValidateArtifact_RefusesWhenTheNamedValidatorWasNeverResolved(t *testing.T) {
	fx := newCommittedFixture(t)
	ctx := context.Background()

	result, err := fx.svc.ValidateArtifact(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("ValidateArtifact (control): %v", err)
	}
	if !result.Passed {
		t.Fatalf("control result = %+v, want Passed: this artifact validates cleanly with no validator configured", result)
	}

	// The backup set now names a registered validator that nothing ever
	// resolved into a runnable command.
	fx.svc.Config.Sources[0].BackupSets[0].Validation.ValidatorID = "trailer-marker"

	before, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	result, err = fx.svc.ValidateArtifact(ctx, fx.artifact)
	if err == nil {
		t.Fatalf("ValidateArtifact reported %+v for a backup set whose validator was never resolved; want a refusal", result)
	}
	if !errors.Is(err, config.ErrValidatorNotResolved) {
		t.Errorf("error = %v, want one wrapping config.ErrValidatorNotResolved", err)
	}
	if result.Passed {
		t.Error("result.Passed is true on the refusal path")
	}

	after, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != before.State {
		t.Errorf("state moved from %q to %q: an unresolved validator says nothing about the artifact, so it must not quarantine one", before.State, after.State)
	}
}

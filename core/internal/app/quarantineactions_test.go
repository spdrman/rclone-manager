package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// quarantinedFixture is one discovered artifact walked to a quarantine
// state through the journal's own transition log.
//
// The route (DISCOVERED -> TRANSFERRING -> TRANSFERRED -> VERIFYING ->
// QUARANTINED) is every edge internal/lifecycle's Transitions table
// declares, in order, so this fixture stands the artifact somewhere the
// real pipeline can actually put it rather than somewhere only a test can.
type quarantinedFixture struct {
	svc      *Service
	journal  Journal
	artifact model.ArtifactID
	localDir string
}

func newQuarantinedFixture(t *testing.T, target lifecycle.State) quarantinedFixture {
	t.Helper()
	ctx := context.Background()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "quarantine-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload for quarantine", epoch.Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	var route []lifecycle.State
	switch target {
	case lifecycle.Quarantined:
		route = []lifecycle.State{lifecycle.Transferring, lifecycle.Transferred, lifecycle.Verifying, lifecycle.Quarantined}
	case lifecycle.QuarantinedLost:
		route = []lifecycle.State{
			lifecycle.Transferring, lifecycle.Transferred, lifecycle.Verifying, lifecycle.Verified,
			lifecycle.Committing, lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.Complete,
			lifecycle.QuarantinedLost,
		}
	default:
		t.Fatalf("newQuarantinedFixture: %s is not a quarantine state", target)
	}

	from := lifecycle.Discovered
	for i, to := range route {
		if _, err := journal.RecordTransition(ctx, state.Transition{
			Artifact:   rec.Artifact,
			Key:        "fixture-" + string(to),
			From:       string(from),
			To:         string(to),
			OccurredAt: epoch.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("RecordTransition(%s -> %s): %v", from, to, err)
		}
		from = to
	}

	got, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != string(target) {
		t.Fatalf("precondition failed: state = %q, want %q", got.State, target)
	}

	return quarantinedFixture{svc: svc, journal: journal, artifact: rec.Artifact, localDir: localDir}
}

// TestRevalidateQuarantined_ReportsAVerdictAndWritesNothing is the whole
// contract of the operator-facing "Revalidate" action. The write assertion
// is the load-bearing half: the lifecycle graph has no edge from
// QUARANTINED to any healthy state, so a pass that quietly moved the
// artifact would be moving it somewhere illegal, and a pass that moved it
// nowhere but SAID it had would be worse.
func TestRevalidateQuarantined_ReportsAVerdictAndWritesNothing(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.Quarantined)
	ctx := context.Background()

	// The artifact never reached COMMITTED, so no local final file was
	// recorded; give it one so the check has something real to look at.
	local := fx.localDir + "/backup.dump"
	if err := os.WriteFile(local, []byte("payload for quarantine"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := fx.journal.RecordTransition(ctx, state.Transition{
		Artifact:   fx.artifact,
		Key:        "fixture-local-path",
		From:       string(lifecycle.Quarantined),
		To:         string(lifecycle.Quarantined),
		LocalPath:  &local,
		OccurredAt: epoch,
	}); err != nil {
		t.Fatalf("recording the local path: %v", err)
	}

	before, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The transition log, not only the artifact row. This assertion
	// started as an UpdatedAt comparison and a mutation walked straight
	// past it: a same-state lifecycle.Advance appends a row to
	// state_transitions and leaves updated_at exactly where it was, so a
	// revalidate that quietly recorded an audit write looked identical to
	// one that wrote nothing.
	transitionsBefore, err := fx.journal.(*state.Journal).RecentActivity(ctx, 100)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(transitionsBefore) == 0 {
		t.Fatal("the transition log is empty, so counting it below would prove nothing")
	}

	result, err := fx.svc.RevalidateQuarantined(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("RevalidateQuarantined: %v", err)
	}
	if !result.Checked {
		t.Errorf("Checked = false, so nothing was actually examined: %+v", result)
	}
	if !result.Passed {
		t.Errorf("Passed = false for an intact local file: %+v", result)
	}
	if result.Reason == "" {
		t.Error("Reason is empty, so an operator is given a verdict with no evidence")
	}

	after, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.Quarantined) {
		t.Errorf("State = %q, want it left in %q: a pass may not rehabilitate a quarantined artifact", after.State, lifecycle.Quarantined)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("UpdatedAt moved from %s to %s: revalidate writes nothing", before.UpdatedAt, after.UpdatedAt)
	}

	transitionsAfter, err := fx.journal.(*state.Journal).RecentActivity(ctx, 100)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(transitionsAfter) != len(transitionsBefore) {
		t.Errorf("revalidate appended %d transition(s) to the durable log; it must write nothing at all",
			len(transitionsAfter)-len(transitionsBefore))
	}
}

// TestRevalidateQuarantined_FailsWhenTheLocalCopyIsGone is the negative
// half of the pair above: without it, "Passed" could be a constant.
func TestRevalidateQuarantined_FailsWhenTheLocalCopyIsGone(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.Quarantined)
	ctx := context.Background()

	missing := fx.localDir + "/never-written.dump"
	if _, err := fx.journal.RecordTransition(ctx, state.Transition{
		Artifact:   fx.artifact,
		Key:        "fixture-missing-local-path",
		From:       string(lifecycle.Quarantined),
		To:         string(lifecycle.Quarantined),
		LocalPath:  &missing,
		OccurredAt: epoch,
	}); err != nil {
		t.Fatalf("recording the local path: %v", err)
	}

	result, err := fx.svc.RevalidateQuarantined(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("RevalidateQuarantined: %v", err)
	}
	if result.Passed {
		t.Errorf("Passed = true for a local file that is not there: %+v", result)
	}
}

func TestRevalidateQuarantined_RefusesAnArtifactThatIsNotQuarantined(t *testing.T) {
	fx := newCommittedFixture(t)

	_, err := fx.svc.RevalidateQuarantined(context.Background(), fx.artifact)
	if !errors.Is(err, ErrNotQuarantined) {
		t.Fatalf("error = %v, want ErrNotQuarantined", err)
	}
}

// TestRetryQuarantinedIngestion_ReturnsTheArtifactToDiscovered drives the
// one recovery edge the lifecycle graph gives QUARANTINED.
func TestRetryQuarantinedIngestion_ReturnsTheArtifactToDiscovered(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.Quarantined)
	ctx := context.Background()

	if err := fx.svc.RetryQuarantinedIngestion(ctx, fx.artifact); err != nil {
		t.Fatalf("RetryQuarantinedIngestion: %v", err)
	}

	got, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != string(lifecycle.Discovered) {
		t.Fatalf("State = %q, want %q", got.State, lifecycle.Discovered)
	}

	// The move is durable and auditable, not only in memory: it shows up
	// in the append-only transition log the activity feed reads.
	activity, err := fx.journal.(*state.Journal).RecentActivity(ctx, 5)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(activity) == 0 || activity[0].To != string(lifecycle.Discovered) {
		t.Fatalf("newest transition = %+v, want one into DISCOVERED", activity)
	}
	if !strings.Contains(activity[0].Detail, "operator-triggered retry") {
		t.Errorf("Detail = %q, want it to say who asked for this", activity[0].Detail)
	}
}

// TestRetryQuarantinedIngestion_RefusesAnIrrecoverableLoss. QUARANTINED_LOST
// is reached only from COMPLETE, which confirms the remote source is gone,
// so there is nothing to re-ingest and the graph gives that state no
// outgoing edge at all.
func TestRetryQuarantinedIngestion_RefusesAnIrrecoverableLoss(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.QuarantinedLost)
	ctx := context.Background()

	err := fx.svc.RetryQuarantinedIngestion(ctx, fx.artifact)
	if !errors.Is(err, ErrQuarantineIrrecoverable) {
		t.Fatalf("error = %v, want ErrQuarantineIrrecoverable", err)
	}

	got, gotErr := fx.journal.Get(ctx, fx.artifact)
	if gotErr != nil {
		t.Fatalf("Get: %v", gotErr)
	}
	if got.State != string(lifecycle.QuarantinedLost) {
		t.Errorf("State = %q, want it left in %q", got.State, lifecycle.QuarantinedLost)
	}
}

func TestRetryQuarantinedIngestion_RefusesAnArtifactThatIsNotQuarantined(t *testing.T) {
	fx := newCommittedFixture(t)

	err := fx.svc.RetryQuarantinedIngestion(context.Background(), fx.artifact)
	if !errors.Is(err, ErrNotQuarantined) {
		t.Fatalf("error = %v, want ErrNotQuarantined", err)
	}
}

// newReinstatableFixture is the fixture issue #220 is actually about: an
// artifact that really walked the pipeline to COMMITTED, whose durable
// local final file exists with the sha256 recorded at VERIFIED, and which
// a later content check then quarantined.
//
// newQuarantinedFixture above cannot serve here, deliberately: it routes
// through VERIFYING, so its artifact never committed and reinstating it is
// refused by name. That refusal is itself worth having a fixture for, and
// it is why these two are separate rather than one with a flag.
func newReinstatableFixture(t *testing.T) quarantinedFixture {
	t.Helper()
	ctx := context.Background()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "reinstate-test"}

	const payload = "payload for reinstatement"
	tr := newFakeTransport()
	tr.put("backup.dump", payload, epoch.Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	local := localDir + "/backup.dump"
	mustWriteFile(t, local, payload)
	sum := sha256.Sum256([]byte(payload))
	hashes := &state.HashUpdate{Alg: "sha256", Hash: hex.EncodeToString(sum[:])}

	steps := []struct {
		from, to  lifecycle.State
		localPath *string
		hashes    *state.HashUpdate
	}{
		{lifecycle.Discovered, lifecycle.Transferring, nil, nil},
		{lifecycle.Transferring, lifecycle.Transferred, nil, nil},
		{lifecycle.Transferred, lifecycle.Verifying, nil, nil},
		{lifecycle.Verifying, lifecycle.Verified, nil, hashes},
		{lifecycle.Verified, lifecycle.Committing, nil, nil},
		{lifecycle.Committing, lifecycle.Committed, &local, nil},
		{lifecycle.Committed, lifecycle.Quarantined, nil, nil},
	}
	for i, s := range steps {
		if _, err := journal.RecordTransition(ctx, state.Transition{
			Artifact:   rec.Artifact,
			Key:        "reinstatable-" + string(s.to),
			From:       string(s.from),
			To:         string(s.to),
			LocalPath:  s.localPath,
			Hashes:     s.hashes,
			OccurredAt: epoch.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("RecordTransition(%s -> %s): %v", s.from, s.to, err)
		}
	}

	return quarantinedFixture{svc: svc, journal: journal, artifact: rec.Artifact, localDir: localDir}
}

func transitionRows(t *testing.T, j Journal) int {
	t.Helper()
	rows, err := j.(*state.Journal).RecentActivity(context.Background(), 1000)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	return len(rows)
}

// The whole point of issue #220: an operator with a provably intact local
// copy gets the artifact back as a restore point, and the append-only log
// says it was re-trusted rather than never distrusted.
func TestReinstateQuarantined_ReturnsAnIntactBackupToService(t *testing.T) {
	fx := newReinstatableFixture(t)
	ctx := context.Background()

	result, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "confirmed a false positive")
	if err != nil {
		t.Fatalf("ReinstateQuarantined: %v", err)
	}
	if !result.Reinstated || !result.Passed || !result.Checked {
		t.Fatalf("result = %+v, want a checked, passing, applied reinstatement", result)
	}
	if result.NewState != lifecycle.Committed {
		t.Fatalf("NewState = %q, want COMMITTED", result.NewState)
	}

	rec, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Committed) {
		t.Fatalf("journal state = %q, want COMMITTED", rec.State)
	}

	at, ok, err := fx.journal.LastTransition(ctx, fx.artifact, string(lifecycle.Quarantined), string(lifecycle.Committed))
	if err != nil || !ok {
		t.Fatalf("LastTransition(QUARANTINED -> COMMITTED) = (ok %v, err %v), want (true, nil): the audit record is the edge itself", ok, err)
	}
	if at.IsZero() {
		t.Error("the reinstatement was recorded with a zero timestamp")
	}
}

// A failing check is a verdict about the backup, not a failed request, and
// it must write nothing at all. The counted observation is the transition
// log, not UpdatedAt: this package has already been bitten once by an
// UpdatedAt assertion walking past a same-state write (see
// TestRevalidateQuarantined_ReportsAVerdictAndWritesNothing).
func TestReinstateQuarantined_ReportsAFailingCheckAndWritesNothing(t *testing.T) {
	fx := newReinstatableFixture(t)
	ctx := context.Background()

	// Corrupt the durable local copy so the recorded hash no longer
	// matches. Nothing else about the fixture changes.
	mustWriteFile(t, fx.localDir+"/backup.dump", "something else entirely")

	before := transitionRows(t, fx.journal)

	result, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "")
	if err != nil {
		t.Fatalf("ReinstateQuarantined returned an error for a failing check; that is a verdict, not a failure: %v", err)
	}
	if result.Reinstated {
		t.Fatal("a backup whose durable local copy no longer matches its recorded hash was reinstated")
	}
	if !result.Checked || result.Passed {
		t.Fatalf("result = %+v, want a checked, failing verdict", result)
	}
	if !strings.Contains(result.Reason, "hash") {
		t.Errorf("Reason = %q, want it to say what was found", result.Reason)
	}

	if after := transitionRows(t, fx.journal); after != before {
		t.Fatalf("state_transitions grew from %d to %d rows on a failing check", before, after)
	}
	rec, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Quarantined) {
		t.Fatalf("journal state = %q, want it left at QUARANTINED", rec.State)
	}
}

// The other fixture's artifact never committed, so its local path is still
// a .partial and there is no durable copy to re-trust. internal/lifecycle
// refuses it by name, and this package must surface that refusal rather
// than swallow it into a verdict.
func TestReinstateQuarantined_RefusesAnArtifactThatNeverCommitted(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.Quarantined)
	ctx := context.Background()

	// A recorded hash the file still matches, so the evidence itself is
	// conclusive and this test isolates the one refusal it is about
	// rather than tripping the evidence rule on the way there.
	const payload = "payload for quarantine"
	local := fx.localDir + "/backup.dump"
	mustWriteFile(t, local, payload)
	sum := sha256.Sum256([]byte(payload))
	if _, err := fx.journal.RecordTransition(ctx, state.Transition{
		Artifact:   fx.artifact,
		Key:        "fixture-local-path",
		From:       string(lifecycle.Quarantined),
		To:         string(lifecycle.Quarantined),
		LocalPath:  &local,
		Hashes:     &state.HashUpdate{Alg: "sha256", Hash: hex.EncodeToString(sum[:])},
		OccurredAt: epoch,
	}); err != nil {
		t.Fatalf("recording the local path: %v", err)
	}

	before := transitionRows(t, fx.journal)

	_, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "")
	if err == nil {
		t.Fatal("an artifact that never committed was reinstated to COMMITTED")
	}
	if _, ok := lifecycle.AsNeverHeldTargetState(err); !ok {
		t.Fatalf("err = %v, want a *lifecycle.NeverHeldTargetStateError", err)
	}
	if after := transitionRows(t, fx.journal); after != before {
		t.Fatalf("state_transitions grew from %d to %d rows on a refused reinstatement", before, after)
	}
}

// QUARANTINED_LOST reinstates to COMPLETE, the state it came from. This is
// the case an unmounted volume produces: FR-17 finds every COMPLETE
// artifact's local copy missing and writes the whole set off, and before
// this there was no way back at all.
func TestReinstateQuarantined_ReturnsAQuarantinedLostBackupToComplete(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.QuarantinedLost)
	ctx := context.Background()

	const payload = "payload for quarantine"
	local := fx.localDir + "/backup.dump"
	mustWriteFile(t, local, payload)
	sum := sha256.Sum256([]byte(payload))
	if _, err := fx.journal.RecordTransition(ctx, state.Transition{
		Artifact:   fx.artifact,
		Key:        "fixture-lost-local",
		From:       string(lifecycle.QuarantinedLost),
		To:         string(lifecycle.QuarantinedLost),
		LocalPath:  &local,
		Hashes:     &state.HashUpdate{Alg: "sha256", Hash: hex.EncodeToString(sum[:])},
		OccurredAt: epoch,
	}); err != nil {
		t.Fatalf("recording the local path: %v", err)
	}

	result, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "the backup volume was not mounted when the check ran")
	if err != nil {
		t.Fatalf("ReinstateQuarantined: %v", err)
	}
	if !result.Reinstated || result.NewState != lifecycle.Complete {
		t.Fatalf("result = %+v, want a reinstatement to COMPLETE", result)
	}
}

// The mixed verdict, end to end: the backup set has a validator, it runs
// and passes, and the recorded hash no longer matches. The copy the hook
// just exercised is demonstrably not the copy this manager verified, so
// "the validator passed" must not carry the reinstatement.
//
// This is the case that a rule written only in positives lets through: the
// evidence really does contain a check that could have failed and did not.
// What refuses it is this package reporting that something else DID fail.
func TestReinstateQuarantined_RefusesAPassingValidatorWhenTheHashNoLongerMatches(t *testing.T) {
	fx := newReinstatableFixture(t)
	ctx := context.Background()

	// A validator that always passes, configured on the backup set the
	// service reads. Nothing about it is wrong; the artifact is.
	script := filepath.Join(t.TempDir(), "always-pass.sh")
	mustWriteFile(t, script, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	bs := testBackupSet(t, fx.localDir)
	bs.Validation = config.Validation{Command: &config.Command{Executable: script, Timeout: config.Duration(10 * time.Second)}}
	fx.svc.Config = testConfig(t, testSource("production", bs))

	// Positive control first, on the untouched fixture: with the same
	// validator configured and the hash still matching, the reinstatement
	// goes through. Without this, the refusal below would be equally
	// consistent with the validator wiring being broken.
	control, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "")
	if err != nil {
		t.Fatalf("positive control: an intact copy with a passing validator was refused: %v", err)
	}
	if !control.Reinstated {
		t.Fatalf("positive control: result = %+v, want a reinstatement", control)
	}

	// Now the real case, on a fresh fixture whose local copy has changed.
	fx = newReinstatableFixture(t)
	fx.svc.Config = testConfig(t, testSource("production", testBackupSetWithValidator(t, fx.localDir, script)))
	mustWriteFile(t, fx.localDir+"/backup.dump", "something else entirely")

	before := transitionRows(t, fx.journal)

	result, err := fx.svc.ReinstateQuarantined(ctx, fx.artifact, "")
	if err != nil {
		t.Fatalf("ReinstateQuarantined: %v", err)
	}
	if result.Reinstated {
		t.Fatal("a passing validator reinstated a backup whose bytes are not the bytes that were verified")
	}
	if after := transitionRows(t, fx.journal); after != before {
		t.Fatalf("state_transitions grew from %d to %d rows", before, after)
	}
}

func testBackupSetWithValidator(t *testing.T, localDir, script string) config.BackupSet {
	t.Helper()
	bs := testBackupSet(t, localDir)
	bs.Validation = config.Validation{Command: &config.Command{Executable: script, Timeout: config.Duration(10 * time.Second)}}
	return bs
}

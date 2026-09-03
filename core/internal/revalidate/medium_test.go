package revalidate

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// countingStore records what a revalidation pass actually asked the medium
// for. The counters are the point: FR-31's "automatic medium revalidation
// never downloads" is only checkable if the fixture can say whether a
// download happened, which is the same request-accounting the issue's
// INTEGRATION step asks for.
type countingStore struct {
	size    int64
	statErr error

	stats  int
	opens  int
	digest int
}

func (s *countingStore) StatObject(_ context.Context, _ transport.Medium, key string) (transport.ObjectInfo, error) {
	s.stats++
	if s.statErr != nil {
		return transport.ObjectInfo{}, s.statErr
	}
	return transport.ObjectInfo{Key: key, Size: s.size}, nil
}

func (s *countingStore) OpenObject(_ context.Context, _ transport.Medium, _ string) (io.ReadCloser, error) {
	s.opens++
	return io.NopCloser(strings.NewReader("")), nil
}

func (s *countingStore) ObjectChecksum(_ context.Context, _ transport.Medium, _ string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	s.digest++
	return transport.ChecksumAttestation{}, transport.NewError(transport.UnsupportedCapability, "object_checksum", errors.New("this backend exposes md5 and nothing else"))
}

// fixedMediums is a Mediums with one entry.
type fixedMediums struct {
	id string
}

func (m fixedMediums) MediumFor(id string) (transport.Medium, bool) {
	if id != m.id {
		return transport.Medium{}, false
	}
	return transport.Medium{ID: id, Type: transport.MediumTypeS3, Bucket: "nas-backups"}, true
}

// moveToMedium takes an artifact that already has a durable local copy and
// makes its only ACTIVE placement a medium one, which is the state an
// artifact is in after the move engine (#238) finishes with it.
//
// It writes the placements directly through the journal, because that is
// the only writer there is: nothing in Phase 1 moves anything, and a test
// that waited for a mover would be a test that never runs.
func moveToMedium(t *testing.T, j *state.Journal, artifact model.ArtifactID, mediumID string, content []byte, at time.Time) {
	t.Helper()
	ctx := context.Background()
	size := int64(len(content))

	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + ":gone-local", From: "COMPLETE", To: "COMPLETE", OccurredAt: at,
		Placement: &state.PlacementUpdate{
			Medium: state.MediumLocal, Location: "/backups/pg/" + artifact.Name,
			Status: state.PlacementGone,
		},
	}); err != nil {
		t.Fatalf("retiring the local placement: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + ":on-medium", From: "COMPLETE", To: "COMPLETE", OccurredAt: at,
		Placement: &state.PlacementUpdate{
			Medium: mediumID, Location: "rclone-manager/production/postgres-primary/" + artifact.Name,
			Size: &size, Hash: sha256Hex(content), HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("recording the medium placement: %v", err)
	}
}

// addLocalPlacement records the ACTIVE local placement that lifecycle's
// own Commit records in production.
//
// The helpers in revalidate_test.go predate placements and record none, so
// a record they build answers ReadableLocalPath out of the LocalPath
// fallback. That is a real Phase 1 shape and worth keeping, but it is not
// the shape a test about ACTIVE local placements can be written against.
func addLocalPlacement(t *testing.T, j *state.Journal, artifact model.ArtifactID, localPath string, content []byte, at time.Time) {
	t.Helper()
	size := int64(len(content))
	if _, err := j.RecordTransition(context.Background(), state.Transition{
		Artifact: artifact, Key: artifact.String() + ":local-placement", From: "COMPLETE", To: "COMPLETE", OccurredAt: at,
		Placement: &state.PlacementUpdate{
			Medium: state.MediumLocal, Location: localPath, Size: &size,
			Hash: sha256Hex(content), HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("recording the local placement: %v", err)
	}
}

// addMediumPlacement records a medium placement WITHOUT retiring the local
// one, which is the state an artifact is in mid-move: the copy has been
// uploaded and the source has not been deleted yet, so both placements are
// ACTIVE at once. FR-30 is why that window exists at all, since the source
// copy survives every uncertainty.
func addMediumPlacement(t *testing.T, j *state.Journal, artifact model.ArtifactID, mediumID string, content []byte, at time.Time) {
	t.Helper()
	size := int64(len(content))
	if _, err := j.RecordTransition(context.Background(), state.Transition{
		Artifact: artifact, Key: artifact.String() + ":also-on-medium", From: "COMPLETE", To: "COMPLETE", OccurredAt: at,
		Placement: &state.PlacementUpdate{
			Medium: mediumID, Location: "rclone-manager/production/postgres-primary/" + artifact.Name,
			Size: &size, Hash: sha256Hex(content), HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("recording the second placement: %v", err)
	}
}

// TestRevalidationOfAMediumPlacementIsExistenceAndSaysSo is the issue's
// own behavioural contract: the placement is existence-checked, the
// recorded and reported class is existence, and no bytes are downloaded.
func TestRevalidationOfAMediumPlacementIsExistenceAndSaysSo(t *testing.T) {
	ctx := context.Background()
	j := openJournal(t)
	artifact := artifactNamed(t, "on-medium.dump")
	content := []byte("bytes that now live in a bucket")
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)

	completeArtifact(t, j, artifact, content, long)
	moveToMedium(t, j, artifact, "offsite_s3", content, long)

	store := &countingStore{size: int64(len(content))}
	deps := Deps{Journal: j, Store: store, Mediums: fixedMediums{id: "offsite_s3"}}
	cfg := config.Revalidation{Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10, Hash: true}

	report, err := Run(ctx, deps, artifact.Set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", report.Findings)
	}
	f := report.Findings[0]

	if !f.Checked {
		t.Fatalf("the artifact was not checked at all: %s", f.Reason)
	}
	if !f.Passed {
		t.Fatalf("the existence check failed against an object that is there: %s", f.Reason)
	}
	if f.Class != placement.Existence {
		t.Errorf("Class = %q, want %q; a HEAD proves nothing about the bytes and must not be reported as anything stronger", f.Class, placement.Existence)
	}
	if store.opens != 0 {
		t.Errorf("the automatic pass downloaded the object %d times; FR-31 makes anything that costs egress operator-initiated, because a surprise bill is how a safety feature gets turned off", store.opens)
	}
	if store.digest != 0 {
		t.Errorf("the automatic pass asked for %d attestations; the ceiling is existence", store.digest)
	}
	if store.stats != 1 {
		t.Errorf("the medium was statted %d times, want exactly 1", store.stats)
	}

	// And the durable record says the same thing the Finding does. This is
	// the half that matters six months later, when nobody has the Finding
	// any more and an operator is reading the journal to find out when
	// this artifact was last actually looked at.
	// Read out of the append-only transition log rather than through
	// LastEnteredDetail, which deliberately ignores a same-state write:
	// a revalidation pass IS a same-state write, which is exactly why it
	// does not count as the artifact having freshly "entered" anything.
	activity, err := j.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	detail := ""
	for _, a := range activity {
		if a.Artifact == artifact && strings.Contains(a.Detail, "revalidation") {
			detail = a.Detail
			break
		}
	}
	if detail == "" {
		t.Fatalf("no revalidation transition was recorded for the pass; the log holds %+v", activity)
	}
	if !strings.Contains(detail, string(placement.Existence)) {
		t.Errorf("the recorded detail %q does not name the class that ran", detail)
	}
	if strings.Contains(detail, string(placement.Content)) {
		t.Errorf("the recorded detail %q names content verification for a pass that only HEADed the object", detail)
	}
}

// TestAMediumPlacementThisDeploymentCannotReachIsNotAPass is the
// checked-versus-passed distinction extended to mediums, and it is the
// case that protects the due-ness clock. An unreachable bucket must leave
// the artifact selectable next cycle rather than looking freshly checked.
func TestAMediumPlacementThisDeploymentCannotReachIsNotAPass(t *testing.T) {
	ctx := context.Background()
	content := []byte("bytes in a bucket nobody here can reach")
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)
	cfg := config.Revalidation{Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10, Hash: true}

	for _, tc := range []struct {
		name string
		deps func(j *state.Journal) Deps
	}{
		{
			name: "no store at all",
			deps: func(j *state.Journal) Deps { return Deps{Journal: j} },
		},
		{
			name: "a medium the configuration does not name",
			deps: func(j *state.Journal) Deps {
				return Deps{Journal: j, Store: &countingStore{}, Mediums: fixedMediums{id: "some_other_medium"}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := openJournal(t)
			artifact := artifactNamed(t, "on-medium.dump")
			completeArtifact(t, j, artifact, content, long)
			moveToMedium(t, j, artifact, "offsite_s3", content, long)

			before, err := j.Get(ctx, artifact)
			if err != nil {
				t.Fatalf("Get before: %v", err)
			}

			report, err := Run(ctx, tc.deps(j), artifact.Set, cfg)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(report.Findings) != 1 {
				t.Fatalf("Findings = %+v, want exactly one", report.Findings)
			}
			if report.Findings[0].Checked {
				t.Errorf("the pass reported itself as having checked something: %s", report.Findings[0].Reason)
			}
			if report.Findings[0].Class != "" {
				t.Errorf("an unchecked pass reported class %q", report.Findings[0].Class)
			}

			after, err := j.Get(ctx, artifact)
			if err != nil {
				t.Fatalf("Get after: %v", err)
			}
			if !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("the due-ness clock moved from %s to %s for an artifact nothing checked; it would then look freshly verified until the interval elapsed again",
					before.UpdatedAt, after.UpdatedAt)
			}
			if after.State != before.State {
				t.Errorf("an unreachable medium changed the artifact's state from %q to %q", before.State, after.State)
			}
		})
	}
}

// TestAMediumThatDoesNotAnswerIsReportedAsAnError is the other half of
// the same distinction. A medium this deployment was never configured to
// reach is a configuration fact, which an operator reads past; a medium
// that was there to ask and did not answer is a backup nobody could check,
// which somebody should find out about. So the first is an unchecked
// finding and the second is an error, and neither touches the journal.
func TestAMediumThatDoesNotAnswerIsReportedAsAnError(t *testing.T) {
	ctx := context.Background()
	j := openJournal(t)
	artifact := artifactNamed(t, "on-medium.dump")
	content := []byte("bytes in a bucket that did not answer")
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)

	completeArtifact(t, j, artifact, content, long)
	moveToMedium(t, j, artifact, "offsite_s3", content, long)

	before, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	deps := Deps{
		Journal: j,
		Store:   &countingStore{statErr: transport.NewError(transport.Transient, "stat_object", errors.New("connection reset"))},
		Mediums: fixedMediums{id: "offsite_s3"},
	}
	cfg := config.Revalidation{Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10, Hash: true}

	report, err := Run(ctx, deps, artifact.Set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("Errors = %+v, want exactly one; a bucket that did not answer is a backup nobody could check", report.Errors)
	}
	if len(report.Findings) != 0 {
		t.Errorf("Findings = %+v, want none: an artifact nothing could check has no verdict", report.Findings)
	}

	after, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the due-ness clock moved from %s to %s for an artifact nothing checked", before.UpdatedAt, after.UpdatedAt)
	}
	if after.State != before.State {
		t.Errorf("an unreachable medium changed the artifact's state from %q to %q", before.State, after.State)
	}
}

// TestAMissingObjectOnAMediumQuarantines is the failing half: existence is
// a weak check, but a weak check that FAILS is still a real verdict about
// the artifact, and it routes exactly where a failed local recheck routes.
func TestAMissingObjectOnAMediumQuarantines(t *testing.T) {
	ctx := context.Background()
	j := openJournal(t)
	artifact := artifactNamed(t, "on-medium.dump")
	content := []byte("bytes that are no longer in the bucket")
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)

	completeArtifact(t, j, artifact, content, long)
	moveToMedium(t, j, artifact, "offsite_s3", content, long)

	store := &countingStore{statErr: transport.NewError(transport.NotFound, "stat_object", errors.New("object not found"))}
	deps := Deps{Journal: j, Store: store, Mediums: fixedMediums{id: "offsite_s3"}}
	cfg := config.Revalidation{Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10, Hash: true}

	report, err := Run(ctx, deps, artifact.Set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", report.Findings)
	}
	f := report.Findings[0]
	if !f.Checked || f.Passed {
		t.Fatalf("an object that is gone was not reported as a failed check: %+v", f)
	}
	if f.Class != placement.Existence {
		t.Errorf("Class = %q, want %q", f.Class, placement.Existence)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != "QUARANTINED_LOST" {
		t.Errorf("state = %q, want QUARANTINED_LOST: a COMPLETE artifact whose only copy is gone has already had its remote source deleted", rec.State)
	}
}

// TestALocalPlacementStillGetsTodaysCheck is the regression half of FR-31
// stated positively: local placements keep today's behaviour exactly, and
// the class they achieve is the content check they have always run.
func TestALocalPlacementStillGetsTodaysCheck(t *testing.T) {
	ctx := context.Background()
	j := openJournal(t)
	artifact := artifactNamed(t, "still-local.dump")
	content := []byte("bytes still on the NAS")
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)

	completeArtifact(t, j, artifact, content, long)

	store := &countingStore{}
	deps := Deps{Journal: j, Store: store, Mediums: fixedMediums{id: "offsite_s3"}}
	cfg := config.Revalidation{Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10, Hash: true}

	report, err := Run(ctx, deps, artifact.Set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", report.Findings)
	}
	f := report.Findings[0]
	if !f.Checked || !f.Passed {
		t.Fatalf("a local artifact did not pass its own content check: %+v", f)
	}
	if f.Class != placement.Content {
		t.Errorf("Class = %q, want %q", f.Class, placement.Content)
	}
	if store.stats != 0 || store.opens != 0 || store.digest != 0 {
		t.Errorf("a local artifact's revalidation touched the medium store: %d stats, %d opens, %d digests", store.stats, store.opens, store.digest)
	}
}

// TestAnArtifactMidMoveIsStillCheckedLocally is FR-31's "local placements
// keep today's behaviour exactly" for the one case where the sentence has
// any content: an artifact that has BOTH an ACTIVE local placement and an
// ACTIVE medium placement, which is where a move leaves it between the
// upload and the source delete.
//
// Without this, the local fork is untestable: every other case in this
// file has exactly one active placement, so removing the rule that a local
// placement wins changes nothing and the rule is a comment rather than a
// behaviour. Getting it wrong is not cosmetic either. Checking the medium
// copy instead would downgrade a mid-move artifact from the content check
// it got yesterday to a HEAD, silently, for as long as the move takes.
func TestAnArtifactMidMoveIsStillCheckedLocally(t *testing.T) {
	ctx := context.Background()
	j := openJournal(t)
	artifact := artifactNamed(t, "mid-move.dump")
	content := []byte("bytes that are in two places at once")
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)

	localPath := completeArtifact(t, j, artifact, content, long)
	addLocalPlacement(t, j, artifact, localPath, content, long)
	addMediumPlacement(t, j, artifact, "offsite_s3", content, long)

	// The fixture has to actually be in the state this test is named for,
	// or it is another single-placement test wearing a longer name.
	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, hasLocal := rec.LocalPlacement(); !hasLocal {
		t.Fatalf("the fixture has no ACTIVE local placement, so it is not mid-move: %+v", rec.Placements)
	}
	medium := 0
	for _, p := range rec.Placements {
		if !p.IsLocal() && p.Status == state.PlacementActive {
			medium++
		}
	}
	if medium != 1 {
		t.Fatalf("the fixture has %d ACTIVE medium placements, want exactly 1: %+v", medium, rec.Placements)
	}

	store := &countingStore{size: int64(len(content))}
	deps := Deps{Journal: j, Store: store, Mediums: fixedMediums{id: "offsite_s3"}}
	cfg := config.Revalidation{Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10, Hash: true}

	report, err := Run(ctx, deps, artifact.Set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", report.Findings)
	}
	f := report.Findings[0]
	if !f.Checked || !f.Passed {
		t.Fatalf("a mid-move artifact did not pass: %+v", f)
	}
	if f.Class != placement.Content {
		t.Errorf("Class = %q, want %q: an artifact that still has its local copy is checked the way it was checked yesterday, not downgraded to a HEAD for the duration of a move", f.Class, placement.Content)
	}
	if store.stats != 0 || store.opens != 0 || store.digest != 0 {
		t.Errorf("the medium was consulted for an artifact whose local copy is still there: %d stats, %d opens, %d digests", store.stats, store.opens, store.digest)
	}
}

// TestAConfiguredRestoreTestSaysItDidNotRunOnAMedium is the other half of
// "report the verification that happened".
//
// A restore test opens the artifact, so running one against a bucket is a
// download, and FR-31 makes anything that costs egress operator-initiated.
// Skipping it is right. Skipping it silently is not: the operator asked
// for two tiers, one of them stopped running the day the bytes moved, and
// a green pass that says nothing about it is how a safety feature becomes
// decorative.
func TestAConfiguredRestoreTestSaysItDidNotRunOnAMedium(t *testing.T) {
	ctx := context.Background()
	j := openJournal(t)
	artifact := artifactNamed(t, "on-medium.dump")
	content := []byte("bytes that now live in a bucket")
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)

	completeArtifact(t, j, artifact, content, long)
	moveToMedium(t, j, artifact, "offsite_s3", content, long)

	// A hook that would pass loudly if anything ever ran it, so a silent
	// skip cannot be mistaken for a silent pass.
	hook := mustScript(t, "exit 0")
	store := &countingStore{size: int64(len(content))}
	deps := Deps{Journal: j, Store: store, Mediums: fixedMediums{id: "offsite_s3"}}
	cfg := config.Revalidation{
		Interval:    config.Duration(24 * time.Hour),
		MaxPerCycle: 10,
		Hash:        true,
		Command:     &config.Command{Executable: hook, Timeout: config.Duration(30 * time.Second)},
	}

	report, err := Run(ctx, deps, artifact.Set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", report.Findings)
	}
	f := report.Findings[0]
	if !f.Checked || !f.Passed {
		t.Fatalf("the existence check did not pass: %+v", f)
	}
	if f.Class != placement.Existence {
		t.Errorf("Class = %q, want %q", f.Class, placement.Existence)
	}
	if !strings.Contains(f.Reason, "restore-test hook did not run") {
		t.Errorf("a pass with a configured restore test says nothing about it not having run: %q", f.Reason)
	}
	if store.opens != 0 {
		t.Errorf("the restore-test hook downloaded the object %d times", store.opens)
	}

	// And the journal says it too, because the Finding is gone in an hour
	// and the audit trail is what somebody reads in six months.
	activity, err := j.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	found := false
	for _, a := range activity {
		if a.Artifact == artifact && strings.Contains(a.Detail, "restore-test hook did not run") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no recorded transition says the restore test did not run; the log holds %+v", activity)
	}
}

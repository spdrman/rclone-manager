package mediumcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// A preflight exists in order to fail, so most of this suite is about
// making it fail one way at a time. fakeStore is shaped for exactly that:
// every misbehaviour a real bucket can show up with, a refused credential,
// an absent bucket, a denied write, an endpoint that stores a class it was
// not asked for, a delete that reports success and removes nothing, is a
// single field. No test has to build a broken world to reach the one
// failure it is actually about.
//
// The pairs are where most of the value sits. A credential that could not
// be obtained and one the endpoint rejected are two different people's
// jobs, so they are asserted apart rather than both settling for "not OK".
// An endpoint that reports no storage class at all is an absence rather
// than a failure, and it is asserted next to the case where it reports the
// wrong one. The attested class is driven both ways, against an endpoint
// that cannot attest and one that can, because a refusal that fired
// unconditionally would satisfy the first of those and prove nothing.
//
// Two tests cover what a Report must never contain, and they work from
// opposite ends. One plants a canary everywhere a medium can reference a
// secret, runs a real check, and searches the serialised report for it,
// with a positive control proving the canary was in play and did reach the
// log. The other counts the fields on a serialised Report, so a field added
// later that could carry a credential fails here rather than in an exported
// API response.

// --- a MediumStore that behaves like a bucket, and can be made to
// misbehave one way at a time ---

type fakeStore struct {
	objects map[string][]byte

	// storedClass is what the endpoint says it stored an object as. Empty
	// means "this endpoint reports no class", which is a real answer a
	// plain S3-compatible server gives.
	storedClass string

	statErr     error
	uploadErr   error
	openErr     error
	checksumErr error
	deleteErr   error

	// attests, when true, makes ObjectChecksum answer instead of refusing,
	// which is what a future rclone surfacing x-amz-checksum-sha256 would
	// look like from this side of the boundary.
	attests bool

	// corruptOnRead replaces what OpenObject hands back, without touching
	// what StatObject reports: an endpoint that says the right size and
	// returns the wrong bytes is the shape a size-only check misses.
	corruptOnRead []byte

	// keepOnDelete makes DeleteObject report success and change nothing,
	// which is exactly what the adapter measured a missing bucket doing
	// before confirmBucket existed.
	keepOnDelete bool

	uploads int
	deletes int
}

func newFakeStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func notFound(op string) error {
	return transport.NewError(transport.NotFound, op, errors.New("no such key"))
}

func (f *fakeStore) StatObject(_ context.Context, _ transport.Medium, key string) (transport.ObjectInfo, error) {
	if f.statErr != nil {
		return transport.ObjectInfo{}, f.statErr
	}
	body, ok := f.objects[key]
	if !ok {
		return transport.ObjectInfo{}, notFound("stat_object")
	}
	return transport.ObjectInfo{Key: key, Size: int64(len(body)), StorageClass: f.storedClass}, nil
}

func (f *fakeStore) UploadFromLocal(_ context.Context, _ transport.Medium, localPath, key string, _ transport.UploadOptions) (transport.UploadResult, error) {
	f.uploads++
	if f.uploadErr != nil {
		return transport.UploadResult{}, f.uploadErr
	}
	body, err := readFile(localPath)
	if err != nil {
		return transport.UploadResult{}, err
	}
	f.objects[key] = body
	return transport.UploadResult{Key: key, BytesUploaded: int64(len(body))}, nil
}

func (f *fakeStore) OpenObject(_ context.Context, _ transport.Medium, key string) (io.ReadCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, notFound("open_object")
	}
	if f.corruptOnRead != nil {
		body = f.corruptOnRead
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

func (f *fakeStore) ObjectChecksum(_ context.Context, _ transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	if f.checksumErr != nil {
		return transport.ChecksumAttestation{}, f.checksumErr
	}
	if !f.attests {
		return transport.ChecksumAttestation{}, transport.NewError(transport.UnsupportedCapability, "object_checksum",
			errors.New("this endpoint serves no full-object digest"))
	}
	return transport.ChecksumAttestation{Algorithm: alg, Value: "deadbeef"}, nil
}

func (f *fakeStore) DeleteObject(_ context.Context, _ transport.Medium, key string) error {
	f.deletes++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if f.keepOnDelete {
		return nil
	}
	delete(f.objects, key)
	return nil
}

func (f *fakeStore) ListObjects(context.Context, transport.Medium, string) ([]transport.ObjectInfo, error) {
	return nil, errors.New("fakeStore: ListObjects is not part of the preflight")
}

func (f *fakeStore) RestoreStatus(context.Context, transport.Medium, string) (*transport.RestoreState, error) {
	return nil, errors.New("fakeStore: RestoreStatus is not part of the preflight")
}

func (f *fakeStore) InitiateRestore(context.Context, transport.Medium, string, int) error {
	return errors.New("fakeStore: InitiateRestore is not part of the preflight")
}

var _ transport.MediumStore = (*fakeStore)(nil)

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// --- helpers ---

func testMedium() transport.Medium {
	return transport.Medium{
		ID:           "offsite_s3",
		Type:         transport.MediumTypeS3,
		Region:       "us-east-1",
		Bucket:       "nas-backups",
		Prefix:       "backups",
		StorageClass: config.StorageClassStandardIA,
		Credentials:  transport.MediumCredentials{Env: "BACKUP_S3_OFFSITE"},
	}
}

func preflight(t *testing.T, store transport.MediumStore, medium transport.Medium, class placement.Class) Report {
	t.Helper()
	report, err := Run(context.Background(), Deps{Store: store}, medium, class)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Checks) != len(Steps) {
		t.Fatalf("report carries %d checks, want one per step (%d): %+v", len(report.Checks), len(Steps), report.Checks)
	}
	for i, step := range Steps {
		if report.Checks[i].Step != step {
			t.Fatalf("check %d is for %q, want %q: the order a surface renders has to be the order this package declares", i, report.Checks[i].Step, step)
		}
	}
	return report
}

func checkFor(t *testing.T, r Report, step Step) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Step == step {
			return c
		}
	}
	t.Fatalf("no check for step %q in %+v", step, r.Checks)
	return Check{}
}

func wantOutcome(t *testing.T, r Report, step Step, outcome Outcome) Check {
	t.Helper()
	c := checkFor(t, r, step)
	if c.Outcome != outcome {
		t.Fatalf("step %q = %q (%s), want %q", step, c.Outcome, c.Detail, outcome)
	}
	return c
}

// --- allow: the whole thing works ---

func TestPreflight_AWorkingMedium_ProvesEveryStepAndRollsItsProbeBack(t *testing.T) {
	store := newFakeStore()
	store.storedClass = config.StorageClassStandardIA

	report := preflight(t, store, testMedium(), placement.Content)
	if !report.OK {
		t.Fatalf("report is not OK: %+v", report.Failures())
	}
	for _, step := range Steps {
		wantOutcome(t, report, step, Passed)
	}

	// The probe is gone. A preflight that leaves an object behind has put
	// something in an operator's bucket that nothing in this product will
	// ever clean up.
	if len(store.objects) != 0 {
		t.Fatalf("the bucket still holds %d object(s) after a successful preflight: %v", len(store.objects), keysOf(store))
	}
	if store.uploads != 1 || store.deletes != 1 {
		t.Fatalf("uploads=%d deletes=%d, want exactly one of each", store.uploads, store.deletes)
	}
}

// --- deny: the credential cannot be obtained ---

func TestPreflight_UnreadableCredential_IsItsOwnAnswerAndStopsBeforeTheEndpoint(t *testing.T) {
	store := newFakeStore()
	store.statErr = transport.NewError(transport.Configuration, "medium_credentials",
		fmt.Errorf("%w: medium %q: credentials.file is not accessible", transport.ErrCredentialsUnavailable, "offsite_s3"))

	report := preflight(t, store, testMedium(), placement.Content)
	if report.OK {
		t.Fatal("a medium whose credential cannot be obtained reported OK")
	}
	creds := wantOutcome(t, report, StepCredentials, Failed)
	if creds.Category != transport.Configuration.String() {
		t.Fatalf("credentials category = %q, want %q", creds.Category, transport.Configuration)
	}
	wantOutcome(t, report, StepReach, Skipped)
	for _, step := range []Step{StepDeliverable, StepWrite, StepReadBack, StepStorageClass, StepVerification, StepDelete} {
		wantOutcome(t, report, step, Skipped)
	}
	if store.uploads != 0 {
		t.Fatalf("uploads = %d, want 0: nothing may be sent when there was no credential to send it with", store.uploads)
	}
}

// --- deny: the credential is obtained and rejected ---

func TestPreflight_RejectedCredential_IsToldApartFromOneThatCouldNotBeObtained(t *testing.T) {
	store := newFakeStore()
	store.statErr = transport.NewError(transport.Authentication, "stat_object", errors.New("403"))

	report := preflight(t, store, testMedium(), placement.Content)
	if report.OK {
		t.Fatal("a medium whose credential is rejected reported OK")
	}
	// This is the distinction #443 asks for: the credential step PASSES,
	// because the credential was obtained. What failed is the endpoint's
	// opinion of it, which is a different person's job.
	wantOutcome(t, report, StepCredentials, Passed)
	reach := wantOutcome(t, report, StepReach, Failed)
	if reach.Category != transport.Authentication.String() {
		t.Fatalf("reach category = %q, want %q", reach.Category, transport.Authentication)
	}
	if store.uploads != 0 {
		t.Fatalf("uploads = %d, want 0", store.uploads)
	}
}

// --- deny: the bucket is not there ---

func TestPreflight_MissingBucket_IsConfigurationAndNotAuthentication(t *testing.T) {
	store := newFakeStore()
	store.statErr = transport.NewError(transport.Configuration, "stat_object", errors.New("the medium's bucket does not exist"))

	report := preflight(t, store, testMedium(), placement.Content)
	if report.OK {
		t.Fatal("a medium naming a bucket that is not there reported OK")
	}
	wantOutcome(t, report, StepCredentials, Passed)
	reach := wantOutcome(t, report, StepReach, Failed)
	if reach.Category != transport.Configuration.String() {
		t.Fatalf("reach category = %q, want %q", reach.Category, transport.Configuration)
	}
	if !strings.Contains(reach.Detail, "nas-backups") {
		t.Fatalf("reach detail = %q, want it to name the bucket somebody has to go and fix", reach.Detail)
	}
}

// --- deny: the write is denied ---

func TestPreflight_DeniedWrite_FailsTheWriteAndStillRollsBack(t *testing.T) {
	store := newFakeStore()
	store.uploadErr = transport.NewError(transport.PermissionDenied, "upload_from_local", errors.New("AccessDenied"))

	report := preflight(t, store, testMedium(), placement.Content)
	if report.OK {
		t.Fatal("a medium that denies PutObject reported OK")
	}
	wantOutcome(t, report, StepReach, Passed)
	wantOutcome(t, report, StepDeliverable, Passed)
	write := wantOutcome(t, report, StepWrite, Failed)
	if write.Category != transport.PermissionDenied.String() {
		t.Fatalf("write category = %q, want %q", write.Category, transport.PermissionDenied)
	}
	for _, step := range []Step{StepReadBack, StepStorageClass, StepVerification} {
		wantOutcome(t, report, step, Skipped)
	}
	// The delete is still ATTEMPTED, because a failed upload can leave an
	// object behind, and it is reported as skipped rather than passed,
	// because this run has no claim to make about it.
	wantOutcome(t, report, StepDelete, Skipped)
	if store.deletes != 1 {
		t.Fatalf("deletes = %d, want 1: a failed upload still gets rolled back", store.deletes)
	}
}

// --- deny: the bytes do not come back ---

func TestPreflight_ReadBackReturnsSomethingElse_FailsRatherThanCheckingSizes(t *testing.T) {
	store := newFakeStore()
	store.storedClass = config.StorageClassStandardIA
	store.corruptOnRead = []byte("something entirely different")

	report := preflight(t, store, testMedium(), placement.Content)
	if report.OK {
		t.Fatal("an endpoint that returns different bytes than it was given reported OK")
	}
	wantOutcome(t, report, StepWrite, Passed)
	wantOutcome(t, report, StepReadBack, Failed)
	// The verification class this medium requires IS the read-back, so it
	// must not report green off the back of a comparison that failed.
	wantOutcome(t, report, StepVerification, Failed)
	// And the probe is still cleaned up.
	if len(store.objects) != 0 {
		t.Fatalf("the probe was left behind: %v", keysOf(store))
	}
}

// --- deny: the endpoint ignored the storage class ---

func TestPreflight_EndpointStoredADifferentClass_SaysTheConfigurationIsClaimingSomethingUntrue(t *testing.T) {
	store := newFakeStore()
	store.storedClass = config.StorageClassStandard // asked for STANDARD_IA

	report := preflight(t, store, testMedium(), placement.Content)
	if report.OK {
		t.Fatal("an endpoint that silently ignored storage_class reported OK")
	}
	class := wantOutcome(t, report, StepStorageClass, Failed)
	for _, want := range []string{config.StorageClassStandardIA, config.StorageClassStandard} {
		if !strings.Contains(class.Detail, want) {
			t.Fatalf("storage class detail = %q, want it to name %q", class.Detail, want)
		}
	}
}

func TestPreflight_EndpointReportsNoClassAtAll_IsAnAbsenceRatherThanAFailure(t *testing.T) {
	store := newFakeStore()
	store.storedClass = "" // a plain S3-compatible server with no tiering

	report := preflight(t, store, testMedium(), placement.Content)
	class := wantOutcome(t, report, StepStorageClass, Passed)
	if !strings.Contains(class.Detail, "cannot confirm") {
		t.Fatalf("storage class detail = %q, want it to say plainly that nothing was confirmed", class.Detail)
	}
	if !report.OK {
		t.Fatalf("an endpoint that simply reports no class made the whole preflight fail: %+v", report.Failures())
	}
}

// --- deny: attested cannot be achieved here ---

func TestPreflight_AttestedAgainstAnEndpointThatCannotAttest_RefusesRatherThanReportingGreen(t *testing.T) {
	store := newFakeStore()
	store.storedClass = config.StorageClassStandardIA

	report := preflight(t, store, testMedium(), placement.Attested)
	if report.OK {
		t.Fatal("a medium requiring attested against an endpoint that cannot attest reported OK: that is the exact lie a preflight exists to prevent")
	}
	verification := wantOutcome(t, report, StepVerification, Failed)
	if verification.Category != transport.UnsupportedCapability.String() {
		t.Fatalf("verification category = %q, want %q", verification.Category, transport.UnsupportedCapability)
	}
	if !strings.Contains(verification.Detail, "readback") {
		t.Fatalf("verification detail = %q, want it to say what to write instead", verification.Detail)
	}
	// Everything that CAN be proved still was: the failure is specific,
	// not a blanket refusal of the medium.
	wantOutcome(t, report, StepWrite, Passed)
	wantOutcome(t, report, StepReadBack, Passed)
	wantOutcome(t, report, StepDelete, Passed)
}

func TestPreflight_AttestedAgainstAnEndpointThatCan_Passes(t *testing.T) {
	store := newFakeStore()
	store.storedClass = config.StorageClassStandardIA
	store.attests = true

	report := preflight(t, store, testMedium(), placement.Attested)
	if !report.OK {
		t.Fatalf("report is not OK: %+v", report.Failures())
	}
	wantOutcome(t, report, StepVerification, Passed)
}

// --- deny: an archive class cannot take delivery, and nothing is spent
// finding that out ---

func TestPreflight_ArchiveClass_RefusesDeliveryWithoutWritingAnything(t *testing.T) {
	medium := testMedium()
	medium.StorageClass = config.StorageClassDeepArchive
	store := newFakeStore()

	report := preflight(t, store, medium, placement.Content)
	if report.OK {
		t.Fatal("an archive-class medium reported that an artifact can be delivered to it")
	}
	wantOutcome(t, report, StepReach, Passed)
	deliverable := wantOutcome(t, report, StepDeliverable, Failed)
	if !strings.Contains(deliverable.Detail, config.StorageClassDeepArchive) {
		t.Fatalf("deliverable detail = %q, want it to name the class", deliverable.Detail)
	}
	if !strings.Contains(deliverable.Detail, "restore") {
		t.Fatalf("deliverable detail = %q, want it to keep the restore case legal in so many words", deliverable.Detail)
	}
	for _, step := range []Step{StepWrite, StepReadBack, StepStorageClass, StepVerification, StepDelete} {
		wantOutcome(t, report, step, Skipped)
	}
	// The measurement that matters: DEEP_ARCHIVE bills a 180-day minimum
	// duration for every object written to it, so a probe here is a bill
	// for an answer this product already holds.
	if store.uploads != 0 {
		t.Fatalf("uploads = %d, want 0: a probe object on an archive class is billed for months", store.uploads)
	}
}

func TestPreflight_GlacierInstantRetrieval_IsNotRefused(t *testing.T) {
	// The trap internal/archive's own table exists to disarm: the word
	// Glacier in a class name does not mean an artifact cannot be
	// delivered to it.
	medium := testMedium()
	medium.StorageClass = config.StorageClassGlacierIR
	store := newFakeStore()
	store.storedClass = config.StorageClassGlacierIR

	report := preflight(t, store, medium, placement.Content)
	if !report.OK {
		t.Fatalf("GLACIER_IR was refused: %+v", report.Failures())
	}
	wantOutcome(t, report, StepDeliverable, Passed)
	wantOutcome(t, report, StepWrite, Passed)
}

// --- deny: the delete does not delete ---

func TestPreflight_DeleteReportsSuccessAndChangesNothing_IsCaught(t *testing.T) {
	store := newFakeStore()
	store.storedClass = config.StorageClassStandardIA
	store.keepOnDelete = true

	report := preflight(t, store, testMedium(), placement.Content)
	if report.OK {
		t.Fatal("a medium whose delete reports success and changes nothing reported OK")
	}
	del := wantOutcome(t, report, StepDelete, Failed)
	if !strings.Contains(del.Detail, "still") {
		t.Fatalf("delete detail = %q, want it to say the object is still there", del.Detail)
	}
}

func TestPreflight_DeleteRefused_IsCaught(t *testing.T) {
	store := newFakeStore()
	store.storedClass = config.StorageClassStandardIA
	store.deleteErr = transport.NewError(transport.PermissionDenied, "delete_object", errors.New("AccessDenied"))

	report := preflight(t, store, testMedium(), placement.Content)
	if report.OK {
		t.Fatal("a medium that refuses DeleteObject reported OK")
	}
	del := wantOutcome(t, report, StepDelete, Failed)
	if del.Category != transport.PermissionDenied.String() {
		t.Fatalf("delete category = %q, want %q", del.Category, transport.PermissionDenied)
	}
}

// --- FR-33: the canary ---

// preflightCanary is a value that exists nowhere else in this repository,
// so finding it in an output is proof of where it came from. Same
// enforcement shape E1.3 built for the transport layer, aimed at the
// surface this issue adds.
const preflightCanary = "CANARY-443-71c0d5e8a4b2-DO-NOT-SERVE"

func TestPreflight_ReportNeverCarriesACredentialOrWhereItCameFrom(t *testing.T) {
	medium := testMedium()
	// The canary planted in every place a medium references a secret:
	// the environment variable's NAME, the file PATH and the command. A
	// path is not a secret and it is still a fact about this machine that
	// an API caller has no use for.
	medium.Credentials = transport.MediumCredentials{Env: "BACKUP_S3_" + preflightCanary}

	store := newFakeStore()
	// And planted again in what the transport says went wrong, which is
	// the realistic leak: the classified cause names the variable.
	store.statErr = transport.NewError(transport.Configuration, "medium_credentials",
		fmt.Errorf("%w: medium %q: resolving credentials from environment variable %q: not set",
			transport.ErrCredentialsUnavailable, medium.ID, medium.Credentials.Env))

	var observed []error
	report, err := Run(context.Background(), Deps{Store: store, Observe: func(_ Step, err error) {
		observed = append(observed, err)
	}}, medium, placement.Content)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	rendered, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(rendered), preflightCanary) {
		t.Fatalf("the preflight response carries the canary:\n%s", rendered)
	}
	for _, forbidden := range []string{"BACKUP_S3", "credentials.file", "/var/lib", "aws_secret"} {
		if strings.Contains(strings.ToLower(string(rendered)), strings.ToLower(forbidden)) {
			t.Fatalf("the preflight response carries %q:\n%s", forbidden, rendered)
		}
	}

	// The positive control. Without it this test passes just as happily
	// against a preflight that reported nothing at all, so it has to prove
	// the canary really was in play and really did reach the one place it
	// is allowed to go, which is the operator's own log.
	if len(observed) == 0 {
		t.Fatal("nothing was observed, so this test never had a canary to leak")
	}
	var leakedToTheLog bool
	for _, err := range observed {
		if strings.Contains(err.Error(), preflightCanary) {
			leakedToTheLog = true
		}
	}
	if !leakedToTheLog {
		t.Fatal("the canary never reached Observe either, so this test proved nothing about redaction")
	}
	if wantOutcome(t, report, StepCredentials, Failed).Detail == "" {
		t.Fatal("the credentials step failed with no explanation at all, which is redaction taken past honesty")
	}
}

// TestReport_HasNoFieldASecretCouldTravelIn is FR-33 at the type level,
// before the canary above reaches for a running check: the report type has
// nowhere for a credential to hide, so the absence is structural rather
// than something a filter has to keep achieving.
func TestReport_HasNoFieldASecretCouldTravelIn(t *testing.T) {
	rendered, err := json.Marshal(Report{
		Medium: "offsite_s3",
		Checks: []Check{{Step: StepCredentials, Outcome: Failed, Category: "configuration", Detail: "..."}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Every field a Check and a Report have, named, so adding one is this
	// test failing rather than a review missing it.
	for _, want := range []string{"Medium", "OK", "Checks", "Step", "Outcome", "Category", "Detail"} {
		if !strings.Contains(string(rendered), want) {
			t.Fatalf("the report no longer carries a %q field; if it was renamed, this list is the place that says which fields exist:\n%s", want, rendered)
		}
	}
	var fields map[string]any
	if err := json.Unmarshal(rendered, &fields); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("Report has %d fields (%v), want exactly Medium, OK and Checks: a new field is a new place a secret can travel", len(fields), fields)
	}
}

// --- guard clauses ---

func TestRun_RefusesWithoutAStoreOrAMedium(t *testing.T) {
	if _, err := Run(context.Background(), Deps{}, testMedium(), placement.Content); err == nil {
		t.Fatal("Run with no MediumStore succeeded")
	}
	if _, err := Run(context.Background(), Deps{Store: newFakeStore()}, transport.Medium{}, placement.Content); err == nil {
		t.Fatal("Run with an unidentified medium succeeded")
	}
}

func TestPreflight_AnUnprovableVerificationClass_RefusesRatherThanReportingGreen(t *testing.T) {
	store := newFakeStore()
	store.storedClass = config.StorageClassStandardIA

	// placement.Existence is deliberately unreachable from any medium's
	// configuration, and if it ever became reachable a preflight must
	// refuse it rather than quietly report the strongest thing it happened
	// to run.
	report := preflight(t, store, testMedium(), placement.Existence)
	if report.OK {
		t.Fatal("a verification class this preflight cannot prove reported OK")
	}
	wantOutcome(t, report, StepVerification, Failed)
}

// --- the probe key ---

func TestProbeKey_LivesUnderASegmentNoArtifactCanReach(t *testing.T) {
	first, err := probeKey("backups")
	if err != nil {
		t.Fatalf("probeKey: %v", err)
	}
	second, err := probeKey("backups")
	if err != nil {
		t.Fatalf("probeKey: %v", err)
	}
	if first == second {
		t.Fatal("two probe keys are identical: two preflights at once would share one object, and the delete step's whole verdict is that the object IT wrote is gone")
	}
	if !strings.HasPrefix(first, "backups/"+probePrefix+"/") {
		t.Fatalf("probe key %q does not live under the medium's prefix and the reserved segment", first)
	}

	// An artifact's key is composed of a source, a backup set and an
	// artifact name, none of which config lets carry a "/", so no
	// configured artifact can produce a key under the reserved segment.
	// This is the same argument internal/placement's staging area makes
	// for using a subdirectory rather than a name suffix.
	bare, err := probeKey("")
	if err != nil {
		t.Fatalf("probeKey: %v", err)
	}
	if !strings.HasPrefix(bare, probePrefix+"/") {
		t.Fatalf("probe key %q for a medium with no prefix does not start at the reserved segment", bare)
	}
}

func keysOf(f *fakeStore) []string {
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	return out
}

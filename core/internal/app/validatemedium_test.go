package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Issue #435: `validate <id>` against an artifact whose only ACTIVE
// placement is on a storage medium checks the copy on the medium, instead
// of refusing (#434's stop-gap) or quarantining it as lost (the bug #434
// fixed).
//
// The distinction every test here exists to hold is the one #434's chain
// got wrong in the other direction: "I checked and it is gone" is a fact
// about the artifact and quarantines it, and "I could not check" is a fact
// about the endpoint and must never.

// --- a medium a validate test can steer ---

// validateMedium is transport.MediumStore with exactly the knobs these
// tests need: what it holds, whether it answers at all, and whether it can
// attest a full-object SHA-256.
//
// down is a whole-endpoint failure and it is deliberately NOT
// NotFound-classified. That is the one shape #435 is about: a bucket that
// did not answer says nothing whatever about whether the backup is still
// in it, and a fake that reported it as NotFound would let a fail-open
// implementation pass every test in this file.
type validateMedium struct {
	mu sync.Mutex

	objects map[string][]byte

	// down makes every call fail with a transport error that is not
	// NotFound, which is what an unreachable endpoint looks like.
	down bool

	// downFor is down for one medium id only, so a test can hold two
	// copies where one endpoint answers and the other does not.
	downFor map[string]bool

	// timeoutFor is issue #388's shape for one medium id: a transport
	// error classified as Transient whose CAUSE is still reachable as
	// context.DeadlineExceeded, which is what a connect timeout rclone
	// imposed on itself looks like. It is not the caller giving up, and a
	// predicate that reads it as one abandons a pass over one slow bucket.
	timeoutFor map[string]bool

	// cancelFor is a genuine cancellation for one medium id: a transport
	// error the adapter itself classified Cancelled. It is the control
	// for timeoutFor, and it must stop the whole check.
	cancelFor map[string]bool

	// attests makes ObjectChecksum answer with a real SHA-256 of the
	// stored bytes. It is false by default because that is what rclone
	// v1.75.0's s3 backend does: Fs.Hashes() returns only MD5, so no s3
	// medium reachable through this build can attest at all.
	attests bool

	// counters, so a test can prove the content class did not run.
	stats     int
	opens     int
	checksums int
}

func newValidateMedium() *validateMedium {
	return &validateMedium{objects: map[string][]byte{}, downFor: map[string]bool{}, timeoutFor: map[string]bool{}, cancelFor: map[string]bool{}}
}

func (m *validateMedium) put(key, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = []byte(content)
}

func (m *validateMedium) unreachable() *transport.Error {
	return &transport.Error{
		Category: transport.Transient, Op: "stat",
		Cause: errors.New("dial tcp: connection refused"),
	}
}

// selfImposedTimeout is the error rclone produces when its own connect
// deadline expires: classified Transient, with context.DeadlineExceeded
// still reachable underneath (issue #388).
func (m *validateMedium) selfImposedTimeout() *transport.Error {
	return &transport.Error{
		Category: transport.Transient, Op: "stat",
		Cause: fmt.Errorf("connect timeout: %w", context.DeadlineExceeded),
	}
}

// failFor is whatever this fake is meant to answer for medium id, or nil
// when it should answer honestly.
func (m *validateMedium) failFor(id string) error {
	switch {
	case m.down || m.downFor[id]:
		return m.unreachable()
	case m.timeoutFor[id]:
		return m.selfImposedTimeout()
	case m.cancelFor[id]:
		return &transport.Error{Category: transport.Cancelled, Op: "stat", Cause: context.Canceled}
	}
	return nil
}

func (m *validateMedium) StatObject(_ context.Context, med transport.Medium, key string) (transport.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats++
	if err := m.failFor(med.ID); err != nil {
		return transport.ObjectInfo{}, err
	}
	b, ok := m.objects[key]
	if !ok {
		return transport.ObjectInfo{}, &transport.Error{Category: transport.NotFound, Op: "stat", Cause: errors.New("no such key")}
	}
	return transport.ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (m *validateMedium) OpenObject(_ context.Context, med transport.Medium, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opens++
	if err := m.failFor(med.ID); err != nil {
		return nil, err
	}
	b, ok := m.objects[key]
	if !ok {
		return nil, &transport.Error{Category: transport.NotFound, Op: "open", Cause: errors.New("no such key")}
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func (m *validateMedium) ObjectChecksum(_ context.Context, med transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checksums++
	if err := m.failFor(med.ID); err != nil {
		return transport.ChecksumAttestation{}, err
	}
	if !m.attests {
		return transport.ChecksumAttestation{}, &transport.Error{
			Category: transport.UnsupportedCapability, Op: "checksum",
			Cause: fmt.Errorf("this backend cannot attest a full-object %s", alg),
		}
	}
	b, ok := m.objects[key]
	if !ok {
		return transport.ChecksumAttestation{}, &transport.Error{Category: transport.NotFound, Op: "checksum", Cause: errors.New("no such key")}
	}
	sum := sha256.Sum256(b)
	return transport.ChecksumAttestation{Algorithm: transport.SHA256, Value: hex.EncodeToString(sum[:])}, nil
}

func (m *validateMedium) UploadFromLocal(context.Context, transport.Medium, string, string, transport.UploadOptions) (transport.UploadResult, error) {
	panic("validate must never upload")
}

func (m *validateMedium) DeleteObject(context.Context, transport.Medium, string) error {
	panic("validate must never delete")
}

func (m *validateMedium) ListObjects(context.Context, transport.Medium, string) ([]transport.ObjectInfo, error) {
	panic("validate must never enumerate a medium")
}

func (m *validateMedium) RestoreStatus(_ context.Context, med transport.Medium, key string) (*transport.RestoreState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failFor(med.ID); err != nil {
		return nil, err
	}
	if _, ok := m.objects[key]; !ok {
		return nil, &transport.Error{Category: transport.NotFound, Op: "restore-status", Cause: errors.New("no such key")}
	}
	return nil, nil
}

func (m *validateMedium) InitiateRestore(context.Context, transport.Medium, string, int) error {
	panic("validate must never initiate a restore")
}

// --- fixtures ---

const validateMediumID = "cold_offsite"

// movedFixture is newCommittedFixture driven one step further: the shape a
// completed move to a storage medium really leaves, which is a GONE local
// placement beside an ACTIVE, content-verified medium one and no local
// file on disk at all.
type movedFixture struct {
	committedFixture

	store *validateMedium
	key   string

	// content is what the medium holds, so a test can corrupt it.
	content string
	hash    string
}

// moveToMedium retires the fixture's local copy and records the medium
// placement, seeding store with the object unless withObject is false.
func moveToMedium(t *testing.T, f committedFixture, store *validateMedium, class string, withObject bool) movedFixture {
	t.Helper()
	ctx := context.Background()

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

	const content = "payload for validate"
	key := "rclone-manager/production/pg/" + f.artifact.Name
	size := int64(len(content))
	for _, p := range []state.PlacementUpdate{
		{Medium: state.MediumLocal, Location: local.Location, Status: state.PlacementGone},
		{Medium: validateMediumID, Location: key, Size: &size,
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

	if withObject {
		store.put(key, content)
	}
	f.svc.MediumStore = store
	f.svc.Config.StorageMediums = []config.StorageMedium{{
		ID:           validateMediumID,
		Type:         config.StorageMediumTypeS3,
		Region:       "us-east-1",
		Bucket:       "nas-backups",
		Prefix:       "rclone-manager",
		StorageClass: class,
	}}

	return movedFixture{committedFixture: f, store: store, key: key, content: content, hash: before.LocalHash}
}

func newMovedFixture(t *testing.T) movedFixture {
	t.Helper()
	return moveToMedium(t, newCommittedFixture(t), newValidateMedium(), config.StorageClassStandard, true)
}

func mustStayComplete(t *testing.T, f movedFixture) {
	t.Helper()
	ctx := context.Background()
	after, err := f.journal.Get(ctx, f.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.Complete) {
		t.Fatalf("journal state = %q, want COMPLETE left untouched", after.State)
	}
	if _, found, err := f.journal.LastTransition(ctx, f.artifact, string(lifecycle.Complete), string(lifecycle.QuarantinedLost)); err != nil {
		t.Fatalf("LastTransition: %v", err)
	} else if found {
		t.Error("a COMPLETE -> QUARANTINED_LOST transition was written")
	}
}

// --- the tests ---

// TestValidateArtifact_ChecksTheCopyOnTheMedium is #435's acceptance line.
// The artifact's only ACTIVE placement is on a medium, the object is
// there, and `validate` now says so instead of refusing.
func TestValidateArtifact_ChecksTheCopyOnTheMedium(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if !result.Checked || !result.Passed {
		t.Fatalf("result = %+v, want Checked && Passed", result)
	}
	if result.NewState != "" {
		t.Errorf("NewState = %q, want empty: a pass moves nothing", result.NewState)
	}
	if !strings.Contains(result.Reason, validateMediumID) {
		t.Errorf("Reason = %q, want it to name the medium the copy is on", result.Reason)
	}
	mustStayComplete(t, f)
}

// TestValidateArtifact_DefaultRunNeverDownloadsTheObject holds FR-31's
// cost rule at the operator door: existence and attested are free and run
// without ceremony, and the content class, which downloads the object, is
// behind an explicit flag.
func TestValidateArtifact_DefaultRunNeverDownloadsTheObject(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	if _, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{}); err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if f.store.opens != 0 {
		t.Errorf("the default validate opened the object %d time(s); anything that costs egress is operator-initiated (FR-31)", f.store.opens)
	}
}

// TestValidateArtifact_StepsDownToExistenceOutLoud is the honesty rule.
// rclone v1.75.0's s3 backend answers Fs.Hashes() with MD5 alone, so no s3
// medium can attest a full-object SHA-256, and the default run therefore
// lands on existence every time in production. What it must never do is
// report that as anything stronger, or step down silently: the reason has
// to say the attested class was tried, why it could not run, and what
// existence actually proves.
func TestValidateArtifact_StepsDownToExistenceOutLoud(t *testing.T) {
	f := newMovedFixture(t) // attests is false, exactly like s3
	ctx := context.Background()

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %+v, want Passed", result)
	}
	if f.store.checksums == 0 {
		t.Error("the attested class was never attempted, so the step-down to existence was assumed rather than found out")
	}
	for _, want := range []string{"attested", "existence"} {
		if !strings.Contains(result.Reason, want) {
			t.Errorf("Reason = %q, want it to name %q: a class that silently stopped running is how a check becomes decorative", result.Reason, want)
		}
	}
}

// TestValidateArtifact_ReportsAttestedWhereTheMediumCanAttest is the other
// half of the step-down: where the endpoint really can attest, the run
// reports attested, and it does not download anything to get there.
func TestValidateArtifact_ReportsAttestedWhereTheMediumCanAttest(t *testing.T) {
	f := newMovedFixture(t)
	f.store.attests = true
	ctx := context.Background()

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %+v, want Passed", result)
	}
	if !strings.Contains(result.Reason, "attested") {
		t.Errorf("Reason = %q, want it to report the attested class it actually achieved", result.Reason)
	}
	if f.store.opens != 0 {
		t.Errorf("an attested check downloaded the object %d time(s); it is one metadata call", f.store.opens)
	}
}

// TestValidateArtifact_MissingObjectOnTheMediumQuarantines is the verdict
// half. The medium answered, the object is not there, and no other ACTIVE
// placement remains, so the artifact takes the COMPLETE ->
// QUARANTINED_LOST edge that state already means.
func TestValidateArtifact_MissingObjectOnTheMediumQuarantines(t *testing.T) {
	f := moveToMedium(t, newCommittedFixture(t), newValidateMedium(), config.StorageClassStandard, false)
	ctx := context.Background()

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if result.Passed {
		t.Fatalf("result = %+v, want a failed verdict: the medium answered and the object is not there", result)
	}
	if result.NewState != lifecycle.QuarantinedLost {
		t.Errorf("NewState = %q, want %q", result.NewState, lifecycle.QuarantinedLost)
	}
	after, err := f.journal.Get(ctx, f.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.QuarantinedLost) {
		t.Errorf("journal State = %q, want %q", after.State, lifecycle.QuarantinedLost)
	}
}

// TestValidateArtifact_UnreachableMediumIsAnErrorNotAVerdict is the heart
// of #435, and the shape #434 got wrong through the other door.
//
// An endpoint that did not answer is not evidence that a backup is gone.
// The operator gets an error, the artifact is left exactly where it was,
// and nothing is written.
func TestValidateArtifact_UnreachableMediumIsAnErrorNotAVerdict(t *testing.T) {
	f := newMovedFixture(t)
	f.store.down = true
	ctx := context.Background()

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err == nil {
		t.Fatalf("ValidateArtifact returned a verdict (%+v) for a medium that did not answer; want an error", result)
	}
	if result.NewState != "" {
		t.Errorf("NewState = %q, want empty: an unreachable medium moves nothing", result.NewState)
	}
	if !strings.Contains(err.Error(), validateMediumID) {
		t.Errorf("err = %v, want it to name the medium that did not answer", err)
	}
	mustStayComplete(t, f)
}

// TestValidateArtifact_UnreachableMediumUnderContentIsStillAnError is the
// same rule on the expensive path. A download that could not even start is
// still not evidence about the bytes.
func TestValidateArtifact_UnreachableMediumUnderContentIsStillAnError(t *testing.T) {
	f := newMovedFixture(t)
	f.store.down = true
	ctx := context.Background()

	if _, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{Content: true}); err == nil {
		t.Fatal("ValidateArtifact returned a verdict for a medium that did not answer under --content; want an error")
	}
	mustStayComplete(t, f)
}

// TestValidateArtifact_ContentClassCatchesWhatExistenceCannot proves the
// flag actually buys something. The object on the medium is the right SIZE
// and the wrong BYTES, which is precisely the corruption an existence
// check cannot see: the default run passes it, and --content fails it and
// quarantines the artifact.
func TestValidateArtifact_ContentClassCatchesWhatExistenceCannot(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	corrupt := strings.Repeat("x", len(f.content))
	if len(corrupt) != len(f.content) {
		t.Fatalf("the corrupted body is %d bytes and the original is %d; the point of this test is that they are the same size", len(corrupt), len(f.content))
	}
	f.store.put(f.key, corrupt)

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact (default): %v", err)
	}
	if !result.Passed {
		t.Fatalf("the default run failed a same-size object (%+v); an existence check cannot see corrupted bytes, and a test where it can proves nothing about the flag", result)
	}

	result, err = f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{Content: true})
	if err != nil {
		t.Fatalf("ValidateArtifact (--content): %v", err)
	}
	if result.Passed {
		t.Fatalf("result = %+v, want a failed verdict: the bytes on the medium do not hash to the recorded hash", result)
	}
	if result.NewState != lifecycle.QuarantinedLost {
		t.Errorf("NewState = %q, want %q", result.NewState, lifecycle.QuarantinedLost)
	}
	if f.store.opens == 0 {
		t.Error("--content never opened the object, so whatever failed was not a content check")
	}
}

// TestValidateArtifact_ContentClassPassesTheIntactObject is the control
// for the test above: the same --content run against the bytes that were
// actually uploaded passes, so the failure there is the corruption and not
// the flag.
func TestValidateArtifact_ContentClassPassesTheIntactObject(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{Content: true})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %+v, want Passed", result)
	}
	if f.store.opens == 0 {
		t.Error("--content never opened the object")
	}
	mustStayComplete(t, f)
}

// TestValidateArtifact_ArchivedCopyRefusesContentRatherThanFailingIt is
// FR-34 meeting FR-31. A copy on DEEP_ARCHIVE cannot be read at all until
// a restore finishes, so --content against one is a refusal, never a
// failed verdict, and certainly never a quarantine. The default run still
// works, because existence is all an archived copy can give.
func TestValidateArtifact_ArchivedCopyRefusesContentRatherThanFailingIt(t *testing.T) {
	f := moveToMedium(t, newCommittedFixture(t), newValidateMedium(), config.StorageClassDeepArchive, true)
	ctx := context.Background()

	if _, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{Content: true}); err == nil {
		t.Fatal("--content against an archived copy returned a verdict; want a refusal, because nothing can read those bytes until a restore finishes")
	}
	if f.store.opens != 0 {
		t.Errorf("the archived copy was opened %d time(s); the gate exists so the request is never spent", f.store.opens)
	}
	mustStayComplete(t, f)

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("the default run against an archived copy: %v", err)
	}
	if !result.Passed {
		t.Errorf("result = %+v, want Passed: an existence check works on an archived object", result)
	}
}

// TestValidateArtifact_UndeclaredMediumIsAnErrorNotAVerdict covers the
// placement row that outlived its configuration. A medium this deployment
// no longer declares cannot be asked, which leaves the question open, and
// an open question is not a lost backup.
func TestValidateArtifact_UndeclaredMediumIsAnErrorNotAVerdict(t *testing.T) {
	f := newMovedFixture(t)
	f.svc.Config.StorageMediums = nil
	ctx := context.Background()

	if _, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{}); err == nil {
		t.Fatal("ValidateArtifact returned a verdict for a copy on a medium the configuration does not declare; want an error")
	}
	mustStayComplete(t, f)
}

// TestValidateArtifact_QuarantinesOnlyWhenNoVerifiedCopyRemains is FR-31's
// placement-scoped quarantine rule: a failing check is a verdict about one
// COPY, and quarantine is a verdict about the ARTIFACT. With two ACTIVE
// medium copies and one of them gone, the artifact is still fine, and the
// reason has to name the copy that is not.
func TestValidateArtifact_QuarantinesOnlyWhenNoVerifiedCopyRemains(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	const second = "warm_offsite"
	size := int64(len(f.content))
	p := state.PlacementUpdate{
		Medium: second, Location: "rclone-manager/second/" + f.artifact.Name, Size: &size,
		Hash: f.hash, HashAlg: "sha256",
		VerificationClass: state.VerificationContent, Status: state.PlacementActive,
	}
	if _, err := f.journal.RecordTransition(ctx, state.Transition{
		Artifact: f.artifact, Key: f.artifact.String() + ":placement:" + second,
		From: string(lifecycle.Complete), To: string(lifecycle.Complete), OccurredAt: epoch, Placement: &p,
	}); err != nil {
		t.Fatalf("recording the second placement: %v", err)
	}
	f.svc.Config.StorageMediums = append(f.svc.Config.StorageMediums, config.StorageMedium{
		ID: second, Type: config.StorageMediumTypeS3, Region: "us-east-1", Bucket: "second-bucket",
	})
	// The second bucket does not hold the object: the medium answers, and
	// it is not there.

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %+v, want Passed: one ACTIVE verified copy still answers, and FR-31 quarantines the artifact only when none does", result)
	}
	if !strings.Contains(result.Reason, second) {
		t.Errorf("Reason = %q, want it to name the copy that is missing; a green tick that hides a lost copy is worse than no check", result.Reason)
	}
	mustStayComplete(t, f)
}

// TestValidateArtifact_OneUnreachableCopyDoesNotHideBehindAPass is the
// same idea for the other way a copy goes unasked. The pass stands,
// because a copy really was checked, and it must not imply that every copy
// was.
func TestValidateArtifact_OneUnreachableCopyDoesNotHideBehindAPass(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	const second = "warm_offsite"
	size := int64(len(f.content))
	p := state.PlacementUpdate{
		Medium: second, Location: "rclone-manager/second/" + f.artifact.Name, Size: &size,
		Hash: f.hash, HashAlg: "sha256",
		VerificationClass: state.VerificationContent, Status: state.PlacementActive,
	}
	if _, err := f.journal.RecordTransition(ctx, state.Transition{
		Artifact: f.artifact, Key: f.artifact.String() + ":placement:" + second,
		From: string(lifecycle.Complete), To: string(lifecycle.Complete), OccurredAt: epoch, Placement: &p,
	}); err != nil {
		t.Fatalf("recording the second placement: %v", err)
	}
	// Declared nowhere, so it cannot be asked at all.
	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %+v, want Passed", result)
	}
	if !strings.Contains(result.Reason, second) {
		t.Errorf("Reason = %q, want it to say the copy on %q was not checked", result.Reason, second)
	}
}

// TestValidateArtifact_StillRefusesWithNoMediumStore keeps #434's refusal
// exactly where it belongs: a deployment with no way to reach a medium at
// all. It is the one case where `validate` still has nothing to check and
// says so.
func TestValidateArtifact_StillRefusesWithNoMediumStore(t *testing.T) {
	f := newMovedFixture(t)
	f.svc.MediumStore = nil
	ctx := context.Background()

	_, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err == nil {
		t.Fatal("ValidateArtifact returned a verdict in a deployment with no MediumStore; want #434's refusal")
	}
	if !strings.Contains(err.Error(), validateMediumID) {
		t.Errorf("err = %v, want it to name the medium the durable copy is on", err)
	}
	if strings.Contains(err.Error(), "no local final path") {
		t.Errorf("err = %v reads a moved artifact as a lost file", err)
	}
	mustStayComplete(t, f)
}

// TestRevalidateQuarantined_ChecksTheCopyOnTheMedium covers the second
// door into the same checks. `quarantine revalidate` reports and writes
// nothing, so the copy on the medium is exactly what an operator deciding
// what to do next needs to hear about.
func TestRevalidateQuarantined_ChecksTheCopyOnTheMedium(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	if _, err := f.journal.RecordTransition(ctx, state.Transition{
		Artifact: f.artifact, Key: f.artifact.String() + ":quarantine",
		From: string(lifecycle.Complete), To: string(lifecycle.QuarantinedLost), OccurredAt: epoch,
		Detail: "planted for this test",
	}); err != nil {
		t.Fatalf("quarantining the fixture: %v", err)
	}

	result, err := f.svc.RevalidateQuarantined(ctx, f.artifact)
	if err != nil {
		t.Fatalf("RevalidateQuarantined: %v", err)
	}
	if !result.Checked || !result.Passed {
		t.Errorf("result = %+v, want Checked && Passed: the copy is on the medium and it is there", result)
	}
}

// TestReinstateQuarantined_ExistenceAloneIsNotEnoughEvidence is the
// fail-open this change could have introduced and does not. An existence
// check passes on nothing more than an object of the right size being at a
// key, which is not a check that could have failed on content, so it must
// never be enough to trust a quarantined artifact again.
func TestReinstateQuarantined_ExistenceAloneIsNotEnoughEvidence(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	if _, err := f.journal.RecordTransition(ctx, state.Transition{
		Artifact: f.artifact, Key: f.artifact.String() + ":quarantine",
		From: string(lifecycle.Complete), To: string(lifecycle.QuarantinedLost), OccurredAt: epoch,
		Detail: "planted for this test",
	}); err != nil {
		t.Fatalf("quarantining the fixture: %v", err)
	}

	result, err := f.svc.ReinstateQuarantined(ctx, f.artifact, "")
	if _, ok := lifecycle.AsInsufficientEvidence(err); !ok {
		t.Fatalf("ReinstateQuarantined: result = %+v, err = %v; want an insufficient-evidence refusal, because an existence check proves nothing about the bytes", result, err)
	}
	after, err := f.journal.Get(ctx, f.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.QuarantinedLost) {
		t.Errorf("journal State = %q, want the artifact left in %q", after.State, lifecycle.QuarantinedLost)
	}
}

// TestValidateArtifact_NamesTheRestoreTestHookThatDidNotRun is FR-13's
// honesty rule where it is easiest to lose.
//
// The restore-test hook opens the artifact, and off local disk opening it
// means downloading it, so it does not run against a copy on a medium. An
// operator who configured one and reads back a green tick has been told
// less than they asked for, and a check that quietly stops running is how
// a safety feature becomes decorative. So the pass names the tier that did
// not run.
func TestValidateArtifact_NamesTheRestoreTestHookThatDidNotRun(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	f.svc.Config.Sources[0].BackupSets[0].Validation = config.Validation{
		Command: &config.Command{Executable: "/nonexistent/restore-test", Timeout: config.Duration(10 * time.Second)},
	}

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %+v, want Passed", result)
	}
	if !strings.Contains(result.Reason, "restore-test hook did not run") {
		t.Errorf("Reason = %q, want it to name the configured restore-test hook that did not run", result.Reason)
	}
}

// TestValidateArtifact_UnresolvedValidatorIsStillRefusedOnAMedium keeps
// the one combination that must never read as "no validator configured"
// refused on this path too. A ValidatorID nothing resolved into a runnable
// command is a wiring problem, and reporting a moved artifact as passing
// while its backup set names a validator nobody could find is the same
// fail-open FR-13 exists to prevent, reached through the newest door.
func TestValidateArtifact_UnresolvedValidatorIsStillRefusedOnAMedium(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()

	f.svc.Config.Sources[0].BackupSets[0].Validation = config.Validation{ValidatorID: "never-resolved"}

	if _, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{}); err == nil {
		t.Fatal("ValidateArtifact reported on an artifact whose backup set names a validator nothing resolved; want a refusal")
	}
	mustStayComplete(t, f)
}

// addSecondMediumPlacement gives the fixture's artifact a second ACTIVE
// copy on medium id, declaring it in the configuration only when declare
// is true. The object is never seeded, so a declared, reachable second
// medium answers "not there".
func addSecondMediumPlacement(t *testing.T, f movedFixture, id string, declare bool) {
	t.Helper()
	ctx := context.Background()
	size := int64(len(f.content))
	p := state.PlacementUpdate{
		Medium: id, Location: "rclone-manager/second/" + f.artifact.Name, Size: &size,
		Hash: f.hash, HashAlg: "sha256",
		VerificationClass: state.VerificationContent, Status: state.PlacementActive,
	}
	if _, err := f.journal.RecordTransition(ctx, state.Transition{
		Artifact: f.artifact, Key: f.artifact.String() + ":placement:" + id,
		From: string(lifecycle.Complete), To: string(lifecycle.Complete), OccurredAt: epoch, Placement: &p,
	}); err != nil {
		t.Fatalf("recording the %s placement: %v", id, err)
	}
	if declare {
		f.svc.Config.StorageMediums = append(f.svc.Config.StorageMediums, config.StorageMedium{
			ID: id, Type: config.StorageMediumTypeS3, Region: "us-east-1", Bucket: "second-bucket",
		})
	}
}

// TestValidateArtifact_AFailedCopyBesideAnUndeclaredOneIsNotAQuarantine is
// the fail-open this shape invites, and it is subtle enough that I wrote
// it into the first version of checkMediumCopies myself.
//
// One copy was asked and is not there. The other sits on a medium the
// configuration no longer declares, so nothing could look at it, and it
// may very well still be there. "Every copy failed" is what quarantine
// means, and this is not that: it is "one copy failed and the other was
// never asked", which leaves "no verified copy remains" unproven. So it is
// an error and the artifact is left alone.
func TestValidateArtifact_AFailedCopyBesideAnUndeclaredOneIsNotAQuarantine(t *testing.T) {
	f := moveToMedium(t, newCommittedFixture(t), newValidateMedium(), config.StorageClassStandard, false)
	ctx := context.Background()
	addSecondMediumPlacement(t, f, "forgotten_offsite", false)

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err == nil {
		t.Fatalf("ValidateArtifact returned a verdict (%+v) while one of the artifact's copies was never asked about; want an error", result)
	}
	if !strings.Contains(err.Error(), "forgotten_offsite") {
		t.Errorf("err = %v, want it to name the copy that could not be asked", err)
	}
	mustStayComplete(t, f)
}

// TestValidateArtifact_AFailedCopyBesideAnUnreachableOneIsNotAQuarantine
// is the same rule for the other way a copy goes unasked: the medium is
// declared, and its endpoint did not answer.
func TestValidateArtifact_AFailedCopyBesideAnUnreachableOneIsNotAQuarantine(t *testing.T) {
	f := moveToMedium(t, newCommittedFixture(t), newValidateMedium(), config.StorageClassStandard, false)
	ctx := context.Background()
	addSecondMediumPlacement(t, f, "warm_offsite", true)
	f.store.downFor["warm_offsite"] = true

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err == nil {
		t.Fatalf("ValidateArtifact returned a verdict (%+v) while one of the artifact's endpoints did not answer; want an error", result)
	}
	if !strings.Contains(err.Error(), "warm_offsite") {
		t.Errorf("err = %v, want it to name the medium that did not answer", err)
	}
	mustStayComplete(t, f)
}

// TestValidateArtifact_EveryCopyAskedAndEveryCopyFailedDoesQuarantine is
// the control for the two above. Without it they would both be satisfied
// by a build that never quarantines anything at all, which is its own
// fail-open: the whole point of the check is that a backup nobody can find
// stops being counted as a restore point.
func TestValidateArtifact_EveryCopyAskedAndEveryCopyFailedDoesQuarantine(t *testing.T) {
	f := moveToMedium(t, newCommittedFixture(t), newValidateMedium(), config.StorageClassStandard, false)
	ctx := context.Background()
	addSecondMediumPlacement(t, f, "warm_offsite", true)

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v", err)
	}
	if result.Passed {
		t.Fatalf("result = %+v, want a failed verdict: both mediums answered and neither holds the object", result)
	}
	if result.NewState != lifecycle.QuarantinedLost {
		t.Errorf("NewState = %q, want %q", result.NewState, lifecycle.QuarantinedLost)
	}
}

// TestValidateArtifact_AConnectTimeoutIsNotTheOperatorGivingUp is issue
// #388's shape reaching this code.
//
// rclone imposes its own connect deadline and reports the result as a
// Transient transport error whose cause is still reachable as
// context.DeadlineExceeded. A cancellation predicate that asks
// errors.Is(err, context.DeadlineExceeded) before it asks the transport's
// own classification reads that as the caller having given up, and
// abandons the whole check over one slow bucket.
//
// internal/revalidate's isCancelled documents that ordering and the reason
// for it; this asserts that this package's twin agrees. One bucket answers
// and its copy is there, so the artifact passes, and the timing-out copy
// is named rather than allowed to end the pass.
func TestValidateArtifact_AConnectTimeoutIsNotTheOperatorGivingUp(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()
	addSecondMediumPlacement(t, f, "warm_offsite", true)
	f.store.timeoutFor["warm_offsite"] = true

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err != nil {
		t.Fatalf("ValidateArtifact: %v; a connect timeout against one of two buckets is not the caller cancelling the check", err)
	}
	if !result.Passed {
		t.Fatalf("result = %+v, want Passed: the copy on %s answered and is there", result, validateMediumID)
	}
	if !strings.Contains(result.Reason, "warm_offsite") {
		t.Errorf("Reason = %q, want it to name the copy that timed out", result.Reason)
	}
}

// TestValidateArtifact_ARealCancellationStopsTheWholeCheck is the control
// for the test above, and without it that one is satisfied by a build that
// never recognises a cancellation at all.
//
// The first copy answers and passes. The second is cancelled, which the
// adapter classifies as such rather than leaving to be inferred, and a
// pass reported on the strength of whichever copy happened to be asked
// before the operator pressed Ctrl-C is a pass about a check that did not
// finish. So the whole call is an error.
func TestValidateArtifact_ARealCancellationStopsTheWholeCheck(t *testing.T) {
	f := newMovedFixture(t)
	ctx := context.Background()
	addSecondMediumPlacement(t, f, "warm_offsite", true)
	f.store.cancelFor["warm_offsite"] = true

	result, err := f.svc.ValidateArtifact(ctx, f.artifact, ValidateOptions{})
	if err == nil {
		t.Fatalf("ValidateArtifact returned %+v for a check that was cancelled partway through; want an error", result)
	}
	mustStayComplete(t, f)
}

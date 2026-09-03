package placement_test

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

var testNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fakeStore is a MediumStore double that answers exactly what a test tells
// it to, including refusing to attest, which is the case no real endpoint
// reachable through this build can currently avoid.
type fakeStore struct {
	content []byte
	// attestation is what ObjectChecksum answers with. Empty with
	// attestErr nil means the endpoint answered with nothing, which is a
	// shape a real backend produces and which must not read as a match.
	attestation string
	// attestAlg is the algorithm the answer claims to be. Empty means the
	// one that was asked for, which is what an honest backend does.
	attestAlg transport.HashAlgorithm
	attestErr error
	statErr   error
	openErr   error
	size      *int64

	opened  int
	statted int
}

func (f *fakeStore) StatObject(_ context.Context, _ transport.Medium, key string) (transport.ObjectInfo, error) {
	f.statted++
	if f.statErr != nil {
		return transport.ObjectInfo{}, f.statErr
	}
	size := int64(len(f.content))
	if f.size != nil {
		size = *f.size
	}
	return transport.ObjectInfo{Key: key, Size: size}, nil
}

func (f *fakeStore) OpenObject(_ context.Context, _ transport.Medium, _ string) (io.ReadCloser, error) {
	f.opened++
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(strings.NewReader(string(f.content))), nil
}

func (f *fakeStore) ObjectChecksum(_ context.Context, _ transport.Medium, _ string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	if f.attestErr != nil {
		return transport.ChecksumAttestation{}, f.attestErr
	}
	if f.attestAlg != "" {
		alg = f.attestAlg
	}
	return transport.ChecksumAttestation{Algorithm: alg, Value: f.attestation}, nil
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mediumPlacement(content []byte) state.Placement {
	size := int64(len(content))
	return state.Placement{
		Medium:   "offsite_s3",
		Location: "rclone-manager/production/pg/a.dump",
		Size:     &size,
		Hash:     sha256Of(content),
		HashAlg:  string(transport.SHA256),
		Status:   state.PlacementActive,
	}
}

// TestTheLadderIsExactlyTheSchemasVocabulary is the drift guard between
// this package, which owns what a class MEANS, and internal/state plus
// 0007_placements.sql, which own what can be stored. A rung added here
// without a migration cannot be written; a value the schema admits with no
// rung here cannot be produced.
func TestTheLadderIsExactlyTheSchemasVocabulary(t *testing.T) {
	want := map[string]bool{
		state.VerificationContent:   true,
		state.VerificationAttested:  true,
		state.VerificationExistence: true,
	}
	got := map[string]bool{}
	for _, c := range placement.Classes {
		got[string(c)] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("internal/state stores %q but the ladder has no rung for it", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("the ladder has a rung %q that internal/state cannot store; 0007_placements.sql's CHECK would refuse it", name)
		}
	}
	if len(placement.Classes) != 3 {
		t.Errorf("the ladder has %d rungs, want 3", len(placement.Classes))
	}
}

// TestEachClassReportsItselfAndNothingStronger is FR-31's planted
// violation, made a permanent test rather than a one-off: force each rung
// and assert the surface cannot call it anything else.
func TestEachClassReportsItselfAndNothingStronger(t *testing.T) {
	content := []byte("the artifact's bytes")
	p := mediumPlacement(content)

	for _, tc := range []struct {
		want  placement.Class
		store *fakeStore
	}{
		{placement.Content, &fakeStore{content: content}},
		{placement.Attested, &fakeStore{content: content, attestation: sha256Of(content)}},
		{placement.Existence, &fakeStore{content: content}},
	} {
		t.Run(string(tc.want), func(t *testing.T) {
			got, err := placement.Verify(context.Background(), tc.store, transport.Medium{ID: "offsite_s3"}, p, tc.want, testNow)
			if err != nil {
				t.Fatalf("Verify(%s): %v", tc.want, err)
			}
			if !got.Passed {
				t.Fatalf("Verify(%s) did not pass against a correct object: %s", tc.want, got.Detail)
			}
			if got.Class != tc.want {
				t.Fatalf("Verify(%s) reported class %s", tc.want, got.Class)
			}
			for _, stronger := range placement.Classes {
				if stronger.Stronger(got.Class) && strings.Contains(strings.ToLower(got.Detail), string(stronger)) {
					t.Errorf("a %s result's own detail names the stronger class %s: %q", got.Class, stronger, got.Detail)
				}
			}
		})
	}
}

// TestAnExistenceRunCannotBeCalledContentVerification is the planted
// violation the spec names for this guard, spelled as the mutation it
// describes: force an existence run, and assert nothing about the result
// can be read as a content verification.
func TestAnExistenceRunCannotBeCalledContentVerification(t *testing.T) {
	content := []byte("the artifact's bytes")
	store := &fakeStore{content: content}

	got, err := placement.Verify(context.Background(), store, transport.Medium{ID: "offsite_s3"}, mediumPlacement(content), placement.Existence, testNow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Class != placement.Existence {
		t.Fatalf("class = %s, want %s", got.Class, placement.Existence)
	}
	if store.opened != 0 {
		t.Errorf("an existence check opened the object %d times; it is one HEAD request and nothing else", store.opened)
	}
	if got.Class.Stronger(placement.Existence) {
		t.Error("the existence rung reports itself as stronger than existence")
	}
	if placement.Content.Stronger(got.Class) != true {
		t.Error("content is not reported as stronger than existence, so the ladder has no order")
	}
	if got.Class.CostsEgress() {
		t.Error("an existence check reports that it costs egress")
	}
}

// TestAttestationRefusalIsExplicit is FR-13's rule: where the endpoint
// cannot attest, the answer is a capability refusal, never a weaker result
// under the stronger name.
//
// The two shapes a real backend produces are covered separately. An
// endpoint that refuses outright (which is what rclone v1.75.0's s3
// backend does for SHA-256, every time) and an endpoint that answers with
// nothing at all: an empty digest compared against a recorded one would
// compare unequal to everything, which is a verdict this product must not
// reach by accident.
func TestAttestationRefusalIsExplicit(t *testing.T) {
	content := []byte("the artifact's bytes")
	p := mediumPlacement(content)

	for _, tc := range []struct {
		name  string
		store *fakeStore
	}{
		{"the endpoint refuses", &fakeStore{content: content, attestErr: transport.NewError(transport.UnsupportedCapability, "object_checksum", errors.New("this backend exposes md5 and nothing else"))}},
		{"the endpoint answers with nothing", &fakeStore{content: content, attestation: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := placement.Verify(context.Background(), tc.store, transport.Medium{ID: "offsite_s3"}, p, placement.Attested, testNow)
			if err == nil {
				t.Fatalf("Verify returned %+v with no error; an attestation that could not be obtained is a capability refusal", got)
			}
			if !errors.Is(err, placement.ErrClassUnavailable) {
				t.Errorf("the refusal is not ErrClassUnavailable: %v; a caller cannot tell it apart from a failed verification", err)
			}
			if got != (placement.Result{}) {
				t.Errorf("Verify returned %+v alongside its refusal; a refused class must carry no result a caller could record", got)
			}
		})
	}
}

// TestVerifyNeverFallsBack is the property that makes the two tests above
// worth anything: asking for a class an endpoint cannot serve must not
// quietly produce the class it can.
func TestVerifyNeverFallsBack(t *testing.T) {
	content := []byte("the artifact's bytes")
	store := &fakeStore{
		content:   content,
		attestErr: transport.NewError(transport.UnsupportedCapability, "object_checksum", errors.New("no")),
	}

	got, err := placement.Verify(context.Background(), store, transport.Medium{ID: "offsite_s3"}, mediumPlacement(content), placement.Attested, testNow)
	if err == nil {
		t.Fatalf("Verify fell back and returned %+v", got)
	}
	if store.statted != 0 || store.opened != 0 {
		t.Errorf("a refused attestation reached for another rung anyway: %d stats, %d opens", store.statted, store.opened)
	}
}

// TestAFailedCheckIsNotACapabilityRefusal keeps the two apart from the
// other side. "We checked and it is wrong" is a fact about the artifact,
// and a caller that reads it as "we could not check" leaves a corrupt
// backup on the shelf.
func TestAFailedCheckIsNotACapabilityRefusal(t *testing.T) {
	p := mediumPlacement([]byte("what was ingested"))

	for _, tc := range []struct {
		name  string
		class placement.Class
		store *fakeStore
	}{
		{"content mismatch", placement.Content, &fakeStore{content: []byte("something else entirely")}},
		{"attestation mismatch", placement.Attested, &fakeStore{attestation: sha256Of([]byte("something else entirely"))}},
		{"the object is gone", placement.Existence, &fakeStore{statErr: transport.NewError(transport.NotFound, "stat_object", errors.New("object not found"))}},
		{"the object is the wrong size", placement.Existence, &fakeStore{size: ptrInt64(999999)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := placement.Verify(context.Background(), tc.store, transport.Medium{ID: "offsite_s3"}, p, tc.class, testNow)
			if err != nil {
				t.Fatalf("Verify reported a capability refusal for a check it actually ran: %v", err)
			}
			if got.Passed {
				t.Fatalf("Verify passed: %s", got.Detail)
			}
			if got.Class != tc.class {
				t.Errorf("a failed %s check reported class %s", tc.class, got.Class)
			}
			if got.Detail == "" {
				t.Error("a failed check explains nothing")
			}
		})
	}
}

// TestAMediumThatCannotBeReachedIsNotAnAbsence is the distinction a mover
// deletes a local copy on the strength of. A network failure must not read
// as "the object is not there".
func TestAMediumThatCannotBeReachedIsNotAnAbsence(t *testing.T) {
	store := &fakeStore{statErr: transport.NewError(transport.Transient, "stat_object", errors.New("connection reset"))}

	got, err := placement.Verify(context.Background(), store, transport.Medium{ID: "offsite_s3"}, mediumPlacement([]byte("x")), placement.Existence, testNow)
	if err == nil {
		t.Fatalf("an unreachable medium produced a verdict about the artifact: %+v", got)
	}
	if !errors.Is(err, placement.ErrClassUnavailable) {
		t.Errorf("the refusal is not ErrClassUnavailable: %v", err)
	}
}

// TestContentVerificationRefusesWithoutARecordedHash: there is nothing to
// compare against, and "content verified" for a comparison that never
// happened is the exact dishonesty this package exists to prevent.
func TestContentVerificationRefusesWithoutARecordedHash(t *testing.T) {
	p := mediumPlacement([]byte("x"))
	p.Hash = ""
	p.HashAlg = ""

	store := &fakeStore{content: []byte("x")}
	for _, class := range []placement.Class{placement.Content, placement.Attested} {
		if got, err := placement.Verify(context.Background(), store, transport.Medium{ID: "offsite_s3"}, p, class, testNow); err == nil {
			t.Errorf("Verify(%s) with no recorded hash returned %+v, want a capability refusal", class, got)
		}
	}
	if store.opened != 0 {
		t.Errorf("a refusal that needs no network call opened the object %d times", store.opened)
	}
}

// TestCostAndProofAreStatedForEveryRung is the acceptance line "three
// classes, each with stated cost". A rung whose cost nobody wrote down is
// a rung an operator cannot choose between.
func TestCostAndProofAreStatedForEveryRung(t *testing.T) {
	for _, c := range placement.Classes {
		if strings.TrimSpace(c.Cost()) == "" || c.Cost() == "unknown" {
			t.Errorf("class %s states no cost", c)
		}
		if strings.TrimSpace(c.Proves()) == "" || c.Proves() == "nothing" {
			t.Errorf("class %s states nothing it proves", c)
		}
	}
	if !placement.Content.CostsEgress() {
		t.Error("content verification does not report that it costs egress; the automatic revalidation path refuses on exactly this answer")
	}
	if placement.Attested.CostsEgress() || placement.Existence.CostsEgress() {
		t.Error("a metadata-only class reports that it costs egress, which would make it unusable automatically for no reason")
	}
}

func ptrInt64(n int64) *int64 { return &n }

// TestAnAttestationOfTheWrongAlgorithmIsARefusal is the last rung of
// FR-32's first rule, held at the boundary where it would actually cost
// something.
//
// ObjectChecksum is asked for a SHA-256. A store that answers with
// anything else has not attested this object, and the dangerous part is
// what the obvious code does next: it compares the digest it got against
// the recorded hash, they differ, and the artifact is reported as having
// FAILED verification. On the revalidation path that quarantines it. So a
// backend quietly handing back the ETag's MD5 would not look like a
// capability gap, it would look like every backup going corrupt at once.
//
// The adapter this product ships cannot produce that answer: it asks
// rclone for SHA-256 or refuses. But Store is an interface, the refusal
// has to hold for whatever is behind it, and without this check
// ChecksumAttestation.Algorithm is a field nothing reads.
func TestAnAttestationOfTheWrongAlgorithmIsARefusal(t *testing.T) {
	content := []byte("the artifact's bytes")
	p := mediumPlacement(content)

	// An MD5 of the very same bytes: the honest answer to a question
	// nobody asked, and the one an S3 ETag actually carries.
	md5sum := md5.Sum(content)
	store := &fakeStore{
		content:     content,
		attestation: hex.EncodeToString(md5sum[:]),
		attestAlg:   transport.HashAlgorithm("md5"),
	}

	got, err := placement.Verify(context.Background(), store, transport.Medium{ID: "offsite_s3"}, p, placement.Attested, testNow)
	if err == nil {
		t.Fatalf("Verify(attested) returned %+v for a digest of the wrong algorithm; comparing it to the recorded hash reports a correct object as corrupt", got)
	}
	if !errors.Is(err, placement.ErrClassUnavailable) {
		t.Errorf("the refusal is not ErrClassUnavailable: %v", err)
	}
	if got != (placement.Result{}) {
		t.Errorf("Verify returned %+v alongside its refusal; a caller could record that as a failed verification", got)
	}
	if !strings.Contains(err.Error(), "md5") {
		t.Errorf("the refusal does not say what it was handed instead: %v", err)
	}
}

// TestAnAttestationOfTheRightAlgorithmStillPasses is the positive control
// for the check above. A refusal that fires on every attestation would
// pass the test above for the wrong reason and make the whole rung
// unreachable.
func TestAnAttestationOfTheRightAlgorithmStillPasses(t *testing.T) {
	content := []byte("the artifact's bytes")
	for _, alg := range []transport.HashAlgorithm{"", transport.SHA256, transport.HashAlgorithm("SHA256")} {
		store := &fakeStore{content: content, attestation: sha256Of(content), attestAlg: alg}
		got, err := placement.Verify(context.Background(), store, transport.Medium{ID: "offsite_s3"}, mediumPlacement(content), placement.Attested, testNow)
		if err != nil {
			t.Fatalf("Verify(attested) with algorithm %q: %v", alg, err)
		}
		if !got.Passed || got.Class != placement.Attested {
			t.Fatalf("Verify(attested) with algorithm %q = %+v", alg, got)
		}
	}
}

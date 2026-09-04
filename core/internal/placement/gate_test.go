package placement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// These tests came across from internal/archive with the gate (see
// gate.go for why it moved). They are unchanged in what they claim; only
// the package they claim it in changed.

var gateNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func gateTime(t time.Time) *time.Time { return &t }

func gateHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// gateStore is one object on one pretend medium, plus a call counter for
// every method a test might want to prove was NOT reached.
//
// The counters are the point of it. Half of what the gate promises is
// about things that must not happen: a refusal must be decided from held
// facts rather than from a request nobody should have spent. A double that
// only returns canned answers cannot show that, because "returned the
// right refusal" and "returned the right refusal after doing the expensive
// thing anyway" look identical from the outside.
type gateStore struct {
	mu sync.Mutex

	content     []byte
	attestation string
	statErr     error

	stats, opens, checksums int
}

func (f *gateStore) spentARequestOnTheObject() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats+f.opens+f.checksums > 0
}

func (f *gateStore) StatObject(_ context.Context, _ transport.Medium, _ string) (transport.ObjectInfo, error) {
	f.mu.Lock()
	f.stats++
	f.mu.Unlock()
	if f.statErr != nil {
		return transport.ObjectInfo{}, f.statErr
	}
	return transport.ObjectInfo{Size: int64(len(f.content))}, nil
}

func (f *gateStore) OpenObject(_ context.Context, _ transport.Medium, _ string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return io.NopCloser(bytes.NewReader(f.content)), nil
}

func (f *gateStore) ObjectChecksum(_ context.Context, _ transport.Medium, _ string, _ transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	f.mu.Lock()
	f.checksums++
	f.mu.Unlock()
	return transport.ChecksumAttestation{Algorithm: transport.SHA256, Value: f.attestation}, nil
}

// archivedPlacement is a placement row on a storage medium, verified at
// class.
func archivedPlacement(medium, key string, size int64, hash, class string) state.Placement {
	sz := size
	return state.Placement{
		Medium:            medium,
		Location:          key,
		Size:              &sz,
		Hash:              hash,
		HashAlg:           string(transport.SHA256),
		VerificationClass: class,
		VerifiedAt:        gateTime(gateNow.Add(-24 * time.Hour)),
		Status:            state.PlacementActive,
	}
}

func glacierMedium() transport.Medium {
	return transport.Medium{ID: "cold-store", Type: transport.MediumTypeS3, Bucket: "b", StorageClass: config.StorageClassGlacier}
}

func deepArchiveMedium() transport.Medium {
	return transport.Medium{ID: "cold-store", Type: transport.MediumTypeS3, Bucket: "b", StorageClass: config.StorageClassDeepArchive}
}

func standardMedium() transport.Medium {
	return transport.Medium{ID: "warm-store", Type: transport.MediumTypeS3, Bucket: "b", StorageClass: config.StorageClassStandard}
}

// gateNotFound is what a medium returns for a key it does not hold,
// classified the way an adapter classifies it so Verify's own NotFound
// branch is exercised rather than bypassed.
var gateNotFound = transport.NewError(transport.NotFound, "stat", fmt.Errorf("no such key"))

// TestCeilingBoundsWhatAnAccessStateCanProve is FR-31's archive rule as a
// table: an archived copy is existence-checkable and nothing more, until a
// restore changes that.
func TestCeilingBoundsWhatAnAccessStateCanProve(t *testing.T) {
	tests := []struct {
		access archive.State
		want   Class
	}{
		{archive.Immediate, Content},
		{archive.RequiresRestore, Existence},
		{archive.Restoring, Existence},
		{archive.Unreachable, ""},
	}
	for _, tc := range tests {
		if got := Ceiling(tc.access); got != tc.want {
			t.Errorf("Ceiling(%q) = %q, want %q", tc.access, got, tc.want)
		}
	}
}

// TestAnArchivedCopyCannotEarnAConfidenceItHasNotEarned is the central
// honesty claim of the gate.
//
// The copy in this test is on DEEP_ARCHIVE and its bytes are physically
// unreadable. VerifyWithAccess is asked for the strongest class, and it
// has to refuse in the one way that does not defame the artifact: a
// wrapped ErrClassUnavailable, which this package's own doc says means "I
// could not check", never a Result with Passed false, which means "I
// checked and it is wrong" and eventually quarantines a perfectly good
// backup.
//
// It also has to refuse without spending a request. That is asserted here
// and not left to taste: a GET of an archived object returns
// InvalidObjectState, and the caller that has to interpret THAT is the
// caller that gets it wrong.
func TestAnArchivedCopyCannotEarnAConfidenceItHasNotEarned(t *testing.T) {
	body := []byte("the artifact's bytes, which nobody can currently have")
	store := &gateStore{content: body, attestation: gateHash(body)}
	p := archivedPlacement("cold-store", "prefix/src/set/artifact", int64(len(body)), gateHash(body), "")

	for _, want := range []Class{Content, Attested} {
		t.Run(string(want), func(t *testing.T) {
			result, err := VerifyWithAccess(context.Background(), store, deepArchiveMedium(), p, want,
				archive.Observation{Probe: archive.Answered}, gateNow)
			if !errors.Is(err, ErrClassUnavailable) {
				t.Fatalf("VerifyWithAccess(%s) error = %v, want a wrapped ErrClassUnavailable", want, err)
			}
			if result.Class != "" || result.Passed {
				t.Fatalf("VerifyWithAccess(%s) returned a result (%+v); a class that could not be attempted is not a verdict about the artifact", want, result)
			}
			if store.spentARequestOnTheObject() {
				t.Fatalf("VerifyWithAccess(%s) went to the medium anyway (stat=%d open=%d checksum=%d); the refusal is derivable from held facts and has to happen first",
					want, store.stats, store.opens, store.checksums)
			}
		})
	}
}

// TestAnArchivedCopyStillGetsItsExistenceChecked is the positive control
// for the test above, and it is what makes that test non-vacuous.
//
// If the refusal above fired because the placement was malformed, or
// because the double was empty, or for any reason other than the storage
// class, this would fail too. It passes, so the refusal really is about
// the class and nothing else.
func TestAnArchivedCopyStillGetsItsExistenceChecked(t *testing.T) {
	body := []byte("the artifact's bytes, which nobody can currently have")
	store := &gateStore{content: body, attestation: gateHash(body)}
	p := archivedPlacement("cold-store", "prefix/src/set/artifact", int64(len(body)), gateHash(body), "")

	result, err := VerifyWithAccess(context.Background(), store, deepArchiveMedium(), p, Existence,
		archive.Observation{Probe: archive.Answered}, gateNow)
	if err != nil {
		t.Fatalf("VerifyWithAccess(existence) against an archived copy: %v", err)
	}
	if !result.Passed || result.Class != Existence {
		t.Fatalf("VerifyWithAccess(existence) = %+v, want a passing existence result", result)
	}
	if store.opens != 0 {
		t.Fatalf("an existence check opened the object %d times; it is one HEAD and nothing else", store.opens)
	}
}

// TestARestoredCopyCanBeContentVerified is the other half of FR-34's
// promise: after a completed restore, the stronger classes become
// possible, and they become possible against the restored copy rather
// than in principle.
func TestARestoredCopyCanBeContentVerified(t *testing.T) {
	body := []byte("the artifact's bytes, now that somebody paid for them")
	store := &gateStore{content: body, attestation: gateHash(body)}
	p := archivedPlacement("cold-store", "prefix/src/set/artifact", int64(len(body)), gateHash(body), state.VerificationExistence)

	restored := archive.Observation{Probe: archive.Answered, Restore: &archive.RestoreState{ExpiresAt: gateTime(gateNow.Add(48 * time.Hour))}}
	result, err := VerifyWithAccess(context.Background(), store, glacierMedium(), p, Content, restored, gateNow)
	if err != nil {
		t.Fatalf("VerifyWithAccess(content) against a restored copy: %v", err)
	}
	if !result.Passed || result.Class != Content {
		t.Fatalf("VerifyWithAccess(content) = %+v, want a passing content result", result)
	}
	if store.opens != 1 {
		t.Fatalf("content verification opened the object %d times, want exactly 1", store.opens)
	}
}

// TestVerifyWithAccessRefusesEverythingAgainstAMediumThatDidNotAnswer,
// because a check that could not run is not a weak pass.
func TestVerifyWithAccessRefusesEverythingAgainstAMediumThatDidNotAnswer(t *testing.T) {
	store := &gateStore{}
	p := archivedPlacement("warm-store", "prefix/src/set/artifact", 4, gateHash([]byte("body")), "")

	for _, want := range []Class{Content, Attested, Existence} {
		_, err := VerifyWithAccess(context.Background(), store, standardMedium(), p, want,
			archive.Observation{Probe: archive.DidNotAnswer}, gateNow)
		if !errors.Is(err, ErrClassUnavailable) {
			t.Errorf("VerifyWithAccess(%s) against an unreachable medium: error = %v, want ErrClassUnavailable", want, err)
		}
	}
	if store.spentARequestOnTheObject() {
		t.Error("VerifyWithAccess went to a medium already known not to be answering")
	}
}

// TestTheAutomaticClassNeverCostsEgress is FR-31's "anything that costs
// egress is operator-initiated", as a property over every access state
// rather than as a constant somebody could raise.
//
// It reads Class.CostsEgress, which is the mechanism, so a future change
// that made an extra class free would flow through here rather than
// needing this test edited, and a change that made a billed class
// automatic would fail it.
func TestTheAutomaticClassNeverCostsEgress(t *testing.T) {
	for _, s := range archive.States {
		got := AutomaticClass(s)
		if got == "" {
			continue
		}
		if got.CostsEgress() {
			t.Errorf("AutomaticClass(%q) = %q, which costs egress; an unattended pass must never download an artifact", s, got)
		}
		if got.Stronger(Ceiling(s)) {
			t.Errorf("AutomaticClass(%q) = %q, which is stronger than the ceiling %q", s, got, Ceiling(s))
		}
	}
}

// TestAnArchiveClassCannotBeRevalidatedIntoASurpriseBill is the same rule
// aimed at the case that would actually generate one.
//
// An archived object's content check does not merely cost egress. It
// cannot happen at all without a restore first, and a restore is billed
// per object for a window measured in days. So the automatic class for an
// archived copy has to be existence, and getting there has to be
// impossible to reach by configuration rather than merely unusual.
func TestAnArchiveClassCannotBeRevalidatedIntoASurpriseBill(t *testing.T) {
	for _, class := range archive.Classes() {
		if !archive.IsArchive(class) {
			continue
		}
		access, err := archive.Access("cold-store", class, archive.Observation{Probe: archive.Answered}, gateNow)
		if err != nil {
			t.Fatalf("Access(%q): %v", class, err)
		}
		if got := AutomaticClass(access); got != Existence {
			t.Errorf("AutomaticClass for %s is %q, want %q", class, got, Existence)
		}
		if err := CheckClass(access, Content); !errors.Is(err, ErrClassUnavailable) {
			t.Errorf("CheckClass(%s, content) = %v, want a refusal", class, err)
		}
	}
}

// TestAnArchivedObjectThatIsActuallyMissingIsAFailedCheckNotARefusal keeps
// the third fact apart from the other two.
//
// "This copy is archived" and "this medium did not answer" are both facts
// about reachability, and neither says anything about the artifact. "There
// is no object at that key" is a fact about the artifact, and it has to
// arrive as a failed existence Result so that quarantine can act on it,
// not as a capability refusal that would leave the journal believing in a
// copy that is not there.
func TestAnArchivedObjectThatIsActuallyMissingIsAFailedCheckNotARefusal(t *testing.T) {
	store := &gateStore{statErr: gateNotFound}
	p := archivedPlacement("cold-store", "prefix/src/set/artifact", 12, gateHash([]byte("body")), "")

	result, err := VerifyWithAccess(context.Background(), store, glacierMedium(), p, Existence,
		archive.Observation{Probe: archive.Answered}, gateNow)
	if err != nil {
		t.Fatalf("VerifyWithAccess(existence) against a missing object: err = %v, want a failed result instead", err)
	}
	if result.Passed {
		t.Fatal("VerifyWithAccess(existence) passed for an object the medium does not hold")
	}
	if result.Class != Existence {
		t.Fatalf("class = %q, want %q; a failed attempt still carries the class it ran", result.Class, Existence)
	}
}

// TestTheGateCannotInitiateARestore is the structural half of FR-34's "a
// read never initiates a restore", carried over from internal/archive's
// sweep of every function in that package.
//
// The gate takes a Store, and Store has no InitiateRestore method, so
// there is no call this file could make that would start one. That is
// pinned by the type system rather than by a counter: a Store that could
// initiate a restore would have to grow the method first, and this test is
// the place that change would have to be argued.
func TestTheGateCannotInitiateARestore(t *testing.T) {
	var s Store = &gateStore{}
	if _, ok := s.(interface {
		InitiateRestore(context.Context, transport.Medium, string, int) error
	}); ok {
		t.Fatal("the gate's Store can initiate a restore; a read path must have no way to start one")
	}
}

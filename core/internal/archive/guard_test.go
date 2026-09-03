package archive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

func glacierMedium() transport.Medium {
	return transport.Medium{ID: "cold-store", Type: transport.MediumTypeS3, Bucket: "b", StorageClass: config.StorageClassGlacier}
}

func standardMedium() transport.Medium {
	return transport.Medium{ID: "warm-store", Type: transport.MediumTypeS3, Bucket: "b", StorageClass: config.StorageClassStandard}
}

// TestCeilingBoundsWhatAnAccessStateCanProve is FR-31's archive rule as a
// table: an archived copy is existence-checkable and nothing more, until a
// restore changes that.
func TestCeilingBoundsWhatAnAccessStateCanProve(t *testing.T) {
	tests := []struct {
		access State
		want   placement.Class
	}{
		{Immediate, placement.Content},
		{RequiresRestore, placement.Existence},
		{Restoring, placement.Existence},
		{Unreachable, ""},
	}
	for _, tc := range tests {
		if got := Ceiling(tc.access); got != tc.want {
			t.Errorf("Ceiling(%q) = %q, want %q", tc.access, got, tc.want)
		}
	}
}

// TestAnArchivedCopyCannotEarnAConfidenceItHasNotEarned is the central
// honesty claim of this lane.
//
// The copy in this test is on DEEP_ARCHIVE and its bytes are physically
// unreadable. Verify is asked for the strongest class, and it has to
// refuse in the one way that does not defame the artifact: a wrapped
// placement.ErrClassUnavailable, which internal/placement's own doc says
// means "I could not check", never a Result with Passed false, which means
// "I checked and it is wrong" and eventually quarantines a perfectly good
// backup.
//
// It also has to refuse without spending a request. That is asserted here
// and not left to taste: a GET of an archived object returns
// InvalidObjectState, and the caller that has to interpret THAT is the
// caller that gets it wrong.
func TestAnArchivedCopyCannotEarnAConfidenceItHasNotEarned(t *testing.T) {
	body := []byte("the artifact's bytes, which nobody can currently have")
	store := &fakeMedium{content: body, attestation: hashOf(body)}
	p := mediumPlacement("cold-store", "prefix/src/set/artifact", int64(len(body)), hashOf(body), "")
	medium := transport.Medium{ID: "cold-store", Type: transport.MediumTypeS3, Bucket: "b", StorageClass: config.StorageClassDeepArchive}

	for _, want := range []placement.Class{placement.Content, placement.Attested} {
		t.Run(string(want), func(t *testing.T) {
			result, err := Verify(context.Background(), store, medium, p, want,
				Observation{Probe: Answered}, testNow)
			if !errors.Is(err, placement.ErrClassUnavailable) {
				t.Fatalf("Verify(%s) error = %v, want a wrapped placement.ErrClassUnavailable", want, err)
			}
			if result.Class != "" || result.Passed {
				t.Fatalf("Verify(%s) returned a result (%+v); a class that could not be attempted is not a verdict about the artifact", want, result)
			}
			if store.spentARequestOnTheObject() {
				stats, opens, checksums, _, _ := store.counts()
				t.Fatalf("Verify(%s) went to the medium anyway (stat=%d open=%d checksum=%d); the refusal is derivable from held facts and has to happen first",
					want, stats, opens, checksums)
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
	store := &fakeMedium{content: body, attestation: hashOf(body)}
	p := mediumPlacement("cold-store", "prefix/src/set/artifact", int64(len(body)), hashOf(body), "")
	medium := transport.Medium{ID: "cold-store", Type: transport.MediumTypeS3, Bucket: "b", StorageClass: config.StorageClassDeepArchive}

	result, err := Verify(context.Background(), store, medium, p, placement.Existence,
		Observation{Probe: Answered}, testNow)
	if err != nil {
		t.Fatalf("Verify(existence) against an archived copy: %v", err)
	}
	if !result.Passed || result.Class != placement.Existence {
		t.Fatalf("Verify(existence) = %+v, want a passing existence result", result)
	}
	if _, _, _, _, initiates := store.counts(); initiates != 0 {
		t.Fatalf("checking an archived copy's existence started %d restores; a read never initiates one", initiates)
	}
}

// TestARestoredCopyCanBeContentVerified is the other half of FR-34's
// promise: after a completed restore, the stronger classes become
// possible, and they become possible against the restored copy rather
// than in principle.
func TestARestoredCopyCanBeContentVerified(t *testing.T) {
	body := []byte("the artifact's bytes, now that somebody paid for them")
	store := &fakeMedium{content: body, attestation: hashOf(body)}
	p := mediumPlacement("cold-store", "prefix/src/set/artifact", int64(len(body)), hashOf(body), state.VerificationExistence)
	medium := glacierMedium()

	restored := Observation{Probe: Answered, Restore: &RestoreState{ExpiresAt: ptrTime(testNow.Add(48 * time.Hour))}}
	result, err := Verify(context.Background(), store, medium, p, placement.Content, restored, testNow)
	if err != nil {
		t.Fatalf("Verify(content) against a restored copy: %v", err)
	}
	if !result.Passed || result.Class != placement.Content {
		t.Fatalf("Verify(content) = %+v, want a passing content result", result)
	}
	if _, opens, _, _, _ := store.counts(); opens != 1 {
		t.Fatalf("content verification opened the object %d times, want exactly 1", opens)
	}
}

// TestVerifyRefusesEverythingAgainstAMediumThatDidNotAnswer, because a
// check that could not run is not a weak pass.
func TestVerifyRefusesEverythingAgainstAMediumThatDidNotAnswer(t *testing.T) {
	store := &fakeMedium{}
	p := mediumPlacement("warm-store", "prefix/src/set/artifact", 4, hashOf([]byte("body")), "")

	for _, want := range []placement.Class{placement.Content, placement.Attested, placement.Existence} {
		_, err := Verify(context.Background(), store, standardMedium(), p, want,
			Observation{Probe: DidNotAnswer}, testNow)
		if !errors.Is(err, placement.ErrClassUnavailable) {
			t.Errorf("Verify(%s) against an unreachable medium: error = %v, want ErrClassUnavailable", want, err)
		}
	}
	if store.spentARequestOnTheObject() {
		t.Error("Verify went to a medium already known not to be answering")
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
	for _, s := range append([]State{}, States...) {
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
	for _, class := range Classes() {
		if !IsArchive(class) {
			continue
		}
		access, err := Access("cold-store", class, Observation{Probe: Answered}, testNow)
		if err != nil {
			t.Fatalf("Access(%q): %v", class, err)
		}
		if got := AutomaticClass(access); got != placement.Existence {
			t.Errorf("AutomaticClass for %s is %q, want %q", class, got, placement.Existence)
		}
		if err := CheckClass(access, placement.Content); !errors.Is(err, placement.ErrClassUnavailable) {
			t.Errorf("CheckClass(%s, content) = %v, want a refusal", class, err)
		}
	}
}

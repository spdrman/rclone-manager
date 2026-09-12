// The per-backend classification, which is the acceptance criterion "no
// supported backend relies on an unproven metadata assumption" written as a
// test rather than as a claim.
//
// Two halves. The first is coverage: every backend this build ships as a
// SOURCE has a row in the signal table, discovered from the shipped
// manifests rather than from a list in this file, so adding a backend and
// forgetting its trust signals fails here instead of defaulting to whatever
// the zero SourceSignals classifies as. The second is the honesty check:
// no row may reach the strong class without naming evidence that cannot lie
// about content, and no row may claim a timestamp resolution finer than the
// backend's protocol can carry.

package sourceconsistency

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/model"
)

// bundledManifestDir is read rather than enumerated so that a fourth
// backend cannot be shipped without a trust classification. It is the same
// directory core/internal/backend loads its catalogue from.
const bundledManifestDir = "../backend/bundled"

func bundledBackendIDs(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(bundledManifestDir)
	if err != nil {
		t.Fatalf("read %s: %v", bundledManifestDir, err)
	}

	var ids []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(bundledManifestDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}

		var manifest struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		if manifest.ID == "" {
			t.Fatalf("%s declares no id", e.Name())
		}

		ids = append(ids, manifest.ID)
	}

	if len(ids) == 0 {
		t.Fatalf("no bundled manifests found under %s", bundledManifestDir)
	}

	return ids
}

// Coverage: a shipped backend with no signals is a backend whose metadata
// this manager would reason about without having established what it
// reports, which is precisely the unproven assumption this issue exists to
// remove.
func TestEveryBundledBackendHasSourceSignals(t *testing.T) {
	for _, id := range bundledBackendIDs(t) {
		if _, ok := SourceSignalsFor(id); !ok {
			t.Errorf("backend %q ships with no source signals; declare its capability matrix so the projection has something to read", id)
		}
	}
}

// The classification each shipped backend gets, pinned by value. These three
// rows are the operator-visible matrix the ADR publishes: a change here is a
// change to what this product promises about a source it reads.
func TestBundledBackendTrustClasses(t *testing.T) {
	want := map[string]model.TrustClass{
		"local_volume": model.TrustWeak,
		"s3":           model.TrustStrong,
		"sftp":         model.TrustWeak,
	}

	for id, wantClass := range want {
		sig, ok := SourceSignalsFor(id)
		if !ok {
			t.Errorf("no signals for %q", id)
			continue
		}

		got := model.ClassifyMetadataTrust(sig)
		if got.Class != wantClass {
			t.Errorf("%s classified %q (%s), want %q", id, got.Class, got.Reason, wantClass)
		}
	}
}

// The honesty check, run over the whole table rather than the three names
// above: strong is reachable only through a hash the backend computes
// without this manager reading the object, or an identifier that changes
// when the object is overwritten. A row that reached strong any other way
// would be a trusted metadata assumption with nothing behind it.
func TestNoBackendReachesStrongTrustWithoutContentEvidence(t *testing.T) {
	for _, id := range bundledBackendIDs(t) {
		sig, ok := SourceSignalsFor(id)
		if !ok {
			t.Errorf("no signals for %q", id)
			continue
		}

		class := model.ClassifyMetadataTrust(sig).Class
		if class != model.TrustStrong {
			continue
		}
		if len(sig.RemoteHashAlgorithms) == 0 && !sig.ObjectGeneration {
			t.Errorf("%s is classified strong with neither a backend-computed hash nor an object generation identifier", id)
		}
	}
}

// A local filesystem and an SFTP account are weak for a NAMED reason, and
// the named reason is that neither hands back a content hash without this
// manager reading the bytes. rclone can produce md5 or sha1 for both, by
// reading the whole file, which is not a metadata signal - it is the re-read
// the policy was deciding whether to do. Recording those algorithms in
// RemoteHashAlgorithms would classify both sources strong on the strength of
// work that has not happened.
//
// The SFTP row carries the other half of that argument, already written down
// in model/identity.go: this project recommends a shell-less SFTP account,
// and rclone's sftp backend computes hashes by running sha1sum over the SSH
// session, which such an account cannot do at all.
func TestLocalAndSftpAreWeakBecauseNoHashArrivesWithoutReading(t *testing.T) {
	for _, id := range []string{"local_volume", "sftp"} {
		sig, ok := SourceSignalsFor(id)
		if !ok {
			t.Fatalf("no signals for %q", id)
		}
		if len(sig.RemoteHashAlgorithms) != 0 {
			t.Errorf("%s claims backend-computed hashes %v; both of rclone's hashes for it are computed by reading the file", id, sig.RemoteHashAlgorithms)
		}
		if sig.ObjectGeneration {
			t.Errorf("%s claims an object generation identifier; a filesystem path is a slot, not a version", id)
		}
		if !sig.StableSize {
			t.Errorf("%s claims unstable sizes, which would make it unknown rather than weak", id)
		}
	}
}

// The row that is deliberately NOT the backend's own answer. A local
// filesystem keeps nanosecond timestamps and the backend capability matrix
// says so, because the matrix describes the backend. This table describes
// what survives into this manager, and transport.RemoteArtifact.ModTime is
// unix SECONDS on every path into it (core/internal/transport/
// transport.go:152), so the sub-second half is discarded before any
// comparison here can see it.
//
// Recording 1ns would be claiming a resolution this engine throws away,
// which is exactly the unproven metadata assumption this whole table exists
// to refuse. The class does not move either way - a settable timestamp is
// weak evidence at any resolution - but the WIDTH of the blind window is
// the number an operator is quoted, so it has to be the real one.
//
// The reason string is asserted, not just the field, because the reason is
// what reaches the operator. A transport that carried nanoseconds would
// move this test and the row together, on purpose.
func TestLocalVolumeRecordsTheResolutionThisEngineKeepsNotTheOneTheDiskHas(t *testing.T) {
	sig, ok := SourceSignalsFor("local_volume")
	if !ok {
		t.Fatal("no signals for local_volume")
	}

	if sig.MTimePrecision != model.MTimeSecond {
		t.Fatalf("local_volume declares %q; this engine truncates modification times to unix seconds, so anything finer is a resolution it does not keep", sig.MTimePrecision)
	}

	trust, ok := TrustForBackend("local_volume")
	if !ok {
		t.Fatal("no trust classification for local_volume")
	}
	if !strings.Contains(trust.Reason, "1s") {
		t.Errorf("the reason quotes no blind window an operator can act on: %q", trust.Reason)
	}
}

// The narrowing applied to EVERY backend, stated as the property rather
// than one row at a time: nothing may claim a modification time finer than
// transport.RemoteArtifact.ModTime can carry, which is unix seconds. s3's
// matrix says 1ms and its projection says 1s, and that costs the
// classification nothing, which is the second half of this test: s3 is
// strong on its generation identifier, which settles every comparison
// before a timestamp is consulted.
func TestNoBackendClaimsAResolutionFinerThanTheTransportCarries(t *testing.T) {
	floor, ok := transportMTimeFloor.Resolution()
	if !ok {
		t.Fatalf("the floor itself is unreadable: %q", transportMTimeFloor)
	}

	for _, id := range bundledBackendIDs(t) {
		sig, ok := SourceSignalsFor(id)
		if !ok {
			t.Errorf("no signals for %q", id)
			continue
		}

		res, ok := sig.MTimePrecision.Resolution()
		if !ok {
			// "unknown" is a legitimate value and classifies as
			// unknown trust. An unparseable string is a typo that
			// would silently do the same thing for the wrong reason,
			// so the projection maps it to the honest admission and
			// this asserts nothing further about it.
			if sig.MTimePrecision != model.MTimePrecisionUnknown {
				t.Errorf("%s projects mtime precision %q, which this package cannot read", id, sig.MTimePrecision)
			}
			continue
		}
		if res < floor {
			t.Errorf("%s projects %q, finer than the %q this engine keeps", id, sig.MTimePrecision, transportMTimeFloor)
		}
	}

	s3, ok := SourceSignalsFor("s3")
	if !ok {
		t.Fatal("no signals for s3")
	}
	// Strip the timestamp entirely and the class must not budge, which is
	// what "does not rest on it" means as a property rather than a comment.
	withoutTime := s3
	withoutTime.MTimePrecision = model.MTimePrecisionUnknown
	if got := model.ClassifyMetadataTrust(withoutTime).Class; got != model.TrustStrong {
		t.Errorf("without its timestamp s3 classifies %q; the generation identifier was supposed to be doing this work", got)
	}
}

// The projection is a projection: what SourceSignalsFor reports for a
// shipped backend is what SignalsFromCapabilities produces from that
// backend's declared matrix, with nothing added on the way. This is the
// test that would have caught the drift the hand-written table had, and it
// is the reason the table is gone.
func TestSignalsAreProjectedFromTheDeclaredMatrix(t *testing.T) {
	reg, err := backend.Bundled()
	if err != nil {
		t.Fatalf("backend.Bundled: %v", err)
	}

	for _, id := range bundledBackendIDs(t) {
		m, err := reg.Backend(id)
		if err != nil {
			t.Fatalf("registry has no %q: %v", id, err)
		}
		caps, declared := m.DeclaredCapabilities()
		if !declared {
			t.Errorf("%s declares no capability matrix", id)
			continue
		}

		got, ok := SourceSignalsFor(id)
		if !ok {
			t.Errorf("no signals for %q", id)
			continue
		}

		want := SignalsFromCapabilities(id, caps)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s signals %+v; the projection of its declared matrix is %+v", id, got, want)
		}

		// The four keys that come straight off the matrix, checked
		// against the matrix itself rather than against the projection,
		// so a projection that dropped one is not compared with itself.
		if got.StableSize != caps.StableSize {
			t.Errorf("%s projects stable_size %v from a matrix that says %v", id, got.StableSize, caps.StableSize)
		}
		if wantGen := caps.GenerationIdentity == backend.GenerationVersioned; got.ObjectGeneration != wantGen {
			t.Errorf("%s projects an object generation of %v from generation_identity %q", id, got.ObjectGeneration, caps.GenerationIdentity)
		}
		if string(got.MetadataSupport) != string(caps.MetadataSupport) {
			t.Errorf("%s projects metadata support %q from a matrix that says %q", id, got.MetadataSupport, caps.MetadataSupport)
		}
	}
}

// hash_support is the one key that does NOT project, and this is the test
// that keeps somebody from "fixing" that. Both local_volume and sftp
// declare hashes in the matrix (or, for sftp, declare none for a reason of
// its own), and neither may reach RemoteSignals as a free hash, because the
// matrix key answers "could a hash be obtained at all" - including by
// reading every byte - and the signal answers "does one arrive without
// reading".
func TestHashSupportIsNotProjectedBecauseItAnswersADifferentQuestion(t *testing.T) {
	reg, err := backend.Bundled()
	if err != nil {
		t.Fatalf("backend.Bundled: %v", err)
	}

	local, err := reg.Backend("local_volume")
	if err != nil {
		t.Fatalf("registry has no local_volume: %v", err)
	}
	caps, declared := local.DeclaredCapabilities()
	if !declared {
		t.Fatal("local_volume declares no capability matrix")
	}
	if len(caps.HashSupport) == 0 {
		t.Fatal("local_volume's matrix declares no hashes, so this test proves nothing; it exists because it declares several that all cost a full read")
	}

	sig := SignalsFromCapabilities("local_volume", caps)
	if len(sig.RemoteHashAlgorithms) != 0 {
		t.Errorf("local_volume projects free hashes %v out of a matrix whose hashes are computed by reading the whole file", sig.RemoteHashAlgorithms)
	}
}

// The end-to-end shape a caller actually uses: a backend id and an operator
// preset in, a policy out, with no step at which a caller has to know how
// the classification works. Both presets are exercised for all three
// backends because the combination is what the ADR's matrix publishes.
func TestPolicyForBackendCoversBothPresets(t *testing.T) {
	for _, id := range bundledBackendIDs(t) {
		for _, preset := range []model.MetadataTrustPreset{model.PresetTrustMetadata, model.PresetConservative} {
			pol, ok := PolicyForBackend(id, preset)
			if !ok {
				t.Errorf("PolicyForBackend(%q, %q) found no signals", id, preset)
				continue
			}
			if pol.Mode == model.VerifyNever {
				t.Errorf("PolicyForBackend(%q, %q) = %q", id, preset, pol.Mode)
			}
			if pol.Mode != model.VerifyAlways && pol.ReverifyInterval <= 0 {
				t.Errorf("PolicyForBackend(%q, %q) may skip content with no re-verification interval", id, preset)
			}
		}
	}

	if _, ok := PolicyForBackend("not-a-backend", model.PresetTrustMetadata); ok {
		t.Error("PolicyForBackend invented a policy for a backend it has no signals for")
	}
}

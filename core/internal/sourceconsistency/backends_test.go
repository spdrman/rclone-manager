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
	"testing"

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
			t.Errorf("backend %q ships with no source signals; add a row to BundledSourceSignals with the resolution, size stability and hash availability its protocol actually gives", id)
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
	for id, sig := range BundledSourceSignals {
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

// Every row states a resolution this package understands. "unknown" is a
// legitimate value and classifies as unknown trust; an unparseable string
// is a typo that would silently do the same thing for the wrong reason, and
// would then be indistinguishable from an honest admission.
func TestEverySignalRowStatesAResolutionThisPackageUnderstands(t *testing.T) {
	for id, sig := range BundledSourceSignals {
		if sig.MTimePrecision == model.MTimePrecisionUnknown {
			continue
		}
		if _, ok := sig.MTimePrecision.Resolution(); !ok {
			t.Errorf("%s declares mtime precision %q, which this package cannot read", id, sig.MTimePrecision)
		}
	}
}

// The end-to-end shape a caller actually uses: a backend id and an operator
// preset in, a policy out, with no step at which a caller has to know how
// the classification works. Both presets are exercised for all three
// backends because the combination is what the ADR's matrix publishes.
func TestPolicyForBackendCoversBothPresets(t *testing.T) {
	for _, id := range bundledBackendIDs(t) {
		for _, preset := range []model.MetadataTrustPreset{model.PresetStrong, model.PresetConservative} {
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

	if _, ok := PolicyForBackend("not-a-backend", model.PresetStrong); ok {
		t.Error("PolicyForBackend invented a policy for a backend it has no signals for")
	}
}

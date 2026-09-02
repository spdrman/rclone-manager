package mediumcontract_test

import (
	"fmt"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/mediumcontract"
)

// filesystemFixtures drives the reference implementation. Every medium gets
// its own id and its own prefix so the isolation Fixtures promises is real
// rather than assumed.
type filesystemFixtures struct {
	store *mediumcontract.FilesystemStore
	n     int
}

func (f *filesystemFixtures) NewMedium(t *testing.T) transport.Medium {
	t.Helper()
	f.n++
	return transport.Medium{
		ID:           fmt.Sprintf("reference-%d", f.n),
		Type:         transport.MediumTypeS3,
		Bucket:       "reference-bucket",
		Prefix:       fmt.Sprintf("run/%d", f.n),
		StorageClass: "STANDARD",
	}
}

func (f *filesystemFixtures) AttestsChecksums() bool { return f.store.AttestsChecksums() }

// TestContractSuiteAgainstTheReferenceImplementation is the in-tree half of
// FR-28's "run against the local backend in-tree and against a MinIO
// fixture in integration". It needs no container, so it runs on every gate,
// and it is what keeps the suite honest: a case that only passes against S3
// was never a contract case.
//
// Both attestation postures run, because the real S3 adapter can only ever
// demonstrate the refusal (rclone's s3 backend reports MD5 and that MD5 is
// the ETag, which FR-32 forbids believing), so without the attesting
// fixture here the suite's positive branch would be code nothing executes.
func TestContractSuiteAgainstTheReferenceImplementation(t *testing.T) {
	for _, attest := range []bool{true, false} {
		name := "without checksum attestation"
		if attest {
			name = "with checksum attestation"
		}
		t.Run(name, func(t *testing.T) {
			store := mediumcontract.NewFilesystemStore(t.TempDir(), attest)
			mediumcontract.Run(t, store, &filesystemFixtures{store: store})
		})
	}
}

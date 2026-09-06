// Package recovery implements EPIC-B section 19.3's sidecar recovery
// manifest: a small, non-secret JSON file written next to every committed
// artifact in the user backup root, carrying exactly enough metadata to
// reconstruct that artifact's FR-9 journal row if the private state
// database is ever lost or corrupted (issue #102, "B3.3 Catalog
// reconstruction and state-loss recovery").
//
// # Why this lives in its own package
//
// internal/lifecycle writes a manifest immediately after a successful
// durable commit (see commit.go's writeRecoveryManifest), and
// internal/app's catalog-rebuild use case reads every manifest back to
// reconstruct a lost journal. Neither of those packages may import the
// other (lifecycle owns FR-10 through FR-16; internal/app sits above both
// and already imports lifecycle), so the manifest format and its
// read/write functions live here instead, depending on nothing but
// internal/model and the standard library: a leaf package both sides can
// share, the same role internal/model itself plays for everything above
// it.
//
// # Security domain
//
// Section 19.1/19.2 of the EPIC treats the private state database and the
// user backup root as two separate security domains. A sidecar manifest
// belongs to the second one: WriteManifest writes it into the same
// directory as the artifact itself (the configured backup-set LocalPath,
// i.e. the backup root), never into the private state directory. Manifest
// is deliberately a narrow, hand-picked set of fields, exactly section
// 19.3's recovery-metadata list plus one addition (RemotePath, see its own
// doc comment). Nothing in this package ever reads, or has anywhere to
// put, an SSH private key, an authentication token, a remote password or a
// secret environment value: every value written into a Manifest is copied
// out of internal/state.Record, which itself has no secret fields to copy
// from, and manifest_test.go's TestManifestFieldsExcludeSecrets fails the
// build the moment a future field name even looks like it could carry one.
package recovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// CurrentFormatVersion is the "backup-manager format version" section
// 19.3 asks every recovery manifest to carry. ReadManifest refuses a
// manifest whose FormatVersion is newer than this binary knows how to
// interpret, mirroring internal/state/migrate.go's own "refuse rather than
// guess" convention for a schema version it does not recognise, instead of
// attempting to read a future format's fields as if they were this one's.
const CurrentFormatVersion = 1

// manifestSuffix names the sidecar file. It is appended to the artifact's
// final basename, exactly the way internal/lifecycle/transfer.go's
// partialSuffix (".partial") is: a directory listing makes a manifest's
// association with its artifact obvious at a glance, backup.dump sitting
// next to backup.dump.manifest.json.
const manifestSuffix = ".manifest.json"

// Manifest is one artifact's sidecar recovery manifest.
type Manifest struct {
	// FormatVersion is "backup-manager format version" (section 19.3).
	FormatVersion int `json:"format_version"`

	// Source and BackupSet together are "backup-set stable ID/name"
	// (section 19.3): model.BackupSetID's two validated halves, stored
	// separately so ReadManifest can rebuild the identity through
	// model.NewBackupSetID rather than re-parsing a rendered string.
	Source    string `json:"source"`
	BackupSet string `json:"backup_set"`

	// ArtifactName is "artifact ID" (section 19.3): model.ArtifactID's
	// Name half. Source/BackupSet above supply the other half.
	ArtifactName string `json:"artifact_name"`

	// RemotePath is not one of section 19.3's named fields, but catalog
	// rebuild cannot reconstruct a usable journal row without it:
	// internal/reconcile's FR-17 pass needs a remote path to check the
	// artifact's remote object against (see internal/reconcile/remote.go's
	// statRemote), and internal/state's schema has no way to leave it
	// unset. It carries no secret and no more information than the backup
	// set's own configured remote root already implies, so it stays inside
	// section 19.3's non-secret guarantee.
	RemotePath string `json:"remote_path"`

	// ProducerTimestamp is "producer timestamp": the remote object's own
	// modification time as captured at discovery (state.Record's
	// Remote.ModTime), or nil when the backend never reported one.
	ProducerTimestamp *time.Time `json:"producer_timestamp,omitempty"`

	// ReceivedTimestamp is "received timestamp": when this manager
	// finished durably committing the artifact locally. It round-trips
	// through the manifest file for an operator's or auditor's benefit;
	// see internal/app/catalog.go's doc for why a reconstructed journal
	// row does not carry it as a value distinct from RetentionTimestamp.
	ReceivedTimestamp time.Time `json:"received_timestamp"`

	// RetentionTimestamp is "retention-relevant timestamp": the exact
	// value internal/retention reads as state.Record.DiscoveredAt, which
	// that package calls the artifact's received timestamp. A rebuilt
	// row's DiscoveredAt is set to exactly this value.
	//
	// It is not on its own what places an artifact in a retention bucket.
	// FR-18 places every artifact twice, by its received timestamp and by
	// the producer's own timestamp where one is admissible, and unions the
	// two selections (see internal/retention/bucketkey.go). The producer
	// half of that pair is ProducerTimestamp above, which is why both
	// fields have to round-trip through this manifest for a rebuilt
	// catalog to reach the identical retention verdicts the lost journal
	// would have, and not just this one.
	RetentionTimestamp time.Time `json:"retention_timestamp"`

	// SizeBytes is "size": the artifact's size in bytes.
	SizeBytes int64 `json:"size_bytes"`

	// Checksum and ChecksumAlgorithm are "checksum(s)": the LOCAL content
	// hash computed during FR-13 verification (state.Record's LocalHash /
	// LocalHashAlg), the same value internal/reconcile's checkLocalFinal
	// and internal/lifecycle's own pre-delete revalidation both compare
	// the current file against to detect corruption. This is deliberately
	// the local hash, not a remote-reported one: it is what lets a
	// rebuilt row's later reconciliation pass tell a genuinely intact
	// local copy apart from a corrupted one, which is the whole point of
	// "trustworthy" recovery.
	Checksum          string `json:"checksum,omitempty"`
	ChecksumAlgorithm string `json:"checksum_algorithm,omitempty"`

	// ValidationPassed and ValidationDetail are "validation result
	// summary" (state.Record's ValidationPassed / ValidationDetail).
	// ValidationPassed is nil when no application validator ran for this
	// artifact.
	ValidationPassed *bool  `json:"validation_passed,omitempty"`
	ValidationDetail string `json:"validation_detail,omitempty"`

	// Placements is where this artifact's durable copies were when the
	// manifest was written (EPIC E, FR-29), one entry per copy.
	//
	// # Why the format version did not go up for this
	//
	// The field is additive and omitempty, so a manifest written before
	// EPIC E parses into a nil slice and a manifest written after it is
	// read by an older binary as a manifest with one field it does not
	// know about, which encoding/json ignores. That older binary then
	// reconstructs exactly the row it reconstructs today, and in Phase 1
	// it is right to: every placement is local, and a local placement
	// says nothing the manifest did not already imply.
	//
	// Bumping CurrentFormatVersion would instead make every older binary
	// refuse the whole manifest, for a field it has no need of, which is
	// the opposite of what recovery metadata is for.
	//
	// The bump belongs to the change that makes this field load-bearing:
	// once an artifact's only copy can be on a storage medium, a manifest
	// read by a binary that ignores Placements reconstructs a row claiming
	// a local copy that is not there, and THAT is a difference a version
	// number has to stop.
	Placements []ManifestPlacement `json:"placements,omitempty"`
}

// ManifestPlacement is one durable copy of an artifact, as recorded in a
// recovery manifest.
//
// It mirrors internal/state.Placement without importing it: this package
// depends on internal/model and the standard library and nothing else, so
// that both internal/lifecycle (which writes manifests) and internal/app
// (which reads them) can share it without either importing the other. That
// is the same reason Manifest itself copies rather than embeds a
// state.Record.
//
// # What is deliberately not here
//
// No endpoint, no bucket, no region, no credential reference of any kind.
// A medium is named by its configured ID and nothing else, and the object
// is named by its key. That is enough for a rebuild to say "this artifact
// also lives on the medium you call offsite_s3, at this key" and to leave
// the operator's own configuration to say what offsite_s3 is. It is also
// the FR-33 rule applied to a file that, for a medium sidecar, is written
// into the bucket itself: anything in here is readable by everyone who can
// read the bucket.
type ManifestPlacement struct {
	// Medium is "local" or the id of a configured storage medium.
	Medium string `json:"medium"`

	// Location is an absolute path for a local placement and an object key
	// for a medium placement.
	Location string `json:"location"`

	// SizeBytes is what this copy measures, or nil when nobody recorded
	// it.
	SizeBytes *int64 `json:"size_bytes,omitempty"`

	// Checksum and ChecksumAlgorithm are the content hash recorded for
	// this copy, spelled the way the manifest's own Checksum fields are.
	Checksum          string `json:"checksum,omitempty"`
	ChecksumAlgorithm string `json:"checksum_algorithm,omitempty"`

	// VerificationClass is the class of verification this copy had
	// ACHIEVED when the manifest was written, or empty for none.
	VerificationClass string `json:"verification_class,omitempty"`

	// Status is the copy's own status: ACTIVE, DELETE_PENDING or GONE.
	Status string `json:"status"`
}

// ManifestPath computes the sidecar path for one artifact, exactly the
// way internal/lifecycle/transfer.go's finalPath/partialPath compute the
// artifact's own local paths: localDir joined with the artifact's basename
// plus a fixed suffix. WriteManifest, ReadManifest and ScanManifests all
// funnel through this one function, so the writer (commit.go) and the
// reader (catalog rebuild) can never disagree about where a manifest
// lives.
func ManifestPath(localDir, artifactName string) string {
	return filepath.Join(localDir, artifactName+manifestSuffix)
}

// manifestObjectDir is the key namespace a medium's sidecar objects live
// in, as FR-28 fixes the layout: <prefix>/<source>/<set>/.manifest/.
const manifestObjectDir = ".manifest"

// manifestObjectSuffix is appended to the artifact's own name to make its
// sidecar object's key.
//
// It is ".json" and not the local sidecar's ".manifest.json", because the
// two are disambiguated differently. A local sidecar sits in the same
// directory as the artifact, so its name has to say what it is; an object
// sidecar sits in its own .manifest/ namespace, where saying it twice
// would just make the key longer.
const manifestObjectSuffix = ".json"

// ManifestObjectKey derives the key of an artifact's sidecar OBJECT from
// the key of the artifact itself (EPIC E, FR-29).
//
// It is derived rather than composed independently, and that is the whole
// point: the artifact's key is built in one place (the transport
// boundary's own MediumKey, which owns FR-28's layout), and a sidecar
// whose key was composed a second time from the same parts would be one
// refactor away from pointing somewhere the artifact is not. Given the
// artifact's key, this can only ever produce a sidecar beside it.
//
// It refuses a key it cannot derive from rather than guessing, for
// MediumKey's own reason: a key is not only ever a key, since a restore
// writes it to a local path derived from it.
func ManifestObjectKey(artifactKey string) (string, error) {
	if artifactKey == "" {
		return "", fmt.Errorf("recovery: cannot derive a sidecar key from an empty artifact key")
	}
	if strings.HasSuffix(artifactKey, "/") {
		return "", fmt.Errorf("recovery: %q names a directory, not an artifact", artifactKey)
	}
	dir, base := "", artifactKey
	if i := strings.LastIndex(artifactKey, "/"); i >= 0 {
		dir, base = artifactKey[:i], artifactKey[i+1:]
	}
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("recovery: %q has no artifact name to derive a sidecar key from", artifactKey)
	}
	if dir == "" {
		return manifestObjectDir + "/" + base + manifestObjectSuffix, nil
	}
	return dir + "/" + manifestObjectDir + "/" + base + manifestObjectSuffix, nil
}

// Validate checks that m carries everything ReadManifest and WriteManifest
// both require: a format version this binary understands, a well-formed
// artifact identity, a non-empty remote path, and non-zero timestamps.
// WriteManifest calls this defensively before ever touching disk;
// ReadManifest calls it on whatever JSON it just parsed, so a
// hand-corrupted or truncated manifest is refused with a clear reason
// rather than silently reconstructed from partial data.
func (m Manifest) Validate() error {
	switch {
	case m.FormatVersion <= 0:
		return fmt.Errorf("recovery: manifest format_version must be positive, got %d", m.FormatVersion)
	case m.FormatVersion > CurrentFormatVersion:
		return fmt.Errorf("recovery: manifest format_version %d is newer than this binary understands (max %d)", m.FormatVersion, CurrentFormatVersion)
	case m.RemotePath == "":
		return fmt.Errorf("recovery: manifest needs a non-empty remote_path")
	case m.SizeBytes < 0:
		return fmt.Errorf("recovery: manifest size_bytes must not be negative, got %d", m.SizeBytes)
	case m.ReceivedTimestamp.IsZero():
		return fmt.Errorf("recovery: manifest needs a non-zero received_timestamp")
	case m.RetentionTimestamp.IsZero():
		return fmt.Errorf("recovery: manifest needs a non-zero retention_timestamp")
	}
	set, err := model.NewBackupSetID(m.Source, m.BackupSet)
	if err != nil {
		return fmt.Errorf("recovery: manifest backup set identity: %w", err)
	}
	if _, err := model.NewArtifactID(set, m.ArtifactName); err != nil {
		return fmt.Errorf("recovery: manifest artifact identity: %w", err)
	}
	return nil
}

// Artifact rebuilds the model.ArtifactID m describes. Callers use this
// instead of reaching into Source/BackupSet/ArtifactName by hand, so
// there is exactly one place that assembles the identity from a Manifest's
// raw fields.
func (m Manifest) Artifact() (model.ArtifactID, error) {
	set, err := model.NewBackupSetID(m.Source, m.BackupSet)
	if err != nil {
		return model.ArtifactID{}, err
	}
	return model.NewArtifactID(set, m.ArtifactName)
}

// WriteManifest durably writes m's sidecar file into localDir, deriving
// the path from m.ArtifactName via ManifestPath.
//
// The write is write-temp-then-rename, not a direct os.WriteFile: a
// process killed mid-write must never leave a torn, half-written manifest
// sitting at the real path where a later ScanManifests call would trip
// over it. This is a best-effort durability measure, not FR-14's own
// crash-safety proof (there is no matching directory fsync here: a
// recovery manifest is deliberately not part of that path's guarantee,
// only a courtesy written alongside it), and it is safe to call more than
// once for the same artifact: writing identical content twice, which is
// what a Commit retry does (see lifecycle/commit.go), simply overwrites
// the same bytes.
func WriteManifest(localDir string, m Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	path := ManifestPath(localDir, m.ArtifactName)
	data, err := EncodeManifest(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("recovery: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("recovery: rename %s to %s: %w", tmp, path, err)
	}
	return nil
}

// ReadManifest reads and validates the manifest at path.
func ReadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("recovery: read %s: %w", path, err)
	}
	m, err := DecodeManifest(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("recovery: %s: %w", path, err)
	}
	return m, nil
}

// EncodeManifest renders m as the bytes a sidecar carries, once m has been
// checked.
//
// It exists beside WriteManifest because a sidecar does not always land on
// a filesystem. FR-29 gives every artifact uploaded to a storage medium a
// sidecar OBJECT, under the medium's own .manifest/ namespace, carrying
// exactly these bytes, and an object is written through a MediumStore
// rather than through os.Rename. Splitting the encoding from the writing
// is what lets both sides share one format instead of growing a second,
// almost-identical one for objects.
//
// Nothing in Phase 1 uploads anything, so nothing calls this with a medium
// in mind yet; #238 is where the move engine does.
func EncodeManifest(m Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("recovery: encode manifest for %s: %w", m.ArtifactName, err)
	}
	return data, nil
}

// DecodeManifest parses and validates sidecar bytes, wherever they came
// from: a file beside the artifact, or an object read back off a medium.
//
// Everything it returns is an untrusted PROPOSAL (FR-32). A sidecar found
// on a medium was written by this product, but it sits in a bucket whose
// contents this product does not control, so nothing downstream may treat
// a value from here as authority over a journal row that already exists.
func DecodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

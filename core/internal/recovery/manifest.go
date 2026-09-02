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
	"path"
	"path/filepath"
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

	// Placements is every durable copy of this artifact the journal knew
	// about when the manifest was written (FR-29), so a catalog rebuilt
	// from sidecars can propose where the bytes are and not only that they
	// once existed.
	//
	// It is omitempty and FormatVersion is deliberately NOT bumped for it.
	// The field is purely additive: encoding/json ignores a key it has
	// never heard of, so a binary predating EPIC E reads a manifest
	// carrying placements and reconstructs exactly the row it reconstructs
	// today. Bumping the version would instead make every manifest this
	// build writes unreadable to the build an operator might roll back to,
	// which is a real cost paid for no information.
	Placements []ManifestPlacement `json:"placements,omitempty"`
}

// ManifestPlacement is one durable copy of an artifact as a sidecar
// records it: enough to FIND the copy and to say what was known about it,
// and deliberately nothing about how to authenticate to wherever it lives.
//
// FR-33's rule is that a credential never reaches a recovery manifest or a
// sidecar object, and the way this type keeps that rule is by not having
// anywhere to put one. It names the medium by its configured id and stops:
// no endpoint, no bucket, no region, no access key, no session token, no
// URL. Everything needed to actually reach the medium lives in config.yaml
// and in private state, where it already lives, and a sidecar sitting in
// the user backup root (a different security domain, see this package's
// own doc) is precisely the wrong place for any of it.
//
// TestManifestFieldsExcludeSecrets walks this struct too, recursively, so
// a field added here later has to survive the same name check the
// top-level ones do.
type ManifestPlacement struct {
	// Medium is the configured medium id ("local", or the id of one of
	// config.StorageMediums). It is an identifier, not a location: what
	// that id resolves to is config's business.
	Medium string `json:"medium"`

	// Location is the absolute path of a local copy, or the object key of
	// a copy on a medium. A key is not a credential and not an endpoint:
	// it is where inside an already-authenticated medium to look, and a
	// rebuild that cannot say that can only propose that an artifact
	// exists somewhere.
	Location string `json:"location"`

	// SizeBytes is what this product measured when it wrote this copy, or
	// nil when it never measured one.
	SizeBytes *int64 `json:"size_bytes,omitempty"`

	// Checksum and ChecksumAlgorithm are this copy's recorded content
	// hash, the same value and the same meaning as the manifest's own
	// top-level pair.
	Checksum          string `json:"checksum,omitempty"`
	ChecksumAlgorithm string `json:"checksum_algorithm,omitempty"`

	// VerificationClass is FR-31's ladder value, empty when nothing has
	// been proven about this copy, and VerifiedAt is when that class was
	// last achieved, nil when it never was. A rebuild treats both as an
	// untrusted proposal like everything else in the file: a sidecar
	// claiming "content" is a claim written by whoever wrote the file, not
	// a verification this process performed.
	VerificationClass string     `json:"verification_class,omitempty"`
	VerifiedAt        *time.Time `json:"verified_at,omitempty"`

	// Status is the placement status the journal recorded (ACTIVE,
	// DELETE_PENDING, GONE).
	Status string `json:"status"`
}

// ObjectManifestKeyFor derives the sidecar key for an artifact's object
// key on a medium (FR-28's layout, FR-29's sidecar): the artifact at
// <prefix>/<source>/<set>/<name> gets its manifest at
// <prefix>/<source>/<set>/.manifest/<name>.json.
//
// It takes the object key rather than building one from a prefix, source
// and set, so there is exactly one place in the product that decides an
// artifact's key (the MediumStore's, #235) and this composes onto whatever
// that decides instead of computing a second, drifting answer to the same
// question. The layout is deterministic and carries no timestamp and no
// random component, so re-uploading a sidecar targets the same object.
//
// Nothing writes one yet: the upload path is #235's and the move engine
// that would trigger it is #238's. The format and the key are settled here
// because a rebuild has to be able to read them, and a format decided by
// whoever happens to write the first one is how two halves of a recovery
// story end up disagreeing.
func ObjectManifestKeyFor(objectKey string) string {
	dir, name := path.Split(objectKey)
	return dir + objectManifestDir + "/" + name + objectManifestSuffix
}

// objectManifestDir and objectManifestSuffix spell FR-28's
// ".manifest/<artifact-name>.json". They are separate from manifestSuffix
// above because the two sidecars live differently on purpose: the local
// one sits beside its artifact so a directory listing shows the pair, and
// the object one sits in its own key namespace so a plain prefix listing
// of a bucket returns artifacts and not a manifest for every one of them.
const (
	objectManifestDir    = ".manifest"
	objectManifestSuffix = ".json"
)

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
	for i, p := range m.Placements {
		if err := p.validate(); err != nil {
			return fmt.Errorf("recovery: manifest placements[%d]: %w", i, err)
		}
	}
	return nil
}

// validate checks the shape of one recorded placement. It deliberately
// does NOT check that the medium is one this deployment has configured, or
// that anything is actually at the location: a sidecar is untrusted input
// (FR-32) and a rebuild's job is to propose what it says, not to believe
// it. What is checked here is only that the file is not garbled: a
// placement with no medium names nothing, and a status outside the
// vocabulary is a value nothing downstream can interpret.
func (p ManifestPlacement) validate() error {
	switch {
	case p.Medium == "":
		return fmt.Errorf("needs a non-empty medium")
	case p.SizeBytes != nil && *p.SizeBytes < 0:
		return fmt.Errorf("size_bytes must not be negative, got %d", *p.SizeBytes)
	}
	switch p.Status {
	case PlacementActive, PlacementDeletePending, PlacementGone:
	default:
		return fmt.Errorf("status %q is not one of %s, %s, %s", p.Status, PlacementActive, PlacementDeletePending, PlacementGone)
	}
	switch p.VerificationClass {
	case "", VerificationExistence, VerificationAttested, VerificationContent:
	default:
		return fmt.Errorf("verification_class %q is not one this build understands", p.VerificationClass)
	}
	return nil
}

// The placement vocabularies a sidecar may spell, mirroring
// internal/state's own. This package cannot import internal/state (it is a
// leaf both internal/lifecycle and internal/app depend on, and state
// depends on neither), so the two are pinned to each other by a test, the
// same arrangement config and artifactstore already use for the local
// medium id.
const (
	PlacementActive        = "ACTIVE"
	PlacementDeletePending = "DELETE_PENDING"
	PlacementGone          = "GONE"

	VerificationExistence = "existence"
	VerificationAttested  = "attested"
	VerificationContent   = "content"
)

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
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("recovery: encode manifest for %s: %w", m.ArtifactName, err)
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
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("recovery: parse %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("recovery: %s: %w", path, err)
	}
	return m, nil
}

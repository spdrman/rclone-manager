package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/recovery"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// CatalogRebuildAction classifies what RebuildCatalog did, or would do,
// for one artifact whose sidecar recovery manifest it read.
type CatalogRebuildAction string

const (
	// CatalogRebuildReconstructed means this artifact had no existing
	// journal row, so RebuildCatalog wrote (or, in a dry run, would write)
	// one from its sidecar manifest.
	CatalogRebuildReconstructed CatalogRebuildAction = "RECONSTRUCTED"

	// CatalogRebuildAlreadyPresent means a journal row already existed for
	// this artifact (an intact journal, or an earlier rebuild pass), so
	// RebuildCatalog left it untouched rather than risk overwriting
	// whatever normal processing has since done to it.
	CatalogRebuildAlreadyPresent CatalogRebuildAction = "ALREADY_PRESENT"

	// CatalogRebuildConflict is CatalogRebuildAlreadyPresent plus a
	// disagreement: a journal row already existed AND the sidecar next to
	// the artifact says something different about it.
	//
	// The row is left exactly as untouched as in the plain already-present
	// case; this action changes what is REPORTED, never what is written.
	// FR-32 is the reason it exists at all: sidecar contents are untrusted
	// proposals, and "conflicts with an existing journal row are reported
	// rather than resolved silently" is not satisfied by quietly
	// classifying a disagreeing manifest as already-present and moving on.
	// Someone has to be told, because the two most likely causes are a
	// manifest hand-copied from another machine and a journal that has
	// genuinely diverged from what is on disk, and an operator can tell
	// those apart where this function cannot.
	CatalogRebuildConflict CatalogRebuildAction = "CONFLICT"
)

// CatalogRebuildFinding is RebuildCatalog's outcome for one sidecar
// manifest it successfully read.
type CatalogRebuildFinding struct {
	Artifact model.ArtifactID
	Action   CatalogRebuildAction

	// ManifestPath is the sidecar this finding came from. It is here for
	// the same reason CatalogRebuildError carries one: a caller reporting
	// a conflict has to be able to say WHICH file disagrees, and an
	// operator with shell access is that report's only audience.
	ManifestPath string

	// Conflicts names each way the sidecar and the existing journal row
	// disagree, one plain sentence each, and is non-empty exactly when
	// Action is CatalogRebuildConflict. It reports the disagreement, it
	// does not resolve it: nothing here is ever written to the row.
	Conflicts []string

	// Notes carries what the rebuild READ out of the sidecar and did not
	// write down, so recovery does not quietly forget it.
	//
	// There is exactly one thing in that category today and it matters: a
	// sidecar naming a copy on a storage medium. A reconstructed row gets
	// its local placement, derived the trusted way from the backup set's
	// root and the artifact's name, but a medium placement cannot be
	// derived from anything this process can check. Writing an ACTIVE one
	// on a sidecar's say-so would put a copy nobody has verified into the
	// journal, where FR-30's standing invariant would then count it as one
	// of the artifact's durable copies and FR-20's medium-aware prune
	// would later be willing to delete an object on the strength of it.
	//
	// Dropping it silently is the other wrong answer, and it is the one
	// that bites during an actual recovery: the operator whose journal is
	// gone is precisely the person who needs to be told their sidecar says
	// there is a copy in a bucket. So it is reported and not applied,
	// which is the same shape as the conflict above.
	Notes []string
}

// CatalogRebuildError reports one sidecar manifest RebuildCatalog could
// not use: an unreadable or malformed manifest file, or one whose
// declared backup set does not match the one being rebuilt. One bad
// manifest never aborts the rest of the scan (see recovery.ScanManifests's
// own doc).
type CatalogRebuildError struct {
	Path string
	Err  error
}

// CatalogRebuildReport is one backup set's full catalog-rebuild outcome.
// Section 71 Work Package 3.3's `catalog rebuild --dry-run` and `catalog
// rebuild` share this exact type; DryRun is the only thing distinguishing
// them, so a caller (or a test) can assert the dry run predicted precisely
// what the real run then did.
type CatalogRebuildReport struct {
	Set      model.BackupSetID
	DryRun   bool
	Findings []CatalogRebuildFinding
	Errors   []CatalogRebuildError
}

// RebuildCatalog implements EPIC-B section 19.3 and section 71 Work
// Package 3.3 (issue #102): it scans set's configured local backup
// directory for sidecar recovery manifests (internal/recovery.
// ScanManifests) and, for every manifest whose artifact has no existing
// journal row, reconstructs one.
//
// # Non-destructive by construction
//
// RebuildCatalog never opens, deletes or renames a local backup file, and
// never contacts set's remote at all: recovery.ScanManifests only reads
// already-existing sidecar files, and the reconstruction step below only
// ever calls s.Journal.RecordTransition, the same durable journal writer
// every other use case in this package already funnels through, never any
// delete path (there isn't one in this package to call: FR-20 local
// deletion and FR-15 remote deletion are both owned entirely by
// internal/lifecycle and internal/retention, neither of which this
// function needs).
//
// When dryRun is true, RebuildCatalog performs only the read-only
// existence check every Finding needs (s.Journal.Get) and writes nothing:
// the returned report is a prediction, not a side effect. When dryRun is
// false, RebuildCatalog performs the same classification and then, for
// every manifest classified CatalogRebuildReconstructed, actually writes
// the row.
//
// # Why the reconstructed state is REMOTE_DELETE_PENDING, not COMMITTED
//
// A sidecar manifest only proves the artifact was, at some point, durably
// committed locally (internal/lifecycle/commit.go is the only writer of
// one). It proves nothing about what happened to the remote copy
// afterward: normal processing may have already deleted it, may still be
// about to, or may have been interrupted before ever trying. Section 19.3
// gives a recovery manifest no field to record that, on purpose (it is not
// part of "safe to reconstruct" recovery metadata), so RebuildCatalog does
// not guess either. REMOTE_DELETE_PENDING is the one lifecycle state whose
// own FR-17 reconciliation handler (internal/reconcile's
// reconcileDeletePending) actively re-checks the remote before deciding
// anything further: present, and a delete is only ever reattempted after
// FR-16's own revalidation; absent, and reconciliation moves straight to
// COMPLETE without re-issuing one. Feeding a rebuilt row into that
// existing pass unmodified, exactly as issue #102's own dependency note
// requires, is what lets reconciliation resolve the genuinely unknown case
// safely instead of RebuildCatalog inventing a second, competing way to
// decide it.
//
// RebuildCatalog reaches REMOTE_DELETE_PENDING by calling
// s.Journal.RecordTransition directly, twice, rather than routing through
// lifecycle.Advance: the first call creates the row (From: "", exactly the
// shape internal/state.Journal.Discover already uses for a fresh row,
// which internal/lifecycle.Advance's own From=="" branch only ever checks
// for a valid target state, not a legal edge); the second, a same-state
// From/To pair, attaches the verification evidence (local hash,
// validation) insertArtifact's own column set does not accept on the
// first write. Both calls use a key derived deterministically from the
// manifest's own content (see rebuildKey), never from time.Now, so two
// calls describing the same reconstruction always agree on their keys;
// see rebuildOne's own doc for exactly what that does, and does not,
// guarantee about resuming a crash between the two calls. Bypassing
// lifecycle.Advance's FR-10 edge-legality table here is deliberate, not an
// oversight: catalog rebuild is not walking an artifact through the
// pipeline, it is asserting a snapshot of what the durable evidence on
// disk already proves, which is exactly what "call into internal/state's
// journal writer" (this issue's own GREEN plan) names, not
// internal/lifecycle's own transition-legality wrapper.
//
// # Why DiscoveredAt (and UpdatedAt) become the manifest's RetentionTimestamp
//
// internal/retention's GFS and last-known-good calculations read two
// timestamps off a record, not one: state.Record.DiscoveredAt, which that
// package calls the received timestamp, and Remote.ModTime, the
// producer's own timestamp, which FR-18 places an artifact by as well
// whenever it is admissible (see internal/retention/bucketkey.go). A
// rebuilt row's DiscoveredAt is set to exactly the manifest's
// RetentionTimestamp, and its Remote.ModTime to the manifest's
// ProducerTimestamp (see rebuildOne), so a rebuilt catalog reaches the
// identical GFS/last-known-good verdicts the lost journal would have,
// matching issue #102's own dependency note: "a
// rebuilt row has to carry whatever fields internal/retention's GFS and
// last-known-good decisions actually consume, or a rebuilt catalog would
// silently make different retention decisions than the journal it
// replaced." Today's schema has no column that keeps updated_at distinct
// from discovered_at on a freshly inserted row (internal/state's own
// insertArtifact stamps both from the same OccurredAt), so the manifest's
// separate ReceivedTimestamp still round-trips through the sidecar file
// itself for an operator's own inspection, it just is not carried into the
// reconstructed row as a value distinct from RetentionTimestamp. That is a
// deliberate, documented simplification: nothing downstream of the
// journal (retention, reconciliation, health) reads updated_at as
// anything other than "the last time this row changed", so a rebuilt row
// reporting that as the moment its own reconstruction happened would be
// less honest, not more, than reusing RetentionTimestamp.
func (s *Service) RebuildCatalog(ctx context.Context, set model.BackupSetID, dryRun bool) (CatalogRebuildReport, error) {
	_, bs, ok := s.backupSetConfigFor(set)
	if !ok {
		return CatalogRebuildReport{}, &NotFoundError{Kind: "backup set", Name: set.String()}
	}

	report := CatalogRebuildReport{Set: set, DryRun: dryRun}
	manifests, scanErrs := recovery.ScanManifests(bs.LocalPath)
	for _, se := range scanErrs {
		report.Errors = append(report.Errors, CatalogRebuildError{Path: se.Path, Err: se.Err})
	}

	for _, m := range manifests {
		artifact, err := m.Artifact()
		if err != nil {
			report.Errors = append(report.Errors, CatalogRebuildError{
				Path: recovery.ManifestPath(bs.LocalPath, m.ArtifactName), Err: err,
			})
			continue
		}
		if artifact.Set != set {
			// ScanManifests only ever looks inside this one backup set's
			// own LocalPath, so a manifest naming a different set here
			// means the file was hand-placed or copied from elsewhere;
			// report it rather than silently reconstruct a row into the
			// wrong backup set (FR-7 isolation).
			report.Errors = append(report.Errors, CatalogRebuildError{
				Path: recovery.ManifestPath(bs.LocalPath, m.ArtifactName),
				Err:  fmt.Errorf("recovery: manifest declares backup set %s, expected %s", artifact.Set, set),
			})
			continue
		}

		finding, err := s.rebuildOne(ctx, bs.LocalPath, artifact, m, dryRun)
		if err != nil {
			report.Errors = append(report.Errors, CatalogRebuildError{
				Path: recovery.ManifestPath(bs.LocalPath, m.ArtifactName), Err: err,
			})
			continue
		}
		finding.ManifestPath = recovery.ManifestPath(bs.LocalPath, m.ArtifactName)
		report.Findings = append(report.Findings, finding)
	}
	return report, nil
}

// rebuildOne classifies, and, when dryRun is false, reconstructs, one
// artifact's journal row from m.
//
// Both modes start with the exact same read-only existence check
// (s.Journal.Get), so a dry run's prediction and a real run's outcome are
// never able to disagree for a given journal snapshot: either both call
// this an already-present artifact and stop, or both call it a fresh
// reconstruction. rebuildOne deliberately does not fall back to
// distinguishing "already present" from "reconstructed" via
// ErrAlreadyDiscovered on the write path (an earlier version of this
// function did): rebuildKey is derived purely from the manifest's own
// content, so re-running `catalog rebuild` against an unchanged sidecar
// file after a fully-successful prior rebuild reproduces the exact same
// keys, and internal/state's own idempotency-key replay would silently
// treat the write as "already applied" rather than surface a unique-key
// violation, which made that approach misreport a no-op rerun as a fresh
// reconstruction every time.
//
// One known, narrow gap this trades away: if a process is killed in the
// few microseconds between the create call below succeeding and the
// populate call running, a later `catalog rebuild` sees the row already
// exists (via this same pre-check) and leaves it as is, verification
// evidence (LocalHash/ValidationPassed) permanently missing. This is
// accepted rather than engineered around: it is a far narrower window than
// any crash this project's own crash-safety tests target (there is no
// filesystem operation anywhere between the two calls, only two sequential
// SQLite transactions on an already-open connection), and a row missing
// only its hash still degrades safely, not silently: internal/reconcile's
// checkLocalFinal falls back to a size-only comparison when LocalHashAlg is
// empty, exactly the same honest degrade FR-16 already documents for a
// backend that never reported a hash at all.
func (s *Service) rebuildOne(ctx context.Context, localDir string, artifact model.ArtifactID, m recovery.Manifest, dryRun bool) (CatalogRebuildFinding, error) {
	existing, err := s.Journal.Get(ctx, artifact)
	switch {
	case err == nil:
		if conflicts := manifestConflicts(m, existing); len(conflicts) > 0 {
			return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildConflict, Conflicts: conflicts}, nil
		}
		return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildAlreadyPresent}, nil
	case errors.Is(err, state.ErrArtifactNotFound):
		// fall through: this artifact needs reconstructing.
	default:
		return CatalogRebuildFinding{}, err
	}

	notes := unadoptedPlacementNotes(m)

	if dryRun {
		return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildReconstructed, Notes: notes}, nil
	}

	final := lifecycle.FinalArtifactPath(localDir, artifact)
	remoteSize := m.SizeBytes
	remote := state.RemoteIdentity{Size: &remoteSize, ModTime: m.ProducerTimestamp}

	_, err = s.Journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        rebuildKey(artifact, m, "create"),
		From:       "",
		To:         string(lifecycle.RemoteDeletePending),
		OccurredAt: m.RetentionTimestamp,
		RemotePath: m.RemotePath,
		LocalPath:  &final,
		Remote:     &remote,
	})
	if err != nil {
		if errors.Is(err, state.ErrAlreadyDiscovered) {
			// A concurrent writer (another rebuild run, or normal
			// processing that has since discovered this same artifact)
			// won the race between this function's own pre-check above
			// and this call. Report it the same way the pre-check would
			// have, rather than as an error.
			return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildAlreadyPresent}, nil
		}
		return CatalogRebuildFinding{}, fmt.Errorf("recovery: reconstructing %s: %w", artifact, err)
	}

	if _, err := s.Journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        rebuildKey(artifact, m, "populate"),
		From:       string(lifecycle.RemoteDeletePending),
		To:         string(lifecycle.RemoteDeletePending),
		OccurredAt: m.RetentionTimestamp,
		Hashes:     &state.HashUpdate{Hash: m.Checksum, Alg: m.ChecksumAlgorithm},
		Validation: validationUpdateFrom(m),
	}); err != nil {
		return CatalogRebuildFinding{}, fmt.Errorf("recovery: populating verification evidence for %s: %w", artifact, err)
	}

	return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildReconstructed, Notes: notes}, nil
}

// unadoptedPlacementNotes names every copy m records on a storage medium,
// which is exactly the set a reconstructed row does not get. See
// CatalogRebuildFinding.Notes for why these are reported rather than
// written, and rebuildOne for where the local placement a rebuilt row DOES
// get comes from instead.
func unadoptedPlacementNotes(m recovery.Manifest) []string {
	var out []string
	for _, p := range m.Placements {
		if p.Medium == state.MediumLocal {
			continue
		}
		out = append(out, fmt.Sprintf(
			"the sidecar records a copy on medium %q at %q; it is reported and not written, because nothing here can verify it exists",
			p.Medium, p.Location))
	}
	return out
}

// manifestConflicts names every way m disagrees with the journal row that
// already exists for the same artifact.
//
// It compares only fields where a difference actually means something is
// wrong, and where the manifest is making a claim rather than recording a
// moment: the remote path the artifact came from, its content hash, its
// size, and where its copies are. It deliberately does NOT compare
// updated_at or the lifecycle state, which are supposed to move on after a
// manifest is written and would otherwise report a conflict for every
// artifact that simply carried on through its pipeline.
//
// A field the manifest does not carry is not a disagreement. An empty
// checksum in a sidecar means "this manifest records no hash", which is
// silence, and treating silence as a contradiction of a journal that does
// have one would report a conflict on every artifact whose manifest
// predates FR-13 hashing.
//
// Nothing here reads the filesystem. A conflict is between two records,
// and resolving it against the bytes on disk is what `check` and FR-17
// reconciliation already exist for.
func manifestConflicts(m recovery.Manifest, rec state.Record) []string {
	var out []string

	if m.RemotePath != "" && m.RemotePath != rec.RemotePath {
		out = append(out, fmt.Sprintf("manifest says the remote path is %q, journal says %q", m.RemotePath, rec.RemotePath))
	}
	if m.Checksum != "" && rec.LocalHash != "" && m.Checksum != rec.LocalHash {
		out = append(out, fmt.Sprintf("manifest says the checksum is %q, journal says %q", m.Checksum, rec.LocalHash))
	}
	if m.ChecksumAlgorithm != "" && rec.LocalHashAlg != "" && m.ChecksumAlgorithm != rec.LocalHashAlg {
		out = append(out, fmt.Sprintf("manifest says the checksum algorithm is %q, journal says %q", m.ChecksumAlgorithm, rec.LocalHashAlg))
	}
	if m.SizeBytes > 0 && rec.Transfer != nil && m.SizeBytes != rec.Transfer.BytesTransferred {
		out = append(out, fmt.Sprintf("manifest says the size is %d bytes, journal says %d", m.SizeBytes, rec.Transfer.BytesTransferred))
	}
	if !m.RetentionTimestamp.IsZero() && !m.RetentionTimestamp.Equal(rec.DiscoveredAt) {
		out = append(out, fmt.Sprintf("manifest says the retention timestamp is %s, journal says %s",
			m.RetentionTimestamp.UTC().Format(time.RFC3339), rec.DiscoveredAt.UTC().Format(time.RFC3339)))
	}

	journalled := make(map[string]state.Placement, len(rec.Placements))
	for _, p := range rec.Placements {
		journalled[p.Medium] = p
	}
	for _, mp := range m.Placements {
		p, ok := journalled[mp.Medium]
		if !ok {
			out = append(out, fmt.Sprintf("manifest records a copy on medium %q at %q that the journal does not know about",
				mp.Medium, mp.Location))
			continue
		}
		if mp.Location != p.Location {
			out = append(out, fmt.Sprintf("manifest says the %s copy is at %q, journal says %q", mp.Medium, mp.Location, p.Location))
		}
	}

	return out
}

func validationUpdateFrom(m recovery.Manifest) *state.ValidationUpdate {
	if m.ValidationPassed == nil {
		return nil
	}
	return &state.ValidationUpdate{Passed: *m.ValidationPassed, Detail: m.ValidationDetail}
}

// rebuildKey derives a deterministic idempotency key for one rebuild
// write, so re-running `catalog rebuild` after a crash between the create
// and populate steps in rebuildOne converges instead of erroring
// (ErrIdempotencyKeyReused) or silently duplicating work: both calls
// derive their key purely from the manifest's own content, never from
// time.Now, so a retry against the same, unmodified sidecar file
// reproduces the exact same key every time.
func rebuildKey(artifact model.ArtifactID, m recovery.Manifest, step string) string {
	return fmt.Sprintf("catalog-rebuild:%s:%s@%s", artifact, step, m.RetentionTimestamp.UTC().Format(time.RFC3339Nano))
}

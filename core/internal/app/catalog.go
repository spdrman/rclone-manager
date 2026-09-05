package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/recovery"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Rebuilding the journal from what is on the disk, when the journal is the
// thing that was lost.
//
// Every other read in this package treats the journal as the truth and a
// sidecar manifest as something written beside an artifact for later. This
// file is the one place that runs the other way round, for the one situation
// where it has to: the database is gone or unreadable, the backups are all
// still there, and the manifests are the only record of what they are.
//
// That inversion is exactly why nothing here overwrites. A sidecar is an
// untrusted PROPOSAL, not a source of truth (FR-32), so a row that already
// exists is left alone, and a sidecar that disagrees with an existing row
// about a hash, a size or a retention timestamp is reported as a conflict and
// applied to nothing. Resolving a disagreement in the sidecar's favour would
// be a path by which a stale or tampered file on the backup root rewrites
// what retention keeps and what verification compares against, which is a
// much worse failure than a rebuild that needs a person to look at it.
//
// CONFLICT and ALREADY_PRESENT are separate outcomes for the same reason: to
// a person reading the report they mean opposite things, one being "nothing
// to do here" and the other "two things that should agree do not".

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

	// CatalogRebuildConflict means a journal row already existed AND the
	// sidecar disagrees with it about something that matters: a different
	// content hash, a different size, or a different retention timestamp.
	//
	// It is reported and never applied (EPIC E, FR-32). A sidecar is an
	// untrusted PROPOSAL, and a reconstruction that resolved a
	// disagreement in the sidecar's favour would be a path by which a
	// stale or tampered file on the backup root rewrites the journal's own
	// hashes and timestamps, which is to say rewrites what retention keeps
	// and what verification compares against. The operator is told, and
	// the row stays exactly as it was.
	//
	// It is a distinct outcome from ALREADY_PRESENT rather than a variant
	// of it, because the two mean opposite things to a person reading the
	// report: already-present is "nothing to do here", and a conflict is
	// "two things that should agree do not, and you need to find out why".
	CatalogRebuildConflict CatalogRebuildAction = "CONFLICT"
)

// CatalogRebuildFinding is RebuildCatalog's outcome for one sidecar
// manifest it successfully read.
type CatalogRebuildFinding struct {
	Artifact model.ArtifactID
	Action   CatalogRebuildAction

	// ManifestPath is the sidecar this finding came from. It is carried
	// on the finding, the same way CatalogRebuildError already carries
	// one, so a caller reporting a conflict can name the file without
	// recomputing the path or importing internal/recovery to do it.
	ManifestPath string

	// Conflicts names, in words, each way the sidecar disagreed with the
	// existing journal row. It is populated only for
	// CatalogRebuildConflict, and it is prose rather than a structured
	// diff on purpose: the audience is an operator deciding whether a
	// sidecar is stale or a journal is wrong, and neither answer comes
	// from the shape of the difference.
	Conflicts []string

	// Notes are things the operator needs to know about a reconstruction
	// that otherwise succeeded: what the sidecar said that this rebuild
	// read and did not adopt.
	//
	// A note is not a failure and not a conflict. The row was rebuilt and
	// is correct as far as it goes; a note says where it stops going.
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
		// The row is left exactly as it is either way. What changes is
		// what the operator is told: a sidecar that agrees is nothing to
		// act on, and one that does not is (EPIC E, FR-32).
		if conflicts := manifestConflicts(existing, m); len(conflicts) > 0 {
			return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildConflict, Conflicts: conflicts}, nil
		}
		return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildAlreadyPresent}, nil
	case errors.Is(err, state.ErrArtifactNotFound):
		// fall through: this artifact needs reconstructing.
	default:
		return CatalogRebuildFinding{}, err
	}

	notes := unadoptablePlacementNotes(m)

	if dryRun {
		return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildReconstructed, Notes: notes}, nil
	}

	final, err := lifecycle.FinalArtifactPath(localDir, artifact)
	if err != nil {
		return CatalogRebuildFinding{}, fmt.Errorf("resolving where %s belongs under %q: %w", artifact, localDir, err)
	}
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

// unadoptablePlacementNotes describes every copy the sidecar names that a
// rebuild reads and deliberately does not write into the journal.
//
// A reconstructed row gets its local placement the trusted way, derived
// from the backup set's own root and the artifact's name, exactly as
// ingestion derives it. There is no equivalent derivation for an object in
// a bucket: the only source for it is the sidecar, and a sidecar is an
// untrusted proposal (FR-32). Writing an ACTIVE medium placement on its
// say-so would put a copy nobody has verified into the journal, where
// FR-30's standing invariant counts it as one of the artifact's durable
// copies and a medium-aware prune becomes willing to delete an object on
// the strength of it.
//
// Dropping it silently is the other wrong answer, and it is the one that
// bites during a real recovery: the operator whose journal is gone is
// exactly the person who needs to be told their sidecar says there is a
// copy somewhere else. So it is reported and not applied, which is the
// shape the conflict verdict above already has.
func unadoptablePlacementNotes(m recovery.Manifest) []string {
	var out []string
	for _, p := range m.Placements {
		if p.Medium == "" || p.Medium == state.MediumLocal {
			continue
		}
		out = append(out, fmt.Sprintf(
			"the sidecar records a copy on medium %q at %q, which this rebuild reports and does not adopt: a placement on a medium can only come from the sidecar, and a sidecar is a proposal",
			p.Medium, p.Location))
	}
	return out
}

// manifestConflicts lists the ways m disagrees with the journal row that
// already exists for the same artifact.
//
// It compares only the three facts a rebuild would otherwise have written,
// and that a later verification or retention decision then acts on: the
// content hash, the size, and the retention-relevant timestamp. It
// deliberately does not compare everything a manifest carries. A validation
// detail string that has been reworded, or a remote path that changed
// because a producer moved its output directory, are not disagreements
// about the artifact; flagging them would train an operator to scroll past
// this report, which is the failure mode a conflict report has.
//
// A field the sidecar does not carry at all is not a conflict either. An
// artifact verified without hash: sha256 has no checksum in its manifest
// and none in its journal row, and an older manifest may predate a field
// entirely; absence is silence, and silence disagrees with nothing.
func manifestConflicts(rec state.Record, m recovery.Manifest) []string {
	var out []string

	if m.Checksum != "" && rec.LocalHash != "" && !strings.EqualFold(m.Checksum, rec.LocalHash) {
		out = append(out, fmt.Sprintf(
			"the sidecar records checksum %s but the journal recorded %s at verification",
			m.Checksum, rec.LocalHash))
	}
	if m.SizeBytes > 0 && rec.Transfer != nil && rec.Transfer.BytesTransferred != m.SizeBytes {
		out = append(out, fmt.Sprintf(
			"the sidecar records %d bytes but the journal recorded %d transferred",
			m.SizeBytes, rec.Transfer.BytesTransferred))
	}
	if !m.RetentionTimestamp.IsZero() && !m.RetentionTimestamp.Equal(rec.DiscoveredAt) {
		out = append(out, fmt.Sprintf(
			"the sidecar records a retention timestamp of %s but the journal recorded %s, which would place this artifact in a different retention bucket",
			m.RetentionTimestamp.UTC().Format(time.RFC3339), rec.DiscoveredAt.UTC().Format(time.RFC3339)))
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

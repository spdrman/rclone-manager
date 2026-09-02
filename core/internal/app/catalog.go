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
)

// CatalogRebuildFinding is RebuildCatalog's outcome for one sidecar
// manifest it successfully read.
type CatalogRebuildFinding struct {
	Artifact model.ArtifactID
	Action   CatalogRebuildAction
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
	_, err := s.Journal.Get(ctx, artifact)
	switch {
	case err == nil:
		return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildAlreadyPresent}, nil
	case errors.Is(err, state.ErrArtifactNotFound):
		// fall through: this artifact needs reconstructing.
	default:
		return CatalogRebuildFinding{}, err
	}

	if dryRun {
		return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildReconstructed}, nil
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

	return CatalogRebuildFinding{Artifact: artifact, Action: CatalogRebuildReconstructed}, nil
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

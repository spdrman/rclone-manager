package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MediumLocal is the implicit storage medium every deployment already has:
// the backup set's own local_path, with exactly today's semantics.
//
// It is the same string config.MediumLocal and artifactstore.KindLocal
// spell, and it is deliberately spelled again here rather than imported:
// internal/state sits under everything and imports internal/model and
// nothing else of this project's own, for the reason that a journal that
// depended on the config package could not be read without one. The
// agreement between the three is pinned by a test.
const MediumLocal = "local"

// A placement's own status, distinct from the artifact's lifecycle state.
//
// These three are internal/state's own vocabulary, unlike Record.State and
// unlike Placement.VerificationClass: where a copy is in its own life is a
// fact about storage, which is what this package owns, while a lifecycle
// state belongs to internal/lifecycle and a verification class belongs to
// internal/placement (#237). The CHECK constraint in 0007_placements.sql
// is the schema-side copy of this list.
const (
	// PlacementActive means the copy is meant to be there.
	PlacementActive = "ACTIVE"
	// PlacementDeletePending means a delete has been decided and durably
	// recorded, and may not have happened yet. FR-30's move engine writes
	// it before it touches anything, so a crash between the decision and
	// the delete is reconcilable rather than ambiguous.
	PlacementDeletePending = "DELETE_PENDING"
	// PlacementGone means the copy is no longer there and the journal
	// knows it.
	PlacementGone = "GONE"
)

// The verification classes a placement can have ACHIEVED (FR-31's ladder).
//
// The names live here because 0007_placements.sql constrains the column to
// exactly this set, and a vocabulary the schema enforces should be spelled
// once, beside the schema that enforces it. What each class MEANS, what it
// costs, and what it takes to earn one is core/internal/placement's (#237):
// this package only stores the answer, the same division of labour
// Record.State already has with core/internal/lifecycle. #237 carries the
// test that pins its ladder against this list, so the two cannot drift.
//
// Empty is a fourth value and the default: nothing has verified this copy.
// It is not a class, which is why it has no constant: a caller reaching for
// a name for "unverified" is usually about to record it as a weak pass,
// and the point of the ladder is that a class is something achieved.
const (
	// VerificationContent means the bytes were read back and hashed, and
	// they match the hash the journal recorded.
	VerificationContent = "content"
	// VerificationAttested means the medium's own stored full-object
	// checksum equals the recorded hash. One metadata call, no egress,
	// and it trusts the endpoint.
	VerificationAttested = "attested"
	// VerificationExistence means the object exists at the recorded size.
	// One HEAD request, and it proves nothing about the bytes.
	VerificationExistence = "existence"
)

// Placement is one durable copy of one artifact (EPIC E, FR-29).
//
// It exists as a row of its own rather than as columns on the artifact
// because an artifact can have several copies at once, during a move, and
// no local copy at all after one, and neither of those is expressible in a
// single local_path column.
type Placement struct {
	// Medium is MediumLocal or the id of a configured storage medium.
	Medium string

	// Location is an absolute path for a local placement and an object key
	// for a medium placement. Only the store named by Medium knows how to
	// interpret it.
	Location string

	// Size is what this copy measures, or nil when nobody recorded it. A
	// pointer for RemoteIdentity's reason: an artifact can genuinely be
	// zero bytes, so a zero must not double as "not reported".
	Size *int64

	// Hash and HashAlg are the content hash recorded for THIS copy.
	Hash    string
	HashAlg string

	// VerificationClass is the strongest class of verification this copy
	// has ACHIEVED, never the strongest one configured (FR-31). Empty
	// means nothing has verified it. See the Verification* constants.
	VerificationClass string

	// VerifiedAt is when VerificationClass was last achieved, or nil.
	VerifiedAt *time.Time

	// Status is one of PlacementActive, PlacementDeletePending or
	// PlacementGone.
	Status string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsLocal reports whether this placement is the local one.
func (p Placement) IsLocal() bool { return p.Medium == MediumLocal }

// PlacementUpdate is what a caller tells RecordTransition about where an
// artifact's bytes now are.
//
// It is caller-supplied rather than derived here from LocalPath, and that
// is the load-bearing decision in this file. LocalPath carries two
// meanings during an artifact's life: at TRANSFERRING it is the .partial
// being written, and only from COMMITTED onward is it the finished
// artifact. This package cannot tell those apart without knowing
// internal/lifecycle's state vocabulary, and a placement written for a
// half-transferred file would be a row claiming a durable copy exists
// where none does. So the caller that actually creates the durable copy,
// lifecycle's own Commit, is the caller that says so.
//
// Writing the same placement twice updates the one row rather than adding
// a second: FR-28's key layout is deterministic, so an artifact has
// exactly one location per medium, and an interrupted upload that resumes
// writes the same placement again.
type PlacementUpdate struct {
	Medium            string
	Location          string
	Size              *int64
	Hash              string
	HashAlg           string
	VerificationClass string
	VerifiedAt        *time.Time

	// Status defaults to PlacementActive when empty, because that is what
	// a caller recording a copy it just made always means.
	Status string
}

// LocalPlacement returns the artifact's ACTIVE local copy, if it has one.
func (r Record) LocalPlacement() (Placement, bool) {
	for _, p := range r.Placements {
		if p.IsLocal() && p.Status == PlacementActive {
			return p, true
		}
	}
	return Placement{}, false
}

// ReadableLocalPath answers "can I read this artifact off local disk, and
// where", and it is the one place every caller that used to reach for
// Record.LocalPath now asks.
//
// # Why an accessor rather than four packages reading a field
//
// FR-29's sweep: lifecycle's pre-delete check, revalidation, the
// application-level validate pass and reconciliation all assume LocalPath
// is a readable file. That assumption is true today and stops being true
// the moment an artifact's only copy lives on a storage medium. Routing
// them through one function means the change that makes it false (#239) is
// a change to this function, not a hunt through four packages for the ones
// somebody forgot.
//
// # Why it falls back to LocalPath
//
// The fallback is what makes this sweep provably behaviour-neutral in
// Phase 1. Migration 0007 backfills a local placement for every artifact
// with a durable local copy, and lifecycle's Commit records one for every
// artifact committed since, so a record loaded from the journal answers
// from its placement. A Record built by hand, which is how most of this
// repository's tests build one, has no placements at all, and for those
// the honest answer is still the one the field gives.
//
// It is not a permanent arrangement. When a placement can legitimately be
// somewhere other than local, a record with no placements stops meaning
// "local, as before" and starts meaning "nobody said", and this fallback
// is what #239 removes.
func (r Record) ReadableLocalPath() (string, bool) {
	if p, ok := r.LocalPlacement(); ok {
		return p.Location, p.Location != ""
	}
	if len(r.Placements) > 0 {
		// The artifact has placements and none of them is an active local
		// one, which is a positive statement rather than an absence: its
		// bytes are somewhere this caller cannot read with an os.Open.
		return "", false
	}
	return r.LocalPath, r.LocalPath != ""
}

const placementColumns = `
	medium, location, size_bytes, hash, hash_alg,
	verification_class, verified_at, status, created_at, updated_at`

// loadPlacements reads the placements for one artifact row.
func loadPlacements(ctx context.Context, q querier, artifactRowID int64) ([]Placement, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+placementColumns+` FROM placements WHERE artifact_id = ? ORDER BY medium`,
		artifactRowID)
	if err != nil {
		return nil, fmt.Errorf("state: read placements: %w", err)
	}
	defer rows.Close()

	var out []Placement
	for rows.Next() {
		p, err := scanPlacement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: read placements: %w", err)
	}
	return out, nil
}

// loadPlacementsFor reads the placements for many artifact rows in one
// query and returns them keyed by artifact row id.
//
// One query rather than one per record, because the list paths
// (ListByState, ListByBackupSet) are what a retention cycle runs over every
// backup set, and an N+1 there is a real cost on a NAS with thousands of
// artifacts.
func loadPlacementsFor(ctx context.Context, q querier, artifactRowIDs []int64) (map[int64][]Placement, error) {
	out := map[int64][]Placement{}
	if len(artifactRowIDs) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(artifactRowIDs))
	args := make([]any, len(artifactRowIDs))
	for i, id := range artifactRowIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := q.QueryContext(ctx,
		`SELECT artifact_id, `+placementColumns+
			` FROM placements WHERE artifact_id IN (`+join(placeholders, ", ")+`) ORDER BY artifact_id, medium`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("state: read placements: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var artifactRowID int64
		var (
			medium, location, hash, hashAlg string
			verificationClass, status       string
			size                            sql.NullInt64
			verifiedAt                      sql.NullString
			createdAt, updatedAt            string
		)
		if err := rows.Scan(&artifactRowID, &medium, &location, &size, &hash, &hashAlg,
			&verificationClass, &verifiedAt, &status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("state: scan placement: %w", err)
		}
		p, err := buildPlacement(medium, location, size, hash, hashAlg, verificationClass, verifiedAt, status, createdAt, updatedAt)
		if err != nil {
			return nil, err
		}
		out[artifactRowID] = append(out[artifactRowID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: read placements: %w", err)
	}
	return out, nil
}

func scanPlacement(row scanRow) (Placement, error) {
	var (
		medium, location, hash, hashAlg string
		verificationClass, status       string
		size                            sql.NullInt64
		verifiedAt                      sql.NullString
		createdAt, updatedAt            string
	)
	if err := row.Scan(&medium, &location, &size, &hash, &hashAlg,
		&verificationClass, &verifiedAt, &status, &createdAt, &updatedAt); err != nil {
		return Placement{}, fmt.Errorf("state: scan placement: %w", err)
	}
	return buildPlacement(medium, location, size, hash, hashAlg, verificationClass, verifiedAt, status, createdAt, updatedAt)
}

func buildPlacement(
	medium, location string, size sql.NullInt64, hash, hashAlg string,
	verificationClass string, verifiedAt sql.NullString, status string,
	createdAt, updatedAt string,
) (Placement, error) {
	created, err := parseTime(createdAt)
	if err != nil {
		return Placement{}, fmt.Errorf("state: stored placement created_at %q is invalid: %w", createdAt, err)
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return Placement{}, fmt.Errorf("state: stored placement updated_at %q is invalid: %w", updatedAt, err)
	}
	verified, err := scanOptionalTime(verifiedAt)
	if err != nil {
		return Placement{}, fmt.Errorf("state: stored placement verified_at is invalid: %w", err)
	}
	return Placement{
		Medium:            medium,
		Location:          location,
		Size:              scanOptionalInt64(size),
		Hash:              hash,
		HashAlg:           hashAlg,
		VerificationClass: verificationClass,
		VerifiedAt:        verified,
		Status:            status,
		CreatedAt:         created,
		UpdatedAt:         updated,
	}, nil
}

// upsertPlacement writes one placement inside the caller's transaction, so
// it lands with the transition that describes it or not at all.
//
// ON CONFLICT on (artifact_id, medium) rather than a delete-then-insert:
// the row's created_at is when this copy first came into being, and a
// resumed upload writing the same placement again must not restart that
// clock.
func upsertPlacement(ctx context.Context, tx *sql.Tx, artifactRowID int64, p PlacementUpdate, now time.Time) error {
	if p.Medium == "" {
		return fmt.Errorf("state: a placement needs a medium")
	}
	if p.Location == "" {
		return fmt.Errorf("state: a placement on %q needs a location", p.Medium)
	}
	status := p.Status
	if status == "" {
		status = PlacementActive
	}
	stamp := formatTime(now)

	_, err := tx.ExecContext(ctx, `
		INSERT INTO placements (
			artifact_id, medium, location, size_bytes, hash, hash_alg,
			verification_class, verified_at, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (artifact_id, medium) DO UPDATE SET
			location           = excluded.location,
			size_bytes         = excluded.size_bytes,
			hash               = excluded.hash,
			hash_alg           = excluded.hash_alg,
			verification_class = excluded.verification_class,
			verified_at        = excluded.verified_at,
			status             = excluded.status,
			updated_at         = excluded.updated_at`,
		artifactRowID, p.Medium, p.Location, optionalInt64(p.Size), p.Hash, p.HashAlg,
		p.VerificationClass, optionalTimeText(p.VerifiedAt), status, stamp, stamp,
	)
	if err != nil {
		return fmt.Errorf("state: recording the %s placement: %w", p.Medium, err)
	}
	return nil
}

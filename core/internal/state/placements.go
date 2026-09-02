package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MediumLocal is the medium id every placement carried before EPIC E and
// every placement this phase writes: the backup set's own local_path, with
// exactly today's semantics.
//
// It is the same string as config.MediumLocal and artifactstore.KindLocal.
// This package does not import either one (it sits under both, and the
// journal has no business knowing what a configured medium is), so the
// three are pinned to each other by a test instead, the same arrangement
// config's own doc describes.
const MediumLocal = "local"

// The FR-29 placement statuses, exactly as migration 0007's CHECK pins
// them.
//
// ACTIVE is the only one this phase writes. A placement is ACTIVE when it
// is the journal's answer to "where does this artifact live"; the other
// two belong to the move engine (#238), which is the only thing that can
// ever put an artifact's copy in the middle of being removed.
const (
	PlacementActive        = "ACTIVE"
	PlacementDeletePending = "DELETE_PENDING"
	PlacementGone          = "GONE"
)

// The FR-31 verification classes, exactly as migration 0007's CHECK pins
// them, ordered weakest to strongest.
//
// VerificationNone is not a class, it is the absence of one: nothing has
// been proven about this copy. It is the default, and it is what a
// placement keeps until something actually checks, because the failure
// mode this ladder exists to prevent is a copy that everyone believes is
// verified because a field said so.
//
// This phase records VerificationContent and nothing else, because a local
// read-back is the only check this product performs today (FR-13). The
// ladder proper, and what it takes to raise a medium placement above
// VerificationExistence, is #237's.
const (
	VerificationNone      = ""
	VerificationExistence = "existence"
	VerificationAttested  = "attested"
	VerificationContent   = "content"
)

// The FR-30 move phases, exactly as migration 0007's CHECK pins them.
// Nothing writes placement_moves yet; the move engine (#238) is what these
// are here for, and pinning the vocabulary now is what stops it inventing
// a phase the way FR-10's own vocabulary drifted twice before a test
// pinned it (see TestJournalAcceptsExactlyTheStatesTheMachineDefines).
const (
	MovePlanned             = "PLANNED"
	MoveCopying             = "COPYING"
	MoveCopied              = "COPIED"
	MoveVerifying           = "VERIFYING"
	MoveVerified            = "VERIFIED"
	MoveSourceDeletePending = "SOURCE_DELETE_PENDING"
	MoveDone                = "DONE"
	MoveAbandoned           = "ABANDONED"
)

// stateVerified is the one FR-10 state name this package spells out.
//
// It does not own the vocabulary (internal/lifecycle does, see
// Record.State) and it does not want to, but recording a verification
// class honestly means knowing which transition actually verified
// something: the same HashUpdate arrives both from lifecycle's own
// read-back at VERIFIED and from catalog rebuild copying a hash out of a
// sidecar manifest, and only the first one is evidence that anybody read
// the bytes. TestPlacementClassMatchesTheStateMachinesVerifiedState pins
// this string against the state machine's own so the two cannot drift
// apart silently, which is exactly how QUARANTINED_LOST got lost.
const stateVerified = "VERIFIED"

// Placement is one durable copy of an artifact (FR-29): where a copy of
// this artifact's bytes lives, what is known about it, and how far it has
// actually been verified.
//
// Placement is the answer to "can I read this artifact", which artifacts.
// local_path only ever approximated. In this phase every placement is
// local and mirrors local_path exactly, so asking the placement and
// asking local_path give the same answer for every artifact in every
// deployment; once the move engine exists they stop agreeing, and the
// placement is the one that stays right.
type Placement struct {
	// Medium is MediumLocal or the id of a configured storage medium.
	Medium string

	// Location is an absolute path for a local placement, an object key
	// for a medium one, and empty when the journal has not recorded one
	// yet (an artifact that has landed nowhere). Empty is a real answer,
	// not a missing one.
	Location string

	// SizeBytes is what this product measured when it wrote this copy, and
	// nil when it never measured one. It is never a size a remote
	// reported: that is a claim about the remote object, which is a
	// different object.
	SizeBytes *int64

	// Hash and HashAlg are the locally computed content hash (FR-13), or
	// empty when none was recorded.
	Hash    string
	HashAlg string

	// VerificationClass is what has been PROVEN about this copy, from the
	// constants above, and VerificationNone when nothing has.
	VerificationClass string

	// VerifiedAt is when that class was last achieved, nil when it never
	// was. A class with no VerifiedAt is legal: a catalog-rebuilt row's
	// hash is real evidence with no recoverable moment attached.
	VerifiedAt *time.Time

	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsActive reports whether this placement is the journal's current answer
// for where the artifact lives.
func (p Placement) IsActive() bool { return p.Status == PlacementActive }

// LocalPlacement returns the artifact's local placement, and whether it
// has one. Every artifact has exactly one, guaranteed by migration 0007's
// backfill for every row that predates it and by the journal's own writer
// for every row since, so the false case means a row written by something
// that is not this package.
func (r Record) LocalPlacement() (Placement, bool) {
	for _, p := range r.Placements {
		if p.Medium == MediumLocal {
			return p, true
		}
	}
	return Placement{}, false
}

// LocalLocation is the path an artifact's local copy is at according to
// the placement record, or empty when there is no ACTIVE local placement
// to name one.
//
// This is what code asking "can I read this artifact off local disk"
// should use instead of Record.LocalPath. The two agree for every artifact
// in every deployment today, which is what makes swapping one for the
// other behaviour-neutral (TestLocalLocationAgreesWithLocalPathThroughout
// pins it). They stop agreeing the moment the move engine deletes a local
// copy after migrating it to a medium: LocalPath keeps meaning the
// ingestion landing path, which is a historical fact and stays true, while
// this goes empty, which is the answer a caller about to open a file
// needs.
func (r Record) LocalLocation() string {
	p, ok := r.LocalPlacement()
	if !ok || !p.IsActive() {
		return ""
	}
	return p.Location
}

// execer is satisfied by *sql.Tx, which is the only thing that writes a
// placement: every placement write happens inside the same transaction as
// the artifact write it mirrors, which is what stops a crash leaving an
// artifact row and its placement disagreeing.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const placementColumns = `
	p.artifact_id, p.medium, p.location, p.size_bytes, p.hash, p.hash_alg,
	p.verification_class, p.verified_at, p.status, p.created_at, p.updated_at`

// loadPlacementsFor reads the placements of every artifact matching
// artifactWhere (a predicate over the artifacts table, aliased a), keyed by
// artifact row id.
//
// It is a second query rather than a join onto selectColumns because a
// join would return one row per artifact per placement and every caller
// would then have to de-duplicate the artifact half of it. Two reads that
// each say one thing beat one read that says two.
func loadPlacementsFor(ctx context.Context, q querier, artifactWhere string, args ...any) (map[int64][]Placement, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT`+placementColumns+`
		   FROM placements p
		  WHERE p.artifact_id IN (SELECT a.id FROM artifacts a WHERE `+artifactWhere+`)
		  ORDER BY p.artifact_id, p.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: load placements: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]Placement)
	for rows.Next() {
		var (
			artifactID           int64
			p                    Placement
			size                 sql.NullInt64
			verifiedAt           sql.NullString
			createdAt, updatedAt string
		)
		if err := rows.Scan(&artifactID, &p.Medium, &p.Location, &size, &p.Hash, &p.HashAlg,
			&p.VerificationClass, &verifiedAt, &p.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("state: scan placement: %w", err)
		}
		p.SizeBytes = scanOptionalInt64(size)
		if p.VerifiedAt, err = scanOptionalTime(verifiedAt); err != nil {
			return nil, fmt.Errorf("state: stored placement verified_at is invalid: %w", err)
		}
		if p.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("state: stored placement created_at %q is invalid: %w", createdAt, err)
		}
		if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, fmt.Errorf("state: stored placement updated_at %q is invalid: %w", updatedAt, err)
		}
		out[artifactID] = append(out[artifactID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: load placements: %w", err)
	}
	return out, nil
}

// insertLocalPlacement writes the one local placement a freshly created
// artifact row gets, in the same transaction as the row itself.
//
// This is the other half of migration 0007's backfill, and it is what
// makes "no code path can observe an artifact with zero placements" a
// property of the journal rather than a property of one migration that ran
// once: the backfill covers every artifact that existed when a deployment
// upgraded, and this covers every artifact discovered since.
//
// The location is whatever local path the creating transition carried,
// which for an ordinary Discover is empty, exactly as artifacts.local_path
// is. Nothing is verified at discovery, so the class stays
// VerificationNone.
func insertLocalPlacement(ctx context.Context, tx execer, artifactRowID int64, localPath, occurred string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO placements (artifact_id, medium, location, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		artifactRowID, MediumLocal, localPath, PlacementActive, occurred, occurred,
	); err != nil {
		return fmt.Errorf("state: insert local placement: %w", err)
	}
	return nil
}

// updateLocalPlacement mirrors onto the local placement whatever t changed
// about where the artifact's local copy is and what is known about it, and
// does nothing at all when t changed none of those.
//
// The mirroring, not a second source of truth, is the point: for as long
// as every placement is local, a placement says exactly what local_path,
// transfer_bytes, local_hash and local_hash_alg already said. That is what
// lets the "is it readable locally" call sites move onto placements
// without any of them changing behaviour.
func updateLocalPlacement(ctx context.Context, tx execer, artifactRowID int64, t Transition) error {
	set := []string{}
	args := []any{}

	if t.LocalPath != nil {
		set = append(set, "location = ?")
		args = append(args, *t.LocalPath)
	}
	if t.Transfer != nil {
		set = append(set, "size_bytes = ?")
		args = append(args, t.Transfer.BytesTransferred)
	}
	if t.Hashes != nil {
		set = append(set, "hash = ?", "hash_alg = ?")
		args = append(args, t.Hashes.Hash, t.Hashes.Alg)

		// A class is only recorded for the transition that actually
		// verified something. The same HashUpdate also arrives from
		// catalog rebuild, which copies a hash out of a sidecar manifest
		// without reading a single byte of the artifact, and calling that
		// content-verified would be the exact dishonesty FR-31 exists to
		// rule out.
		if t.To == stateVerified && t.From != stateVerified && t.Hashes.Hash != "" {
			set = append(set, "verification_class = ?", "verified_at = ?")
			args = append(args, VerificationContent, formatTime(t.OccurredAt))
		}
	}

	if len(set) == 0 {
		return nil
	}

	set = append(set, "updated_at = ?")
	args = append(args, formatTime(t.OccurredAt), artifactRowID, MediumLocal)

	if _, err := tx.ExecContext(ctx,
		"UPDATE placements SET "+join(set, ", ")+" WHERE artifact_id = ? AND medium = ?", args...,
	); err != nil {
		return fmt.Errorf("state: update local placement: %w", err)
	}
	return nil
}

package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// This file is the durable half of FR-30's move journal: one row in
// placement_moves per attempted migration of one artifact from one
// placement to one medium, written to BEFORE every side effect.
//
// 0007_placements.sql created the table empty and said outright that
// nothing writes it until the move engine exists. This is that first
// writer's storage layer, and the engine itself (internal/placement) holds
// the semantics, exactly the way internal/lifecycle holds the meaning of
// artifacts.state while this package only stores the string.
//
// # Why the phase write and the placement writes share one transaction
//
// The whole safety argument of a three-phase move is that the journal
// never disagrees with the world in the dangerous direction. Two of the
// phase writes carry a placement change with them: the write that records
// VERIFIED is also the write that creates the destination's placement, and
// the write that records DONE is also the write that marks the source
// GONE. If those could come apart, a crash between them would leave either
// a phase claiming a verified destination with no placement to prove it,
// or a source marked gone while the phase still says the delete had not
// been decided. AdvanceMove therefore takes the placement updates with the
// phase and applies them in the one transaction, the same way
// RecordTransition already takes a Placement alongside a lifecycle
// transition.
//
// # Why the caller states the phase it expects to be leaving
//
// MoveAdvance.From is compared in the UPDATE's own WHERE clause, so a
// write against a row that has moved on affects no rows and is reported as
// a conflict rather than silently overwriting. internal/placement's
// transition table is the readable statement of which phase changes are
// legal; this is the wall underneath it that a concurrent second process,
// or a resumed one racing a live one, cannot walk through.

// The phases of a move (FR-30). The strings are exactly the set
// 0007_placements.sql's CHECK constrains, and internal/placement pins its
// own Phase vocabulary against this list so the two cannot drift.
//
// What each phase MEANS, and which changes between them are legal, belongs
// to internal/placement. This package only stores the answer.
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

// ErrMovePhaseConflict is what AdvanceMove returns when the row is not in
// the phase the caller said it was leaving.
//
// It is a named error rather than a formatted string because the engine
// has to tell it apart from a storage failure: a conflict means another
// worker (or a resumed process racing a live one) already moved this row
// on, which is a reason to re-read and stand down, while a storage failure
// is a reason to stop and report. A caller that confuses the two either
// hammers a row it does not own or gives up on a database that is fine.
var ErrMovePhaseConflict = errors.New("state: the move is not in the phase this write expected to be leaving")

// ErrMoveNotFound is returned for a move id no row carries.
var ErrMoveNotFound = errors.New("state: no such move")

// Move is one row of placement_moves, joined with the placement it names
// as its source.
//
// SourceMedium and SourceLocation come from that join rather than from
// columns of their own, and that is the point of the foreign key: a move
// preserves the copy it was planned against, not whatever copy happens to
// be on that medium when the delete finally runs. A source placement that
// has been replaced under the move is therefore visible as a changed
// location, which the engine re-checks before it deletes anything.
type Move struct {
	ID       int64
	Artifact model.ArtifactID

	// SourcePlacementID is the placements row this move is copying FROM,
	// or nil for a row whose source placement has since been deleted.
	SourcePlacementID *int64

	// SourceMedium and SourceLocation describe that placement as it stands
	// now. Both are empty when SourcePlacementID is nil.
	SourceMedium   string
	SourceLocation string

	DestinationMedium string
	DestinationKey    string

	Phase string

	// BytesCopied is what the destination reported after the copy, or nil
	// before one has happened. A pointer for Placement.Size's reason: a
	// zero-byte artifact is a real thing, so a zero must not double as
	// "nothing was copied".
	BytesCopied *int64

	// Error is the last thing that went wrong on this move, or empty. It
	// is carried on the row rather than only logged because the phase
	// alone cannot explain an ABANDONED move to an operator.
	Error string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Terminal reports whether this move has finished, one way or the other.
func (m Move) Terminal() bool { return m.Phase == MoveDone || m.Phase == MoveAbandoned }

// MovePlan is the durable intent that opens a move: this artifact's copy
// on this medium is to be copied to that medium, at that key.
//
// It is written before anything is uploaded, verified or deleted, which is
// what makes a crash before the first side effect reconcilable rather than
// invisible.
type MovePlan struct {
	Artifact model.ArtifactID

	// SourceMedium names the ACTIVE placement being moved FROM. PlanMove
	// resolves it to a placements row id and refuses if there is no ACTIVE
	// placement on that medium: a move whose source cannot be named is a
	// move that cannot promise to preserve it.
	SourceMedium string

	DestinationMedium string
	DestinationKey    string

	OccurredAt time.Time
}

// MoveAdvance is one durable phase write, plus whatever placement facts
// become true at the same instant.
type MoveAdvance struct {
	MoveID int64

	// From is the phase the caller believes the row is in. It is checked
	// in the UPDATE's WHERE clause; a mismatch is ErrMovePhaseConflict and
	// nothing is written.
	From string

	// To is the phase being entered. From == To is legal and is how a
	// caller records a placement fact or an error without changing phase.
	To string

	OccurredAt time.Time

	// BytesCopied, when non-nil, overwrites the recorded byte count.
	BytesCopied *int64

	// Error overwrites the recorded error text, including with "" to
	// clear it. It is written on every advance so a move that recovers
	// does not carry the sentence that explains a failure it survived.
	Error string

	// Placements are applied inside this write's own transaction. See the
	// file comment for why they cannot be a separate call.
	Placements []PlacementUpdate
}

// Update returns the PlacementUpdate that would rewrite this placement as
// it currently stands, so a caller changing one field (almost always
// Status) does not have to restate the rest and cannot silently drop a
// hash or a verification class by forgetting one.
func (p Placement) Update() PlacementUpdate {
	return PlacementUpdate{
		Medium:            p.Medium,
		Location:          p.Location,
		Size:              p.Size,
		Hash:              p.Hash,
		HashAlg:           p.HashAlg,
		VerificationClass: p.VerificationClass,
		VerifiedAt:        p.VerifiedAt,
		Status:            p.Status,
	}
}

// WithStatus returns a copy of u carrying status.
func (u PlacementUpdate) WithStatus(status string) PlacementUpdate {
	u.Status = status
	return u
}

// moveColumns and moveFrom are spelled once and shared by every read in
// this file, because scanMove decodes them by position. A SELECT that
// listed its own columns would only have to disagree with that Scan by one
// to hand a destination key back as a phase, silently, on one code path.
//
// The LEFT JOIN is what makes SourcePlacementID's nil case reachable: a
// move whose source placement row has been deleted still has a history
// worth reading, so it comes back with empty source fields rather than
// disappearing from the listing.
const moveColumns = `
	m.id, a.source, a.backup_set, a.artifact_name,
	m.source_placement_id, p.medium, p.location,
	m.destination_medium, m.destination_key,
	m.phase, m.bytes_copied, m.error, m.created_at, m.updated_at`

const moveFrom = `
	FROM placement_moves m
	JOIN artifacts a ON a.id = m.artifact_id
	LEFT JOIN placements p ON p.id = m.source_placement_id`

// PlanMove writes the PLANNED row for one move and returns it.
//
// It refuses rather than guesses in three places, and each refusal is the
// conservative direction: an artifact it cannot find, a source medium with
// no ACTIVE placement, and a second non-terminal move for the same
// artifact. The last one matters most: two live moves for one artifact are
// two independent opinions about which copy is disposable, and the way
// that ends is both of them deleting the copy the other was relying on.
func (j *Journal) PlanMove(ctx context.Context, p MovePlan) (Move, error) {
	switch {
	case p.Artifact.Name == "" || p.Artifact.Set.IsZero():
		return Move{}, fmt.Errorf("state: planning a move requires a valid artifact id")
	case p.SourceMedium == "":
		return Move{}, fmt.Errorf("state: planning a move for %s requires the source medium", p.Artifact)
	case p.DestinationMedium == "":
		return Move{}, fmt.Errorf("state: planning a move for %s requires a destination medium", p.Artifact)
	case p.DestinationKey == "":
		return Move{}, fmt.Errorf("state: planning a move for %s requires a destination key", p.Artifact)
	case p.DestinationMedium == p.SourceMedium:
		return Move{}, fmt.Errorf("state: planning a move for %s from %q to itself is not a move", p.Artifact, p.SourceMedium)
	case p.OccurredAt.IsZero():
		return Move{}, fmt.Errorf("state: planning a move for %s requires OccurredAt", p.Artifact)
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return Move{}, fmt.Errorf("state: begin plan move: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	var artifactRowID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM artifacts WHERE source = ? AND backup_set = ? AND artifact_name = ?`,
		p.Artifact.Set.Source, p.Artifact.Set.Set, p.Artifact.Name,
	).Scan(&artifactRowID)
	if errors.Is(err, sql.ErrNoRows) {
		return Move{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, p.Artifact)
	}
	if err != nil {
		return Move{}, fmt.Errorf("state: plan move: %w", err)
	}

	var sourcePlacementID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM placements WHERE artifact_id = ? AND medium = ? AND status = ?`,
		artifactRowID, p.SourceMedium, PlacementActive,
	).Scan(&sourcePlacementID)
	if errors.Is(err, sql.ErrNoRows) {
		return Move{}, fmt.Errorf(
			"state: refusing to plan a move of %s off %q: no ACTIVE placement records a copy there, and a move that cannot name the copy it preserves must not start",
			p.Artifact, p.SourceMedium)
	}
	if err != nil {
		return Move{}, fmt.Errorf("state: plan move: %w", err)
	}

	var live int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM placement_moves WHERE artifact_id = ? AND phase NOT IN (?, ?)`,
		artifactRowID, MoveDone, MoveAbandoned,
	).Scan(&live); err != nil {
		return Move{}, fmt.Errorf("state: plan move: %w", err)
	}
	if live > 0 {
		return Move{}, fmt.Errorf(
			"state: refusing a second live move for %s: %d is already in flight, and two moves of one artifact are two opinions about which copy is disposable",
			p.Artifact, live)
	}

	stamp := formatTime(p.OccurredAt)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO placement_moves (
			artifact_id, source_placement_id, destination_medium, destination_key,
			phase, bytes_copied, error, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, NULL, '', ?, ?)`,
		artifactRowID, sourcePlacementID, p.DestinationMedium, p.DestinationKey,
		MovePlanned, stamp, stamp,
	)
	if err != nil {
		return Move{}, fmt.Errorf("state: plan move for %s: %w", p.Artifact, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Move{}, fmt.Errorf("state: plan move for %s: %w", p.Artifact, err)
	}

	mv, err := getMove(ctx, tx, id)
	if err != nil {
		return Move{}, err
	}
	if err := tx.Commit(); err != nil {
		return Move{}, fmt.Errorf("state: commit plan move: %w", err)
	}
	return mv, nil
}

// AdvanceMove records one phase write, with any placement facts that
// become true at the same instant, in one transaction.
func (j *Journal) AdvanceMove(ctx context.Context, a MoveAdvance) (Move, error) {
	switch {
	case a.MoveID == 0:
		return Move{}, fmt.Errorf("state: advancing a move requires its id")
	case a.From == "":
		return Move{}, fmt.Errorf("state: advancing move %d requires the phase it is leaving", a.MoveID)
	case a.To == "":
		return Move{}, fmt.Errorf("state: advancing move %d requires the phase it is entering", a.MoveID)
	case a.OccurredAt.IsZero():
		return Move{}, fmt.Errorf("state: advancing move %d requires OccurredAt", a.MoveID)
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return Move{}, fmt.Errorf("state: begin advance move: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	var artifactRowID int64
	var phase string
	err = tx.QueryRowContext(ctx,
		`SELECT artifact_id, phase FROM placement_moves WHERE id = ?`, a.MoveID,
	).Scan(&artifactRowID, &phase)
	if errors.Is(err, sql.ErrNoRows) {
		return Move{}, fmt.Errorf("%w: %d", ErrMoveNotFound, a.MoveID)
	}
	if err != nil {
		return Move{}, fmt.Errorf("state: advance move: %w", err)
	}

	redact := j.redact.Load()
	stamp := formatTime(a.OccurredAt)

	// The phase is compared here as well as read above, deliberately: the
	// read is what produces a legible error, and the WHERE clause is what
	// actually makes the write conditional, so a row that changed between
	// the two is refused by the database rather than by a stale variable.
	res, err := tx.ExecContext(ctx, `
		UPDATE placement_moves
		   SET phase = ?,
		       bytes_copied = COALESCE(?, bytes_copied),
		       error = ?,
		       updated_at = ?
		 WHERE id = ? AND phase = ?`,
		a.To, optionalInt64(a.BytesCopied), redact.Filter(a.Error), stamp, a.MoveID, a.From,
	)
	if err != nil {
		return Move{}, fmt.Errorf("state: advance move %d to %s: %w", a.MoveID, a.To, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Move{}, fmt.Errorf("state: advance move %d to %s: %w", a.MoveID, a.To, err)
	}
	if affected == 0 {
		return Move{}, fmt.Errorf("%w: move %d is at %q, not the %q this write was leaving",
			ErrMovePhaseConflict, a.MoveID, phase, a.From)
	}

	for _, p := range a.Placements {
		if err := upsertPlacement(ctx, tx, artifactRowID, p, a.OccurredAt); err != nil {
			return Move{}, err
		}
	}

	mv, err := getMove(ctx, tx, a.MoveID)
	if err != nil {
		return Move{}, err
	}
	if err := tx.Commit(); err != nil {
		return Move{}, fmt.Errorf("state: commit advance move: %w", err)
	}
	return mv, nil
}

// GetMove reads one move by id.
func (j *Journal) GetMove(ctx context.Context, id int64) (Move, error) {
	return getMove(ctx, j.db, id)
}

// getMove is GetMove's body, taking a querier so PlanMove and AdvanceMove
// can read back the row they just wrote from inside their own open
// transaction. Reading it any other way would either miss the write or see
// somebody else's.
func getMove(ctx context.Context, q querier, id int64) (Move, error) {
	row := q.QueryRowContext(ctx, `SELECT `+moveColumns+moveFrom+` WHERE m.id = ?`, id)
	mv, err := scanMove(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Move{}, fmt.Errorf("%w: %d", ErrMoveNotFound, id)
	}
	return mv, err
}

// ListMoves returns every move in one of the given phases, oldest first.
// With no phases it returns every move.
func (j *Journal) ListMoves(ctx context.Context, phases ...string) ([]Move, error) {
	query := `SELECT ` + moveColumns + moveFrom
	args := make([]any, 0, len(phases))
	if len(phases) > 0 {
		holders := make([]string, len(phases))
		for i, p := range phases {
			holders[i] = "?"
			args = append(args, p)
		}
		query += ` WHERE m.phase IN (` + strings.Join(holders, ", ") + `)`
	}
	query += ` ORDER BY m.id`

	rows, err := j.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list moves: %w", err)
	}
	defer rows.Close()

	var out []Move
	for rows.Next() {
		mv, err := scanMove(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, mv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list moves: %w", err)
	}
	return out, nil
}

// MovesForArtifact returns every move ever recorded for one artifact,
// oldest first.
func (j *Journal) MovesForArtifact(ctx context.Context, artifact model.ArtifactID) ([]Move, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT `+moveColumns+moveFrom+
			` WHERE a.source = ? AND a.backup_set = ? AND a.artifact_name = ? ORDER BY m.id`,
		artifact.Set.Source, artifact.Set.Set, artifact.Name)
	if err != nil {
		return nil, fmt.Errorf("state: list moves for %s: %w", artifact, err)
	}
	defer rows.Close()

	var out []Move
	for rows.Next() {
		mv, err := scanMove(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, mv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list moves for %s: %w", artifact, err)
	}
	return out, nil
}

// scanMove decodes one row of moveColumns.
//
// A timestamp that will not parse is a hard failure rather than a zero
// time, and that is the same choice every scanner in this package makes:
// the move engine compares these against each other to decide what to do
// next, and a silent zero would read as the oldest move there has ever
// been.
func scanMove(row scanRow) (Move, error) {
	var (
		id                                   int64
		source, backupSet, artifactName      string
		sourcePlacementID                    sql.NullInt64
		sourceMedium, sourceLocation         sql.NullString
		destinationMedium, destinationKey    string
		phase, errText, createdAt, updatedAt string
		bytesCopied                          sql.NullInt64
	)
	if err := row.Scan(&id, &source, &backupSet, &artifactName,
		&sourcePlacementID, &sourceMedium, &sourceLocation,
		&destinationMedium, &destinationKey,
		&phase, &bytesCopied, &errText, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Move{}, err
		}
		return Move{}, fmt.Errorf("state: scan move: %w", err)
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return Move{}, fmt.Errorf("state: stored move created_at %q is invalid: %w", createdAt, err)
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return Move{}, fmt.Errorf("state: stored move updated_at %q is invalid: %w", updatedAt, err)
	}
	return Move{
		ID:                id,
		Artifact:          model.ArtifactID{Set: model.BackupSetID{Source: source, Set: backupSet}, Name: artifactName},
		SourcePlacementID: scanOptionalInt64(sourcePlacementID),
		SourceMedium:      sourceMedium.String,
		SourceLocation:    sourceLocation.String,
		DestinationMedium: destinationMedium,
		DestinationKey:    destinationKey,
		Phase:             phase,
		BytesCopied:       scanOptionalInt64(bytesCopied),
		Error:             errText,
		CreatedAt:         created,
		UpdatedAt:         updated,
	}, nil
}

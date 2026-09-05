// One aggregate query: how many bytes of local disk this manager is
// occupying, as the journal knows it.
//
// It gets a file to itself for the same reason the query is an aggregate
// rather than a listing. The FR-21 capacity guard asks this before every
// transfer, so the answer has to cost the same on a tree of ten artifacts
// and a tree of ten thousand, and it has to count only what this manager
// put there rather than whatever else shares the mount.
//
// Everything else worth knowing about the measurement lives on
// LocalBytesInUse: which of two recorded sizes it prefers, where it
// deliberately over-counts, why over-counting is the only safe direction
// for a number a cap is enforced from, and why the list of states holding
// a local copy arrives as an argument instead of being restated here.
package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LocalBytesInUse sums the recorded size of every artifact currently in one
// of states: how much space this manager is occupying on the local
// filesystem, as the catalog knows it.
//
// # Why the catalog rather than the filesystem
//
// The FR-21 capacity guard needs this number to enforce an operator's
// storage cap (issue #286), and there are only two places to get it. A `du`
// over the backup root is O(tree) on every check, and it counts every file
// anything else put on that mount, which is the opposite of the question:
// the cap is about what THIS manager uses, not about what the volume holds.
// This table already records what was transferred and how big each one was,
// so the answer is one aggregate query with no rows crossing into Go, it
// costs the same on a tree of ten artifacts and a tree of ten thousand, and
// it counts only files this manager put there.
//
// # Which size, and why that order
//
// transfer_bytes first, remote_size second, zero last. transfer_bytes is
// what the copy actually wrote and is therefore what is on the disk;
// remote_size is what a listing claimed, which is the only figure available
// while a .partial is still being written and is the size that .partial is
// heading for. An artifact whose backend reported no size at all counts as
// zero rather than failing the whole measurement, because RemoteIdentity's
// fields are optional by contract and one sizeless backend must not make
// the cap unenforceable everywhere.
//
// # Where this over-counts, and why that direction is the safe one
//
// internal/retention's PruneApply removes a local file without writing
// anything back to this journal, so a pruned artifact's row stays in
// COMPLETE and keeps contributing its bytes here. This measurement is
// therefore an upper bound on real usage, not an exact figure. Every other
// approximation in it points the same way (a half-written .partial counts
// at its full eventual size). That matters because of what the number is
// used for: over-stating usage can only shrink the headroom a cap reports,
// which refuses transfers that would have fitted. Under-stating it would
// admit transfers that breach the ceiling, which is the failure this whole
// mechanism exists to prevent. See core/service's manager storage reading,
// which reports the catalog figure alongside the filesystem's own so the
// two can be seen to disagree rather than one quietly standing in for the
// other.
//
// # Why the state vocabulary is an argument
//
// internal/lifecycle owns which states hold a local copy
// (lifecycle.StatesHoldingLocalCopy, and the test there that refuses to let
// a new state go unclassified). That package imports this one, so the list
// travels in as an argument rather than being restated here, where a second
// copy would drift silently and under-count.
//
// An empty list is refused rather than answered. It matches no rows, so it
// would report a confident zero, and a zero here is read as "this manager
// is using nothing", which hands a caller the entire cap as free headroom.
func (j *Journal) LocalBytesInUse(ctx context.Context, states []string) (uint64, error) {
	if len(states) == 0 {
		return 0, fmt.Errorf("state: local bytes in use: at least one artifact state must be named; an empty list matches nothing and would report a confident zero")
	}

	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, s := range states {
		placeholders[i] = "?"
		args[i] = s
	}

	// COALESCE twice, deliberately: the inner one picks transfer_bytes over
	// remote_size per row, the outer one turns SUM's NULL over zero matching
	// rows into a plain 0.
	query := `SELECT COALESCE(SUM(COALESCE(transfer_bytes, remote_size, 0)), 0) FROM artifacts WHERE state IN (` +
		strings.Join(placeholders, ", ") + `)`

	var total sql.NullInt64
	if err := j.db.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("state: local bytes in use: %w", err)
	}
	if !total.Valid || total.Int64 < 0 {
		// Neither is reachable through this package's own writes (the
		// column is only ever written from a non-negative byte count), so
		// this is about a hand-edited or corrupted row rather than an
		// ordinary case. Clamping to zero would understate usage, which is
		// the one direction that lets a cap be breached, so it is a loud
		// failure instead.
		return 0, fmt.Errorf("state: local bytes in use: the artifacts table summed to a negative or null byte count (%v), which no write in this package can produce", total)
	}
	return uint64(total.Int64), nil
}

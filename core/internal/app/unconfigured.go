// This file is issue #418: what happens to a backup set's artifacts after
// its configuration is removed (#391).
//
// # The category that appeared, and had no rules
//
// Removing a backup set takes it out of config.yaml and leaves every
// journal row and every file it produced exactly where they are. That is
// the contract the confirmation dialog promises and it is the right one.
// What nobody had decided was what happens NEXT, and the answer that fell
// out by default was "nothing at all": retention walks the configuration,
// so it never sees them; reconcile walks the configuration, so it never
// sees them; health, capacity forecasting, discovery, the move engine and
// the catalog rebuild all walk the configuration too. One read
// (ListArtifacts) was widened to show them, so they are visible,
// unreachable by every maintenance path, and pinned on the disk until
// somebody deletes them by hand. On a NAS with a ceiling that is a real
// problem arriving slowly, which is the worst way for it to arrive.
//
// So a backup set is now two things: one the configuration has, and one
// the journal remembers. This file gives the second one a name, a
// lifecycle, and a report an operator can actually read.
//
// # The lifecycle, decided rather than defaulted
//
// An unconfigured set's artifacts are RETAINED AND UNGOVERNED. Concretely:
//
//   - Nothing collects for them. There is no remote to reach, no
//     credentials to reach it with and no schedule that visits them.
//   - Nothing deletes them. Not this file, not retention, not the removal
//     itself. There is no policy behind them to authorise a deletion, and
//     a manager that destroys backups on the strength of a policy that no
//     longer exists is exactly the manager this product refuses to be.
//   - Nothing repairs them either, and that is the honest half. FR-17
//     reconciliation would need this set's transport source to check a
//     remote object, and the operator removed the declaration that said
//     where that remote is and how to authenticate to it. Reaching a host
//     whose configuration was deleted is not a repair, it is a
//     connection nobody authorised.
//   - They stay listed under Backups, they stay countable, and they stay
//     re-adoptable: creating the same source and name again hands every
//     one of them back, under that set's policy, from the next cycle on
//     (backupsetremove.go's "Re-creating a set with the same source and
//     name", and #411 for the acknowledgement that create path owes).
//
// That is a defined lifecycle rather than an absent one, and the whole
// point of writing it down here is that the next person to add a
// config-walking read has somewhere to look up what this category is
// supposed to do.
//
// # Why this reports and does not delete
//
// Deleting a removed set's backups automatically would be this product
// destroying data because a configuration file changed. It is also
// unnecessary: the operator who wants those bytes gone can delete them,
// and the operator who wants them aged out can create the set again and
// let its own retention chain do it under FR-20's identity checks, which
// is the only route in this codebase that is allowed to remove a backup
// at all. So UnconfiguredSets reports, and the surfaces that list these
// backups say which policy governs them, which is none.
//
// # The one thing it does clear, and why that is not a backup
//
// A cycle stopped mid-flight by a removal can leave an artifact in an
// acquisition state (DISCOVERED through COMMITTING) pointing at a
// .partial file. Those rows are the residue #410 flagged and could not
// resolve: no promise covers them, no cycle will ever advance them, and
// they sit on the Backups list forever in a state that claims a transfer
// is in progress when nothing is running.
//
// Clearing one destroys nothing, and that is provable rather than
// asserted. FR-12 gives an in-progress copy a .partial name specifically
// so that nothing can mistake it for a restore point, and transfer.go
// already deletes whatever is at that path before every attempt without
// asking anyone. The remote object those bytes came from has never been
// touched: FR-15's delete is reachable only from COMMITTED, which is past
// every state this sweep will act on. So the artifact still exists where
// it always did, and the only thing removed is an incomplete copy this
// manager itself declared disposable.
//
// FAILED is where the row ends, and it is not an arbitrary choice.
// internal/lifecycle/localfootprint.go defines FAILED as a state holding
// NO local bytes, precisely because transfer.go clears the .partial on
// its way there. Ending a row at FAILED while leaving its .partial on
// disk would make FR-21's capacity arithmetic stop counting bytes that
// are still there, and that file is explicit that a bias must never point
// that way. The file removal and the transition are therefore one
// operation, in that order; see clearOne.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// ErrBackupSetConfigured refuses an operation that only makes sense for a
// backup set the configuration no longer names.
//
// It is a distinct error rather than a *NotFoundError because the two mean
// opposite things: NotFoundError says the id names nothing, this says the
// id names something that is very much still running. An operator who
// reads "no such backup set" for a set sitting on their own dashboard
// learns nothing they can act on.
var ErrBackupSetConfigured = errors.New("app: this backup set is configured; its in-flight artifacts belong to the processing cycle")

// UnconfiguredSet is one backup set the journal remembers and the
// configuration no longer names, plus enough arithmetic for an operator to
// decide whether they care.
//
// The counts are split the way the decisions are. Retained is what the
// removal confirmation promised would stay, and it is the number that
// costs disk. Stranded is residue no promise covers and the only thing
// ClearStranded will act on. Quarantined is neither: it is being held for
// a human, and the three quarantine actions all refuse a row whose set is
// unconfigured (quarantineactions.go), so it is a number that exists to
// explain why those rows have no buttons.
type UnconfiguredSet struct {
	Set model.BackupSetID

	// Artifacts is every journal row on record for this set, whatever
	// state it is in.
	Artifacts int

	// Retained is how many of them hold a durable local copy this manager
	// still vouches for: COMMITTED, REMOTE_DELETE_PENDING, REMOTE_RETAINED
	// or COMPLETE. These are the backups.
	Retained int

	// Stranded is how many are stuck in an acquisition state (DISCOVERED
	// through COMMITTING) that nothing will ever advance, because the
	// pipeline walks configured sets.
	Stranded int

	// Quarantined is how many are in QUARANTINED or QUARANTINED_LOST.
	Quarantined int

	// Bytes is the recorded size of every row that holds local bytes
	// (internal/lifecycle.HoldsLocalCopy), which is the answer to "how
	// much disk is this costing me and nothing is managing".
	//
	// It is the size the remote reported at discovery, so a .partial
	// part-way through a copy is counted at its eventual full size. That
	// over-states slightly and in the safe direction, matching what FR-21's
	// own cap arithmetic already does with the same number.
	Bytes int64

	// FirstDiscovered and LastActivity bracket this set's history, so a
	// report can say how old the pinned backups are without a second read.
	FirstDiscovered time.Time
	LastActivity    time.Time
}

// StrandedArtifact is one acquisition-state row of an unconfigured backup
// set: work a cycle started and no cycle will ever finish.
type StrandedArtifact struct {
	Artifact model.ArtifactID
	State    lifecycle.State

	// PartialPath is the .partial file this row points at. It is empty for
	// a DISCOVERED row, which is noticed-but-not-started and has no local
	// bytes at all.
	PartialPath string

	// PartialBytes is what that file measures right now, or 0 when there
	// is nothing at the path. It is measured rather than taken from the
	// journal, because the whole question here is what is actually sitting
	// on the disk.
	PartialBytes int64

	// Cleared reports that this call removed the residue and ended the
	// row. It is always false from StrandedArtifacts, which writes
	// nothing.
	Cleared bool

	// Err is why this row was left exactly where it was. A sweep isolates
	// per-artifact problems here rather than aborting, so one row it
	// refuses never hides the rest of the set's answer.
	Err error
}

// reportUngoverned writes what this deployment is holding outside every
// configured backup set into the FR-23 event stream, once per cycle,
// and only when there is something to say.
//
// It is here for the reason `run` cannot cover on its own: a daemon has
// no exit status and nobody typing commands at it, so a NAS quietly
// filling up with backups no policy governs would be visible only to
// somebody who thought to go and look. This is the same argument
// reportBarrenSets makes for issue #361's verdict (cycleoutcome.go), and
// the line is deliberately one event for the whole deployment rather than
// one per set: what an operator acts on is "how much am I holding that
// nothing manages", and a per-set fan-out of that on every poll interval
// is how a line stops being read.
//
// Info rather than an error, because nothing has failed. An operator
// removed a backup set and this manager kept its promise to leave the
// backups alone; saying that in the vocabulary of a fault would make
// every removal look like a problem.
//
// It cannot fail a cycle. The configuration is not involved, no work is
// started, and a journal that will not answer this question has already
// failed the cycle somewhere it matters more.
func (s *Service) reportUngoverned(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	sets, err := s.UnconfiguredSets(ctx)
	if err != nil || len(sets) == 0 {
		return
	}
	artifacts, stranded := 0, 0
	var bytes int64
	for _, u := range sets {
		artifacts += u.Artifacts
		stranded += u.Stranded
		bytes += u.Bytes
	}
	s.logger().Event(ctx, obs.LevelInfo, "artifacts_ungoverned",
		"this deployment is holding backups for backup sets its configuration no longer names; no retention policy applies to them and nothing will delete them",
		slog.Int("backup_sets", len(sets)),
		slog.Int("artifacts", artifacts),
		slog.Int("stranded_artifacts", stranded),
		slog.Int64("bytes", bytes),
	)
}

// IsAcquisitionState reports whether an FR-10 state string is one an
// artifact is still trying to get OUT of on its way to being a durable
// backup: DISCOVERED through COMMITTING.
//
// It is exported for one caller, core/service's removal event, which has
// to say how many rows it just stranded and must not keep its own list of
// state names to do it. A second copy of that list is a copy that drifts
// the first time a state is added, and the drift is silent.
func IsAcquisitionState(st string) bool { return acquiring(lifecycle.State(st)) }

// UnconfiguredSets is every backup set the journal holds history for and
// the configuration does not name, in id order.
//
// Id order rather than config order, deliberately: there is no
// configuration to take an order from, and a report whose rows moved
// between runs for no reason an operator could see would be worse than
// one that is merely alphabetical.
func (s *Service) UnconfiguredSets(ctx context.Context) ([]UnconfiguredSet, error) {
	known, err := s.Journal.ListBackupSetIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: unconfigured sets: listing the backup sets on record: %w", err)
	}

	var out []UnconfiguredSet
	for _, id := range known {
		if _, _, configured := s.backupSetConfigFor(id); configured {
			continue
		}
		records, err := s.Journal.ListByBackupSet(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("app: unconfigured sets: listing %s: %w", id, err)
		}
		out = append(out, summariseUnconfigured(id, records))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Set.String() < out[j].Set.String() })
	return out, nil
}

// summariseUnconfigured is the arithmetic of one report row, kept apart
// from the read so the classification can be read in one screen and so the
// three counts cannot drift from the vocabulary they borrow (durable,
// acquiring and lifecycle's own quarantine predicate).
func summariseUnconfigured(id model.BackupSetID, records []state.Record) UnconfiguredSet {
	u := UnconfiguredSet{Set: id, Artifacts: len(records)}
	for _, rec := range records {
		st := lifecycle.State(rec.State)
		switch {
		case durable(st):
			u.Retained++
		case acquiring(st):
			u.Stranded++
		case lifecycle.IsQuarantineState(st):
			u.Quarantined++
		}
		if lifecycle.HoldsLocalCopy(st) && rec.Remote.Size != nil {
			u.Bytes += *rec.Remote.Size
		}
		if u.FirstDiscovered.IsZero() || rec.DiscoveredAt.Before(u.FirstDiscovered) {
			u.FirstDiscovered = rec.DiscoveredAt
		}
		if rec.UpdatedAt.After(u.LastActivity) {
			u.LastActivity = rec.UpdatedAt
		}
	}
	return u
}

// StrandedArtifacts previews what ClearStranded would do to set, and
// writes nothing at all.
//
// It exists as its own method rather than as a flag on the sweep because
// this product previews before it changes anything, and a preview that
// shared a code path with the real thing is one edit away from stopping
// being a preview.
func (s *Service) StrandedArtifacts(ctx context.Context, set model.BackupSetID) ([]StrandedArtifact, error) {
	records, err := s.strandedRecords(ctx, set)
	if err != nil {
		return nil, err
	}
	out := make([]StrandedArtifact, 0, len(records))
	for _, rec := range records {
		out = append(out, describeStranded(rec))
	}
	return out, nil
}

// ClearStranded removes the .partial residue of every stranded row in set
// and ends each row at FAILED, and reports what it did to each one.
//
// It refuses a set the configuration still names (ErrBackupSetConfigured):
// a configured set's acquisition rows are the processing cycle's work in
// progress, and sweeping those would be this call racing a transfer and
// deleting the file it is writing. It refuses an id that is neither
// configured nor on record (*NotFoundError), following issue #187's rule,
// which matters more here than anywhere: on an operation that removes
// files, a mistyped name reporting "nothing to clear" is a success message
// for something that never happened.
//
// A per-artifact problem lands in that artifact's own Err and the sweep
// carries on, so one row it will not touch never hides the rest.
func (s *Service) ClearStranded(ctx context.Context, set model.BackupSetID) ([]StrandedArtifact, error) {
	records, err := s.strandedRecords(ctx, set)
	if err != nil {
		return nil, err
	}
	out := make([]StrandedArtifact, 0, len(records))
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		found := describeStranded(rec)
		if err := s.clearOne(ctx, rec); err != nil {
			found.Err = err
		} else {
			found.Cleared = true
		}
		out = append(out, found)
	}
	return out, nil
}

// strandedRecords is the shared front half of both calls: refuse what
// neither of them may act on, then return this set's acquisition rows in
// journal order.
func (s *Service) strandedRecords(ctx context.Context, set model.BackupSetID) ([]state.Record, error) {
	if set.IsZero() {
		return nil, &NotFoundError{Kind: "backup set", Name: set.String()}
	}
	if _, _, configured := s.backupSetConfigFor(set); configured {
		return nil, fmt.Errorf("%w: %s", ErrBackupSetConfigured, set)
	}

	records, err := s.Journal.ListByBackupSet(ctx, set)
	if err != nil {
		return nil, fmt.Errorf("app: stranded artifacts: listing %s: %w", set, err)
	}
	// No configuration and no history is an id this deployment has never
	// heard of, which is a typo rather than a filter. See this method's
	// callers' docs.
	if len(records) == 0 {
		return nil, &NotFoundError{Kind: "backup set", Name: set.String()}
	}

	var out []state.Record
	for _, rec := range records {
		if acquiring(lifecycle.State(rec.State)) {
			out = append(out, rec)
		}
	}
	return out, nil
}

// describeStranded reads what is actually on the disk for one row.
func describeStranded(rec state.Record) StrandedArtifact {
	found := StrandedArtifact{
		Artifact:    rec.Artifact,
		State:       lifecycle.State(rec.State),
		PartialPath: rec.LocalPath,
	}
	if found.PartialPath == "" {
		return found
	}
	if info, err := os.Stat(found.PartialPath); err == nil {
		found.PartialBytes = info.Size()
	}
	return found
}

// clearOne removes one stranded row's residue and ends the row.
//
// # The two refusals, and why they are checks rather than assumptions
//
// Only a .partial path is ever removed. Every production path that writes
// LocalPath for an acquisition state writes the .partial name
// (internal/lifecycle/transfer.go records it on the DISCOVERED ->
// TRANSFERRING edge), so this check should never fire; it is here because
// "should never" is not a property a file deletion gets to rest on, and a
// row carrying a final path would otherwise cost an operator a backup.
//
// A row with a placement is refused for the same reason from the other
// direction: a placement is this journal's record of a durable copy, and
// no acquisition state has one. If the two ever disagree, the copy wins.
//
// # The order
//
// File first, then the journal write. Both orders leave residue if the
// process dies between them and they are not equally bad: this way the
// row still says TRANSFERRING, FR-21 keeps counting bytes that are already
// gone (an over-count, the direction internal/service/usage.go says every
// bias here must point) and a second sweep finishes the job. The other way
// round the row says FAILED, which internal/lifecycle defines as holding
// no local bytes, and the file stops being counted by anything while still
// occupying the disk.
func (s *Service) clearOne(ctx context.Context, rec state.Record) error {
	if len(rec.Placements) > 0 {
		return fmt.Errorf("app: refusing to clear %s: it is recorded in %s and carries %d durable placement(s); a copy this journal vouches for is never residue",
			rec.Artifact, rec.State, len(rec.Placements))
	}
	if rec.LocalPath != "" && !lifecycle.IsPartialPath(rec.LocalPath) {
		return fmt.Errorf("app: refusing to clear %s: its recorded local path %q is not one of FR-12's .partial names, and this only ever removes those",
			rec.Artifact, rec.LocalPath)
	}

	if rec.LocalPath != "" {
		if err := os.Remove(rec.LocalPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("app: clearing the residue of %s at %s: %w", rec.Artifact, rec.LocalPath, err)
		}
	}

	// Keyed off the row's own UpdatedAt rather than a clock reading, the
	// same derivation internal/reconcile uses and for the same reason: a
	// sweep re-run after a crash, against a row nothing else has touched,
	// reproduces the key and the journal's own replay recognises it.
	_, err := lifecycle.Advance(ctx, s.lifecycleDeps(), state.Transition{
		Artifact: rec.Artifact,
		Key:      fmt.Sprintf("app:clear-stranded:%s@%s", rec.Artifact, rec.UpdatedAt.UTC().Format(time.RFC3339Nano)),
		From:     rec.State,
		To:       string(lifecycle.Failed),
		Detail:   "issue #418: this backup set's configuration was removed while this artifact was still being acquired, so no cycle will ever advance it; an operator cleared its .partial residue",
	})
	if err != nil {
		return fmt.Errorf("app: ending %s: %w", rec.Artifact, err)
	}
	return nil
}

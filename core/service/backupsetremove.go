// This file is issue #391: removing one backup set's configuration.
//
// # What it removes, and what it deliberately keeps
//
// It removes the backup set from config.yaml and nothing else. Every
// journal row the set ever wrote stays exactly where it is, every file it
// pulled down stays on local storage, and both keep being listed: the
// backups list reads the journal (ListArtifacts, artifacts.go), never the
// configuration, so an artifact does not need a configured set behind it
// to be visible. That is not an accident of the implementation, it is the
// contract the Web UI's own confirmation dialog has been promising since
// before this operation existed:
//
//	Backup Manager will stop collecting backups for <name>.
//	<n> retained backups (<size>) stay on NAS storage and remain listed
//	under Backups.
//
// Deleting retained backups is a different operation with a different
// consent model (FR-20, the destructive gate, and retention's own
// identity checks before it removes any file). It is not this one, and it
// is not a second option on this one.
//
// # Why this is not a tombstone
//
// A removed set does not stay in the catalog in any form. A set that was
// still listed but no longer collecting would be indistinguishable to
// anyone reading the screen from a set that is DISABLED, which is a state
// this product already has, one click above Remove on the same card and
// fully reversible. Building a second, worse Disable and calling it
// Remove would leave every read surface needing a third truth value to
// tell them apart. So GET on a removed set answers 404
// ErrBackupSetNotFound, the same as an id that never existed, and a
// client that wants the set back creates it again.
//
// # Re-creating a set with the same source and name
//
// It re-adopts everything. An artifact id is source/set/name
// (model.NewArtifactID), so a backup set is identified by where it is and
// what it is called, not by a surrogate key nothing would ever match
// again. Creating "production/postgres-primary" after removing
// "production/postgres-primary" hands the new set every artifact the old
// one produced, along with its retention history.
//
// That is the behaviour I want, because it is what the overwhelmingly
// common reason to re-create needs: an operator undoing a removal, or
// rebuilding a NAS, or putting a name back. Detaching the history would
// mean re-fetching a volume full of backups to get back where they
// already were. What I am not willing to have is it happening quietly, so
// CreateBackupSet says out loud how much history a new set adopted
// (backupsets.go).
//
// The case this does NOT cover, said plainly rather than left to be
// found: remove a set, then create one with the same id pointing at a
// different remote_path or local_path, and you have exactly the repoint
// backupsetrepoint.go refuses on the update path, with no acknowledgement
// asked for, because the create path has never had one. Removal is what
// opens that route. Closing it belongs on create, with the same
// acknowledgement update already has, and it is filed rather than folded
// in here.
//
// # The sequence, and the one step that is not shared with the others
//
// Everything from config.Load onward is SetBackupSetEnabled's sequence
// (backupsetenabled.go), for the reasons recorded there in full: re-read
// the file rather than trusting this process's copy, encode BEFORE
// Validate resolves defaults in place, resolve the validator catalog
// before the write so the only step after it cannot fail, then one atomic
// state.Store.
//
// The step that is new is the hold. Rewriting the file and swapping this
// service's *app.Service does not reach a cycle that is already running:
// runScheduledCycle reads state.Load().inner once (scheduler.go) and that
// cycle keeps the pointer and the configuration snapshot it started with
// for its whole run. So a removal that only wrote the file would leave
// the current cycle discovering, transferring and, for a set that is not
// read-only, DELETING FROM THE OPERATOR'S SOURCE MACHINE for a set they
// just removed. The hold stops the pass where it stands and blocks the
// next one, and a removal's hold does not expire (edithold.go).
//
// # Where the hold is taken, and why it is under the lock
//
// It is taken inside configMu, once the file on disk has been read and
// the set found in it, and before anything is written. Not before the
// lock, which is where it first lived. The property that matters is
// "before the write and the swap", because those are the only two events
// in this method, and reading the file is neither; a hold taken a few
// milliseconds earlier stops nothing a hold taken here does not.
//
// What taking it earlier DID do is break under two overlapping removals
// of the same set, which is two tabs, two operators, or one client
// retrying a slow response. Both got past the cheap atomic-state check,
// both took the hold (the second call a no-op), one won the lock and
// removed the set, and the other found it gone, refused, and gave the
// hold back on the way out. The registry cannot tell whose hold that was,
// and the set was left gone from the configuration with nothing stopping
// a cycle in flight from processing it. Making the release conditional on
// "this call placed it" does not close that either: the placer can lose
// the lock to the second caller, which is then the one that removes the
// set, and the placer refuses and releases it. Under the lock, a caller
// that finds the set gone never took a hold and never gives one back, and
// a caller that took one holds the lock for every step that could make it
// give it back. There is no ordering left in which a hold is released by
// anyone but the call that placed it, for a set that is still there.
//
// What a stopped pass can leave behind is worth knowing: an interrupted
// transfer leaves a .partial file and a journal row short of a terminal
// state, and FR-17 reconcile is what would normally tidy that up on the
// next cycle. Reconcile only runs for configured sets, so no cycle ever
// will. It is not a retained backup and no promise covers it, but it is
// residue on a disk.
//
// That is issue #418, and it now has an answer rather than only a
// warning. The event below counts what this removal stranded, so the
// moment it happens says so; `backup-manager unconfigured` lists it
// afterwards along with everything else this set still holds; and
// internal/app's ClearStranded (unconfigured.go) is the operator-driven
// sweep that removes the .partial and ends the row. None of that touches
// a retained backup, and this removal still does not either.
package service

import (
	"context"
	"fmt"
	"log/slog"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// RemoveBackupSet removes one backup set's configuration and hot-reloads,
// so nothing is collected for it from here on.
//
// It returns ErrBackupSetNotFound for an id this deployment does not
// configure, including a second call for a set the first one removed. The
// EFFECT is idempotent (removing an already-removed set changes nothing)
// and the status deliberately is not: on a destructive control, a
// mistyped name reporting success is the failure this whole issue is
// about.
func (b *BackupService) RemoveBackupSet(ctx context.Context, id string) error {
	if b.configPath == "" {
		return ErrConfigNotFileBacked
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return wrapNotFound(id)
	}
	// Asked before the hold so a typo cannot pause anything, and asked
	// again under the lock below against the file itself, which is the
	// answer that decides. This read is the cheap one, off the atomic
	// state pointer, and it exists to keep the ordinary "no such set"
	// case from touching the registry at all.
	if err := b.requireBackupSet(id); err != nil {
		return err
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return fmt.Errorf("service: re-reading configuration: %w", err)
	}

	found := false
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		sets := cfg.Sources[i].BackupSets
		for j := range sets {
			if sets[j].Name != setName {
				continue
			}
			// The source itself stays, even when this was its last set.
			// It carries issue #282's source-level ReadOnly default, and
			// dropping the source would drop that declaration and hand
			// the next set created under the same name a silently
			// different posture. config.Validate permits an empty source
			// for exactly this reason; see its own comment.
			cfg.Sources[i].BackupSets = append(sets[:j:j], sets[j+1:]...)
			found = true
			break
		}
		if found {
			break
		}
	}
	if !found {
		// Gone from the file. Either a duplicate of this call got here
		// first, in which case its hold stands and must not be touched,
		// or the set was never there. Either way nothing was taken, so
		// nothing is given back.
		return wrapNotFound(id)
	}

	// The set is on disk and this call is the one taking it out. The hold
	// goes in HERE: under the lock, after the read, before the write. See
	// this file's own doc for why it moved off the front of the method,
	// and edithold.go for why it never expires.
	b.holds.holdRemoved(id)
	removed := false
	defer func() {
		// Every route out of here that took the hold and did NOT remove
		// the set gives it back, so a failed write never leaves a
		// configured set paused. The lock is still held when this runs,
		// so the hold being given back is this call's own.
		if !removed {
			b.holds.forgetRemoved(id)
		}
	}()

	// Encoded before Validate, which resolves defaults in place; see
	// SetBackupSetEnabled's own comment for the full reasoning and for
	// what an unrelated edit would otherwise freeze into the file.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("service: encoding configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return err
	}

	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		return fmt.Errorf("service: persisting configuration: %w", err)
	}

	applyValidators()

	b.adoptConfig(cfg)

	removed = true

	// On the record, at the moment it happens, and naming what stayed
	// behind rather than only what went. A destructive control that
	// leaves no trace of having been used is how a support conversation
	// six weeks later becomes archaeology.
	b.logger.Event(ctx, obs.LevelInfo, "backup_set_removed",
		"backup set configuration removed",
		slog.String("backup_set", id),
		slog.Int("retained_artifacts", b.artifactCountFor(ctx, id)),
		// Issue #418. The removal is the one moment that knows it just
		// stranded these, and a count of zero here is the ordinary case
		// worth stating: it is the difference between "this removal left
		// nothing half-done" and "nobody looked".
		slog.Int("stranded_artifacts", b.strandedCountFor(ctx, id)),
	)
	return nil
}

// artifactCountFor is how many journal rows this deployment holds for one
// backup set id, or -1 when it cannot say.
//
// -1 rather than 0, and that distinction is the point: a removal that
// could not read the journal must not log "0 retained artifacts", which
// is a specific and reassuring claim, when what it means is "I did not
// look". It never fails the removal it is describing; the configuration
// is already written by the time this runs, and a logging call is not
// allowed to turn a completed write into an error.
func (b *BackupService) artifactCountFor(ctx context.Context, id string) int {
	records, ok := b.recordsFor(ctx, id)
	if !ok {
		return -1
	}
	return len(records)
}

// strandedCountFor is how many of those rows this removal just stranded
// mid-acquisition: rows in DISCOVERED through COMMITTING, which nothing
// will ever advance once the set stops being configured (issue #418).
//
// It is a second read rather than a second return value from the count
// above, because that method has another caller in the create path and
// this lane does not own that file. -1 for the same reason, and it is the
// sharper case here: a removal that logged "0 stranded" when it could not
// look would be claiming the tidiest possible outcome for a question
// nobody can re-ask later, since the next reader has no way to know what
// the journal held at this instant.
func (b *BackupService) strandedCountFor(ctx context.Context, id string) int {
	records, ok := b.recordsFor(ctx, id)
	if !ok {
		return -1
	}
	stranded := 0
	for _, rec := range records {
		if app.IsAcquisitionState(rec.State) {
			stranded++
		}
	}
	return stranded
}

// recordsFor is the journal read both counts above share, and the one
// place their "I could not look" answer is decided.
func (b *BackupService) recordsFor(ctx context.Context, id string) ([]state.Record, bool) {
	if b.journal == nil {
		return nil, false
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return nil, false
	}
	setID, err := model.NewBackupSetID(sourceName, setName)
	if err != nil {
		return nil, false
	}
	records, err := b.journal.ListByBackupSet(ctx, setID)
	if err != nil {
		return nil, false
	}
	return records, true
}

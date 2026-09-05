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
// The case that used to be left open here, and now is not: remove a set,
// then create one with the same id pointing at a different remote_path or
// local_path, and you have exactly the repoint backupsetrepoint.go
// refuses on the update path. Removal is what opens that route, so
// removal is also where the one thing needed to close it is written down.
// This method records the address the set was pointing at as it takes the
// set out of the configuration (state.BackupSetAddress, migration 0008),
// and CreateBackupSet compares a later create over the same id against
// it (issue #411).
//
// That record is not a tombstone and does not put the set back in the
// catalog in any form: nothing lists it, no read surface can see it, and
// GET on the id still answers 404. It says where a set that used to exist
// was pointing, which is a question only the next create over that id
// ever asks.
//
// It is written BEFORE the configuration is rewritten, with the same
// discipline as every other fallible step in this sequence: everything
// that can fail happens while nothing has been persisted, so a failure
// here is a removal that did not happen rather than a removal with no
// record behind it. Refusing the removal outright is the point. The
// alternative is a removal that succeeds having quietly given up the only
// thing standing between the next create and a silent adoption, which is
// exactly the failure this is here to prevent.
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
// next cycle. Reconcile only runs for configured sets, so after a removal
// nothing ever will. It is not a retained backup and no promise covers
// it, but it is residue on a disk, and an operator should read that here
// rather than discover it.
package service

import (
	"context"
	"fmt"
	"log/slog"

	"gopkg.in/yaml.v3"

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
	var removedSet config.BackupSet
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		sets := cfg.Sources[i].BackupSets
		for j := range sets {
			if sets[j].Name != setName {
				continue
			}
			// Copied out before the splice below, which overwrites this
			// element with its successor.
			removedSet = sets[j]
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

	// Where this set was pointing, on the record, before anything is
	// written: see this file's own doc for what reads it and why a failure
	// here refuses the removal rather than proceeding without it.
	if err := b.recordRemovedAddress(ctx, sourceName, setName, removedSet); err != nil {
		return err
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
	if b.journal == nil {
		return -1
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return -1
	}
	setID, err := model.NewBackupSetID(sourceName, setName)
	if err != nil {
		return -1
	}
	records, err := b.journal.ListByBackupSet(ctx, setID)
	if err != nil {
		return -1
	}
	return len(records)
}

// recordRemovedAddress writes down where bs was pointing, so a later
// create over the same id can be checked against it
// (backupsetrepoint.go's requireCreateRepointAcknowledgement).
//
// The id is built through model.NewBackupSetID rather than read off
// bs.ID: this method's config was read with config.Load and never
// validated (the validation happens after the set is spliced out), and
// Load does not resolve ids, so bs.ID is still zero here.
//
// A BackupService with no journal has nowhere to write it and nothing
// that could ever read it back, so it records nothing: that is the
// in-memory construction core/ tests use, never a deployment.
func (b *BackupService) recordRemovedAddress(ctx context.Context, sourceName, setName string, bs config.BackupSet) error {
	if b.journal == nil {
		return nil
	}
	setID, err := model.NewBackupSetID(sourceName, setName)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBackupSetNotFound, err)
	}
	if err := b.journal.RecordBackupSetAddress(ctx, state.BackupSetAddress{
		Set:        setID,
		Host:       bs.Remote.Host,
		RemotePath: bs.RemotePath,
		LocalPath:  bs.LocalPath,
		RecordedAt: now(),
	}); err != nil {
		return fmt.Errorf("service: recording where %s was pointing before removing it: %w", setID, err)
	}
	return nil
}

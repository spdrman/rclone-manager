// This file is the one refusal on issue #350's update path and on issue
// #411's create path, and the reason it exists is narrower than "editing
// is dangerous".
//
// Almost every field an edit can change is safe on a set that already has
// history. Changing the port, the user, the include patterns, the
// completion strategy, the staleness budget or the validator changes how
// the NEXT pass behaves and leaves every artifact already on record
// exactly where it is. Three fields are not like that, because together
// they are what "the data this set is about" means:
//
//   - remote.host and remote_path name WHERE THE DATA COMES FROM. Every
//     artifact this set has ever produced is keyed by source/set/NAME
//     (model.NewArtifactID), and discovery matches a candidate against the
//     journal by that name relative to the set's own root. Repoint the
//     root at a different dataset that happens to use the same file names
//     (which is what a second NAS, a second database dump directory or a
//     restored-from-backup share looks like) and every one of those
//     candidates comes back AlreadyKnown. The cycle reports "already
//     known: 40", the health surface stays green, and not one byte of the
//     new dataset is ever fetched. A backup that has silently stopped
//     happening while reporting healthy is the worst outcome this product
//     has.
//
//   - local_path names WHERE THE BYTES LIVE. Retention identifies the
//     file it is about to delete by requiring the journal's own recorded
//     local path to equal the one the set's root and the artifact's name
//     compute (internal/retention's pruneVerifySafeToDelete). That check
//     is what stands between retention and a file it never positively
//     identified, and it is exactly right. But it also means that after a
//     repoint, EVERY artifact under the old root fails it forever: they
//     are refused rather than pruned, for as long as the set exists, and
//     the disk they are on fills with data an operator believes is being
//     managed. Catalog reconstruction stops seeing them for the same
//     reason (it scans the set's configured root).
//
// # Why an acknowledgement rather than a refusal, or a warning
//
// None of the above is data loss. Nothing is deleted, and pointing the
// field back restores every one of those relationships, which is why an
// outright refusal would be wrong: an operator whose NAS got a new
// address, or whose volume moved, has a legitimate change to make and no
// other way to make it now that hand-editing config.yaml is no longer the
// answer.
//
// A warning after the fact would be wrong for the opposite reason. The
// two failures above are both silent by construction (one reports success
// with a green cycle, the other reports refusals an operator has no
// reason to connect to an edit they made weeks ago), so the moment to say
// something is the moment of the change, while the operator still has the
// old value in front of them.
//
// So: this manager cannot tell "the same data at a new address" from "a
// different dataset", and nothing can from the outside, so it says so,
// names what is on record, and asks. Acknowledging is one flag, and the
// answer is recorded in the request rather than in a mode the next caller
// inherits.
//
// # What is deliberately NOT in the list
//
// remote.port and remote.user. Neither changes which directory on which
// machine holds the data: a port is how you reach the same host and a
// user is who you reach it as. Adding them would make the acknowledgement
// routine, and an acknowledgement an operator clicks through by habit
// protects nothing.
package service

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// repointedField is one of the three fields above, changing from what is
// on record to what the request asks for.
type repointedField struct {
	name string
	from string
	to   string
	cost string
}

// The costs, once, because the update path and the create path pay
// exactly the same ones and a second copy that drifted would tell one
// caller something the other is not told. Each is the consequence of the
// artifacts already on record staying where they are while the set moves.
const (
	hostCost = "a dataset at the new host whose file names match ones already on record is read as already backed up, and is never fetched"

	remotePathCost = "a dataset under the new path whose file names match ones already on record is read as already backed up, and is never fetched"

	localPathCost = "every artifact already stored under the old path stops matching what retention computes for it, so it is refused rather than pruned from now on, and catalog rebuild stops seeing it"
)

// repointedFields reports which of the identity-of-the-data fields this
// request actually CHANGES. A request that names a field and sets it to
// what it already is repoints nothing, and must not be made to
// acknowledge anything: the Web UI's per-box Save sends the box's current
// contents, and a save an operator makes for some other reason should not
// depend on which boxes happened to be in the request.
func repointedFields(current config.BackupSet, req UpdateBackupSetRequest) []repointedField {
	var changed []repointedField
	if req.Host != nil && *req.Host != current.Remote.Host {
		changed = append(changed, repointedField{
			name: "remote.host", from: current.Remote.Host, to: *req.Host,
			cost: hostCost,
		})
	}
	if req.RemotePath != nil && *req.RemotePath != current.RemotePath {
		changed = append(changed, repointedField{
			name: "remote_path", from: current.RemotePath, to: *req.RemotePath,
			cost: remotePathCost,
		})
	}
	if req.LocalPath != nil && *req.LocalPath != current.LocalPath {
		changed = append(changed, repointedField{
			name: "local_path", from: current.LocalPath, to: *req.LocalPath,
			cost: localPathCost,
		})
	}
	return changed
}

// requireRepointAcknowledgement refuses an update that moves one of those
// three fields on a backup set that already has artifacts on record,
// unless the request says it knows.
//
// "Already has artifacts on record" is every journal row for the set,
// whatever state it is in, because both failure modes are about names the
// journal remembers rather than about bytes still present: a COMPLETE
// artifact whose remote copy is long gone still occupies its name in
// discovery's already-known check, and still has a local file retention
// is managing.
//
// A set with no history at all is a set nothing can be orphaned from, so
// it is edited with no ceremony. That is the ordinary case for a
// mis-typed path corrected minutes after the wizard, which is the one
// edit that must not be made to feel dangerous.
func (b *BackupService) requireRepointAcknowledgement(ctx context.Context, sourceName, setName string, current config.BackupSet, req UpdateBackupSetRequest) error {
	if req.AcknowledgeRepoint {
		return nil
	}
	changed := repointedFields(current, req)
	if len(changed) == 0 {
		return nil
	}
	// No journal, no history: a BackupService built without one has never
	// journaled an artifact, so there is nothing to orphan. This is the
	// in-memory construction core/ tests use, never a deployment.
	if b.journal == nil {
		return nil
	}
	id, err := model.NewBackupSetID(sourceName, setName)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBackupSetNotFound, err)
	}
	records, err := b.journal.ListByBackupSet(ctx, id)
	if err != nil {
		return fmt.Errorf("service: reading this backup set's history: %w", err)
	}
	if len(records) == 0 {
		return nil
	}

	var moves []string
	var costs []string
	for _, f := range changed {
		moves = append(moves, fmt.Sprintf("%s from %q to %q", f.name, f.from, f.to))
		costs = append(costs, f.name+": "+f.cost)
	}
	return fmt.Errorf(
		"%w: this would move %s while %d artifact(s) are on record for %s. Those artifacts stay with the set and are not re-pointed with it (%s). If the new location holds the same data under a new address, that is fine and this is the acknowledgement asking you to confirm it; if it holds a different dataset, create a separate backup set for it instead. Re-send with acknowledge_repoint to proceed",
		ErrRepointNotAcknowledged,
		strings.Join(moves, ", "),
		len(records),
		id,
		strings.Join(costs, "; "),
	)
}

// # The same question, on the way in (issue #411)
//
// Everything above is written about an edit, and until a backup set's
// configuration could be removed it only ever had to be. A set's id could
// not be freed up, so there was no way to reach the state this file
// refuses except by moving a field on a set that was already there.
//
// Removal (issue #391) freed the id. Remove production/postgres-primary,
// create production/postgres-primary again pointing somewhere else, and
// the new set takes every artifact the old one left on record, at an
// address none of them came from: the AlreadyKnown failure and the
// never-pruned failure above, both, with nothing edited and nothing
// asked. So the create path asks the same question, in the same
// vocabulary, and re-creating a set at the address it was removed from
// stays silent and free, because that is the undo the removal path exists
// to allow.
//
// # What "what is on record" means here, which is not the same thing
//
// The update path compares the request against the set's own
// configuration, because there is one. On create there is not, by
// definition: the whole situation is an id with history and no
// configuration. So the comparison is against two different records, in
// this order.
//
//   - The address the id was last configured with, which RemoveBackupSet
//     writes as it takes a set out of the configuration
//     (state.BackupSetAddress, migration 0008). All three fields, and it
//     is the record this issue is actually about, since removal is what
//     opens the route.
//
//   - Failing that, the local root the artifacts on record actually
//     landed in, which the journal has held all along: state.Record's
//     LocalPath is the full path of the file, so the directory it sits in
//     is exactly the local_path the set was configured with when it was
//     fetched. This is the fallback for an id whose configuration went
//     away some other way (a build older than migration 0008, a hand
//     edit, a journal carried onto a rebuilt config.yaml), and it is
//     deliberately partial: nothing anywhere records which host or which
//     remote root an artifact came from, since state.Record.RemotePath is
//     one object's path RELATIVE to its set's root. So in the fallback
//     the remote half cannot be checked at all, and saying so here is
//     better than implying a check that is not happening.
//
// A set created over an id with NO artifacts on record is created with no
// ceremony either way, for the same reason an edit to one is: there is
// nothing to orphan. That is every ordinary create, including every one
// the wizard has ever made.

// createRepointedFields reports which of the identity-of-the-data fields
// this create request puts somewhere other than where the id's recorded
// address says its history came from.
func createRepointedFields(recorded state.BackupSetAddress, req CreateBackupSetRequest) []repointedField {
	var changed []repointedField
	if req.Host != recorded.Host {
		changed = append(changed, repointedField{
			name: "remote.host", from: recorded.Host, to: req.Host, cost: hostCost,
		})
	}
	if req.RemotePath != recorded.RemotePath {
		changed = append(changed, repointedField{
			name: "remote_path", from: recorded.RemotePath, to: req.RemotePath, cost: remotePathCost,
		})
	}
	if req.LocalPath != recorded.LocalPath {
		changed = append(changed, repointedField{
			name: "local_path", from: recorded.LocalPath, to: req.LocalPath, cost: localPathCost,
		})
	}
	return changed
}

// strandedLocalPath is the fallback comparison: the local roots the
// artifacts on record actually landed in, against the one this request
// asks for.
//
// Several roots is a real answer rather than a broken one. A set that was
// repointed with an acknowledgement earlier in its life has artifacts
// under both roots, and a request naming either of them is naming a place
// its own history genuinely is, so it repoints nothing that is not
// already where it will look. A request naming neither strands all of
// them.
//
// A record with no local path at all (DISCOVERED, or a transfer that
// never completed) contributes nothing: it names no place, so it cannot
// be stranded from one.
func strandedLocalPath(records []state.Record, localPath string) []repointedField {
	var roots []string
	seen := map[string]bool{}
	for _, rec := range records {
		if rec.LocalPath == "" {
			continue
		}
		root := filepath.Dir(rec.LocalPath)
		if root == localPath {
			return nil
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		return nil
	}
	sort.Strings(roots)
	return []repointedField{{
		name: "local_path", from: strings.Join(roots, ", "), to: localPath, cost: localPathCost,
	}}
}

// requireCreateRepointAcknowledgement refuses to create a backup set over
// an id that already has artifacts on record, at an address other than
// the one those artifacts came from, unless the request says it knows.
//
// It is called before anything at all is persisted, known_hosts file
// included: a refusal that had already written half the creation would be
// worse than no refusal.
func (b *BackupService) requireCreateRepointAcknowledgement(ctx context.Context, sourceName string, req CreateBackupSetRequest) error {
	if req.AcknowledgeRepoint {
		return nil
	}
	// No journal, no history: a BackupService built without one has never
	// journaled an artifact, so there is nothing to adopt. This is the
	// in-memory construction core/ tests use, never a deployment.
	if b.journal == nil {
		return nil
	}
	id, err := model.NewBackupSetID(sourceName, req.Name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	records, err := b.journal.ListByBackupSet(ctx, id)
	if err != nil {
		return fmt.Errorf("service: reading what is already on record for %s: %w", id, err)
	}
	if len(records) == 0 {
		return nil
	}

	recorded, found, err := b.journal.BackupSetAddress(ctx, id)
	if err != nil {
		// Refused rather than waved through, and it costs nothing to
		// refuse here: this runs before the first byte of the creation is
		// persisted, so the caller loses an attempt and not a set. The
		// alternative is proceeding on "I could not check", which is the
		// silence this whole file exists to stop.
		return fmt.Errorf("service: reading the address %s was last configured with: %w", id, err)
	}

	var changed []repointedField
	var against string
	if found {
		changed = createRepointedFields(recorded, req)
		against = "the address it was last configured with"
	} else {
		changed = strandedLocalPath(records, req.LocalPath)
		against = "where those artifacts actually landed, which is all this deployment recorded about them"
	}
	if len(changed) == 0 {
		return nil
	}

	var moves []string
	var costs []string
	for _, f := range changed {
		moves = append(moves, fmt.Sprintf("%s on record as %q, requested as %q", f.name, f.from, f.to))
		costs = append(costs, f.name+": "+f.cost)
	}
	return fmt.Errorf(
		"%w: %d artifact(s) are already on record for %s, and a backup set created under that id takes every one of them. Compared against %s, this creates it elsewhere: %s. Those artifacts do not move with it (%s). If this is the same data at a new address, that is fine and this is the acknowledgement asking you to confirm it; if it is a different dataset, give it a backup set id of its own instead. Re-send with acknowledge_repoint to proceed",
		ErrHistoryRepointNotAcknowledged,
		len(records),
		id,
		against,
		strings.Join(moves, ", "),
		strings.Join(costs, "; "),
	)
}

// This file is the one refusal on issue #350's update path, and the
// reason it exists is narrower than "editing is dangerous".
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
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// repointedField is one of the three fields above, changing from what is
// on disk to what the request asks for.
type repointedField struct {
	name string
	from string
	to   string
	cost string
}

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
			cost: "a dataset at the new host whose file names match ones already on record is read as already backed up, and is never fetched",
		})
	}
	if req.RemotePath != nil && *req.RemotePath != current.RemotePath {
		changed = append(changed, repointedField{
			name: "remote_path", from: current.RemotePath, to: *req.RemotePath,
			cost: "a dataset under the new path whose file names match ones already on record is read as already backed up, and is never fetched",
		})
	}
	if req.LocalPath != nil && *req.LocalPath != current.LocalPath {
		changed = append(changed, repointedField{
			name: "local_path", from: current.LocalPath, to: *req.LocalPath,
			cost: "every artifact already stored under the old path stops matching what retention computes for it, so it is refused rather than pruned from now on, and catalog rebuild stops seeing it",
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

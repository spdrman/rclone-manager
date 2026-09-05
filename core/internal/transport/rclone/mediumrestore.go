// This file is #241's half of FR-28: transport.MediumStore's restore pair,
// implemented over rclone v1.75.0's own s3 backend commands `restore` and
// `restore-status` (backend/s3/s3.go, the Fs.Command switch).
//
// # Everything here exists to stop one command doing more than it was asked
//
// rclone's `restore` is addressed by a REMOTE, not by an object. It runs
// operations.ListFn over whatever the Fs is rooted at and issues a
// RestoreObject for every archived object it walks. This adapter roots its
// Fs at the medium's bucket, so handing that command an unmodified context
// would restore every archived backup in the bucket: a per-object
// retrieval charge for the whole deployment, accepted by the provider
// before anything here could notice, and not cancellable afterwards.
//
// So restoreScope below is not a convenience. It is the mechanism that
// makes the blast radius one object, and TestARestoreTouchesExactlyOneObject
// is the test that watches it hold against a directory with three.
//
// # And to stop it reporting a success it did not have
//
// Neither command fails the way a Go caller expects. `restore` returns nil
// and a per-object list whose Status field carries the refusal ("Not
// GLACIER or DEEP_ARCHIVE or INTELLIGENT_TIERING storage class", or the
// provider's own error text), so a caller that only checks err believes a
// restore started that never did, and then waits hours for it. Both
// methods below read that list rather than the error alone.
package rclone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// restoreCommand and restoreStatusCommand are rclone's own names for the
// two s3 backend commands. They are constants here so the two call sites
// and the two tests that assert against them cannot drift apart from one
// another by a typo, which on this pair would be an ErrorCommandNotFound
// that reads as "this endpoint cannot restore".
const (
	restoreCommand       = "restore"
	restoreStatusCommand = "restore-status"
)

// restorePriority is the retrieval tier every restore this product asks
// for uses.
//
// Standard, always, and it is not configurable. Expedited is billed at a
// large multiple of Standard and is capacity-limited (a request for it can
// be REFUSED outright when the provider has no expedited capacity, which
// would turn a restore into an error an operator cannot act on), and Bulk
// is cheaper but slower than the figures archive.Behaviour.RestoreWait
// publishes, so offering it would make the one duration statement this
// product makes untrue. A deployment that genuinely needs a different tier
// is a configuration decision with its own issue, not a field on a
// request that an operator picks under pressure.
const restorePriority = "Standard"

// errRestoreUnsupported is what a medium type with no archive tier
// answers. It is a sentinel so a caller (and a test) can recognise the
// refusal without matching prose.
var errRestoreUnsupported = errors.New("this medium type has no archive tier, so nothing on it can be restored")

// errRestoreNotAccepted is what InitiateRestore reports when rclone
// answered without an error but its per-object status is not "OK", or
// when it restored a number of objects that is not exactly one.
var errRestoreNotAccepted = errors.New("the medium did not accept the restore")

// restoreScope splits a key into the Fs root and the single remote that
// addresses one object under it.
//
// The two are returned together because they are only correct together:
// the root bounds what restore-status lists, and the remote is what the
// files-from filter confines restore to. A caller that used one without
// the other would either list the whole bucket or restore a directory.
func restoreScope(bucket, key string) (root, remote string) {
	dir, leaf := path.Split(key)
	dir = strings.Trim(dir, "/")
	if dir == "" {
		// A key with no directory component. transport.MediumKey never
		// produces one (a key is always at least source/set/name), so
		// this is a hand-built key in a test or a medium configured
		// outside MediumKey. The bucket root is the only honest answer,
		// and the filter still confines the restore itself.
		return bucket, leaf
	}
	return bucket + "/" + dir, leaf
}

// confinedContext is the context both commands run under.
//
// # NoTraverse plus a files-from filter is what makes restore single-object
//
// The chain is worth spelling out, because it is not obvious from any one
// of the three settings and because a future reader deleting one of them
// would not see a test fail unless they knew this is what the test is
// about:
//
//   - the `restore` command calls operations.ListFn, which calls
//     walk.ListR;
//   - walk.ListR sees Filter.HaveFilesFrom() and takes the walk route
//     rather than the backend's own recursive listing;
//   - walk.Walk sees NoTraverse AND HaveFilesFrom and uses
//     Filter.MakeListR, which does not list anything at all: it calls
//     Fs.NewObject once per named file.
//
// So the restore visits exactly the objects named, by name, with no
// listing of the bucket anywhere in it.
//
// # DryRun and Interactive are pinned off, deliberately
//
// rclone's restore loop calls operations.SkipDestructive, and when that
// answers true it SKIPS the object and leaves its status as "OK". A caller
// reading that list gets a success for a restore that was never asked for.
// Nothing in this process sets either flag today, so this is belt and
// braces, but the failure it guards against is silent and expensive and
// the fix is two lines.
func confinedContext(ctx context.Context, remote string) (context.Context, error) {
	bounded, config := fs.AddConfig(ctx)
	config.LowLevelRetries = mediumRetries
	config.NoTraverse = true
	config.DryRun = false
	config.Interactive = false

	filtered, fi := filter.AddConfig(bounded)
	if err := fi.AddFile(remote); err != nil {
		return nil, err
	}
	return filtered, nil
}

// restoreStatusEntry is rclone's restore-status output for one object,
// re-declared here because the backend's own type is unexported.
//
// It is read back through JSON rather than by reflection over an
// unexported struct: the JSON encoding is what rclone documents and what
// `rclone backend restore-status` prints, so it is the part of that type
// upstream is least free to change without noticing.
type restoreStatusEntry struct {
	Remote        string `json:"Remote"`
	StorageClass  string `json:"StorageClass"`
	RestoreStatus *struct {
		IsRestoreInProgress *bool      `json:"IsRestoreInProgress"`
		RestoreExpiryDate   *time.Time `json:"RestoreExpiryDate"`
	} `json:"RestoreStatus"`
}

// restoreOutcomeEntry is rclone's restore output for one object.
type restoreOutcomeEntry struct {
	Status string `json:"Status"`
	Remote string `json:"Remote"`
}

// decodeCommandOutput re-reads an rclone backend command's `any` result
// into a typed slice.
//
// rclone's Command contract is that the result is "capable of being JSON
// encoded", which is exactly the guarantee this relies on and the only one
// it has: the concrete types behind both results are unexported.
func decodeCommandOutput(out any, into any) error {
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("re-reading the medium's answer: %w", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("re-reading the medium's answer: %w", err)
	}
	return nil
}

// remoteRelativeTo works out what rclone will call our object, given the
// root the Fs actually ended up with.
//
// It is given the root the Fs REPORTS rather than the one it was asked
// for, because the s3 backend can move it: NewFs HEADs the last segment of a
// root and, when that segment turns out to be an object (or when it cannot
// tell), returns an Fs rooted at the PARENT instead. Assuming the
// requested root would then compare a leaf name against a "set/leaf"
// remote and conclude, wrongly and quietly, that no restore is in play.
func remoteRelativeTo(fsRoot, bucket, key string) string {
	root := strings.Trim(strings.TrimPrefix(strings.Trim(fsRoot, "/"), bucket), "/")
	if root == "" {
		return key
	}
	return strings.TrimPrefix(strings.TrimPrefix(key, root), "/")
}

// canRestore reports whether this medium type has an archive tier at all.
//
// Only s3 does. The local-dir type exists so the MediumStore contract
// suite has an in-tree backend, and a directory on this machine has no
// asynchronous retrieval to ask about; answering a restore request there
// with a cheerful nil would leave a caller waiting for a state change that
// is never coming, which is exactly the silent degradation FR-13 forbids.
func canRestore(medium transport.Medium) bool { return medium.Type == transport.MediumTypeS3 }

// RestoreStatus reports what the medium says about a restore of key.
//
// # What it costs, and why it is rooted where it is
//
// rclone's restore-status does not obey filters (its own help says so) and
// is addressed by the Fs root, so the root is the only thing that bounds
// it. Rooted at the bucket it is a full-bucket listing on every status
// poll. So this one method roots its Fs at the object's DIRECTORY, which
// under transport.MediumKey's layout is one backup set, and it asks for
// the default output rather than -o all, so the answer carries only
// objects that actually have a restore status.
//
// # nil, and the one thing nil must not mean
//
// nil means the medium reports no restore status for this object, which
// covers "nobody ever asked" and "a restore finished and its window has
// been reaped". It must never also cover "there is no such object", so
// when the listing has nothing for this key the object is probed directly,
// and a key the medium does not hold comes back as a NotFound-classified
// error (and a MISSING BUCKET as a Configuration one, the distinction
// confirmBucket exists for). That probe only happens on the path that was
// about to answer nil, so an object that is actually restoring costs one
// listing and nothing else.
func (a *Adapter) RestoreStatus(ctx context.Context, medium transport.Medium, key string) (*transport.RestoreState, error) {
	if key == "" {
		return nil, transport.NewError(transport.Configuration, "restore_status",
			errors.New("a restore status needs an object key"))
	}
	if !canRestore(medium) {
		return nil, nil
	}

	ctx = mediumContext(ctx)
	root, _ := restoreScope(medium.Bucket, key)
	f, err := a.mediumFsAt(ctx, medium, root)
	if err != nil {
		return nil, err
	}
	defer shutdownFs(ctx, f)

	remote := remoteRelativeTo(f.Root(), medium.Bucket, key)

	commander, ok := f.(fs.Commander)
	if !ok {
		return nil, transport.NewError(transport.UnsupportedCapability, "restore_status",
			fmt.Errorf("medium %q: %w", medium.ID, errRestoreUnsupported))
	}
	out, err := commander.Command(ctx, restoreStatusCommand, nil, nil)
	if err != nil {
		if errors.Is(err, fs.ErrorCommandNotFound) {
			return nil, transport.NewError(transport.UnsupportedCapability, "restore_status",
				fmt.Errorf("medium %q: %w", medium.ID, errRestoreUnsupported))
		}
		wrapped := WrapCtx(ctx, "restore_status", err)
		if isNotFound(wrapped) {
			return nil, a.absenceOrMissingBucket(ctx, f, medium, "restore_status", err)
		}
		return nil, wrapped
	}

	var entries []restoreStatusEntry
	if err := decodeCommandOutput(out, &entries); err != nil {
		return nil, transport.NewError(transport.Unclassified, "restore_status", err)
	}
	if st := restoreStateFor(entries, remote); st != nil {
		return st, nil
	}

	// Nothing said about this key. Before answering "no restore in play",
	// establish that there is something at the key at all; see this
	// method's own doc for why the two must not collapse.
	if _, err := f.NewObject(ctx, remote); err != nil {
		wrapped := WrapCtx(ctx, "restore_status", err)
		if isNotFound(wrapped) {
			return nil, a.absenceOrMissingBucket(ctx, f, medium, "restore_status", err)
		}
		return nil, wrapped
	}
	return nil, nil
}

// restoreStateFor picks this object's row out of rclone's restore-status
// answer and turns it into the manager's own reading.
//
// It matches the remote EXACTLY rather than by suffix or by containment. A
// restore-status listing is recursive, so "photos.tar.gz" and
// "old/photos.tar.gz" both appear in it, and a match that accepted either
// would report one object's restore as the other's. That is not a
// hypothetical shape here: the key layout puts an artifact's name at the
// end of every key, so the names in one listing are exactly the ones most
// likely to share a suffix.
//
// Every field is a pointer on the way in, because rclone's own struct has
// them as pointers and an absent IsRestoreInProgress is not a false. It
// resolves to false anyway, which is the direction that claims the least:
// "no restore is running" plus a nil expiry reads, in archive.Access, as
// requires_restore.
func restoreStateFor(entries []restoreStatusEntry, remote string) *transport.RestoreState {
	for _, e := range entries {
		if e.Remote != remote || e.RestoreStatus == nil {
			continue
		}
		st := &transport.RestoreState{}
		if e.RestoreStatus.IsRestoreInProgress != nil {
			st.InProgress = *e.RestoreStatus.IsRestoreInProgress
		}
		if e.RestoreStatus.RestoreExpiryDate != nil {
			expiry := *e.RestoreStatus.RestoreExpiryDate
			st.ExpiresAt = &expiry
		}
		return st
	}
	return nil
}

// InitiateRestore asks the medium to make the object at key readable for
// windowDays days.
//
// It restores exactly one object. See this file's own header for the
// mechanism and for why an approximation here is not a performance
// question but a bill.
func (a *Adapter) InitiateRestore(ctx context.Context, medium transport.Medium, key string, windowDays int) error {
	if key == "" {
		return transport.NewError(transport.Configuration, "initiate_restore",
			errors.New("a restore needs an object key"))
	}
	if windowDays <= 0 {
		// The policy bounds live in internal/archive, which is where a
		// refusal an operator reads belongs. This one is the boundary
		// refusing to send a lifetime S3 itself would reject, so it is
		// deliberately only the floor.
		return transport.NewError(transport.Configuration, "initiate_restore",
			fmt.Errorf("a restore window of %d days is not a window", windowDays))
	}
	if !canRestore(medium) {
		return transport.NewError(transport.UnsupportedCapability, "initiate_restore",
			fmt.Errorf("medium %q: %w", medium.ID, errRestoreUnsupported))
	}

	ctx = mediumContext(ctx)
	f, err := a.mediumFs(ctx, medium)
	if err != nil {
		return err
	}
	defer shutdownFs(ctx, f)

	commander, ok := f.(fs.Commander)
	if !ok {
		return transport.NewError(transport.UnsupportedCapability, "initiate_restore",
			fmt.Errorf("medium %q: %w", medium.ID, errRestoreUnsupported))
	}

	// The Fs is rooted at the bucket, so the remote IS the key, and the
	// filter below is the only thing standing between this call and every
	// archived object the bucket holds.
	confined, err := confinedContext(ctx, key)
	if err != nil {
		return transport.NewError(transport.Configuration, "initiate_restore", err)
	}

	out, err := commander.Command(confined, restoreCommand, nil, map[string]string{
		"priority": restorePriority,
		"lifetime": strconv.Itoa(windowDays),
	})
	if err != nil {
		if errors.Is(err, fs.ErrorCommandNotFound) {
			return transport.NewError(transport.UnsupportedCapability, "initiate_restore",
				fmt.Errorf("medium %q: %w", medium.ID, errRestoreUnsupported))
		}
		wrapped := WrapCtx(ctx, "initiate_restore", err)
		if isNotFound(wrapped) {
			return a.absenceOrMissingBucket(ctx, f, medium, "initiate_restore", err)
		}
		return wrapped
	}
	return checkRestoreAccepted(out, key)
}

// checkRestoreAccepted turns rclone's per-object status list into an error
// or a nil.
//
// It is a free function so the three answers that are not a success can be
// tested without a bucket, which matters because two of the three are
// answers a real endpoint gives rarely and expensively.
//
// Empty is a failure, and that is the case worth naming. rclone lists what
// it walked, so an empty list means the filter matched nothing: the object
// is not there. Reporting that as a started restore leaves an operation
// row running against a job nobody is doing, waiting for a state change
// that cannot arrive.
func checkRestoreAccepted(out any, key string) error {
	var outcomes []restoreOutcomeEntry
	if err := decodeCommandOutput(out, &outcomes); err != nil {
		return transport.NewError(transport.Unclassified, "initiate_restore", err)
	}
	if len(outcomes) == 0 {
		return transport.NewError(transport.NotFound, "initiate_restore",
			fmt.Errorf("%w: nothing at %q was restored, so there is nothing at that key", errRestoreNotAccepted, key))
	}
	if len(outcomes) > 1 {
		// Unreachable through confinedContext, and checked anyway: this
		// is the assertion that the confinement held, made at the one
		// moment the answer is still in hand.
		return transport.NewError(transport.Unclassified, "initiate_restore",
			fmt.Errorf("%w: asking for %q restored %d objects, which means the request was not confined to one",
				errRestoreNotAccepted, key, len(outcomes)))
	}
	if outcomes[0].Status != "OK" {
		return transport.NewError(transport.UnsupportedCapability, "initiate_restore",
			fmt.Errorf("%w: %q: %s", errRestoreNotAccepted, key, outcomes[0].Status))
	}
	return nil
}

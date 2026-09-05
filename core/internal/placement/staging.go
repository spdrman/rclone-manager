package placement

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the local staging copy a medium-to-medium move goes
// through, and the failure modes it brings with it (issue #429).
//
// # What was here before, and why it was not a one-liner
//
// eligibleSource used to refuse a move whose two ends are both mediums,
// with a comment saying "FR-27's home rule only ever produces
// local-to-medium and medium-to-local". That sentence is false for a chain
// with two medium tiers, which is exactly the chain EPIC E's phase 2 exit
// gate names: an artifact ages out of the monthly window while its only
// copy is on the monthly medium, and the annual tier's home is a different
// one.
//
// The refusal was still load-bearing rather than conservative policy.
// copyToMedium hands the SOURCE placement's location straight to
// UploadFromLocal, and for a medium-resident source that location is an
// object key, not a path on disk, so lifting the guard alone produces
// "upload_from_local: not_found: object not found" rather than a move.
// There is no path behind it at all, which is what the refusal's first
// sentence meant by "a local staging copy and a second set of failure
// modes".
//
// # The shape, and where the invariant rests while three copies exist
//
// A staged move is the ordinary six phases with a longer copy phase.
// Inside COPYING it reads the source object down to a file on the backup
// set's own disk, proves that file hashes to what the journal recorded,
// uploads it to the destination, and removes it. Everything after that is
// the engine's existing machinery unchanged: the destination is verified,
// VERIFIED records its placements row, and only then does the source
// become DELETE_PENDING.
//
// So there really are three copies of the bytes in the world at once, and
// FR-30's standing invariant rests on the SOURCE for all of it. The
// staging copy is deliberately not a placement and never becomes one:
//
//   - It is not durable in the sense a placement claims. It exists for one
//     copy phase and is removed at the end of it, and a reader coming
//     back to a placements row expects to find a copy that is still there.
//   - Giving it a row would break the move it is part of. eligibleSource
//     refuses an artifact with more than one ACTIVE placement ("this
//     engine will not choose which of them is disposable"), and startCopy
//     re-checks eligibility on every resume, so a staged move that
//     recorded its staging file would refuse itself on the next cycle.
//   - It lives in a subdirectory of the backup-set root, which is
//     structurally outside what a local placement can name:
//     proveLocalSourceSafe accepts a local source only when its canonical
//     directory IS the canonical root. A staging file cannot be mistaken
//     for a managed artifact by the one function that authorises deleting
//     one.
//
// That last point is also why the staging area is a directory rather than
// a suffix on the artifact's own name. An artifact may legitimately be
// called "<anything>.staging": model.NewArtifactID refuses only the empty
// name, "." and "..", a path separator and padding whitespace. A
// subdirectory cannot collide with a bare basename.
//
// # The two questions a staged move has to ask that no other move does
//
// It has to READ the source, which is a content-class capability, so an
// archived source with no restore in effect is refused before the GET is
// spent rather than after the endpoint answers InvalidObjectState. That is
// the same gate verifyDestination already puts in front of a read, asked
// about the other end.
//
// And it needs the artifact's whole size on the backup set's own disk. A
// download that runs out of space partway has spent the egress, left a
// truncated file, and pushed the backup root closer to full, so the
// arithmetic happens first, from the size the journal already holds.

// StagingDirName is the directory, directly under a backup set's
// local_path, that a staged move writes its local copy into.
//
// Exported so a test and an operator-facing document can name the same
// string this engine uses, rather than each carrying a copy. The leading
// dot keeps it out of a casual listing of a backup root, which is the same
// reason artifactstore.Local.Put's temporary files carry one; and
// recovery.ScanManifests, the one thing that reads a backup root
// directly, skips directories, so nothing downstream sees it at all.
const StagingDirName = ".moves"

// stagingTempPrefix is what artifactstore.Local.Put names the temporary
// file it writes through before it links the result into place.
//
// It is a second copy of a literal that package owns, and it is here for
// one reason: a process killed part-way through a staged download leaves
// that temp file behind, because the deferred cleanup that would have
// removed it never runs. Nothing else ever will. One artifact-sized file
// per interrupted download, on the backup set's own disk, which is the
// disk the next hop's size check is about.
//
// The copy is pinned behaviourally rather than by a shared constant, and
// that is stronger than either exporting it or trusting this line. The
// crash matrix kills a real process in the middle of a real staged
// download and then requires the staging area to be empty after the
// restart (tests/movecrash, S1). If artifactstore ever names its temp
// files differently, that cell fails with the actual leftover name in the
// message. It is the cell that put this constant here in the first place.
const stagingTempPrefix = ".artifactstore-"

// stagingPath is where a staged move's local copy of one artifact goes.
//
// It is deterministic, with no timestamp and no random component, for the
// reason destinationLocator gives for the destination key: an interrupted
// copy that resumes has to target the same file and converge, instead of
// leaving one artifact-sized file per attempt on the backup set's disk.
// stageFromMedium is what makes converging safe, by checking what is
// already there rather than trusting it.
//
// The name comes from a Local rooted at the staging directory rather than
// from a join written here, so the formula is artifactstore's own. Issue
// #390 removed the last exported way to compute an artifact's local
// location without going through a store, and adding a second one back
// for this would be the third conversion #334's package doc warned about.
func (e *Engine) stagingPath(artifact model.ArtifactID) (string, error) {
	bs, err := e.backupSet(artifact.Set)
	if err != nil {
		return "", err
	}
	if bs.LocalPath == "" {
		return "", fmt.Errorf("backup set %s has no configured local_path", bs.ID)
	}
	if !filepath.IsAbs(bs.LocalPath) {
		return "", fmt.Errorf("backup set %s local_path %q is not an absolute path", bs.ID, bs.LocalPath)
	}
	store, err := artifactstore.NewLocal(filepath.Join(bs.LocalPath, StagingDirName))
	if err != nil {
		return "", fmt.Errorf("placement: backup set %s: %w", bs.ID, err)
	}
	return store.Locator(artifact)
}

// canStage is the free half of the medium-to-medium question, asked at
// plan time: is there anywhere on this deployment to put a staging copy?
//
// It reads configuration and nothing else, so a deployment that could
// never stage is refused before a move row is written, which is the same
// shape destinationCanBeVerified's refusal takes and costs the same
// nothing.
func (e *Engine) canStage(artifact model.ArtifactID) error {
	if e.Local == nil {
		return fmt.Errorf("no local store is configured, so there is nowhere to stage a copy")
	}
	if _, err := e.stagingPath(artifact); err != nil {
		return err
	}
	return nil
}

// copyMediumToMedium is the copy phase for a move whose two ends are both
// mediums.
//
// The ordering is deliberate and every step is refused-before-spent where
// it can be: prove the source is readable, prove there is room, stage,
// prove the staged bytes are the artifact, upload, remove the staging
// copy. Nothing here touches the source object or any placement row; the
// source is still ACTIVE and content-verified throughout, and it is what
// FR-30's invariant rests on until the VERIFIED write records the
// destination.
func (e *Engine) copyMediumToMedium(ctx context.Context, mv state.Move, src state.Placement) (int64, error) {
	if e.Store == nil {
		return 0, fmt.Errorf("no medium store is configured")
	}
	if e.Local == nil {
		return 0, fmt.Errorf("no local store is configured")
	}
	source, _, err := e.resolve(src.Medium)
	if err != nil {
		return 0, err
	}
	destination, _, err := e.resolve(mv.DestinationMedium)
	if err != nil {
		return 0, err
	}
	if err := e.sourceCanBeStaged(ctx, source, src); err != nil {
		return 0, err
	}

	staged, err := e.stagingPath(mv.Artifact)
	if err != nil {
		return 0, err
	}
	// The one direct os call in this package's production code, and it is
	// worth saying why it is allowed to be one.
	//
	// destructive_test.go's rule is that every byte this package DESTROYS
	// or OVERWRITES goes through LocalStore or MediumStore, because those
	// are the two seams a guard sits on. MkdirAll destroys nothing and
	// overwrites nothing: it creates a directory, and it fails rather than
	// replacing anything that is already at the path. The seam has no
	// method for it, and adding one would widen an interface whose whole
	// value is how narrow it is.
	//
	// It fails when the path is occupied by a file, which is the one
	// collision a subdirectory leaves: an artifact may legitimately be
	// named ".moves". That is a refusal with the source untouched, which
	// is the direction every uncertainty in this engine falls, and
	// TestAStagingAreaThatCannotBeCreatedRefusesTheMove pins it.
	if err := os.MkdirAll(filepath.Dir(staged), 0o750); err != nil {
		return 0, fmt.Errorf("preparing the staging area for %s: %w", mv.Artifact, err)
	}
	// Before the size check, not after, because what it sweeps is space
	// the check is about.
	if err := e.sweepStagingTemps(ctx, filepath.Dir(staged)); err != nil {
		return 0, err
	}
	if err := e.roomToStage(staged, src); err != nil {
		return 0, err
	}
	if err := e.stageFromMedium(ctx, source, src, staged); err != nil {
		return 0, err
	}

	res, uploadErr := e.Store.UploadFromLocal(ctx, destination, staged, mv.DestinationKey, transport.UploadOptions{})

	// The staging copy goes whether the upload worked or not, and a
	// failure to remove it is reported rather than swallowed.
	//
	// Swallowing it is the tempting choice, because a leftover file is
	// not dangerous: it is not a placement, nothing downstream can mistake
	// it for one, and the next attempt over the same artifact checks it
	// before reusing it. What it IS is an artifact-sized file on the
	// backup set's own disk, and the disk this move just measured is the
	// one the next move measures. A move engine that cannot delete from
	// the backup root has a problem worth stopping on, given that
	// deleting from the backup root is half of what it does.
	//
	// The cost of that choice, stated rather than glossed: a removal that
	// keeps failing leaves the move at COPYING, and the next cycle stages
	// again (reusing the file, so no egress) and uploads again. That is a
	// PUT per cycle, which is the shape #437 exists to prevent, so it is
	// worth being clear about why it is acceptable here and was not
	// there. #437's loop was reachable from a configuration an operator
	// could write and validate; this one needs the backup set's own root
	// to have become unwritable, which has already stopped ingestion, and
	// the destination cannot be an archive class because config refuses
	// that pairing at load, so no discarded copy buys a minimum billing
	// period. It is loud in the cycle report every cycle, and everything
	// still exists.
	rmErr := e.Local.Remove(ctx, staged)
	switch {
	case uploadErr != nil && rmErr != nil:
		return 0, fmt.Errorf("%w (and the staging copy at %q could not be removed either: %v)", uploadErr, staged, rmErr)
	case uploadErr != nil:
		return 0, uploadErr
	case rmErr != nil:
		return 0, fmt.Errorf("the copy to %q arrived and the staging copy at %q could not be removed: %w",
			mv.DestinationMedium, staged, rmErr)
	}
	return res.BytesUploaded, nil
}

// sourceCanBeStaged refuses a source this move could not read.
//
// Staging means downloading the object, which is exactly what Content
// class costs, so the question is the one CheckClass already answers:
// given what can be done with this copy right now, may a content-class
// read be attempted against it? An archived object with no restore in
// effect answers no, and answering it here rather than at the GET is what
// keeps the engine from reading the endpoint's InvalidObjectState as a
// failed copy worth retrying.
//
// The refusal comes back wrapped in ErrClassRefused for an archive class,
// which copy turns into an abandon rather than a retry: the next attempt
// is identical to this one and so is the thousandth.
//
// This is the third caller of observe, and the first two are named in
// destructive_test.go's table with an argument for why each is allowed.
// The argument for this one is the same as verifyCopy's: it reads and it
// refuses, it deletes nothing, and the answer is used to decline rather
// than to authorise.
func (e *Engine) sourceCanBeStaged(ctx context.Context, medium transport.Medium, src state.Placement) error {
	obs := e.observe(ctx, medium, src.Location, medium.StorageClass)
	access, err := archive.Access(src.Medium, medium.StorageClass, obs, e.now())
	if err != nil {
		return fmt.Errorf("%w: the source copy on %q: %w", ErrClassUnavailable, src.Medium, err)
	}
	if err := CheckClass(access, Content); err != nil {
		// The storage class is named as well as the access state, for the
		// reason CheckDestinationClass names it on the other end: the
		// access state says what is true right now and the class says
		// which line of the configuration made it true, and an operator
		// can only act on the second.
		return fmt.Errorf("%w; the source copy on %q is on storage class %s and has to be read back onto local disk "+
			"before it can be uploaded anywhere else, and a move between two mediums has no other way to reach the bytes",
			err, src.Medium, classOrDefault(medium.StorageClass))
	}
	return nil
}

// classOrDefault spells the class a medium writes with, resolving the
// unconfigured case the way config.StorageMedium.EffectiveStorageClass
// resolves it, so a message never reads "storage class """.
func classOrDefault(class string) string {
	if class == "" {
		return config.StorageClassStandard
	}
	return class
}

// roomToStage refuses a staged move that would not fit on the backup
// set's own disk.
//
// It is not FR-21's admission control and it deliberately invents no
// safety margin. FR-21's thresholds are configuration the ingest path
// reads, and a second, differently-tuned copy of them here would be a
// second answer to "how full is too full". What this asks is the one
// arithmetic fact this move needs and nothing more: the artifact's
// recorded size has to fit in what the filesystem says is available,
// before a byte of egress is spent on finding out that it does not.
//
// A placement with no recorded size is a refusal rather than an
// assumption, in the same direction every uncertainty in this engine
// falls: nothing here can say whether an unknown quantity fits.
func (e *Engine) roomToStage(staged string, src state.Placement) error {
	if src.Size == nil {
		return fmt.Errorf("the source placement on %q records no size, so nothing here can say whether a staging copy of it fits on local disk", src.Medium)
	}
	dir := filepath.Dir(staged)
	free, err := e.stagingFreeBytes(dir)
	if err != nil {
		return fmt.Errorf("the staging area %q could not be measured, so nothing here can say whether a staging copy fits: %w", dir, err)
	}
	if free < *src.Size {
		return fmt.Errorf("staging this copy needs %d bytes under %q and the filesystem reports %d available; "+
			"a move between two mediums goes through a local copy, so the backup set's own disk has to hold the whole artifact",
			*src.Size, dir, free)
	}
	return nil
}

// stagingFreeBytes reports how many bytes are available where a staging
// copy would go.
//
// StagingFreeBytes is the seam, for the reason Now is one: a test that
// needs a full disk cannot have one, and the alternative to injecting this
// is a check that is never exercised. Nil means the real thing.
func (e *Engine) stagingFreeBytes(dir string) (int64, error) {
	if e.StagingFreeBytes != nil {
		return e.StagingFreeBytes(dir)
	}
	stat, err := capacity.StatPath(dir)
	if err != nil {
		return 0, err
	}
	if stat.AvailableBytes > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(stat.AvailableBytes), nil
}

// stageFromMedium puts the artifact's bytes at staged, and proves they are
// the artifact's bytes before it says so.
//
// A staging file already at the path is the crash case, and it is handled
// by checking rather than by trusting or by refusing:
//
//   - It hashes to what the journal recorded, so it IS the artifact and is
//     reused. The move that made it already paid the egress and being
//     interrupted afterwards should not make anybody pay it twice.
//   - It is anything else, so it is a truncated or stale download, and it
//     is removed. Uploading it would put bytes on the destination that the
//     verification then refuses, twice, before the move gives up; and
//     leaving it would block Put, which links rather than renames and
//     refuses to clobber.
//
// Removing it is the one delete in this package that addresses neither a
// source nor a destination, and it is safe for a reason worth stating: the
// path is computed by this engine from the configured backup-set root, the
// staging directory's name and the artifact's own name, it is inside a
// directory nothing but this file writes to, and it can never be a
// placement's location. See this file's own comment.
func (e *Engine) stageFromMedium(ctx context.Context, medium transport.Medium, src state.Placement, staged string) error {
	ok, err := e.stagedCopyIsTheArtifact(ctx, staged, src)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if err := e.Local.Remove(ctx, staged); err != nil {
		return fmt.Errorf("clearing a staging copy at %q that is not this artifact: %w", staged, err)
	}

	rc, err := e.Store.OpenObject(ctx, medium, src.Location)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := e.Local.Put(ctx, staged, rc); err != nil && !errors.Is(err, artifactstore.ErrAlreadyPresent) {
		return err
	}

	ok, err = e.stagedCopyIsTheArtifact(ctx, staged, src)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the staging copy at %q does not hash to the %s the journal recorded for this artifact, so what arrived from %q is not it",
			staged, src.HashAlg, src.Medium)
	}
	return nil
}

// stagedCopyIsTheArtifact reports whether the file at staged is this
// artifact, by reading it and hashing it.
//
// A hash rather than a size, and re-read rather than remembered. This runs
// against a file another process may have written, on the resume path, and
// the whole reason it exists is that a size comparison cannot tell a
// complete download from a differently-corrupt one of the same length.
//
// An absent file is "no", not an error: nothing has been staged yet, which
// is the ordinary case on the first attempt.
func (e *Engine) stagedCopyIsTheArtifact(ctx context.Context, staged string, src state.Placement) (bool, error) {
	if _, err := e.Local.Stat(ctx, staged); err != nil {
		if errors.Is(err, artifactstore.ErrNotPresent) {
			return false, nil
		}
		return false, fmt.Errorf("looking at the staging copy at %q: %w", staged, err)
	}
	result, err := e.verifyLocalCopy(ctx, state.Placement{
		Medium:   config.MediumLocal,
		Location: staged,
		Size:     src.Size,
		Hash:     src.Hash,
		HashAlg:  src.HashAlg,
	})
	if err != nil {
		return false, fmt.Errorf("reading the staging copy at %q back: %w", staged, err)
	}
	return result.Passed, nil
}

// sweepStagingTemps removes the temporary files a killed process left in
// the staging directory.
//
// artifactstore.Local.Put writes through a temp file and links it into
// place, and removes the temp name on every path out of itself. Every path
// it takes, that is: a SIGKILL takes none of them, so a process killed
// during a staged download leaves a temp behind and the file at the
// staging name is never created at all. The crash matrix found this with a
// real kill (tests/movecrash, S1), and it is a leak nothing else collects
// because a temp name is random and cannot be attributed to an artifact.
//
// # Why this is allowed to delete files it cannot name in advance
//
// Every other delete in this package addresses exactly one path it
// computed itself, which is the property that makes it safe without a
// guard. This one is a prefix match over a directory, so it needs a
// different argument, and the argument is that the directory is entirely
// this file's: nothing else writes to it, an artifact's own copy can never
// be in it (proveLocalSourceSafe accepts a local source only in the
// canonical backup-set root), and the prefix is one only Put produces. It
// still goes through the LocalStore seam, so the guards a fixture puts on
// deletes see every one of them.
//
// It cannot race a download of its own. The engine drives one move at a
// time within a cycle and there are no goroutines under RunCycle, so at
// the moment this runs there is no staged read in flight to interrupt.
//
// A directory that cannot be listed is not an error here. The sweep is
// housekeeping in front of the real work, and the real work will fail on
// its own terms a moment later if the directory is genuinely unusable.
func (e *Engine) sweepStagingTemps(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), stagingTempPrefix) {
			continue
		}
		leftover := filepath.Join(dir, entry.Name())
		if err := e.Local.Remove(ctx, leftover); err != nil {
			return fmt.Errorf("clearing a temporary file %q left in the staging area by an interrupted download: %w", leftover, err)
		}
	}
	return nil
}

// discardStaging removes whatever this move left in the staging area.
//
// It is called from abandon, which is the one terminal end a staged move
// can reach with a staging copy still on disk: the successful path removes
// it inside the copy phase, and recopyOrAbandon deliberately leaves it
// alone because the retry it is arranging will check it and reuse it.
//
// A move with a local end never staged anything, and a move whose backup
// set can no longer be resolved never got as far as staging either, since
// staging needs the same resolution. Both are nil rather than an error:
// there is nothing there to remove.
func (e *Engine) discardStaging(ctx context.Context, mv state.Move) error {
	if mv.SourceMedium == config.MediumLocal || mv.DestinationMedium == config.MediumLocal {
		return nil
	}
	if e.Local == nil {
		return nil
	}
	staged, err := e.stagingPath(mv.Artifact)
	if err != nil {
		return nil
	}
	if err := e.Local.Remove(ctx, staged); err != nil {
		return fmt.Errorf("placement: discarding %s's staging copy at %q: %w", mv.Artifact, staged, err)
	}
	// And the temporary file an interrupted download of this artifact may
	// have left, which nothing else collects. See sweepStagingTemps.
	return e.sweepStagingTemps(ctx, filepath.Dir(staged))
}

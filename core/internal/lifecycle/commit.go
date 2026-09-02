package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/recovery"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file implements FR-14, the durable NAS commit. Steps 1 and 2 of
// FR-14's list (finish the transfer to .partial, finish required
// verification) are FR-11's and FR-13's job and are already done, and
// already durably recorded as VERIFIED, by the time Commit below is ever
// called. Commit performs the rest:
//
//	(durably record COMMITTING, before touching any file: see below)
//	3. fsync the .partial file's content        -> fsyncFile
//	4. atomically rename .partial to its final name, without
//	   clobbering an unrelated collision        -> linkWithoutClobbering
//	5. fsync the containing directory           -> fsyncDir
//	6. durably record COMMITTED                 -> the second Advance call
//
// Recording COMMITTING happens first, before step 3, not after step 5:
// state.go documents COMMITTING as meaning "the local file is being
// fsynced and renamed ... followed by a directory fsync", and
// crash_safety.go's COMMITTING -> COMMITTED walkthrough already reasons
// about exactly this window. This file's job is to make that reasoning
// true, not to relitigate it.
//
// # What this establishes about the target NAS filesystem, and what it does not
//
// FR-14 asks for the target filesystem's behaviour and limitations to be
// documented honestly, because an fsync the filesystem quietly ignores
// would make everything above theatre. Here is the honest accounting.
//
// What Commit's calls guarantee, PROVIDED the filesystem beneath them
// honours the POSIX contract for fsync(2), rename-equivalent primitives and
// directory entries:
//
//   - After step 3 returns without error, the .partial file's content is
//     durable against a crash, independent of what name it can be found
//     under.
//   - After step 4 returns without error, exactly one of the .partial name
//     or the final name resolves to that content (never both, never
//     neither, and never a foreign file silently replaced: see
//     linkWithoutClobbering).
//   - After step 5 returns without error, the directory entry created or
//     removed by step 4 is itself durable, not just sitting in the
//     directory inode's writeback cache. This is the step people skip: a
//     directory is a separate inode from the file it names, with its own
//     separate writeback and journalling state, so fsyncing the file's
//     data in step 3 says nothing about whether the *name* pointing at it
//     survives a power loss. Skip step 5 and a crash can leave content
//     that was genuinely fsynced sitting under an inode nothing in the
//     directory points at yet, or still only reachable under the old
//     .partial name, while COMMITTED already claims otherwise.
//
// What this repository's own test suite (commit_test.go) actually checked:
// every fsync, link, remove and rename-equivalent call runs against a real
// filesystem (whatever backs Go's t.TempDir() locally, and whatever backs
// it on the CI runner) and is proven idempotent under a simulated crash
// injected between step 4 and step 5. That proves the *ordering and
// recovery logic in this file* is correct given that the primitives behave
// the way POSIX documents. It does not, and cannot, prove anything about
// physical durability, because no unit test can observe what a disk does
// after power is actually cut.
//
// What was not, and could not be, established from inside this development
// environment:
//
//   - Whether fsync on the NAS's actual storage stack durably flushes to
//     the physical medium, versus succeeding against a volatile write
//     cache (drive-level or RAID/controller-level) that a real power loss
//     can still lose. That is a property of the disk firmware and storage
//     controller beneath the filesystem, not of this code, and no Go test
//     run here can observe it. This implementation trusts fsync(2)'s
//     documented contract because that is the only durability primitive
//     the OS exposes, not because physical durability has been verified
//     against real UGREEN hardware from this sandbox.
//   - Directory-fsync semantics specifically on UGOS Pro, the OS the
//     target UGREEN NAS runs. Public/community documentation (not this
//     repository's own testing) describes UGOS Pro volumes as ext4 by
//     default with btrfs offered as an option; both are Linux filesystems
//     that support fsync on a directory file descriptor for exactly the
//     reason described above. That is second-hand vendor/community
//     information, not something this environment could confirm against
//     the physical device.
//   - Behaviour if the configured local destination is ever a *network*
//     filesystem (an NFS or SMB/CIFS mount) rather than storage local to
//     the NAS. NFS and CIFS clients are well documented to sometimes
//     report fsync as successful without the server having actually
//     committed the write, and directory fsync over those protocols is
//     frequently unsupported or a silent no-op depending on the client,
//     server and mount options. Nothing in this file can detect that case:
//     a filesystem that quietly no-ops fsync returns the same nil error a
//     filesystem that genuinely flushed does. Operators MUST point the
//     configured local path at storage local to the NAS itself, matching
//     the architecture this project already assumes (the manager runs on
//     the NAS and writes to "UGREEN NAS filesystem" directly, per
//     docs/EPIC.md), not at a share re-exported over the network.
//   - Silent bit rot or media corruption after the fact. FR-13's
//     verification defends the *content* at commit time; FR-14 only ever
//     promises "what was verified is now durably at its final name", never
//     "what is at that name is still correct forever". That is why FR-17's
//     reconciliation and periodic re-verification exist as a separate,
//     ongoing concern rather than something this file tries to also solve.
//
// The honest summary: this code makes every durability call the OS
// provides, in the order that call matters, and is provably idempotent
// under a crash injected at any point in that sequence. Whether a specific
// disk in a specific UGREEN unit actually honours those calls once power is
// cut is a hardware and firmware trust assumption this design makes
// explicit, rather than a claim this code, or any unit test, can prove.

// CommitInput is what Commit needs to durably move one artifact from
// VERIFIED to COMMITTED.
type CommitInput struct {
	Artifact model.ArtifactID

	// LocalDir is the backup set's configured local destination directory,
	// exactly the value TransferParams.LocalDir held when this artifact was
	// transferred (config.BackupSet.LocalPath). Commit derives both the
	// .partial path and the final path from LocalDir and Artifact using the
	// same finalPath/partialPath helpers transfer.go uses, rather than
	// taking either path as a caller-supplied string, so the transfer step
	// and the commit step can never disagree about where an artifact's
	// files live, and the two paths are always in the same directory by
	// construction: that is what lets the single directory fsync in step 5
	// durably cover both the added final-name entry and the removed
	// .partial-name entry with one call.
	LocalDir string

	// CommittingKey and CommittedKey are the FR-9 idempotency keys for the
	// two separate durable journal writes Commit makes (VERIFIED ->
	// COMMITTING, then COMMITTING -> COMMITTED). Derive both
	// deterministically per artifact, for example artifact-id plus a fixed
	// suffix, and reuse the exact same values on every retry: Commit's
	// crash recovery depends on the journal recognising a retried call as
	// the same logical attempt (see state.Transition's Key doc), not on
	// Commit inspecting the journal's current state itself.
	CommittingKey string
	CommittedKey  string
}

func (in CommitInput) validate() error {
	switch {
	case in.Artifact.Name == "" || in.Artifact.Set.IsZero():
		return fmt.Errorf("lifecycle: Commit needs a valid artifact id")
	case in.LocalDir == "":
		return fmt.Errorf("lifecycle: Commit needs a LocalDir")
	case in.CommittingKey == "":
		return fmt.Errorf("lifecycle: Commit needs a CommittingKey")
	case in.CommittedKey == "":
		return fmt.Errorf("lifecycle: Commit needs a CommittedKey")
	case in.CommittingKey == in.CommittedKey:
		return fmt.Errorf("lifecycle: CommittingKey and CommittedKey must differ, they record two different transitions")
	}
	return nil
}

// Commit durably moves one artifact from VERIFIED to COMMITTED (FR-14),
// performing the fsync/rename/fsync sequence described in this file's
// package doc in between the two journal writes that bracket it.
//
// Commit is safe to call more than once, or to resume with, for the same
// CommitInput after a crash at any point in the sequence, including before
// it ever started, that never wrote anything, and after it fully finished,
// which case simply converges. It relies entirely on the FR-9 journal's
// idempotency-key replay (see state.Transition's Key doc) to recognise its
// own earlier attempts, rather than inspecting current state up front, so
// the caller's obligation is exactly the one that doc already states: reuse
// the same CommittingKey/CommittedKey across retries of the same logical
// attempt.
func Commit(ctx context.Context, d Deps, in CommitInput) (state.Outcome, error) {
	if err := in.validate(); err != nil {
		return state.Outcome{}, err
	}

	partial := partialPath(in.LocalDir, in.Artifact)
	final := finalPath(in.LocalDir, in.Artifact)

	committing, err := Advance(ctx, d, state.Transition{
		Artifact: in.Artifact,
		Key:      in.CommittingKey,
		From:     string(Verified),
		To:       string(Committing),
	})
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: commit %s: recording COMMITTING: %w", in.Artifact, err)
	}

	switch {
	case committing.Applied:
		// Fresh: this call itself just recorded COMMITTING. The file
		// operations below might still be left over from an *earlier*
		// attempt that got as far as touching the filesystem before ever
		// reaching this write in a previous run (see the CommittingKey
		// doc); commitFile's own idempotence handles that regardless.
	case committing.Record.State == string(Committed):
		// A previous attempt using this same CommittingKey ran all the way
		// through to COMMITTED before this call was made (or observed
		// success from a call that crashed only after the fact). Converge
		// without touching the filesystem or the journal again, other than
		// re-ensuring the recovery manifest exists (see writeRecoveryManifest's
		// doc: a process killed after COMMITTED landed but before the
		// manifest was ever written must not leave it missing forever).
		if committing.Record.LocalPath != final {
			return state.Outcome{}, fmt.Errorf(
				"lifecycle: commit %s: already COMMITTED at %q, not the final path %q this LocalDir computes",
				in.Artifact, committing.Record.LocalPath, final)
		}
		if err := writeRecoveryManifest(in.LocalDir, committing.Record); err != nil {
			return state.Outcome{}, err
		}
		return committing, nil
	case committing.Record.State == string(Committing):
		// A previous attempt recorded COMMITTING and then the process
		// died somewhere before COMMITTED landed. Proceed exactly as the
		// fresh case does; commitFile is idempotent regardless of how far
		// that attempt got.
	default:
		return state.Outcome{}, fmt.Errorf(
			"lifecycle: commit %s: CommittingKey was already used for a transition to %s, refusing to guess what to do from here",
			in.Artifact, committing.Record.State)
	}

	if committing.Record.LocalPath != partial {
		return state.Outcome{}, fmt.Errorf(
			"lifecycle: commit %s: journal's recorded local path %q does not match the .partial path %q this LocalDir computes",
			in.Artifact, committing.Record.LocalPath, partial)
	}

	if err := commitFile(partial, final); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: commit %s: %w", in.Artifact, err)
	}

	committed, err := Advance(ctx, d, state.Transition{
		Artifact:  in.Artifact,
		Key:       in.CommittedKey,
		From:      string(Committing),
		To:        string(Committed),
		LocalPath: &final,
	})
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: commit %s: recording COMMITTED: %w", in.Artifact, err)
	}
	if err := writeRecoveryManifest(in.LocalDir, committed.Record); err != nil {
		return state.Outcome{}, err
	}
	return committed, nil
}

// writeRecoveryManifest writes rec's EPIC-B section 19.3 sidecar recovery
// manifest into localDir, deriving every field from rec itself, which,
// like every internal/state.Record, never carries a secret (see
// internal/recovery's package doc for the structural guarantee this
// relies on).
//
// It is called from both of Commit's success paths above: the
// fresh/resumed one and the already-converged one. That duplication is
// deliberate, not an oversight: a process killed after the COMMITTED
// journal write landed but before the manifest was ever written must
// still get one written on the very next call with the same
// CommittingKey/CommittedKey, via the already-converged branch, rather
// than silently staying without recovery metadata forever. Since the
// manifest's content is derived entirely, deterministically, from the
// journal record, writing it again on a later converged call is always
// safe: it overwrites the same bytes it would have written the first time.
func writeRecoveryManifest(localDir string, rec state.Record) error {
	size := int64(0)
	switch {
	case rec.Transfer != nil:
		size = rec.Transfer.BytesTransferred
	case rec.Remote.Size != nil:
		size = *rec.Remote.Size
	}

	m := recovery.Manifest{
		FormatVersion:      recovery.CurrentFormatVersion,
		Source:             rec.Artifact.Set.Source,
		BackupSet:          rec.Artifact.Set.Set,
		ArtifactName:       rec.Artifact.Name,
		RemotePath:         rec.RemotePath,
		ProducerTimestamp:  rec.Remote.ModTime,
		ReceivedTimestamp:  rec.UpdatedAt,
		RetentionTimestamp: rec.DiscoveredAt,
		SizeBytes:          size,
		Checksum:           rec.LocalHash,
		ChecksumAlgorithm:  rec.LocalHashAlg,
		ValidationPassed:   rec.ValidationPassed,
		ValidationDetail:   rec.ValidationDetail,
		Placements:         manifestPlacements(rec),
	}
	if err := recovery.WriteManifest(localDir, m); err != nil {
		return fmt.Errorf("lifecycle: commit %s: writing recovery manifest: %w", rec.Artifact, err)
	}
	return nil
}

// manifestPlacements copies rec's FR-29 placements into the sidecar's own
// shape, field for field, with nothing added and nothing derived.
//
// Only the medium ID travels, never anything about how to reach that
// medium: a sidecar lives in the user backup root, which is a different
// security domain from private state (EPIC-B section 19.1), and FR-33's
// rule is that no endpoint and no credential is ever written there.
// recovery.ManifestPlacement has nowhere to put one, which is what makes
// this a copy rather than a filter.
func manifestPlacements(rec state.Record) []recovery.ManifestPlacement {
	if len(rec.Placements) == 0 {
		return nil
	}
	out := make([]recovery.ManifestPlacement, 0, len(rec.Placements))
	for _, p := range rec.Placements {
		out = append(out, recovery.ManifestPlacement{
			Medium:            p.Medium,
			Location:          p.Location,
			SizeBytes:         p.SizeBytes,
			Checksum:          p.Hash,
			ChecksumAlgorithm: p.HashAlg,
			VerificationClass: p.VerificationClass,
			VerifiedAt:        p.VerifiedAt,
			Status:            p.Status,
		})
	}
	return out
}

// commitFile performs FR-14 steps 3 through 5: fsync the transferred file's
// content, rename it to its final name without silently clobbering an
// unrelated file already there, and fsync the directory the rename's
// durability depends on.
//
// Its parameters are named partial and final, not partialPath/finalPath,
// deliberately: those latter names are transfer.go's package-level helpers
// that compute these same two strings from a LocalDir and an
// model.ArtifactID (see Commit), and this function only ever needs the
// already-resolved paths, never to call those helpers itself.
//
// It is safe to call more than once for the same (partial, final) pair. The
// crash matrix (docs/EPIC.md) can kill the process after any one of the
// underlying syscalls below, and Commit's caller is expected to retry the
// whole thing with no memory of how far an earlier attempt got; this
// function figures that out fresh from what is actually on disk each time,
// rather than trusting any in-memory or journalled record of its own
// progress.
func commitFile(partial, final string) error {
	finalInfo, finalErr := os.Stat(final)
	partialInfo, partialErr := os.Stat(partial)

	switch {
	case finalErr == nil && partialErr == nil:
		// Both names exist. Under this function's own invariants the only
		// way that happens is an earlier attempt completing the link below
		// and being killed before the matching remove finished: link
		// creates a second name for the very same inode, so os.SameFile is
		// a precise, zero-ambiguity test for "this is our own interrupted
		// retry," not a coincidence. Anything else already sitting at
		// final is the FR-12 collision linkWithoutClobbering exists to
		// refuse, just discovered here instead of at the link call.
		if !os.SameFile(finalInfo, partialInfo) {
			return &FinalPathCollisionError{PartialPath: partial, FinalPath: final}
		}
		if err := removeIfExists(partial); err != nil {
			return fmt.Errorf("removing %s after an earlier attempt already linked it to %s: %w", partial, final, err)
		}

	case finalErr == nil:
		// final exists, partial does not: an earlier attempt finished the
		// rename (link, then remove) before being killed, most likely
		// before the directory fsync below, or before the COMMITTED
		// journal write ever landed. Nothing left to do to the files
		// themselves; fall through to the directory fsync, which is itself
		// idempotent and might still be the very thing that never happened
		// last time.

	case os.IsNotExist(finalErr) && partialErr == nil:
		// Normal case: the .partial file is there, nothing occupies the
		// final name yet. Do the actual work, in FR-14's order: flush the
		// content durably *before* it becomes reachable under the name
		// that is about to be promised durable.
		if err := fsyncFile(partial); err != nil {
			return fmt.Errorf("fsyncing %s before renaming it: %w", partial, err)
		}
		if err := linkWithoutClobbering(partial, final); err != nil {
			return err
		}
		if err := removeIfExists(partial); err != nil {
			return fmt.Errorf("removing %s after linking it to %s: %w", partial, final, err)
		}

	case os.IsNotExist(finalErr) && os.IsNotExist(partialErr):
		// Neither name exists. FR-11 and FR-13 are supposed to guarantee
		// the .partial file is present by the time anything calls Commit,
		// so this is not a retryable crash window: it is data loss (or a
		// caller passing the wrong LocalDir), and it has to say so loudly
		// rather than let "nothing to commit" be mistaken for success.
		return &ArtifactFileMissingError{PartialPath: partial, FinalPath: final}

	default:
		if finalErr != nil && !os.IsNotExist(finalErr) {
			return fmt.Errorf("checking %s: %w", final, finalErr)
		}
		return fmt.Errorf("checking %s: %w", partial, partialErr)
	}

	if testHookAfterRename != nil {
		if err := testHookAfterRename(); err != nil {
			return err
		}
	}

	// Fsync the containing directory. This looks redundant to anyone who
	// has not been bitten by it before: rename(2), and the link-then-remove
	// pair linkWithoutClobbering uses in its place, are each atomic with
	// respect to a crash in the sense that an observer never sees a torn
	// pathname mid-operation, but "atomic" only means the operation cannot
	// be observed half-done, not that its result is durable yet. A
	// directory is a separate inode from the file it names, with its own
	// separate writeback and journalling state that step 3's file-content
	// fsync says nothing about. Skipping this step means COMMITTED's
	// promise, "the local copy durably exists at its final name," can be
	// false even though the content itself was genuinely fsynced: a power
	// loss right after the rename can lose the directory's record of the
	// change while the data blocks it pointed at survive, leaving the
	// content durable on disk but findable under neither name, or still
	// only under the old .partial name.
	if err := fsyncDir(filepath.Dir(final)); err != nil {
		return fmt.Errorf("fsyncing the directory containing %s: %w", final, err)
	}
	return nil
}

// linkWithoutClobbering performs FR-14 step 4, the atomic rename, as a hard
// link followed by removing the old name, instead of a plain os.Rename.
// Plain rename(2) on POSIX silently replaces whatever already sits at
// newname; FR-12 requires the opposite ("Final-name collisions SHALL fail
// safely rather than overwrite a known-good backup"), and os.Rename offers
// no way to ask for that. os.Link instead fails with EEXIST if final is
// already occupied, which is exactly "fail safely" instead of "guess and
// clobber." The link and the later removal of the old name carry the same
// atomicity guarantee plain rename has for a crashing observer: at every
// instant there is either just the old name, both names (this function's
// own deliberately-idempotent transient state), or just the new name, never
// a torn or vanished file.
func linkWithoutClobbering(partial, final string) error {
	err := os.Link(partial, final)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("linking %s to %s: %w", partial, final, err)
	}

	// Something is already at final. Proceeding is only safe if it is
	// literally the same file this call is trying to link, i.e. a
	// previous, interrupted attempt (or a racing one) reached here first;
	// anything else is the FR-12 collision this function exists to refuse.
	finalInfo, ferr := os.Stat(final)
	partialInfo, perr := os.Stat(partial)
	if ferr == nil && perr == nil && os.SameFile(finalInfo, partialInfo) {
		return nil
	}
	return &FinalPathCollisionError{PartialPath: partial, FinalPath: final}
}

// fsyncFile opens path read-only purely to obtain a file descriptor to
// fsync: fsync(2) flushes an inode's data by file descriptor regardless of
// the mode that descriptor was opened with, so nothing here needs, or
// takes, write access to content FR-11/FR-13 already finished writing and
// verifying.
func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// fsyncDir fsyncs a directory the same way fsyncFile fsyncs a file: open it
// (POSIX only ever allows opening a directory read-only in the first
// place) and call Sync on the resulting descriptor. See commitFile's
// comment on its own call to this function for why a directory needs this
// at all.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// removeIfExists removes path, treating "already gone" as success rather
// than an error. Every caller here is finishing a remove that a previous,
// interrupted attempt might have already completed.
func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// testHookAfterRename, when non-nil, is invoked by commitFile immediately
// after step 4 (the rename, or finishing an earlier attempt's interrupted
// one) but before step 5, the directory fsync. It exists solely so
// commit_test.go can simulate the process being killed in exactly that
// window and prove the next call converges safely; nothing outside this
// package ever sets it, and it is nil, so a no-op, in every real build.
var testHookAfterRename func() error

// FinalPathCollisionError reports that Commit's rename target is already
// occupied by a file that is not the artifact being committed. FR-12:
// "Final-name collisions SHALL fail safely rather than overwrite a
// known-good backup." Neither the .partial file nor whatever already
// exists at FinalPath is touched.
type FinalPathCollisionError struct {
	PartialPath string
	FinalPath   string
}

func (e *FinalPathCollisionError) Error() string {
	return fmt.Sprintf("lifecycle: %s already exists and is not %s, refusing to overwrite it", e.FinalPath, e.PartialPath)
}

// ArtifactFileMissingError reports that neither the .partial file nor the
// final file exists on disk when Commit went looking for them. FR-11 and
// FR-13 are supposed to guarantee the .partial file is present by the time
// Commit runs, so this is not a crash window Commit can retry through: the
// local copy is actually gone, or the caller passed the wrong paths.
type ArtifactFileMissingError struct {
	PartialPath string
	FinalPath   string
}

func (e *ArtifactFileMissingError) Error() string {
	return fmt.Sprintf("lifecycle: neither %s nor %s exists on disk", e.PartialPath, e.FinalPath)
}

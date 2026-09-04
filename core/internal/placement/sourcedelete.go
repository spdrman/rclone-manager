package placement

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the gate in front of the one irreversible act in this
// package. It is internal/retention/prune.go's pruneVerifySafeToDelete
// applied to the other kind of local delete, and it is written the same
// way on purpose: every fact re-derived from the artifact's own journal
// record and the backup set's own configured root, at the moment of the
// delete, never trusted from whatever the caller already checked.
//
// The redundancy is the design. deleteSource has already re-verified the
// destination before it gets here; phases.go's table has already made
// arriving here from anywhere but SOURCE_DELETE_PENDING impossible;
// state.AdvanceMove has already refused a write against a row that moved
// on. This function assumes none of that and proves it again, because the
// cost of being wrong once is a backup that no longer exists.
//
// # prunePartialSuffix, spelled a third time
//
// internal/retention already duplicates internal/lifecycle's unexported
// partialSuffix, with a comment explaining why it is a literal rather than
// an export. The same argument applies here and produces the same literal.
// A move is only ever planned for an artifact in COMPLETE, whose final
// name cannot carry this suffix, so this check is unreachable through the
// engine's own path and is kept anyway as its own named refusal: FR-20
// asks for "never a .partial" as a guarantee, not as an implication of
// something else that happens to be true.
const partialSuffix = ".partial"

// errSourceAlreadyGone is what the proof returns when the source copy is
// not there any more, having proved everything else about the world.
//
// It is a distinct answer rather than a refusal, and getting that
// distinction wrong is a real bug this suite caught: a crash between the
// source delete landing on disk and the DONE write leaves a journal at
// SOURCE_DELETE_PENDING and a file that no longer exists, and a guard that
// treats "cannot stat" as a refusal leaves that row stuck forever with
// nothing to move it. That is #372's shape, one machine down, and the fix
// is the same one FR-15 already uses for a remote object reconciliation
// finds already deleted: the caller's intent is satisfied, so record it
// and finish.
//
// It is only ever reachable AFTER every other clause of the guard has
// passed, including the durable proof that a verified destination copy
// exists. "The source is gone" on its own is never a reason to do
// anything.
var errSourceAlreadyGone = errors.New("placement: the source copy is already gone")

// guardSourceDelete proves that removing this move's source copy cannot
// leave the artifact without a good one, and returns exactly what to
// remove.
//
// Every path out of it that is not the last line preserves the source.
func (e *Engine) guardSourceDelete(ctx context.Context, mv state.Move, rec state.Record, want Class) (deleteTarget, error) {
	refuse := func(format string, args ...any) (deleteTarget, error) {
		return deleteTarget{}, fmt.Errorf("placement: refusing to delete %s's source copy on %q: "+format,
			append([]any{mv.Artifact, mv.SourceMedium}, args...)...)
	}

	// 1. The phase. A delete is authorised by exactly one phase, and the
	// journal is asked rather than the caller's variable.
	if Phase(mv.Phase) != SourceDeletePending {
		return refuse("the move journal says phase %q, and only %s authorises a source delete", mv.Phase, SourceDeletePending)
	}

	// 2. The artifact's own lifecycle state. FR-30 makes only COMPLETE
	// move-eligible so that FR-15's pre-delete local-file checks can never
	// observe a half-moved file; re-checking it at the delete is what
	// makes that true even if the artifact changed under a resumed move.
	if lifecycle.State(rec.State) != lifecycle.Complete {
		return refuse("the artifact is %s, and only COMPLETE artifacts may move", rec.State)
	}

	// 3. The destination, from the DURABLE journal. Not from the Result
	// deleteSource just computed: a verification that happened and a
	// verification that was durably recorded are different facts, and this
	// gate exists to require the second one.
	dst, ok := placementOn(rec, mv.DestinationMedium)
	switch {
	case !ok:
		return refuse("the journal records no placement on the destination %q, so nothing there can be relied on", mv.DestinationMedium)
	case dst.Status != state.PlacementActive:
		return refuse("the destination placement on %q is %s, not %s", mv.DestinationMedium, dst.Status, state.PlacementActive)
	case dst.Location != mv.DestinationKey:
		return refuse("the destination placement records %q, and this move copied to %q; refusing to guess which is the copy",
			dst.Location, mv.DestinationKey)
	case Class(dst.VerificationClass) != want:
		return refuse("the destination placement records verification class %q, and this medium requires %q",
			classOrUnverified(dst.VerificationClass), want)
	case dst.VerifiedAt == nil:
		return refuse("the destination placement records no verification time, so nothing says when it was checked")
	case dst.Hash == "":
		return refuse("the destination placement records no hash")
	}

	// 4. The source, also from the durable journal. DELETE_PENDING is the
	// recorded intent; anything else means the write that decided this
	// delete is not the write this row carries.
	src, ok := placementOn(rec, mv.SourceMedium)
	switch {
	case !ok:
		return refuse("the journal records no placement there any more")
	case src.Status != state.PlacementDeletePending:
		return refuse("the source placement is %s, and a delete is only ever issued against %s",
			src.Status, state.PlacementDeletePending)
	case src.Location == "":
		return refuse("the source placement records no location")
	case src.Medium == mv.DestinationMedium:
		return refuse("the source and the destination are the same medium")
	}

	// 5. The two copies must actually be the same artifact. A destination
	// whose recorded hash disagrees with the source's is not a copy of it,
	// whatever the verification class says, and the two came from
	// different writes so they can genuinely disagree.
	if !strings.EqualFold(dst.Hash, src.Hash) {
		return refuse("the destination records hash %s and the source records %s, so they are not the same bytes", dst.Hash, src.Hash)
	}

	// 6. FR-30's last question: no tier whose medium is the source still
	// selects this artifact. A nil guard is a refusal and not a pass: an
	// engine that cannot ask cannot prove the source is unwanted, and
	// uncertainty preserves the source.
	if e.Tiers == nil {
		return refuse("no retention-tier guard is configured, so nothing here can prove no tier still wants a copy on %q", mv.SourceMedium)
	}
	selected, why, err := e.Tiers.SourceStillSelected(ctx, rec, mv.SourceMedium)
	if err != nil {
		return refuse("asking whether a tier still selects it on %q failed: %v", mv.SourceMedium, err)
	}
	if selected {
		return refuse("a retention tier still selects it on %q: %s", mv.SourceMedium, why)
	}

	// 7. The medium-specific proof. It runs before the last clause because
	// it is the one that can find the source already gone, and a delete
	// that already happened has nothing left to protect: refusing it would
	// leave the journal at SOURCE_DELETE_PENDING for ever, with a source
	// row that says DELETE_PENDING about a copy that no longer exists.
	var target deleteTarget
	if src.Medium == config.MediumLocal {
		path, err := e.proveLocalSourceSafe(rec, src)
		if err != nil {
			return deleteTarget{}, err
		}
		target = deleteTarget{localPath: path}
	} else {
		medium, key, err := e.proveMediumSourceSafe(ctx, mv, src)
		if err != nil {
			return deleteTarget{}, err
		}
		target = deleteTarget{medium: medium, key: key}
	}

	// 8. FR-34's question, which none of the seven above can answer: once
	// this copy is gone, can some SURVIVING copy actually be READ right
	// now? Clause 3 proved the destination row says content-verified, and
	// that row is true; it is also true of an object that a bucket
	// lifecycle rule moved to DEEP_ARCHIVE last week, because nothing
	// rewrites verification_class when that happens. internal/archive
	// owns what a storage class does to readability, so the decision is
	// its, over copies whose access states were derived a moment ago from
	// the configuration and, for an archive class, from the medium's own
	// answer about a restore. This is the call
	// archive.TestNothingDeletesACopyWithoutAskingWhetherAnotherOneIsReadable
	// fails the build without.
	copies, err := e.copiesOf(ctx, rec)
	if err != nil {
		return refuse("%v", err)
	}
	var deleting archive.Copy
	for _, c := range copies {
		if c.Placement.Medium == src.Medium {
			deleting = c
		}
	}
	if err := archive.CheckSourceDelete(deleting, copies); err != nil {
		return refuse("%w", err)
	}
	return target, nil
}

func classOrUnverified(c string) string {
	if c == "" {
		return "unverified"
	}
	return c
}

// proveLocalSourceSafe is FR-20's discipline: canonicalize the path, prove
// it beneath the configured backup-set root, confirm it is a final managed
// artifact and never a .partial, and refuse a symlink or a traversal
// escape. It returns the fully resolved, symlink-free path.
//
// It is a second implementation of a check internal/retention already
// carries, and that is deliberate rather than an oversight. Sharing one
// would mean exporting a function whose whole value is that it is called
// immediately before a delete with the caller's own freshly re-read facts,
// and the two callers do not have the same facts: prune knows a GFS
// verdict and this one knows a move journal. What must not drift is the
// rule, and TestTheLocalSourceProofMatchesFR20 walks both and pins them
// together.
func (e *Engine) proveLocalSourceSafe(rec state.Record, src state.Placement) (string, error) {
	refuse := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("placement: refusing to delete %s's local source copy: "+format,
			append([]any{rec.Artifact}, args...)...)
	}

	bs, err := e.backupSet(rec.Artifact.Set)
	if err != nil {
		return refuse("%v", err)
	}
	switch {
	case bs.LocalPath == "":
		return refuse("backup set %s has no configured local_path", bs.ID)
	case !filepath.IsAbs(bs.LocalPath):
		return refuse("backup set %s local_path %q is not an absolute path", bs.ID, bs.LocalPath)
	}

	// The path this backup set's root and this artifact's name compute,
	// which is the only path this function will ever consider. Nothing
	// here is derived from the string being deleted.
	expected, err := localArtifactPath(bs, rec.Artifact)
	if err != nil {
		return refuse("%v", err)
	}
	if strings.HasSuffix(expected, partialSuffix) {
		return refuse("the computed path %q carries the %s marker", expected, partialSuffix)
	}
	if src.Location != expected {
		return refuse("the placement records %q, which is not %q, the path this backup set's root and artifact name compute; refusing to guess which is correct",
			src.Location, expected)
	}

	info, err := os.Lstat(expected)
	if errors.Is(err, os.ErrNotExist) {
		// Everything above has already been proved, including that a
		// verified destination copy exists and that the delete was
		// durably decided. So this is the delete having already happened,
		// which is convergence. See errSourceAlreadyGone.
		return "", errSourceAlreadyGone
	}
	if err != nil {
		return refuse("cannot stat %q: %v", expected, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return refuse("%q is a symlink, never a valid final managed artifact", expected)
	}
	if !info.Mode().IsRegular() {
		return refuse("%q is not a regular file", expected)
	}

	// Canonicalize, then contain, in that order. Comparing resolved forms
	// for exact equality is what catches a crafted artifact name whose
	// computed directory is not the root at all, and it has no notion of
	// "shares a prefix", so a sibling directory whose name merely extends
	// the root's cannot be mistaken for being inside it.
	resolvedRoot, err := filepath.EvalSymlinks(bs.LocalPath)
	if err != nil {
		return refuse("cannot canonicalize the backup-set root %q: %v", bs.LocalPath, err)
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(expected))
	if err != nil {
		return refuse("cannot canonicalize %q: %v", filepath.Dir(expected), err)
	}
	if resolvedDir != resolvedRoot {
		return refuse("the canonical directory %q is not the canonical backup-set root %q", resolvedDir, resolvedRoot)
	}

	// The size the journal recorded must still be what is on disk. This is
	// not FR-20's list; it is FR-16's identity idea applied to the local
	// end, and it is here because a local file that changed size under a
	// move is not the copy the destination was verified against.
	if src.Size != nil && info.Size() != *src.Size {
		return refuse("%q is %d bytes and the placement records %d", expected, info.Size(), *src.Size)
	}

	return filepath.Join(resolvedRoot, rec.Artifact.Name), nil
}

// proveMediumSourceSafe is the same idea for a source that lives on a
// medium: FR-16's identity re-check, which is stat the object and compare
// what it says against what the placement recorded.
//
// There is no containment proof because there is nothing to contain: a key
// is not a path, it has no parent directory and no symlinks, and inventing
// a check shaped like one would read as a safety proof while proving
// nothing. What a key CAN be is the wrong key, so the check is that the
// object at it is the size the journal recorded.
func (e *Engine) proveMediumSourceSafe(ctx context.Context, mv state.Move, src state.Placement) (transport.Medium, string, error) {
	refuse := func(format string, args ...any) (transport.Medium, string, error) {
		return transport.Medium{}, "", fmt.Errorf("placement: refusing to delete %s's source copy on %q: "+format,
			append([]any{mv.Artifact, src.Medium}, args...)...)
	}

	medium, _, err := e.resolve(src.Medium)
	if err != nil {
		return refuse("%v", err)
	}
	if e.Store == nil {
		return refuse("no medium store is configured")
	}
	info, err := e.Store.StatObject(ctx, medium, src.Location)
	if category, _ := transport.CategoryOf(err); err != nil && category == transport.NotFound {
		// The medium answered, and the object is not there. Same
		// convergence as the local half; see errSourceAlreadyGone. Note
		// this is the ONLY error shape treated this way: a medium that
		// could not be reached to ask falls through to the refusal below,
		// because a source recorded as deleted on the strength of a
		// network failure is exactly the confusion StatObject's own doc
		// warns about.
		return transport.Medium{}, "", errSourceAlreadyGone
	}
	if err != nil {
		// Deliberately not treated as "already gone". A medium that cannot
		// be reached to ask and a medium that answered "not there" are
		// different facts, and confusing them is how a source is recorded
		// as deleted on the strength of a network failure.
		return refuse("the medium could not be asked about %q: %v", src.Location, err)
	}
	if src.Size != nil && info.Size != *src.Size {
		return refuse("the object at %q is %d bytes and the placement records %d", src.Location, info.Size, *src.Size)
	}
	return medium, src.Location, nil
}

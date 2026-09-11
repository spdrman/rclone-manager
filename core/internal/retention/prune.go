// This file implements FR-20: local deletion safety and its mandatory
// dry-run. gfs.go (FR-18) and lastknowngood.go (FR-19) decide what to
// keep, and neither one deletes anything, on purpose. This file is where
// deleting finally happens, and it is the second most dangerous piece of
// code in this project. lifecycle/remotedelete.go (FR-15) is the first,
// and its own package doc says outright that it "destroys data" and is
// "the most dangerous line in the project on purpose." This file destroys
// something arguably worse: the local copies FR-20 protects are the
// restore points a remote incident is supposed to be recoverable from.
// Deleting the wrong one here does not risk a remote object; it risks the
// backup itself.
//
// # The six checks, and why they are independent rather than one flag
//
// FR-20's own text asks for six things to hold before a delete: the path
// is canonicalized, it is proven beneath the configured backup-set root,
// it is confirmed a final managed artifact (never a .partial), no
// retention tier selects it, it is not last-known-good, and a symlink or
// path-traversal escape is rejected. pruneVerifySafeToDelete below
// implements every one of these as its own check, in that rough order,
// deliberately re-deriving each fact from the artifact's own record and
// this backup set's own configured root rather than trusting a caller's
// upstream filtering (DecideKeep already filters out everything not
// GFS-eligible before a candidate ever reaches this file). That
// duplication is intentional: see lifecycle/remotedelete.go's own package
// doc for the same philosophy applied to FR-15's four revalidations,
// "never trusting that an earlier pass already checked them." A safety
// check worth having is worth re-running at the point of the dangerous
// action, not just upstream of it, and this file's own tests
// (TestPruneRefusesPartialArtifactEvenWhenToldToDeleteIt) exist precisely
// to prove that redundancy actually holds, not just to assert it does.
//
// # Canonicalize, then contain: the pair that actually matters
//
// The most dangerous single mistake this file could make is checking
// containment against an *unresolved* path. A symlink sitting inside the
// backup root, but pointing outside it, makes a naive
// strings.HasPrefix(path, root) check pass even though the real file the
// path resolves to is nowhere near root. This file instead treats any
// symlink at the artifact's own final path as disqualifying on its own
// (pruneVerifySafeToDelete's Lstat check, below): Commit
// (lifecycle/commit.go) only ever produces a final-name file via a hard
// link followed by removing the .partial name, never a symlink, so
// finding one there is already an anomaly outside every invariant this
// project's own pipeline maintains. Resolving it and deleting whatever it
// points to, even if that target happens to sit inside the configured
// root, would mean deleting a file this check never independently
// identified as the artifact in question at all: the journal knows this
// artifact's identity, not the symlink's target's.
//
// Separately, model.ArtifactID.Name is only ever guaranteed to be a bare
// basename (no "/", no "..") when it was built through
// model.NewArtifactID. A record whose ArtifactID was not (a hand-edited
// journal row, a future scanRecord bug, schema drift) can carry a name
// like "../secret.txt", and pruneFinalPath's plain filepath.Join then
// computes a path whose *directory* is no longer the configured root at
// all, not merely a differently-resolved version of it. Resolving both
// the configured root and that computed directory with
// filepath.EvalSymlinks and comparing the two *resolved* forms for exact
// equality catches this (and, incidentally, any genuine symlinked
// ancestor directory too) without reintroducing the naive prefix bug:
// exact equality has no notion of "shares a prefix," so a sibling
// directory whose name happens to extend the root's own name
// (.../backups/setA vs .../backups/setA-evil) can never be mistaken for
// being inside root, which a bare strings.HasPrefix comparison would get
// wrong. See TestPruneRejectsPathTraversalViaCraftedArtifactName and
// TestPruneRejectsSiblingPrefixDirectory.
//
// Note what this check is *not* defending against: a well-formed,
// basename-only artifact name can never itself produce a computed
// directory different from the configured root, symlinks or not, because
// filepath.Dir(filepath.Join(root, basename)) is always exactly
// filepath.Clean(root). A backup root that is itself a symlink to real
// storage (a normal NAS/mount pattern) is therefore never refused by this
// check on that basis alone: see TestPruneAllowsDeletionWhenBackupRootItselfIsASymlink.
//
// # One decision path for the dry-run and the real run
//
// PruneDecide computes, and only computes, the KEEP/DELETE/REFUSE verdict
// for every artifact FR-18/FR-19 have an opinion about in one backup set.
// It performs no mutation: `backupd retention --dry-run` (owned by
// issue #25/#26, elsewhere) calls this and only this to render its
// explanation, which is FR-20's other mandatory half, "a retention engine
// you cannot interrogate is one you cannot trust" per the EPIC.
//
// PruneApply is the only function in this file that deletes anything, and
// its very first step is calling PruneDecide with the exact same
// arguments a dry-run would use. There is no second implementation of the
// decision logic anywhere in this file for PruneApply to disagree with:
// it can only ever act on a PruneDelete verdict PruneDecide itself
// produced. What PruneApply adds beyond PruneDecide is a second call to
// pruneVerifySafeToDelete, immediately before the one irreversible
// os.Remove in this file, against whatever is actually on disk at that
// moment rather than what PruneDecide observed earlier in the same pass.
// That is not a second opinion; it is the identical predicate, evaluated
// again because time (and possibly a concurrent process) passed between
// the decision and the act. See TestPruneDryRunAndApplyAgree, which
// proves the two calls produce identical verdicts for the same
// unmodified input, and PruneApply's own doc comment for the residual
// TOCTOU window this cannot close.
//
// # What this file does not do
//
// It never lists a directory to discover what to delete. Every candidate
// this file ever considers arrives as a state.Record the caller already
// read from the FR-9 journal; a file sitting in a backup set's local
// directory with no corresponding record is never inspected, matched, or
// touched, by construction (see TestPruneNeverConsidersAFileTheJournalDoesNotKnowAbout).
// "Positively identified database-managed files" in FR-20's own words is
// exactly this: only a journal record can identify a file to this file at
// all.
package retention

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/artifactstore"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// prunePartialSuffix mirrors lifecycle/transfer.go's own unexported
// partialSuffix constant (FR-12's non-restorable, in-flight file marker).
// This package cannot import that unexported symbol, and this change's
// own file scope does not extend to lifecycle, so the literal is
// deliberately duplicated here rather than exported from lifecycle just
// for this one check. TestPruneRefusesPartialArtifactEvenWhenToldToDeleteIt
// pins the real-world shape of this marker (a Transferring-state record's
// own recorded .partial path), so a future rename of one without the
// other fails a test instead of silently drifting apart.
const prunePartialSuffix = ".partial"

// PruneAction is pruneEvaluate's (and PruneDecide's) verdict for one
// artifact: what should happen, or did happen, to its local file.
type PruneAction string

const (
	// PruneKeep means at least one GFS tier or last-known-good protection
	// selected this artifact: its local file is never touched.
	PruneKeep PruneAction = "KEEP"

	// PruneDelete means no retention tier and no last-known-good
	// protection selects this artifact, and every one of FR-20's safety
	// checks passed against its recorded local path. From PruneDecide,
	// this means the file *would* be deleted by PruneApply. From
	// PruneApply, it means the file *was* deleted.
	PruneDelete PruneAction = "DELETE"

	// PruneRefuse means this artifact was a candidate for deletion (no
	// tier keeps it) but at least one FR-20 safety check did not pass, or
	// (only ever from PruneApply) the delete itself failed. The local
	// file is never touched when this is the verdict. This is
	// deliberately a third outcome, distinct from PruneKeep: FR-20 exists
	// precisely so "policy says delete, but it isn't safe to" and "policy
	// says keep" are never collapsed into the same, less alarming,
	// outcome. See this file's package doc.
	PruneRefuse PruneAction = "REFUSE"
)

// PruneVerdict is FR-20's fully explained answer for one artifact: what
// happened (or would happen) to its local file, and why, in language an
// operator reading `backupd retention --dry-run` output can act
// on without reading this package's source.
type PruneVerdict struct {
	Artifact model.ArtifactID
	Action   PruneAction

	// Path is the artifact's final local path, computed the same way
	// lifecycle's own Commit computes it (this backup set's LocalPath
	// joined with the artifact's basename). It is populated whenever this
	// verdict is about a LOCAL copy, regardless of Action, so a REFUSE
	// verdict still names exactly which file was refused.
	//
	// It is empty when Medium names something other than local. An
	// artifact whose durable copy is an object has no local file this
	// verdict is about, and rendering the path it WOULD have had would
	// name a file that is not there and was never considered.
	Path string

	// Medium is where the copy this verdict is about lives (EPIC E FR-30,
	// issue #239): config.MediumLocal, or the id of a configured storage
	// medium. It is empty on exactly two verdicts, both REFUSE: a
	// contested location (more than one ACTIVE placement, a move in
	// flight), where there are genuinely two answers and this verdict
	// declines to pick one, and an artifact whose final path could not be
	// resolved at all, where nothing was established about it. A DELETE
	// always names its medium. See localBranchMedium.
	//
	// FR-30 asks the mandatory dry-run to explain per-artifact WHERE a
	// deletion would happen, not only whether, and this is that answer.
	// It matters most on the surface an operator reads before confirming:
	// "delete 40 artifacts" means something very different when half of
	// them are objects in a bucket somebody else pays for.
	Medium string

	// Tiers lists every GFS tier, and/or TierLastKnownGood, that kept this
	// artifact, each paired with which of FR-18's two placements selected
	// it there (issue #218). Populated only when Action is PruneKeep; nil
	// otherwise. This is copied from the composed DecideKeep verdict,
	// never recomputed, so it can never disagree with it.
	Tiers []GFSTierSelection

	// Reason is a one-sentence, human-readable explanation: which tier
	// kept it, or a plain statement that nothing did and every safety
	// check passed, or exactly which safety check refused it. This is the
	// dry-run's actual deliverable, not a debug aid: see this file's
	// package doc and FR-20's own text, "a retention engine you cannot
	// interrogate is one you cannot trust."
	Reason string

	// HoldReason is set, on a PruneRefuse and never on anything else,
	// when this verdict was refused because the whole backup set was
	// held: the restore point FR-19 reports as protected has no confirmed
	// readable copy (FR-30, issue #602). It names what could not be
	// confirmed, and Reason carries the same sentence wrapped in the
	// refusal.
	//
	// It is a field rather than something a caller pattern-matches out of
	// Reason because the two say different things. Every other REFUSE in
	// this package is a routine outcome of a plan, decided per artifact
	// and read where plans are read. This one is a standing fact about
	// the SET which stops retention for it entirely until somebody
	// reconciles the inventory, and the layer above has to be able to
	// tell those apart to raise the second as a condition without raising
	// the first (see internal/app's PruneApplySnapshot and
	// BuildHealthReport). Matching on a sentence would make an operator's
	// wording change into an alerting change.
	HoldReason string
}

// LocationStatus is how well an ArtifactLocator could answer "where is
// this artifact's durable copy".
//
// There are three answers, not two, and collapsing the last two is a bug
// this issue shipped once: "the journal says nothing about this artifact"
// and "the journal says two contradictory things" are both failures to
// name a single medium, and they are not the same claim. One is the
// ordinary state of every artifact written before FR-29's placement table
// existed. The other is a positive assertion that a move is operating on
// this artifact's copies right now.
type LocationStatus string

const (
	// LocationConfirmed: exactly one ACTIVE placement. Medium names it.
	LocationConfirmed LocationStatus = "CONFIRMED"

	// LocationUnrecorded: no ACTIVE placement row at all. That is the
	// pre-EPIC-E artifact, the hand-built Record, and the artifact still
	// transferring, and it takes nothing away from FR-20, whose proof was
	// never a placement row: it is a canonicalized path, proven beneath
	// the configured root, re-derived at the moment of the delete.
	//
	// It is emphatically NOT permission for the MOVE ENGINE, whose own
	// standing invariant (FR-30, and the spec's "no copy confirmed, never
	// no copy needed") is about deleting a source copy it never proved.
	// The two rules differ because the proofs available to them differ,
	// not because one of them is careless.
	LocationUnrecorded LocationStatus = "UNRECORDED"

	// LocationContested: more than one ACTIVE placement, which is FR-30's
	// copy phase in flight. There are two answers to "where is this", and
	// removing either copy is the race FR-30's journal exists to make
	// unrepresentable.
	LocationContested LocationStatus = "CONTESTED"
)

// Location is one artifact's answer from an ArtifactLocator.
type Location struct {
	// Medium is config.MediumLocal or a configured medium id, and is
	// meaningful only when Status is LocationConfirmed. It is empty
	// otherwise, deliberately, so a caller that ignores Status reads an
	// empty string rather than a plausible-looking medium id.
	Medium string

	Status LocationStatus
}

// OnMedium reports whether this location is confirmed to be somewhere
// other than the implicit local medium, which is the one question that
// sends a prune down a different set of checks.
func (l Location) OnMedium() bool {
	return l.Status == LocationConfirmed && l.Medium != config.MediumLocal
}

// ArtifactLocator answers where one artifact's durable copy is right now.
//
// The status is the load-bearing part, and it means "I could confirm
// this", never "it is there". A placement row records a DURABLE copy, so
// an artifact mid-move has two ACTIVE rows and an artifact still
// transferring has none. PlanHomeMoves takes the same shape for the same
// reason; see its doc, and internal/app.ActiveMediumFromRecords for the
// reading.
//
// It is a function rather than a placement lookup because internal/
// retention may not read a placement row at all. FR-32 says nothing a
// medium reported may reach a retention decision, and this package holds
// that structurally: there is no medium-supplied value in scope here for a
// future change to reach for, and placement.TestRetentionReadsNoMedium
// SuppliedValue fails the build if one appears. The adapter that BUILDS
// this function reads the rows, one package up, where it is not a
// retention decision.
type ArtifactLocator func(model.ArtifactID) Location

// AllLocal is the locator for a deployment with no storage mediums: every
// durable copy is a local file, which is exactly what every artifact in
// every deployment written before EPIC E is.
//
// It exists as a named value rather than as a nil-means-local default,
// because a nil locator on a delete path would have to mean something and
// every meaning available is a guess about where a copy of a backup lives.
// Written out, the assumption is greppable and a caller that should not be
// making it is visible.
func AllLocal(model.ArtifactID) Location {
	return Location{Medium: config.MediumLocal, Status: LocationConfirmed}
}

// MediumPruner removes an artifact's copy from a storage medium, having
// first re-proved that the object there is the one the journal recorded.
//
// FR-30 spells the obligation: "an FR-16-style identity re-check (stat the
// object; compare size and, where available, checksum against the
// placement record; refuse on mismatch and require reconciliation)". None
// of that can happen in this package, which is the point of the seam: the
// re-check reads a placement row and a transport.ObjectInfo, and FR-32
// keeps both out of here. internal/placement.Reclaimer is the
// implementation.
//
// The record is passed whole because the implementation needs the
// placement row behind it, and because the re-check must derive its facts
// from the journal at the moment of the delete rather than from anything
// this package already decided. A non-nil error is a refusal and the
// object is untouched.
type MediumPruner interface {
	DeleteFromMedium(ctx context.Context, rec state.Record, medium string) error
}

// pruneFinalPath computes the same final local path lifecycle's own
// unexported finalPath (transfer.go) computes for the same (LocalDir,
// Artifact) pair: the artifact's basename, under the backup set's
// configured local directory.
//
// It used to duplicate that join, with a comment here and another there
// naming the two of them as the only places in the project allowed to
// compute it. Both now ASK THE STORE, which is issue #334's deferred
// conversion: internal/artifactstore landed the Store seam with no
// production caller so the contract could be argued before anything
// depended on it, and its package doc named this function and lifecycle's
// finalPath as the two that would convert. Neither composes a path out of
// LocalPath any more, so neither can drift from the other or from the
// store that actually owns the answer.
//
// Those two, not every join of a root and an artifact name in this file:
// pruneVerifySafeToDelete ends by joining that name onto the canonicalized
// root EvalSymlinks handed back, which is deliberately a different
// computation on a different input. See its own comment there.
//
// The error is what the conversion added. NewLocal refuses an empty root
// rather than resolving under the process working directory, and Locator
// refuses an artifact it cannot address. Both are refusals this function
// could not previously make, and on a delete path a refusal is the answer
// that costs nothing: see pruneEvaluate, which turns one into PruneRefuse.
func pruneFinalPath(bs config.BackupSet, artifact model.ArtifactID) (string, error) {
	store, err := artifactstore.NewLocal(bs.LocalPath)
	if err != nil {
		return "", fmt.Errorf("retention: prune: backup set %s: %w", bs.ID, err)
	}
	return store.Locator(artifact)
}

// pruneVerifySafeToDelete runs every one of FR-20's checks against one
// managed-complete artifact and returns the exact, symlink-free, fully
// canonicalized path that is safe to remove, or an error naming precisely
// which check refused it. It touches the filesystem only to read (Lstat,
// EvalSymlinks): it never creates, modifies, or removes anything, so it is
// safe to call as many times as a caller likes, including the second call
// PruneApply makes immediately before its one os.Remove.
//
// rec is expected to be the journal's own record for the artifact FR-18's
// GFSDecide already classified as not kept by any tier; this function does
// not consult GFSVerdict or LastKnownGoodResult itself; the caller
// (pruneEvaluate) already checked those before ever calling this, and
// checks here again anyway that the record's own state is a final managed
// artifact, never a .partial, because that specific guarantee is FR-20's
// own, separate from what tier math decided (see the package doc's "The
// six checks" section).
func pruneVerifySafeToDelete(bs config.BackupSet, rec state.Record) (string, error) {
	if bs.LocalPath == "" {
		return "", fmt.Errorf("retention: prune: backup set %s has no configured local_path", bs.ID)
	}
	if !filepath.IsAbs(bs.LocalPath) {
		return "", fmt.Errorf("retention: prune: backup set %s local_path %q must be an absolute path", bs.ID, bs.LocalPath)
	}

	// Check: a final managed artifact, never a .partial. Re-derived from
	// rec.State itself via the exact same gfsIsManagedComplete gfs.go and
	// lastknowngood.go already use for "eligible" (see this package's
	// GFSDecide doc), rather than trusted from whatever upstream filtering
	// already happened. See TestPruneRefusesPartialArtifactEvenWhenToldToDeleteIt.
	if !gfsIsManagedComplete(rec.State) {
		return "", fmt.Errorf(
			"retention: prune: refusing %s: journal state %q is not a final managed artifact (must be %s)",
			rec.Artifact, rec.State, gfsManagedCompleteNames())
	}

	expected, err := pruneFinalPath(bs, rec.Artifact)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(expected, prunePartialSuffix) {
		// Unreachable given model.NewArtifactID's own basename validation
		// (an artifact name can never contain the separators that would
		// let a caller smuggle this suffix in), kept anyway as its own
		// explicit, named check: FR-20 asks for "never a .partial" to be
		// its own guarantee, not merely an implication of the state check
		// above.
		return "", fmt.Errorf("retention: prune: refusing %s: computed path %q carries the %s marker", rec.Artifact, expected, prunePartialSuffix)
	}

	// Check: the journal's own recorded local path must exactly match what
	// this backup set's root and this artifact's name compute.
	// "Positively identified" means the file about to be touched is the
	// one the journal actually knows this artifact as; a mismatch here
	// (whatever produced it: a stale record, a hand-edited row, a bug
	// upstream) is refused outright rather than guessed at, exactly like
	// lifecycle's own Commit and DeleteRemote refuse rather than guess
	// when a recorded path disagrees with a computed one. This is also
	// what stands between this function and a "file the journal does not
	// know about": nothing here ever considers a path that was not the
	// journal's own recorded LocalPath.
	if rec.LocalPath != expected {
		return "", fmt.Errorf(
			"retention: prune: refusing %s: journal records local path %q, which does not match %q, the path this backup set's root and artifact name compute; refusing to guess which is correct",
			rec.Artifact, rec.LocalPath, expected)
	}

	// Check: the final path must be a genuine regular file, never a
	// symlink. lifecycle's Commit only ever produces the final name via a
	// hard link followed by removing the .partial name; it has no code
	// path that creates a symlink there. Finding one is already an
	// anomaly outside every invariant this project's own pipeline
	// maintains, so it is refused outright rather than resolved and
	// conditionally allowed: resolving it and deleting whatever it points
	// to, even when that target nominally sits inside the backup root,
	// would mean deleting a file this check never independently
	// identified as this artifact at all. See TestPruneRefusesSymlinkAtFinalPath.
	info, err := os.Lstat(expected)
	if err != nil {
		return "", fmt.Errorf("retention: prune: refusing %s: cannot stat %q: %w", rec.Artifact, expected, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("retention: prune: refusing %s: %q is a symlink, never a valid final managed artifact", rec.Artifact, expected)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("retention: prune: refusing %s: %q is not a regular file", rec.Artifact, expected)
	}

	// Check: canonicalize, then prove containment on the canonical form,
	// in that order. Resolving the configured root and the artifact's own
	// computed containing directory, before ever comparing them, is what
	// catches a malformed or compromised ArtifactID.Name (one that never
	// went through model.NewArtifactID) computing a directory that is not
	// really the configured root at all: see the package doc's
	// "Canonicalize, then contain" section,
	// TestPruneRejectsPathTraversalViaCraftedArtifactName and
	// TestPruneRejectsSiblingPrefixDirectory (a plain, unresolved
	// strings.HasPrefix comparison would wrongly accept a sibling
	// directory whose name merely extends the root's own name). A
	// well-formed, basename-only name can never trip this check, symlinked
	// root or not: see TestPruneAllowsDeletionWhenBackupRootItselfIsASymlink.
	resolvedRoot, err := filepath.EvalSymlinks(bs.LocalPath)
	if err != nil {
		return "", fmt.Errorf("retention: prune: refusing %s: cannot canonicalize backup-set root %q: %w", rec.Artifact, bs.LocalPath, err)
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(expected))
	if err != nil {
		return "", fmt.Errorf("retention: prune: refusing %s: cannot canonicalize %q: %w", rec.Artifact, filepath.Dir(expected), err)
	}
	if resolvedDir != resolvedRoot {
		return "", fmt.Errorf(
			"retention: prune: refusing %s: canonical directory %q is not the canonical backup-set root %q",
			rec.Artifact, resolvedDir, resolvedRoot)
	}

	// expected's own final component was already proven, above, to be a
	// real (non-symlink) directory entry directly inside resolvedDir, so
	// joining the canonical directory back onto the artifact's own name
	// is the fully canonical, safe-to-remove path: no further resolution
	// of the final component is needed or wanted.
	//
	// This is deliberately NOT the store's own Locator, even though it has
	// the same shape. That method answers "where does the configured root
	// say this artifact goes"; this line answers "what is the
	// resolved, symlink-free path this function just proved safe", and
	// routing it back through the configured root would throw away the
	// resolution the checks above exist to produce. Same shape, different
	// question, and this one fails closed.
	return filepath.Join(resolvedRoot, rec.Artifact.Name), nil
}

// pruneEvaluate is PruneDecide's per-artifact decision: given the composed
// GFS/last-known-good verdict DecideKeep already produced for this
// artifact, decide KEEP, DELETE or REFUSE.
func pruneEvaluate(bs config.BackupSet, rec state.Record, keepVerdict GFSVerdict, lkg LastKnownGoodResult, where ArtifactLocator) PruneVerdict {
	loc := where(rec.Artifact)

	// An artifact whose durable copy is an object takes a different set of
	// checks, because FR-20's list is about a PATH: canonicalization,
	// containment beneath a root, symlinks, traversal. A key has none of
	// those, and inventing checks shaped like them would read as a safety
	// proof while proving nothing (internal/placement's own
	// proveMediumSourceSafe says the same thing about the same question).
	// What survives the move to a medium is FR-19, FR-18's own verdict,
	// the "final managed artifact, never a .partial" guarantee, and the
	// identity re-check, and those are what the branch below runs.
	//
	// Everything else, an unrecorded location included, takes FR-20's own
	// path. See LocationUnrecorded for why the absence of a placement row
	// takes nothing away from a proof that was never made of one.
	if loc.OnMedium() {
		return pruneEvaluateOnMedium(rec, keepVerdict, lkg, loc.Medium)
	}

	path, err := pruneFinalPath(bs, rec.Artifact)
	if err != nil {
		// A store that cannot say where this artifact belongs is a store
		// this function must not guess on behalf of. REFUSE rather than
		// KEEP, because the two are different claims: KEEP asserts a tier
		// selected it, and this asserts nothing was decided at all. Path
		// stays empty on purpose, so nothing downstream can act on a
		// half-computed one.
		//
		// This runs before the Keep check below, so a kept artifact
		// refuses here too. That is the ordering the claim demands: a
		// resolution failure never comes out as KEEP, not even when a
		// tier would have selected the artifact, because KEEP would then
		// be reporting a tier decision about a file nothing can locate.
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneRefuse,
			Reason:   fmt.Sprintf("refusing to decide about %s: %v", rec.Artifact, err),
		}
	}

	if keepVerdict.Keep {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneKeep,
			Path:     path,
			Medium:   localBranchMedium(loc),
			Tiers:    append([]GFSTierSelection(nil), keepVerdict.Tiers...),
			Reason:   pruneKeepReason(keepVerdict.Tiers),
		}
	}

	// Defensive only, and should be unreachable: ApplyLastKnownGood always
	// flips Keep to true for whichever artifact lkg.Artifact names
	// whenever lkg.Protected is true (see that function's own doc), so
	// keepVerdict.Keep is already false here only when this artifact truly
	// holds no last-known-good protection either. This is checked again
	// anyway, independently of the Keep flag above, because FR-20 names
	// "confirm it is not last-known-good" as its own guarantee: a caller
	// that passed a keepVerdict computed from different records or a
	// different lkg than the ones actually describing rec should never be
	// able to make this function agree to delete FR-19's protected
	// restore point by accident. See lastknowngood.go's ApplyLastKnownGood
	// for the identical "defensive only" reasoning applied to composition
	// instead of deletion.
	if lkg.Protected && lkg.Artifact == rec.Artifact {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneRefuse,
			Path:     path,
			Medium:   localBranchMedium(loc),
			Reason: fmt.Sprintf(
				"refusing to delete %s: it holds FR-19 last-known-good protection, but the GFS verdict passed in claims Keep=false; this contradiction means mismatched inputs were passed to this decision, not a real delete candidate",
				rec.Artifact),
		}
	}

	// Not kept, not protected, and the journal holds MORE than one ACTIVE
	// placement for this artifact. That is FR-30's copy phase in flight,
	// so something else is operating on this artifact's copies right now
	// and removing one of them is the race FR-30's journal exists to make
	// unrepresentable. REFUSE, which is a different claim from KEEP and is
	// what makes the collision visible instead of quiet.
	//
	// It is checked here, after the KEEP branch, rather than first,
	// because a contested placement is a reason not to DELETE and never a
	// reason to report a tier's KEEP as something this pass refused. An
	// artifact a tier selects is not being deleted, so there is nothing
	// for the in-flight move to collide with.
	//
	// LocationUnrecorded deliberately does not land here: see its own doc.
	if loc.Status == LocationContested {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneRefuse,
			Path:     path,
			Reason: fmt.Sprintf(
				"refusing to delete %s: the journal records more than one ACTIVE placement for it, which is a move in flight; deleting a copy out from under a move is the race FR-30's journal exists to make unrepresentable",
				rec.Artifact),
		}
	}

	safePath, err := pruneVerifySafeToDelete(bs, rec)
	if err != nil {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneRefuse,
			Path:     path,
			Medium:   localBranchMedium(loc),
			Reason:   err.Error(),
		}
	}

	return PruneVerdict{
		Artifact: rec.Artifact,
		Action:   PruneDelete,
		Path:     safePath,
		Medium:   localBranchMedium(loc),
		Reason:   pruneDeleteReason(keepVerdict),
	}
}

// localBranchMedium is what a verdict taken on FR-20's local path names as
// its medium.
//
// It is config.MediumLocal for a confirmed local placement and for an
// unrecorded one alike, and that is not a guess: this branch's verdict is
// about the file at Path, under the backup set's own local_path, and
// config.MediumLocal is exactly what that place is called. An empty string
// here would say "nowhere" about a file this function just canonicalized.
//
// A contested location gets an empty medium, because there really are two
// answers and this verdict declines to pick one. That verdict is always a
// KEEP or a REFUSE (the DELETE branch above refuses a contested location
// outright), so the empty string never travels beside a deletion.
func localBranchMedium(loc Location) string {
	if loc.Status == LocationContested {
		return ""
	}
	return config.MediumLocal
}

// pruneEvaluateOnMedium is pruneEvaluate for an artifact whose durable
// copy is an object on a storage medium (EPIC E FR-30, issue #239).
//
// It decides; it never deletes, exactly like the local branch. The actual
// removal, and the FR-16 identity re-check that has to precede it, happen
// in PruneApply through a MediumPruner, which is the only thing in this
// product allowed to look at what the medium says.
//
// # What it re-derives rather than trusts
//
// The same two things the local branch re-derives at the point of the
// dangerous action: that no tier and no last-known-good protection selects
// the artifact, and that the record is a final managed artifact rather
// than something still in flight. DecideKeep already filtered on both
// before a candidate reached here, and both are checked again anyway, for
// the reason lifecycle/remotedelete.go states for its own four
// revalidations: a safety check worth having is worth re-running at the
// point of the dangerous action, not just upstream of it.
//
// There is no containment or symlink check, and their absence is a
// decision rather than an omission. Those checks answer "is this path the
// one this backup set owns", and a key is not a path: it has no parent
// directory, no symlink to follow and no ".." to escape through. The
// question a key CAN get wrong is "is the object at it still the one the
// journal recorded", and that is FR-16's, answered immediately before the
// delete by the MediumPruner rather than here, where it would be answered
// against a medium's state that can change before anything acts on it.
func pruneEvaluateOnMedium(rec state.Record, keepVerdict GFSVerdict, lkg LastKnownGoodResult, medium string) PruneVerdict {
	if keepVerdict.Keep {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneKeep,
			Medium:   medium,
			Tiers:    append([]GFSTierSelection(nil), keepVerdict.Tiers...),
			Reason:   pruneKeepReason(keepVerdict.Tiers),
		}
	}

	if lkg.Protected && lkg.Artifact == rec.Artifact {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneRefuse,
			Medium:   medium,
			Reason: fmt.Sprintf(
				"refusing to delete %s from %q: it holds FR-19 last-known-good protection, but the GFS verdict passed in claims Keep=false; this contradiction means mismatched inputs were passed to this decision, not a real delete candidate",
				rec.Artifact, medium),
		}
	}

	if !gfsIsManagedComplete(rec.State) {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneRefuse,
			Medium:   medium,
			Reason: fmt.Sprintf(
				"retention: prune: refusing %s on %q: journal state %q is not a final managed artifact (must be %s)",
				rec.Artifact, medium, rec.State, gfsManagedCompleteNames()),
		}
	}
	if strings.HasSuffix(rec.Artifact.Name, prunePartialSuffix) {
		// Unreachable through model.NewArtifactID, kept for the reason the
		// local branch keeps its own copy of this check: FR-20 asks for
		// "never a .partial" as its own guarantee rather than as an
		// implication of the state check above.
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneRefuse,
			Medium:   medium,
			Reason:   fmt.Sprintf("retention: prune: refusing %s on %q: its name carries the %s marker", rec.Artifact, medium, prunePartialSuffix),
		}
	}

	return PruneVerdict{
		Artifact: rec.Artifact,
		Action:   PruneDelete,
		Medium:   medium,
		Reason: fmt.Sprintf("%s; its durable copy is an object on %q, which is where the deletion would happen, "+
			"after re-checking immediately beforehand that the object there is still the one this journal recorded",
			pruneDeleteReason(keepVerdict), medium),
	}
}

// pruneDeleteReason renders a PruneDelete verdict's Reason: FR-20's usual
// sentence, plus, when keepVerdict carries any (issue #292), a warning
// naming every sibling this artifact tied on an identical timestamp with
// and lost only the deterministic tie-break to -- not the safety checks
// pruneVerifySafeToDelete already passed, and not this function's own
// KEEP/DELETE decision, which is unchanged either way (see this issue's
// own scope decision: the split itself is not what this function refuses,
// only its silence). This is what carries issue #292's signal to
// PruneApply's actual, HTTP-reachable delete path
// (core/service/retention.go's ApplyRetentionPlan, gated on an
// administrator having reviewed exactly this Reason text in a prior
// preview), the same way GFSVerdict.SiblingCollisionLines carries it to
// `retention --dry-run`.
func pruneDeleteReason(keepVerdict GFSVerdict) string {
	base := "no configured GFS retention tier selects this artifact and it does not hold last-known-good protection; " +
		"its canonical path was confirmed beneath the backup-set root, confirmed a final managed artifact, and confirmed not a symlink"
	lines := keepVerdict.SiblingCollisionLines()
	if len(lines) == 0 {
		return base
	}
	return base + "; " + strings.Join(lines, "; ")
}

// pruneKeepReason renders the tiers that kept an artifact into the
// sentence PruneVerdict.Reason carries for a PruneKeep verdict: FR-20
// asks that the reason "name the tier that kept it," not just report a
// bare KEEP.
//
// Each tier is rendered with the placement that selected it (issue #218),
// because naming the tier alone leaves an operator unable to tell a KEEP
// this manager's own clock produced from one that rests on a timestamp
// FR-8 says to distrust. GFSTierSelection.String is what spells it, so
// this sentence and the CLI's own per-artifact line cannot come to
// disagree about how a placement is written.
func pruneKeepReason(tiers []GFSTierSelection) string {
	if len(tiers) == 0 {
		// Cannot happen: DecideKeep never returns Keep == true with an
		// empty Tiers slice. Guarded anyway rather than let a reason
		// silently claim nothing kept an artifact that was, in fact, kept.
		return "kept, but no tier is recorded against it; this should not happen and is worth investigating as a bug"
	}
	names := make([]string, len(tiers))
	for i, t := range tiers {
		names[i] = t.String()
	}
	word := "tier"
	if len(names) > 1 {
		word = "tiers"
	}
	return fmt.Sprintf("kept by the %s %s", strings.Join(names, ", "), word)
}

// PruneDecide computes FR-20's KEEP/DELETE/REFUSE verdict for every
// artifact FR-18's GFSDecide and FR-19's LastKnownGoodDecide have an
// opinion about in one backup set. It performs no mutation whatsoever: it
// is exactly, and only, the function `backupd retention --dry-run`
// (issue #25/#26) calls to render its mandatory explanation, and it is
// also the first step PruneApply takes, so a real run's decisions can
// never be computed by a second, potentially divergent, code path. See
// this file's package doc.
//
// now, cfg, set and records mean exactly what they mean for DecideKeep
// (gfs.go/lastknowngood.go), which this function calls to obtain the
// composed KEEP union; bs additionally supplies the configured local
// directory FR-20's containment check needs, and must describe the same
// backup set as set == bs.ID.
//
// where says, per artifact, which medium its durable copy is on (EPIC E
// FR-30, issue #239). It decides which set of safety checks a delete
// candidate faces: FR-20's path discipline for a local file, and FR-16's
// identity re-check for an object. Pass AllLocal for a deployment with no
// storage mediums, which is exactly the behaviour every caller had before
// this parameter existed. It has no default, because the only defaults
// available are guesses about where a copy of a backup lives.
//
// Artifacts outside GFS's own scope (still in flight, or in an
// exceptional non-recoverable state: see GFSDecide's doc on
// gfsManagedCompleteStates) never receive a verdict here, exactly as they
// never receive one from GFSDecide: there is nothing for retention to
// decide about a backup that has not yet succeeded. The returned slice is
// sorted by artifact name, same as GFSDecide's own, so two calls over the
// same inputs render identically.
func PruneDecide(now time.Time, cfg config.Retention, bs config.BackupSet, records []state.Record, where ArtifactLocator) ([]PruneVerdict, error) {
	if bs.ID.IsZero() {
		return nil, fmt.Errorf("retention: PruneDecide needs a non-zero backup set id")
	}
	if where == nil {
		return nil, fmt.Errorf("retention: PruneDecide needs a way to say where each artifact's durable copy is; " +
			"pass AllLocal for a deployment with no storage mediums, and internal/app.ActiveMediumFromRecords otherwise")
	}

	verdicts, lkg, err := DecideKeep(now, cfg, bs.ID, records)
	if err != nil {
		return nil, fmt.Errorf("retention: prune: %w", err)
	}

	recByArtifact := make(map[model.ArtifactID]state.Record, len(records))
	for _, rec := range records {
		recByArtifact[rec.Artifact] = rec
	}

	out := make([]PruneVerdict, 0, len(verdicts))
	for _, v := range verdicts {
		rec, ok := recByArtifact[v.Artifact]
		if !ok {
			// Cannot happen: every v.Artifact GFSDecide returns came from
			// records in the first place. Guarded rather than silently
			// evaluating a zero-value Record that was never real input.
			return nil, fmt.Errorf("retention: prune: internal inconsistency: verdict for %s has no matching record", v.Artifact)
		}
		out = append(out, pruneEvaluate(bs, rec, v, lkg, where))
	}
	pruneHoldEveryDelete(out, pruneLastKnownGoodUnconfirmed(bs, recByArtifact, lkg, where))
	sortPruneVerdicts(out)
	return out, nil
}

// LastKnownGoodUnconfirmed answers FR-30's question about one backup set
// without deciding, planning or removing anything: is there actually a
// readable copy of the restore point FR-19 is protecting?
//
// It returns the empty string when there is, and the one sentence
// pruneLastKnownGoodUnconfirmed composes when there is not. The arguments
// mean exactly what PruneDecide's do, and it runs the same DecideKeep pass
// PruneDecide would, so the answer cannot come from a different reading of
// the journal than the one a plan is drawn from.
//
// This exists for the surfaces that report rather than prune. A held pass
// stops retention for a backup set until somebody reconciles it, and until
// this existed the only way to find that out was to ask for a plan and
// read the refusals in it: FR-24's health report computes DecideKeep and
// PlanHomeMoves and never asked this at all, so a set could sit wedged for
// weeks with local copies piling up and nothing anywhere saying why. See
// internal/app's BuildHealthReport.
//
// It stats and reads only, like everything else on this path, and it asks
// no medium anything (FR-32).
func LastKnownGoodUnconfirmed(now time.Time, cfg config.Retention, bs config.BackupSet, records []state.Record, where ArtifactLocator) (string, error) {
	if bs.ID.IsZero() {
		return "", fmt.Errorf("retention: LastKnownGoodUnconfirmed needs a non-zero backup set id")
	}
	if where == nil {
		return "", fmt.Errorf("retention: LastKnownGoodUnconfirmed needs a way to say where each artifact's durable copy is; " +
			"pass AllLocal for a deployment with no storage mediums, and internal/app.ActiveMediumFromRecords otherwise")
	}

	_, lkg, err := DecideKeep(now, cfg, bs.ID, records)
	if err != nil {
		return "", fmt.Errorf("retention: last known good: %w", err)
	}

	recByArtifact := make(map[model.ArtifactID]state.Record, len(records))
	for _, rec := range records {
		recByArtifact[rec.Artifact] = rec
	}
	return pruneLastKnownGoodUnconfirmed(bs, recByArtifact, lkg, where), nil
}

// pruneLastKnownGoodUnconfirmed answers the question FR-19 never asked: is
// there actually a copy of the restore point this policy is protecting?
//
// It returns the empty string when there is, and one sentence naming what
// is wrong when there is not (issue #602).
//
// # Why the question has to be asked at all
//
// FR-19's protection is computed from journal rows and nothing else
// (lastknowngood.go takes no filesystem, no clock and, per FR-32, nothing
// a medium reported). That is the right shape for a decision and it makes
// the protection exactly as good as the row behind it. A row whose file is
// gone (an operator's rm, a disk that failed, a restore that went
// sideways: everything FR-17 reconciliation exists to notice) still
// protects a NAME, while every other artifact in the set is still selected
// for deletion on its age alone. Carrying that plan out empties the backup
// set, and the preview the operator confirmed said "kept by the
// LAST_KNOWN_GOOD tier" about a file that was not there.
//
// FR-30's invariant is that at no instant may an artifact have no
// confirmed readable copy, and that is what this closes: the last delete
// in such a pass lands at an instant when nothing in the set has one.
//
// # It fires only when the operator asked for the protection
//
// lkg.Protected is false both when the set has no eligible artifact and
// when protect_last_known_good is explicitly false, and the second of
// those is a deployment saying out loud that retention may empty this
// backup set. Overriding that would silently stop a documented
// configuration working the first time a file went missing, so the guard
// has nothing to say about it. See
// TestPruneDeletesWhenTheOperatorTurnedTheProtectionOff.
//
// # What counts as confirmed, per medium, and why the answers differ
//
// A durable copy CONFIRMED on a storage medium is confirmed by its own
// ACTIVE placement row, which the locator read one package up where
// reading it is not a retention decision. Nothing here asks the medium
// anything, because FR-32 says nothing a medium reported may reach a
// retention decision and this is one. A CONTESTED location is more than
// one ACTIVE placement, which is more copies rather than fewer, so it is
// confirmed too.
//
// Everything else is a local file, and the confirmation is the same one
// pruneVerifySafeToDelete makes about a delete candidate: a real, regular,
// non-symlink directory entry at the path this backup set's root and the
// artifact's own name compute. Deliberately the same predicate rather than
// a looser "something is there": FR-20 refuses to treat a symlink at a
// final path as a positively identified managed artifact, and a symlink
// cannot be too untrustworthy to delete and trustworthy enough to justify
// deleting everything else. See
// TestPruneRefusesEveryDeleteWhenTheLastKnownGoodPathIsASymlink.
//
// It stats and reads only, like every other check in this file, so calling
// it once per plan and again before every delete costs one extra Lstat per
// deletion and no correctness. PruneApply's own doc says why it is asked
// that often.
//
// # Which half of FR-30 this closes, said plainly
//
// FR-30 says at no instant may an artifact have no confirmed readable
// copy. This closes that for a restore point whose durable copy is a local
// file, and it leaves it exactly as open as it was for one that has been
// moved to a medium.
//
// The medium branch above returns "confirmed" from an ACTIVE placement row
// and nothing else. That row is a journal fact, so a bucket lifecycle
// rule, an out-of-band delete or a bucket somebody else pays for going
// away leaves the row saying ACTIVE and this guard saying confirmed, which
// is precisely the "protects a name, not a copy" shape this whole function
// exists to end on the local side. The difference is not an oversight and
// is not fixable here: FR-32 forbids anything a medium reported from
// reaching a retention decision, and this is one. Asking the medium would
// break that rule outright, and no weaker version of the question exists,
// because a medium's answer is a medium's answer however it is phrased.
//
// So the omission is deliberate and stated rather than implied. What would
// close it is the same shape FR-16 already uses for a delete: an
// object-identity check performed one package up, where reading a medium
// is not a retention decision, whose RESULT reaches a placement row this
// function then reads exactly as it reads one now. That is a piece of
// work, not a line, and it belongs to whoever owns the medium half of
// FR-30 rather than to this guard.
func pruneLastKnownGoodUnconfirmed(bs config.BackupSet, recByArtifact map[model.ArtifactID]state.Record, lkg LastKnownGoodResult, where ArtifactLocator) string {
	if !lkg.Protected {
		return ""
	}

	rec, ok := recByArtifact[lkg.Artifact]
	if !ok {
		// Cannot happen: lkg.Artifact came out of these same records.
		// Refused rather than waved through, because this branch is
		// reached only when something about the inputs is already wrong
		// and the fail-safe direction on a delete path is the one that
		// costs nothing.
		return fmt.Sprintf(
			"this backup set's last known good is %s and no record in this pass describes it, so nothing here can confirm a readable copy of it exists",
			lkg.Artifact)
	}

	loc := where(lkg.Artifact)
	if loc.Status == LocationContested || loc.OnMedium() {
		return ""
	}

	path, err := pruneFinalPath(bs, rec.Artifact)
	if err != nil {
		return fmt.Sprintf(
			"this backup set's last known good is %s and nothing here can say where its copy belongs: %v",
			lkg.Artifact, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Sprintf(
			"this backup set's last known good is %s and there is no readable copy of it at %s: %v",
			lkg.Artifact, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Sprintf(
			"this backup set's last known good is %s and %s is a symlink, which FR-20 never treats as a positively identified managed artifact",
			lkg.Artifact, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf(
			"this backup set's last known good is %s and %s is not a regular file",
			lkg.Artifact, path)
	}
	return ""
}

// pruneHoldEveryDelete turns every PruneDelete in verdicts into a
// PruneRefuse naming why, or does nothing at all when why is empty.
//
// Every delete, not the ones that look related. The fact that stops a
// deletion here is about the backup SET (its last known good has no
// confirmed copy), so it disqualifies the set rather than an artifact, and
// half a pass carried out is the outcome this whole file is arranged to
// make impossible: the artifacts a partial run removed are the ones that
// were still readable.
//
// "Every delete in verdicts", though, and PruneApply hands it the tail of
// its own slice rather than the whole of it: the deletes it has not
// reached yet. A pass that has already removed something and then finds
// the last known good gone stops there, and rewriting the verdicts of the
// artifacts it already unlinked would report a deletion that happened as
// one that was refused. The hold is about what happens next, which is the
// only thing still available to decide.
//
// REFUSE and never KEEP, for the reason pruneEvaluate gives at its own
// resolution-failure branch: KEEP asserts a tier selected this artifact,
// and this asserts that nothing was decided about it safely. Collapsing
// the two would report a data-loss risk as an ordinary retention outcome.
func pruneHoldEveryDelete(verdicts []PruneVerdict, why string) {
	if why == "" {
		return
	}
	for i := range verdicts {
		if verdicts[i].Action != PruneDelete {
			continue
		}
		verdicts[i].Action = PruneRefuse
		verdicts[i].HoldReason = why
		verdicts[i].Reason = fmt.Sprintf(
			"refusing to delete %s: %s. Deleting anything in this backup set now would remove a copy while the restore point FR-19 reports as protected is not one, "+
				"so nothing here is deleted until a reconciliation (FR-17) settles what this set actually holds",
			verdicts[i].Artifact, why)
	}
}

// PruneApply computes PruneDecide's own verdicts and deletes the local
// file behind every PruneDelete result. Its first act is calling
// PruneDecide with exactly the arguments it was given; every KEEP or
// REFUSE it returns is one PruneDecide itself already decided, and a
// caller comparing a preceding --dry-run's output for the same,
// unmodified backup set against PruneApply's return value should see the
// same verdicts, modulo one thing: PruneApply's own final safety re-check,
// below.
//
// Immediately before the actual os.Remove for each delete candidate,
// PruneApply calls pruneVerifySafeToDelete a second time, against
// whatever is on disk at that exact moment rather than what PruneDecide
// observed earlier in this same pass (deleting other artifacts in the
// same call takes real time; another process could act on this backup
// set concurrently). If that second check disagrees, the verdict is
// downgraded to PruneRefuse with a reason saying so explicitly, and the
// file is left untouched: this function never deletes anything
// pruneVerifySafeToDelete has not just, freshly, approved. This closes
// the gap between "was safe when decided" and "is safe right now" as far
// as a userspace check reasonably can; it does not, and cannot, close the
// underlying kernel-level TOCTOU window between that check's last syscall
// and os.Remove's own unlink (no portable, cross-compiling stdlib
// primitive available to this project closes that window completely; see
// lifecycle/commit.go's own "honest accounting" section for the same kind
// of limit acknowledged rather than hidden).
//
// Exactly the same argument, and at exactly the same frequency, applies
// to FR-19's own confirmation: immediately before every delete, this
// function re-derives pruneLastKnownGoodUnconfirmed against the disk as it
// is at that moment, and holds that delete and every one still to come
// when the restore point FR-19 reports as protected has no copy anything
// could read. PruneDecide already asked, and the answer is about a file
// another process can take away in between (issue #602).
//
// Per delete rather than once for the pass, which is what it used to be.
// A pass removing a thousand artifacts is not an instant, and a
// confirmation taken before the loop says nothing about the second half of
// it: the deletes that would follow the loss are precisely the copies that
// were still readable when it happened. The cost of asking every time is
// one Lstat per delete against one unlink per delete, which is nothing,
// and pruneLastKnownGoodUnconfirmed stats and reads only, so asking again
// can change no state and reach no medium. See
// TestPruneHoldsTheRestOfThePassWhenTheLastKnownGoodGoesPartwayThrough and
// TestPruneReconfirmsTheLastKnownGoodAfterThePlanBeforeItDeletes, which
// are the two deadlines this placement is about: after the plan, and
// between one delete and the next.
//
// # The object half
//
// A verdict whose Medium is not local is not a local file, and it is
// removed through medium (a MediumPruner) rather than os.Remove. That
// call carries FR-16's identity re-check with it, for the same reason the
// second pruneVerifySafeToDelete call above exists: the decision was taken
// earlier, and an object can be replaced between the two. A nil
// MediumPruner is a REFUSE and never a pass, the same direction #238 gave
// its own nil TierGuard, because a delete this product cannot prove is a
// delete it does not make.
func PruneApply(ctx context.Context, now time.Time, cfg config.Retention, bs config.BackupSet, records []state.Record, where ArtifactLocator, medium MediumPruner) ([]PruneVerdict, error) {
	verdicts, err := PruneDecide(now, cfg, bs, records, where)
	if err != nil {
		return nil, err
	}

	recByArtifact := make(map[model.ArtifactID]state.Record, len(records))
	for _, rec := range records {
		recByArtifact[rec.Artifact] = rec
	}

	// FR-19's protection, re-derived here rather than read off the plan,
	// so the artifact the confirmation below is about is the one THIS
	// call's own inputs name. It reads journal rows and a clock and
	// nothing else, so it is the reviewed decision and not the fresh
	// evidence: the freshness is the per-delete confirmation in the loop.
	_, lkg, err := DecideKeep(now, cfg, bs.ID, records)
	if err != nil {
		return nil, fmt.Errorf("retention: prune apply: %w", err)
	}

	for i := range verdicts {
		if verdicts[i].Action != PruneDelete {
			continue
		}

		// FR-30's own re-check, immediately before this delete, for the
		// reason the second pruneVerifySafeToDelete call below exists:
		// the last thing that asked was either PruneDecide, a moment
		// before this pass began, or this same line before the previous
		// delete, and the answer is about a file another process can take
		// away in between. The decision is the reviewed one; the evidence
		// is fresh. Holding verdicts[i:] rather than all of them stops
		// the pass here instead of rewriting deletes it has already
		// carried out. See pruneLastKnownGoodUnconfirmed (issue #602).
		if why := pruneLastKnownGoodUnconfirmed(bs, recByArtifact, lkg, where); why != "" {
			pruneHoldEveryDelete(verdicts[i:], why)
			continue
		}

		rec, ok := recByArtifact[verdicts[i].Artifact]
		if !ok {
			// Cannot happen for the same reason it cannot happen in
			// PruneDecide above; guarded rather than deleting on the
			// strength of a verdict this call cannot re-derive.
			verdicts[i].Action = PruneRefuse
			verdicts[i].Reason = "internal inconsistency: no matching record at delete time"
			continue
		}

		if verdicts[i].Medium != config.MediumLocal {
			// The object half. Every fact this delete rests on is
			// re-derived by the pruner from the journal and from the
			// medium itself, at this moment, which is FR-16's whole
			// point: the plan was made earlier, and an object can be
			// replaced between the two.
			if medium == nil {
				verdicts[i].Action = PruneRefuse
				verdicts[i].Reason = fmt.Sprintf(
					"refusing to delete %s from %q: nothing here can re-check the object's identity before removing it, and an unproven delete is not a delete this product makes",
					verdicts[i].Artifact, verdicts[i].Medium)
				continue
			}
			if err := medium.DeleteFromMedium(ctx, rec, verdicts[i].Medium); err != nil {
				verdicts[i].Action = PruneRefuse
				verdicts[i].Reason = fmt.Sprintf("nothing was removed from %q: %v", verdicts[i].Medium, err)
			}
			continue
		}

		safePath, err := pruneVerifySafeToDelete(bs, rec)
		if err != nil {
			verdicts[i].Action = PruneRefuse
			verdicts[i].Reason = fmt.Sprintf(
				"passed FR-20's checks moments ago but refused again immediately before deleting, so nothing was removed: %v", err)
			continue
		}

		if err := os.Remove(safePath); err != nil {
			verdicts[i].Action = PruneRefuse
			verdicts[i].Reason = fmt.Sprintf("delete failed, nothing was removed: %v", err)
			continue
		}
	}

	return verdicts, nil
}

// sortPruneVerdicts orders out by artifact name, so PruneDecide's result
// never depends on GFSDecide's or DecideKeep's own internal map iteration
// order.
func sortPruneVerdicts(out []PruneVerdict) {
	sort.Slice(out, func(i, j int) bool { return out[i].Artifact.Name < out[j].Artifact.Name })
}

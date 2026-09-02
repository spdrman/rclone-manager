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
// It performs no mutation: `backup-manager retention --dry-run` (owned by
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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
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
// operator reading `backup-manager retention --dry-run` output can act
// on without reading this package's source.
type PruneVerdict struct {
	Artifact model.ArtifactID
	Action   PruneAction

	// Path is the artifact's final local path, computed the same way
	// lifecycle's own Commit computes it (this backup set's LocalPath
	// joined with the artifact's basename). It is always populated,
	// regardless of Action, so a REFUSE verdict still names exactly which
	// file was refused.
	Path string

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
}

// pruneFinalPath computes the same final local path lifecycle's own
// unexported finalPath (transfer.go) computes for the same (LocalDir,
// Artifact) pair: the artifact's basename, joined directly under the
// backup set's configured local directory. Duplicated here for the same
// reason prunePartialSuffix is (this package cannot reach lifecycle's
// unexported helper, and file scope keeps it that way): this formula, and
// lifecycle's own copy of it, are the only two places in the whole
// project allowed to compute it.
func pruneFinalPath(bs config.BackupSet, artifact model.ArtifactID) string {
	return filepath.Join(bs.LocalPath, artifact.Name)
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
			"retention: prune: refusing %s: journal state %q is not a final managed artifact (must be COMMITTED, REMOTE_DELETE_PENDING or COMPLETE)",
			rec.Artifact, rec.State)
	}

	expected := pruneFinalPath(bs, rec.Artifact)
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
	return filepath.Join(resolvedRoot, rec.Artifact.Name), nil
}

// pruneEvaluate is PruneDecide's per-artifact decision: given the composed
// GFS/last-known-good verdict DecideKeep already produced for this
// artifact, decide KEEP, DELETE or REFUSE.
func pruneEvaluate(bs config.BackupSet, rec state.Record, keepVerdict GFSVerdict, lkg LastKnownGoodResult) PruneVerdict {
	path := pruneFinalPath(bs, rec.Artifact)

	if keepVerdict.Keep {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneKeep,
			Path:     path,
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
			Reason: fmt.Sprintf(
				"refusing to delete %s: it holds FR-19 last-known-good protection, but the GFS verdict passed in claims Keep=false; this contradiction means mismatched inputs were passed to this decision, not a real delete candidate",
				rec.Artifact),
		}
	}

	safePath, err := pruneVerifySafeToDelete(bs, rec)
	if err != nil {
		return PruneVerdict{
			Artifact: rec.Artifact,
			Action:   PruneRefuse,
			Path:     path,
			Reason:   err.Error(),
		}
	}

	return PruneVerdict{
		Artifact: rec.Artifact,
		Action:   PruneDelete,
		Path:     safePath,
		Reason:   pruneDeleteReason(keepVerdict),
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
// is exactly, and only, the function `backup-manager retention --dry-run`
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
// Artifacts outside GFS's own scope (still in flight, or in an
// exceptional non-recoverable state: see GFSDecide's doc on
// gfsManagedCompleteStates) never receive a verdict here, exactly as they
// never receive one from GFSDecide: there is nothing for retention to
// decide about a backup that has not yet succeeded. The returned slice is
// sorted by artifact name, same as GFSDecide's own, so two calls over the
// same inputs render identically.
func PruneDecide(now time.Time, cfg config.Retention, bs config.BackupSet, records []state.Record) ([]PruneVerdict, error) {
	if bs.ID.IsZero() {
		return nil, fmt.Errorf("retention: PruneDecide needs a non-zero backup set id")
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
		out = append(out, pruneEvaluate(bs, rec, v, lkg))
	}
	sortPruneVerdicts(out)
	return out, nil
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
func PruneApply(now time.Time, cfg config.Retention, bs config.BackupSet, records []state.Record) ([]PruneVerdict, error) {
	verdicts, err := PruneDecide(now, cfg, bs, records)
	if err != nil {
		return nil, err
	}

	recByArtifact := make(map[model.ArtifactID]state.Record, len(records))
	for _, rec := range records {
		recByArtifact[rec.Artifact] = rec
	}

	for i := range verdicts {
		if verdicts[i].Action != PruneDelete {
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

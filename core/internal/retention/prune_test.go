package retention

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// --- test helpers (prefixed prune* so they never collide with gfs_test.go's
// gfs*-prefixed helpers or lastknowngood_test.go's lkg*-prefixed ones in
// this same package; gfsMustSet and gfsMustArtifact are generic enough to
// reuse directly) ---

// pruneBackupSet builds the config.BackupSet PruneDecide/PruneApply need:
// just enough for the identity and the local root, exactly like config's
// own Validate would have populated after a real YAML load.
func pruneBackupSet(set model.BackupSetID, localRoot string) config.BackupSet {
	return config.BackupSet{Name: set.Set, ID: set, LocalPath: localRoot}
}

// pruneRecord builds one journal row with an explicit LocalPath, unlike
// gfs_test.go's gfsBuildRecords (which never sets it: GFSDecide and
// LastKnownGoodDecide never look at it). FR-20's whole job is examining
// this field, so these tests need to control it directly, including
// setting it to something that deliberately disagrees with what the
// backup set's root and the artifact's name would compute.
func pruneRecord(artifact model.ArtifactID, st lifecycle.State, discovered time.Time, localPath string) state.Record {
	return state.Record{
		Artifact:     artifact,
		State:        string(st),
		LocalPath:    localPath,
		DiscoveredAt: discovered,
		UpdatedAt:    discovered,
	}
}

// pruneRawArtifact builds a model.ArtifactID directly, bypassing
// model.NewArtifactID's own basename validation entirely. Every attack
// this file demonstrates against a crafted artifact name (a "/" or a ".."
// segment) is only reachable this way in real Go code: NewArtifactID
// itself already refuses these at construction. Building one anyway and
// running it through PruneDecide/PruneApply is what proves FR-20 does not
// rely solely on that upstream validation ever having run: a hand-edited
// database row, a future scanRecord bug, or a schema migration gone wrong
// could all produce a Record carrying exactly this shape, and this
// package's own safety checks must catch it independently.
func pruneRawArtifact(set model.BackupSetID, name string) model.ArtifactID {
	return model.ArtifactID{Set: set, Name: name}
}

// pruneTodayOnlyChain returns a live one-tier chain whose whole window is
// pruneNow's own calendar day, with last-known-good protection explicitly
// off. Every artifact this file dates before pruneNow is therefore a
// guaranteed GFS delete candidate, so DecideKeep's Keep flag depends on
// nothing a test did not put there itself.
//
// It used to spell that as "every tier disabled", which gfsResolveChain
// now refuses: a chain out of which nothing can be selected is an error,
// because it puts every managed backup in the set on the delete side. A
// narrow live tier says the same thing about these fixtures without
// asking the resolver to accept a policy no operator should be able to
// write.
func pruneTodayOnlyChain() config.Retention {
	off := false
	return config.Retention{
		Timezone:     "UTC",
		WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "daily", Granularity: config.GranularityDay, Keep: 1},
		},
		ProtectLastKnownGood: &off,
	}
}

func pruneWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func pruneMustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("expected %s to still exist, but Lstat failed: %v", path, err)
	}
}

func pruneMustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be gone, but Lstat returned err=%v", path, err)
	}
}

// pruneFindVerdict is a small assertion helper: locate one artifact's
// verdict by name, failing the test immediately if it is missing.
func pruneFindVerdict(t *testing.T, verdicts []PruneVerdict, name string) PruneVerdict {
	t.Helper()
	for _, v := range verdicts {
		if v.Artifact.Name == name {
			return v
		}
	}
	t.Fatalf("no verdict for artifact %q in %+v", name, verdicts)
	return PruneVerdict{}
}

var pruneNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// --- the positive control: PruneDecide/PruneApply must actually delete a
// genuinely unprotected artifact, not just refuse everything ---

func TestPruneApplyDeletesGenuineCandidate(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-positive", "set")
	artifact := gfsMustArtifact(t, set, "good-backup.zst")
	path := filepath.Join(root, "good-backup.zst")
	pruneWriteFile(t, path, "a perfectly good, unprotected restore point")

	records := []state.Record{pruneRecord(artifact, lifecycle.Complete, pruneNow.Add(-365*24*time.Hour), path)}
	bs := pruneBackupSet(set, root)
	cfg := pruneTodayOnlyChain()

	verdicts, err := PruneApply(pruneNow, cfg, bs, records)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "good-backup.zst")
	if v.Action != PruneDelete {
		t.Fatalf("Action = %s, want %s (reason: %s)", v.Action, PruneDelete, v.Reason)
	}
	if v.Reason == "" {
		t.Error("Reason is empty; a DELETE verdict must still explain itself")
	}
	pruneMustNotExist(t, path)
}

// A backup set root that is itself a symlink to real storage (a common,
// legitimate NAS/mount pattern) must not be refused just because
// EvalSymlinks resolves it to something else: the artifact's own file, if
// genuinely a regular file reached consistently through that same root,
// is still a valid delete candidate.
func TestPruneAllowsDeletionWhenBackupRootItselfIsASymlink(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-storage")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rootLink := filepath.Join(base, "configured-root")
	if err := os.Symlink(realRoot, rootLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	set := gfsMustSet(t, "prune-symlinked-root", "set")
	artifact := gfsMustArtifact(t, set, "good.zst")
	// Commit would have joined LocalDir (rootLink) with the artifact name
	// and recorded exactly that string in the journal; the OS resolves the
	// symlinked ancestor transparently when the file is actually written.
	journalPath := filepath.Join(rootLink, "good.zst")
	pruneWriteFile(t, filepath.Join(realRoot, "good.zst"), "real content behind the symlinked root")

	records := []state.Record{pruneRecord(artifact, lifecycle.Committed, pruneNow.Add(-365*24*time.Hour), journalPath)}
	bs := pruneBackupSet(set, rootLink)
	cfg := pruneTodayOnlyChain()

	verdicts, err := PruneApply(pruneNow, cfg, bs, records)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "good.zst")
	if v.Action != PruneDelete {
		t.Fatalf("Action = %s, want %s (reason: %s); a symlinked root should not by itself cause a refusal", v.Action, PruneDelete, v.Reason)
	}
	pruneMustNotExist(t, filepath.Join(realRoot, "good.zst"))
}

// --- KEEP verdicts must name what kept the artifact ---

func TestPruneDecideKeepsArtifactSelectedByGFSTier(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-keep-gfs", "set")
	artifact := gfsMustArtifact(t, set, "todays-backup.zst")
	path := filepath.Join(root, "todays-backup.zst")
	pruneWriteFile(t, path, "kept by the daily tier")

	records := []state.Record{pruneRecord(artifact, lifecycle.Complete, pruneNow, path)}
	bs := pruneBackupSet(set, root)
	off := false
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 7, ProtectLastKnownGood: &off}

	verdicts, err := PruneDecide(pruneNow, cfg, bs, records)
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "todays-backup.zst")
	if v.Action != PruneKeep {
		t.Fatalf("Action = %s, want %s", v.Action, PruneKeep)
	}
	if len(v.Tiers) != 1 || v.Tiers[0].Tier != GFSDaily {
		t.Errorf("Tiers = %v, want [%s]", v.Tiers, GFSDaily)
	}
	if !strings.Contains(v.Reason, string(GFSDaily)) {
		t.Errorf("Reason %q does not name the DAILY tier that kept it", v.Reason)
	}
	pruneMustExist(t, path) // PruneDecide never touches the filesystem
}

func TestPruneDecideKeepsLastKnownGoodArtifact(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-keep-lkg", "set")
	artifact := gfsMustArtifact(t, set, "only-backup.zst")
	path := filepath.Join(root, "only-backup.zst")
	pruneWriteFile(t, path, "the only restore point in this set, ancient but still protected")

	// Old enough to fall outside every GFS tier, but still the newest (and
	// only) eligible artifact in the set: FR-19 must protect it anyway.
	old := pruneNow.Add(-5 * 365 * 24 * time.Hour)
	records := []state.Record{pruneRecord(artifact, lifecycle.Complete, old, path)}
	bs := pruneBackupSet(set, root)
	on := true
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 7, WeeklyMonths: 3, MonthlyMonths: 12, ProtectLastKnownGood: &on}

	verdicts, err := PruneDecide(pruneNow, cfg, bs, records)
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "only-backup.zst")
	if v.Action != PruneKeep {
		t.Fatalf("Action = %s, want %s (reason: %s)", v.Action, PruneKeep, v.Reason)
	}
	if len(v.Tiers) != 1 || v.Tiers[0].Tier != TierLastKnownGood {
		t.Errorf("Tiers = %v, want [%s]", v.Tiers, TierLastKnownGood)
	}
	if !strings.Contains(v.Reason, string(TierLastKnownGood)) {
		t.Errorf("Reason %q does not name last-known-good as the reason it was kept", v.Reason)
	}
}

// --- the six FR-20 attacks, each proven refused ---

// Attack: a record still in flight (never final, .partial by construction)
// is handed to the decision function with a verdict that lies and claims
// Keep == false, i.e. "delete this". This bypasses PruneDecide's own
// filtering (which never even considers a non-managed-complete record) to
// prove the safety check inside pruneEvaluate/pruneVerifySafeToDelete
// refuses it independently, not merely because an upstream filter happened
// to exclude it first.
func TestPruneRefusesPartialArtifactEvenWhenToldToDeleteIt(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-partial", "set")
	artifact := gfsMustArtifact(t, set, "in-flight.zst")
	partialPath := filepath.Join(root, "in-flight.zst.partial")
	pruneWriteFile(t, partialPath, "not yet a restore point")

	rec := pruneRecord(artifact, lifecycle.Transferring, pruneNow, partialPath)
	lyingVerdict := GFSVerdict{Artifact: artifact, Keep: false} // "no tier keeps it"
	bs := pruneBackupSet(set, root)

	got := pruneEvaluate(bs, rec, lyingVerdict, LastKnownGoodResult{})
	if got.Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s: a .partial artifact must never be deleted regardless of what the GFS verdict claims", got.Action, PruneRefuse)
	}
	if !strings.Contains(got.Reason, "final managed artifact") {
		t.Errorf("Reason %q does not explain that this is not a final managed artifact", got.Reason)
	}
	pruneMustExist(t, partialPath)

	// Also confirm the public entry point never even surfaces this record
	// as a decision at all, matching GFSDecide's own scope.
	verdicts, err := PruneDecide(pruneNow, pruneTodayOnlyChain(), bs, []state.Record{rec})
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	if len(verdicts) != 0 {
		t.Errorf("PruneDecide returned %d verdicts for an in-flight artifact, want 0: %+v", len(verdicts), verdicts)
	}
}

// Attack: a file sitting in the backup set's directory that has no
// corresponding journal record at all. PruneDecide/PruneApply only ever
// iterate records the caller read from the journal; this proves an
// untracked file is never inspected, matched, or touched.
func TestPruneNeverConsidersAFileTheJournalDoesNotKnowAbout(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-untracked", "set")

	untrackedPath := filepath.Join(root, "mystery-file.zst")
	pruneWriteFile(t, untrackedPath, "sitting in the directory, but the journal has never heard of it")

	// One legitimate, unrelated managed artifact in the same directory, so
	// this test proves the untracked file is skipped, not that the whole
	// pass did nothing.
	artifact := gfsMustArtifact(t, set, "tracked.zst")
	trackedPath := filepath.Join(root, "tracked.zst")
	pruneWriteFile(t, trackedPath, "the journal knows exactly this one")
	records := []state.Record{pruneRecord(artifact, lifecycle.Complete, pruneNow.Add(-365*24*time.Hour), trackedPath)}

	bs := pruneBackupSet(set, root)
	verdicts, err := PruneApply(pruneNow, pruneTodayOnlyChain(), bs, records)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	for _, v := range verdicts {
		if v.Artifact.Name == "mystery-file.zst" {
			t.Fatalf("PruneApply produced a verdict for an untracked file: %+v", v)
		}
	}
	pruneMustExist(t, untrackedPath)
	pruneMustNotExist(t, trackedPath) // the actually-tracked artifact was still deleted
}

// Attack: the journal's own recorded LocalPath disagrees with the path
// this backup set's root and the artifact's name compute. Whatever
// produced the disagreement (a stale row, a hand edit, a bug), FR-20 must
// refuse rather than delete either the file the record points at or the
// file its own name would suggest.
func TestPruneRefusesWhenJournalPathDisagreesWithComputedPath(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-mismatch", "set")
	artifact := gfsMustArtifact(t, set, "official.zst")

	correctPath := filepath.Join(root, "official.zst")
	pruneWriteFile(t, correctPath, "the file this artifact's name actually computes to")

	wrongPath := filepath.Join(root, "sneaky.zst")
	pruneWriteFile(t, wrongPath, "an unrelated file the corrupted record happens to point at")

	// The journal record for "official.zst" claims its local path is
	// "sneaky.zst" instead of the path config would actually compute.
	records := []state.Record{pruneRecord(artifact, lifecycle.Complete, pruneNow.Add(-365*24*time.Hour), wrongPath)}
	bs := pruneBackupSet(set, root)

	verdicts, err := PruneApply(pruneNow, pruneTodayOnlyChain(), bs, records)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "official.zst")
	if v.Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s (a mismatched journal path must never be trusted)", v.Action, PruneRefuse)
	}
	if !strings.Contains(v.Reason, "does not match") {
		t.Errorf("Reason %q does not explain the path mismatch", v.Reason)
	}
	pruneMustExist(t, correctPath)
	pruneMustExist(t, wrongPath)
}

// Attack: the headline scenario from this issue's brief. The artifact's
// own final path, sitting directly inside the backup root, is a symlink
// pointing at a file entirely outside the root. A naive containment check
// on the unresolved path would see "root/good.zst" and consider it
// contained; this proves the actual check refuses it because it is a
// symlink at all, regardless of where it resolves.
func TestPruneRefusesSymlinkAtFinalPath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "backup-root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := filepath.Join(base, "outside-the-root")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secretPath := filepath.Join(outside, "secret.txt")
	pruneWriteFile(t, secretPath, "this must never be touched by retention")

	set := gfsMustSet(t, "prune-symlink-escape", "set")
	artifact := gfsMustArtifact(t, set, "good.zst")
	linkPath := filepath.Join(root, "good.zst")
	if err := os.Symlink(secretPath, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	records := []state.Record{pruneRecord(artifact, lifecycle.Complete, pruneNow.Add(-365*24*time.Hour), linkPath)}
	bs := pruneBackupSet(set, root)

	verdicts, err := PruneApply(pruneNow, pruneTodayOnlyChain(), bs, records)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "good.zst")
	if v.Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s: a symlink escaping the backup root must never be deleted through", v.Action, PruneRefuse)
	}
	if !strings.Contains(v.Reason, "symlink") {
		t.Errorf("Reason %q does not mention the symlink refusal", v.Reason)
	}
	pruneMustExist(t, secretPath) // the real target survives untouched
	pruneMustExist(t, linkPath)   // the symlink itself is also left alone
}

// Attack: a record whose ArtifactID was never built through
// model.NewArtifactID (a hand-edited row, a corrupted journal, a future
// bug) carries a ".."-bearing name. pruneFinalPath's Join then computes a
// path outside the configured root entirely, landing on a real file this
// test plants there. This is the path-traversal escape FR-20 names
// explicitly, exercised independently of the symlink attack above.
func TestPruneRejectsPathTraversalViaCraftedArtifactName(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "setA")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secretPath := filepath.Join(base, "secret.txt")
	pruneWriteFile(t, secretPath, "a sibling of the backup root, never a member of it")

	set := gfsMustSet(t, "prune-traversal", "set")
	artifact := pruneRawArtifact(set, "../secret.txt")
	computed := filepath.Join(root, "../secret.txt") // == secretPath

	records := []state.Record{pruneRecord(artifact, lifecycle.Complete, pruneNow.Add(-365*24*time.Hour), computed)}
	bs := pruneBackupSet(set, root)

	verdicts, err := PruneApply(pruneNow, pruneTodayOnlyChain(), bs, records)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("got %d verdicts, want 1: %+v", len(verdicts), verdicts)
	}
	if verdicts[0].Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s: a traversal escape must never be deleted", verdicts[0].Action, PruneRefuse)
	}
	pruneMustExist(t, secretPath)
}

// Attack: the classic prefix-confusion bug. The escape target sits in a
// directory whose name merely extends the configured root's own name
// (".../setA-evil" vs ".../setA"), which a naive strings.HasPrefix
// containment check on the two directories would wrongly treat as
// "inside." Exact canonical-path equality must reject it.
func TestPruneRejectsSiblingPrefixDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "setA")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	sibling := filepath.Join(base, "setA-evil")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	secretPath := filepath.Join(sibling, "secret.txt")
	pruneWriteFile(t, secretPath, "outside setA, but setA-evil shares setA's own name as a prefix")

	set := gfsMustSet(t, "prune-prefix", "set")
	artifact := pruneRawArtifact(set, "../setA-evil/secret.txt")
	computed := filepath.Join(root, "../setA-evil/secret.txt") // == secretPath

	// Sanity check on the test itself: a naive prefix comparison on the
	// unresolved strings really would get this wrong, which is exactly
	// why pruneVerifySafeToDelete does not use one.
	if !strings.HasPrefix(sibling, root) {
		t.Fatalf("test setup error: %q is not expected to share %q as a naive string prefix", sibling, root)
	}

	records := []state.Record{pruneRecord(artifact, lifecycle.Complete, pruneNow.Add(-365*24*time.Hour), computed)}
	bs := pruneBackupSet(set, root)

	verdicts, err := PruneApply(pruneNow, pruneTodayOnlyChain(), bs, records)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("got %d verdicts, want 1: %+v", len(verdicts), verdicts)
	}
	if verdicts[0].Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s: a sibling directory sharing a name prefix must never be treated as contained", verdicts[0].Action, PruneRefuse)
	}
	pruneMustExist(t, secretPath)
}

// Attack: even if a caller manages to pass pruneEvaluate a GFSVerdict that
// claims Keep == false for the exact artifact a LastKnownGoodResult names
// as protected (mismatched inputs, a caller bug), the last-known-good
// artifact must still never be deleted. See ApplyLastKnownGood's own
// "defensive only" fallback for the same reasoning applied to composition.
func TestPruneRefusesLastKnownGoodEvenIfGFSVerdictLies(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-lkg-defensive", "set")
	artifact := gfsMustArtifact(t, set, "protected.zst")
	path := filepath.Join(root, "protected.zst")
	pruneWriteFile(t, path, "last-known-good, but a lying verdict claims otherwise")

	rec := pruneRecord(artifact, lifecycle.Complete, pruneNow, path)
	lyingVerdict := GFSVerdict{Artifact: artifact, Keep: false}
	lkg := LastKnownGoodResult{Set: set, Enabled: true, Protected: true, Artifact: artifact, Reason: "the newest eligible restore point"}
	bs := pruneBackupSet(set, root)

	got := pruneEvaluate(bs, rec, lyingVerdict, lkg)
	if got.Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s: last-known-good protection must hold even against a contradicting GFS verdict", got.Action, PruneRefuse)
	}
	pruneMustExist(t, path)
}

// --- structural properties ---

func TestPruneDecideRejectsRecordFromAnotherBackupSet(t *testing.T) {
	root := t.TempDir()
	setA := gfsMustSet(t, "prune-iso", "a")
	setB := gfsMustSet(t, "prune-iso", "b")

	own := gfsMustArtifact(t, setA, "own.zst")
	foreign := gfsMustArtifact(t, setB, "foreign.zst")
	records := []state.Record{
		pruneRecord(own, lifecycle.Complete, pruneNow, filepath.Join(root, "own.zst")),
		pruneRecord(foreign, lifecycle.Complete, pruneNow, filepath.Join(root, "foreign.zst")),
	}
	bs := pruneBackupSet(setA, root)

	if _, err := PruneDecide(pruneNow, pruneTodayOnlyChain(), bs, records); err == nil {
		t.Fatal("expected an error for a record belonging to a different backup set (FR-7 isolation would be silently broken)")
	}
}

func TestPruneDecideRejectsZeroBackupSet(t *testing.T) {
	if _, err := PruneDecide(pruneNow, pruneTodayOnlyChain(), config.BackupSet{}, nil); err == nil {
		t.Fatal("expected an error for a zero backup set id, got nil")
	}
}

// --- the core promise: dry-run and the real run cannot diverge ---

// TestPruneDryRunAndApplyAgree is this package's proof for the API's
// central claim: PruneApply's first act is calling PruneDecide with
// identical arguments, so a preceding dry-run's verdicts and the real
// run's verdicts can never structurally disagree. This exercises a mixed
// backup set (one KEEP by GFS tier, one genuine DELETE, one REFUSE) and
// asserts the two calls produce byte-for-byte identical PruneVerdict
// slices, before confirming the DELETE artifact's file is what actually
// changed between the two calls. (Last-known-good's own KEEP path is
// exercised on its own in TestPruneDecideKeepsLastKnownGoodArtifact;
// LastKnownGoodDecide always protects the single newest eligible artifact
// in a set, which would collide with "kept by tier" here rather than
// producing a fourth, independent case.)
func TestPruneDryRunAndApplyAgree(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-agree", "set")

	kept := gfsMustArtifact(t, set, "kept-by-tier.zst")
	keptPath := filepath.Join(root, "kept-by-tier.zst")
	pruneWriteFile(t, keptPath, "kept by the daily tier, and also the newest artifact in the set")

	deletable := gfsMustArtifact(t, set, "deletable.zst")
	deletablePath := filepath.Join(root, "deletable.zst")
	pruneWriteFile(t, deletablePath, "unprotected, safe to delete")

	refused := gfsMustArtifact(t, set, "refused.zst")
	refusedWrongPath := filepath.Join(root, "somewhere-else.zst")
	pruneWriteFile(t, refusedWrongPath, "a mismatched journal path, must be refused")

	// "deletable" and "refused" are both old enough to fall outside the
	// daily window; "kept" is today's, both inside the window and the
	// newest artifact in the set.
	old := pruneNow.Add(-5 * 365 * 24 * time.Hour)
	records := []state.Record{
		pruneRecord(kept, lifecycle.Complete, pruneNow, keptPath),
		pruneRecord(deletable, lifecycle.Complete, old, deletablePath),
		pruneRecord(refused, lifecycle.Complete, old, refusedWrongPath),
	}
	bs := pruneBackupSet(set, root)
	on := true
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 7, ProtectLastKnownGood: &on}

	dryRun, err := PruneDecide(pruneNow, cfg, bs, records)
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	// Confirm the dry run touched nothing.
	pruneMustExist(t, keptPath)
	pruneMustExist(t, deletablePath)
	pruneMustExist(t, refusedWrongPath)

	applied, err := PruneApply(pruneNow, cfg, bs, records)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}

	if !reflect.DeepEqual(dryRun, applied) {
		t.Fatalf("dry-run and apply produced different verdicts:\n dry-run: %+v\n applied: %+v", dryRun, applied)
	}

	// Sanity-check the shape actually exercises three distinct outcomes,
	// so this test cannot pass by accident on an all-KEEP or all-REFUSE
	// scenario.
	if v := pruneFindVerdict(t, applied, "kept-by-tier.zst"); v.Action != PruneKeep {
		t.Errorf("kept-by-tier.zst Action = %s, want %s", v.Action, PruneKeep)
	}
	if v := pruneFindVerdict(t, applied, "deletable.zst"); v.Action != PruneDelete {
		t.Errorf("deletable.zst Action = %s, want %s", v.Action, PruneDelete)
	}
	if v := pruneFindVerdict(t, applied, "refused.zst"); v.Action != PruneRefuse {
		t.Errorf("refused.zst Action = %s, want %s", v.Action, PruneRefuse)
	}

	// Now confirm what actually changed on disk: only the DELETE verdict's
	// file is gone.
	pruneMustExist(t, keptPath)
	pruneMustNotExist(t, deletablePath)
	pruneMustExist(t, refusedWrongPath)
}

// TestPruneDeleteReasonNamesASiblingCollision is issue #292's own PruneApply
// path: pruneEvaluate still decides PruneDelete for the artifact that
// lost the timestamp tie (this issue does not change FR-20's KEEP/DELETE
// decision, only what it explains), but the Reason it hands back must
// name the sibling it collided with, since PruneApply is the actual,
// HTTP-reachable deletion (core/service's ApplyRetentionPlan), unlike
// `retention --dry-run`, which today never calls this package at all.
func TestPruneDeleteReasonNamesASiblingCollision(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-sibling-collision", "gitea-forge")
	winner := gfsMustArtifact(t, set, "gitea-dump-20260828T120000Z.tar.gz")
	loser := gfsMustArtifact(t, set, "gitea-db-20260828T120000Z.dump")
	winnerPath := filepath.Join(root, winner.Name)
	loserPath := filepath.Join(root, loser.Name)
	pruneWriteFile(t, winnerPath, "the portable archive half of one restore point")
	pruneWriteFile(t, loserPath, "the pg_dump half of the very same restore point")

	// Both carry the exact same producer (remote modification) timestamp:
	// the run's own shared timestamp, in the issue's own reproduction.
	// pruneRecord alone cannot express this (it never sets Remote.ModTime,
	// since none of this file's other cases need FR-18's producer pass at
	// all), so these two records are built directly instead.
	runTimestamp := pruneNow
	winnerRec := pruneRecord(winner, lifecycle.Complete, pruneNow, winnerPath)
	winnerRec.Remote.ModTime = &runTimestamp
	loserRec := pruneRecord(loser, lifecycle.Complete, pruneNow, loserPath)
	loserRec.Remote.ModTime = &runTimestamp
	records := []state.Record{winnerRec, loserRec}
	bs := pruneBackupSet(set, root)
	cfg := pruneTodayOnlyChain()

	verdicts, err := PruneDecide(pruneNow, cfg, bs, records)
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}

	kept := pruneFindVerdict(t, verdicts, winner.Name)
	if kept.Action != PruneKeep {
		t.Fatalf("%s: Action = %s, want %s (the fixture's own tie-break winner)", winner.Name, kept.Action, PruneKeep)
	}

	deleted := pruneFindVerdict(t, verdicts, loser.Name)
	if deleted.Action != PruneDelete {
		t.Fatalf("%s: Action = %s, want %s (issue #292 does not change this decision, only its Reason)", loser.Name, deleted.Action, PruneDelete)
	}
	if !strings.Contains(deleted.Reason, "sibling collision") {
		t.Errorf("%s: Reason = %q, want it to mention a sibling collision", loser.Name, deleted.Reason)
	}
	if !strings.Contains(deleted.Reason, winner.Name) {
		t.Errorf("%s: Reason = %q, want it to name %s, the sibling it tied with", loser.Name, deleted.Reason, winner.Name)
	}
}

// --- a store that cannot say where an artifact belongs ---

// TestPruneRefusesWhenItCannotResolveWhereTheArtifactBelongs is issue #390's
// heart. The prune path asks internal/artifactstore where an artifact lives
// instead of composing the path itself, and when the store refuses to answer,
// that refusal has to come out as PruneRefuse.
//
// REFUSE and KEEP are different claims. KEEP says a retention tier selected
// this artifact and the engine decided about it. REFUSE says nothing was
// decided at all. Collapsing the second into the first is how a prune reports
// a decision it never made, and an operator reading a dry run cannot tell the
// two apart from the Action alone.
//
// The fixture is one backup set with an empty local_path and two records: one
// old enough that no tier selects it, and one dated today that the daily tier
// keeps. Running those same two records against a real root first is the
// positive control, and it is what makes the empty-root half mean anything:
// it proves this fixture really does produce a KEEP and a DELETE, so a REFUSE
// in the other half is the missing root talking rather than some unrelated
// safety check refusing everything in sight.
//
// Both rows refuse at the empty root, the kept one included, and that is
// deliberate rather than incidental: pruneEvaluate resolves the path before
// it looks at the verdict, so a store that cannot answer stops the decision
// instead of being papered over by a keep. Path stays empty on a refusal so
// nothing downstream can act on a half-computed one.
func TestPruneRefusesWhenItCannotResolveWhereTheArtifactBelongs(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-unresolvable", "set")

	kept := gfsMustArtifact(t, set, "todays-backup.zst")
	doomed := gfsMustArtifact(t, set, "ancient-backup.zst")
	keptPath := filepath.Join(root, "todays-backup.zst")
	doomedPath := filepath.Join(root, "ancient-backup.zst")
	pruneWriteFile(t, keptPath, "kept by the daily tier")
	pruneWriteFile(t, doomedPath, "old enough that no tier selects it")

	records := []state.Record{
		pruneRecord(kept, lifecycle.Complete, pruneNow, keptPath),
		pruneRecord(doomed, lifecycle.Complete, pruneNow.Add(-365*24*time.Hour), doomedPath),
	}
	cfg := pruneTodayOnlyChain()

	control, err := PruneDecide(pruneNow, cfg, pruneBackupSet(set, root), records)
	if err != nil {
		t.Fatalf("PruneDecide against a real root: %v", err)
	}
	if v := pruneFindVerdict(t, control, "todays-backup.zst"); v.Action != PruneKeep {
		t.Fatalf("control: todays-backup.zst = %s, want %s (reason: %s); the positive control is broken, so the empty-root half below proves nothing", v.Action, PruneKeep, v.Reason)
	}
	if v := pruneFindVerdict(t, control, "ancient-backup.zst"); v.Action != PruneDelete {
		t.Fatalf("control: ancient-backup.zst = %s, want %s (reason: %s); the positive control is broken, so the empty-root half below proves nothing", v.Action, PruneDelete, v.Reason)
	}

	verdicts, err := PruneDecide(pruneNow, cfg, pruneBackupSet(set, ""), records)
	if err != nil {
		t.Fatalf("PruneDecide against an unrooted store: %v", err)
	}
	for _, name := range []string{"ancient-backup.zst", "todays-backup.zst"} {
		v := pruneFindVerdict(t, verdicts, name)
		if v.Action != PruneRefuse {
			t.Errorf("with no configured local_path, %s = %s, want %s: a store that cannot say where the artifact belongs has decided nothing, and reporting that as %s claims a tier selected it (reason: %s)", name, v.Action, PruneRefuse, v.Action, v.Reason)
		}
		if v.Path != "" {
			t.Errorf("%s: Path = %q, want empty: nothing downstream may act on a path the store refused to compute", name, v.Path)
		}
		if !strings.Contains(v.Reason, "local_path") {
			t.Errorf("%s: Reason = %q, which does not name the missing local_path an operator has to fix", name, v.Reason)
		}
	}

	pruneMustExist(t, keptPath)
	pruneMustExist(t, doomedPath)
}

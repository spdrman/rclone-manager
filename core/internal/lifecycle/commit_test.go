package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// openTestJournal opens a real, on-disk, WAL-mode SQLite journal, the same
// one production uses (see internal/state.Open's durability doc), rather
// than a hand-rolled fake. Commit's idempotence is defined entirely in
// terms of the journal's own idempotency-key replay behaviour (see
// CommitInput's Key docs), so a fake that reimplemented that behaviour
// would risk drifting from the real thing and proving nothing.
// openTestJournal lives in remotedelete_test.go. Every file here is one
// package, so a second copy would not compile.

// walkToVerified durably drives artifact through the nominal path from
// DISCOVERED to VERIFIED, recording partial as the local path at
// TRANSFERRING exactly as FR-11 would. It does not touch the filesystem:
// callers create partial themselves, since Commit's own contract is that
// FR-11/FR-13 already finished doing so before Commit is ever called.
func walkToVerified(t *testing.T, ctx context.Context, d Deps, artifact model.ArtifactID, partial string) {
	t.Helper()
	steps := []struct {
		from, to  State
		localPath *string
	}{
		{"", Discovered, nil},
		{Discovered, Transferring, &partial},
		{Transferring, Transferred, nil},
		{Transferred, Verifying, nil},
		{Verifying, Verified, nil},
	}
	for i, s := range steps {
		_, err := Advance(ctx, d, state.Transition{
			Artifact:   artifact,
			Key:        fmt.Sprintf("setup-%d-%s", i, s.to),
			From:       string(s.from),
			To:         string(s.to),
			LocalPath:  s.localPath,
			RemotePath: "/incoming/" + artifact.Name,
		})
		if err != nil {
			t.Fatalf("walkToVerified: Advance %s -> %s: %v", s.from, s.to, err)
		}
	}
}

// The nominal path: a real .partial file, a real journal, one Commit call.
// The .partial and final paths are computed with transfer.go's own
// partialPath/finalPath helpers, exactly as Commit itself computes them, so
// the test is pinned to the real FR-12 naming convention rather than an
// arbitrary literal that could quietly drift from it.
func TestCommitWalksVerifiedToCommitted(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	final := mustFinalPath(t, dir, artifact)
	content := []byte("durable backup content")
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	walkToVerified(t, ctx, d, artifact, partial)

	out, err := Commit(ctx, d, CommitInput{
		Artifact:      artifact,
		LocalDir:      dir,
		CommittingKey: "commit-1-committing",
		CommittedKey:  "commit-1-committed",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !out.Applied {
		t.Fatal("Commit: Applied = false on a fresh commit")
	}
	if out.Record.State != string(Committed) {
		t.Fatalf("Record.State = %q, want COMMITTED", out.Record.State)
	}
	if out.Record.LocalPath != final {
		t.Fatalf("Record.LocalPath = %q, want %q", out.Record.LocalPath, final)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("final content = %q, want %q", got, content)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf(".partial file still present after commit: err=%v", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(Committed) || rec.LocalPath != final {
		t.Fatalf("journal record after commit = %+v", rec)
	}
}

// The crash matrix's hardest window: the file has already been renamed to
// its final name (data fsynced first, per FR-14's ordering) but the
// directory fsync never ran and COMMITTED was never recorded, because the
// process died in between. A restart has to converge on COMMITTED, not
// fail, and not re-run the rename against a .partial file that is already
// gone.
func TestCommitConvergesAfterACrashBetweenRenameAndDirectorySync(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	final := mustFinalPath(t, dir, artifact)
	content := []byte("durable backup content")
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	walkToVerified(t, ctx, d, artifact, partial)

	simulatedCrash := errors.New("simulated crash between rename and directory fsync")
	testHookAfterRename = func() error { return simulatedCrash }
	t.Cleanup(func() { testHookAfterRename = nil })

	in := CommitInput{
		Artifact:      artifact,
		LocalDir:      dir,
		CommittingKey: "crash-committing",
		CommittedKey:  "crash-committed",
	}

	if _, err := Commit(ctx, d, in); !errors.Is(err, simulatedCrash) {
		t.Fatalf("first Commit: err = %v, want it to wrap the simulated crash", err)
	}

	// What the "crash" actually left behind: renamed, .partial gone,
	// directory never fsynced, journal never advanced past COMMITTING.
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("final file missing after the simulated crash: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf(".partial file unexpectedly still present: err=%v", err)
	}
	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(Committing) {
		t.Fatalf("journal state after the simulated crash = %q, want COMMITTING", rec.State)
	}

	// "Restart": a fresh call, with no memory of the one above, and the
	// fault that killed it is gone.
	testHookAfterRename = nil

	out, err := Commit(ctx, d, in)
	if err != nil {
		t.Fatalf("Commit after restart: %v", err)
	}
	if out.Record.State != string(Committed) || out.Record.LocalPath != final {
		t.Fatalf("journal record after restart = %+v", out.Record)
	}
	got, err := os.ReadFile(final)
	if err != nil || string(got) != string(content) {
		t.Fatalf("final content after restart = %q, %v, want %q", got, err, content)
	}
}

// A previous process durably recorded COMMITTING and died before it ever
// touched the filesystem. This is the "before" case in crash_safety.go's
// COMMITTING -> COMMITTED walkthrough: nothing has happened to the file
// yet, so Commit should just run the whole sequence normally from here.
func TestCommitFinishesWhenAnEarlierAttemptOnlyRecordedCommitting(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	if err := os.WriteFile(partial, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	walkToVerified(t, ctx, d, artifact, partial)

	committingKey := "precommitted-committing"
	if _, err := Advance(ctx, d, state.Transition{
		Artifact: artifact, Key: committingKey, From: string(Verified), To: string(Committing),
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	out, err := Commit(ctx, d, CommitInput{
		Artifact:      artifact,
		LocalDir:      dir,
		CommittingKey: committingKey,
		CommittedKey:  "precommitted-committed",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if out.Record.State != string(Committed) || out.Record.LocalPath != mustFinalPath(t, dir, artifact) {
		t.Fatalf("Record after commit = %+v", out.Record)
	}
}

// Calling Commit a second time with the exact same input after it already
// fully succeeded must converge without redoing anything: the .partial file
// is already gone, so if this path mistakenly tried to fsync or link it
// again it would fail outright instead of quietly succeeding.
func TestCommitConvergesWhenCalledAgainAfterFullSuccess(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	if err := os.WriteFile(partial, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	walkToVerified(t, ctx, d, artifact, partial)

	in := CommitInput{
		Artifact: artifact, LocalDir: dir,
		CommittingKey: "twice-committing", CommittedKey: "twice-committed",
	}
	first, err := Commit(ctx, d, in)
	if err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if !first.Applied {
		t.Fatal("first Commit: Applied = false, want true")
	}

	second, err := Commit(ctx, d, in)
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if second.Applied {
		t.Fatal("second Commit: Applied = true, want a converged no-op")
	}
	if second.Record.State != string(Committed) || second.Record.LocalPath != mustFinalPath(t, dir, artifact) {
		t.Fatalf("second Commit record = %+v", second.Record)
	}
}

// A caller (or an operator changing config between steps) whose LocalDir at
// commit time does not match the LocalDir Transfer actually used is a bug,
// not a crash window, and Commit has to say so instead of guessing which
// one is right. Critically, the COMMITTING write itself must still land
// durably before this check runs (recording intent has to come before any
// file I/O, per FR-14's ordering), so a corrected retry using the same
// CommittingKey has to succeed afterwards.
func TestCommitRefusesAMismatchedLocalDirThenRecoversOnRetry(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	correctDir := t.TempDir()
	actualPartial := mustPartialPath(t, correctDir, artifact)
	if err := os.WriteFile(actualPartial, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	walkToVerified(t, ctx, d, artifact, actualPartial)

	wrongDir := t.TempDir()

	_, err := Commit(ctx, d, CommitInput{
		Artifact: artifact, LocalDir: wrongDir,
		CommittingKey: "mismatch-committing", CommittedKey: "mismatch-committed",
	})
	if err == nil {
		t.Fatal("Commit: want an error for a LocalDir that does not match the journal, got none")
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(Committing) {
		t.Fatalf("journal state after the mismatch = %q, want COMMITTING", rec.State)
	}
	if _, statErr := os.Stat(mustPartialPath(t, wrongDir, artifact)); !os.IsNotExist(statErr) {
		t.Fatal("commitFile touched the filesystem despite the mismatch")
	}

	out, err := Commit(ctx, d, CommitInput{
		Artifact: artifact, LocalDir: correctDir,
		CommittingKey: "mismatch-committing", CommittedKey: "mismatch-committed",
	})
	if err != nil {
		t.Fatalf("corrected retry: %v", err)
	}
	if out.Record.State != string(Committed) {
		t.Fatalf("corrected retry record = %+v", out.Record)
	}
}

// Replaying a completed commit with a LocalDir other than the one that
// actually produced the committed final path must fail loudly rather than
// silently report success for the wrong path.
func TestCommitRejectsLocalDirMismatchOnAnAlreadyCommittedRecord(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	if err := os.WriteFile(partial, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	walkToVerified(t, ctx, d, artifact, partial)

	in := CommitInput{
		Artifact: artifact, LocalDir: dir,
		CommittingKey: "k1", CommittedKey: "k2",
	}
	if _, err := Commit(ctx, d, in); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	in.LocalDir = t.TempDir() // a different directory than what was committed
	if _, err := Commit(ctx, d, in); err == nil {
		t.Fatal("Commit: want an error replaying with a different LocalDir than what was committed")
	}
}

// Commit validates its input before it ever reaches the journal: Deps{} has
// a nil Journal, and Advance would panic-free but error on that too, so the
// assertion checks specifically for validate()'s own message to prove
// validation, not Advance's nil-Journal guard, is what caught this.
func TestCommitRefusesInvalidInputBeforeTouchingTheJournal(t *testing.T) {
	_, err := Commit(context.Background(), Deps{}, CommitInput{})
	if err == nil || !strings.Contains(err.Error(), "Commit needs") {
		t.Fatalf("Commit: err = %v, want a CommitInput validation error", err)
	}
}

func TestCommitInputValidate(t *testing.T) {
	artifact := mustID(t)
	base := CommitInput{
		Artifact:      artifact,
		LocalDir:      "/data",
		CommittingKey: "ck",
		CommittedKey:  "dk",
	}
	if err := base.validate(); err != nil {
		t.Fatalf("validate: base input should be valid: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*CommitInput)
	}{
		{"zero artifact", func(in *CommitInput) { in.Artifact = model.ArtifactID{} }},
		{"empty local dir", func(in *CommitInput) { in.LocalDir = "" }},
		{"empty committing key", func(in *CommitInput) { in.CommittingKey = "" }},
		{"empty committed key", func(in *CommitInput) { in.CommittedKey = "" }},
		{"same keys", func(in *CommitInput) { in.CommittedKey = in.CommittingKey }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			c.mutate(&in)
			if err := in.validate(); err == nil {
				t.Fatalf("validate: want an error for %s, got none", c.name)
			}
		})
	}
}

// --- commitFile: pure filesystem-level tests, no journal involved ---

func TestCommitFileRenamesAndFsyncsWithoutError(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "a.partial")
	final := filepath.Join(dir, "a.final")
	content := []byte("hello durable world")
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := commitFile(partial, final); err != nil {
		t.Fatalf("commitFile: %v", err)
	}
	got, err := os.ReadFile(final)
	if err != nil || string(got) != string(content) {
		t.Fatalf("final content = %q, %v, want %q", got, err, content)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal(".partial survived commitFile")
	}
}

// Final present, partial gone: an earlier attempt finished the rename
// before being killed. commitFile must converge, not error.
func TestCommitFileConvergesWhenAlreadyFullyRenamed(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "a.partial")
	final := filepath.Join(dir, "a.final")
	if err := os.WriteFile(final, []byte("already there"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := commitFile(partial, final); err != nil {
		t.Fatalf("commitFile: %v", err)
	}
}

// Both names present and pointing at the same inode: an earlier attempt
// completed the link but was killed before removing the old name.
// commitFile must finish that remove, not error and not re-link.
func TestCommitFileFinishesAnInterruptedRemove(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "a.partial")
	final := filepath.Join(dir, "a.final")
	if err := os.WriteFile(partial, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Link(partial, final); err != nil {
		t.Fatalf("Link: %v", err)
	}

	if err := commitFile(partial, final); err != nil {
		t.Fatalf("commitFile: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal(".partial survived finishing the interrupted remove")
	}
	got, err := os.ReadFile(final)
	if err != nil || string(got) != "content" {
		t.Fatalf("final content = %q, %v", got, err)
	}
}

// FR-12: a final-name collision with an unrelated file must fail safely,
// leaving both files exactly as they were.
func TestCommitFileRefusesToClobberAForeignFinalFile(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "a.partial")
	final := filepath.Join(dir, "a.final")
	if err := os.WriteFile(partial, []byte("new content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(final, []byte("someone else's known-good backup"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := commitFile(partial, final)
	var collision *FinalPathCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("commitFile: err = %v, want *FinalPathCollisionError", err)
	}

	gotPartial, perr := os.ReadFile(partial)
	if perr != nil || string(gotPartial) != "new content" {
		t.Fatalf(".partial content changed: %q, %v", gotPartial, perr)
	}
	gotFinal, ferr := os.ReadFile(final)
	if ferr != nil || string(gotFinal) != "someone else's known-good backup" {
		t.Fatalf("final content changed: %q, %v", gotFinal, ferr)
	}
}

// Neither name exists: not a crash window, actual loss (or a caller bug).
// commitFile must say so rather than treat "nothing to do" as success.
func TestCommitFileReturnsAClearErrorWhenBothFilesAreMissing(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "a.partial")
	final := filepath.Join(dir, "a.final")

	err := commitFile(partial, final)
	var missing *ArtifactFileMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("commitFile: err = %v, want *ArtifactFileMissingError", err)
	}
}

// --- linkWithoutClobbering: exercised directly to reach its own internal
// EEXIST-then-recheck branch, which commitFile's own Stat-based routing
// normally short-circuits around. ---

func TestLinkWithoutClobberingToleratesARaceOntoTheSameInode(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "a.partial")
	final := filepath.Join(dir, "a.final")
	if err := os.WriteFile(partial, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Link(partial, final); err != nil {
		t.Fatalf("Link: %v", err)
	}

	if err := linkWithoutClobbering(partial, final); err != nil {
		t.Fatalf("linkWithoutClobbering: %v", err)
	}
}

func TestLinkWithoutClobberingRefusesAForeignFile(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "a.partial")
	final := filepath.Join(dir, "a.final")
	if err := os.WriteFile(partial, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(final, []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := linkWithoutClobbering(partial, final)
	var collision *FinalPathCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("linkWithoutClobbering: err = %v, want *FinalPathCollisionError", err)
	}
}

// --- small helpers ---

func TestFsyncFileAndFsyncDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fsyncFile(file); err != nil {
		t.Fatalf("fsyncFile: %v", err)
	}
	if err := fsyncDir(dir); err != nil {
		t.Fatalf("fsyncDir: %v", err)
	}
	if err := fsyncFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("fsyncFile: want an error for a missing file")
	}
}

func TestRemoveIfExistsToleratesAnAlreadyGoneFile(t *testing.T) {
	if err := removeIfExists(filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatalf("removeIfExists: %v", err)
	}
}

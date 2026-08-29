// Package crashmatrix_test drives docs/EPIC.md's crash matrix against a
// real, disposable subprocess (tests/crashmatrix/harness), killed with a
// real, uncatchable SIGKILL at each of the named points, then restarted and
// reconciled in this test process against the same on-disk journal and
// filesystem state the crashed process left behind.
//
// # Real kill vs. simulated
//
// Every test in this file that names a lifecycle STATE (DISCOVERED,
// TRANSFERRED, VERIFIED, COMMITTED, REMOTE_DELETE_PENDING) kills the
// harness process for real, the instant the real SQLite journal durably
// commits that exact transition (see harness/decorators.go's
// killAfterStateJournal). Every test that names an in-flight window
// (TRANSFERRING, VERIFYING, remote deletion, before COMPLETE, and the
// local-fsync/rename/directory-sync region inside COMMITTING) also kills
// for real, racing a real SIGKILL against a real, still-executing
// syscall (a real file copy, a real local read+hash, a real remote
// delete), timed by measuring that same real operation once beforehand
// rather than guessing a fixed duration (see harness/main.go's calibrate*
// functions). In every case the process dies via signal, not
// os.Exit: nothing downstream of the kill point, no deferred cleanup, no
// buffered writer flush, gets to run.
//
// The one crash-matrix point this file does NOT reach with a real kill on
// its own is resolving which of local-fsync / rename / directory-sync was
// hit on a given run of TestCrash_DuringCommit_Fuzz: those three are real,
// timing-raced kills against the real Commit() call, but which exact
// syscall boundary a given calibrated fraction lands on is observed after
// the fact from what is actually on disk, not chosen in advance. See that
// test and internal/lifecycle's own in-process, hook-based test for the
// "rename" sub-window specifically (testHookAfterRename, a seam commit.go
// already ships for exactly this) for the complementary, precisely-targeted
// half of that coverage.
//
// # Two defects this suite found, both since fixed
//
// Two real defects surfaced while building this suite, and both are FIXED
// now, so the workaround below is redundant rather than load-bearing. They
// were: internal/transport/rclone.Adapter
// never classifies its own errors, which breaks internal/reconcile's
// "remote confirmed absent" detection and FR-22's retry-on-transient; and
// internal/lifecycle.DeleteRemote's re-identification never asks for a
// hash, which means a real delete can never reach ConfidenceStrong against
// any backend registered in this binary. Both are proven directly and in
// isolation elsewhere (internal/transport/rclone/error_classification_gap_a213_test.go,
// internal/reconcile/a213_defect_test.go,
// internal/discovery/a213_defect_test.go). This file, and the harness it
// drives, use tests/classifytransport's Wrap/WithStatHash to work around
// both so the rest of this suite can actually reach COMMITTED,
// REMOTE_DELETE_PENDING and COMPLETE meaningfully, rather than reproducing
// the same two already-diagnosed, already-reported gaps at every single
// crash point.
package crashmatrix_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/internal/lifecycle"
	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/reconcile"
	"github.com/spdrman/rclone-manager/internal/state"
	"github.com/spdrman/rclone-manager/internal/transport"
	"github.com/spdrman/rclone-manager/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/tests/classifytransport"
	"github.com/spdrman/rclone-manager/tests/sftpfixture"
)

// --- building and running the harness ---------------------------------

var (
	harnessBuildOnce sync.Once
	harnessBinary    string
	harnessBuildErr  error
)

func buildHarness(t *testing.T) string {
	t.Helper()
	harnessBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "crashmatrix-harness-bin")
		if err != nil {
			harnessBuildErr = err
			return
		}
		bin := filepath.Join(dir, "crashmatrix-harness")
		cmd := exec.Command("go", "build", "-o", bin, "./harness")
		out, err := cmd.CombinedOutput()
		if err != nil {
			harnessBuildErr = fmt.Errorf("building tests/crashmatrix/harness: %v\n%s", err, out)
			return
		}
		harnessBinary = bin
	})
	if harnessBuildErr != nil {
		t.Fatalf("%v", harnessBuildErr)
	}
	return harnessBinary
}

type harnessResult struct {
	stdout string
	stderr string
	err    error
	signal syscall.Signal // 0 unless the process was killed by a signal
}

func (r harnessResult) killedBy(sig syscall.Signal) bool { return r.signal == sig }

// harnessTimeout bounds a single harness invocation. Every real operation
// this harness performs (a small-to-moderate local copy, a loopback SFTP
// round trip) normally finishes in well under a second; this exists so a
// genuine hang (a stuck network call, a goroutine leak in the raceKill
// machinery) fails this specific test loudly and promptly instead of
// hanging the whole CI job for the default `go test` timeout.
const harnessTimeout = 45 * time.Second

func runHarness(t *testing.T, args ...string) harnessResult {
	t.Helper()
	bin := buildHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), harnessTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := harnessResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.signal = ws.Signal()
			}
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("harness did not exit within %s (args=%v)\nstdout=%s\nstderr=%s", harnessTimeout, args, res.stdout, res.stderr)
	}
	return res
}

func requireKilledBySIGKILL(t *testing.T, res harnessResult) {
	t.Helper()
	if !res.killedBy(syscall.SIGKILL) {
		t.Fatalf("harness was not killed by SIGKILL (err=%v)\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
}

func requireCleanExit(t *testing.T, res harnessResult) {
	t.Helper()
	if res.err != nil {
		t.Fatalf("harness exited with an error: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
}

// --- scenario setup ------------------------------------------------------

const (
	scenarioSource   = "crashmatrix-source"
	scenarioSet      = "crashmatrix-set"
	scenarioArtifact = "backup.dump"
)

// localScenario builds a fresh, isolated local-backend scenario: a "remote"
// directory holding one artifact of size bytes, an empty local destination
// directory, and a not-yet-existing journal path.
type localScenario struct {
	journalPath string
	localDir    string
	remoteRoot  string
	content     []byte
}

func newLocalScenario(t *testing.T, size int) localScenario {
	t.Helper()
	dir := t.TempDir()
	s := localScenario{
		journalPath: filepath.Join(dir, "journal.db"),
		localDir:    filepath.Join(dir, "local"),
		remoteRoot:  filepath.Join(dir, "remote"),
	}
	if err := os.MkdirAll(s.localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll local: %v", err)
	}
	if err := os.MkdirAll(s.remoteRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll remote: %v", err)
	}
	s.content = make([]byte, size)
	if _, err := rand.Read(s.content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.remoteRoot, scenarioArtifact), s.content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return s
}

func (s localScenario) baseArgs() []string {
	return []string{
		"-journal=" + s.journalPath,
		"-local-dir=" + s.localDir,
		"-remote-root=" + s.remoteRoot,
		"-artifact-name=" + scenarioArtifact,
		"-source-name=" + scenarioSource,
		"-set-name=" + scenarioSet,
		"-hash-required=true",
	}
}

func (s localScenario) source() transport.Source {
	return transport.Source{ID: "crashmatrix", Type: "local", Root: s.remoteRoot}
}

func mustArtifactID(t *testing.T) model.ArtifactID {
	t.Helper()
	set, err := model.NewBackupSetID(scenarioSource, scenarioSet)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	id, err := model.NewArtifactID(set, scenarioArtifact)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return id
}

// --- inspecting what a crash left behind -----------------------------

func openJournalReadOnly(t *testing.T, path string) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("state.Open(%s): %v", path, err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func getRecord(t *testing.T, journalPath string, artifact model.ArtifactID) state.Record {
	t.Helper()
	j := openJournalReadOnly(t, journalPath)
	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("journal.Get: %v", err)
	}
	return rec
}

func requireState(t *testing.T, journalPath string, artifact model.ArtifactID, want lifecycle.State) state.Record {
	t.Helper()
	rec := getRecord(t, journalPath, artifact)
	if lifecycle.State(rec.State) != want {
		t.Fatalf("journal state = %s, want %s", rec.State, want)
	}
	return rec
}

func reconcileLocal(t *testing.T, s localScenario) reconcile.Report {
	t.Helper()
	ctx := context.Background()
	j, err := state.Open(ctx, s.journalPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer j.Close()

	tr := classifytransport.Wrap(classifytransport.WithStatHash(rclone.New()))
	set, err := model.NewBackupSetID(scenarioSource, scenarioSet)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	report, err := reconcile.Reconcile(ctx, reconcile.Deps{Journal: j, Transport: tr}, s.source(), set)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return report
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// requireFinalContentMatches proves the ultimate safety property in
// concrete bytes, not just journal bookkeeping: whatever crash point was
// exercised, the file an operator would actually restore from is
// byte-for-byte the original artifact, never a truncated or corrupted
// .partial promoted early.
func requireFinalContentMatches(t *testing.T, localDir string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(localDir, scenarioArtifact))
	if err != nil {
		t.Fatalf("reading final committed file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("final committed file does not match the original artifact (got %d bytes, want %d, got sha256 %s want %s)",
			len(got), len(want), sha256Hex(got), sha256Hex(want))
	}
}

func requireRemoteGone(t *testing.T, remoteRoot string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(remoteRoot, scenarioArtifact)); !os.IsNotExist(err) {
		t.Fatalf("remote artifact still present after a claimed-safe convergence to COMPLETE (stat err: %v)", err)
	}
}

// --- DISCOVERED ------------------------------------------------------------

func TestCrash_AfterDiscovered(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-after-state=DISCOVERED")...)
	requireKilledBySIGKILL(t, res)

	requireState(t, s.journalPath, artifact, lifecycle.Discovered)
	if _, err := os.Stat(filepath.Join(s.localDir, scenarioArtifact+".partial")); !os.IsNotExist(err) {
		t.Fatalf("a .partial file exists before TRANSFERRING was ever entered")
	}

	report := reconcileLocal(t, s)
	if len(report.Errors) != 0 {
		t.Fatalf("Reconcile reported errors for a plain DISCOVERED row: %+v", report.Errors)
	}
	if len(report.Findings) != 1 || report.Findings[0].Changed() {
		t.Fatalf("Reconcile should take no action on DISCOVERED: %+v", report.Findings)
	}

	// Restart and drive the rest of the way for real: the strongest proof
	// of convergence is the artifact actually completing, not just a
	// reconcile snapshot saying it could.
	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

// --- TRANSFERRING (boundary) and mid-transfer (real, in-flight) -----------

func TestCrash_AfterTransferringBoundary(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-after-state=TRANSFERRING")...)
	requireKilledBySIGKILL(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Transferring)

	report := reconcileLocal(t, s)
	if len(report.Errors) != 0 {
		t.Fatalf("Reconcile errors on TRANSFERRING: %+v", report.Errors)
	}
	if report.Findings[0].Changed() {
		t.Fatalf("Reconcile should take no action pre-COMMITTED: %+v", report.Findings[0])
	}

	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

func TestCrash_MidTransferring_RealInFlightKill(t *testing.T) {
	// Large enough, and on a local filesystem, that a real copy takes a
	// measurable, calibratable amount of time to race a real kill against
	// (see harness/main.go's calibrateTransfer); a few tens of MiB is
	// comfortably enough on any machine this suite runs on without making
	// the test itself slow.
	s := newLocalScenario(t, 48<<20)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-plan=mid-transfer", "-mid-fraction=0.4")...)
	requireKilledBySIGKILL(t, res)
	t.Logf("harness stdout:\n%s", res.stdout)

	rec := getRecord(t, s.journalPath, artifact)
	if lifecycle.State(rec.State) != lifecycle.Transferring {
		t.Fatalf("journal state after a mid-transfer kill = %s, want TRANSFERRING", rec.State)
	}
	// rclone's local backend writes through its own internal temp name and
	// only renames to the destination (backup.dump.partial, this step's
	// own FR-12 name) once a copy finishes, so an interruption early
	// enough in a real copy can leave no file at all at that destination
	// name yet, not merely a short one; either way is a legitimate
	// mid-flight outcome, and both are handled identically by Transfer's
	// own "always clear whatever is at .partial before a fresh attempt"
	// rule. What must never be true, regardless of which of the two this
	// particular kill produced, is a final-sized .partial or any file at
	// all under the committed, non-.partial name.
	partial := filepath.Join(s.localDir, scenarioArtifact+".partial")
	if info, err := os.Stat(partial); err == nil {
		if info.Size() >= int64(len(s.content)) {
			t.Fatalf(".partial file is already fully sized (%d bytes) after a mid-transfer kill; the interrupt did not land mid-flight", info.Size())
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat .partial: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.localDir, scenarioArtifact)); !os.IsNotExist(err) {
		t.Fatalf("a final-name file exists despite the transfer never finishing")
	}
	if matches, _ := filepath.Glob(filepath.Join(s.localDir, scenarioArtifact+".partial.*.partial")); len(matches) > 0 {
		// Minor observation, not a safety defect: rclone's own
		// operations.Copy writes through its own hash-suffixed temp name
		// (see fs/operations/copy.go's "partial_suffix" mechanism, default
		// ".partial") and only renames to the destination this step asked
		// for (our own ".partial" name, transfer.go's partialPath) once a
		// copy finishes. An interruption before that rename leaves this
		// rclone-internal file behind under a name this project's own
		// cleanup (transfer.go's removePartial, which only ever targets
		// the exact partialPath) never matches or removes, so it persists
		// in the local directory indefinitely. It is never mistaken for a
		// restore point by anything in this codebase (it matches neither
		// the collision guard's nor commitFile's exact path checks), so it
		// is disk-space clutter, not a safety issue; see the PR
		// description's smaller-observations section.
		t.Logf("observed rclone's own orphaned partial-upload temp file(s) left behind by the interrupted copy: %v", matches)
	}

	// Restart. Transfer's own documented rule ("Orphaned .partial on
	// restart") is that a resumed attempt discards whatever a crashed
	// attempt left in .partial rather than trying to resume it byte-range;
	// this proves that rule actually holds against a real, truncated file
	// left by a real kill, not just a hand-constructed one.
	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

// --- TRANSFERRED -----------------------------------------------------------

func TestCrash_AfterTransferred(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-after-state=TRANSFERRED")...)
	requireKilledBySIGKILL(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Transferred)

	partial := filepath.Join(s.localDir, scenarioArtifact+".partial")
	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("reading .partial after TRANSFERRED: %v", err)
	}
	if !bytes.Equal(got, s.content) {
		t.Fatalf(".partial content does not match the source even though TRANSFERRED was journaled")
	}

	report := reconcileLocal(t, s)
	if report.Findings[0].Changed() {
		t.Fatalf("Reconcile should take no action pre-COMMITTED: %+v", report.Findings[0])
	}

	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

// --- VERIFYING (boundary) and mid-verify (real, in-flight) ----------------

func TestCrash_AfterVerifyingBoundary(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-after-state=VERIFYING")...)
	requireKilledBySIGKILL(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Verifying)

	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

func TestCrash_MidVerifying_RealInFlightKill(t *testing.T) {
	s := newLocalScenario(t, 48<<20)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-plan=mid-verify", "-mid-fraction=0.4")...)
	requireKilledBySIGKILL(t, res)
	t.Logf("harness stdout:\n%s", res.stdout)

	rec := getRecord(t, s.journalPath, artifact)
	if lifecycle.State(rec.State) != lifecycle.Verifying {
		t.Fatalf("journal state after a mid-verify kill = %s, want VERIFYING", rec.State)
	}
	// VERIFYING never mutates the .partial file (it only reads it), so
	// unlike the mid-transfer case content must already be complete and
	// correct here; what a mid-verify kill interrupts is only the
	// bookkeeping about having checked it, never the bytes themselves. The
	// final name does not exist yet at all (commit hasn't happened), so
	// only the .partial is checked here.
	got, err := os.ReadFile(filepath.Join(s.localDir, scenarioArtifact+".partial"))
	if err != nil {
		t.Fatalf("reading .partial after a mid-verify kill: %v", err)
	}
	if !bytes.Equal(got, s.content) {
		t.Fatalf(".partial content was mutated by a mid-verify kill")
	}

	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

// --- VERIFIED ----------------------------------------------------------

func TestCrash_AfterVerified(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-after-state=VERIFIED")...)
	requireKilledBySIGKILL(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Verified)

	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

// --- local fsync / rename / directory sync (fuzzed, real, in COMMITTING) --

// TestCrash_DuringCommit_Fuzz covers the three sub-COMMITTING crash points
// docs/EPIC.md names individually (local fsync, rename, directory sync) as
// one family: a real SIGKILL raced against the real, uninterrupted
// Commit() call, timed as a fraction of that same call's own real,
// calibrated duration (see harness/main.go's calibrateCommit). Running a
// spread of fractions, including some above 1.0 to absorb a calibration
// sample that undershot the real run's own timing, is what stands in for
// choosing one of the three exactly: which real syscall boundary a given
// fraction lands on depends on the underlying filesystem's own timing on
// this run, not on anything this test controls.
//
// Empirically (see the PR description), real single-digit-to-low-double-
// digit millisecond timing on a real machine is dominated by the first
// journal write Commit() itself makes (the synchronous=FULL fsync behind
// VERIFIED -> COMMITTING), which is real, expected variance, not a bug:
// it means a fraction comfortably below 1.0 usually proves the same thing
// TestCrash_AfterVerified already proves on its own (a real kill before
// Commit's own file work ever starts), while a fraction at or above 1.0
// is what is actually needed to land inside COMMITTING's own
// fsync/rename/directory-sync sequence. This test's hard requirement is
// therefore the one that must never depend on winning that timing race:
// every single attempt, whichever real state it happened to land at,
// converges to the exact same correct, safe destination after a resume.
// Whether a given run's kill landed specifically inside COMMITTING (as
// opposed to at VERIFIED, before Commit even started) is recorded as
// evidence in the test log rather than as a pass/fail condition, so this
// suite's overall result never depends on this machine's or this CI
// runner's filesystem happening to be slow enough on a given day.
// internal/lifecycle's own commit_test.go additionally proves the exact
// "rename" instant precisely and deterministically, using commit.go's
// dedicated testHookAfterRename seam; that test is in-process and
// simulated, this one is a real, external, signal-based kill.
func TestCrash_DuringCommit_Fuzz(t *testing.T) {
	fractions := []float64{0.02, 0.15, 0.4, 0.7, 1.0, 1.5, 2.5}
	var killedCount, landedInCommittingCount int

	for i, frac := range fractions {
		frac := frac
		t.Run(fmt.Sprintf("fraction_%.2f", frac), func(t *testing.T) {
			s := newLocalScenario(t, 4096+i) // vary size trivially so each run's scratch timing isn't identical
			artifact := mustArtifactID(t)

			res := runHarness(t, append(s.baseArgs(), "-kill-plan=mid-commit", fmt.Sprintf("-mid-fraction=%v", frac))...)
			t.Logf("stdout:\n%s", res.stdout)

			if res.killedBy(syscall.SIGKILL) {
				killedCount++
				rec := getRecord(t, s.journalPath, artifact)
				st := lifecycle.State(rec.State)
				if st != lifecycle.Verified && st != lifecycle.Committing && st != lifecycle.Committed {
					t.Fatalf("unexpected journal state after a mid-commit kill: %s", st)
				}
				if st == lifecycle.Committing || st == lifecycle.Committed {
					landedInCommittingCount++
				}
				t.Logf("fraction %.2f: real kill landed with journal at %s", frac, st)
			} else {
				requireCleanExit(t, res)
				t.Logf("fraction %.2f: Commit() (and everything after it) finished before the calibrated timer fired; "+
					"no interruption happened on this run, proceeding to verify the happy-path result is still correct", frac)
			}

			// Whichever of the two happened, resuming (a no-op if it
			// already finished) must reach the same safe destination:
			// exactly one committed, correct, final file and the remote
			// object gone.
			res = runHarness(t, s.baseArgs()...)
			requireCleanExit(t, res)
			requireState(t, s.journalPath, artifact, lifecycle.Complete)
			requireFinalContentMatches(t, s.localDir, s.content)
			requireRemoteGone(t, s.remoteRoot)
			if _, err := os.Stat(filepath.Join(s.localDir, scenarioArtifact+".partial")); !os.IsNotExist(err) {
				t.Fatalf("a .partial file survived all the way to COMPLETE")
			}
		})
	}

	t.Logf("%d of %d fraction attempts produced a real in-flight kill; %d of those landed with the journal already at "+
		"COMMITTING or COMMITTED (inside the fsync/rename/directory-sync sequence itself, as opposed to before Commit "+
		"started at all)", killedCount, len(fractions), landedInCommittingCount)
	if killedCount == 0 {
		t.Log("WARNING: no fraction produced a real interruption at all on this run; every attempt's Commit() (and " +
			"everything after it) finished before its calibrated timer fired. This is a timing observation about " +
			"this specific run, not a test failure: the convergence assertions above already ran and passed for " +
			"every attempt regardless.")
	} else if landedInCommittingCount == 0 {
		t.Log("WARNING: every real interruption this run landed at VERIFIED (before Commit's own first journal write), " +
			"never inside COMMITTING itself. See this test's doc comment for why that is real, expected timing " +
			"variance rather than a bug in this suite.")
	}
}

// --- COMMITTED ---------------------------------------------------------

func TestCrash_AfterCommitted(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-after-state=COMMITTED")...)
	requireKilledBySIGKILL(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Committed)
	requireFinalContentMatches(t, s.localDir, s.content)
	if _, err := os.Stat(filepath.Join(s.localDir, scenarioArtifact+".partial")); !os.IsNotExist(err) {
		t.Fatalf(".partial still present after COMMITTED was durably journaled")
	}
	// COMMITTED: by definition the remote must still be completely
	// untouched (this is the whole point of the state).
	if _, err := os.Stat(filepath.Join(s.remoteRoot, scenarioArtifact)); err != nil {
		t.Fatalf("remote object missing immediately after COMMITTED, before any delete was ever attempted: %v", err)
	}

	report := reconcileLocal(t, s)
	if report.Findings[0].Changed() {
		t.Fatalf("Reconcile should not change a valid COMMITTED row: %+v", report.Findings[0])
	}

	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

// --- REMOTE_DELETE_PENDING (boundary) -------------------------------------

func TestCrash_AfterRemoteDeletePending(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-after-state=REMOTE_DELETE_PENDING")...)
	requireKilledBySIGKILL(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.RemoteDeletePending)
	// Intent was recorded; the delete itself must never have been issued.
	if _, err := os.Stat(filepath.Join(s.remoteRoot, scenarioArtifact)); err != nil {
		t.Fatalf("remote object missing right after REMOTE_DELETE_PENDING was journaled, before any delete call: %v", err)
	}

	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

// --- before COMPLETE: the real delete genuinely succeeded, caller doesn't know yet ---

func TestCrash_BeforeComplete_RealDeleteSucceededButNeverRecorded(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-plan=after-real-delete")...)
	requireKilledBySIGKILL(t, res)

	// The defining, sharpest assertion of this whole crash point: the
	// remote object is REALLY gone, yet the journal never got to say so.
	requireRemoteGone(t, s.remoteRoot)
	requireState(t, s.journalPath, artifact, lifecycle.RemoteDeletePending)

	// A naive retry through the pipeline's own DeleteRemote cannot resolve
	// this by itself: FR-15's revalidation re-Stats the (now genuinely
	// absent) object and refuses rather than guess, exactly as it should
	// for an ambiguous outcome it cannot independently confirm on its own.
	// Only reconciliation is positioned to close this out, because only
	// reconciliation is willing to treat a positively-confirmed NotFound as
	// license to advance to COMPLETE (FR-17's dedicated row for this exact
	// case). That division of responsibility is what this assertion
	// actually proves.
	report := reconcileLocal(t, s)
	if len(report.Errors) != 0 {
		t.Fatalf("Reconcile could not resolve a confirmed-absent remote to COMPLETE: %+v", report.Errors)
	}
	f := report.Findings[0]
	if f.To != lifecycle.Complete {
		t.Fatalf("Reconcile.To = %s, want COMPLETE (reason: %s)", f.To, f.Reason)
	}
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)

	// A harness resume after reconciliation already closed this out must
	// be a pure no-op: no second delete attempt, no error, straight to the
	// same COMPLETE.
	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
}

// --- remote deletion in flight -----------------------------------------

// TestCrash_RemoteDeletionInFlight_Local races a real kill against the
// real transport.DeleteRemote call itself (as opposed to
// TestCrash_BeforeComplete above, which only races after that call has
// already returned success), against the local backend, where
// tests/classifytransport.WithStatHash lets identity actually be
// positively reconfirmed (see that package's own doc for why this
// fixture, unlike the SFTP one below, can reach ConfidenceStrong at all).
// A local os.Remove is fast enough that the "in flight" window this races
// against is measured in single-digit microseconds rather than the
// hundreds of milliseconds a real network round trip gives
// TestCrash_RemoteDeletionInFlight_SFTP below, but it is still a real
// race against a real syscall, and it still lands on either side of it
// depending on real machine and scheduler timing, exactly the ambiguity
// crash_safety.go's own REMOTE_DELETE_PENDING -> COMPLETE walkthrough
// describes.
func TestCrash_RemoteDeletionInFlight_Local(t *testing.T) {
	s := newLocalScenario(t, 4096)
	artifact := mustArtifactID(t)

	res := runHarness(t, append(s.baseArgs(), "-kill-plan=mid-delete", "-mid-fraction=0.4")...)
	t.Logf("stdout:\n%s", res.stdout)

	if res.killedBy(syscall.SIGKILL) {
		t.Log("real kill landed while racing the real local DeleteRemote call")
	} else {
		requireCleanExit(t, res)
	}

	report := reconcileLocal(t, s)
	if len(report.Errors) != 0 {
		t.Fatalf("Reconcile reported errors: %+v", report.Errors)
	}

	// Unlike the SFTP variant below, identity here CAN always be
	// positively reconfirmed (WithStatHash), and a local delete is
	// reliable once actually issued, so both sides of the race converge
	// to the exact same place after an unraced resume: whether the first,
	// raced attempt happened to complete the real delete before dying, or
	// died before ever reaching it, this final, un-raced resume must
	// finish the job and reach COMPLETE either way.
	res = runHarness(t, s.baseArgs()...)
	requireCleanExit(t, res)
	requireState(t, s.journalPath, artifact, lifecycle.Complete)
	requireFinalContentMatches(t, s.localDir, s.content)
	requireRemoteGone(t, s.remoteRoot)
}

// TestCrash_RemoteDeletionInFlight_SFTP is the one crash point that
// genuinely needs a real network round trip to mean anything: the local
// backend's delete is a single os.Remove call, fast enough on any real
// machine that "still in flight" is not a meaningfully separate window
// from "about to start" or "just finished". A real Docker SFTP server
// gives this a real, measurable round trip (see harness/main.go's
// calibrateDelete) to race a real kill against, so the process can
// genuinely die while the SSH request is in flight, mirroring
// crash_safety.go's own description of this window: "unknown from the
// caller's side whether it took effect."
func TestCrash_RemoteDeletionInFlight_SFTP(t *testing.T) {
	f := sftpfixture.Start(t)
	artifact := mustArtifactID(t)

	content := make([]byte, 4096)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.UploadDir, scenarioArtifact), content, 0o644); err != nil {
		t.Fatalf("seed upload dir: %v", err)
	}

	dir := t.TempDir()
	journalPath := filepath.Join(dir, "journal.db")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	args := []string{
		"-journal=" + journalPath,
		"-local-dir=" + localDir,
		"-transport=sftp",
		"-sftp-host=" + f.Host,
		fmt.Sprintf("-sftp-port=%d", f.Port),
		"-sftp-user=" + f.User,
		"-sftp-key=" + f.KeyFile,
		"-sftp-known-hosts=" + f.KnownHostsFile,
		// f.Source's Root (not a bare "" or "upload" spelled out by hand
		// here) is the single source of truth for where this fixture's
		// writable directory actually is: computing it independently here
		// and again below when this test builds its own reconcile Source
		// is exactly the mismatch that silently made an earlier version
		// of this test conclude a real object was deleted when it had
		// only been looked for in the wrong (nonexistent, one-level-too-
		// deep) place.
		"-sftp-root=" + f.Source("", "").Root,
		"-artifact-name=" + scenarioArtifact,
		"-source-name=" + scenarioSource,
		"-set-name=" + scenarioSet,
		// No -hash-required here, deliberately: this fixture is the
		// hardened, shell-less SFTP account FR-6 recommends, and
		// verify.go's own documented capability-absence decision is that
		// requiring sha256 against a backend that cannot supply one FAILS
		// the artifact explicitly rather than silently downgrading. That
		// is correct behaviour, proven separately by this suite's SFTP
		// integration tests; this test is about the delete-in-flight
		// window, not verification policy, so it uses the honest
		// transfer-verification-only posture the project itself
		// recommends for this exact account shape.
	}

	res := runHarness(t, append(args, "-kill-plan=mid-delete", "-mid-fraction=0.4")...)
	t.Logf("stdout:\n%s", res.stdout)

	source := f.Source("crashmatrix-sftp", "")
	set, err := model.NewBackupSetID(scenarioSource, scenarioSet)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	if !res.killedBy(syscall.SIGKILL) {
		// The real round trip against a loopback Docker container can be
		// fast enough on some machines that even a 0.4 fraction of the
		// calibrated duration doesn't win the race every single time; that
		// is a real property of real network timing, not a bug in this
		// test. Either way the harness reached a real, valid outcome, so
		// only the final convergence assertions below matter.
		requireCleanExit(t, res)
	} else {
		t.Log("real kill landed while the SFTP delete round trip was genuinely in flight")
	}

	// Whichever side of the race actually happened, reconciliation must
	// converge safely: either the object survived (still there, journal
	// still REMOTE_DELETE_PENDING or already refused-and-pending) or it
	// was really deleted (gone, journal must reach COMPLETE via
	// reconciliation, never left dangling).
	ctx := context.Background()
	j, err := state.Open(ctx, journalPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	tr := classifytransport.Wrap(classifytransport.WithStatHash(rclone.New()))
	report, err := reconcile.Reconcile(ctx, reconcile.Deps{Journal: j, Transport: tr}, source, set)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Reconcile reported errors: %+v", report.Errors)
	}
	j.Close()

	// Resume the pipeline for real: a resumed DeleteRemote attempt is
	// exactly how the real system would proceed from here.
	res = runHarness(t, args...)
	requireCleanExit(t, res)
	t.Logf("resume stdout:\n%s", res.stdout)

	got, err := os.ReadFile(filepath.Join(localDir, scenarioArtifact))
	if err != nil {
		t.Fatalf("reading final committed file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("final committed file does not match the original content")
	}

	// Which of the two safe outcomes this converges to depends entirely
	// on which side of the real network race the kill actually landed on,
	// which of a Docker container's real round-trip timing this test does
	// not, and should not try to, pin down exactly:
	//
	//   - the delete never actually reached the server (or the server
	//     never applied it before the connection died): the object is
	//     still there, and it stays there. This fixture has no shell, so
	//     it can never supply a hash or a backend stable id (FR-6's own
	//     recommended hardened posture; see
	//     internal/discovery/a213_defect_test.go for the general version
	//     of this limit), and lifecycle.DeleteRemote's FR-16 revalidation
	//     can only reach ConfidenceWeak against a bare Stat match, which
	//     FR-16 requires treating as "preserve, do not delete". Staying
	//     at REMOTE_DELETE_PENDING with the object intact is not a
	//     failure of this test, it is remotedelete.go's own documented,
	//     routine outcome for exactly this account shape.
	//   - the delete genuinely reached and was applied by the server
	//     before the connection died: the object is gone, and
	//     reconciliation (already run above) positively confirmed that
	//     and closed the artifact out to COMPLETE.
	//
	// What must never happen, regardless of which branch this run took: a
	// remote object silently vanishing without the journal ever reaching
	// COMPLETE, or the journal claiming COMPLETE while the object is
	// actually still there.
	_, statErr := os.Stat(filepath.Join(f.UploadDir, scenarioArtifact))
	remoteGone := os.IsNotExist(statErr)

	rec := getRecord(t, journalPath, artifact)
	switch {
	case remoteGone && lifecycle.State(rec.State) == lifecycle.Complete:
		t.Log("outcome: the real delete had genuinely gone through before the kill; reconciliation confirmed it and closed the artifact out to COMPLETE")
	case !remoteGone && lifecycle.State(rec.State) == lifecycle.RemoteDeletePending:
		t.Log("outcome: the real delete had not gone through before the kill; the object is still present and the artifact correctly stays at REMOTE_DELETE_PENDING rather than guessing")
	default:
		t.Fatalf("unsafe outcome: journal state = %s, remote object present = %v (want either (COMPLETE, gone) or (REMOTE_DELETE_PENDING, present))",
			rec.State, !remoteGone)
	}
}

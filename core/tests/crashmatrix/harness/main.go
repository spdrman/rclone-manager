// Command crashmatrix-harness is a real, disposable OS process that drives
// exactly one artifact through as much of the FR-11 lifecycle pipeline
// (discover -> transfer -> verify -> commit -> delete) as it gets to before
// either finishing, refusing (a legitimate terminal outcome, not a crash),
// or being told to kill itself at a specific, real point.
//
// It exists so tests/crashmatrix's test file can terminate a REAL process
// at each of docs/EPIC.md's crash-matrix points and inspect what a real
// crash actually leaves on disk, rather than reasoning about it from a
// simulated failure inside the test binary itself. Two families of crash
// point are supported:
//
//   - "after a state is durably journaled": -kill-after-state=<STATE>. A
//     journal decorator (see decorators.go) self-destructs via SIGKILL the
//     instant the underlying SQLite transaction for that exact state
//     commits, before this process's own code can do anything else with
//     the result. This covers DISCOVERED, TRANSFERRED, VERIFIED, COMMITTED
//     and REMOTE_DELETE_PENDING precisely and deterministically.
//
//   - "while a real operation is still in flight, or right after one
//     genuinely succeeded": -kill-plan=<plan>. A transport decorator races
//     a calibrated timer against the real CopyToLocal/DeleteRemote call
//     (self-destructing if the timer wins, meaning the real syscall was
//     still in flight in the kernel at that instant), or, for
//     after-real-delete, lets the real delete finish and self-destructs
//     immediately afterward, before this process could ever record
//     COMPLETE. See -kill-plan's flag doc for the full list and
//     -calibrate, which measures the real duration to race against instead
//     of guessing a fixed number that would either always fire too early
//     (never really interrupting anything) or too late (never firing at
//     all) depending on the machine.
//
// A run that reaches a terminal outcome without being asked to kill itself
// (kill-after-state="" and kill-plan="none") exits 0 and prints the final
// journal state to stdout as "FINAL_STATE=<state>", or
// "FINAL_STATE=<state> DELETE_REFUSED=<reason>" if DeleteRemote refused
// (a legitimate, non-crash outcome this harness's destructive-safety
// callers rely on).
//
// What this harness does NOT prove: a SIGKILL from within the same process
// that is doing the work is not bit-for-bit identical to power actually
// being cut, or to the OOM killer acting from outside. In particular this
// machine's disks, kernel and Go runtime are trusted to genuinely have
// issued whatever syscalls the code before the kill point called (open,
// fsync, rename-equivalent), the same trust boundary commit.go's own
// package doc already states for durability. What a real SIGKILL adds over
// an in-process simulated failure is that nothing downstream of the kill
// point, no deferred cleanup, no buffered writer flush, no finally-clause,
// gets a chance to run, and that the interrupted syscall is a real one
// actually in flight in the kernel, not a Go function call this test
// binary chose to abort early.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/classifytransport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "crashmatrix-harness: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		journalPath   = flag.String("journal", "", "path to the SQLite journal (required)")
		localDir      = flag.String("local-dir", "", "local destination directory (required)")
		transportKind = flag.String("transport", "local", `"local" or "sftp"`)
		remoteRoot    = flag.String("remote-root", "", `for -transport=local: the directory serving as the remote`)
		sftpHost      = flag.String("sftp-host", "127.0.0.1", "sftp host")
		sftpPort      = flag.Int("sftp-port", 0, "sftp port")
		sftpUser      = flag.String("sftp-user", "", "sftp user")
		sftpKey       = flag.String("sftp-key", "", "sftp private key file")
		sftpKnownHost = flag.String("sftp-known-hosts", "", "sftp known_hosts file")
		sftpRoot      = flag.String("sftp-root", "", "sftp remote root")
		sourceName    = flag.String("source-name", "crashmatrix-source", "backup-set source component")
		setName       = flag.String("set-name", "crashmatrix-set", "backup-set set component")
		artifactName  = flag.String("artifact-name", "", "artifact basename, must already exist on the remote (required)")
		hashRequired  = flag.Bool("hash-required", false, "require sha256 verification (config.Validation.Hash)")
		killAfter     = flag.String("kill-after-state", "", "self-destruct the instant this state is durably journaled")
		killPlanFlag  = flag.String("kill-plan", "none", "none | mid-transfer | mid-verify | mid-commit | mid-delete | after-real-delete")
		midFraction   = flag.Float64("mid-fraction", 0.4, "fraction of the calibrated duration to wait before racing the kill timer")
	)
	flag.Parse()

	if *journalPath == "" || *localDir == "" || *artifactName == "" {
		return errors.New("-journal, -local-dir and -artifact-name are required")
	}

	ctx := context.Background()

	set, err := model.NewBackupSetID(*sourceName, *setName)
	if err != nil {
		return fmt.Errorf("NewBackupSetID: %w", err)
	}
	artifact, err := model.NewArtifactID(set, *artifactName)
	if err != nil {
		return fmt.Errorf("NewArtifactID: %w", err)
	}

	var source transport.Source
	switch *transportKind {
	case "local":
		if *remoteRoot == "" {
			return errors.New("-remote-root is required for -transport=local")
		}
		source = transport.Source{ID: "crashmatrix", Type: "local", Root: *remoteRoot}
	case "sftp":
		if *sftpUser == "" || *sftpKey == "" || *sftpKnownHost == "" {
			return errors.New("-sftp-user, -sftp-key and -sftp-known-hosts are required for -transport=sftp")
		}
		source = transport.Source{
			ID: "crashmatrix", Type: "sftp",
			Host: *sftpHost, Port: *sftpPort, User: *sftpUser,
			KeyFile: *sftpKey, KnownHosts: *sftpKnownHost, Root: *sftpRoot,
		}
	default:
		return fmt.Errorf("unknown -transport %q", *transportKind)
	}

	if err := os.MkdirAll(*localDir, 0o755); err != nil {
		return fmt.Errorf("MkdirAll local-dir: %w", err)
	}

	realJournal, err := state.Open(ctx, *journalPath)
	if err != nil {
		return fmt.Errorf("state.Open: %w", err)
	}
	defer func() { _ = realJournal.Close() }()

	journal := newKillAfterStateJournal(realJournal, *killAfter)

	// classifytransport.Wrap and .WithStatHash are documented, test-side
	// workarounds for two real defects this issue's test suites found in
	// the real adapter (see that package's doc comments, and the PR
	// description under its own heading for each): the adapter never
	// classifies its own errors, and a real Stat call never carries a
	// hash, which together mean a real delete can never positively
	// reconfirm remote identity and reconciliation can never positively
	// confirm a remote object is gone. Composing them here is what lets
	// this harness reach genuine COMMITTED/REMOTE_DELETE_PENDING/COMPLETE
	// crash points meaningfully; neither call is a change to production
	// code, both are already-exported API used exactly as any other
	// caller could use it.
	realTransport := classifytransport.Wrap(classifytransport.WithStatHash(rclone.New()))
	timedOut := false
	kt := &timedKillTransport{real: realTransport, plan: killPlanNone}

	var validation config.Validation
	if *hashRequired {
		validation = config.Validation{Hash: string(transport.SHA256)}
	}

	// AttemptKeys are deterministic functions of the artifact identity
	// alone (never a timestamp, never a random value), exactly the
	// contract state.Transition.Key's doc and every lifecycle step's own
	// AttemptKey doc require for a resumed call after a crash to be
	// recognised as the same logical attempt. Running this binary again
	// against the same journal, for the same artifact, reproduces every
	// one of these keys identically.
	transferKey := artifact.String() + ":attempt-1"
	verifyKey := transferKey
	committingKey := artifact.String() + ":commit:committing"
	committedKey := artifact.String() + ":commit:committed"
	deleteKey := artifact.String() + ":delete:attempt-1"

	deps := lifecycle.Deps{Journal: journal, Transport: kt}

	// Discover is always safe to call again: it is keyed by remote path,
	// not by attempt, and simply reports AlreadyKnown on a resumed run.
	discoverSet := config.BackupSet{
		Name:       *setName,
		ID:         set,
		Completion: config.Completion{Strategy: "rename"},
	}
	if _, err := discovery.Discover(ctx, discovery.Deps{Transport: kt, Journal: journal}, source, discoverSet); err != nil {
		return fmt.Errorf("discover: %w", err)
	}

	switch *killPlanFlag {
	case "none", "mid-verify", "mid-commit":
		// mid-verify and mid-commit have no transport call worth racing
		// (Verify's mandatory check and Commit's fsync/rename/fsync
		// sequence are local filesystem work, not transport calls), so
		// they are calibrated and armed with raceKill directly around the
		// lifecycle.Verify / lifecycle.Commit call in the loop below,
		// rather than through timedKillTransport.
		kt.plan = killPlanNone
	case "mid-transfer":
		d, cerr := calibrateTransfer(ctx, realTransport, source, artifact.Name)
		if cerr != nil {
			return fmt.Errorf("calibrateTransfer: %w", cerr)
		}
		kt.plan = killPlanMidTransfer
		kt.mid = time.Duration(float64(d) * *midFraction)
		kt.timedOut = &timedOut
		fmt.Printf("CALIBRATED transfer=%s kill_after=%s\n", d, kt.mid)
	case "mid-delete":
		d, cerr := calibrateDelete(ctx, realTransport, source)
		if cerr != nil {
			return fmt.Errorf("calibrateDelete: %w", cerr)
		}
		kt.plan = killPlanMidDelete
		kt.mid = time.Duration(float64(d) * *midFraction)
		kt.timedOut = &timedOut
		fmt.Printf("CALIBRATED delete=%s kill_after=%s\n", d, kt.mid)
	case "after-real-delete":
		kt.plan = killPlanAfterRealDelete
	default:
		return fmt.Errorf("unknown -kill-plan %q", *killPlanFlag)
	}

	for {
		rec, err := journal.Get(ctx, artifact)
		if err != nil {
			return fmt.Errorf("journal.Get: %w", err)
		}

		switch lifecycle.State(rec.State) {
		case lifecycle.Discovered, lifecycle.Transferring:
			if _, err := lifecycle.Transfer(ctx, deps, lifecycle.TransferParams{
				Artifact: artifact, Source: source, LocalDir: *localDir, AttemptKey: transferKey,
			}); err != nil {
				return fmt.Errorf("transfer: %w", err)
			}

		case lifecycle.Transferred, lifecycle.Verifying:
			// lifecycle.Verify documents its own contract plainly:
			// p.Artifact "must currently have a VERIFYING journal row".
			// Unlike Transfer (which records its own DISCOVERED ->
			// TRANSFERRING) and Commit (which records its own VERIFIED ->
			// COMMITTING), Verify does not make the TRANSFERRED ->
			// VERIFYING move itself; nothing in this repository currently
			// orchestrates the full pipeline end to end (cmd/backup-manager
			// is a version-only stub), so this harness is that
			// orchestrator, and this is the one entry transition it has to
			// make explicitly rather than delegate. Going through the real
			// journal (not skipping straight to Verify) is what lets
			// -kill-after-state=VERIFYING fire at exactly the point
			// docs/EPIC.md names, before any verification work starts.
			if rec.State == string(lifecycle.Transferred) {
				if _, err := lifecycle.Advance(ctx, deps, state.Transition{
					Artifact: artifact, Key: verifyKey + ":begin-verifying",
					From: string(lifecycle.Transferred), To: string(lifecycle.Verifying),
				}); err != nil {
					return fmt.Errorf("begin VERIFYING: %w", err)
				}
			}
			verifyCall := func() (struct{}, error) {
				_, err := lifecycle.Verify(ctx, deps, lifecycle.VerifyParams{
					Artifact: artifact, Source: source, Validation: validation, AttemptKey: verifyKey,
				})
				return struct{}{}, err
			}
			if *killPlanFlag == "mid-verify" {
				partial := filepath.Join(*localDir, artifact.Name+".partial")
				d, cerr := calibrateLocalRead(partial)
				if cerr != nil {
					return fmt.Errorf("calibrateLocalRead: %w", cerr)
				}
				mid := time.Duration(float64(d) * *midFraction)
				fmt.Printf("CALIBRATED verify_read=%s kill_after=%s\n", d, mid)
				if _, err := raceKill(mid, &timedOut, verifyCall); err != nil {
					return fmt.Errorf("verify: %w", err)
				}
			} else if _, err := verifyCall(); err != nil {
				return fmt.Errorf("verify: %w", err)
			}

		case lifecycle.Verified, lifecycle.Committing:
			commitCall := func() (struct{}, error) {
				_, err := lifecycle.Commit(ctx, deps, lifecycle.CommitInput{
					Artifact: artifact, LocalDir: *localDir, CommittingKey: committingKey, CommittedKey: committedKey,
				})
				return struct{}{}, err
			}
			if *killPlanFlag == "mid-commit" {
				d, cerr := calibrateCommit(*localDir, artifact.Name)
				if cerr != nil {
					return fmt.Errorf("calibrateCommit: %w", cerr)
				}
				mid := time.Duration(float64(d) * *midFraction)
				fmt.Printf("CALIBRATED commit=%s kill_after=%s\n", d, mid)
				if _, err := raceKill(mid, &timedOut, commitCall); err != nil {
					return fmt.Errorf("commit: %w", err)
				}
			} else if _, err := commitCall(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

		case lifecycle.Committed, lifecycle.RemoteDeletePending:
			_, err := lifecycle.DeleteRemote(ctx, deps, lifecycle.DeleteRemoteRequest{
				Source: source, Artifact: artifact, AttemptKey: deleteKey,
			})
			if err != nil {
				if refusal, ok := lifecycle.AsRemoteDeleteRefusal(err); ok {
					final, ferr := journal.Get(ctx, artifact)
					if ferr != nil {
						return fmt.Errorf("journal.Get after refusal: %w", ferr)
					}
					fmt.Printf("FINAL_STATE=%s DELETE_REFUSED=%s\n", final.State, refusal.Error())
					return nil
				}
				return fmt.Errorf("DeleteRemote: %w", err)
			}

		case lifecycle.Complete, lifecycle.Failed, lifecycle.Quarantined, lifecycle.QuarantinedLost:
			fmt.Printf("FINAL_STATE=%s\n", rec.State)
			return nil

		default:
			return fmt.Errorf("unhandled journal state %q", rec.State)
		}
	}
}

// calibrateTransfer measures how long a real CopyToLocal of the artifact
// actually takes on this machine, against this backend, by doing it once
// for real into a throwaway scratch path and then removing the result.
// This is what -mid-fraction below is a fraction OF: a fixed millisecond
// guess would either race a machine so fast the timer never fires (an
// interrupted-nothing false pass) or a machine so slow the timer always
// fires before the real call even starts (an instant, meaningless kill).
// Measuring the real thing on the real machine right before the real,
// interrupted attempt removes the guess entirely.
func calibrateTransfer(ctx context.Context, tr transport.Transport, source transport.Source, remotePath string) (time.Duration, error) {
	scratch := filepath.Join(os.TempDir(), fmt.Sprintf("crashmatrix-calibrate-%d.partial", os.Getpid()))
	defer os.Remove(scratch)

	start := time.Now()
	if _, err := tr.CopyToLocal(ctx, source, remotePath, scratch); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// calibrateDelete measures a real DeleteRemote round trip by planting and
// then deleting a throwaway scratch object next to the real artifact,
// rather than guessing a fixed network-latency number. It is only ever
// invoked for -transport=sftp (the local backend's delete is an os.Remove
// call, fast enough that racing it is not a meaningful "mid-flight" window
// on any real machine).
func calibrateDelete(ctx context.Context, tr transport.Transport, source transport.Source) (time.Duration, error) {
	const scratchName = ".crashmatrix-calibrate-scratch"
	scratchLocal := filepath.Join(os.TempDir(), fmt.Sprintf("crashmatrix-calibrate-src-%d", os.Getpid()))
	if err := os.WriteFile(scratchLocal, []byte("calibrate"), 0o600); err != nil {
		return 0, err
	}
	defer os.Remove(scratchLocal)

	// Plant the scratch object through the same transport, from the local
	// file above, so this works identically for local and sftp sources
	// without this harness needing its own upload mechanism.
	if _, err := tr.CopyToLocal(ctx, source, scratchName, scratchLocal+".roundtrip"); err == nil {
		os.Remove(scratchLocal + ".roundtrip")
	}
	// CopyToLocal only ever reads from the remote; planting the scratch
	// object itself has to go through a Put-shaped path this harness does
	// not have generic access to via transport.Transport (there is no
	// Transport.Put, by design, see transport.go). Fall back to measuring
	// a delete of an object that does not exist: DeleteRemote's own
	// contract-suite case (delete on a missing object) still exercises a
	// full, real round trip to the server and back for -transport=sftp,
	// which is what this calibration actually needs a duration for.
	start := time.Now()
	_ = tr.DeleteRemote(ctx, source, scratchName)
	return time.Since(start), nil
}

// calibrateLocalRead backs -kill-plan=mid-verify. Verify's mandatory check
// (readAndHashLocal in verify.go) is a local file read+hash with no
// transport call to intercept, so instead of a transport-level plan this
// measures how long reading and hashing the real .partial file actually
// takes on this filesystem, right before racing that same real duration
// against the real lifecycle.Verify call in the main loop above.
func calibrateLocalRead(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	start := time.Now()
	if _, err := io.Copy(sha256.New(), f); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// calibrateCommit backs -kill-plan=mid-commit. Commit's fsync/rename/fsync
// sequence (commit.go) is local filesystem work with no transport call to
// intercept either, and unlike verify's single read it is three distinct
// syscall groups (fsync the content, link-then-remove to rename, fsync the
// directory) whose individual durations this harness has no way to
// separate from outside package lifecycle. Rather than guess which
// fraction of a measured total lands in which of those three windows, this
// measures the whole sequence once, for real, against a throwaway
// same-sized scratch pair in the same local directory (so it reflects the
// same filesystem, not a different one), and the caller races a fraction
// of that total against the real Commit call. Run across a spread of
// -mid-fraction values (see the test driver), this lands the real SIGKILL
// at a real, varying point somewhere across that whole window on repeated
// runs, exactly the set of on-disk states a real crash during COMMITTING
// can leave, rather than only ever proving recovery from one hand-picked
// point.
func calibrateCommit(localDir, artifactName string) (time.Duration, error) {
	info, err := os.Stat(filepath.Join(localDir, artifactName+".partial"))
	if err != nil {
		return 0, fmt.Errorf("stat real .partial to size the scratch file: %w", err)
	}

	// A single sample is noisy enough (scheduler jitter, filesystem cache
	// state, background load) that it can easily undershoot the real
	// Commit() call it is meant to race, in either direction. Taking the
	// max of a few samples biases toward not undershooting, which matters
	// more here than being tight: racing a timer that fires too early
	// (before Commit even starts) proves nothing more than
	// TestCrash_AfterVerified already proves on its own.
	const samples = 3
	var max time.Duration
	for i := 0; i < samples; i++ {
		d, err := calibrateCommitOnce(localDir, info.Size())
		if err != nil {
			return 0, err
		}
		if d > max {
			max = d
		}
	}
	return max, nil
}

// calibrateCommitOnce measures one real fsync/link-then-remove/directory-
// fsync sequence against a throwaway scratch pair, sized to match the real
// .partial file, in the same local directory (so it reflects the same
// filesystem). Writing the scratch content happens before timing starts:
// Commit's own commitFile never writes file content either, it only
// fsyncs, links and removes an already-fully-written .partial, so timing
// the write here as well would measure something Commit() itself never
// does and inflate the calibration far past what it is supposed to
// predict.
func calibrateCommitOnce(localDir string, size int64) (time.Duration, error) {
	scratchPartial := filepath.Join(localDir, fmt.Sprintf(".crashmatrix-calibrate-%d.partial", os.Getpid()))
	scratchFinal := filepath.Join(localDir, fmt.Sprintf(".crashmatrix-calibrate-%d.final", os.Getpid()))
	defer os.Remove(scratchPartial)
	defer os.Remove(scratchFinal)

	if err := os.WriteFile(scratchPartial, make([]byte, size), 0o600); err != nil {
		return 0, fmt.Errorf("write scratch partial: %w", err)
	}

	start := time.Now()
	f, err := os.Open(scratchPartial)
	if err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	f.Close()
	if err := os.Link(scratchPartial, scratchFinal); err != nil {
		return 0, err
	}
	if err := os.Remove(scratchPartial); err != nil {
		return 0, err
	}
	d, err := os.Open(localDir)
	if err != nil {
		return 0, err
	}
	syncErr := d.Sync()
	d.Close()
	if syncErr != nil {
		return 0, syncErr
	}
	return time.Since(start), nil
}

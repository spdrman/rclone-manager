// Package sftpintegration_test is issue #31's SFTP integration suite
// against a disposable server: authentication, host-key verification,
// listing, copy, interruption, cancellation, hashing where supported,
// explicit delete, permission denial, remote object replacement, and
// multiple sources, plus the internal/transport/contract suite run
// against the SFTP backend for the first time (see the PR description's
// "SFTP contract run" heading).
//
// It deliberately does not re-prove what tests/sftpfixture's existing
// callers already cover well: internal/transport/rclone/ssh_test.go's
// TestSFTPHostKeyVerification (an unknown host key and a changed/MITM
// host key, both refused, with a positive control) already IS this
// suite's host-key-verification evidence, and
// internal/transport/rclone/gate_test.go's TestPhase1Gate already covers
// authentication, listing, copy with byte-for-byte verification, transfer
// statistics, explicit delete, and both an already-cancelled context and
// a real mid-transfer cancellation, all against the real adapter. This
// file adds what those two do not: the shared contract suite driven
// against SFTP, permission denial against a real chrooted account,
// multiple isolated sources sharing one journal, cancellation proven at
// the lifecycle.Transfer level (not just the raw adapter), a real remote
// object replacement refused end to end through lifecycle.DeleteRemote,
// and the SFTP contract suite's hash-capability decision, made explicit
// and asserted rather than merely logged.
package sftpintegration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/contract"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/classifytransport"
	"github.com/spdrman/rclone-manager/core/tests/sftpfixture"
)

func openJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func mustSetID(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

// --- the shared transport contract suite, driven against real SFTP -------

// TestSFTPContractSuite is issue #31's explicit ask: "Also run the
// existing internal/transport/contract suite against the SFTP backend,
// which nobody has done yet." It does not call contract.Run (see the
// "hash capability" section of the PR description for why): the suite's
// hash_capability case structurally assumes Fixtures.SupportedHash()
// names an algorithm the backend can actually compute, and a properly
// hardened, shell-less SFTP account (exactly the posture docs/ssh-setup.md
// recommends, and exactly what tests/sftpfixture stands up) can never
// compute one at all; forcing that assumption would mean either always
// failing the suite for the deployment this project actually recommends,
// or weakening the fixture with a shell just to satisfy a test, defeating
// the point of testing the recommended posture. So this file drives the
// contract suite's other eight cases directly against the real adapter and
// this real fixture, using the same assertions contract.go's own
// unexported test functions make, and covers hash capability separately
// and honestly in TestSFTPHashCapability below.
func TestSFTPContractSuite(t *testing.T) {
	f := sftpfixture.Start(t)
	adapter := rclone.New()
	ctx := context.Background()

	newSource := func(t *testing.T, root string) transport.Source {
		t.Helper()
		full := filepath.Join(f.UploadDir, root)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", full, err)
		}
		return f.Source("sftp-contract-"+root, root)
	}
	put := func(t *testing.T, root, remotePath string, content []byte) {
		t.Helper()
		full := filepath.Join(f.UploadDir, root, filepath.FromSlash(remotePath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", full, err)
		}
	}

	t.Run("list", func(t *testing.T) {
		root := "list"
		source := newSource(t, root)
		put(t, root, "one.txt", bytes.Repeat([]byte("x"), 5))
		put(t, root, "two.txt", bytes.Repeat([]byte("x"), 7))

		got, err := adapter.List(ctx, source)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		byPath := map[string]transport.RemoteArtifact{}
		for _, a := range got {
			byPath[a.Path] = a
		}
		if byPath["one.txt"].Size != 5 || byPath["two.txt"].Size != 7 {
			t.Fatalf("List = %+v, want one.txt=5 two.txt=7", got)
		}
	})

	t.Run("stat", func(t *testing.T) {
		root := "stat"
		source := newSource(t, root)
		content := []byte("hello world")
		put(t, root, "file.txt", content)

		a, err := adapter.Stat(ctx, source, "file.txt")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if a.Path != "file.txt" || a.Size != int64(len(content)) {
			t.Fatalf("Stat = %+v", a)
		}
	})

	t.Run("copy_to_local", func(t *testing.T) {
		root := "copy"
		source := newSource(t, root)
		content := bytes.Repeat([]byte("payload-"), 512)
		put(t, root, "artifact.bin", content)

		dest := filepath.Join(t.TempDir(), "artifact.bin.partial")
		result, err := adapter.CopyToLocal(ctx, source, "artifact.bin", dest)
		if err != nil {
			t.Fatalf("CopyToLocal: %v", err)
		}
		if result.BytesTransferred != int64(len(content)) {
			t.Errorf("BytesTransferred = %d, want %d", result.BytesTransferred, len(content))
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("reading copied file: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatal("copied content does not match source")
		}
	})

	t.Run("delete", func(t *testing.T) {
		root := "delete"
		source := newSource(t, root)
		put(t, root, "to-delete.txt", []byte("bye"))

		if err := adapter.DeleteRemote(ctx, source, "to-delete.txt"); err != nil {
			t.Fatalf("DeleteRemote: %v", err)
		}
		if _, err := adapter.Stat(ctx, source, "to-delete.txt"); err == nil {
			t.Fatal("Stat succeeded after DeleteRemote")
		}
	})

	t.Run("cancel", func(t *testing.T) {
		root := "cancel"
		source := newSource(t, root)
		put(t, root, "big.bin", bytes.Repeat([]byte("cancel-me-"), 4096))

		cctx, cancel := context.WithCancel(ctx)
		cancel()
		dest := filepath.Join(t.TempDir(), "big.bin.partial")
		_, err := adapter.CopyToLocal(cctx, source, "big.bin", dest)
		if err == nil {
			t.Fatal("CopyToLocal succeeded against an already-cancelled context")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want errors.Is(_, context.Canceled)", err)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		root := "notfound"
		source := newSource(t, root)
		const missing = "does-not-exist.txt"
		if _, err := adapter.Stat(ctx, source, missing); err == nil {
			t.Error("Stat succeeded on a missing object")
		}
		if _, err := adapter.CopyToLocal(ctx, source, missing, filepath.Join(t.TempDir(), "out")); err == nil {
			t.Error("CopyToLocal succeeded on a missing object")
		}
		if err := adapter.DeleteRemote(ctx, source, missing); err == nil {
			t.Error("DeleteRemote succeeded on a missing object")
		}
	})

	t.Run("permission_denied", func(t *testing.T) {
		root := "permdenied"
		source := newSource(t, root)
		put(t, root, "secret.txt", []byte("shh"))
		cleanup := f.Deny(t, filepath.Join(root, "secret.txt"))
		defer cleanup()

		if _, err := adapter.CopyToLocal(ctx, source, "secret.txt", filepath.Join(t.TempDir(), "out")); err == nil {
			t.Error("CopyToLocal succeeded reading a denied object")
		}
	})

	t.Run("changed_object_detection", func(t *testing.T) {
		root := "changed"
		source := newSource(t, root)
		alg := transport.SHA256

		t.Run("unchanged_object_is_not_flagged", func(t *testing.T) {
			put(t, root, "steady.bin", []byte("same content, never touched"))
			discovered, err := contract.Capture(ctx, adapter, source, "steady.bin", alg)
			if err != nil {
				t.Fatalf("Capture (discovery): %v", err)
			}
			current, err := contract.Capture(ctx, adapter, source, "steady.bin", alg)
			if err != nil {
				t.Fatalf("Capture (recheck): %v", err)
			}
			changed, confident := contract.Changed(discovered, current)
			if changed || !confident {
				// A hash-less SFTP account can still confirm an UNCHANGED
				// object as unconfirmed rather than confidently unchanged;
				// what must never happen is reporting it changed.
				if changed {
					t.Fatalf("Changed() = (changed=%v, confident=%v) for an untouched object, want changed=false", changed, confident)
				}
				t.Logf("confidence could not reach strong for an unchanged object on this hash-less account (changed=%v, confident=%v); "+
					"expected for FR-6's recommended posture, see TestSFTPHashCapability", changed, confident)
			}
		})

		t.Run("different_size_replacement_is_caught", func(t *testing.T) {
			put(t, root, "grows.bin", []byte("short"))
			discovered, err := contract.Capture(ctx, adapter, source, "grows.bin", alg)
			if err != nil {
				t.Fatalf("Capture (discovery): %v", err)
			}
			put(t, root, "grows.bin", []byte("this replacement is much longer than the original"))
			current, err := contract.Capture(ctx, adapter, source, "grows.bin", alg)
			if err != nil {
				t.Fatalf("Capture (recheck): %v", err)
			}
			changed, confident := contract.Changed(discovered, current)
			if !changed || !confident {
				t.Fatalf("Changed() = (changed=%v, confident=%v) for a differently-sized replacement, want (true, true)", changed, confident)
			}
		})
	})
}

// --- hash capability: the deliberate decision -----------------------------

// TestSFTPHashCapability is the decision issue #31 asks this suite to make
// deliberately: a hardened, shell-less SFTP account (docs/ssh-setup.md,
// and exactly what tests/sftpfixture stands up) cannot compute ANY remote
// hash at all, for any algorithm, because rclone's sftp backend computes
// hashes by running a shell command over the SSH session
// (backend/sftp/sftp.go's shellType detection), and internal-sftp (the
// forced subsystem this fixture, like the recommended deployment, uses)
// never grants a shell in the first place. This is asserted here as fact,
// not merely logged as a maybe: it is deterministic given how the backend
// and this fixture are built, not a coin flip this environment happened to
// land on.
//
// The decision this makes concrete: RemoteHash must fail with an explicit,
// typed capability error for this account shape, never a silent empty
// success (a caller could mistake that for "verified"); and
// internal/lifecycle.Verify's own documented capability-absence policy
// (see verify.go's package doc) must actually hold end to end against this
// real backend: config.Validation.Hash == "" trusts transfer verification
// alone and reaches VERIFIED, while Hash == "sha256" fails the artifact
// explicitly rather than silently downgrading, exactly the two honest
// postures verify.go documents.
func TestSFTPHashCapability(t *testing.T) {
	f := sftpfixture.Start(t)
	adapter := rclone.New()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(f.UploadDir, "hash-capability-probe"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	source := f.Source("hash-capability", "hash-capability-probe")

	content := []byte("hash target content")
	if err := os.WriteFile(filepath.Join(f.UploadDir, "hash-capability-probe", "hash-me.bin"), content, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := adapter.RemoteHash(ctx, source, "hash-me.bin", transport.SHA256)
	if err == nil {
		t.Fatal("RemoteHash succeeded against a hardened, shell-less SFTP account; " +
			"this fixture (and the recommended deployment it mirrors) should never be able to compute a hash at all")
	}
	t.Logf("RemoteHash correctly failed with an explicit capability error, never a silent success: %v", err)

	// Now prove the consequence at the lifecycle level, both ways.
	runPipeline := func(t *testing.T, name string, hashPolicy string) (state.Record, error) {
		t.Helper()
		journal := openJournal(t)
		localDir := t.TempDir()
		set := mustSetID(t, "hash-capability-source", name)
		bs := config.BackupSet{Name: name, ID: set, Completion: config.Completion{Strategy: "rename"}}

		// Each pipeline run gets its own isolated remote subdirectory:
		// otherwise every earlier run's artifact (still sitting in the
		// shared upload dir) would show up as newly Discovered too, since
		// each call opens a fresh journal that has never seen any of them.
		remoteDir := name
		if err := os.MkdirAll(filepath.Join(f.UploadDir, remoteDir), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		source := f.Source("hash-capability-"+name, remoteDir)
		if err := os.WriteFile(filepath.Join(f.UploadDir, remoteDir, name+".bin"), content, 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}

		res, err := discovery.Discover(ctx, discovery.Deps{Transport: adapter, Journal: journal}, source, bs)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(res.Discovered) != 1 {
			t.Fatalf("Discovered = %+v, want exactly one", res.Discovered)
		}
		artifact := res.Discovered[0].Artifact

		deps := lifecycle.Deps{Journal: journal, Transport: adapter}
		if _, err := lifecycle.Transfer(ctx, deps, lifecycle.TransferParams{
			Artifact: artifact, Source: source, LocalDir: localDir, AttemptKey: "attempt-1",
		}); err != nil {
			t.Fatalf("Transfer: %v", err)
		}
		rec, err := journal.Get(ctx, artifact)
		if err != nil {
			t.Fatalf("journal.Get: %v", err)
		}
		if _, err := lifecycle.Advance(ctx, deps, state.Transition{
			Artifact: artifact, Key: "attempt-1:begin-verifying", From: rec.State, To: string(lifecycle.Verifying),
		}); err != nil {
			t.Fatalf("begin VERIFYING: %v", err)
		}
		if _, err := lifecycle.Verify(ctx, deps, lifecycle.VerifyParams{
			Artifact: artifact, Source: source, Validation: config.Validation{Hash: hashPolicy}, AttemptKey: "attempt-1",
		}); err != nil {
			return state.Record{}, err
		}
		final, err := journal.Get(ctx, artifact)
		if err != nil {
			t.Fatalf("journal.Get: %v", err)
		}
		return final, nil
	}

	t.Run("hash_not_required_verifies_on_transfer_verification_alone", func(t *testing.T) {
		rec, err := runPipeline(t, "no-hash", "")
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if lifecycle.State(rec.State) != lifecycle.Verified {
			t.Fatalf("state = %s, want VERIFIED (detail: %s)", rec.State, "")
		}
	})

	t.Run("hash_required_fails_explicitly_never_silently_verifies", func(t *testing.T) {
		rec, err := runPipeline(t, "hash-required", string(transport.SHA256))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if lifecycle.State(rec.State) != lifecycle.Failed {
			t.Fatalf("state = %s, want FAILED (a hardened SFTP account cannot supply the required hash); "+
				"reaching VERIFIED here would mean verification was silently downgraded", rec.State)
		}
		t.Logf("correctly FAILED rather than silently verifying: %s", rec.LastError)
	})
}

// --- multiple sources ------------------------------------------------------

// TestSFTPMultipleSources proves two backup sets, on two isolated
// subdirectories of the same real SFTP server, sharing one journal, never
// cross-contaminate: discovering, transferring and deleting one never
// touches the other's remote object or journal row.
func TestSFTPMultipleSources(t *testing.T) {
	f := sftpfixture.Start(t)
	adapter := rclone.New()
	ctx := context.Background()
	journal := openJournal(t)

	type site struct {
		name      string
		source    transport.Source
		localDir  string
		remoteDir string
	}
	sites := []site{
		{name: "site-a", localDir: t.TempDir()},
		{name: "site-b", localDir: t.TempDir()},
	}
	for i := range sites {
		s := &sites[i]
		s.remoteDir = filepath.Join(f.UploadDir, s.name)
		if err := os.MkdirAll(s.remoteDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.remoteDir, "backup.dump"), []byte("content for "+s.name), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s.source = f.Source("multi-"+s.name, s.name)
	}

	var artifacts []model.ArtifactID
	for _, s := range sites {
		set := mustSetID(t, s.name, "backups")
		bs := config.BackupSet{Name: "backups", ID: set, Completion: config.Completion{Strategy: "rename"}}
		res, err := discovery.Discover(ctx, discovery.Deps{Transport: adapter, Journal: journal}, s.source, bs)
		if err != nil {
			t.Fatalf("Discover(%s): %v", s.name, err)
		}
		if len(res.Discovered) != 1 {
			t.Fatalf("Discover(%s): Discovered = %+v, want exactly one", s.name, res.Discovered)
		}
		artifacts = append(artifacts, res.Discovered[0].Artifact)

		deps := lifecycle.Deps{Journal: journal, Transport: adapter}
		// Idempotency keys are unique across the whole journal, not
		// scoped per artifact (state.Transition's own Key doc), so two
		// sites sharing one journal must not reuse the same literal key.
		if _, err := lifecycle.Transfer(ctx, deps, lifecycle.TransferParams{
			Artifact: res.Discovered[0].Artifact, Source: s.source, LocalDir: s.localDir, AttemptKey: s.name + ":attempt-1",
		}); err != nil {
			t.Fatalf("Transfer(%s): %v", s.name, err)
		}
	}

	// Each set's journal row is isolated by BackupSetID (source component),
	// and each site's local file only ever landed in its own local dir.
	for i, s := range sites {
		rec, err := journal.Get(ctx, artifacts[i])
		if err != nil {
			t.Fatalf("journal.Get(%s): %v", s.name, err)
		}
		if rec.Artifact.Set.Source != s.name {
			t.Fatalf("artifact %s recorded under source %q, want %q", rec.Artifact, rec.Artifact.Set.Source, s.name)
		}
		got, err := os.ReadFile(filepath.Join(s.localDir, "backup.dump.partial"))
		if err != nil {
			t.Fatalf("reading %s's local partial: %v", s.name, err)
		}
		if string(got) != "content for "+s.name {
			t.Fatalf("%s's local content = %q, want its own content, not cross-contaminated from another site", s.name, got)
		}
	}

	// site-b's remote object is untouched by anything done to site-a's.
	if _, err := os.Stat(filepath.Join(sites[1].remoteDir, "backup.dump")); err != nil {
		t.Fatalf("site-b's remote object should still be present: %v", err)
	}
}

// --- interruption / cancellation at the lifecycle level -------------------

// TestSFTPTransferCancellation_ThroughLifecycle proves FR-22's
// cancellation propagates correctly through lifecycle.Transfer itself,
// against a real SFTP transfer, not just the raw adapter call
// gate_test.go's MidTransferCancellation already covers: a context
// cancelled mid-copy must leave the journal at TRANSFERRING, never
// TRANSFERRED and never FAILED, and a resumed call with the same
// AttemptKey must converge to a correct, complete TRANSFERRED afterward.
//
// This uses tests/classifytransport.Wrap around the real adapter, not the
// bare adapter: transfer.go's own cancellation handling
// ("Cancellation must not claim TRANSFERRED... leave the journal exactly
// where it honestly is") depends on transport.CategoryOf recognising the
// copy error as transport.Cancelled, which the real, unwrapped adapter
// never produces (see the PR description's classification-gap defect).
// Run against the bare adapter, this same scenario still happens to leave
// the journal at TRANSFERRING, but only because failCopy's own attempt to
// record FAILED also fails, since it reuses the same already-cancelled
// context for that journal write; that is an accident of two unrelated
// failures cancelling out, not transfer.go's cancellation branch actually
// engaging, and it would stop being true the moment a journal
// implementation tolerated BeginTx on an already-done context. Wrapping
// with Wrap here tests the real, intended code path instead.
func TestSFTPTransferCancellation_ThroughLifecycle(t *testing.T) {
	f := sftpfixture.Start(t)
	adapter := classifytransport.Wrap(rclone.New())
	source := f.Source("cancel-lifecycle", "")
	journal := openJournal(t)
	localDir := t.TempDir()

	const size = 2 * 1024 * 1024
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.UploadDir, "backup.dump"), content, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	set := mustSetID(t, "cancel-lifecycle-source", "set")
	bs := config.BackupSet{Name: "set", ID: set, Completion: config.Completion{Strategy: "rename"}}
	res, err := discovery.Discover(context.Background(), discovery.Deps{Transport: adapter, Journal: journal}, source, bs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Discovered) != 1 {
		t.Fatalf("Discovered = %+v, want exactly one", res.Discovered)
	}
	artifact := res.Discovered[0].Artifact

	// Throttle bandwidth so a real, mid-copy cancellation has a wide,
	// reliable window to land in, the same technique
	// gate_test.go's MidTransferCancellation already established for this
	// exact fixture.
	const bwLimit = 128 * 1024
	bwCtx, ci := fs.AddConfig(context.Background())
	if err := (&ci.BwLimit).Set(fmt.Sprintf("%d", bwLimit)); err != nil {
		t.Fatalf("set bwlimit: %v", err)
	}
	accounting.TokenBucket.StartTokenBucket(bwCtx)
	t.Cleanup(func() {
		unthrottled, _ := fs.AddConfig(context.Background())
		accounting.TokenBucket.StartTokenBucket(unthrottled)
	})

	cancelCtx, cancel := context.WithCancel(bwCtx)
	time.AfterFunc(150*time.Millisecond, cancel)

	deps := lifecycle.Deps{Journal: journal, Transport: adapter}
	_, transferErr := lifecycle.Transfer(cancelCtx, deps, lifecycle.TransferParams{
		Artifact: artifact, Source: source, LocalDir: localDir, AttemptKey: "attempt-1",
	})
	if transferErr == nil {
		t.Fatal("Transfer succeeded despite being cancelled mid-copy; the throttle should have made this reliably interruptible")
	}
	t.Logf("Transfer correctly failed on cancellation: %v", transferErr)

	rec, err := journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("journal.Get: %v", err)
	}
	if lifecycle.State(rec.State) != lifecycle.Transferring {
		t.Fatalf("journal state after a cancelled Transfer = %s, want TRANSFERRING (never TRANSFERRED, never FAILED)", rec.State)
	}

	// Resume with the same AttemptKey, unthrottled: must converge to a
	// correct, complete TRANSFERRED.
	if _, err := lifecycle.Transfer(context.Background(), deps, lifecycle.TransferParams{
		Artifact: artifact, Source: source, LocalDir: localDir, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("resumed Transfer: %v", err)
	}
	rec, err = journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("journal.Get: %v", err)
	}
	if lifecycle.State(rec.State) != lifecycle.Transferred {
		t.Fatalf("state after resume = %s, want TRANSFERRED", rec.State)
	}
	got, err := os.ReadFile(filepath.Join(localDir, "backup.dump.partial"))
	if err != nil {
		t.Fatalf("reading resumed .partial: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("resumed transfer's content does not match the original")
	}
}

// --- remote object replacement, end to end --------------------------------

// TestSFTPRemoteObjectReplacement_RefusesDelete is FR-16's TOCTOU proof
// against a real server: an object replaced under its discovered path,
// with a different size (the case a hash-less account can still catch
// with ConfidenceStrong; see TestSFTPHashCapability for the harder,
// same-size case this account shape cannot distinguish from an untouched
// file), must refuse the pending delete and leave the replacement intact.
func TestSFTPRemoteObjectReplacement_RefusesDelete(t *testing.T) {
	f := sftpfixture.Start(t)
	adapter := rclone.New()
	ctx := context.Background()
	source := f.Source("replacement", "")
	journal := openJournal(t)
	localDir := t.TempDir()

	original := []byte("original content, this is the one that gets backed up")
	if err := os.WriteFile(filepath.Join(f.UploadDir, "backup.dump"), original, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	set := mustSetID(t, "replacement-source", "set")
	bs := config.BackupSet{Name: "set", ID: set, Completion: config.Completion{Strategy: "rename"}}
	res, err := discovery.Discover(ctx, discovery.Deps{Transport: adapter, Journal: journal}, source, bs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	artifact := res.Discovered[0].Artifact

	deps := lifecycle.Deps{Journal: journal, Transport: adapter}
	if _, err := lifecycle.Transfer(ctx, deps, lifecycle.TransferParams{
		Artifact: artifact, Source: source, LocalDir: localDir, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("journal.Get: %v", err)
	}
	if _, err := lifecycle.Advance(ctx, deps, state.Transition{
		Artifact: artifact, Key: "attempt-1:begin-verifying", From: rec.State, To: string(lifecycle.Verifying),
	}); err != nil {
		t.Fatalf("begin VERIFYING: %v", err)
	}
	if _, err := lifecycle.Verify(ctx, deps, lifecycle.VerifyParams{
		Artifact: artifact, Source: source, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := lifecycle.Commit(ctx, deps, lifecycle.CommitInput{
		Artifact: artifact, LocalDir: localDir, CommittingKey: "commit:committing", CommittedKey: "commit:committed",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Someone (a producer overwriting the same filename, an operator, an
	// attacker) replaces the remote object with different, differently
	// sized content, after discovery and after commit, before delete.
	replacement := []byte("a completely different, longer replacement payload placed under the same remote name")
	if err := os.WriteFile(filepath.Join(f.UploadDir, "backup.dump"), replacement, 0o644); err != nil {
		t.Fatalf("replace: %v", err)
	}

	_, deleteErr := lifecycle.DeleteRemote(ctx, deps, lifecycle.DeleteRemoteRequest{
		CompletionStrategy: bs.Completion.Strategy,
		Source:             source, Artifact: artifact, AttemptKey: "delete:attempt-1",
	})
	refusal, ok := lifecycle.AsRemoteDeleteRefusal(deleteErr)
	if !ok {
		t.Fatalf("DeleteRemote against a replaced remote object = %v (%T), want a *RemoteDeleteRefusalError", deleteErr, deleteErr)
	}
	if refusal.Check != "remote identity" {
		t.Fatalf("refusal.Check = %q, want %q", refusal.Check, "remote identity")
	}
	if refusal.Verdict != model.VerdictChanged || refusal.Confidence != model.ConfidenceStrong {
		t.Fatalf("refusal verdict/confidence = %s/%s, want Changed/Strong (a size mismatch is always decisive, even without a hash)",
			refusal.Verdict, refusal.Confidence)
	}

	got, err := os.ReadFile(filepath.Join(f.UploadDir, "backup.dump"))
	if err != nil {
		t.Fatalf("reading remote object after the refused delete: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("the replacement content itself was altered by the refused delete attempt")
	}

	rec, err = journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("journal.Get: %v", err)
	}
	if lifecycle.State(rec.State) != lifecycle.RemoteDeletePending {
		t.Fatalf("journal state = %s, want REMOTE_DELETE_PENDING (intent recorded, delete refused, nothing lost)", rec.State)
	}
	if rec.RemoteDeleteError == "" {
		t.Fatal("the refusal was not persisted into remote_delete_error, so an operator inspecting the journal directly would not see it")
	}
}

// --- a fixture container that dies mid-test -------------------------------

// TestSFTPOperationFailsFastWhenTheFixtureContainerDies is issue #161's
// contract at the level that actually costs time: an operation running
// against a fixture whose container has just gone must come back in
// seconds, with the container's death named as the cause, instead of
// retrying against a corpse.
//
// The bar is deliberately set below what the unaided client does on its
// own. Measured on this machine while writing this test, an adapter.List
// against a container that had just been removed took 11.1s to give up
// with a bare "connection refused", which says nothing about why and is
// only the cheapest of the operations this suite runs. The context the
// fixture hands out is what turns that into a prompt, self-describing
// failure.
//
// It kills the container by the exact id this fixture created. Several
// worktrees on this machine share one docker daemon, so anything matched
// by name pattern or counted from `docker ps` could be another agent's
// container.
func TestSFTPOperationFailsFastWhenTheFixtureContainerDies(t *testing.T) {
	f := sftpfixture.Start(t)
	adapter := rclone.New()
	source := f.Source("container-death", "")
	if err := os.WriteFile(filepath.Join(f.UploadDir, "present.txt"), []byte("present"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Positive control: the very same call, through the very same context,
	// succeeds while the container is healthy. Without it, "List failed"
	// below would also be satisfied by a fixture that never worked.
	if _, err := adapter.List(f.Context(), source); err != nil {
		t.Fatalf("List against a healthy fixture failed (%v), so the fail-fast assertion below would pass for the wrong reason", err)
	}

	f.ExpectContainerDeath()
	if out, err := exec.Command("docker", "rm", "-f", f.ContainerID()).CombinedOutput(); err != nil {
		t.Fatalf("docker rm -f %s: %v\n%s", f.ContainerID(), err, out)
	}

	start := time.Now()
	_, err := adapter.List(f.Context(), source)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("List succeeded against a container that no longer exists")
	}
	if elapsed > 8*time.Second {
		t.Fatalf("List took %s to give up after its container died; the unaided client already manages 11s, so this is not failing fast, it is just failing", elapsed)
	}

	var died *sftpfixture.ContainerDiedError
	if !errors.As(context.Cause(f.Context()), &died) {
		t.Fatalf("the fixture context's cause after the container died is %v, not a *ContainerDiedError; without that a reader cannot tell a dead fixture container from a genuine deadlock in the transport, which is the diagnostic gap #161 is about", context.Cause(f.Context()))
	}
	if !strings.Contains(died.Error(), "died") {
		t.Fatalf("the cause does not say the container died: %q", died.Error())
	}
	t.Logf("List gave up %s after the container died, naming it: %v", elapsed, died)
}

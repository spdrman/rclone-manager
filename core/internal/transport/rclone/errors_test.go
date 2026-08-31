package rclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	rclonefs "github.com/rclone/rclone/fs"

	"github.com/spdrman/rclone-manager/core/internal/transport"

	"github.com/spdrman/rclone-manager/core/internal/testenv"
)

// ---------------------------------------------------------------------------
// Unit-level classification: no Docker, exercised against errors this
// adapter's local backend and the standard library actually return. These
// exist mainly so the fast, always-available case for each category has a
// test that never depends on Docker being present, and they double-check the
// os.ErrNotExist/os.ErrPermission fallback paths transport_test.go's
// "translation shape" tests already found survive unwrapped alongside
// rclone's own sentinels (see the comment in errors.go's PermissionDenied
// case).
// ---------------------------------------------------------------------------

func TestClassify_Nil(t *testing.T) {
	if got := Classify(nil); got != transport.Unclassified {
		t.Fatalf("Classify(nil) = %v, want Unclassified", got)
	}
}

func TestClassify_IsIdempotentOnAnAlreadyWrappedError(t *testing.T) {
	wrapped := transport.NewError(transport.PermissionDenied, "stat", errors.New("boom"))
	if got := Classify(wrapped); got != transport.PermissionDenied {
		t.Fatalf("Classify(already-wrapped) = %v, want the category it already carried (PermissionDenied)", got)
	}
}

func TestClassify_NotFound_RealLocalError(t *testing.T) {
	ctx := context.Background()
	adapter := New()
	source := transport.Source{ID: "cls-notfound", Type: "local", Root: t.TempDir()}

	cases := map[string]func() error{
		"Stat": func() error {
			_, err := adapter.Stat(ctx, source, "missing.txt")
			return err
		},
		"CopyToLocal": func() error {
			_, err := adapter.CopyToLocal(ctx, source, "missing.txt", filepath.Join(t.TempDir(), "out"))
			return err
		},
		"DeleteRemote": func() error {
			return adapter.DeleteRemote(ctx, source, "missing.txt")
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatalf("%s succeeded against a missing object", name)
			}
			if got := Classify(err); got != transport.NotFound {
				t.Fatalf("Classify(%v) = %v, want NotFound", err, got)
			}
		})
	}
}

// TestClassify_PermissionDenied_RealLocalError is the case
// errors.go's PermissionDenied comment is about: Stat cannot observe a
// chmod-000 file on a POSIX filesystem (no read permission on the file
// itself is needed to stat it, only traversal permission on its containing
// directories), so this has to actually open and read the file, which is
// what RemoteHash does for the local backend.
func TestClassify_PermissionDenied_RealLocalError(t *testing.T) {
	testenv.RequirePermissionBitsApply(t)

	ctx := context.Background()
	root := t.TempDir()
	full := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(full, []byte("shh"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(full, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer func() { _ = os.Chmod(full, 0o644) }()

	adapter := New()
	source := transport.Source{ID: "cls-permdenied", Type: "local", Root: root}
	_, err := adapter.RemoteHash(ctx, source, "secret.txt", transport.SHA256)
	if err == nil {
		t.Fatalf("RemoteHash succeeded reading a chmod 000 file")
	}
	if got := Classify(err); got != transport.PermissionDenied {
		t.Fatalf("Classify(%v) = %v, want PermissionDenied", err, got)
	}
}

// TestClassify_Cancelled_RealContextError reuses contract.go's testCancel
// technique directly against the adapter: a context cancelled before the
// call starts must classify as Cancelled, not as some flavour of failure the
// adapter itself caused.
func TestClassify_Cancelled_RealContextError(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("cancel-me-"), 4096)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	adapter := New()
	source := transport.Source{ID: "cls-cancelled", Type: "local", Root: root}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := adapter.CopyToLocal(ctx, source, "big.bin", filepath.Join(t.TempDir(), "big.bin.partial"))
	if err == nil {
		t.Fatalf("CopyToLocal succeeded against an already-cancelled context")
	}
	if got := Classify(err); got != transport.Cancelled {
		t.Fatalf("Classify(%v) = %v, want Cancelled", err, got)
	}
}

// TestClassify_Transient_RealConnectionRefused drives the real sftp backend
// (through this adapter, exactly as production code would) at a TCP port
// nothing is listening on. The dial failure this produces is genuine
// OS/network behaviour, not anything rclone or this adapter manufactures,
// and it is exactly the shape of error rclone's own fs/fserrors.ShouldRetry
// exists to recognize, which is what Classify defers to for Transient.
func TestClassify_Transient_RealConnectionRefused(t *testing.T) {
	dir := t.TempDir()
	keyPath, _ := generateClientSSHKeyPair(t) // needs to parse, never needs to authenticate
	knownHostsPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
		t.Fatalf("writing empty known_hosts: %v", err)
	}

	port := freeTCPPort(t) // reserved and released immediately: nothing listens here
	source := transport.Source{
		ID:         "cls-transient",
		Type:       "sftp",
		Host:       "127.0.0.1",
		Port:       port,
		User:       "backup",
		KeyFile:    keyPath,
		KnownHosts: knownHostsPath,
	}

	adapter := New()
	_, err := adapter.List(context.Background(), source)
	if err == nil {
		t.Fatalf("List succeeded against a port nothing is listening on")
	}
	if got := Classify(err); got != transport.Transient {
		t.Fatalf("Classify(%v) = %v, want Transient", err, got)
	}
}

// ---------------------------------------------------------------------------
// Unit-level classification against real rclone sentinel values, for the two
// categories (Conflict, IntegrityFailure) this adapter's current surface has
// no reachable path to provoke live. These are real values exported by
// rclone (fs.ErrorDirExists) or real, cited wording from rclone's own source
// (see integrityFailurePrefix's doc comment in errors.go), not something
// invented for the test. See the PR description for why a live scenario
// wasn't achievable within this issue's file scope.
// ---------------------------------------------------------------------------

func TestClassify_Conflict_RealRcloneSentinel(t *testing.T) {
	if got := Classify(rclonefs.ErrorDirExists); got != transport.Conflict {
		t.Fatalf("Classify(fs.ErrorDirExists) = %v, want Conflict", got)
	}
	// Also through a wrapping layer, the way it would actually arrive
	// through this adapter's own fmt.Errorf("...: %w", err) wrapping.
	wrapped := fmt.Errorf("copy %q: %w", "some/path", rclonefs.ErrorDirExists)
	if got := Classify(wrapped); got != transport.Conflict {
		t.Fatalf("Classify(wrapped fs.ErrorDirExists) = %v, want Conflict", got)
	}
}

func TestClassify_IntegrityFailure_RealRcloneWording(t *testing.T) {
	// These are the literal fmt.Errorf format strings from rclone v1.75.0's
	// fs/operations/copy.go (lines 289 and 296), reproduced with sample
	// values rather than invented text, and rclone's own manual documents
	// "corrupted on transfer" as the exact phrase this signals.
	sizeDiffer := fmt.Errorf("corrupted on transfer: sizes differ src(%s) %d vs dst(%s) %d", "srcFs", 10, "dstFs", 5)
	hashesDiffer := fmt.Errorf("corrupted on transfer: %v hashes differ src(%s) %q vs dst(%s) %q", "sha256", "srcFs", "aaa", "dstFs", "bbb")

	for _, err := range []error{sizeDiffer, hashesDiffer} {
		if got := Classify(err); got != transport.IntegrityFailure {
			t.Fatalf("Classify(%v) = %v, want IntegrityFailure", err, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Docker-backed classification: real authentication failure, real host-key
// failures (both shapes) and real permission/capability failures against a
// disposable SFTP server. This reuses the exact fixture image, sshd_config
// and helper functions ssh_test.go already built and proved for FR-6
// (buildSFTPFixtureImage, startFixtureContainer, generateClientSSHKeyPair,
// writeKnownHosts, freeTCPPort, requireDocker), rather than a second copy of
// them: same package, same fixture, a different set of assertions layered on
// top (Classify's category, not just the raw rclone error text).
// ---------------------------------------------------------------------------

// dockerExecMust runs `docker exec containerID args...` and fails the test if
// it does not succeed. It exists to set up file content/ownership/mode
// directly on the fixture's disk, which is outside what the sftp protocol
// (as this adapter's Transport interface exposes it: List/Stat/CopyToLocal/
// RemoteHash/DeleteRemote, no write) can do on its own.
func dockerExecMust(t *testing.T, containerID string, args ...string) {
	t.Helper()
	full := append([]string{"exec", containerID}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker exec %v: %v\n%s", args, err, out)
	}
}

// writeRemoteFile creates path inside the fixture container (a path under
// /home/backup, i.e. inside the chrooted sftp root), owned by the backup
// user, with the given permission mode.
func writeRemoteFile(t *testing.T, containerID, path, content string, mode string) {
	t.Helper()
	script := fmt.Sprintf("printf %q > %s && chown backup:backup %s && chmod %s %s",
		content, path, path, mode, path)
	dockerExecMust(t, containerID, "sh", "-c", script)
}

// TestClassify_Docker is one Docker test function, not several, because
// every subtest below shares the one fixture image and (for all but the
// host-verification subtests, which need their own containers by
// construction) the one running container: rebuilding the image or
// restarting sshd per category would multiply this file's Docker cost for no
// added coverage.
func TestClassify_Docker(t *testing.T) {
	requireDocker(t)

	clientKeyPath, authorizedKeyLine := generateClientSSHKeyPair(t)
	image := buildSFTPFixtureImage(t, authorizedKeyLine)

	host := "127.0.0.1"
	port := freeTCPPort(t)
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")

	cont, hostKeyA := startFixtureContainer(t, image, port, "errors-main")
	writeKnownHosts(t, knownHostsPath, host, port, hostKeyA)

	baseSource := transport.Source{
		ID:         "errors-fixture",
		Type:       "sftp",
		Host:       host,
		Port:       port,
		User:       "backup",
		KeyFile:    clientKeyPath,
		KnownHosts: knownHostsPath,
		Root:       "upload",
	}
	adapter := New()
	ctx := context.Background()

	// Positive control: the correctly-authorized key against the recorded
	// host key must work, otherwise every "this attack is refused" result
	// below would prove nothing.
	t.Run("positive_control_recorded_key_and_authorized_client_succeed", func(t *testing.T) {
		if _, err := adapter.List(ctx, baseSource); err != nil {
			logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
			t.Fatalf("List with the recorded host key and authorized client should have succeeded, got: %v\nserver logs:\n%s", err, logs)
		}
	})

	t.Run("authentication_wrong_client_key_is_refused", func(t *testing.T) {
		wrongKeyPath, _ := generateClientSSHKeyPair(t) // freshly generated, never added to authorized_keys
		src := baseSource
		src.KeyFile = wrongKeyPath

		_, err := adapter.List(ctx, src)
		if err == nil {
			t.Fatal("List with an unauthorized client key should have been refused, it succeeded")
		}
		if got := Classify(err); got != transport.Authentication {
			t.Fatalf("Classify(%v) = %v, want Authentication", err, got)
		}
	})

	t.Run("permission_denied_unreadable_remote_file_is_refused", func(t *testing.T) {
		writeRemoteFile(t, cont, "/home/backup/upload/secret.txt", "shh", "000")

		_, err := adapter.CopyToLocal(ctx, baseSource, "secret.txt", filepath.Join(t.TempDir(), "secret.txt.partial"))
		if err == nil {
			t.Fatal("CopyToLocal against a chmod 000 remote file should have been refused, it succeeded")
		}
		if got := Classify(err); got != transport.PermissionDenied {
			t.Fatalf("Classify(%v) = %v, want PermissionDenied", err, got)
		}
	})

	// UnsupportedCapability is the "hardened shell-less SFTP account" fact:
	// the fixture's sshd forces every session to internal-sftp
	// (ForceCommand), and the backup account has no shell (/sbin/nologin),
	// so rclone's shell-type detection finds no shell to run md5sum/sha1sum
	// through and Hashes() reports an empty set, for every object, readable
	// or not. This must surface as an explicit capability result, never as
	// a Permanent failure and never as a silent success.
	t.Run("unsupported_capability_remote_hash_on_a_shell_less_account", func(t *testing.T) {
		writeRemoteFile(t, cont, "/home/backup/upload/hashable.txt", "hash me please", "644")

		got, err := adapter.RemoteHash(ctx, baseSource, "hashable.txt", transport.SHA256)
		if err == nil {
			t.Fatalf("RemoteHash succeeded against a shell-less account (got %q); capability was supposed to be absent, not silently downgraded", got)
		}
		if cat := Classify(err); cat != transport.UnsupportedCapability {
			t.Fatalf("Classify(%v) = %v, want UnsupportedCapability", err, cat)
		}
	})

	t.Run("not_found_missing_remote_object", func(t *testing.T) {
		_, err := adapter.Stat(ctx, baseSource, "does-not-exist.txt")
		if err == nil {
			t.Fatal("Stat succeeded against a missing remote object")
		}
		if got := Classify(err); got != transport.NotFound {
			t.Fatalf("Classify(%v) = %v, want NotFound", err, got)
		}
	})

	// Host verification needs its own containers: "unknown" needs a live
	// server at an address with no known_hosts entry at all, and "mismatch"
	// needs the same host:port to answer with a different host key than the
	// one recorded, which needs the original container gone first.
	stopFixtureContainer(cont)

	t.Run("host_verification_unknown_host_is_refused", func(t *testing.T) {
		unknownPort := freeTCPPort(t)
		contU, _ := startFixtureContainer(t, image, unknownPort, "errors-unknown")
		defer stopFixtureContainer(contU)

		src := baseSource
		src.Port = unknownPort // knownHostsPath has no entry for this host:port

		_, err := adapter.List(ctx, src)
		if err == nil {
			t.Fatal("List against a host with no known_hosts entry should have been refused, it succeeded")
		}
		if got := Classify(err); got != transport.HostVerification {
			t.Fatalf("Classify(%v) = %v, want HostVerification", err, got)
		}
	})

	t.Run("host_verification_changed_host_key_is_refused", func(t *testing.T) {
		contB, hostKeyB := startFixtureContainer(t, image, port, "errors-changed")
		defer stopFixtureContainer(contB)

		if hostKeyB == hostKeyA {
			t.Fatal("test setup bug: the replacement container generated the same host key, so this proves nothing")
		}

		_, err := adapter.List(ctx, baseSource) // same host:port, known_hosts still pinned to the original key
		if err == nil {
			t.Fatal("List against a changed host key should have been refused, it succeeded")
		}
		if got := Classify(err); got != transport.HostVerification {
			t.Fatalf("Classify(%v) = %v, want HostVerification", err, got)
		}
	})
}

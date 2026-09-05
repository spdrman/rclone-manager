// The error classifier held to errors a real server produced, rather than
// to errors this repository wrote for it.
//
// That distinction is the whole reason the file is in the machine tier. A
// classifier tested against hand-built errors is tested against its author's
// memory of what a backend says, and this adapter has already shipped
// wrong on exactly that: what comes back from a failed handshake, a refused
// key or a missing path is not the sentinel a reasonable person would
// predict, and several of the cases below exist because the predicted answer
// was wrong.
//
// Every refusal here is read next to a positive control on the same machine,
// because "refused" and "this fixture is broken" produce the same verdict
// from the outside.
package machinegate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// TestClassify_Docker is the classifier held to real errors from a real
// server, rather than to errors this repository wrote itself.
//
// It is one test function, not several, because every case below shares one
// source machine. Standing up a second sshd per category would multiply
// this package's cost for no added coverage, and the two cases that DO need
// a second machine (an address known_hosts has never seen, and the same
// address answering with a different key) say so where they use it.
//
// It reads its verdict through rclone.ClassifyCtx with a live context
// rather than through the package's own unexported classify. That is not a
// workaround for the move in #448: ClassifyCtx with a live context IS
// classify, plus the one tiebreak (#388) that only fires on a done context,
// and it is the function the adapter itself goes through. The one case that
// wants a done context asks for it explicitly.
func TestClassify_Docker(t *testing.T) {
	m := machines.Start(t)
	src := m.Source(t)
	adapter := rclone.New()
	ctx := context.Background()

	baseSource := src.TransportSource("errors-fixture", "")

	// Positive control: the correctly-authorized key against the recorded
	// host key must work, otherwise every "this attack is refused" result
	// below would prove nothing.
	//
	// One attempt, no retry, on purpose. This assertion's whole value is
	// that it is believed when it fails, and a retry wide enough to absorb
	// a startup flake would have had to absorb "unable to authenticate",
	// which is the one answer a positive control must never shrug off. The
	// race that used to produce that flake (#250) is gone at the source
	// instead: the harness does not return a machine until this exact key
	// has authenticated against it, which is strictly more than the List
	// below needs.
	t.Run("positive_control_recorded_key_and_authorized_client_succeed", func(t *testing.T) {
		if _, err := adapter.List(ctx, baseSource); err != nil {
			t.Fatalf("List with the recorded host key and authorized client should have succeeded, got: %v\nserver logs:\n%s", err, src.ConnectionTable(t))
		}
	})

	t.Run("authentication_wrong_client_key_is_refused", func(t *testing.T) {
		// A key this server has never been told about. It is the machine's
		// own second identity rather than a synthetic file, so it is a real
		// ed25519 key in the real format, refused for the real reason.
		other := m.AnotherSource(t)
		wrong := baseSource
		wrong.KeyFile = other.KeyFile

		_, err := adapter.List(ctx, wrong)
		if err == nil {
			t.Fatal("List with an unauthorized client key should have been refused, it succeeded")
		}
		if got := rclone.ClassifyCtx(ctx, err); got != transport.Authentication {
			t.Fatalf("ClassifyCtx(%v) = %v, want Authentication", err, got)
		}
	})

	t.Run("permission_denied_unreadable_remote_file_is_refused", func(t *testing.T) {
		// Written through the bind mount and then chmod 000, which is
		// unreadable to every uid including the one sshd serves this
		// account as. Writing it from the host rather than through a
		// `docker exec` is what keeps this test out of the harness's job.
		secret := filepath.Join(src.UploadDir, "secret.txt")
		if err := os.WriteFile(secret, []byte("shh"), 0o600); err != nil {
			t.Fatalf("seeding the unreadable file: %v", err)
		}
		if err := os.Chmod(secret, 0o000); err != nil {
			t.Fatalf("chmod 000: %v", err)
		}

		_, err := adapter.CopyToLocal(ctx, baseSource, "secret.txt", filepath.Join(t.TempDir(), "secret.txt.partial"))
		if err == nil {
			t.Fatal("CopyToLocal against a chmod 000 remote file should have been refused, it succeeded")
		}
		if got := rclone.ClassifyCtx(ctx, err); got != transport.PermissionDenied {
			t.Fatalf("ClassifyCtx(%v) = %v, want PermissionDenied", err, got)
		}
	})

	// UnsupportedCapability is the "hardened shell-less SFTP account" fact:
	// the machine's sshd forces every session to internal-sftp
	// (ForceCommand), and the backup account has no shell, so rclone's
	// shell-type detection finds no shell to run md5sum/sha1sum through and
	// Hashes() reports an empty set, for every object, readable or not.
	// This must surface as an explicit capability result, never as a
	// Permanent failure and never as a silent success.
	t.Run("unsupported_capability_remote_hash_on_a_shell_less_account", func(t *testing.T) {
		writeUploadFile(t, src, "hashable.txt", []byte("hash me please"))

		got, err := adapter.RemoteHash(ctx, baseSource, "hashable.txt", transport.SHA256)
		if err == nil {
			t.Fatalf("RemoteHash succeeded against a shell-less account (got %q); capability was supposed to be absent, not silently downgraded", got)
		}
		if cat := rclone.ClassifyCtx(ctx, err); cat != transport.UnsupportedCapability {
			t.Fatalf("ClassifyCtx(%v) = %v, want UnsupportedCapability", err, cat)
		}
	})

	t.Run("not_found_missing_remote_object", func(t *testing.T) {
		_, err := adapter.Stat(ctx, baseSource, "does-not-exist.txt")
		if err == nil {
			t.Fatal("Stat succeeded against a missing remote object")
		}
		if got := rclone.ClassifyCtx(ctx, err); got != transport.NotFound {
			t.Fatalf("ClassifyCtx(%v) = %v, want NotFound", err, got)
		}
	})

	// The two host-key cases. Before #450 each of these built an image and
	// started a container of its own inside the test function; they are two
	// properties of the harness now, and the second machine is a machine
	// rather than a rebuild.
	t.Run("host_verification_unknown_host_is_refused", func(t *testing.T) {
		// A live server at an address this known_hosts has no entry for at
		// all. The second machine really is a second machine, with its own
		// freshly generated host key, so "unknown" is unknown for the real
		// reason.
		other := m.AnotherSource(t)
		unknown := other.TransportSource("errors-unknown", "")
		unknown.KnownHosts = src.KnownHostsFile

		_, err := adapter.List(ctx, unknown)
		if err == nil {
			t.Fatal("List against a host with no known_hosts entry should have been refused, it succeeded")
		}
		if got := rclone.ClassifyCtx(ctx, err); got != transport.HostVerification {
			t.Fatalf("ClassifyCtx(%v) = %v, want HostVerification", err, got)
		}
	})

	t.Run("host_verification_changed_host_key_is_refused", func(t *testing.T) {
		// Same address, a different key pinned for it: exactly the shape of
		// a MITM, or a server quietly replaced. The harness generates the
		// decoy key itself and pins it for this machine's own host:port,
		// which is what makes this a mismatch rather than an absence.
		changed := baseSource
		changed.KnownHosts = src.BadKnownHostsFile

		_, err := adapter.List(ctx, changed)
		if err == nil {
			t.Fatal("List against a changed host key should have been refused, it succeeded")
		}
		if got := rclone.ClassifyCtx(ctx, err); got != transport.HostVerification {
			t.Fatalf("ClassifyCtx(%v) = %v, want HostVerification", err, got)
		}

		// #388's precedence question, settled against this exact real
		// error rather than a described one. A caller working under a
		// deadline can have its context expire while a handshake is still
		// in flight (rclone's sftp dial takes ssh.ClientConfig.Timeout and
		// ignores the caller's context entirely, so that window is the
		// whole handshake, not a scheduling race), and the refusal still
		// arrives afterwards. It is still a refusal. If a done context
		// outranked it, app/halt.go would never record HALT_HOST_KEY_CHANGED
		// for the one condition an operator most needs to be told about.
		if got := rclone.ClassifyCtx(alreadyCancelledContext(), err); got != transport.HostVerification {
			t.Fatalf("ClassifyCtx(done ctx, changed host key) = %v, want HostVerification: a cancellation racing a refusal does not make the refusal less true", got)
		}
	})
}

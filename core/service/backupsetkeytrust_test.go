// This file is issue #572's proof: a backup set's SSH key and its trusted
// host key can be changed after the set exists, and changing the host key
// is a decision an operator is shown before it happens.
//
// The two halves are deliberately not symmetric, and the tests say why by
// what they assert. Rotating the key is ordinary maintenance, so it is
// checked the way every other editable field is: it persists, and the
// thing that will actually use it points at the new material. Re-trusting
// a host key is a trust decision, so most of the file is about the
// refusal: what it refuses, what it says, and that a refused edit leaves
// both the configuration and the trusted line exactly as they were.
//
// Real key material throughout, generated per test. A known_hosts line
// with a made-up base64 body cannot be fingerprinted, and a refusal that
// names two fingerprints is the whole point of the feature, so a fixture
// that could not produce one would be a fixture that could not fail.
package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// newHostKey generates one throwaway ed25519 host key and renders it as
// the known_hosts line for addr, alongside the SHA256 fingerprint an
// operator would compare by eye. Both come out of the same key, so a test
// that pins a fingerprint is pinning the key the line actually carries.
func newHostKey(t *testing.T, host string, port int) (line, fingerprint string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	return knownhosts.Line([]string{addr}, pub), ssh.FingerprintSHA256(pub)
}

// newPrivateKey generates one throwaway ed25519 PRIVATE key in the OpenSSH
// format ImportSSHKey accepts, so a rotation test has a second real key to
// rotate onto rather than a second copy of the one fixture.
func newPrivateKey(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(&priv, "issue-572-test")
	if err != nil {
		t.Fatalf("ssh.MarshalPrivateKey: %v", err)
	}
	return pem.EncodeToMemory(block)
}

// createSFTPSet persists one sftp-shaped backup set through the ordinary
// create path and reports what it was created trusting. Every test below
// starts here rather than from the local-transport fixture, because a set
// with no SSH key and no known_hosts file has neither of the two things
// this issue is about.
func createSFTPSet(t *testing.T, svc *BackupService, name string) (id, keyID, hostLine, hostFingerprint string) {
	t.Helper()
	req := validCreateReq(t, svc, name)
	line, fp := newHostKey(t, req.Host, req.Port)
	req.KnownHostsLine = line
	result, err := svc.CreateBackupSet(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	return result.Set.ID, req.SSHKeyID, line, fp
}

// TestUpdateBackupSet_RotatesTheSSHKey is the ordinary case in the issue:
// the key on the source host is replaced, and the set has to be told.
//
// The assertion is not that the id was written down, it is that the file
// the transport will open to authenticate is the NEW key's. Nothing else
// would distinguish a working rotation from a config field that changed
// and a connection that still uses whatever it used before.
func TestUpdateBackupSet_RotatesTheSSHKey(t *testing.T) {
	svc, configPath := openTestService(t)
	id, oldKeyID, _, _ := createSFTPSet(t, svc, "rotate-key")

	replacement, err := svc.ImportSSHKey(context.Background(), newPrivateKey(t), "")
	if err != nil {
		t.Fatalf("ImportSSHKey: %v", err)
	}
	if replacement.ID == oldKeyID {
		t.Fatal("the replacement key got the same id as the original, so this test proves nothing")
	}

	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SSHKeyID: strPtr(replacement.ID),
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	source, set, _ := splitBackupSetID(id)
	onDisk := readBackupSetFromDisk(t, configPath, source, set)
	if onDisk.Remote.Key.File == "" {
		t.Fatal("the persisted set has no key.file at all after a rotation")
	}
	raw, err := os.ReadFile(onDisk.Remote.Key.File)
	if err != nil {
		t.Fatalf("reading the key the persisted set points at: %v", err)
	}
	_, _, fingerprint, err := rclone.ValidateImportedPrivateKey(raw, "")
	if err != nil {
		t.Fatalf("fingerprinting the key the persisted set points at: %v", err)
	}
	if fingerprint != replacement.Fingerprint {
		t.Errorf("the persisted set authenticates with %s, want the replacement key %s", fingerprint, replacement.Fingerprint)
	}
}

// TestUpdateBackupSet_UnknownSSHKeyIDIsRefused: an id no import ever
// produced must be refused before anything is written, exactly as the
// create path refuses one, rather than persisted as a path to a file that
// is not there.
func TestUpdateBackupSet_UnknownSSHKeyIDIsRefused(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "unknown-key")
	before := readFileOrFail(t, configPath)

	_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SSHKeyID: strPtr("no-such-key"),
	})
	if !errors.Is(err, ErrSSHKeyNotFound) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrSSHKeyNotFound", err)
	}
	if got := readFileOrFail(t, configPath); got != before {
		t.Error("the configuration file changed on a refused rotation")
	}
}

// TestUpdateBackupSet_RetrustsTheHostKey is the sharper case: the source
// host was rebuilt, it offers a new host key, and the set has to be able
// to trust it. With the acknowledgement, that works and the set's own
// known_hosts file holds the new line.
func TestUpdateBackupSet_RetrustsTheHostKey(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, oldLine, _ := createSFTPSet(t, svc, "retrust")

	newLine, newFingerprint := newHostKey(t, "example.internal", 22)
	if newLine == oldLine {
		t.Fatal("the replacement host key rendered the same line as the original")
	}

	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		KnownHostsLine:           strPtr(newLine),
		AcknowledgeHostKeyChange: true,
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	source, set, _ := splitBackupSetID(id)
	onDisk := readBackupSetFromDisk(t, configPath, source, set)
	if got := trustedFingerprints(t, onDisk.Remote.KnownHosts); len(got) != 1 || got[0] != newFingerprint {
		t.Errorf("the set now trusts %v, want exactly [%s]", got, newFingerprint)
	}
}

// TestUpdateBackupSet_ChangedHostKeyWithoutAcknowledgementIsRefused is the
// refusal this feature is built around. A host key that changes is exactly
// the shape of a machine in the middle, so a new one is never accepted
// quietly, and a refused edit leaves both the configuration and the
// trusted line byte for byte as they were.
func TestUpdateBackupSet_ChangedHostKeyWithoutAcknowledgementIsRefused(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "unacknowledged")

	source, set, _ := splitBackupSetID(id)
	knownHostsPath := readBackupSetFromDisk(t, configPath, source, set).Remote.KnownHosts
	configBefore := readFileOrFail(t, configPath)
	trustBefore := readFileOrFail(t, knownHostsPath)

	newLine, _ := newHostKey(t, "example.internal", 22)
	_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		KnownHostsLine: strPtr(newLine),
	})
	if !errors.Is(err, ErrHostKeyChangeNotAcknowledged) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrHostKeyChangeNotAcknowledged", err)
	}
	if got := readFileOrFail(t, configPath); got != configBefore {
		t.Error("the configuration file changed on a refused re-trust")
	}
	if got := readFileOrFail(t, knownHostsPath); got != trustBefore {
		t.Error("the trusted host-key line changed on a refused re-trust")
	}
}

// TestUpdateBackupSet_HostKeyRefusalNamesBothFingerprints: an operator
// deciding whether this is a rebuild or an attack has exactly one thing to
// go on, which is the pair of fingerprints. A refusal that named only the
// new one would be asking them to confirm something they cannot check.
func TestUpdateBackupSet_HostKeyRefusalNamesBothFingerprints(t *testing.T) {
	svc, _ := openTestService(t)
	id, _, _, oldFingerprint := createSFTPSet(t, svc, "fingerprints")

	newLine, newFingerprint := newHostKey(t, "example.internal", 22)
	_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		KnownHostsLine: strPtr(newLine),
	})
	if err == nil {
		t.Fatal("UpdateBackupSet accepted a changed host key with no acknowledgement")
	}
	msg := err.Error()
	if !strings.Contains(msg, oldFingerprint) {
		t.Errorf("the refusal does not name the fingerprint on record (%s):\n%s", oldFingerprint, msg)
	}
	if !strings.Contains(msg, newFingerprint) {
		t.Errorf("the refusal does not name the fingerprint being offered (%s):\n%s", newFingerprint, msg)
	}
}

// TestUpdateBackupSet_SameHostKeyNeedsNoAcknowledgement: re-sending the
// line the set already trusts changes no trust, so it must not ask. The
// Web UI's per-box Save sends the box's current contents, and a save made
// for some other reason must not turn into a trust prompt.
func TestUpdateBackupSet_SameHostKeyNeedsNoAcknowledgement(t *testing.T) {
	svc, _ := openTestService(t)
	id, _, line, _ := createSFTPSet(t, svc, "same-key")

	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		KnownHostsLine: strPtr(line),
	}); err != nil {
		t.Fatalf("UpdateBackupSet re-sending the trusted line: %v", err)
	}
}

// TestUpdateBackupSet_MalformedKnownHostsLineIsRefused: the line is the
// trust anchor, and one that cannot be parsed is one nothing can verify
// against. Refusing it here is what stops a set being left pinned to a
// value that only fails at the next connection.
func TestUpdateBackupSet_MalformedKnownHostsLineIsRefused(t *testing.T) {
	svc, _ := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "malformed")

	_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		KnownHostsLine:           strPtr("example.internal ssh-ed25519 not-really-base64"),
		AcknowledgeHostKeyChange: true,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrInvalidRequest", err)
	}
}

// TestUpdateBackupSet_KeyAndTrustAreNotEmptyPatches: naming either field
// and nothing else is a real edit, so it must not be refused as a patch
// that changes nothing.
func TestUpdateBackupSet_KeyAndTrustAreNotEmptyPatches(t *testing.T) {
	if (UpdateBackupSetRequest{SSHKeyID: strPtr("k")}).isEmpty() {
		t.Error("a patch naming ssh_key_id reads as empty")
	}
	if (UpdateBackupSetRequest{KnownHostsLine: strPtr("l")}).isEmpty() {
		t.Error("a patch naming known_hosts_line reads as empty")
	}
	if !(UpdateBackupSetRequest{AcknowledgeHostKeyChange: true}).isEmpty() {
		t.Error("a patch naming only the acknowledgement reads as a real edit; it names no field to change")
	}
}

// TestUpdateBackupSet_KeyAndTrustRefusedOnALocalRemote: a local-transport
// set has no SSH key and no host to trust, so naming either is a request
// with no meaning rather than one to half-apply.
func TestUpdateBackupSet_KeyAndTrustRefusedOnALocalRemote(t *testing.T) {
	svc, _ := openTestService(t)
	line, _ := newHostKey(t, "example.internal", 22)

	_, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		KnownHostsLine:           strPtr(line),
		AcknowledgeHostKeyChange: true,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrInvalidRequest", err)
	}
}

// TestUpdateBackupSet_TrustingAKeyForANewHostIsNotAHostKeyChange: an edit
// that moves the set to a different machine and pins that machine's key is
// establishing trust, not replacing it, and the question it has to answer
// is the repoint one, which is already asked. Making it answer both would
// be an acknowledgement an operator clicks through, which protects
// nothing.
func TestUpdateBackupSet_TrustingAKeyForANewHostIsNotAHostKeyChange(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "migrated")

	newLine, newFingerprint := newHostKey(t, "replacement.internal", 22)
	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		Host:           strPtr("replacement.internal"),
		KnownHostsLine: strPtr(newLine),
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	source, set, _ := splitBackupSetID(id)
	onDisk := readBackupSetFromDisk(t, configPath, source, set)
	if got := trustedFingerprints(t, onDisk.Remote.KnownHosts); len(got) != 1 || got[0] != newFingerprint {
		t.Errorf("the set now trusts %v, want exactly [%s]", got, newFingerprint)
	}
}

// TestUpdateBackupSet_UnreadableTrustOnRecordIsRefused: "I could not check
// what this set trusts" is not "nothing is being changed". Waving it
// through would mean a set adopts a new host key precisely because its old
// one could not be read, which is the one outcome an attacker would pick.
func TestUpdateBackupSet_UnreadableTrustOnRecordIsRefused(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "unreadable")

	source, set, _ := splitBackupSetID(id)
	knownHostsPath := readBackupSetFromDisk(t, configPath, source, set).Remote.KnownHosts
	if err := os.Rename(knownHostsPath, knownHostsPath+".gone"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	newLine, _ := newHostKey(t, "example.internal", 22)
	_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		KnownHostsLine: strPtr(newLine),
	})
	if !errors.Is(err, ErrHostKeyChangeNotAcknowledged) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrHostKeyChangeNotAcknowledged", err)
	}

	// The control, because a refusal with no way past it would be a
	// refusal rather than an acknowledgement: saying so out loud works.
	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		KnownHostsLine:           strPtr(newLine),
		AcknowledgeHostKeyChange: true,
	}); err != nil {
		t.Fatalf("UpdateBackupSet once acknowledged: %v", err)
	}
}

// readFileOrFail reads a whole file as a string, so a test can compare a
// before and after without a helper per file.
func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(raw)
}

// trustedFingerprints reports the SHA256 fingerprint of every host key a
// known_hosts file pins, in file order. Reading the FILE rather than the
// request is what makes "the set now trusts this" a claim about what a
// connection will check.
func trustedFingerprints(t *testing.T, path string) []string {
	t.Helper()
	rest := []byte(readFileOrFail(t, path))
	var out []string
	for len(rest) > 0 {
		_, _, key, _, remainder, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			break
		}
		out = append(out, ssh.FingerprintSHA256(key))
		rest = remainder
	}
	return out
}

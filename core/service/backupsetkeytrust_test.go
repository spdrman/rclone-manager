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
//
// Every edit here carries SkipConnectionCheck, and that is a statement
// about scope rather than a workaround. Issue #624 made UpdateBackupSet
// prove the connection in front of an edit that changes one, and every
// case in this file changes one: the fixtures point at example.internal,
// which is not a machine, so without the skip each case would drive #624's
// refusal instead of the trust decision it is about. The refusal has its
// own cases, in backupsetverified_test.go and in the CLI's own
// backupsetverify_test.go.
package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/backupd/core/internal/transport/rclone"
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
		SkipConnectionCheck: true,
		SSHKeyID:            strPtr(replacement.ID),
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
		SkipConnectionCheck: true,
		SSHKeyID:            strPtr("no-such-key"),
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
		SkipConnectionCheck:      true,
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
		SkipConnectionCheck: true,
		KnownHostsLine:      strPtr(newLine),
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
		SkipConnectionCheck: true,
		KnownHostsLine:      strPtr(newLine),
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
		SkipConnectionCheck: true,
		KnownHostsLine:      strPtr(line),
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
		SkipConnectionCheck:      true,
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
		SkipConnectionCheck:      true,
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
		SkipConnectionCheck: true,
		Host:                strPtr("replacement.internal"),
		KnownHostsLine:      strPtr(newLine),
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
		SkipConnectionCheck: true,
		KnownHostsLine:      strPtr(newLine),
	})
	if !errors.Is(err, ErrHostKeyChangeNotAcknowledged) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrHostKeyChangeNotAcknowledged", err)
	}
	// The whole error used to be interpolated into this sentence, and
	// every error a failed open produces carries the path in it, so a
	// refusal the HTTP layer echoes verbatim was also handing out this
	// process's own filesystem layout. SSHKeyRef's doc makes the rule that
	// a caller outside core/ never learns it.
	if msg := err.Error(); strings.Contains(msg, knownHostsPath) || strings.Contains(msg, filepath.Dir(configPath)) {
		t.Errorf("the refusal names this deployment's own filesystem layout:\n%s", msg)
	}

	// The control, because a refusal with no way past it would be a
	// refusal rather than an acknowledgement: saying so out loud works.
	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck:      true,
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

// # The trust FILE, rather than the field
//
// Everything above this line is about the fingerprint comparison the
// feature is built around, and every one of those tests passed while six
// separate defects sat under them. Four independent adversarial reviews of
// PR #580 turned them up, and they group into two.
//
// Four are the rest of the known_hosts line, which the first version of
// this code threw away. A line is a marker, a set of host patterns and a
// key; only the key was compared, so a certificate authority could arrive
// described as one fingerprint, a line naming somebody else's host could
// pass as "the key already trusted", a port change could look like an
// address nothing was pinned for, and a file holding two algorithms could
// lose one to a save that changed nothing.
//
// Two are the file itself, which stopped being written once and started
// being rewritten: a name that two different backup sets could share, and
// a commit ordering that could leave a configuration durably naming a
// trust anchor nobody wrote.
//
// Each test below was written against the code that had its defect and
// watched to fail for that defect's own reason, not for a proxy.

// newHostPublicKey generates one throwaway ed25519 host key and hands back
// the public half, for the tests that have to render it against more than
// one address or compare it against a file by hand. newHostKey above is
// the same generator with the line and the fingerprint already made; this
// one is for when the key itself is the thing being carried around.
func newHostPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	return pub
}

// trustFileOf reports the known_hosts path the PERSISTED configuration
// names for a set, never this process's in-memory copy: what a set trusts
// is what a restarted daemon would read.
func trustFileOf(t *testing.T, configPath, id string) string {
	t.Helper()
	source, set, ok := splitBackupSetID(id)
	if !ok {
		t.Fatalf("splitBackupSetID(%q) did not parse", id)
	}
	return readBackupSetFromDisk(t, configPath, source, set).Remote.KnownHosts
}

// TestUpdateBackupSet_APortChangeIsStillAHostKeyChange is the one an
// address-shaped lookup let straight through.
//
// hostKeyAddress includes the port, so an edit sending {port: 2222,
// known_hosts_line: anything} used to ask the current known_hosts what it
// pinned for host:2222, get nothing back because the file pins host:22,
// and read "nothing pinned at this address" as "this set trusts nothing
// yet". Neither acknowledgement fired: remote.port is deliberately not a
// repoint field either, and correctly so, which is exactly what left this
// with no gate at all. A 200, and an arbitrary key pinned.
func TestUpdateBackupSet_APortChangeIsStillAHostKeyChange(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, trustedFingerprint := createSFTPSet(t, svc, "port-change")
	trustBefore := readFileOrFail(t, trustFileOf(t, configPath, id))

	somebodyElse := newHostPublicKey(t)
	_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck: true,
		Port:                intPtr(2222),
		KnownHostsLine:      strPtr(knownhosts.Line([]string{"[example.internal]:2222"}, somebodyElse)),
	})
	if !errors.Is(err, ErrHostKeyChangeNotAcknowledged) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrHostKeyChangeNotAcknowledged", err)
	}
	// The refusal is only worth anything if it names what the operator has
	// to compare, and the key on record is addressed to the OLD port, so
	// this is the case where naming the wrong one would be easiest.
	if msg := err.Error(); !strings.Contains(msg, trustedFingerprint) {
		t.Errorf("the refusal does not name the fingerprint on record (%s):\n%s", trustedFingerprint, msg)
	}
	if got := readFileOrFail(t, trustFileOf(t, configPath, id)); got != trustBefore {
		t.Error("the trusted host-key line changed on a refused port change")
	}
}

// TestUpdateBackupSet_APortChangeKeepingTheTrustedKeyAsksNothing is the
// control for the test above, and the reason that one could not be fixed
// by refusing every port change.
//
// Moving a set to a different SSH port on the SAME host, re-sending the
// key it already trusts addressed to the new port, changes no trust at
// all. A refusal here would be a prompt an operator learns to click
// through, which is the failure the whole acknowledgement is trying not to
// become.
func TestUpdateBackupSet_APortChangeKeepingTheTrustedKeyAsksNothing(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, line, fingerprint := createSFTPSet(t, svc, "port-same-key")

	_, _, trusted, _, _, err := ssh.ParseKnownHosts([]byte(line + "\n"))
	if err != nil {
		t.Fatalf("ParseKnownHosts: %v", err)
	}
	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck: true,
		Port:                intPtr(2222),
		KnownHostsLine:      strPtr(knownhosts.Line([]string{"[example.internal]:2222"}, trusted)),
	}); err != nil {
		t.Fatalf("UpdateBackupSet moving the same key to a new port: %v", err)
	}
	if got := trustedFingerprints(t, trustFileOf(t, configPath, id)); len(got) != 1 || got[0] != fingerprint {
		t.Errorf("the set now trusts %v, want exactly [%s]", got, fingerprint)
	}
}

// TestUpdateBackupSet_ACertAuthorityLineIsRefused: the refusal an operator
// reads is "algorithm + fingerprint", and for "@cert-authority host
// ssh-ed25519 ..." that describes one key while installing a rule.
//
// The marker used to be parsed and dropped, so the operator was shown a
// single fingerprint to compare, said yes to it, and the file received the
// @cert-authority line verbatim: the set then trusted any host
// certificate that key ever signed, for as many machines as it was aimed
// at. Since a marker cannot be honestly described by the only vocabulary
// this edit has, the edit does not take one.
func TestUpdateBackupSet_ACertAuthorityLineIsRefused(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "cert-authority")
	trustBefore := readFileOrFail(t, trustFileOf(t, configPath, id))

	caLine := "@cert-authority " + knownhosts.Line([]string{"example.internal:22"}, newHostPublicKey(t))

	_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck: true,
		KnownHostsLine:      strPtr(caLine),
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrInvalidRequest", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "cert-authority") {
		t.Errorf("the refusal does not name the marker it is refusing:\n%s", msg)
	}

	// And the acknowledgement is not a way past it. This is not the "are
	// you sure this is your host" question with a yes on the end; it is a
	// different trust model arriving through a field that means one key.
	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck:      true,
		KnownHostsLine:           strPtr(caLine),
		AcknowledgeHostKeyChange: true,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateBackupSet with the acknowledgement error = %v, want ErrInvalidRequest", err)
	}
	if got := readFileOrFail(t, trustFileOf(t, configPath, id)); got != trustBefore {
		t.Error("the trusted host-key line changed on a refused certificate-authority line")
	}
}

// TestCreateBackupSet_StillAcceptsACertAuthorityLine holds the other half
// of the decision above in place, because "the edit path refuses a marker"
// is only half a rule and the missing half is the one that would quietly
// disappear. A deployment that really does run a host CA has to be able to
// say so when it configures the set, with the wizard's verify step in
// front of it. What it cannot do is change one afterwards through a field
// whose whole refusal vocabulary is a single fingerprint.
func TestCreateBackupSet_StillAcceptsACertAuthorityLine(t *testing.T) {
	svc, configPath := openTestService(t)
	req := validCreateReq(t, svc, "created-with-a-ca")
	req.KnownHostsLine = "@cert-authority " + knownhosts.Line([]string{"example.internal:22"}, newHostPublicKey(t))

	result, err := svc.CreateBackupSet(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBackupSet with a @cert-authority line: %v", err)
	}
	if got := readFileOrFail(t, trustFileOf(t, configPath, result.Set.ID)); !strings.Contains(got, "@cert-authority") {
		t.Errorf("the created set's trust file does not hold the line it was created with:\n%s", got)
	}
}

// TestUpdateBackupSet_ALineNamingAnotherHostIsRefused is the defect that
// needs no key change at all.
//
// Send the key this set ALREADY trusts, under somebody else's host name.
// The comparison found the key, so nothing was asked, and the file written
// held exactly that line: the set went on existing, with a 200, pinning
// nothing whatsoever for its own host. Every connection after it failed
// with "knownhosts: key is unknown", which reads as an attack rather than
// as the edit that caused it.
//
// The check that catches it is made against the file that would really be
// written rather than by reading host patterns here, because wildcards,
// hashed hostnames and port normalisation all live inside knownhosts' own
// matcher.
func TestUpdateBackupSet_ALineNamingAnotherHostIsRefused(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, line, fingerprint := createSFTPSet(t, svc, "other-host")

	_, _, trusted, _, _, err := ssh.ParseKnownHosts([]byte(line + "\n"))
	if err != nil {
		t.Fatalf("ParseKnownHosts: %v", err)
	}
	_, err = svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck: true,
		KnownHostsLine:      strPtr(knownhosts.Line([]string{"somewhere-else.invalid:22"}, trusted)),
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrInvalidRequest", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "example.internal:22") {
		t.Errorf("the refusal does not name the address the set could no longer verify:\n%s", msg)
	}

	// The point is not the error, it is that the set can still check its
	// own host afterwards.
	if got := trustedFingerprints(t, trustFileOf(t, configPath, id)); len(got) != 1 || got[0] != fingerprint {
		t.Fatalf("the set now trusts %v, want exactly [%s]", got, fingerprint)
	}
	check, err := knownhosts.New(trustFileOf(t, configPath, id))
	if err != nil {
		t.Fatalf("knownhosts.New: %v", err)
	}
	if err := check("example.internal:22", knownHostsAddr("example.internal:22"), trusted); err != nil {
		t.Errorf("the set can no longer verify its own host after a refused edit: %v", err)
	}
}

// TestUpdateBackupSet_DroppingAnotherPinnedKeyIsRefused: a save that
// changes no trust used to silently remove some.
//
// OpenSSH writes one line per host key algorithm, so a host answering with
// both an ed25519 and an RSA key legitimately leaves a set pinning two.
// known_hosts_line pins exactly one and the file it writes is the whole
// file, so re-sending the line the set already trusted was waved through
// as "nothing is changing" and dropped the other one. The next connection
// that negotiated the dropped algorithm failed with a key MISMATCH, which
// is the signature of an attack, weeks after an edit nobody would connect
// it to.
func TestUpdateBackupSet_DroppingAnotherPinnedKeyIsRefused(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, line, _ := createSFTPSet(t, svc, "two-algorithms")
	path := trustFileOf(t, configPath, id)

	// A second, equally valid host key for the same host, exactly as
	// `ssh-keyscan example.internal` would have appended it.
	second := newHostPublicKey(t)
	secondFingerprint := ssh.FingerprintSHA256(second)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(knownhosts.Line([]string{"example.internal:22"}, second) + "\n"); err != nil {
		t.Fatalf("appending the second host key: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck: true,
		KnownHostsLine:      strPtr(line),
	})
	if !errors.Is(err, ErrHostKeyChangeNotAcknowledged) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrHostKeyChangeNotAcknowledged", err)
	}
	if msg := err.Error(); !strings.Contains(msg, secondFingerprint) {
		t.Errorf("the refusal does not name the key that would be dropped (%s):\n%s", secondFingerprint, msg)
	}
	if got := trustedFingerprints(t, trustFileOf(t, configPath, id)); len(got) != 2 {
		t.Errorf("the set now trusts %v, want both keys still pinned", got)
	}

	// The way through, because narrowing a set to one key is a legitimate
	// thing to mean and a refusal with no answer would be a dead end.
	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck:      true,
		KnownHostsLine:           strPtr(line),
		AcknowledgeHostKeyChange: true,
	}); err != nil {
		t.Fatalf("UpdateBackupSet once acknowledged: %v", err)
	}
	if got := trustedFingerprints(t, trustFileOf(t, configPath, id)); len(got) != 1 {
		t.Errorf("the acknowledged save left %v, want the one key it was given", got)
	}
}

// TestUpdateBackupSet_ARefusedTrustChangeStagesNothing: a refusal that had
// already written a file is a refusal that changed something.
//
// The two checks that catch a bad line can only be made against the file
// that would really be written, so by the time they refuse there IS one on
// disk. This is the proof that it goes away again, and it uses the
// dropped-key refusal because that is the one that happens after the write
// rather than before it.
func TestUpdateBackupSet_ARefusedTrustChangeStagesNothing(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, line, _ := createSFTPSet(t, svc, "no-litter")
	path := trustFileOf(t, configPath, id)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(knownhosts.Line([]string{"example.internal:22"}, newHostPublicKey(t)) + "\n"); err != nil {
		t.Fatalf("appending the second host key: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dir := filepath.Dir(path)
	before := trustDirListing(t, dir)
	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck: true,
		KnownHostsLine:      strPtr(line),
	}); !errors.Is(err, ErrHostKeyChangeNotAcknowledged) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrHostKeyChangeNotAcknowledged", err)
	}
	if after := trustDirListing(t, dir); !slices.Equal(before, after) {
		t.Errorf("a refused trust change left the known_hosts directory as %v, want it as it was %v", after, before)
	}
}

// TestKnownHostsFileName_IsInjective is the sharpest statement of a defect
// that only became reachable when this PR made the file rewritable.
//
// Both call sites built "<source>_<name>_known_hosts", and
// model.NewBackupSetID bars only the "/" separator, whitespace and control
// characters, so source "api_x" + set "y" and source "api" + set "x_y" are
// two legal, distinct backup sets that landed on one filename. Before this
// PR the file was written once at create; after it, a PATCH rewrites it,
// so one set's host-key edit silently rewrote another set's trust anchor.
func TestKnownHostsFileName_IsInjective(t *testing.T) {
	if a, b := knownHostsFileName("api_x", "y"), knownHostsFileName("api", "x_y"); a == b {
		t.Fatalf("two different backup sets share the trust file %q", a)
	}
	// And the ids with nothing to escape keep the name they already have
	// on disk, which is what makes this need no migration: a set written
	// under the old scheme goes on being written under the same one.
	if got, want := knownHostsFileName("api", "nightly"), "api_nightly_known_hosts"; got != want {
		t.Errorf("knownHostsFileName(api, nightly) = %q, want %q", got, want)
	}
}

// TestUpdateBackupSet_DoesNotRewriteAnotherSetsTrustAnchor is the same
// defect end to end, through the real create and update paths, because a
// helper being injective is only interesting if both call sites use it.
func TestUpdateBackupSet_DoesNotRewriteAnotherSetsTrustAnchor(t *testing.T) {
	svc, configPath := openTestService(t)

	neighbour := validCreateReq(t, svc, "y")
	neighbour.SourceName = "api_x"
	neighbourLine, neighbourFingerprint := newHostKey(t, neighbour.Host, neighbour.Port)
	neighbour.KnownHostsLine = neighbourLine
	neighbourSet, err := svc.CreateBackupSet(context.Background(), neighbour)
	if err != nil {
		t.Fatalf("CreateBackupSet(api_x/y): %v", err)
	}

	colliding := validCreateReq(t, svc, "x_y")
	colliding.SourceName = "api"
	collidingLine, _ := newHostKey(t, colliding.Host, colliding.Port)
	colliding.KnownHostsLine = collidingLine
	collidingSet, err := svc.CreateBackupSet(context.Background(), colliding)
	if err != nil {
		t.Fatalf("CreateBackupSet(api/x_y): %v", err)
	}

	if a, b := trustFileOf(t, configPath, neighbourSet.Set.ID), trustFileOf(t, configPath, collidingSet.Set.ID); a == b {
		t.Fatalf("%s and %s share one trust file (%s)", neighbourSet.Set.ID, collidingSet.Set.ID, a)
	}

	// The re-trust that used to land on the neighbour's anchor.
	rebuilt, _ := newHostKey(t, "example.internal", 22)
	if _, err := svc.UpdateBackupSet(context.Background(), collidingSet.Set.ID, UpdateBackupSetRequest{
		SkipConnectionCheck:      true,
		KnownHostsLine:           strPtr(rebuilt),
		AcknowledgeHostKeyChange: true,
	}); err != nil {
		t.Fatalf("UpdateBackupSet(api/x_y): %v", err)
	}
	if got := trustedFingerprints(t, trustFileOf(t, configPath, neighbourSet.Set.ID)); len(got) != 1 || got[0] != neighbourFingerprint {
		t.Errorf("%s trusts %v after its neighbour was edited, want exactly [%s]",
			neighbourSet.Set.ID, got, neighbourFingerprint)
	}
}

// TestUpdateBackupSet_TrustIsInPlaceBeforeTheConfigurationNamesIt is the
// durability invariant, and it is checked by taking away the one thing the
// old ordering depended on.
//
// The trusted line used to be renamed into the set's canonical path AFTER
// writeConfigBytesAtomically, on the argument that such a rename is as
// near infallible as this package gets. A fixed path is one something else
// can be occupying, and when the rename lost, UpdateBackupSet returned an
// error with the whole rest of the edit already durably written, before
// adoptConfig and before the validator catalog was applied: disk said the
// edit had happened, the process said it had not, and the caller was told
// it failed. After a restart it took effect with a trust anchor that had
// never been written, and config.Validate does not stat known_hosts, so
// the daemon came up green and the set failed at connect time.
//
// Occupying that path is how this test reaches the step. What it asserts
// is the invariant rather than the mechanism: the edit lands in both
// places or in neither, and the trust file the persisted configuration
// names is a real file holding the key that was offered.
func TestUpdateBackupSet_TrustIsInPlaceBeforeTheConfigurationNamesIt(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "commit-order")
	source, set, _ := splitBackupSetID(id)

	previous := trustFileOf(t, configPath, id)
	occupied := filepath.Join(filepath.Dir(previous), knownHostsFileName(source, set))
	if err := os.Remove(occupied); err != nil {
		t.Fatalf("removing the canonical trust file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(occupied, "in-the-way"), 0o700); err != nil {
		t.Fatalf("occupying the canonical trust path: %v", err)
	}

	// Acknowledged, because taking the file away is also taking away what
	// this set trusts, and an unreadable anchor is refused on its own.
	rebuilt, rebuiltFingerprint := newHostKey(t, "example.internal", 22)
	updated, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck:      true,
		User:                     strPtr("rotated-user"),
		KnownHostsLine:           strPtr(rebuilt),
		AcknowledgeHostKeyChange: true,
	})
	if err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	onDisk := readBackupSetFromDisk(t, configPath, source, set)
	inMemory, err := svc.GetBackupSet(context.Background(), id)
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if onDisk.Remote.User != "rotated-user" || inMemory.User != "rotated-user" || updated.User != "rotated-user" {
		t.Fatalf("the edit is not in all three places: on disk %q, in memory %q, returned %q",
			onDisk.Remote.User, inMemory.User, updated.User)
	}
	if got := trustedFingerprints(t, onDisk.Remote.KnownHosts); len(got) != 1 || got[0] != rebuiltFingerprint {
		t.Errorf("the trust file the persisted configuration names holds %v, want exactly [%s]", got, rebuiltFingerprint)
	}
}

// TestUpdateBackupSet_LeavesThePreviousTrustFileAlone pins the deliberate
// consequence of the ordering above, because it is the kind of leftover
// somebody tidies up later without reading why it is there.
//
// A re-trust writes a NEW file and points the set at it. The one it used
// to name is left exactly where it was, and must be: a set configured by
// hand may point at a known_hosts file it SHARES with other sets, and
// deleting that would take away trust anchors nobody asked to lose.
// Nothing reads the old file afterwards, because remote.known_hosts is the
// only thing that ever named it.
func TestUpdateBackupSet_LeavesThePreviousTrustFileAlone(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, previousFingerprint := createSFTPSet(t, svc, "leaves-the-old-one")
	previous := trustFileOf(t, configPath, id)

	rebuilt, _ := newHostKey(t, "example.internal", 22)
	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SkipConnectionCheck:      true,
		KnownHostsLine:           strPtr(rebuilt),
		AcknowledgeHostKeyChange: true,
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	if now := trustFileOf(t, configPath, id); now == previous {
		t.Fatal("the re-trust wrote over the file the set was already pointing at, so nothing after the configuration write is safe")
	}
	if got := trustedFingerprints(t, previous); len(got) != 1 || got[0] != previousFingerprint {
		t.Errorf("the previous trust file now holds %v, want it untouched at [%s]", got, previousFingerprint)
	}
}

// trustDirListing reports the names in a known_hosts directory, sorted, so
// a test can say "this left nothing behind" without caring what the files
// are called.
func trustDirListing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// # What the read surface says about the trusted key
//
// The tests above are about changing a trust anchor. These are about
// being able to see one, which nothing could until now: no field on any
// read surface carried a host key or its algorithm, so the Web UI's
// connection panel printed the literal "ssh-ed25519" beside an empty
// fingerprint on every deployment. A set whose anchor was an RSA key was
// described as an ed25519 one, with no digest beside it to check that
// against, and the halt banner for a CHANGED host key linked to that panel
// so an operator could make exactly that comparison.

// TestGetBackupSet_ReportsTheHostKeyItActuallyTrusts is the claim the
// panel rests on: the algorithm and fingerprint served are the ones in the
// set's own known_hosts, read from the file rather than assumed.
func TestGetBackupSet_ReportsTheHostKeyItActuallyTrusts(t *testing.T) {
	svc, _ := openTestService(t)
	id, _, _, fingerprint := createSFTPSet(t, svc, "reports-its-key")

	got, err := svc.GetBackupSet(context.Background(), id)
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if len(got.TrustedHostKeys) != 1 {
		t.Fatalf("the set reports %d trusted host keys, want exactly 1: %+v", len(got.TrustedHostKeys), got.TrustedHostKeys)
	}
	if got.TrustedHostKeys[0].Fingerprint != fingerprint {
		t.Errorf("reported fingerprint %q, want the one the set was created trusting %q",
			got.TrustedHostKeys[0].Fingerprint, fingerprint)
	}
	if got.TrustedHostKeys[0].Algorithm != "ssh-ed25519" {
		t.Errorf("reported algorithm %q, want the key's own %q", got.TrustedHostKeys[0].Algorithm, "ssh-ed25519")
	}
	// Written by this deployment, so there is an honest answer to "when
	// was this trusted" and it is not the zero time.
	if got.TrustedHostKeyRecordedAt.IsZero() {
		t.Error("the set reports no moment for a trust anchor this deployment wrote itself")
	}
}

// TestGetBackupSet_ReportsEveryPinnedAlgorithm: a host answering with more
// than one key algorithm has a known_hosts line each, so reporting one of
// two would show an operator a fingerprint the server in front of them may
// not present. That is the same failure as reporting an invented one, one
// step subtler, which is why the field is a list.
func TestGetBackupSet_ReportsEveryPinnedAlgorithm(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, first := createSFTPSet(t, svc, "two-pinned")

	second := newHostPublicKey(t)
	path := trustFileOf(t, configPath, id)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(knownhosts.Line([]string{"example.internal:22"}, second) + "\n"); err != nil {
		t.Fatalf("appending the second host key: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := svc.GetBackupSet(context.Background(), id)
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	var reported []string
	for _, k := range got.TrustedHostKeys {
		reported = append(reported, k.Fingerprint)
	}
	want := []string{first, ssh.FingerprintSHA256(second)}
	if !slices.Equal(reported, want) {
		t.Errorf("the set reports %v, want both pinned keys %v", reported, want)
	}
}

// TestGetBackupSet_ReportsNothingRatherThanAGuess: an anchor this
// deployment cannot read has to come back as nothing at all, so the
// surface says "we could not read it" instead of drawing a panel with a
// blank where a fingerprint goes. Empty is the honest answer for an
// unreadable file, a file pinning nothing for this host, and a
// local-transport set with no host to trust; none of them is a key.
func TestGetBackupSet_ReportsNothingRatherThanAGuess(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "unreadable-anchor")

	path := trustFileOf(t, configPath, id)
	if err := os.Rename(path, path+".gone"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	got, err := svc.GetBackupSet(context.Background(), id)
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if len(got.TrustedHostKeys) != 0 {
		t.Errorf("the set reports %+v for an anchor that cannot be read, want nothing", got.TrustedHostKeys)
	}

	// The local-transport fixture, which has no host and no anchor at all.
	local, err := svc.GetBackupSet(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("GetBackupSet(%s): %v", fixtureSetID, err)
	}
	if len(local.TrustedHostKeys) != 0 {
		t.Errorf("a local-transport set reports %+v, want nothing", local.TrustedHostKeys)
	}
	if !local.TrustedHostKeyRecordedAt.IsZero() {
		t.Error("a local-transport set reports a moment it trusted a host key")
	}
}

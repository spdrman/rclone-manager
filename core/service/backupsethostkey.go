// This file is issue #572's half that is not a field: what happens when an
// edit changes which host key a backup set trusts.
//
// # Why this is not just another editable field
//
// Rotating the SSH key is maintenance. A key is replaced on the source
// host, an operator moves from a shared key to a per-set one, and nothing
// about the backup set's relationship to its data changes. It goes through
// the ordinary update path with no ceremony.
//
// The trusted host key is the other one. known_hosts_line is what pins the
// server's identity, and the wizard makes an operator choose it
// deliberately, with a warning about what a future host-key change will
// do. When that key legitimately changes, a rebuild or a migration, the
// set has to be able to be told. But a host key that has changed is also
// exactly what a machine in the middle looks like, and from here the two
// are indistinguishable: a different key is a different key.
//
// So this refuses, once, and says what it is refusing between. It names
// the fingerprint on record and the fingerprint being offered, because
// those two strings are the entire content of the decision: an operator
// deciding whether this is their rebuilt NAS or somebody else's server has
// nothing else to compare, and a refusal that named only the new one would
// be asking them to confirm something they cannot check.
//
// # Why a second acknowledgement rather than acknowledge_repoint
//
// backupsetrepoint.go's flag answers "this is the same data at a new
// address". This one answers "this is the same host with a new key". They
// coincide often enough to be tempting to merge, and merging them would be
// wrong in the direction that matters: an operator moving a set to a new
// path would be granting a re-trust they never looked at, and the one
// acknowledgement people learn to click through would be the one guarding
// the host key.
//
// # What is deliberately NOT refused
//
// Trusting a key for a host this set has no pinned key for. That is not a
// change of trust, it is trust being established, and it is what an edit
// that moves the set to a different host legitimately does. The question
// that edit has to answer is the repoint one, which is already asked.
//
// Re-sending the line already trusted. The Web UI's per-box Save sends
// what the box holds, and a save made for some other reason must not turn
// into a trust prompt for a key that is not changing.
package service

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// ErrHostKeyChangeNotAcknowledged is returned by UpdateBackupSet when
// known_hosts_line would pin a different host key for the same host than
// the one the set trusts now, and the request did not say it meant to.
// The API layer maps it to 409 BACKUP_SET_HOST_KEY_CHANGE_NOT_ACKNOWLEDGED.
var ErrHostKeyChangeNotAcknowledged = errors.New("service: this edit changes the host key this backup set trusts")

// defaultSSHPort is the port a known_hosts entry is addressed to when a
// backup set's own port is 0. Zero means "the backend's default"
// everywhere else in this codebase (config.Remote.Port), and a
// known_hosts address needs a number: knownhosts.Line normalises an
// explicit :22 away, so a set with no port configured and one that says
// 22 verify against the same line either way.
const defaultSSHPort = 22

// knownHostsLineProblem is the field rule for a known_hosts line, in the
// same shape as backupsets.go's other field rules so it can be joined into
// the same one-sentence refusal.
//
// It parses, which is more than the create path does with the same value,
// and that is deliberate rather than an inconsistency left lying around.
// The line is the trust anchor; one that does not parse pins nothing, and
// a set left holding it fails at the next connection with an error naming
// a file rather than the edit that put it there. This path also has to
// parse it anyway, because the refusal above is built out of fingerprints.
func knownHostsLineProblem(line string) string {
	if strings.TrimSpace(line) == "" {
		return "known_hosts_line must not be empty (probe and trust the host key first, or omit the field to leave the trusted key alone)"
	}
	if _, err := parseKnownHostsLine(line); err != nil {
		return "known_hosts_line is not a usable known_hosts entry: " + err.Error()
	}
	return ""
}

// parseKnownHostsLine reads the ONE host key a known_hosts line carries.
//
// One, not the first of several: a value that is really two lines is a
// caller sending something other than what this field means, and taking
// its first line would silently pin a key while ignoring another the
// caller thought they had asked for.
func parseKnownHostsLine(line string) (ssh.PublicKey, error) {
	_, _, key, _, rest, err := ssh.ParseKnownHosts([]byte(strings.TrimRight(line, "\n") + "\n"))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("it carries more than one entry, and this field pins exactly one host key")
	}
	return key, nil
}

// describeHostKey is how a host key is named to an operator: the algorithm
// and the SHA256 fingerprint, in the same form `ssh-keygen -lf` prints and
// the wizard's own verify step already shows. Never the key material,
// which would be a wall of base64 nobody compares by eye.
func describeHostKey(key ssh.PublicKey) string {
	return key.Type() + " " + ssh.FingerprintSHA256(key)
}

// hostKeyAddress is the address a known_hosts entry for this backup set is
// looked up under.
func hostKeyAddress(remote config.Remote) string {
	port := remote.Port
	if port <= 0 {
		port = defaultSSHPort
	}
	return net.JoinHostPort(remote.Host, strconv.Itoa(port))
}

// knownHostsAddr carries an address to knownhosts' own callback, which
// wants a net.Addr beside the hostname even though the hostname is what it
// prefers. There is no connection here and never will be: this compares
// two records, and dialling the host to find out what it offers is exactly
// what a trust decision must not do on the operator's behalf.
type knownHostsAddr string

func (a knownHostsAddr) Network() string { return "tcp" }
func (a knownHostsAddr) String() string  { return string(a) }

// pinnedHostKeys asks the set's CURRENT known_hosts file what it trusts for
// addr, and whether offered is one of them.
//
// It asks through knownhosts' own callback rather than by reading lines,
// and that is the load-bearing choice in this file: the callback is the
// same code path a real connection's host-key check runs, so this refuses
// exactly when a connection would have failed with a host-key mismatch,
// and stays quiet exactly when one would have succeeded. A hand-rolled
// comparison would be a second opinion, and a second opinion about host
// keys is one that can be wrong in the permissive direction.
func pinnedHostKeys(path, addr string, offered ssh.PublicKey) (matched bool, want []ssh.PublicKey, err error) {
	check, err := knownhosts.New(path)
	if err != nil {
		return false, nil, err
	}
	switch checkErr := check(addr, knownHostsAddr(addr), offered).(type) {
	case nil:
		return true, nil, nil
	case *knownhosts.KeyError:
		for _, k := range checkErr.Want {
			want = append(want, k.Key)
		}
		return false, want, nil
	default:
		return false, nil, checkErr
	}
}

// requireHostKeyChangeAcknowledgement refuses an edit that would trust a
// different host key for the same host, unless the request says it knows.
//
// current is the set as it stands, which is where the trusted file lives;
// edited is the set the request would leave behind, which is whose address
// the new line has to be about. Those differ when the same edit also moves
// the host, and then the lookup finds nothing pinned for the new address,
// which is trust being established rather than changed. That edit is
// already answering the repoint question instead.
func requireHostKeyChangeAcknowledgement(current, edited config.BackupSet, req UpdateBackupSetRequest) error {
	if req.KnownHostsLine == nil || req.AcknowledgeHostKeyChange {
		return nil
	}
	offered, err := parseKnownHostsLine(*req.KnownHostsLine)
	if err != nil {
		// Already refused as an invalid request by the field rules, which
		// run first. Nothing to compare against, and nothing to say that
		// the caller has not already been told.
		return nil
	}

	addr := hostKeyAddress(edited.Remote)
	matched, want, err := pinnedHostKeys(current.Remote.KnownHosts, addr, offered)
	if err != nil {
		// Refused rather than waved through, and it costs nothing to
		// refuse here: this runs before the first byte of the edit is
		// persisted. Proceeding on "I could not check" is how a set ends
		// up trusting a new key because its old one was unreadable, which
		// is the one outcome an attacker would choose.
		return fmt.Errorf(
			"%w: what this backup set trusts now could not be read (%v), so nothing here can tell a rebuilt host from an impersonated one. Check the fingerprint against the host itself, then re-send with acknowledge_host_key_change to proceed",
			ErrHostKeyChangeNotAcknowledged, err,
		)
	}
	if matched || len(want) == 0 {
		return nil
	}

	var onRecord []string
	for _, k := range want {
		onRecord = append(onRecord, describeHostKey(k))
	}
	return fmt.Errorf(
		"%w: %s currently trusts %s, and the line offered pins %s. A host key changes when a server is rebuilt or migrated, and it changes in exactly the same way when something else is answering in its place, so this cannot tell those apart and will not guess. Compare the offered fingerprint against the host itself, the way the wizard's verify step did the first time. Re-send with acknowledge_host_key_change to proceed",
		ErrHostKeyChangeNotAcknowledged,
		addr,
		strings.Join(onRecord, ", "),
		describeHostKey(offered),
	)
}

// stagedKnownHosts is a new trusted line written and synced but not yet in
// place: commit puts it there, discard throws it away.
//
// It exists so the ONE step that happens after the configuration file is
// written cannot fail in a way that matters, which is the discipline
// CreateBackupSet already records at length. The trusted line and the
// configuration are two files and there is no way to write both at once,
// so the choice is which order to leave a half-applied edit in. Writing
// the line first would mean a failed config write had already re-trusted a
// host key; writing it last, as a rename inside a directory whose new file
// is already synced, leaves nothing fallible after the commit point.
type stagedKnownHosts struct {
	// Path is where the line will be once committed, and is what the
	// backup set's Remote.KnownHosts is pointed at.
	Path    string
	commit  func() error
	discard func()
}

// stageKnownHostsLine prepares line as sourceName/name's trusted host key
// without putting it in place yet.
//
// The destination is this deployment's own per-set file
// (writeKnownHostsIn's convention), whatever the set currently points at.
// A set configured by hand may share one known_hosts file with several
// others, and rewriting a shared file to pin one host would take away
// entries nobody asked to lose. Writing our own and pointing the set at it
// leaves the operator's file untouched and is visible in config.yaml
// afterwards, which is the honest version of the same edit.
func stageKnownHostsLine(configPath, sourceName, name, line string) (*stagedKnownHosts, error) {
	dir := filepath.Join(filepath.Dir(configPath), "known_hosts.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sourceName+"_"+name+"_known_hosts")
	// Defense in depth, exactly as writeKnownHostsIn's own doc argues for
	// it: the caller has already refused a path-unsafe source or name, and
	// this refuses to write outside dir regardless of what let one
	// through.
	if rel, err := filepath.Rel(dir, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: source_name/name must not resolve outside the known_hosts directory", ErrInvalidRequest)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	discard := func() { _ = os.Remove(tmpPath) } // a no-op once the rename below has happened
	if _, err := tmp.WriteString(strings.TrimRight(line, "\n") + "\n"); err != nil {
		_ = tmp.Close()
		discard()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		discard()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		discard()
		return nil, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		discard()
		return nil, err
	}
	return &stagedKnownHosts{
		Path:    path,
		commit:  func() error { return os.Rename(tmpPath, path) },
		discard: discard,
	}, nil
}

// DescribeKnownHostsLine reports the algorithm and SHA256 fingerprint of
// the host key a known_hosts line carries, in the same form ProbeHostKey
// answers with and `ssh-keygen -lf` prints.
//
// It is exported for one caller: `backup-manager backup-set patch`, which
// has to be able to say which host key it just pinned. printBackupSet's
// own doc makes the rule that a command able to change a field and then
// printing a set without it leaves an operator unable to confirm what it
// did, and the backup set on the wire carries no trust anchor to print.
// Exported rather than reimplemented there so there is one answer to
// "which key is this line", shared with the refusal that compares two of
// them.
func DescribeKnownHostsLine(line string) (algorithm, fingerprint string, err error) {
	key, err := parseKnownHostsLine(line)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return key.Type(), ssh.FingerprintSHA256(key), nil
}

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
// # The three questions an offered line has to answer
//
// A known_hosts line is not one fact, it is three: a marker, a set of host
// patterns, and a key. An earlier version of this file compared only the
// third, and every defect that came out of it was some version of the same
// mistake, so the questions are now asked separately and in this order.
//
//  1. Is this a CHANGE of trust? Answered by comparing the offered key
//     against what the set's current known_hosts pins, through knownhosts'
//     own callback, at BOTH the address the set has now and the address
//     this edit leaves it at. Asking only about the edited address is what
//     let a port-only change re-pin an arbitrary key in silence: nothing
//     is pinned for host:2222 when the file pins host:22, and "nothing
//     pinned here" was being read as "this set trusts nothing".
//
//  2. Does the line WORK for this set? Answered against the file that
//     would actually be written, by asking it to verify the set's own
//     address with the offered key. A line whose host patterns name a
//     different machine parses, fingerprints, and matches the key on
//     record, and would have gone through with a 200 while leaving the set
//     unable to verify its own host ever again.
//
//  3. Does it keep everything the set trusts TODAY? Answered by asking the
//     staged file about every key the current one pins for the set's
//     address. OpenSSH writes one line per host key algorithm, so a set
//     legitimately pinning both an ed25519 and an RSA key for one host
//     would have lost one of them to a save that "changes no trust".
//
// # What is deliberately NOT refused
//
// Trusting a key for a HOST this set has no pinned key for. That is not a
// change of trust, it is trust being established, and it is what an edit
// that moves the set to a different machine legitimately does. The
// question that edit has to answer is the repoint one, and remote.host is
// a repoint field, so it is genuinely asked.
//
// The carve-out is for the host and nothing else, which is narrower than
// it used to be and deliberately so. remote.port is NOT a repoint field
// (backupsetrepoint.go says why, and is right: a port is how you reach the
// same machine), so a port change answers no other question and cannot be
// allowed to answer this one either.
//
// Re-sending the line already trusted. The Web UI's per-box Save sends
// what the box holds, and a save made for some other reason must not turn
// into a trust prompt for a key that is not changing.
package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/backupd/core/internal/config"
)

// ErrHostKeyChangeNotAcknowledged is returned by UpdateBackupSet when
// known_hosts_line would change what host keys this backup set trusts for
// its own address, and the request did not say it meant to. Both
// directions count: a different key being pinned, and a key on record
// that the offered line would stop pinning.
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
	entry, err := parseKnownHostsLine(line)
	if err != nil {
		return "known_hosts_line is not a usable known_hosts entry: " + err.Error()
	}
	if entry.Marker != "" {
		// A marker is a different trust model wearing the same syntax,
		// and the difference does not survive being described. Everything
		// this path shows an operator about a line is its algorithm and
		// its fingerprint, which for "@cert-authority host ssh-ed25519 ..."
		// names one key and hides the fact that accepting it makes the set
		// trust every host certificate that key ever signs, for as many
		// machines as it is pointed at. So the edit path takes plain host
		// keys only.
		//
		// The create path still accepts a marker, and that is not an
		// inconsistency to tidy up later: creating a set is an operator
		// choosing a trust anchor from nothing, with the wizard's verify
		// step in front of them, and a deployment that really does use a
		// host CA has to be able to say so somewhere. Changing one after
		// the fact is the part that has to be decided on its own.
		return fmt.Sprintf(
			"known_hosts_line carries the %q marker, and this field pins ONE host key rather than a rule about who may vouch for the host. Accepting a %s line here would make this backup set trust every host certificate that key signs, which is a bigger decision than the fingerprint comparison this edit shows you. Send the host's own key line instead",
			entry.Marker, entry.Marker)
	}
	return ""
}

// knownHostsEntry is the WHOLE of one known_hosts line: its marker, the
// host patterns it applies to, and the key it carries.
//
// All three, because the two that used to be thrown away were exactly the
// two that carried the defects. A marker turns "trust this server" into
// "trust anything this key vouches for"; a host pattern decides which
// machine the key is about at all. A comparison that sees only the key
// answers a question nobody asked.
type knownHostsEntry struct {
	// Marker is "@cert-authority", "@revoked" or "" for an ordinary
	// pinned host key, exactly as ssh.ParseKnownHosts reports it.
	Marker string
	// Hosts are the patterns the entry applies to, in file order.
	Hosts []string
	Key   ssh.PublicKey
}

// parseKnownHostsLine reads the ONE known_hosts entry a line carries.
//
// One, not the first of several: a value that is really two lines is a
// caller sending something other than what this field means, and taking
// its first line would silently pin a key while ignoring another the
// caller thought they had asked for.
func parseKnownHostsLine(line string) (knownHostsEntry, error) {
	marker, hosts, key, _, rest, err := ssh.ParseKnownHosts([]byte(strings.TrimRight(line, "\n") + "\n"))
	if err != nil {
		return knownHostsEntry{}, err
	}
	if strings.TrimSpace(string(rest)) != "" {
		return knownHostsEntry{}, errors.New("it carries more than one entry, and this field pins exactly one host key")
	}
	return knownHostsEntry{Marker: marker, Hosts: hosts, Key: key}, nil
}

// readKnownHostsEntries reads every entry a known_hosts file holds, in
// file order.
//
// It is the one place in this package that looks at a trust file as a LIST
// rather than as a callback, and it exists for one question only: what
// would this edit take away. Nothing here decides whether an entry applies
// to an address; that is still asked of knownhosts' own callback, one
// candidate key at a time.
func readKnownHostsEntries(path string) ([]knownHostsEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []knownHostsEntry
	rest := raw
	for len(bytes.TrimSpace(rest)) > 0 {
		marker, hosts, key, _, remainder, err := ssh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, knownHostsEntry{Marker: marker, Hosts: hosts, Key: key})
		rest = remainder
	}
	return out, nil
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

// pinnedHostKeys asks the known_hosts file at path what it trusts for
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

// unreadableTrustRefusal is what "I could not read what this set trusts"
// turns into.
//
// Refused rather than waved through, and it costs nothing to refuse: this
// runs before the first byte of the edit is persisted. Proceeding on "I
// could not check" is how a set ends up trusting a new key because its old
// one was unreadable, which is the one outcome an attacker would choose.
//
// It says WHY without saying WHERE. The whole error used to be interpolated
// in, and every error a failed open or a failed parse produces carries the
// path in it, so a refusal an operator was meant to act on was also handing
// out this process's own filesystem layout. That layout is deliberately not
// on the wire anywhere else: SSHKeyRef's own doc makes the rule, and the
// HTTP layer echoes this sentence verbatim on the strength of it being
// built from this package's own text and the caller's own values.
func unreadableTrustRefusal(err error) error {
	return fmt.Errorf(
		"%w: what this backup set trusts now could not be read (%s), so nothing here can tell a rebuilt host from an impersonated one. Check the fingerprint against the host itself, then re-send with acknowledge_host_key_change to proceed",
		ErrHostKeyChangeNotAcknowledged, describeTrustReadFailure(err),
	)
}

// describeTrustReadFailure names the reason a trust file could not be read
// in the three shapes an operator can act on, and nothing else. Anything it
// cannot classify falls to the general sentence rather than to the error's
// own text, because the error's own text is where the path is.
func describeTrustReadFailure(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "the file this set names as its known_hosts is not there"
	case errors.Is(err, fs.ErrPermission):
		return "this process is not allowed to read the file this set names as its known_hosts"
	default:
		return "the file this set names as its known_hosts is not a readable known_hosts file"
	}
}

// hostKeyChangeRefusal is the refusal itself: what is on record at addr,
// what is being offered, and where the offered line says it applies.
//
// The host patterns are in there because they are half of what a line
// means and the operator cannot see them anywhere else. A refusal naming
// two fingerprints for "example.internal:22" while the line offered is
// really about some other machine is a refusal that describes an edit
// nobody is making.
func hostKeyChangeRefusal(addr string, onRecord []ssh.PublicKey, offered knownHostsEntry) error {
	var described []string
	for _, k := range onRecord {
		described = append(described, describeHostKey(k))
	}
	return fmt.Errorf(
		"%w: %s currently trusts %s, and the line offered pins %s for %s. A host key changes when a server is rebuilt or migrated, and it changes in exactly the same way when something else is answering in its place, so this cannot tell those apart and will not guess. Compare the offered fingerprint against the host itself, the way the wizard's verify step did the first time. Re-send with acknowledge_host_key_change to proceed",
		ErrHostKeyChangeNotAcknowledged,
		addr,
		strings.Join(described, ", "),
		describeHostKey(offered.Key),
		strings.Join(offered.Hosts, ", "),
	)
}

// requireHostKeyChangeAcknowledgement refuses an edit that would trust a
// different host key for this backup set's host, unless the request says
// it knows.
//
// current is the set as it stands, which is where the trusted file lives;
// edited is the set the request would leave behind. Those differ whenever
// the edit also moves the address, and BOTH are asked about, because the
// question is "does this set stop trusting what it trusts" and the set
// trusts it at the address it has now.
//
// Asking only about the edited address is the shape of the port defect:
// hostKeyAddress includes the port, so a request carrying {port: 2222,
// known_hosts_line: anything} looked up host:2222 in a file that pins
// host:22, found nothing, and read "nothing pinned at this address" as
// "this set has no trust to change". Nothing else caught it either, since
// remote.port is not a repoint field.
//
// The one address change that still asks nothing is a change of HOST, and
// only when nothing is pinned for the new one. That edit is establishing
// trust for a machine this set has never trusted, and remote.host IS a
// repoint field, so the question it has to answer is genuinely being
// asked next door.
func requireHostKeyChangeAcknowledgement(current, edited config.BackupSet, req UpdateBackupSetRequest, offered knownHostsEntry) error {
	if req.AcknowledgeHostKeyChange {
		return nil
	}
	path := current.Remote.KnownHosts
	editedAddr := hostKeyAddress(edited.Remote)
	currentAddr := hostKeyAddress(current.Remote)

	matched, want, err := pinnedHostKeys(path, editedAddr, offered.Key)
	if err != nil {
		return unreadableTrustRefusal(err)
	}
	if matched {
		return nil
	}
	if len(want) > 0 {
		return hostKeyChangeRefusal(editedAddr, want, offered)
	}
	// Nothing is pinned for the address this edit leaves the set at. That
	// is "trust being established" only if the set pins nothing for the
	// address it has right now either, so ask.
	if currentAddr == editedAddr {
		return nil
	}
	matchedHere, wantHere, err := pinnedHostKeys(path, currentAddr, offered.Key)
	if err != nil {
		return unreadableTrustRefusal(err)
	}
	if matchedHere || len(wantHere) == 0 {
		return nil
	}
	if edited.Remote.Host != current.Remote.Host {
		return nil
	}
	return hostKeyChangeRefusal(currentAddr, wantHere, offered)
}

// requireNoDroppedTrustAcknowledgement refuses an edit whose one line
// would stop this backup set trusting something it trusts today, unless
// the request says it knows.
//
// The case that is easy to miss: OpenSSH writes one line per host key
// algorithm, so a host answering with both an ed25519 and an RSA key
// legitimately has two lines, and a set pinning both is a set in perfectly
// ordinary shape. known_hosts_line pins exactly one, and the file it
// writes is the whole file, so re-sending the ed25519 line the set already
// trusts used to be waved through as "changes no trust" and then dropped
// the RSA key. The next connection that negotiated RSA failed with a key
// mismatch, which is the shape of an attack, weeks after an edit nobody
// would connect it to.
//
// Merging the offered line into the existing file instead would be the
// wrong fix. The per-set write exists precisely so this never edits a file
// an operator maintains by hand (see stageKnownHostsLine), and a merge
// would put it back to appending into whatever the set happens to point
// at.
//
// So: it is a refusal, and it is the acknowledgeable one rather than a
// hard stop, because narrowing a set to a single key is a legitimate thing
// to mean. It just has to be meant.
func requireNoDroppedTrustAcknowledgement(currentPath, stagedPath, addr string, offered knownHostsEntry, req UpdateBackupSetRequest) error {
	if req.AcknowledgeHostKeyChange {
		return nil
	}
	dropped, err := droppedTrust(currentPath, stagedPath, addr)
	if err != nil {
		return unreadableTrustRefusal(err)
	}
	if len(dropped) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%w: saving this would stop this backup set trusting %s for %s. The line offered pins %s and the file it writes holds nothing else, so everything else on record for that address stops verifying. A host answering with more than one key algorithm has a known_hosts line each, which is the ordinary way to end up here. If narrowing this set to the one key offered is what you mean, re-send with acknowledge_host_key_change; if it is not, leave known_hosts_line out of this save",
		ErrHostKeyChangeNotAcknowledged,
		strings.Join(dropped, ", "),
		addr,
		describeHostKey(offered.Key),
	)
}

// droppedTrust reports, in operator-readable form, everything the current
// known_hosts file trusts for addr that the staged one would not.
//
// Each candidate is put to knownhosts' own callback twice, once against
// each file, rather than compared as text: "does this file trust this key
// for this address" is a question with wildcards, hashed hostnames and
// port normalisation in it, and the callback is the code a real connection
// runs.
//
// A marker entry is the exception, and it is reported without being
// checked. Whether "@cert-authority *.example.com" covers this set's host
// is a question that can only be answered with a host certificate in hand,
// and there is none here, so this says so rather than guessing in the
// permissive direction. Two things can put one in front of this, a create
// that took a marker line and a set pointed at a known_hosts file an
// operator keeps by hand, and being wrong about either costs one
// acknowledgement on one edit: that edit moves the set onto a file of its
// own, and this never fires for it again.
func droppedTrust(currentPath, stagedPath, addr string) ([]string, error) {
	entries, err := readKnownHostsEntries(currentPath)
	if err != nil {
		return nil, err
	}
	before, err := knownhosts.New(currentPath)
	if err != nil {
		return nil, err
	}
	after, err := knownhosts.New(stagedPath)
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, entry := range entries {
		if entry.Marker != "" {
			dropped = append(dropped, fmt.Sprintf("a %s rule for %s (%s)",
				entry.Marker, strings.Join(entry.Hosts, ", "), describeHostKey(entry.Key)))
			continue
		}
		if before(addr, knownHostsAddr(addr), entry.Key) != nil {
			// Not trusted for this address in the first place, so nothing
			// about this address is being taken away.
			continue
		}
		if after(addr, knownHostsAddr(addr), entry.Key) == nil {
			continue
		}
		dropped = append(dropped, describeHostKey(entry.Key))
	}
	return dropped, nil
}

// keysTrustedFor reports every plain host key the known_hosts file at path
// verifies addr with, in file order.
//
// Each candidate is read out of the file and then put back to knownhosts'
// own callback, which is the same two-step droppedTrust uses and for the
// same reason: reading gives the list, and only the callback can say
// whether an entry applies to an address, because that answer has
// wildcards, hashed hostnames and port normalisation in it.
//
// A marker entry is left out. "@cert-authority" says who may vouch for a
// host rather than which key the host has, and the surfaces this feeds
// show a fingerprint an operator compares against the server in front of
// them; a CA's fingerprint is not that, and printing it under the same
// heading would invite exactly the wrong comparison.
func keysTrustedFor(path, addr string) ([]ssh.PublicKey, error) {
	entries, err := readKnownHostsEntries(path)
	if err != nil {
		return nil, err
	}
	check, err := knownhosts.New(path)
	if err != nil {
		return nil, err
	}
	var out []ssh.PublicKey
	for _, entry := range entries {
		if entry.Marker != "" {
			continue
		}
		if check(addr, knownHostsAddr(addr), entry.Key) == nil {
			out = append(out, entry.Key)
		}
	}
	return out, nil
}

// TrustedHostKey is ONE host key a backup set actually pins, named the way
// an operator compares it: the algorithm and the SHA256 fingerprint, the
// form `ssh-keygen -lf` prints and the wizard's verify step shows. Never
// the key material, which is a wall of base64 nobody checks by eye.
type TrustedHostKey struct {
	Algorithm   string
	Fingerprint string
}

// trustedHostKeysFor reads what a backup set's known_hosts pins for that
// set's own address, and when this deployment wrote it.
//
// It reads the FILE rather than reporting anything from the configuration,
// because the file is what a connection checks. That matters more than it
// sounds: until this existed, the Web UI's connection panel printed the
// literal "ssh-ed25519" beside an empty fingerprint on every deployment,
// so a set whose anchor was an RSA key was described as an ed25519 one,
// with no digest beside it to check that against, and the halt banner for
// a CHANGED host key sent the operator to exactly that panel to make
// exactly that comparison.
//
// Empty means "this deployment could not report what this set trusts", and
// deliberately does not distinguish an unreadable file from a file that
// pins nothing for this address. Both are the same sentence to a surface,
// "we cannot show you the key", and both need the same answer from an
// operator, which is to go and look. What a surface must NOT do with it is
// print a blank where a fingerprint goes.
//
// recordedAt is the moment THIS deployment last wrote that anchor, and is
// zero unless the set points at a file in our own known_hosts.d. A set
// configured by hand may point at a known_hosts an operator maintains,
// shared with other sets, and that file's timestamp is about whatever was
// last added to it rather than about this set's host.
func trustedHostKeysFor(configPath string, bs config.BackupSet) (keys []TrustedHostKey, recordedAt time.Time) {
	path := bs.Remote.KnownHosts
	if path == "" {
		return nil, time.Time{}
	}
	found, err := keysTrustedFor(path, hostKeyAddress(bs.Remote))
	if err != nil {
		// Reported as "nothing to show" rather than as an error on a read
		// path: listing backup sets must not fail because one set's trust
		// file is missing, and the surface's answer to an unreadable
		// anchor is the same as its answer to one it could not parse.
		return nil, time.Time{}
	}
	for _, k := range found {
		keys = append(keys, TrustedHostKey{Algorithm: k.Type(), Fingerprint: ssh.FingerprintSHA256(k)})
	}
	if configPath != "" && filepath.Dir(path) == knownHostsDirIn(configPath) {
		if info, statErr := os.Stat(path); statErr == nil {
			recordedAt = info.ModTime()
		}
	}
	return keys, recordedAt
}

// knownHostsDirIn is where this deployment keeps the per-set trusted
// host-key files: configPath's sibling "known_hosts.d", mirroring
// docs/ssh-setup.md's own convention of one known_hosts file the operator
// maintains by hand.
func knownHostsDirIn(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "known_hosts.d")
}

// knownHostsFileToken folds ONE backup set id segment into a filename
// token, doubling every "_" so the separator between the two segments
// stays the only single one.
//
// Without it the name is not injective, and that stopped being a curiosity
// the moment a PATCH could rewrite the file. model.NewBackupSetID bars the
// "/" separator, whitespace and control characters and nothing else, so
// source "api_x" + set "y" and source "api" + set "x_y" are two legal,
// distinct backup sets that both landed on "api_x_y_known_hosts": one
// set's host-key edit rewriting another set's trust anchor, with a 200 and
// nothing in either set's history to say so.
//
// Doubling rather than hashing because the filenames are something an
// operator reads in a directory listing while working out which set a file
// belongs to, and a hash takes that away to solve a problem an escape
// solves.
//
// Nothing has to be migrated, and that is worth writing down rather than
// leaving to be rediscovered. The path each set uses is stored in its own
// config.yaml (remote.known_hosts), so a set written under the old name
// keeps reading the file it already names. A NEW name can never collide
// with an OLD one belonging to a DIFFERENT set either: an id with no "_"
// in it produces the same name under both schemes, and any id with one
// produces a name with a doubled "_" that the old scheme could not
// generate. What this cannot repair is two sets that already collided
// before this existed; they share one file today and go on sharing it
// until one of them is edited, at which point that one moves off to a name
// of its own.
func knownHostsFileToken(v string) string {
	return strings.ReplaceAll(v, "_", "__")
}

// knownHostsFileName is the ONE answer to "what is sourceName/name's
// trusted host-key file called", shared by the create path
// (writeKnownHostsIn) and the edit path (stageKnownHostsLine) so the two
// cannot drift into naming the same set's file differently.
func knownHostsFileName(sourceName, name string) string {
	return knownHostsFileToken(sourceName) + "_" + knownHostsFileToken(name) + "_known_hosts"
}

// knownHostsPathIn resolves sourceName/name's trusted host-key file inside
// configPath's known_hosts.d, and refuses anything that would land outside
// it.
//
// # Path safety (mandatory review finding M2, PR #155)
//
// sourceName/name are folded into ONE filename token, then filepath.Join'd
// onto dir. filepath.Join calls Clean, so an embedded "/" or ".." in
// either value resolves as a real path, not a literal character in a
// filename, verified empirically before that fix: dir=".../known_hosts.d",
// name="../../../../tmp/evil" produced a path outside both the known_hosts
// sandbox and the config directory. validateCreateRequest already refuses
// any such Name/SourceName before either caller is reached (its own
// validPathSegment check), so the filepath.Rel check here is defense in
// depth: even if some future caller arrived with a value that check never
// saw, this refuses to write outside dir.
func knownHostsPathIn(configPath, sourceName, name string) (dir, path string, err error) {
	dir = knownHostsDirIn(configPath)
	path = filepath.Join(dir, knownHostsFileName(sourceName, name))
	rel, relErr := filepath.Rel(dir, path)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w: source_name/name must not resolve outside the known_hosts directory", ErrInvalidRequest)
	}
	return dir, path, nil
}

// stagedKnownHosts is a trusted line that is already written, synced and
// linked into known_hosts.d under a name nothing else uses, waiting for
// the configuration that names it. commit keeps it; discard throws it away.
//
// # Why the file is in place BEFORE the configuration is written
//
// The trusted line and the configuration are two files and there is no way
// to write both at once, so the choice is which order to leave a
// half-applied edit in, and there is exactly one order with an answer.
//
// This used to rename the staged line into the set's canonical path AFTER
// writeConfigBytesAtomically, on the argument that a rename of an
// already-synced file inside its own directory is as close to infallible
// as this package gets. It is not infallible, and the difference showed:
// a rename onto an occupied path fails, and when it did, UpdateBackupSet
// returned an error with the entire rest of the edit already durably on
// disk, before adoptConfig and before the validator catalog was applied.
// Disk said the edit had happened, the running process said it had not,
// and the caller was told it failed. On the next restart the edit took
// effect with a trust anchor that was never written, and config.Validate
// does not stat known_hosts, so the daemon came up happily and the set
// failed at connect time instead.
//
// So the fallible steps all move to the front. The line is written to a
// FRESH name, one os.CreateTemp picks and no other file in the directory
// has, fsynced along with the directory entry, and only then is a
// configuration naming that path written. Nothing after the configuration
// write can fail, because there is nothing after it.
//
// # The stale files this leaves, and why nothing reads them
//
// A set's previous trusted line is left where it is. It is not deleted,
// and the reason is not tidiness: a set configured by hand may point at a
// known_hosts file it SHARES with several others, and deleting that would
// take away trust anchors nobody asked to lose. Nothing reads the old file
// afterwards, because the only thing that ever names it is
// remote.known_hosts in config.yaml and that now names the new one. A
// re-trust an operator does twice a year leaves a file behind each time,
// in a directory of small files under the config, which is a cost worth
// paying to never delete an anchor this code did not write.
type stagedKnownHosts struct {
	// Path is where the line already is, and is what the backup set's
	// Remote.KnownHosts is pointed at.
	Path string
	kept bool
}

// commit keeps the staged line: the configuration naming it is durably on
// disk, so it is now this set's trust anchor. It cannot fail, which is the
// whole point of the ordering above.
func (s *stagedKnownHosts) commit() { s.kept = true }

// discard removes the staged line unless commit has kept it, so a failure
// anywhere between staging and the configuration write leaves nothing
// behind. Safe to call after commit, and safe to call twice.
func (s *stagedKnownHosts) discard() {
	if s == nil || s.kept {
		return
	}
	_ = os.Remove(s.Path)
}

// stageKnownHostsLine writes line as sourceName/name's trusted host key
// and checks that it actually verifies addr, without yet pointing any
// configuration at it.
//
// The destination is this deployment's own per-set file, under a name
// derived from the backup set id (knownHostsFileName) with a fresh suffix,
// whatever the set currently points at. A set configured by hand may share
// one known_hosts file with several others, and rewriting a shared file to
// pin one host would take away entries nobody asked to lose. Writing our
// own and pointing the set at it leaves the operator's file untouched and
// is visible in config.yaml afterwards, which is the honest version of the
// same edit.
//
// The verification at the end is the check that a line's HOST PATTERNS
// have to pass, and it is made against the file that would really be
// written rather than by reading the patterns here. Without it,
// "somewhere-else.invalid <the key this set already trusts>" was accepted
// with a 200: the key matched, so nothing was asked, and the set was left
// pinning nothing at all for its own host, with every later connection
// failing on an unknown host key. Wildcards, hashed hostnames and port
// normalisation all live inside knownhosts' matcher, so the way to find
// out whether a file verifies an address is to ask the file.
func stageKnownHostsLine(configPath, sourceName, name, line, addr string, offered ssh.PublicKey) (*stagedKnownHosts, error) {
	dir, canonical, err := knownHostsPathIn(configPath, sourceName, name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	// CreateTemp both picks the unique name and creates it, so there is no
	// window where two concurrent edits could choose the same one. The
	// suffix is a number rather than anything meaningful; the set's id is
	// still the front of the name, which is what makes a directory listing
	// readable.
	f, err := os.CreateTemp(dir, filepath.Base(canonical)+".")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	staged := &stagedKnownHosts{Path: path}
	if _, err := f.WriteString(strings.TrimRight(line, "\n") + "\n"); err != nil {
		_ = f.Close()
		staged.discard()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		staged.discard()
		return nil, err
	}
	if err := f.Close(); err != nil {
		staged.discard()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		staged.discard()
		return nil, err
	}

	if err := verifiesAddress(path, addr, offered); err != nil {
		staged.discard()
		return nil, err
	}

	// The directory entry, not just the file's bytes: an fsync of the file
	// alone leaves a configuration that could survive a crash naming a
	// path that does not.
	if err := fsyncDir(dir); err != nil {
		staged.discard()
		return nil, err
	}
	return staged, nil
}

// verifiesAddress refuses a staged trust file that does not actually
// verify addr with the key it was given.
func verifiesAddress(path, addr string, offered ssh.PublicKey) error {
	check, err := knownhosts.New(path)
	if err != nil {
		return fmt.Errorf("%w: known_hosts_line is not a usable known_hosts entry: %v", ErrInvalidRequest, err)
	}
	if err := check(addr, knownHostsAddr(addr), offered); err != nil {
		return fmt.Errorf(
			"%w: the known_hosts line offered does not verify %s, which is the address this backup set connects to. A line naming a different host parses and fingerprints exactly like one naming this one, and pinning it would leave the set with nothing to check its own host against: every connection after this would fail with an unknown host key. Send the line for %s",
			ErrInvalidRequest, addr, addr)
	}
	return nil
}

// prepareTrustChange is the whole trust half of an update: the refusals,
// the staged line, and the two checks that can only be made against the
// file that would actually be written.
//
// One function rather than three calls at the call site, because the order
// is load-bearing in both directions. Every refusal has to happen before
// anything is written, and the last two questions cannot be asked until
// something is. Splitting them across UpdateBackupSet is how one of them
// gets left out of some future path.
//
// On any refusal the staged line is removed before returning, so a refused
// edit leaves known_hosts.d exactly as it found it.
func prepareTrustChange(configPath, sourceName, setName string, current, edited config.BackupSet, req UpdateBackupSetRequest) (*stagedKnownHosts, error) {
	offered, err := parseKnownHostsLine(*req.KnownHostsLine)
	if err != nil {
		// validateUpdatedBackupSet's field rules run first and already
		// refuse this, so reaching it means a caller found a way past
		// them. Refused here too rather than waved through: staging a line
		// nothing can parse would pin a trust anchor that verifies nothing.
		return nil, fmt.Errorf("%w: known_hosts_line is not a usable known_hosts entry: %v", ErrInvalidRequest, err)
	}
	if err := requireHostKeyChangeAcknowledgement(current, edited, req, offered); err != nil {
		return nil, err
	}

	addr := hostKeyAddress(edited.Remote)
	staged, err := stageKnownHostsLine(configPath, sourceName, setName, *req.KnownHostsLine, addr, offered.Key)
	if err != nil {
		return nil, err
	}
	if err := requireNoDroppedTrustAcknowledgement(current.Remote.KnownHosts, staged.Path, addr, offered, req); err != nil {
		staged.discard()
		return nil, err
	}
	return staged, nil
}

// DescribeKnownHostsLine reports the algorithm and SHA256 fingerprint of
// the host key a known_hosts line carries, in the same form ProbeHostKey
// answers with and `ssh-keygen -lf` prints.
//
// It is exported for one caller: `backupd backup-set patch`, which
// has to be able to say which host key it just pinned. printBackupSet's
// own doc makes the rule that a command able to change a field and then
// printing a set without it leaves an operator unable to confirm what it
// did, and the backup set on the wire carries no trust anchor to print.
// Exported rather than reimplemented there so there is one answer to
// "which key is this line", shared with the refusal that compares two of
// them.
func DescribeKnownHostsLine(line string) (algorithm, fingerprint string, err error) {
	entry, err := parseKnownHostsLine(line)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return entry.Key.Type(), ssh.FingerprintSHA256(entry.Key), nil
}

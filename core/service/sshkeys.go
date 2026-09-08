// Looking at the keys this deployment can reach (issue #592).
//
// # The gap
//
// ImportSSHKey has persisted keys since #146 and nothing has ever been
// able to list them. The id it returns is a bare uuid.NewString() that
// crosses the wire exactly once, in the response to the POST that created
// it, so `--ssh-key-id` and the edit box both take a value the product
// will not tell anybody. That single missing read is why the create
// wizard's "use a managed key" radio refuses at save, why the edit box
// asks an operator to paste a uuid, and why the key our own installer
// generates and mounts is unreachable from the one surface that could
// offer it.
//
// # Two listings, two different rules about paths
//
// ListSSHKeys describes THIS deployment's own key store, and carries no
// path at all. SSHKeyRef.KeyFile has been kept off the wire since #146 so
// an API caller never learns this process's filesystem layout, and an
// inventory is exactly the shape where a path column looks helpful and is
// not: the file it would name lives inside a distroless container the
// operator has no shell in, so it is a fact about us that helps nobody.
//
// DiscoverSSHKeyCandidates describes keys found ON the machine, and a
// candidate's path IS its identity to an operator: "which of these three
// files do you mean" has no other answer. What makes that safe is that
// the locations are a closed, constant set decided here (sshDiscoveryDirEnv
// and sshDiscoveryMountDir below, plus whatever key files the
// configuration itself names, plus this process's own ~/.ssh), never a
// caller-supplied path and never a recursive walk. A browser cannot widen
// the search, so this cannot be turned into a filesystem oracle, and every
// path it reports is one the operator installed or configured themselves.
//
// The handle that comes BACK is opaque either way. Selecting a candidate
// sends a candidate id, not a path, and that id only resolves against a
// fresh scan of the same fixed locations, so there is no request shape
// that names a file for this process to read.
//
// # Describing a key without reading it
//
// A discovered candidate's fingerprint comes from the .pub beside it and
// from nowhere else. A private key with no .pub is listed with an empty
// fingerprint, marked unselectable, and left unread: deriving a
// fingerprint by parsing a private key would be this package reading key
// material in order to populate a list, which is the one thing a listing
// must not do.
//
// The key STORE is the deliberate exception, and it is not the same act.
// Those files are ours, every one of them went through
// rclone.ValidateImportedPrivateKey on the way in, and there is no .pub
// beside them to read instead. What leaves this package is still only the
// public half: an algorithm, a SHA256 fingerprint, and the
// authorized_keys line, which is the one string that turns a failed
// authentication check into something an operator can fix.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// sshDiscoveryDirEnv names an optional directory this deployment mounts
// read-only for discovery to scan.
//
// It exists so that widening the search to an operator's own keys is a
// MOUNT decision rather than a code one. The packaged engine is
// gcr.io/distroless/static-debian12:nonroot with five host paths and no
// home directory at all, so "scan ~/.ssh on the NAS" is not something
// this process can be made to do by writing more Go; it needs a path
// mounted in. Reading the directory from the environment means an
// operator adds one line to .env and one mount to compose, and the scan
// gains a location instead of gaining a way to name arbitrary paths.
const sshDiscoveryDirEnv = "BACKUP_MANAGER_SSH_DISCOVERY_DIR"

// sshDiscoveryMountDir is where the canonical compose file mounts the
// installer's own generated key: install_docker_host.py writes
// <prefix>/secrets/id_ed25519 when there is none and compose mounts it
// read-only at /etc/backup-manager/id_ed25519.
//
// It is scanned as a directory rather than as that one file so a
// deployment that mounts a second key beside it is described too, and it
// is reported as searched even on a machine where it does not exist,
// because "not mounted here" is an answer and silence is not.
const sshDiscoveryMountDir = "/etc/backup-manager"

// ErrSSHKeyCandidateNotFound is returned by ImportSSHKeyCandidate when
// the id does not resolve against a fresh scan of the fixed locations.
// The API layer maps it to 400 SSH_KEY_CANDIDATE_NOT_FOUND.
//
// One error for "you sent something that is not a candidate id" and for
// "the file that candidate named is gone", deliberately: both mean the
// listing the caller is acting on is not the machine's current state, and
// the answer to both is to scan again.
var ErrSSHKeyCandidateNotFound = errors.New("service: no such SSH key candidate")

// SSHKeyListing is one key in this deployment's own store, described
// without its path and without its private half.
type SSHKeyListing struct {
	// ID is the store filename, which is the id ImportSSHKey returned and
	// the value `--ssh-key-id` and the edit box both take.
	ID string

	// Algorithm and Fingerprint are the public half's, in the form
	// `ssh-keygen -lf` prints. Both are empty when the key could not be
	// described at all, in which case Problem says why.
	Algorithm   string
	Fingerprint string

	// PublicKey is the authorized_keys line for this key.
	//
	// It is public material by definition and it is the single most
	// useful string in this listing: a verification run that reaches the
	// host, matches its key and then fails to authenticate has exactly
	// one fix, which is putting this line in the remote account's
	// authorized_keys. An operator who cannot see it has nothing to
	// paste, so the wizard would report a diagnosis with no remedy.
	PublicKey string

	// ImportedAt is when this deployment wrote the file, which for a key
	// that arrived through ImportSSHKey is when it was imported.
	ImportedAt time.Time

	// PassphraseProtected reports that the stored key needs a passphrase
	// to be usable. #269 refuses to import one without its passphrase, so
	// a key in this state got here another way (dropped in by hand, or
	// restored), and it is listed and marked rather than hidden: it is a
	// real key that simply cannot be verified until its passphrase
	// resolves, which is the same rule ImportSSHKey already applies.
	PassphraseProtected bool

	// Problem is why a row could not be described, in words that never
	// name a path. Empty for an ordinary key.
	Problem string

	// UsedBy is every backup set id whose configuration points at this
	// key, sorted. This is the column that turns a wall of uuids into a
	// decision: a key four sets depend on and a key nothing references are
	// very different things to point a fifth set at.
	UsedBy []string
}

// SSHKeyDiscoveryLocation is one place the scan looked, reported whether
// or not anything was found there.
//
// Every location is always reported, including the ones that are not
// present. An empty candidate list has two readings, "you have no keys"
// and "I could not look where your keys are", and on a packaged install
// the second is the true one. A scan that answered with a bare empty list
// would tell an operator the opposite of what happened.
type SSHKeyDiscoveryLocation struct {
	Path string

	// Kind is why this location is in the set at all, so a caller can
	// render "the key file your configuration names" differently from "a
	// directory that is not mounted in this container".
	//
	// One of: "configured-key-file", "mount", "home", "discovery-dir".
	Kind string

	// Found is how many candidates came from here.
	Found int

	// Problem is why nothing could be read from here, in words an
	// operator can act on. Empty when the location was read.
	Problem string
}

// SSHKeyCandidate is one private key file this engine can actually see,
// described from the .pub beside it.
type SSHKeyCandidate struct {
	// ID is the opaque handle ImportSSHKeyCandidate takes. It is derived
	// from the path so it needs no server-side state, and it is not a
	// path so no request can name a file for this process to read.
	ID string

	// Path is where the file was found. This is the one place in this
	// file a path is reported, and the package doc has the argument: a
	// candidate's path is its identity to the operator, and the location
	// set it comes from is closed and constant.
	Path string

	// Location is the SSHKeyDiscoveryLocation.Path this came from.
	Location string

	// Algorithm and Fingerprint come from the .pub beside the private
	// key, and are empty when there is none. They are never derived by
	// reading the private key.
	Algorithm   string
	Fingerprint string

	// PublicKey is the .pub's own line, or "" when there is none.
	PublicKey string

	// Mode is the private file's permission bits, as an operator would
	// write them ("0600").
	Mode string

	// InStore reports that a key with this fingerprint is already in this
	// deployment's key store, so selecting it again would make a second
	// copy of a key already available on the other list.
	InStore bool

	// InStoreID is that key's store id when InStore, so a caller can
	// offer the existing key rather than a duplicate import.
	InStoreID string

	// Selectable reports whether this candidate can be chosen. Reason
	// says why not when it cannot.
	Selectable bool
	Reason     string
}

// SSHKeyDiscovery is one scan: what was found, and everywhere that was
// looked. Never one without the other.
type SSHKeyDiscovery struct {
	Locations  []SSHKeyDiscoveryLocation
	Candidates []SSHKeyCandidate
}

// ListSSHKeys describes every key in this deployment's own store.
//
// It reads the store directory rather than a manifest, because the
// directory IS the record: importSSHKeyInto writes one file per key named
// by its id, so the filesystem and the listing cannot drift apart the way
// a second index would.
func (b *BackupService) ListSSHKeys(_ context.Context) ([]SSHKeyListing, error) {
	usage := b.sshKeyUsage()
	return listSSHKeysIn(b.configPath, usage)
}

// sshKeyUsage maps a store id to every backup set that points at it.
//
// It reads the loaded configuration rather than the file, so it answers
// for the same state ListBackupSets does, and it resolves a set's
// remote.key.file back to an id only when that file is actually inside
// this deployment's store. A set pointing at a key somewhere else (a
// mounted one, a hand-provisioned one) contributes nothing here, which is
// correct: that key is not in the listing to be attributed to.
func (b *BackupService) sshKeyUsage() map[string][]string {
	out := map[string][]string{}
	if b.configPath == "" {
		return out
	}
	storeDir := filepath.Join(filepath.Dir(b.configPath), "ssh_keys")
	st := b.state.Load()
	if st == nil || st.inner == nil {
		return out
	}
	for _, src := range st.inner.Config.Sources {
		for _, bs := range src.BackupSets {
			id := storeIDForKeyFile(storeDir, bs.Remote.Key.File)
			if id == "" {
				continue
			}
			out[id] = append(out[id], src.Name+"/"+bs.Name)
		}
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

// storeIDForKeyFile turns a configured key.file back into a store id, or
// "" when the file is not in the store.
//
// filepath.Clean on both sides, and a Dir comparison rather than a prefix
// one: "/keys-elsewhere/x" has "/keys" as a string prefix and is not in
// it, and that is the shape of mistake that would attribute somebody
// else's key to a store row.
func storeIDForKeyFile(storeDir, keyFile string) string {
	if keyFile == "" {
		return ""
	}
	cleaned := filepath.Clean(keyFile)
	if filepath.Dir(cleaned) != filepath.Clean(storeDir) {
		return ""
	}
	return filepath.Base(cleaned)
}

// listSSHKeysIn is ListSSHKeys' configPath-only half, following the split
// keysDirIn's own doc explains: the first-run surface has no
// BackupService and resolves the same directory from the same path.
func listSSHKeysIn(configPath string, usage map[string][]string) ([]SSHKeyListing, error) {
	dir, err := keysDirIn(configPath)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("service: reading the key store: %w", err)
	}

	out := make([]SSHKeyListing, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		listing := SSHKeyListing{ID: entry.Name(), UsedBy: usage[entry.Name()]}
		if info, err := entry.Info(); err == nil {
			listing.ImportedAt = info.ModTime()
		}
		describeStoredKey(filepath.Join(dir, entry.Name()), &listing)
		out = append(out, listing)
	}
	// Newest first, so the key an operator just imported is the row they
	// are looking for, with the id as the tiebreak so the order is total
	// and a list rendered twice cannot reshuffle under them.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ImportedAt.Equal(out[j].ImportedAt) {
			return out[i].ImportedAt.After(out[j].ImportedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// describeStoredKey fills in the public half of one stored key.
//
// The private bytes are read here and are never returned, never logged
// and never wrapped into an error: every failure below is described by
// its SHAPE, the same rule validateAndWrapKey states for the identical
// problem, so a corrupt file produces "could not be read as a private
// key" rather than a fragment of whatever it actually holds.
func describeStoredKey(path string, listing *SSHKeyListing) {
	raw, err := os.ReadFile(path)
	if err != nil {
		listing.Problem = "this key file could not be read"
		return
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err == nil {
		setPublicHalf(listing, signer.PublicKey())
		return
	}
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		listing.PassphraseProtected = true
		// An OpenSSH-format private key carries its public half in the
		// clear even when the private half is encrypted, so this row can
		// still be described. A traditional PEM-encrypted key cannot, and
		// says so rather than showing a blank fingerprint under a
		// confident heading.
		if missing.PublicKey != nil {
			setPublicHalf(listing, missing.PublicKey)
			return
		}
		listing.Problem = "this key is passphrase-protected and its public half cannot be read without it"
		return
	}
	listing.Problem = "this file could not be read as an SSH private key"
}

func setPublicHalf(listing *SSHKeyListing, pub ssh.PublicKey) {
	listing.Algorithm = pub.Type()
	listing.Fingerprint = ssh.FingerprintSHA256(pub)
	listing.PublicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
}

// DiscoverSSHKeyCandidates scans the fixed locations this engine can
// reach and describes what it finds, without reading a private key.
func (b *BackupService) DiscoverSSHKeyCandidates(ctx context.Context) (SSHKeyDiscovery, error) {
	stored, err := b.ListSSHKeys(ctx)
	if err != nil && !errors.Is(err, ErrConfigNotFileBacked) {
		return SSHKeyDiscovery{}, err
	}
	return discoverSSHKeyCandidates(b.configuredKeyFiles(), stored), nil
}

// configuredKeyFiles is every key.file this deployment's configuration
// names that is NOT inside the store, sorted and deduplicated.
//
// The store's own files are excluded because they are the other list. A
// key that appears in both would be offered twice, once as something to
// select and once as something to import a second copy of, which is the
// confusion this whole issue exists to end.
func (b *BackupService) configuredKeyFiles() []string {
	if b.configPath == "" {
		return nil
	}
	storeDir := filepath.Join(filepath.Dir(b.configPath), "ssh_keys")
	st := b.state.Load()
	if st == nil || st.inner == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, src := range st.inner.Config.Sources {
		for _, bs := range src.BackupSets {
			file := bs.Remote.Key.File
			if file == "" || seen[file] || storeIDForKeyFile(storeDir, file) != "" {
				continue
			}
			seen[file] = true
			out = append(out, file)
		}
	}
	sort.Strings(out)
	return out
}

// discoverSSHKeyCandidates is the scan itself, taking everything it needs
// as arguments so it can be exercised without a BackupService and so the
// location set is visible in one place.
func discoverSSHKeyCandidates(configuredKeyFiles []string, stored []SSHKeyListing) SSHKeyDiscovery {
	byFingerprint := map[string]string{}
	for _, k := range stored {
		if k.Fingerprint != "" {
			byFingerprint[k.Fingerprint] = k.ID
		}
	}

	var out SSHKeyDiscovery
	seenPath := map[string]bool{}

	add := func(loc *SSHKeyDiscoveryLocation, path string) {
		if seenPath[path] {
			return
		}
		seenPath[path] = true
		candidate := describeCandidate(path, loc.Path, byFingerprint)
		out.Candidates = append(out.Candidates, candidate)
		loc.Found++
	}

	// A key file the configuration itself names. It is a location of one
	// file rather than of a directory: this is a path an operator already
	// wrote down, not a place to go looking.
	for _, file := range configuredKeyFiles {
		loc := SSHKeyDiscoveryLocation{Path: file, Kind: "configured-key-file"}
		if info, err := os.Stat(file); err != nil {
			loc.Problem = locationProblem(err)
		} else if info.IsDir() {
			loc.Problem = "the configuration names this as a key file and it is a directory"
		} else {
			add(&loc, file)
		}
		out.Locations = append(out.Locations, loc)
	}

	for _, dir := range []struct {
		path string
		kind string
	}{
		{sshDiscoveryMountDir, "mount"},
		{homeSSHDir(), "home"},
		{os.Getenv(sshDiscoveryDirEnv), "discovery-dir"},
	} {
		if dir.path == "" {
			continue
		}
		loc := SSHKeyDiscoveryLocation{Path: dir.path, Kind: dir.kind}
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			loc.Problem = locationProblem(err)
			out.Locations = append(out.Locations, loc)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !looksLikeAPrivateKeyFile(entry.Name()) {
				continue
			}
			add(&loc, filepath.Join(dir.path, entry.Name()))
		}
		out.Locations = append(out.Locations, loc)
	}

	sort.Slice(out.Candidates, func(i, j int) bool { return out.Candidates[i].Path < out.Candidates[j].Path })
	return out
}

// homeSSHDir is this process's own ~/.ssh, which is real on a bare-metal
// or development run and absent in the packaged container. Reported
// either way; see SSHKeyDiscoveryLocation's doc.
func homeSSHDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ssh")
}

// locationProblem says why a location could not be read, in the three
// shapes an operator can act on and never by echoing an error that
// carries a path. It mirrors describeTrustReadFailure in
// backupsethostkey.go, for the same reason.
func locationProblem(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "this location is not present in this deployment"
	case errors.Is(err, fs.ErrPermission):
		return "this process is not allowed to read this location"
	default:
		return "this location could not be read"
	}
}

// looksLikeAPrivateKeyFile decides what in a directory is worth
// describing.
//
// It is a name filter and nothing else: the file is never opened to find
// out. Everything a .pub, a known_hosts, an authorized_keys or a config
// file would be named is excluded, and what is left is offered as a
// candidate whose fingerprint comes from the .pub beside it or does not
// come at all. A file that is not a key simply shows up unselectable with
// no fingerprint, which is the same honest row a key with no .pub gets.
func looksLikeAPrivateKeyFile(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".pub") {
		return false
	}
	switch name {
	case "known_hosts", "known_hosts.old", "authorized_keys", "authorized_keys2", "config", "environment", "rc", "agent.sock":
		return false
	}
	return true
}

// describeCandidate fills in one candidate from the .pub beside it.
//
// The private key is stat'ed and never opened. That is the whole rule
// this endpoint is built on: a fingerprint derived by parsing a private
// key would mean the act of listing what is on a machine also reads the
// secrets on it.
func describeCandidate(path, location string, storeByFingerprint map[string]string) SSHKeyCandidate {
	candidate := SSHKeyCandidate{
		ID:       candidateID(path),
		Path:     path,
		Location: location,
	}
	if info, err := os.Stat(path); err == nil {
		candidate.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
	}

	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		candidate.Reason = "there is no readable " + filepath.Base(path) + ".pub beside it, and the private key is not read to make one up"
		return candidate
	}
	parsed, comment, _, _, err := ssh.ParseAuthorizedKey(pub)
	if err != nil {
		candidate.Reason = "the file beside it is not a readable public key"
		return candidate
	}
	_ = comment
	candidate.Algorithm = parsed.Type()
	candidate.Fingerprint = ssh.FingerprintSHA256(parsed)
	candidate.PublicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(parsed)))
	if id, ok := storeByFingerprint[candidate.Fingerprint]; ok {
		candidate.InStore = true
		candidate.InStoreID = id
		candidate.Reason = "this key is already in the key store; select it there rather than importing a second copy"
		return candidate
	}
	candidate.Selectable = true
	return candidate
}

// candidateID is the opaque handle a browser sends back.
//
// A hash of the path rather than a stored token, so this needs no state
// and survives a restart, and truncated because it is an identifier
// rather than a digest anybody verifies. It is one-way in the direction
// that matters: an id cannot be turned into a path, and it only ever
// resolves by matching against a fresh scan of the fixed locations, so
// there is no id at all for a file the scan would not have offered.
func candidateID(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:12])
}

// ImportSSHKeyCandidate copies a discovered candidate into this
// deployment's key store and returns the reference to it.
//
// The private half is read once, server-side, and goes through exactly
// the same rclone.ValidateImportedPrivateKey a pasted key does, so a
// candidate cannot be imported through a check a paste would have failed.
// The original is left where it is: this is the same promise
// `--ssh-key-file` already makes, and answering it differently here would
// mean the wizard and the CLI disagreed about what selecting a key does.
//
// id is matched against a fresh scan rather than resolved into a path.
// Nothing here joins a caller's string onto a directory, so a path, a
// traversal, or an id for a file outside the fixed locations all fail the
// same way: they are not in the scan.
func (b *BackupService) ImportSSHKeyCandidate(ctx context.Context, id string) (SSHKeyRef, error) {
	if id == "" {
		return SSHKeyRef{}, fmt.Errorf("%w: a candidate id is required", ErrInvalidRequest)
	}
	found, err := b.DiscoverSSHKeyCandidates(ctx)
	if err != nil {
		return SSHKeyRef{}, err
	}
	for _, candidate := range found.Candidates {
		if candidate.ID != id {
			continue
		}
		if !candidate.Selectable {
			return SSHKeyRef{}, fmt.Errorf("%w: %s", ErrInvalidRequest, candidate.Reason)
		}
		raw, err := os.ReadFile(candidate.Path)
		if err != nil {
			// The path is deliberately not in this message. It is in the
			// listing the caller already has, and an error is the one
			// place a path reaches a log as well as a screen.
			return SSHKeyRef{}, fmt.Errorf("%w: that key could not be read from where it was found", ErrInvalidRequest)
		}
		ref, err := importSSHKeyInto(b.configPath, raw, "")
		zeroBytes(raw)
		return ref, err
	}
	return SSHKeyRef{}, fmt.Errorf("%w: %s", ErrSSHKeyCandidateNotFound, "scan again and select from the current listing")
}

// zeroBytes overwrites the copy of a private key this process made, for
// the reason internal/transport/rclone's identically named helper states:
// it defends against an accidental later reuse of freed memory, not
// against an attacker already inside this process.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// sshKeyIDFor is what backupSetResponse's ssh_key_id is built from: the
// store id a backup set's configured key file resolves to, or "" when the
// set uses a key this deployment does not manage.
//
// The empty answer is a real one and has to be rendered as such. A set
// pointing at a mounted or hand-provisioned key is in perfectly ordinary
// shape, and a UI that showed a blank where an id goes would be saying
// "this set has no key" about a set that has one.
func sshKeyIDFor(configPath string, remote config.Remote) string {
	if configPath == "" {
		return ""
	}
	return storeIDForKeyFile(filepath.Join(filepath.Dir(configPath), "ssh_keys"), remote.Key.File)
}

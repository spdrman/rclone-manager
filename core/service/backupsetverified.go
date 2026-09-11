// Issue #624 (H2.3): the mark that says a backup set's SSH connection was
// never proven, and the one thing that takes it away.
//
// # Why there is a mark at all
//
// The destination side of this product has verified by default since
// #443: `medium add` proves a bucket before it declares one, `--no-verify`
// is an explicit opt-out, and its output says in so many words that
// nothing was contacted and nothing was written. #624 gives the SOURCE
// side the same shape, and the same escape hatch, because the same
// legitimate case exists: an operator building configuration offline
// against a machine this host cannot currently reach.
//
// The escape hatch is where a mark becomes necessary. Without one, a set
// nobody ever proved and a set checked against a real server are the same
// set in every list, on every screen and in every command's output, and
// the sentence printed at creation lives exactly as long as the terminal
// scrollback. So the skip is recorded in the configuration
// (config.BackupSet.ConnectionUnverified) rather than only announced.
//
// # Why a passing test clears it, and a failing one does not
//
// A mark that could never be removed would not be a state, it would be a
// scar: a set created offline on a Tuesday and proven on the Wednesday
// would go on reporting itself unproven for the rest of its life, and
// warnings that cannot be resolved are warnings people learn to scroll
// past. So a connection test that PASSES against the set removes it.
//
// A test that fails leaves it exactly where it was, and that asymmetry is
// the whole meaning of the mark. Clearing on any call at all would turn
// "this connection works" into "somebody pressed the button", which is a
// claim nobody needs and one an operator would reasonably misread.
//
// # Why this writes config.yaml, and why that is not a promise broken
//
// TestBackupSetConnection has always described itself as read-only, and
// the write below is the TRANSITION and nothing else: it happens on a
// check that passed against a set that is currently marked, and on nothing
// else. So the ordinary "does this still work" button on a set that was
// proven at creation reads the configuration file to find that out and
// then writes nothing, which is a stat and a read on a button press rather
// than a change to what the button means.
//
// Said precisely rather than as "it still writes nothing", because it does
// open the file. The claim worth making is the one about config.yaml
// changing under an operator who pressed a read-only-looking control, and
// that only ever happens when the control has just earned it.
//
// The write happens where the check happened, and that is what settles
// #624's "which world does this run in" question rather than leaving it
// to be discovered. The engine that serves a deployment owns its
// configuration file; a CLI beside a serving engine routes the check to
// that engine (core/cmd/backupd's backupSetRoute), so the process
// that clears the mark is always the process whose configuration the mark
// is in. There is no version of this where one process proves a
// connection and another edits the file.

package service

import (
	"context"
	"errors"
	"log/slog"
	"reflect"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/backupd/core/internal/config"
	"github.com/spdrman/backupd/core/internal/obs"
	"github.com/spdrman/backupd/core/internal/transport"
)

// ErrConnectionNotProven is a write that declares what a backup set
// connects to, a create or an edit that changes it, refused because that
// connection could not be proven (issue #624).
//
// Its own sentinel, like ErrRepointNotAcknowledged and
// ErrHostKeyChangeNotAcknowledged beside it, because it is the same
// SHAPE of answer: not "your request is malformed" and not "this manager
// broke", but "what you asked for is a decision, and here is what it
// costs". The way past it is SkipConnectionCheck on either request, and
// the message names the step that failed so the operator has something
// to fix rather than only something to override.
var ErrConnectionNotProven = errors.New("service: the connection this backup set declares could not be proven")

// candidateConnectionFor is the check a create runs, built out of the
// create request itself.
//
// A create is the one write that can be checked as a candidate, because
// the request carries everything the check needs: the key by store id,
// the trusted line as text, the host, the port, the user and the path.
// That is the same request the wizard's Test connection button and the
// CLI's own pre-write check send to TestConnection, so the service asks
// exactly the question those two ask and gets the same six steps back.
// One construction rather than a copy in each caller, for the reason
// connectionSourceFor gives about the persisted shape: two of them is how
// a check starts proving a slightly different connection from the one
// that gets written.
func candidateConnectionFor(req CreateBackupSetRequest) ConnectionTestRequest {
	return ConnectionTestRequest{
		Host:           req.Host,
		Port:           req.Port,
		User:           req.User,
		SSHKeyID:       req.SSHKeyID,
		KnownHostsLine: req.KnownHostsLine,
		RemotePath:     req.RemotePath,
	}
}

// connectionSourceFor builds the transport.Source one connection test
// runs against, out of a persisted backup set.
//
// One construction, shared by the button (TestBackupSetConnection) and by
// the check UpdateBackupSet runs in front of a connection-changing edit,
// because two of them is how a check quietly starts proving a slightly
// different connection from the one a cycle makes. Everything here is the
// set's own configuration, including the two pieces that were once left
// off and had to be put back: the key passphrase's three sources, without
// which a passphrase-protected set is reported as an unreachable host,
// and #355's connection ceiling, without which the check can pass where a
// cycle fails.
func connectionSourceFor(bs config.BackupSet, keyEnc config.KeyEncryption) transport.Source {
	root := bs.RemotePath
	if root == "" {
		root = "/"
	}
	r := bs.Remote
	return transport.Source{
		ID:                   "connection-test",
		Type:                 r.Type,
		Host:                 r.Host,
		Port:                 connectionTestPort(r),
		User:                 r.User,
		KeyFile:              keyFileOf(r),
		KeyEnv:               r.Key.Env,
		KeyCommand:           r.Key.Command,
		PassphraseFile:       r.Key.Passphrase.File,
		PassphraseEnv:        r.Key.Passphrase.Env,
		PassphraseCommand:    r.Key.Passphrase.Command,
		MaxConnections:       r.MaxConnections,
		KeyEncryptionFile:    keyEnc.File,
		KeyEncryptionEnv:     keyEnc.Env,
		KeyEncryptionCommand: keyEnc.Command,
		KnownHosts:           r.KnownHosts,
		Root:                 root,
	}
}

// keyFileOf is the key file a remote names, under either spelling.
//
// config.Validate folds the deprecated key_file alias (#74) and key.file
// into each other, so a set this process is RUNNING has both. A set
// UpdateBackupSet is editing has not been through Validate yet: it is
// re-read from disk, raw, so an operator's file that still says key_file
// arrives here with Key.File empty. Reading only Key.File ran the check
// for such a set with no key at all, and made changesTheConnection see a
// key appear from nowhere the moment an edit moved the alias onto
// Key.File, which every ssh_key_id edit does.
func keyFileOf(r config.Remote) string {
	if r.Key.File != "" {
		return r.Key.File
	}
	return r.KeyFile
}

// connectionTestPort resolves the port a connection test dials.
//
// A backup set stores port 0 to mean "whatever the default SSH port is",
// which is what config.Remote.Port has always meant and what every set
// created without a --port says. A real transfer is fine with that: rclone
// resolves it. internal/sourcecheck is not, because it opens the TCP
// connection itself, and the address it built for a set with no port was
// host:0, which nothing anywhere answers.
//
// So `Test connection` on the detail page reported "nothing answered TCP on
// <host>:0" for every backup set with no explicit port, on a host that was
// backing up perfectly well, and had done since #596 built the check. The
// candidate mode never had the bug: testConnectionVia defaults the port
// with a comment saying exactly why, and the persisted mode did not get the
// same treatment. #624's two-machine end-to-end proof is what found it, on
// the first set it created without a --port.
//
// Resolved here, for the check only, so both halves stay true: the
// connection is made to 22 and the persisted set still says 0, so a future
// release that changed the default would carry this set with it rather than
// freezing today's answer into the configuration. The known_hosts line
// matches either way, because knownhosts.Line normalises an explicit :22
// away, which is the same reasoning probePortFor states one surface over.
//
// Left alone for a source that is not sftp: a local set has no port at all,
// and the five steps about reaching an SSH server are skipped for it.
func connectionTestPort(r config.Remote) int {
	if r.Type == "sftp" && r.Port <= 0 {
		return defaultSSHPort
	}
	return r.Port
}

// changesTheConnection reports whether an edit moves anything a connection
// test proves (issue #624), and decides it by building the Source the
// check would run against on each side and comparing the two.
//
// The line is still drawn by what the check can have an opinion about
// rather than by what feels important. Host, port and user decide who is
// dialling what; the key and its passphrase decide whether that account
// authenticates; the trusted line decides whether the machine answering
// is the one that was trusted; the remote path decides whether the
// account can read the folder the backups are in. Everything else on an
// edit form (where artifacts land on THIS machine, how completion is
// detected, which validator runs, the freshness budget) is a fact about
// this deployment, and running a network check for one of them would be a
// refusal an operator cannot act on.
//
// Derived rather than listed, because the list it replaced disagreed with
// the check. It named six things and connectionSourceFor beside it
// carried a seventh it called essential, the key passphrase's three
// sources, which the list did not look at. UpdateBackupSet meanwhile
// replaced the whole config.Key for any ssh_key_id at all, clearing the
// passphrase, so re-sending the id a passphrase-protected set already
// used dropped the passphrase, the list saw the same key on both sides,
// no check ran, no mark was set, and the write landed; the next cycle
// could not decrypt its own key (PR #628 review). A hand list and a
// constructor are two statements of which fields matter, and two
// statements drift. One of them now defines the other, and
// backupsetverified_test.go holds every field of the Source to being one
// the constructor fills, so a field added to transport.Source later
// cannot fall outside this comparison without a test saying so.
//
// What that pulls in beyond the six is #355's connection ceiling. It is
// on the Source because a check without it can pass where a cycle fails,
// so a ceiling that moved is a check that proved a different connection.
// Nothing on UpdateBackupSetRequest can move it today, so no edit runs a
// check for it that did not before; if a field for it is ever added, it
// will, and that is the intended direction.
//
// One place this errs, towards proving. A re-trust is staged under a
// fresh filename, so after's KnownHosts differs from before's whenever a
// line was sent, even the line already on record; the list did the same
// by asking the request rather than the paths, and checking a connection
// one more time is the harmless direction to be wrong in.
func changesTheConnection(before, after config.BackupSet, keyEnc config.KeyEncryption) bool {
	return !reflect.DeepEqual(
		comparableSource(connectionSourceFor(before, keyEnc)),
		comparableSource(connectionSourceFor(after, keyEnc)),
	)
}

// comparableSource is src with every empty slice made nil, so two Sources
// that differ only in how an absent command is spelled compare equal.
//
// Not a corner case. This product's own write path encodes a set through
// yaml.Marshal, which spells an absent key.command as `command: []`, and
// that loads back as an empty slice rather than a nil one; a Key rebuilt
// from an ssh_key_id has nil. reflect.DeepEqual tells the two apart, so
// without this every re-send of the key a product-written set already
// used ran a check for a command that was there on neither side, which
// the first run of the derived comparison found on its own fixture.
//
// By reflection over whatever slice fields the Source has rather than by
// naming them, for the same reason the comparison itself is derived: a
// list here would be one more statement of which fields matter to keep
// in step with the constructor.
func comparableSource(src transport.Source) transport.Source {
	rv := reflect.ValueOf(&src).Elem()
	for i := 0; i < rv.NumField(); i++ {
		if f := rv.Field(i); f.Kind() == reflect.Slice && f.Len() == 0 {
			f.Set(reflect.Zero(f.Type()))
		}
	}
	return src
}

// clearConnectionUnverified removes issue #624's mark from one backup set
// after a connection test has actually passed against it.
//
// It is quiet by construction: a set that is not marked, a service with
// no configuration file to write to, and an id this configuration does
// not hold all return having done nothing. That is deliberate rather than
// permissive. This is called from a check whose own result has already
// been decided, and the check succeeded; turning a bookkeeping failure
// here into a failed connection test would tell an operator their source
// is unreachable because this process could not rewrite a file.
//
// What a failure here does instead is say so on the feed, at warn, naming
// the set. An operator who sees a set still reporting itself unverified
// after a check they watched pass has a line explaining why, which is the
// difference between a bug and a mystery.
func (b *BackupService) clearConnectionUnverified(ctx context.Context, id string) {
	if b.configPath == "" {
		// A service built over an in-memory configuration (New rather
		// than Open) has no file the mark could have come from or could
		// be cleared in. Nothing to do, and nothing to report: this is
		// the shape every test fixture and the first-run surface use.
		return
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return
	}

	// Read under the same lock every other configuration write in this
	// package takes, and read from DISK rather than from b.state, for the
	// reason CreateBackupSet gives at length: the write below has to be
	// based on the file's actual current content, never on a possibly
	// stale in-memory copy of it.
	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		b.reportUnclearedMark(ctx, id, "this deployment's configuration could not be re-read", err)
		return
	}

	found := false
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name != setName {
				continue
			}
			if !cfg.Sources[i].BackupSets[j].ConnectionUnverified {
				// The ordinary case, and the one that keeps this cheap:
				// a set that was proven when it was created carries no
				// mark, so pressing the button writes nothing at all.
				return
			}
			cfg.Sources[i].BackupSets[j].ConnectionUnverified = false
			found = true
		}
	}
	if !found {
		return
	}

	// Encoded BEFORE Validate, which resolves retention, alerts and
	// defaults in place. SetBackupSetEnabled's own comment carries the
	// full reasoning: encoding afterwards would freeze this release's
	// defaults into an operator's file as though they had chosen them,
	// and clearing one set's mark would silently pin every other set's
	// policy.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		b.reportUnclearedMark(ctx, id, "this deployment's configuration could not be encoded", err)
		return
	}
	if err := cfg.Validate(); err != nil {
		b.reportUnclearedMark(ctx, id, "the configuration this would have written is not one this deployment would load", err)
		return
	}
	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		b.reportUnclearedMark(ctx, id, "this deployment's validator catalog could not be resolved", err)
		return
	}
	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		b.reportUnclearedMark(ctx, id, "this deployment's configuration could not be written", err)
		return
	}
	applyValidators()
	b.adoptConfig(cfg)

	// Said on the feed, at info, because it is a change to the
	// configuration and every other change to the configuration says so.
	// An operator watching the terminal sees the six steps pass and then
	// sees the mark come off, which is the whole story in one place.
	b.logger.Event(ctx, obs.LevelInfo, connectionTestEventName,
		"connection test: this backup set is no longer marked as unverified, because its connection has now been proven",
		slog.String("backup_set", id),
	)
}

// reportUnclearedMark says that a check passed and the mark stayed on,
// with the reason, rather than letting the two disagree in silence.
//
// The cause goes on the line because everything here is a fact about THIS
// deployment's own configuration file, which is the operator's own
// property and the thing they would go and look at. That is the opposite
// call from a connection step's cause (connectiontest.go), and it is the
// same distinction: a transport error names somebody else's host, and a
// configuration error names the file in front of you.
func (b *BackupService) reportUnclearedMark(ctx context.Context, id, what string, err error) {
	b.logger.Event(ctx, obs.LevelWarn, connectionTestEventName,
		"connection test: this backup set's connection was proven, but it is still marked as unverified because "+what,
		slog.String("backup_set", id),
		slog.String("cause", err.Error()),
	)
}

package service

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/sourcecheck"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Issue #624 (H2.3): the durable half of "this connection was never
// proven".
//
// The sentence a --no-verify create prints is read once, by whoever typed
// the command. What is still there tomorrow is a key in config.yaml, and
// these cases are about that key: that it is accepted, that it is
// reported, and that a check which passes takes it away again.
//
// Every case here goes through a real config file and a real Open,
// because the whole claim is about what survives this process. A test
// against an in-memory service would prove that a struct field can hold a
// bool.

// markTheFixtureUnverified rewrites the package's own config fixture so
// its one backup set carries the mark, and fails the test if the fixture
// changed shape underneath it.
//
// By hand rather than through CreateBackupSet, so each case fails for its
// own reason: a fixture built by the create path would go green on a
// build where creation never marked and nothing ever cleared.
func markTheFixtureUnverified(t *testing.T, configPath string) {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	marked := strings.Replace(string(raw),
		"        completion:\n",
		"        connection_unverified: true\n        completion:\n", 1)
	if marked == string(raw) {
		t.Fatalf("the fixture changed shape and this helper no longer marks anything:\n%s", raw)
	}
	if err := os.WriteFile(configPath, []byte(marked), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestBackupSet_ReportsAnUnverifiedConnection is the read. config.Load
// uses KnownFields(true), so before #624 this key was not merely ignored:
// the whole file failed to parse and the deployment would not start. That
// is what makes this a behaviour test rather than a field test.
func TestBackupSet_ReportsAnUnverifiedConnection(t *testing.T) {
	configPath := writeTestConfigFile(t)
	markTheFixtureUnverified(t, configPath)

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	got, err := svc.GetBackupSet(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if !got.ConnectionUnverified {
		t.Error("a set the configuration marks as never proven reads back as proven, which makes the mark decoration")
	}
}

// TestTestBackupSetConnection_ClearsTheMarkWhenItPasses is the transition
// #624's acceptance names: the mark is a state a passing check ends, not
// a permanent scar on a set that was created offline.
//
// The fixture's source is a local path this process really can list, so
// the check genuinely passes rather than being told to.
func TestTestBackupSetConnection_ClearsTheMarkWhenItPasses(t *testing.T) {
	configPath := writeTestConfigFile(t)
	markTheFixtureUnverified(t, configPath)

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	result, err := svc.TestBackupSetConnection(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("TestBackupSetConnection: %v", err)
	}
	if !result.OK {
		t.Fatalf("the fixture's own local source did not pass its own check, so this case proves nothing: %q %+v", result.Message, result.Checks)
	}

	got, err := svc.GetBackupSet(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if got.ConnectionUnverified {
		t.Error("a passing check left this process still reporting the set as never proven")
	}

	// And on disk, not only in this process's view. The mark exists to
	// outlive a restart, so clearing it has to as well: a set that came
	// back marked after a container restart would send an operator to
	// re-run a check that already passed.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "connection_unverified") {
		t.Errorf("the mark is still in the configuration file after a passing check:\n%s", raw)
	}
}

// TestTestBackupSetConnection_LeavesTheMarkWhenItFails is the other side,
// and it is the one that decides whether the mark means anything. A build
// that cleared on every call would pass the case above and would have
// turned "proven" into "somebody pressed the button".
func TestTestBackupSetConnection_LeavesTheMarkWhenItFails(t *testing.T) {
	configPath := writeTestConfigFile(t)
	markTheFixtureUnverified(t, configPath)

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	// The remote path the fixture reads from, taken away underneath it.
	// A local source whose directory is gone is a real failure of the one
	// step a local set actually runs, reached without a fake transport.
	set, err := svc.GetBackupSet(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if err := os.RemoveAll(set.RemotePath); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	result, err := svc.TestBackupSetConnection(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("TestBackupSetConnection: %v", err)
	}
	if result.OK {
		t.Fatalf("listing a directory that is not there passed, so this case proves nothing: %+v", result.Checks)
	}

	got, err := svc.GetBackupSet(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if !got.ConnectionUnverified {
		t.Error("a FAILING check cleared the mark, which would make the mark mean that somebody pressed the button rather than that the connection works")
	}
}

// TestTestConnection_PutsACandidateCheckOnTheDeploymentsFeed is EPIC H's
// standing rule applied to the one action that was exempt from it: every
// operator-visible action says what it is doing and how it went.
//
// The persisted check has put its six steps in the terminal since #596.
// The CANDIDATE check, which is what the wizard's Test connection button
// runs and what `backup-set create` runs before it writes, left nothing
// anywhere: the browser that pressed the button saw the steps and every
// other window, the global terminal and the log that outlives this process
// saw silence. That is most of what was wrong with the button before #596,
// surviving in the one mode that issue did not reach.
//
// It asserts the DEPLOYMENT feed rather than a set's, which is the half
// that could go wrong quietly: a candidate names a set that does not
// exist, so a line carrying a backup_set field would either invent an id
// or land on somebody else's ring.
func TestTestConnection_PutsACandidateCheckOnTheDeploymentsFeed(t *testing.T) {
	svc, _ := openTestService(t)

	ref, err := svc.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key), "")
	if err != nil {
		t.Fatalf("ImportSSHKey: %v", err)
	}
	// A loopback port with nothing on it: the check really runs and really
	// fails, which is the ordinary shape of a candidate somebody is still
	// filling in.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the probe listener: %v", err)
	}

	if _, err := svc.TestConnection(context.Background(), ConnectionTestRequest{
		Host:           "127.0.0.1",
		Port:           port,
		User:           "backup-agent",
		SSHKeyID:       ref.ID,
		KnownHostsLine: "[127.0.0.1]:" + strconv.Itoa(port) + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ7Zq1i0i7Xw3v0m7d3Wl1nZk5Q9tJm2fVYy0m9c8ZqR",
		RemotePath:     "/srv/backups",
	}); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}

	feed, err := svc.LiveActivity(context.Background(), LiveActivityRequest{DeploymentOnly: true, Limit: 200})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if feed.Deployment == nil {
		t.Fatal("the reading carries no deployment feed at all, so the candidate check went nowhere")
	}
	steps := map[string]bool{}
	for _, e := range feed.Deployment.Events {
		if e.Event != connectionTestEventName {
			continue
		}
		for _, f := range e.Fields {
			if f.Key == "step" {
				steps[f.Value] = true
			}
			if f.Key == "backup_set" {
				t.Errorf("a candidate check carried backup_set=%q, so it landed on a set's own ring instead of the deployment's", f.Value)
			}
		}
	}
	if len(steps) != len(sourcecheck.Steps) {
		t.Errorf("the deployment feed carries %d connection-test steps, want %d. Pressing the wizard's Test connection button has to leave the six steps where everybody else can read them, not only in the browser that pressed it: %v",
			len(steps), len(sourcecheck.Steps), steps)
	}
}

// TestConnectionSourceFor_DialsTheDefaultPortForASetThatNamesNone is a
// regression test for a bug #624's two-machine proof found on the first
// backup set it created without a --port.
//
// A backup set stores port 0 to mean "the default SSH port", which is what
// config.Remote.Port has always meant. A real transfer is fine with that,
// because rclone resolves it. internal/sourcecheck opens the TCP
// connection itself, so the address it built was host:0, and `Test
// connection` reported "nothing answered TCP on <host>:0" for every set
// with no explicit port, on hosts that were backing up perfectly well.
//
// The candidate mode never had it: testConnectionVia defaults the port
// with a comment saying exactly why. The persisted mode did not get the
// same treatment, and this is the case that keeps the two together.
//
// A unit test of the resolution rather than a live one, deliberately: the
// live version would have to bind port 22, which a test cannot do and
// should not want to. The end-to-end half is the two-machine proof, which
// is where this was actually found.
func TestConnectionSourceFor_DialsTheDefaultPortForASetThatNamesNone(t *testing.T) {
	sftp := func(port int) config.BackupSet {
		return config.BackupSet{Remote: config.Remote{Type: "sftp", Host: "nas.internal", Port: port}}
	}
	for _, tc := range []struct {
		name string
		set  config.BackupSet
		want int
	}{
		{"a set that names no port", sftp(0), defaultSSHPort},
		{"a set that names one", sftp(2222), 2222},
		{
			// A local source has no port at all, and the five steps about
			// reaching an SSH server are skipped for it, so inventing 22
			// here would put a number on a report about a directory.
			"a local source", config.BackupSet{Remote: config.Remote{Type: "local"}}, 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := connectionSourceFor(tc.set, config.KeyEncryption{}).Port; got != tc.want {
				t.Errorf("the check dials port %d, want %d", got, tc.want)
			}
		})
	}
}

// The two cases below are PR #628's review findings against the check
// UpdateBackupSet runs in front of a connection-changing edit: one about
// what it can cost the rest of the process, one about what it can fail to
// notice.

// silentTCPListener accepts every connection and sends nothing on any of
// them, holding each socket open until the test ends.
//
// Not a hostile host, and that is the point of using it. A load balancer
// in front of a dead backend, an appliance that answers on 22 with
// something that is not sshd, and a firewall that accepts and then drops
// all look exactly like this from outside: the TCP connect succeeds and no
// SSH identification string ever arrives. sourcecheck's own silentpeer
// cases drive the check directly against the same shape; this one is what
// it does to the service that called it.
func silentTCPListener(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return // closed by cleanup
			}
			// No t.* calls: this goroutine outlives the test body.
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

// TestUpdateBackupSet_ASilentHostCannotHoldTheConfigurationLock is the
// regression case for a deadlock, and it is written against the lock
// rather than against the check because the lock is what made it one.
//
// UpdateBackupSet runs #624's check with configMu held, and the comment
// beside the call defends that as a delay bounded by connectionTestTimeout.
// The bound was not real. The ten seconds went onto a context that
// sourcecheck never handed to the handshake, and the handshake's own
// ClientConfig.Timeout is a field ssh.NewClientConn does not read (only
// Dial does, for the TCP connect it makes itself), so against a host that
// accepted the connection and then said nothing the key exchange blocked
// forever, with the lock held. Every other configuration writer in the
// process (UpdateSettings, CreateBackupSet, SetBackupSetEnabled,
// RemoveStorageMedium, clearConnectionUnverified) then parked a goroutine
// and a connection behind it, and only a restart recovered.
//
// A background context on purpose: that is what the API handler passes,
// so the bound has to come from UpdateBackupSet and the check themselves.
// The call runs on a goroutine and is waited for with a select, because
// the failure under test is a call that never returns and a test that
// hangs reports nothing. The bound on the wait is generous over
// connectionTestTimeout because this host runs other suites at the same
// time; a build with the bug does not come back at all.
func TestUpdateBackupSet_ASilentHostCannotHoldTheConfigurationLock(t *testing.T) {
	svc, configPath := openTestService(t)
	id, _, _, _ := createSFTPSet(t, svc, "silent-host")
	host, port := silentTCPListener(t)

	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
			Host: strPtr(host),
			Port: &port,
		})
		done <- err
	}()

	bound := 3 * connectionTestTimeout
	var err error
	select {
	case err = <-done:
	case <-time.After(bound):
		t.Fatalf("UpdateBackupSet has not returned %s after pointing the set at a host that accepts and says nothing. It is holding configMu while it waits, so every other configuration write in this process is parked behind it", bound)
	}
	took := time.Since(started)

	if !errors.Is(err, ErrConnectionNotProven) {
		t.Fatalf("UpdateBackupSet returned %v, want ErrConnectionNotProven: a host that never offers a key is a connection that could not be proven", err)
	}
	if took > connectionTestTimeout+5*time.Second {
		t.Errorf("the refusal took %s; the comment beside the check promises other configuration writes wait at most connectionTestTimeout (%s)", took.Round(time.Millisecond), connectionTestTimeout)
	}

	// A refusal leaves the file alone, the same promise every other
	// refusal on this path makes.
	source, set, _ := splitBackupSetID(id)
	if onDisk := readBackupSetFromDisk(t, configPath, source, set); onDisk.Remote.Host != "example.internal" {
		t.Errorf("a refused edit reached the file: remote.host = %q on disk", onDisk.Remote.Host)
	}

	// And the lock is free again: a second edit of the same set, one that
	// changes nothing about the connection and so runs no check, goes
	// through promptly. This is the literal claim in the test's name.
	next := make(chan error, 1)
	go func() {
		_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
			LocalPath: strPtr(filepath.Join(t.TempDir(), "moved")),
		})
		next <- err
	}()
	select {
	case err := <-next:
		if err != nil {
			t.Fatalf("the edit after the refused one failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an edit that runs no check has not returned after 10s; the refused check is still holding configMu")
	}
}

// fullyConnectedSet is a backup set with EVERY field a connection test
// reads set to a non-zero value, for the two cases below.
//
// It is not a configuration config.Validate would accept: a key names one
// of File, Env or Command and this names all three, and the same for the
// passphrase. That is deliberate. connectionSourceFor does not validate,
// and these cases are about whether every field reaches the check and the
// comparison, which a fixture that could only set one of each would not
// be able to ask about the other two.
func fullyConnectedSet() config.BackupSet {
	return config.BackupSet{
		Name: "postgres-primary",
		Remote: config.Remote{
			Type:           "sftp",
			Host:           "nas.internal",
			Port:           2222,
			User:           "backup-agent",
			KnownHosts:     "/etc/backup-manager/known_hosts.d/production_postgres-primary_known_hosts",
			MaxConnections: 4,
			Key: config.Key{
				File:    "/etc/backup-manager/ssh_keys/id_ed25519",
				Env:     "BACKUP_SSH_KEY",
				Command: []string{"/usr/local/bin/fetch-key", "postgres-primary"},
				Passphrase: config.Passphrase{
					File:    "/etc/backup-manager/ssh_keys/id_ed25519.passphrase",
					Env:     "BACKUP_SSH_KEY_PASSPHRASE",
					Command: []string{"/usr/local/bin/fetch-passphrase", "postgres-primary"},
				},
			},
		},
		RemotePath: "/var/backups/postgres",
		LocalPath:  "/srv/backups/postgres",
		Include:    []string{"*.dump"},
		Completion: config.Completion{Strategy: "rename"},
		StaleAfter: config.Duration(24 * time.Hour),
	}
}

func fullKeyEncryption() config.KeyEncryption {
	return config.KeyEncryption{
		File:    "/etc/backup-manager/key-encryption",
		Env:     "BACKUP_KEY_ENCRYPTION",
		Command: []string{"/usr/local/bin/fetch-key-encryption"},
	}
}

// TestChangesTheConnection_TracksEveryFieldTheCheckProves pins what an
// edit has to move for UpdateBackupSet to prove the connection first, and
// what it may move without one.
//
// The list it was written against named six things and forgot the
// seventh. connectionSourceFor's own doc calls the key passphrase's three
// sources essential, because without them a passphrase-protected set is
// reported as an unreachable host, and the comparison beside it did not
// look at them. The update path also replaces the whole config.Key when a
// request names an ssh_key_id, which clears the passphrase, so re-sending
// the id a passphrase-protected set already used dropped the passphrase,
// the comparison saw the same key on both sides, no check ran, no mark
// was set, and the write landed. The next cycle could not authenticate.
//
// The fix is structural rather than a seventh entry, and this case is
// written to hold it to that: every field the check's Source carries is a
// field that forces a check when it moves, and every field it does not is
// a field that does not. A connection ceiling is on the first list, which
// the old six were not asking about; #355 put it on the Source because a
// check without it can pass where a cycle fails, and a ceiling that moved
// is a check that proved a different connection. Nothing on
// UpdateBackupSetRequest can move it today, so this forces no check that
// was not forced before, and if a field for it is ever added it will.
func TestChangesTheConnection_TracksEveryFieldTheCheckProves(t *testing.T) {
	keyEnc := fullKeyEncryption()
	cases := []struct {
		name string
		edit func(*config.BackupSet)
		want bool
	}{
		{"host", func(bs *config.BackupSet) { bs.Remote.Host = "other.internal" }, true},
		{"port", func(bs *config.BackupSet) { bs.Remote.Port = 22 }, true},
		{"user", func(bs *config.BackupSet) { bs.Remote.User = "someone-else" }, true},
		{"key file", func(bs *config.BackupSet) { bs.Remote.Key.File = "/elsewhere/id_ed25519" }, true},
		{"key env", func(bs *config.BackupSet) { bs.Remote.Key.Env = "OTHER_KEY" }, true},
		{"key command", func(bs *config.BackupSet) { bs.Remote.Key.Command = []string{"/usr/local/bin/fetch-key", "other"} }, true},
		{"key passphrase file", func(bs *config.BackupSet) { bs.Remote.Key.Passphrase.File = "" }, true},
		{"key passphrase env", func(bs *config.BackupSet) { bs.Remote.Key.Passphrase.Env = "" }, true},
		{"key passphrase command", func(bs *config.BackupSet) { bs.Remote.Key.Passphrase.Command = nil }, true},
		{"the whole key replaced, as an ssh_key_id edit does", func(bs *config.BackupSet) {
			bs.Remote.Key = config.Key{File: bs.Remote.Key.File}
		}, true},
		{"trusted line", func(bs *config.BackupSet) { bs.Remote.KnownHosts = "/elsewhere/known_hosts" }, true},
		{"remote path", func(bs *config.BackupSet) { bs.RemotePath = "/var/backups/other" }, true},
		{"connection ceiling", func(bs *config.BackupSet) { bs.Remote.MaxConnections = 1 }, true},

		{"local path", func(bs *config.BackupSet) { bs.LocalPath = "/srv/elsewhere" }, false},
		{"include", func(bs *config.BackupSet) { bs.Include = []string{"*.sql"} }, false},
		{"completion", func(bs *config.BackupSet) {
			bs.Completion = config.Completion{Strategy: "stable", StableFor: config.Duration(90 * time.Second)}
		}, false},
		{"stale_after", func(bs *config.BackupSet) { bs.StaleAfter = config.Duration(36 * time.Hour) }, false},
		{"validator", func(bs *config.BackupSet) { bs.Validation.ValidatorID = "trailer-marker" }, false},
		{"the mark itself", func(bs *config.BackupSet) { bs.ConnectionUnverified = true }, false},
		{"nothing", func(*config.BackupSet) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := fullyConnectedSet()
			after := fullyConnectedSet()
			tc.edit(&after)
			if got := changesTheConnection(before, after, keyEnc); got != tc.want {
				if tc.want {
					t.Errorf("moving %s is reported as not changing the connection, so an edit of it runs no check, sets no mark, and lands", tc.name)
				} else {
					t.Errorf("moving %s is reported as changing the connection, so an edit of it would run a network check for a fact about this deployment", tc.name)
				}
			}
		})
	}
}

// TestChangesTheConnection_AnAbsentCommandSpelledAsAnEmptyListIsNotAChange
// is the case the derived comparison found on its own fixture the first
// time it ran. This product's own write path spells an absent key.command
// as `command: []`, which loads back as an empty slice, and a Key rebuilt
// from an ssh_key_id carries a nil one. reflect.DeepEqual tells the two
// apart, so a comparison that did not normalise them ran a check on every
// same-key re-send against every set this product had ever written, for a
// command that was there on neither side.
func TestChangesTheConnection_AnAbsentCommandSpelledAsAnEmptyListIsNotAChange(t *testing.T) {
	before := fullyConnectedSet()
	before.Remote.Key.Command = []string{}
	before.Remote.Key.Passphrase.Command = []string{}
	after := fullyConnectedSet()
	after.Remote.Key.Command = nil
	after.Remote.Key.Passphrase.Command = nil
	if changesTheConnection(before, after, fullKeyEncryption()) {
		t.Error("a command absent on both sides, spelled [] on one and nil on the other, is reported as a change, so every same-key re-send on a set this product wrote runs a check")
	}
}

// TestConnectionSourceFor_LeavesNoFieldOfTheSourceUnset is the control on
// the case above, and it is the same control backupsetupdate_test.go keeps
// on its whole-struct isolation comparison: a comparison over a Source
// proves nothing about a field the Source never carries.
//
// changesTheConnection is derived from connectionSourceFor, so the two
// cannot disagree about which fields matter. What they can still both be
// wrong about together is a field transport.Source gains later that
// connectionSourceFor does not fill: the check would run without it and
// the comparison could not see it move, and neither would say so. This
// walks every field of the Source built from a set that sets everything
// and requires it non-zero, so that field fails here on the day it lands
// rather than on the day a cycle fails where the check passed.
//
// No exemption ledger, on purpose. Every field transport.Source has today
// is one the set's own configuration answers, and a field that genuinely
// cannot be is a reason to write down beside a ledger entry, not a reason
// to have the ledger in advance.
func TestConnectionSourceFor_LeavesNoFieldOfTheSourceUnset(t *testing.T) {
	src := connectionSourceFor(fullyConnectedSet(), fullKeyEncryption())
	rv := reflect.ValueOf(src)
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).IsZero() {
			t.Errorf("transport.Source.%s is zero in the Source connectionSourceFor builds from a set that sets everything. Fill it from the set, so the check proves the connection a cycle makes and changesTheConnection can see it move; or, if it genuinely cannot be, say why here and exempt it",
				rv.Type().Field(i).Name)
		}
	}
	if _, isSource := any(src).(transport.Source); !isSource {
		t.Fatal("connectionSourceFor no longer returns a transport.Source, so this walk is over the wrong type")
	}
}

// givePassphraseOnDisk rewrites configPath so the named set's key carries
// a passphrase file, the way an operator whose store key is
// passphrase-protected has to configure it: ImportSSHKey persists a key
// exactly as given, still protected if it was, and nothing on the update
// request can set a passphrase source, so the only way a set gets one is
// by hand.
//
// Through the config types rather than a string replacement, because the
// indentation the encoder chose for a nested key block is not something a
// fixture should guess at.
func givePassphraseOnDisk(t *testing.T, configPath, sourceName, setName, passphrasePath string) {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	target := findBackupSetPointer(&cfg, sourceName, setName)
	if target == nil {
		t.Fatalf("the fixture has no backup set %s/%s:\n%s", sourceName, setName, raw)
	}
	target.Remote.Key.Passphrase.File = passphrasePath
	encoded, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if err := os.WriteFile(configPath, encoded, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestUpdateBackupSet_ResendingTheSameKeyKeepsItsPassphrase is the
// reachable half of the finding TestChangesTheConnection_TracksEveryField
// TheCheckProves pins structurally, driven through the service.
//
// An edit that names the ssh_key_id a set already uses has not changed
// the key, so it is neither a rotation nor a connection change, and it
// has to come out the other side as the no-op it is: the passphrase that
// decrypts that key still on the set, no check run, and no mark. The edit
// deliberately does NOT skip the check. The set's host is a name that
// resolves nowhere, so a build that treats this as a connection change is
// refused here, loudly, rather than passing because it was told to skip.
//
// The old behaviour replaced the whole config.Key for any ssh_key_id at
// all, which cleared the passphrase, and compared only the four key
// spellings, which said nothing had moved. The write landed and the next
// cycle could not decrypt its own key.
func TestUpdateBackupSet_ResendingTheSameKeyKeepsItsPassphrase(t *testing.T) {
	svc, configPath := openTestService(t)
	id, keyID, _, _ := createSFTPSet(t, svc, "same-key")
	source, set, _ := splitBackupSetID(id)

	passphrasePath := filepath.Join(t.TempDir(), "passphrase")
	givePassphraseOnDisk(t, configPath, source, set, passphrasePath)
	if before := readBackupSetFromDisk(t, configPath, source, set); before.Remote.Key.Passphrase.File != passphrasePath {
		t.Fatalf("the fixture did not take the passphrase, so this case proves nothing: %+v", before.Remote.Key)
	}

	// The mark as the fixture left it, read before the edit rather than
	// assumed: createSFTPSet builds from validCreateReq, which skips the
	// create-time check, so the set starts out marked. What this edit must
	// not do is MOVE it, in either direction.
	markBefore := readBackupSetFromDisk(t, configPath, source, set).ConnectionUnverified

	if _, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
		SSHKeyID: strPtr(keyID),
	}); err != nil {
		t.Fatalf("re-sending the key this set already uses was refused: %v", err)
	}

	after := readBackupSetFromDisk(t, configPath, source, set)
	if after.Remote.Key.Passphrase.File != passphrasePath {
		t.Errorf("re-sending the key this set already uses dropped its passphrase: key = %+v on disk. The next cycle cannot decrypt the key it still names", after.Remote.Key)
	}
	if after.Remote.Key.File == "" {
		t.Errorf("the set has no key.file at all after re-sending its own key: %+v", after.Remote.Key)
	}
	if after.ConnectionUnverified != markBefore {
		t.Errorf("nothing about the connection moved, and the edit moved the mark from %v to %v", markBefore, after.ConnectionUnverified)
	}
}

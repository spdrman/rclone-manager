package service

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/sourcecheck"
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

// TestCreateBackupSet_MarksASetItWasNotAskedToProve is the write half:
// what a --no-verify create leaves behind, asked of the service rather
// than of the command, because both surfaces write through here.
//
// The control matters as much as the case. An ordinary create must leave
// the key OUT of the file entirely rather than write `false`, because
// every configuration written before this field existed says nothing and
// has to keep meaning what it always meant.
func TestCreateBackupSet_MarksASetItWasNotAskedToProve(t *testing.T) {
	svc, configPath := openTestService(t)

	req := validCreateReq(t, svc, "offline-set")
	req.ConnectionUnverified = true
	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	got, err := svc.GetBackupSet(context.Background(), "api/offline-set")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if !got.ConnectionUnverified {
		t.Error("a set created without a check reads back as one that was checked")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "connection_unverified: true") {
		t.Errorf("the mark did not reach the configuration file:\n%s", raw)
	}

	proven := validCreateReq(t, svc, "proven-set")
	if _, err := svc.CreateBackupSet(context.Background(), proven); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Count(string(after), "connection_unverified") != 1 {
		t.Errorf("an ordinary create wrote the key too; absence has to keep meaning what it meant in every file written before this field existed:\n%s", after)
	}
}

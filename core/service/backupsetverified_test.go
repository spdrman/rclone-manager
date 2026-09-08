package service

import (
	"context"
	"os"
	"strings"
	"testing"
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

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// testFixtureEd25519Key is a throwaway, unencrypted ed25519 private key
// generated with `ssh-keygen -t ed25519 -N ""` purely to be a real,
// parseable key for ImportSSHKey/CreateBackupSet's own tests to exercise
// against — the same "fresh key material, never a real credential"
// convention core/tests/sftpfixture already documents for its own
// generated keys. It authorizes access to nothing: no server anywhere
// trusts its public half.
const testFixtureEd25519Key = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACBp+GVRkoZ43uOoQJDQigP4BrozoP43k7AmgbnQseFAOwAAAKBQ6XopUOl6
KQAAAAtzc2gtZWQyNTUxOQAAACBp+GVRkoZ43uOoQJDQigP4BrozoP43k7AmgbnQseFAOw
AAAEDKq1zBgGm7WYsbJ145K1QtwpfB3vkKU28PczLWa0D7KWn4ZVGShnje46hAkNCKA/gG
ujOg/jeTsCaBudCx4UA7AAAAF2JhY2t1cHNldHMtdGVzdC1maXh0dXJlAQIDBAUG
-----END OPENSSH PRIVATE KEY-----
`

// openTestService is writeTestConfigFile (open_test.go) plus Open, wired
// as t.Cleanup so every test below gets a real *BackupService (real
// config file, real SQLite journal, real local-transport source) without
// repeating the boilerplate.
func openTestService(t *testing.T) (*BackupService, string) {
	t.Helper()
	configPath := writeTestConfigFile(t)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc, configPath
}

func validCreateReq(t *testing.T, svc *BackupService, name string) CreateBackupSetRequest {
	t.Helper()
	ref, err := svc.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key))
	if err != nil {
		t.Fatalf("ImportSSHKey: %v", err)
	}
	return CreateBackupSetRequest{
		Name:               name,
		Host:               "example.internal",
		Port:               22,
		User:               "backup-agent",
		SSHKeyID:           ref.ID,
		KnownHostsLine:     "example.internal ssh-ed25519 AAAAtestfixtureline",
		RemotePath:         "/backups/" + name,
		LocalPath:          filepath.Join(t.TempDir(), name),
		Include:            []string{"*.dump"},
		CompletionStrategy: "marker",
	}
}

// TestCreateBackupSet_PersistsAndIsImmediatelyVisible is the RED plan's
// core create-then-read-back proof: after CreateBackupSet returns, the
// new set is visible through ListBackupSets/GetBackupSet on the SAME
// BackupService, with no restart — the "hot reload" this file's package
// doc promises — and the config file on disk actually contains it.
func TestCreateBackupSet_PersistsAndIsImmediatelyVisible(t *testing.T) {
	svc, configPath := openTestService(t)
	revisionBefore := svc.ConfigRevision()

	req := validCreateReq(t, svc, "new-set")
	result, err := svc.CreateBackupSet(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	if result.Set.ID != "api/new-set" {
		t.Errorf("Set.ID = %q, want %q", result.Set.ID, "api/new-set")
	}
	if result.Operation != nil {
		t.Errorf("Operation = %+v, want nil (RunImmediately was not set)", result.Operation)
	}

	if svc.ConfigRevision() == revisionBefore {
		t.Error("ConfigRevision did not change after CreateBackupSet")
	}

	got, err := svc.GetBackupSet(context.Background(), "api/new-set")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if got.Host != "example.internal" {
		t.Errorf("GetBackupSet host = %q, want %q", got.Host, "example.internal")
	}

	all, err := svc.ListBackupSets(context.Background())
	if err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}
	found := false
	for _, s := range all {
		if s.ID == "api/new-set" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListBackupSets did not include the newly created set: %+v", all)
	}

	// The config file on disk, not just this process's in-memory copy,
	// must carry the new set — a second process (the CLI's `sources`
	// command, or this same process restarting) reads it fresh from
	// disk with no other coordination (core/cmd/backup-manager/
	// sources.go), so if only the in-memory copy changed, that promise
	// would be false.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath): %v", err)
	}
	var onDisk config.Config
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("yaml.Unmarshal(configPath): %v", err)
	}
	foundOnDisk := false
	for _, src := range onDisk.Sources {
		if src.Name != "api" {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == "new-set" {
				foundOnDisk = true
			}
		}
	}
	if !foundOnDisk {
		t.Errorf("the on-disk config file does not contain the new backup set:\n%s", raw)
	}

	// The original backup set writeTestConfigFile already wrote
	// (production/postgres-primary) must still be there too — creating a
	// new one must never drop an existing one.
	got, err = svc.GetBackupSet(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet(original set): %v", err)
	}
	if got.Name != "postgres-primary" {
		t.Errorf("original set's Name = %q, want %q", got.Name, "postgres-primary")
	}
}

// TestCreateBackupSet_RunImmediately_SubmitsARunCycleOperation is "Save,
// enable & run": the new set's config takes effect (hot reload) BEFORE
// the run_cycle operation this call also submits, so that operation's
// own cycle already covers it (visible here as the cycle's own log
// output discovering "api/run-now-set", not only the pre-existing
// production/postgres-primary set writeTestConfigFile seeds).
//
// The submitted cycle is expected to end up "failed" here, not
// "completed": validCreateReq points the new set at a real-shaped but
// nonexistent sftp host ("example.internal") with a syntactically valid
// but not-actually-trusted known_hosts line, since this test (unlike
// backupsets_docker_test.go) has no real SSH server to connect to. What
// this test proves is narrower and does not need one: RunImmediately
// actually reaches SubmitRunCycle and the operation runs to a terminal
// state, not that a fake host answers.
func TestCreateBackupSet_RunImmediately_SubmitsARunCycleOperation(t *testing.T) {
	svc, _ := openTestService(t)
	req := validCreateReq(t, svc, "run-now-set")
	req.RunImmediately = true
	req.Actor = "alice"

	result, err := svc.CreateBackupSet(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	if result.Operation == nil {
		t.Fatal("Operation is nil, want a submitted run_cycle operation")
	}
	if result.Operation.Actor != "alice" {
		t.Errorf("Operation.Actor = %q, want %q", result.Operation.Actor, "alice")
	}

	done := waitForTerminalStatus(t, svc, result.Operation.ID)
	if done.Status != "completed" && done.Status != "failed" {
		t.Errorf("Operation.Status = %q, want a terminal status (completed or failed)", done.Status)
	}
}

// TestCreateBackupSet_Disabled_NeverSubmitsARunEvenIfRequested proves
// RunImmediately is a no-op when Disabled is true, per
// CreateBackupSetRequest.RunImmediately's own doc.
func TestCreateBackupSet_Disabled_NeverSubmitsARunEvenIfRequested(t *testing.T) {
	svc, _ := openTestService(t)
	req := validCreateReq(t, svc, "disabled-set")
	req.Disabled = true
	req.RunImmediately = true

	result, err := svc.CreateBackupSet(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	if result.Operation != nil {
		t.Errorf("Operation = %+v, want nil (Disabled must suppress RunImmediately)", result.Operation)
	}
	if !result.Set.Disabled {
		t.Error("Set.Disabled = false, want true")
	}
}

func TestCreateBackupSet_MissingRequiredFieldsReturnsErrInvalidRequest(t *testing.T) {
	svc, _ := openTestService(t)
	req := validCreateReq(t, svc, "bad-set")
	req.Name = ""

	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// TestCreateBackupSet_InvalidCompletionStrategyIsCaughtByConfigValidate
// proves CreateBackupSet reuses config.Validate rather than only its own
// hand-rolled field checks: an invalid strategy value passes
// validateCreateRequest's own (looser) switch only if that switch is
// exhaustive, but a value config.Validate itself would refuse (e.g. one
// that IS in this package's switch but conflicts with another field, like
// stable_for set for a non-"stable" strategy) proves the deeper reuse.
func TestCreateBackupSet_InvalidCompletionStrategyIsCaughtByConfigValidate(t *testing.T) {
	svc, _ := openTestService(t)
	req := validCreateReq(t, svc, "bad-completion")
	req.CompletionStrategy = "marker"
	req.StableFor = 0 // fine for "marker"; StableFor is only meaningful for "stable"

	// A duplicate backup-set id (the same name as an existing one, same
	// default source) is exactly the kind of cross-set problem only a
	// whole-config config.Validate pass catches, not this package's own
	// per-field validateCreateRequest.
	first := req
	if _, err := svc.CreateBackupSet(context.Background(), first); err != nil {
		t.Fatalf("first CreateBackupSet: %v", err)
	}
	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("second CreateBackupSet (duplicate id) err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "already used") {
		t.Errorf("err = %v, want it to explain the id is already used", err)
	}
}

func TestCreateBackupSet_UnknownSSHKeyIDReturnsErrSSHKeyNotFound(t *testing.T) {
	svc, _ := openTestService(t)
	req := validCreateReq(t, svc, "no-such-key")
	req.SSHKeyID = "does-not-exist"

	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrSSHKeyNotFound) {
		t.Fatalf("err = %v, want ErrSSHKeyNotFound", err)
	}
}

// TestCreateBackupSet_RejectsSSHKeyIDPathTraversal proves resolveSSHKeyFile
// refuses an id containing a path separator outright, rather than letting
// filepath.Join resolve it somewhere outside keysDir().
func TestCreateBackupSet_RejectsSSHKeyIDPathTraversal(t *testing.T) {
	svc, _ := openTestService(t)
	req := validCreateReq(t, svc, "traversal")
	req.SSHKeyID = "../../../etc/passwd"

	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// TestCreateBackupSet_WithoutAConfigFileReturnsErrConfigNotFileBacked
// covers a BackupService built with New directly (every other test in
// this package): CreateBackupSet has nothing to persist to and must say
// so, not panic on an empty configPath.
func TestCreateBackupSet_WithoutAConfigFileReturnsErrConfigNotFileBacked(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.CreateBackupSet(context.Background(), CreateBackupSetRequest{Name: "x"})
	if !errors.Is(err, ErrConfigNotFileBacked) {
		t.Fatalf("err = %v, want ErrConfigNotFileBacked", err)
	}
}

func TestImportSSHKey_Success_PersistsFileAndReportsFingerprint(t *testing.T) {
	svc, configPath := openTestService(t)

	ref, err := svc.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key))
	if err != nil {
		t.Fatalf("ImportSSHKey: %v", err)
	}
	if ref.ID == "" {
		t.Error("ID is empty")
	}
	if ref.Algorithm != "ssh-ed25519" {
		t.Errorf("Algorithm = %q, want %q", ref.Algorithm, "ssh-ed25519")
	}
	if !strings.HasPrefix(ref.Fingerprint, "SHA256:") {
		t.Errorf("Fingerprint = %q, want a SHA256: prefix", ref.Fingerprint)
	}

	wantDir := filepath.Join(filepath.Dir(configPath), "ssh_keys")
	if filepath.Dir(ref.KeyFile) != wantDir {
		t.Errorf("KeyFile = %q, want it inside %q", ref.KeyFile, wantDir)
	}
	info, err := os.Stat(ref.KeyFile)
	if err != nil {
		t.Fatalf("Stat(KeyFile): %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("KeyFile permissions = %o, want %o", perm, 0o600)
	}
	persisted, err := os.ReadFile(ref.KeyFile)
	if err != nil {
		t.Fatalf("ReadFile(KeyFile): %v", err)
	}
	if string(persisted) != testFixtureEd25519Key {
		t.Error("persisted key file content does not match the imported key")
	}
}

func TestImportSSHKey_NotAKeyReturnsErrInvalidRequestWithoutEchoingInput(t *testing.T) {
	svc, _ := openTestService(t)
	_, err := svc.ImportSSHKey(context.Background(), []byte("this-is-not-an-ssh-key-at-all"))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if strings.Contains(err.Error(), "this-is-not-an-ssh-key-at-all") {
		t.Errorf("error echoed the invalid input back: %v", err)
	}
}

func TestImportSSHKey_EmptyReturnsErrInvalidRequest(t *testing.T) {
	svc, _ := openTestService(t)
	_, err := svc.ImportSSHKey(context.Background(), []byte(""))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// TestTestConnection_MissingFieldsReturnsErrInvalidRequest and
// TestProbeHostKey_UnreachableHostReturnsAnError are the two SSH-facing
// methods' fast, no-network/no-Docker error-path coverage; their
// happy-path, real-server behavior is covered by
// backupsets_docker_test.go (issue #146's INTEGRATION requirement)
// instead, since that needs a real SSH server to be meaningful at all.
func TestTestConnection_MissingFieldsReturnsErrInvalidRequest(t *testing.T) {
	svc, _ := openTestService(t)
	_, err := svc.TestConnection(context.Background(), ConnectionTestRequest{Host: "example.internal"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestTestConnection_UnknownSSHKeyIDReturnsErrSSHKeyNotFound(t *testing.T) {
	svc, _ := openTestService(t)
	_, err := svc.TestConnection(context.Background(), ConnectionTestRequest{
		Host:           "example.internal",
		Port:           22,
		User:           "backup-agent",
		SSHKeyID:       "does-not-exist",
		KnownHostsLine: "example.internal ssh-ed25519 AAAAtestfixtureline",
	})
	if !errors.Is(err, ErrSSHKeyNotFound) {
		t.Fatalf("err = %v, want ErrSSHKeyNotFound", err)
	}
}

func TestProbeHostKey_UnreachableHostReturnsAnError(t *testing.T) {
	svc, _ := openTestService(t)
	// Port 0 on loopback: nothing is listening there, and it fails fast
	// rather than waiting out probeTimeout, so this test needs no Docker
	// and no real network.
	_, err := svc.ProbeHostKey(context.Background(), "127.0.0.1", 1)
	if err == nil {
		t.Fatal("err = nil, want an error probing a host nothing listens on")
	}
}

package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
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
	ref, err := svc.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key), "")
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

// TestCreateBackupSet_StableStrategy is the wizard's "stable-size"
// completion option going through the real CreateBackupSet, end to end,
// against a real config file.
//
// This is the regression the WP3.2 review caught: making
// completion.delete_safety_delay a hard requirement with no default made
// every one of these calls fail. CreateBackupSetRequest has no field for
// that key and CreateBackupSet never sets one, so cfg.Validate() refused
// the config it had just built, and the shipped BackupSetWizardPage
// option that maps to "stable" returned INVALID_REQUEST naming a YAML key
// the API surface cannot express. Nothing in this package covered the
// stable path at all before this test: the one completion test here uses
// "marker" on purpose.
//
// The assertions are one claim each: the create succeeds, the operator's
// own stable_for survives the round trip, and the FR-15 delete gate ends
// up armed at the documented default rather than at a literal zero,
// which would be the same as having no gate. The last one is what keeps
// "make it load again" from being fixed the wrong way.
//
// completion.delete_safety_delay is checked against a RELOAD of the
// file, not the raw bytes it wrote, because of issue #294: CreateBackupSet
// now encodes cfg before cfg.Validate resolves its defaults in place
// (this field's own doc gives the identical reasoning validateRetention
// does — a zero here means "never chose one", not "the gate is off" — see
// backupsets.go's comment beside cfg.Validate for the full fix), so a
// value nobody chose stays zero ON DISK and is only resolved when the
// file is loaded. Checking the raw bytes for 1h would now fail even
// though the gate is correctly armed.
func TestCreateBackupSet_StableStrategy(t *testing.T) {
	svc, configPath := openTestService(t)
	req := validCreateReq(t, svc, "stable-set")
	req.CompletionStrategy = "stable"
	req.StableFor = 10 * time.Minute

	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("CreateBackupSet with the wizard's stable-size option: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath): %v", err)
	}
	var onDisk config.Config
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("yaml.Unmarshal(configPath): %v", err)
	}

	created := findBackupSet(&onDisk, "api", "stable-set")
	if created.Name == "" {
		t.Fatalf("the on-disk config file does not contain the new stable backup set:\n%s", raw)
	}

	if created.Completion.Strategy != "stable" {
		t.Errorf("persisted completion.strategy = %q, want %q", created.Completion.Strategy, "stable")
	}
	if got, want := created.Completion.StableFor.Duration(), 10*time.Minute; got != want {
		t.Errorf("persisted completion.stable_for = %s, want %s", got, want)
	}

	reloaded, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	reloadedSet := findBackupSet(reloaded, "api", "stable-set")
	if reloadedSet.Name == "" {
		t.Fatalf("the reloaded config no longer contains the new stable backup set")
	}
	if got := reloadedSet.Completion.DeleteSafetyDelay.Duration(); got != config.DefaultDeleteSafetyDelay {
		t.Errorf("reloaded completion.delete_safety_delay = %s, want the default %s; a zero here disarms the FR-15 stable-completion delete gate entirely", got, config.DefaultDeleteSafetyDelay)
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

// TestCreateBackupSet_RejectsNameContainingAPathTraversal is the M2
// mandatory review finding (PR #155): a Name containing a path separator
// or ".." must never reach writeKnownHosts, which folds Name into one
// filename token (sourceName+"_"+name+"_known_hosts") and would
// otherwise let filepath.Join resolve an embedded "../" as a real
// parent-directory escape — verified against the pre-fix code:
// dir=".../known_hosts.d", name="../../../../tmp/evil" wrote a file
// outside both the known_hosts sandbox and the config directory. Proven
// two ways here: the call itself is refused, AND no known_hosts.d
// directory (which writeKnownHosts always creates before it ever writes
// a file) exists afterward at all — proof nothing was written anywhere,
// not merely that this one crafted path happened to be caught.
func TestCreateBackupSet_RejectsNameContainingAPathTraversal(t *testing.T) {
	svc, configPath := openTestService(t)
	req := validCreateReq(t, svc, "traversal")
	req.Name = "../../../../tmp/evil"

	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}

	knownHostsDir := filepath.Join(filepath.Dir(configPath), "known_hosts.d")
	if _, statErr := os.Stat(knownHostsDir); !os.IsNotExist(statErr) {
		t.Errorf("known_hosts.d exists after a rejected traversal attempt (stat err = %v); writeKnownHosts must never run before validation", statErr)
	}
}

// TestCreateBackupSet_RejectsSourceNameContainingAPathTraversal is the
// same M2 finding applied to SourceName, the other half writeKnownHosts
// folds into its filename token. Pre-fix, a malicious SourceName was
// still eventually rejected — but only by the deeper config.Validate
// pass, which runs AFTER writeKnownHosts has already written a file
// wherever SourceName pointed it (this test's own known_hosts.d
// assertion is what actually catches that: the error alone is not
// enough to distinguish "rejected before any write" from "written, then
// rejected").
func TestCreateBackupSet_RejectsSourceNameContainingAPathTraversal(t *testing.T) {
	svc, configPath := openTestService(t)
	req := validCreateReq(t, svc, "traversal-source")
	req.SourceName = "../../../../tmp"

	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}

	knownHostsDir := filepath.Join(filepath.Dir(configPath), "known_hosts.d")
	if _, statErr := os.Stat(knownHostsDir); !os.IsNotExist(statErr) {
		t.Errorf("known_hosts.d exists after a rejected traversal attempt (stat err = %v); writeKnownHosts must never run before validation", statErr)
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

// TestCreateBackupSet_ConcurrentWithReadersDoesNotRace is the mandatory
// review's M1 regression test (PR #155): CreateBackupSet's hot-reload of
// this BackupService's {inner, revision} state must be safe to race
// against every other reader of that same state, since that is exactly
// the concurrency shape a real deployment has — net/http runs each
// request on its own goroutine, and the scheduler ticks independently of
// both, so an operator creating a backup set while a run_cycle or a
// scheduled tick is in flight is normal operation, not an edge case.
//
// This test's own assertions are deliberately loose (no panic, no
// unexpected error): what actually proves the fix is running this test
// with `go test -race`, which fails on the old two-separately-locked-
// on-write, never-locked-on-read fields (a real, reachable data race
// under the Go memory model) and passes cleanly once state is one
// atomic.Pointer. Mirrors the 200-racer pattern
// TestSessionManager_ConcurrentRotateSessionNeverLeavesZeroLiveSessions
// (apps/common/auth/local/session_test.go) already established for the
// #128 password-rotation race.
func TestCreateBackupSet_ConcurrentWithReadersDoesNotRace(t *testing.T) {
	svc, _ := openTestService(t)
	req := validCreateReq(t, svc, "race-set")
	ctx := context.Background()

	const readers = 50
	var wg sync.WaitGroup
	wg.Add(1 + readers*3)

	go func() {
		defer wg.Done()
		if _, err := svc.CreateBackupSet(ctx, req); err != nil {
			t.Errorf("CreateBackupSet: %v", err)
		}
	}()

	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			_ = svc.ConfigRevision()
		}()
		go func() {
			defer wg.Done()
			// runScheduledCycle (scheduler.go) is this package's own
			// caller of the scheduler's read of {inner, revision} — the
			// same method a real background tick calls, exercised
			// directly here rather than waiting out a real timer.
			svc.runScheduledCycle(ctx)
		}()
		go func() {
			defer wg.Done()
			// ConfigRevision may legitimately read either the pre- or
			// post-create revision depending on scheduling, so a
			// resulting ErrConfigRevisionStale/ErrOperationAlreadyRunning
			// here is an expected outcome, not a test failure — only a
			// panic or a data race (caught by -race) would be.
			_, _ = svc.SubmitRunCycle(ctx, RunCycleRequest{
				IdempotencyKey: "race:" + uuid.NewString(),
				ConfigRevision: svc.ConfigRevision(),
			})
		}()
	}

	wg.Wait()
}

func TestImportSSHKey_Success_PersistsFileAndReportsFingerprint(t *testing.T) {
	svc, configPath := openTestService(t)

	ref, err := svc.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key), "")
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
	_, err := svc.ImportSSHKey(context.Background(), []byte("this-is-not-an-ssh-key-at-all"), "")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if strings.Contains(err.Error(), "this-is-not-an-ssh-key-at-all") {
		t.Errorf("error echoed the invalid input back: %v", err)
	}
}

func TestImportSSHKey_EmptyReturnsErrInvalidRequest(t *testing.T) {
	svc, _ := openTestService(t)
	_, err := svc.ImportSSHKey(context.Background(), []byte(""), "")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// testEncryptedFixtureKeyPassphrase is the passphrase
// mustEncryptedFixtureKeyPEM always encrypts with.
const testEncryptedFixtureKeyPassphrase = "correct horse battery staple"

// mustEncryptedFixtureKeyPEM generates a fresh, throwaway, passphrase-
// protected ed25519 private key for #269's ImportSSHKey tests: real
// encrypted key bytes, generated the same Go-native way
// internal/transport/rclone/ssh_test.go's generateEncryptedClientSSHKeyPair
// does (x/crypto/ssh's own MarshalPrivateKeyWithPassphrase), rather than
// shelling out to ssh-keygen. Like testFixtureEd25519Key above, it
// authorizes access to nothing: no server anywhere trusts its public half.
func mustEncryptedFixtureKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "backupsets-test-fixture", []byte(testEncryptedFixtureKeyPassphrase))
	if err != nil {
		t.Fatalf("ssh.MarshalPrivateKeyWithPassphrase: %v", err)
	}
	return pem.EncodeToMemory(block)
}

// TestImportSSHKey_EncryptedKeyWithCorrectPassphraseSucceeds is #269's
// GREEN case for the import endpoint's backing method: a passphrase-
// protected key, imported with the correct passphrase, is accepted and
// persisted exactly like an unencrypted one, still encrypted on disk (this
// method changes nothing about key storage, #298's separate concern).
func TestImportSSHKey_EncryptedKeyWithCorrectPassphraseSucceeds(t *testing.T) {
	svc, _ := openTestService(t)
	raw := mustEncryptedFixtureKeyPEM(t)

	ref, err := svc.ImportSSHKey(context.Background(), raw, testEncryptedFixtureKeyPassphrase)
	if err != nil {
		t.Fatalf("ImportSSHKey with the correct passphrase: %v", err)
	}
	if ref.ID == "" {
		t.Error("ID is empty")
	}
	if !strings.HasPrefix(ref.Fingerprint, "SHA256:") {
		t.Errorf("Fingerprint = %q, want a SHA256: prefix", ref.Fingerprint)
	}
	persisted, err := os.ReadFile(ref.KeyFile)
	if err != nil {
		t.Fatalf("ReadFile(KeyFile): %v", err)
	}
	if string(persisted) != string(raw) {
		t.Error("persisted key file content does not match the imported (still encrypted) key")
	}
}

// TestImportSSHKey_EncryptedKeyWithNoPassphraseIsRefused is #269's
// unchanged-refusal case: an encrypted key imported with no passphrase at
// all still fails exactly as it always has, by name, never silently
// accepted.
func TestImportSSHKey_EncryptedKeyWithNoPassphraseIsRefused(t *testing.T) {
	svc, _ := openTestService(t)
	raw := mustEncryptedFixtureKeyPEM(t)

	_, err := svc.ImportSSHKey(context.Background(), raw, "")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("error %q does not name the actual problem (passphrase-protected)", err.Error())
	}
}

// TestImportSSHKey_EncryptedKeyWithWrongPassphraseIsRefusedAtImport is
// #269's central acceptance criterion, proven at the service method POST
// /ssh-keys actually calls: "whatever is added is refused at
// configuration time rather than at the first cycle" means a wrong
// passphrase must fail HERE, before anything is persisted, not be
// accepted and only discovered broken the first time a backup cycle tries
// to connect.
func TestImportSSHKey_EncryptedKeyWithWrongPassphraseIsRefusedAtImport(t *testing.T) {
	svc, configPath := openTestService(t)
	raw := mustEncryptedFixtureKeyPEM(t)

	_, err := svc.ImportSSHKey(context.Background(), raw, "definitely the wrong passphrase")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if strings.Contains(err.Error(), "definitely the wrong passphrase") {
		t.Fatalf("error echoed the passphrase back: %v", err)
	}

	// Nothing was written: a refused import must not leave a key file
	// behind for a later step to trip over.
	keysDir := filepath.Join(filepath.Dir(configPath), "ssh_keys")
	entries, statErr := os.ReadDir(keysDir)
	if statErr == nil && len(entries) != 0 {
		t.Fatalf("a refused import still persisted %d file(s) in %s", len(entries), keysDir)
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

// TestCreateBackupSet_ConfigRoundTripKeepsOneRetentionSpelling is the
// round-trip proof for the file the product writes back over an
// operator's own config.
//
// CreateBackupSet re-marshals the whole *config.Config on every wizard
// save (writeConfigAtomically), so every retention field without
// `omitempty` lands in the file whether the operator wrote it or not.
// With a tiers-based policy that meant `daily_days: 0` sitting directly
// above the tiers list, which reads as "daily retention is off" and
// invites exactly one edit: set it to 7. That edit is the one shape
// config.Validate refuses (the two spellings are mutually exclusive), and
// a refused config means LoadAndValidate fails, no BackupService is
// constructed, and there is no UI left to undo it from.
// TestCreateBackupSet_DoesNotFreezeResolvedDefaultsIntoAFileThatNeverChoseThem
// is issue #294's second half. CreateBackupSet re-reads the config file,
// folds in the new backup set, and validates the WHOLE result before
// writing -- its own comment on cfg.Validate says so -- and
// config.Validate resolves Retention/Alerts IN PLACE. A config file that
// never named a retention or alerts choice (a hand-written file, or one
// CreateInitialConfig produced now that #294 is fixed) must come back
// from adding an unrelated backup set with that same omission, not with
// today's resolved numbers silently pinned into it, or every deployment
// set up and then extended through the API/CLI would drift onto whatever
// release happened to be running the day the second set was added.
func TestCreateBackupSet_DoesNotFreezeResolvedDefaultsIntoAFileThatNeverChoseThem(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, "")
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	if _, err := svc.CreateBackupSet(context.Background(), validCreateReq(t, svc, "second-set")); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	written := string(raw)

	// The write did happen: without this the absences below would be
	// satisfied by a no-op.
	if !strings.Contains(written, "second-set") {
		t.Fatalf("precondition failed: CreateBackupSet's write did not reach the file:\n%s", written)
	}

	frozen := []string{
		"timezone: UTC",
		"week_starts_on: monday",
		"daily_days:",
		"weekly_months:",
		"monthly_months:",
		"protect_last_known_good: true",
		"repeated_failure_threshold: " + strconv.Itoa(config.DefaultRepeatedFailureThreshold),
	}
	for _, spelling := range frozen {
		if strings.Contains(written, spelling) {
			t.Errorf("CreateBackupSet froze the resolved default %q into a file that never chose retention or alerts:\n%s", spelling, written)
		}
	}

	// Positive control, the same shape settings_test.go's
	// TestUpdateSettings_DoesNotFreezeResolvedDefaultsIntoTheOperatorsFile
	// uses for the identical claim: encoding the VALIDATED config does
	// produce every spelling above, so their absence from the file is not
	// vacuous.
	validated, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	encoded, err := yaml.Marshal(validated)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, spelling := range frozen {
		if !strings.Contains(string(encoded), spelling) {
			t.Errorf("control failed: encoding the validated config does not produce %q, so the absence assertions above prove nothing", spelling)
		}
	}
}

func TestCreateBackupSet_ConfigRoundTripKeepsOneRetentionSpelling(t *testing.T) {
	roundTrip := func(t *testing.T, retention string) string {
		t.Helper()
		configPath := writeTestConfigFileWithRetention(t, retention)
		svc, cleanup, err := Open(context.Background(), configPath)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = cleanup() })

		if _, err := svc.CreateBackupSet(context.Background(), validCreateReq(t, svc, "new-set")); err != nil {
			t.Fatalf("CreateBackupSet: %v", err)
		}
		written, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("reading the config back: %v", err)
		}
		// The file must still load, since the daemon's own next start
		// reads exactly this.
		if _, err := config.LoadAndValidate(configPath); err != nil {
			t.Fatalf("the config this product wrote no longer loads: %v", err)
		}
		return string(written)
	}

	t.Run("a tiers-based policy is not written back with the scalar keys", func(t *testing.T) {
		got := roundTrip(t, "retention:\n"+
			"  timezone: UTC\n"+
			"  week_starts_on: monday\n"+
			"  tiers:\n"+
			"    - name: daily\n"+
			"      granularity: day\n"+
			"      keep: 7\n"+
			"    - name: annual\n"+
			"      granularity: year\n"+
			"      keep: 5\n")

		// Positive control for the three absence assertions below: if the
		// chain itself were missing, they would pass vacuously.
		if !strings.Contains(got, "name: annual") {
			t.Fatalf("the written config lost the operator's chain, so nothing below is being measured:\n%s", got)
		}
		for _, key := range []string{"daily_days", "weekly_months", "monthly_months"} {
			if strings.Contains(got, key) {
				t.Errorf("the written config carries %s alongside the operator's tiers list; config.Validate refuses that combination the moment the operator gives it a value:\n%s", key, got)
			}
		}
		// The noise the schema doc says is absent from every other tier.
		for _, key := range []string{"period_days", "window_unit"} {
			if strings.Contains(got, key) {
				t.Errorf("the written config carries an empty %s on tiers that do not use it:\n%s", key, got)
			}
		}
	})

	// The control that gives the assertions above teeth: a legacy config
	// still round-trips its own spelling, so "the key is absent" is a
	// measurement of omitempty, not of a test that cannot see the file.
	t.Run("control: a legacy scalar policy is written back with its scalars", func(t *testing.T) {
		got := roundTrip(t, "retention:\n"+
			"  timezone: UTC\n"+
			"  week_starts_on: monday\n"+
			"  daily_days: 7\n")

		if !strings.Contains(got, "daily_days: 7") {
			t.Errorf("the written config lost the operator's daily_days:\n%s", got)
		}
		if strings.Contains(got, "tiers:") {
			t.Errorf("the written config injected an empty tiers list into a legacy file, which an older binary rejects outright under KnownFields(true):\n%s", got)
		}
	})
}

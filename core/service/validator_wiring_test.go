package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is issue #162's wiring proof: the registered-validator
// catalog (validator.go) selected by id through the same public entry
// points apps/common/webhost actually calls, persisted as an id rather
// than a resolved executable path, resolved back to a config.Command at
// load time, and blocking remote deletion end to end.
//
// Every test here goes through Open or CreateBackupSet. None of them
// hands a hand-built config.Validation.Command to New: that is the
// distinction issue #162 draws between testing the feature and testing a
// fixture, and validator_integration_test.go's own two end-to-end tests
// are the reason the distinction is worth drawing -- they proved
// internal/lifecycle refuses a failing validator, which was never in
// doubt, using a Command this package's API layer had no way to ask for.

// idempotencyCounter mints a distinct RunCycleRequest.IdempotencyKey for
// every cycle these tests submit; see submitOneCycle.
var idempotencyCounter atomic.Int64

// writeValidatorConfigFile writes a complete, loadable config file whose
// single backup set selects validatorID through the config's own
// validation.validator_id key, over a local-transport "remote" directory
// seeded with payload. It returns the config path, the remote directory
// and the remote artifact's path.
func writeValidatorConfigFile(t *testing.T, validatorID string, payload []byte) (configPath, remoteDir, remoteArtifact string) {
	t.Helper()
	dir := t.TempDir()
	remoteDir = filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	remoteArtifact = filepath.Join(remoteDir, "backup.dump")
	if err := os.WriteFile(remoteArtifact, payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath = filepath.Join(dir, "config.yaml")
	validationBlock := ""
	if validatorID != "" {
		validationBlock = "" +
			"        validation:\n" +
			"          validator_id: " + validatorID + "\n"
	}
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		validationBlock +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath, remoteDir, remoteArtifact
}

// runOneCycleThenClose submits one run_cycle through the service's real
// public entry point, waits for it, then closes the service so the
// journal can be reopened for reading. It returns the journal path.
func runOneCycleThenClose(t *testing.T, svc *BackupService, cleanup func() error, dbPath string) []state.Record {
	t.Helper()
	return runOneCycleThenCloseFor(t, svc, cleanup, dbPath, "production", "postgres-primary")
}

// runOneCycleThenCloseFor is runOneCycleThenClose for a backup set this
// package's own CreateBackupSet named, rather than the one
// writeValidatorConfigFile hard-codes.
func runOneCycleThenCloseFor(t *testing.T, svc *BackupService, cleanup func() error, dbPath, source, set string) []state.Record {
	t.Helper()
	runOneCycle(t, svc)
	if err := cleanup(); err != nil {
		t.Fatalf("closing the service: %v", err)
	}
	return readRecords(t, dbPath, source, set)
}

// runOneCycle submits one run_cycle through the service's real public
// entry point and waits for it to reach a terminal status, which it
// requires to be "completed".
func runOneCycle(t *testing.T, svc *BackupService) {
	t.Helper()
	final := submitOneCycle(t, svc)
	if final.Status != "completed" {
		t.Fatalf("operation status = %q, want completed (Error = %q)", final.Status, final.Error)
	}
}

// submitOneCycle submits one run_cycle and returns its terminal
// operation, whatever that turned out to be: the tests that assert a
// cycle is REFUSED need the failed one.
func submitOneCycle(t *testing.T, svc *BackupService) Operation {
	t.Helper()
	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		// A fresh key per submission, because a test that restarts a
		// service submits its second cycle against the same journal the
		// first one wrote to: reusing the key would replay the first
		// operation's stored result and never run a cycle at all.
		IdempotencyKey: fmt.Sprintf("idem-validator-wiring-%d", idempotencyCounter.Add(1)),
		Actor:          "test",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	return waitForTerminalStatus(t, svc, op.ID)
}

// readRecords reopens the journal at dbPath (the service holding it must
// be closed first) and lists one backup set's artifacts.
func readRecords(t *testing.T, dbPath, source, set string) []state.Record {
	t.Helper()
	journal, err := state.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopening the journal: %v", err)
	}
	defer func() { _ = journal.Close() }()

	setID, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	records, err := journal.ListByBackupSet(context.Background(), setID)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	return records
}

// TestOpen_RequiredValidatorFailureBlocksRemoteDeletionThroughTheWiredPath
// is issue #99's first acceptance criterion, ticked through the wiring
// rather than around it: the backup set names its validator by id in
// config.yaml only, Open is what turns that id into a runnable command,
// and an artifact the validator rejects is quarantined with the remote
// source still in place.
func TestOpen_RequiredValidatorFailureBlocksRemoteDeletionThroughTheWiredPath(t *testing.T) {
	configPath, _, remoteArtifact := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes, no trailer"))

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	records := runOneCycleThenClose(t, svc, cleanup, dbPath)
	if len(records) != 1 {
		t.Fatalf("ListByBackupSet returned %d records, want 1", len(records))
	}
	if records[0].State != string(lifecycle.Quarantined) {
		t.Fatalf("artifact state = %q, want %s", records[0].State, lifecycle.Quarantined)
	}
	if _, err := os.Stat(remoteArtifact); err != nil {
		t.Fatalf("the remote artifact is gone (%v): a required validator's failure must prevent remote deletion", err)
	}
}

// TestOpen_RequiredValidatorSuccessStillAllowsRemoteDeletion is the
// positive control the refusal above needs: without it, a wiring bug
// that quarantined every artifact regardless of the validator's verdict
// would pass that test for entirely the wrong reason.
func TestOpen_RequiredValidatorSuccessStillAllowsRemoteDeletion(t *testing.T) {
	configPath, _, remoteArtifact := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes\n--RCLONE-MANAGER-BACKUP-COMPLETE--\n"))

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	records := runOneCycleThenClose(t, svc, cleanup, dbPath)
	if len(records) != 1 {
		t.Fatalf("ListByBackupSet returned %d records, want 1", len(records))
	}
	if records[0].State != string(lifecycle.Complete) {
		t.Fatalf("artifact state = %q, want %s", records[0].State, lifecycle.Complete)
	}
	if _, err := os.Stat(remoteArtifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the remote artifact is still there (err = %v): an accepted artifact must still reach remote deletion", err)
	}
}

// TestOpen_UnregisteredValidatorIDInTheConfigFileFailsStartup is the
// fail-loud half of load-time resolution. A typo in validator_id must
// stop the process coming up, not silently run every backup set with no
// application validation at all while the operator believes one is
// configured.
func TestOpen_UnregisteredValidatorIDInTheConfigFileFailsStartup(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t, "trailer-marker-typo", []byte("payload"))

	svc, cleanup, err := Open(context.Background(), configPath)
	if err == nil {
		if cleanup != nil {
			_ = cleanup()
		}
		t.Fatalf("Open succeeded with an unregistered validator_id; want an error (svc = %v)", svc)
	}
	if !errors.Is(err, errUnregisteredValidator) {
		t.Fatalf("Open error = %v, want one wrapping errUnregisteredValidator", err)
	}
}

// TestClose_LeavesTheValidatorScriptsForAProcessStillUsingThem is issue
// #164's review finding M1. The scripts used to live in a per-process
// os.MkdirTemp, where removing them on shutdown was the only thing
// stopping one directory leaking per process start. They now live in one
// fixed directory beside the state database, shared by every process that
// opens it -- the journal lock is a SHARED one, and startup.go names a
// container restart racing an old process's shutdown against a new one's
// start as a supported case -- so a Close that removed the directory
// would be deleting the scripts a running successor already resolved
// every one of its backup sets against.
//
// Two services over one state directory is exactly that race, without
// needing two processes to reproduce it: the second one is still running
// when the first one closes.
func TestClose_LeavesTheValidatorScriptsForAProcessStillUsingThem(t *testing.T) {
	configPath, _, remoteArtifact := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes\n--RCLONE-MANAGER-BACKUP-COMPLETE--\n"))

	first, closeFirst, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	_ = first
	second, closeSecond, err := Open(context.Background(), configPath)
	if err != nil {
		_ = closeFirst()
		t.Fatalf("Open (second): %v", err)
	}

	script := filepath.Join(filepath.Dir(configPath), "validators", "trailer-marker.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the validator script was not materialized beside the state database: %v", err)
	}

	if err := closeFirst(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the first service's Close deleted a script the second service is still using: %v", err)
	}

	// The point of keeping it is that the surviving service can still run,
	// so prove that rather than only that a file exists: an accepted
	// artifact still reaches COMPLETE and its remote source is deleted.
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")
	records := runOneCycleThenClose(t, second, closeSecond, dbPath)
	if len(records) != 1 || records[0].State != string(lifecycle.Complete) {
		t.Fatalf("records = %+v, want one COMPLETE artifact: the surviving service was wedged by the other one's shutdown", records)
	}
	if _, err := os.Stat(remoteArtifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the remote artifact is still there (err = %v): the surviving service never completed a cycle", err)
	}
}

// TestOpen_AfterACleanCloseResolvesTheValidatorAgain is the other half of
// M1's composition, and the restart every deployment that uses a
// validator performs: nothing opened the same state directory twice with
// a validator configured, which is precisely where a shutdown that
// removed shared state, or a load path that assumed a fresh directory,
// would show up.
func TestOpen_AfterACleanCloseResolvesTheValidatorAgain(t *testing.T) {
	configPath, remoteDir, _ := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("first payload\n--RCLONE-MANAGER-BACKUP-COMPLETE--\n"))
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	first, closeFirst, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	runOneCycle(t, first)
	if err := closeFirst(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	// A second artifact, so the restarted process has real work to do
	// rather than only re-reading what the first one already finished.
	second := filepath.Join(remoteDir, "second.dump")
	if err := os.WriteFile(second, []byte("second payload\n--RCLONE-MANAGER-BACKUP-COMPLETE--\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	restarted, closeRestarted, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open (restarted): %v", err)
	}
	records := runOneCycleThenClose(t, restarted, closeRestarted, dbPath)
	if len(records) != 2 {
		t.Fatalf("ListByBackupSet returned %d records, want 2: %+v", len(records), records)
	}
	for _, rec := range records {
		if rec.State != string(lifecycle.Complete) {
			t.Errorf("artifact %s is %s, want %s after a restart", rec.Artifact, rec.State, lifecycle.Complete)
		}
	}
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the second artifact's remote source is still there (err = %v): the restarted process could not run its validator", err)
	}
}

// TestCreateBackupSet_PersistsTheValidatorIDNeverAnExecutablePath is the
// config.yaml half of issue #162: the id is what is written, and the
// resolved, process-specific executable path is what is not. This is the
// defect that would otherwise quarantine every artifact in a set after
// the first restart, since the recorded path would name a temp directory
// that no longer existed.
func TestCreateBackupSet_PersistsTheValidatorIDNeverAnExecutablePath(t *testing.T) {
	svc, configPath := openTestService(t)

	req := validCreateReq(t, svc, "validated-set")
	req.ValidatorID = ValidatorTrailerMarker
	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath): %v", err)
	}
	var onDisk config.Config
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("yaml.Unmarshal(configPath): %v", err)
	}
	created := findBackupSet(&onDisk, "api", "validated-set")
	if created.Name == "" {
		t.Fatalf("the new backup set is not in the persisted config:\n%s", raw)
	}
	if created.Validation.ValidatorID != string(ValidatorTrailerMarker) {
		t.Errorf("persisted validation.validator_id = %q, want %q", created.Validation.ValidatorID, ValidatorTrailerMarker)
	}
	if created.Validation.Command != nil {
		t.Errorf("persisted validation.command = %+v, want nil: a resolved executable path must never reach config.yaml", created.Validation.Command)
	}
	if strings.Contains(string(raw), "trailer-marker.sh") {
		t.Errorf("the persisted config names the materialized script:\n%s", raw)
	}

	// The read-back surface carries the id too, so a UI can show which
	// validator a set already uses (and an edit path has something to
	// pre-select).
	got, err := svc.GetBackupSet(context.Background(), "api/validated-set")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if got.ValidatorID != ValidatorTrailerMarker {
		t.Errorf("GetBackupSet ValidatorID = %q, want %q", got.ValidatorID, ValidatorTrailerMarker)
	}
}

// TestCreateBackupSet_HotReloadResolvesTheValidatorToARunnableCommand
// proves the other side of the same call: what is persisted is an id,
// but what the running process ends up holding is a real command it can
// execute, materialized under the state directory rather than TMPDIR.
func TestCreateBackupSet_HotReloadResolvesTheValidatorToARunnableCommand(t *testing.T) {
	svc, configPath := openTestService(t)

	req := validCreateReq(t, svc, "validated-set")
	req.ValidatorID = ValidatorTrailerMarker
	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}

	live := findBackupSet(svc.state.Load().inner.Config, "api", "validated-set")
	if live.Validation.Command == nil {
		t.Fatal("the hot-reloaded config has no resolved validator command")
	}
	stateDir := filepath.Dir(configPath)
	wantPrefix := filepath.Join(stateDir, "validators") + string(filepath.Separator)
	if !strings.HasPrefix(live.Validation.Command.Executable, wantPrefix) {
		t.Errorf("resolved Executable = %q, want one under %q", live.Validation.Command.Executable, wantPrefix)
	}
	info, err := os.Stat(live.Validation.Command.Executable)
	if err != nil {
		t.Fatalf("the resolved validator script is not on disk: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("the resolved validator script is not executable: mode %v", info.Mode())
	}
	if live.Validation.Command.Timeout.Duration() <= 0 {
		t.Errorf("resolved Timeout = %s, want positive", live.Validation.Command.Timeout)
	}
}

// TestCreateBackupSet_UnregisteredValidatorIDIsRefusedAndPersistsNothing
// keeps §26 Step 5 true from the request side: an id outside the catalog
// -- including a raw path, which this package refuses structurally rather
// than by sniffing for a path shape -- is an invalid request, and the
// refusal happens before anything is written.
func TestCreateBackupSet_UnregisteredValidatorIDIsRefusedAndPersistsNothing(t *testing.T) {
	for _, id := range []ValidatorID{"/bin/rm", "../../etc/passwd", "trailer-marker.sh", "TRAILER_MARKER", "not-a-real-validator"} {
		t.Run(string(id), func(t *testing.T) {
			svc, configPath := openTestService(t)
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			req := validCreateReq(t, svc, "rejected-set")
			req.ValidatorID = id
			if _, err := svc.CreateBackupSet(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("CreateBackupSet(validator_id=%q) error = %v, want ErrInvalidRequest", id, err)
			}

			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if string(after) != string(before) {
				t.Errorf("the config file changed despite the refusal:\n%s", after)
			}
		})
	}
}

// TestCreateBackupSet_WithoutAValidatorLeavesTheBlockEmpty is the
// no-validator control for the two persistence tests above: the default
// path must be unchanged by this issue, with no validator_id and no
// command written at all.
func TestCreateBackupSet_WithoutAValidatorLeavesTheBlockEmpty(t *testing.T) {
	svc, configPath := openTestService(t)

	req := validCreateReq(t, svc, "plain-set")
	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var onDisk config.Config
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	created := findBackupSet(&onDisk, "api", "plain-set")
	if created.Validation.ValidatorID != "" || created.Validation.Command != nil {
		t.Errorf("validation = %+v, want the empty block a request with no validator has always produced", created.Validation)
	}

	live := findBackupSet(svc.state.Load().inner.Config, "api", "plain-set")
	if live.Validation.Command != nil {
		t.Errorf("the hot-reloaded config invented a validator command: %+v", live.Validation.Command)
	}
}

// tamperedScript is what a validator script gets overwritten with in the
// tests below: a syntactically fine, executable script that passes
// everything it is handed. That is the fail-open direction, and it is the
// one that matters, because a passing validator is exactly what
// authorizes deleting the remote source.
const tamperedScript = "#!/bin/sh\nexit 0\n"

// TestRunCycle_RewritesATamperedValidatorScriptBeforeUsingIt is issue
// #164's review finding M2, the half that is fail-OPEN.
//
// Resolution happens at load and create time, and internal/lifecycle
// execs the config.Command it was handed then, so before this fix nothing
// on the run path ever looked at the script again. A daemon that is not
// creating backup sets could therefore run for weeks against a validator
// script that had been replaced with one that exits 0, passing every
// artifact and deleting every remote source behind it: an untrailered
// artifact reached COMPLETE with its remote gone, which is the exact
// outcome FR-13 exists to prevent.
func TestRunCycle_RewritesATamperedValidatorScriptBeforeUsingIt(t *testing.T) {
	configPath, _, remoteArtifact := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes, no trailer"))
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	script := filepath.Join(filepath.Dir(configPath), "validators", "trailer-marker.sh")
	embedded, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading the materialized script: %v", err)
	}
	if err := os.WriteFile(script, []byte(tamperedScript), 0o755); err != nil {
		t.Fatalf("tampering with the materialized script: %v", err)
	}

	records := runOneCycleThenClose(t, svc, cleanup, dbPath)
	if len(records) != 1 {
		t.Fatalf("ListByBackupSet returned %d records, want 1", len(records))
	}
	if records[0].State != string(lifecycle.Quarantined) {
		t.Errorf("artifact state = %q, want %s: a tampered validator script passed an artifact it should have rejected", records[0].State, lifecycle.Quarantined)
	}
	if _, err := os.Stat(remoteArtifact); err != nil {
		t.Errorf("the remote artifact is gone (%v): a tampered validator script authorized deleting a source it never checked", err)
	}
	if got, err := os.ReadFile(script); err != nil || string(got) != string(embedded) {
		t.Errorf("the script on disk was not rewritten from the embedded copy (err = %v)", err)
	}
}

// TestRunCycle_TamperedScriptStillLetsAGoodArtifactComplete is the
// positive control the refusal above needs. Both of that test's
// assertions are negative -- the artifact must NOT complete, the remote
// must NOT be deleted -- and a cycle that never ran at all, or a service
// that refused every artifact for some unrelated reason, would satisfy
// both. This is the identical setup with the identical tampering, over a
// payload that does carry the completion trailer: it has to reach
// COMPLETE with the remote deleted, which is only possible if the rewrite
// happened, the real validator ran, and the deletion path is reachable
// from this fixture.
func TestRunCycle_TamperedScriptStillLetsAGoodArtifactComplete(t *testing.T) {
	configPath, _, remoteArtifact := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes\n--RCLONE-MANAGER-BACKUP-COMPLETE--\n"))
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	script := filepath.Join(filepath.Dir(configPath), "validators", "trailer-marker.sh")
	if err := os.WriteFile(script, []byte(tamperedScript), 0o755); err != nil {
		t.Fatalf("tampering with the materialized script: %v", err)
	}

	records := runOneCycleThenClose(t, svc, cleanup, dbPath)
	if len(records) != 1 || records[0].State != string(lifecycle.Complete) {
		t.Fatalf("records = %+v, want one COMPLETE artifact", records)
	}
	if _, err := os.Stat(remoteArtifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the remote artifact is still there (err = %v)", err)
	}
}

// TestRunCycle_RewritesAReapedValidatorScriptBeforeUsingIt is M2's
// fail-closed direction: a script removed after startup (a sweeper, an
// operator tidying up, another process that used to delete the directory
// on its way out) wedged the whole backup set. Every artifact failed at
// exec, was classified Failed, re-entered Discovered and looped, for the
// life of the process, with nothing on the run path ever putting the
// script back.
func TestRunCycle_RewritesAReapedValidatorScriptBeforeUsingIt(t *testing.T) {
	configPath, _, remoteArtifact := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes\n--RCLONE-MANAGER-BACKUP-COMPLETE--\n"))
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(filepath.Dir(configPath), "validators")); err != nil {
		t.Fatalf("reaping the validator directory: %v", err)
	}

	records := runOneCycleThenClose(t, svc, cleanup, dbPath)
	if len(records) != 1 {
		t.Fatalf("ListByBackupSet returned %d records, want 1", len(records))
	}
	if records[0].State != string(lifecycle.Complete) {
		t.Errorf("artifact state = %q, want %s: a reaped validator script wedged the backup set instead of repairing itself", records[0].State, lifecycle.Complete)
	}
	if _, err := os.Stat(remoteArtifact); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the remote artifact is still there (err = %v)", err)
	}
}

// TestRunCycle_RefusesTheCycleWhenTheScriptsCannotBeRewritten is the
// fail-closed answer to the rewrite itself failing. Running the cycle
// anyway would mean execing whatever happens to be on disk, which is the
// tampering case again; skipping validation would mean deleting remote
// sources on the strength of a check that never ran. So the cycle is
// refused, the operation records why, and nothing is transferred or
// deleted.
//
// A plain file where the directory belongs is how the failure is
// provoked: it makes MkdirAll fail deterministically, for every user
// including root, without making the state directory itself unusable.
func TestRunCycle_RefusesTheCycleWhenTheScriptsCannotBeRewritten(t *testing.T) {
	configPath, _, remoteArtifact := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes\n--RCLONE-MANAGER-BACKUP-COMPLETE--\n"))

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	scriptDir := filepath.Join(filepath.Dir(configPath), "validators")
	if err := os.RemoveAll(scriptDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.WriteFile(scriptDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	final := submitOneCycle(t, svc)
	if final.Status != "failed" {
		t.Fatalf("operation status = %q, want failed: a cycle that cannot rewrite its validator scripts must not run", final.Status)
	}
	if !strings.Contains(final.Error, "validator") {
		t.Errorf("operation error = %q, want it to name the validator scripts as the reason", final.Error)
	}
	if _, err := os.Stat(remoteArtifact); err != nil {
		t.Errorf("the remote artifact is gone (%v): a refused cycle must not have transferred or deleted anything", err)
	}
}

// TestCreateBackupSet_RefusesBeforeTheWriteWhenTheScriptsCannotBeMaterialized
// is issue #164's review finding M3. Materialization used to run after
// writeConfigAtomically, so a failure there returned an error with an
// empty Set.ID -- which the API layer correctly reads as "creation never
// happened" -- for a backup set that was durably in config.yaml.
//
// The request here names no validator at all, which is what makes the
// ordering visible: the pre-check the old code ran was gated on
// req.ValidatorID, so a plain create into a config where some OTHER set
// already uses a validator skipped materialization entirely until after
// the write.
func TestCreateBackupSet_RefusesBeforeTheWriteWhenTheScriptsCannotBeMaterialized(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes, no trailer"))

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	scriptDir := filepath.Join(filepath.Dir(configPath), "validators")
	if err := os.RemoveAll(scriptDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.WriteFile(scriptDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	req := validCreateReq(t, svc, "plain-set")
	if _, err := svc.CreateBackupSet(context.Background(), req); err == nil {
		t.Fatal("CreateBackupSet succeeded while the validator scripts could not be materialized")
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the config file was written despite the refusal:\n%s", after)
	}
	if _, err := svc.GetBackupSet(context.Background(), "api/plain-set"); err == nil {
		t.Error("the refused backup set is readable through GetBackupSet")
	}
}

// TestCreateBackupSet_StillSucceedsWhenTheScriptsCanBeMaterialized is the
// positive control for the refusal above: the same config, the same
// request, with the validator directory left alone. Without it, a
// CreateBackupSet broken in some entirely different way would satisfy
// every one of that test's assertions.
func TestCreateBackupSet_StillSucceedsWhenTheScriptsCanBeMaterialized(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes, no trailer"))

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	req := validCreateReq(t, svc, "plain-set")
	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	if _, err := svc.GetBackupSet(context.Background(), "api/plain-set"); err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
}

// TestCreateBackupSet_ThenRestartRunsTheValidatorItPersisted is issue
// #164's review finding M6: the central defect this work exists to fix,
// a process-specific executable path persisted into config.yaml, was
// tested only in halves. One test inspected the file and never reopened
// it; another opened a hand-written config that CreateBackupSet never
// produced. Both can pass while the composition fails.
//
// This runs create, Close, Open, then a cycle, over one config file. The
// only thing rewritten between the create and the restart is the remote:
// CreateBackupSet always writes an sftp remote, and a cycle needs one
// this test can actually serve. Everything the create decided about
// validation is left exactly as it wrote it.
func TestCreateBackupSet_ThenRestartRunsTheValidatorItPersisted(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t, "", nil)
	stateDir := filepath.Dir(configPath)
	dbPath := filepath.Join(stateDir, "state.db")

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	req := validCreateReq(t, svc, "created-set")
	req.ValidatorID = ValidatorTrailerMarker
	// "rename", not validCreateReq's "marker": the cycle below serves the
	// artifact out of a plain directory with no marker file beside it.
	req.CompletionStrategy = "rename"
	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Point the created set at a local directory holding one untrailered
	// artifact, leaving its validation block (validator_id and nothing
	// else) exactly as CreateBackupSet persisted it.
	remoteDir := filepath.Join(stateDir, "created-remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	remoteArtifact := filepath.Join(remoteDir, "created.dump")
	if err := os.WriteFile(remoteArtifact, []byte("payload bytes, no trailer"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	created := findBackupSetPtr(t, cfg, "api", "created-set")
	if created.Validation.ValidatorID != string(ValidatorTrailerMarker) {
		t.Fatalf("persisted validator_id = %q, want %q", created.Validation.ValidatorID, ValidatorTrailerMarker)
	}
	created.Remote = config.Remote{Type: "local"}
	created.RemotePath = remoteDir
	if err := writeConfigAtomically(configPath, cfg); err != nil {
		t.Fatalf("writeConfigAtomically: %v", err)
	}

	restarted, closeRestarted, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open (restarted): %v", err)
	}
	records := runOneCycleThenCloseFor(t, restarted, closeRestarted, dbPath, "api", "created-set")
	if len(records) != 1 {
		t.Fatalf("ListByBackupSet returned %d records, want 1", len(records))
	}
	if records[0].State != string(lifecycle.Quarantined) {
		t.Errorf("artifact state = %q, want %s: the validator this set was created with did not run after the restart", records[0].State, lifecycle.Quarantined)
	}
	if _, err := os.Stat(remoteArtifact); err != nil {
		t.Errorf("the remote artifact is gone (%v): a required validator's failure must prevent remote deletion", err)
	}
}

// TestOpen_UnregisteredValidatorIDIsRefusedBeforeAnythingOnDiskMoves is
// issue #164's review finding M5. The catalog check used to run after
// §46.1's startup sequence, which is what opens and MIGRATES the journal,
// so a validator_id this build does not know aborted startup with the
// schema already moved forward -- and a schema that has moved forward
// cannot be walked back by downgrading the binary. The id set is
// code-defined, so this is reachable without any operator error at all: a
// release that retires an id turns a config file the product wrote itself
// into a daemon that will not start.
//
// The journal file's absence is the assertion, because runStartupSequence
// is what creates it.
func TestOpen_UnregisteredValidatorIDIsRefusedBeforeAnythingOnDiskMoves(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t, "trailer-marker-typo", []byte("payload"))
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	svc, cleanup, err := Open(context.Background(), configPath)
	if err == nil {
		if cleanup != nil {
			_ = cleanup()
		}
		t.Fatalf("Open succeeded with an unregistered validator_id (svc = %v)", svc)
	}
	if !errors.Is(err, errUnregisteredValidator) {
		t.Fatalf("Open error = %v, want one wrapping errUnregisteredValidator", err)
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the journal was created (and migrated) before the validator_id was refused (stat err = %v)", statErr)
	}
}

// TestOpen_RegisteredValidatorIDStillMigratesTheJournal is the positive
// control for the ordering above: the same config with an id this build
// does know has to get all the way through §46.1's sequence and leave a
// journal behind. Without it, an Open broken so that it never reached
// runStartupSequence at all would satisfy the assertion above.
func TestOpen_RegisteredValidatorIDStillMigratesTheJournal(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t, string(ValidatorTrailerMarker), []byte("payload"))
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	_, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	if _, statErr := os.Stat(dbPath); statErr != nil {
		t.Fatalf("the journal was never created: %v", statErr)
	}
}

// findBackupSetPtr is findBackupSet (backupsets.go) returning something a
// test can edit in place, so a config just written by CreateBackupSet can
// be retargeted and written back out without rebuilding it by hand.
func findBackupSetPtr(t *testing.T, cfg *config.Config, sourceName, name string) *config.BackupSet {
	t.Helper()
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name == name {
				return &cfg.Sources[i].BackupSets[j]
			}
		}
	}
	t.Fatalf("backup set %s/%s is not in the config", sourceName, name)
	return nil
}

// TestOpen_AnUnrunnableValidatorCommandFailsTheArtifactAndKeepsTheRemote
// is the other end of the "the validator could not be run at all" branch,
// through the trusted config path that names an executable directly (the
// one #162 narrows for the API/UI layer, and leaves exactly as it was for
// CLI/YAML). It is what the run-path rewrite makes unreachable for a
// registered validator, and it is still reachable for a hand-written
// command, so the classification is worth pinning: Failed, not
// Quarantined, because this is infrastructure rather than the validator
// forming an opinion -- and either way the remote source survives, since
// neither state can reach COMMITTED.
func TestOpen_AnUnrunnableValidatorCommandFailsTheArtifactAndKeepsTheRemote(t *testing.T) {
	configPath, _, remoteArtifact := writeValidatorConfigFile(t, "", []byte("payload bytes"))
	stateDir := filepath.Dir(configPath)
	dbPath := filepath.Join(stateDir, "state.db")

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	bs := findBackupSetPtr(t, cfg, "production", "postgres-primary")
	bs.Validation.Command = &config.Command{
		Executable: filepath.Join(stateDir, "no-such-validator.sh"),
		Timeout:    config.Duration(30 * time.Second),
	}
	if err := writeConfigAtomically(configPath, cfg); err != nil {
		t.Fatalf("writeConfigAtomically: %v", err)
	}

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	records := runOneCycleThenClose(t, svc, cleanup, dbPath)
	if len(records) != 1 {
		t.Fatalf("ListByBackupSet returned %d records, want 1", len(records))
	}
	if records[0].State != string(lifecycle.Failed) {
		t.Errorf("artifact state = %q, want %s", records[0].State, lifecycle.Failed)
	}
	if _, err := os.Stat(remoteArtifact); err != nil {
		t.Errorf("the remote artifact is gone (%v): a validator that could not be run must not authorize deleting the source", err)
	}
}

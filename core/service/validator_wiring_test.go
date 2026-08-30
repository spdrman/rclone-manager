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
	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-validator-wiring",
		Actor:          "test",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	if final := waitForTerminalStatus(t, svc, op.ID); final.Status != "completed" {
		t.Fatalf("operation status = %q, want completed (Error = %q)", final.Status, final.Error)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("closing the service: %v", err)
	}

	journal, err := state.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopening the journal: %v", err)
	}
	defer func() { _ = journal.Close() }()

	setID, err := model.NewBackupSetID("production", "postgres-primary")
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

// TestClose_RemovesTheMaterializedValidatorDirectory is issue #162's
// third materialisation item. The old implementation never removed its
// os.MkdirTemp directory at all, so every process start that resolved a
// validator leaked one.
func TestClose_RemovesTheMaterializedValidatorDirectory(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t,
		string(ValidatorTrailerMarker),
		[]byte("payload bytes, no trailer"))

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = svc

	scriptDir := filepath.Join(filepath.Dir(configPath), "validators")
	if _, err := os.Stat(scriptDir); err != nil {
		t.Fatalf("the validator directory was not materialized next to the state database: %v", err)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(scriptDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the validator directory survived Close (err = %v)", err)
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

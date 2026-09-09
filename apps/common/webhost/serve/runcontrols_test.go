// runcontrols_test.go is issue #597's evidence, and it is deliberately
// driven through serve.NewEngine rather than through webhost.NewRouter.
//
// Every green Go test that submits a run cycle before this file passed
// `Gate: alwaysPassGate{}` into NewRouter, a test-only type. That is why
// nothing noticed that POST /api/v1/operations answers 403 on every build
// this project has ever shipped: the production composition was never the
// thing under test. So the two properties below are asserted against the
// composition main.go actually builds.
//
// The first test is the deployment's own answer, with no Gate supplied at
// all, exactly as apps/generic/cmd/rbm-web/main.go builds it.
// It is allowed to assert a refusal: a refusal is a legitimate outcome to
// record, and recording it is the check that would have caught this two
// releases ago. What it is not allowed to do is assert a bare 403, which
// requireCSRF also produces.
//
// The second test opens the gate from the test, which is the only way to
// reach the work at all while #92/#602 keep the shipped gate shut, and
// then proves the bytes moved. A request that returns 202 proves nothing:
// this whole issue exists because a button that looked fine returned a
// 403 nobody rendered. So it compares the landed file's SHA-256 against
// the source's, and asserts the backup set nobody asked for was left
// exactly as it was.
package serve_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
	"github.com/spdrman/rclone-manager/core/service"
)

// openGate is a DestructiveGate that passes, supplied by this test and
// nowhere else. It is the counterpart of the first test below rather than
// a replacement for it: with the shipped wiring pinned to its real answer
// one test over, opening the gate here proves what the route does once
// somebody has opened it, without either test standing in for the other.
type openGate struct{}

func (openGate) Passed() bool { return true }

// twoBackupSets writes a configuration with two backup sets against real
// temp directories, each with one artifact already sitting on its remote,
// and returns the config path plus the two remote/local directory pairs.
//
// Two sets rather than one, because the claim a per-set run makes is
// "this run covers one backup set", and that is the claim that fails
// silently. A single-set fixture cannot see it.
func twoBackupSets(t *testing.T) (configPath string, dirs map[string]struct{ remote, local string }) {
	t.Helper()
	dir := t.TempDir()
	dirs = map[string]struct{ remote, local string }{}

	var sets string
	for _, name := range []string{"alpha", "beta"} {
		remoteDir := filepath.Join(dir, name, "remote")
		localDir := filepath.Join(dir, name, "local")
		if err := os.MkdirAll(remoteDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(remoteDir, name+".dump"), []byte("payload for "+name), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		dirs[name] = struct{ remote, local string }{remoteDir, localDir}
		sets += "      - id: " + name + "\n" +
			"        remote:\n" +
			"          type: local\n" +
			"        remote_path: " + remoteDir + "\n" +
			"        local_path: " + localDir + "\n" +
			"        include:\n" +
			"          - \"*.dump\"\n" +
			"        completion:\n" +
			"          strategy: rename\n" +
			"        stale_after: 24h\n"
	}

	configPath = filepath.Join(dir, "config.yaml")
	content := "poll_interval: 1h\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" + sets +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath, dirs
}

// runHarness stands the engine up over a given configuration and gate,
// signed in as a real administrator through the real enrollment flow.
type runHarness struct {
	*engineHarness
	revision string
}

func newRunHarness(t *testing.T, configPath string, gate webhost.DestructiveGate) *runHarness {
	t.Helper()

	backend, cleanup, err := service.Open(t.Context(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("BackupService cleanup: %v", err)
		}
	})

	authSvc, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	srv := httptest.NewServer(serve.NewEngine(serve.EngineConfig{
		Platform:              testPlatformAdapter{auth: authSvc},
		AuthRoutes:            authSvc.Handler(),
		TrustForwardedHeaders: authSvc.TrustForwardedHeaders(),
		Backend:               backend,
		Gate:                  gate,
		BinaryVersion:         "test",
		Commit:                "testcommit",
	}))
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	h := &runHarness{
		engineHarness: &engineHarness{server: srv, auth: authSvc, client: &http.Client{Jar: jar}},
		revision:      backend.ConfigRevision(),
	}
	enrollAndLogIn(t, h.engineHarness, h.client, srv.URL)
	return h
}

// submit posts exactly what the browser's run button posts: the CSRF pair
// a session already carries, an Idempotency-Key header, and a body naming
// the action and the configuration revision the page was showing.
func (h *runHarness) submit(t *testing.T, key string, body map[string]any) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal submission: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/operations", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set(local.CSRFHeaderName, csrfToken(t, h.client, h.server.URL))

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/operations: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// errorCodeOf reads the typed code out of the service's error envelope.
// A status alone cannot say which control refused: requireCSRF and the
// destructive gate both answer 403, and 409 covers three different
// refusals a client acts on differently.
func errorCodeOf(t *testing.T, body string) string {
	t.Helper()
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("response body is not an error envelope: %v (body=%s)", err, body)
	}
	return doc.Error.Code
}

// awaitOperation polls GET /api/v1/operations/{id} the way the browser
// does, until the row reaches a terminal status.
func (h *runHarness) awaitOperation(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := h.client.Get(h.server.URL + "/api/v1/operations/" + id)
		if err != nil {
			t.Fatalf("GET /api/v1/operations/%s: %v", id, err)
		}
		var doc map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			resp.Body.Close()
			t.Fatalf("decode operation: %v", err)
		}
		resp.Body.Close()
		switch doc["status"] {
		case "completed", "failed":
			return doc
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s never reached a terminal status; last read %v", id, doc)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func digestOf(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// countFiles reports how many regular files live under root, walking it
// whole. Zero for a directory the engine never created, which is the
// state a backup set nobody ran should be in.
func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", root, err)
	}
	return n
}

// TestEngine_TheShippedWiringRefusesEveryRunSubmission is the acceptance
// criterion "whatever the deployment's answer to a run_cycle submission
// is, a test asserts it against the production wiring".
//
// No Gate is supplied, which is exactly how the shipped binary builds its
// serve.EngineConfig, and the request carries everything a correct client
// sends: a live session, the CSRF pair, an Idempotency-Key, and the
// configuration revision the service itself reports. So the only thing
// left that can refuse it is the destructive gate, and the assertion is
// on its typed code rather than on the status, because requireCSRF
// answers 403 too.
//
// Both actions are asserted, because they are in the same tier by
// construction: requireDestructiveGate is middleware that runs before the
// body is decoded, so the route cannot tell them apart even in principle.
func TestEngine_TheShippedWiringRefusesEveryRunSubmission(t *testing.T) {
	configPath, _ := twoBackupSets(t)
	h := newRunHarness(t, configPath, nil)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"run_cycle", map[string]any{"action": "run_cycle", "config_revision": h.revision}},
		{"run_backup_set", map[string]any{
			"action":          "run_backup_set",
			"config_revision": h.revision,
			"backup_set_id":   "production/alpha",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.submit(t, "shipped-wiring-"+tc.name, tc.body)
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", status, http.StatusForbidden, body)
			}
			if code := errorCodeOf(t, body); code != "DESTRUCTIVE_OPERATIONS_DISABLED" {
				t.Errorf("error code = %q, want DESTRUCTIVE_OPERATIONS_DISABLED.\n"+
					"a bare 403 assertion would not have noticed: requireCSRF refuses with the same status",
					code)
			}
		})
	}
}

// TestEngine_RunBackupSetMovesThatSetsBytesAndNothingElse is the per-set
// run itself, end to end over HTTP, against a real journal and a real
// transport.
//
// The digest comparison is the assertion. A file that exists is not a
// backup: the pipeline could have created an empty placeholder, or landed
// the wrong artifact, and a test that only asked whether the path exists
// would pass for both.
//
// The second half is the claim that actually fails silently. `beta` is
// configured against the same deployment and nobody asked for it, so its
// local directory must still hold nothing and its remote artifact must
// still be sitting there un-deleted: FR-15 removes a remote source only
// after that set's own artifact is durable, so a stray beta file on
// either side would mean the "per-set" run was a cycle in disguise.
func TestEngine_RunBackupSetMovesThatSetsBytesAndNothingElse(t *testing.T) {
	configPath, dirs := twoBackupSets(t)
	sourceDigest := digestOf(t, filepath.Join(dirs["alpha"].remote, "alpha.dump"))
	h := newRunHarness(t, configPath, openGate{})

	status, body := h.submit(t, "per-set-run-1", map[string]any{
		"action":          "run_backup_set",
		"config_revision": h.revision,
		"backup_set_id":   "production/alpha",
	})
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", status, http.StatusAccepted, body)
	}

	var submitted struct {
		OperationID string `json:"operation_id"`
		BackupSetID string `json:"backup_set_id"`
		Action      string `json:"action"`
	}
	if err := json.Unmarshal([]byte(body), &submitted); err != nil {
		t.Fatalf("decode submission response: %v (body=%s)", err, body)
	}
	if submitted.BackupSetID != "production/alpha" {
		t.Errorf("operation backup_set_id = %q, want %q; the durable row is what tells a later reader which set this run was for",
			submitted.BackupSetID, "production/alpha")
	}
	if submitted.Action != "run_backup_set" {
		t.Errorf("operation action = %q, want %q", submitted.Action, "run_backup_set")
	}

	done := h.awaitOperation(t, submitted.OperationID)
	if done["status"] != "completed" {
		t.Fatalf("operation status = %v, want completed (error = %v)", done["status"], done["error"])
	}

	// The bytes. Walk the local tree rather than guessing at the layout,
	// and count the artifacts rather than every file: a durable copy
	// lands beside its own manifest sidecar, which is part of the commit
	// and not a second backup.
	var landed, manifests []string
	if err := filepath.Walk(dirs["alpha"].local, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
		case strings.HasSuffix(path, ".manifest.json"):
			manifests = append(manifests, path)
		default:
			landed = append(landed, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking alpha's local path: %v", err)
	}
	if len(landed) != 1 {
		t.Fatalf("alpha landed %d artifacts, want exactly 1: %v", len(landed), landed)
	}
	if len(manifests) != 1 {
		t.Errorf("alpha landed %d manifests beside its artifact, want exactly 1: %v", len(manifests), manifests)
	}
	if got := digestOf(t, landed[0]); got != sourceDigest {
		t.Errorf("landed digest = %s, want %s: the file exists but it is not the artifact that was on the remote", got, sourceDigest)
	}

	// Nothing else moved.
	if n := countFiles(t, dirs["beta"].local); n != 0 {
		t.Errorf("beta's local path holds %d files after a run of alpha alone; a per-set run must not touch another set", n)
	}
	if _, err := os.Stat(filepath.Join(dirs["beta"].remote, "beta.dump")); err != nil {
		t.Errorf("beta's remote artifact is gone after a run of alpha alone (%v); FR-15 deletes a remote source only after that set's own backup is durable", err)
	}
}

// TestEngine_RunBackupSetRefusesAnIdThisDeploymentDoesNotHave pins the
// refusal an operator reaches by pressing a button on a page that has
// moved on. It is a typed 404 rather than a 500, because "check the id"
// and "the service broke" send an operator in opposite directions.
func TestEngine_RunBackupSetRefusesAnIdThisDeploymentDoesNotHave(t *testing.T) {
	configPath, _ := twoBackupSets(t)
	h := newRunHarness(t, configPath, openGate{})

	status, body := h.submit(t, "per-set-run-unknown", map[string]any{
		"action":          "run_backup_set",
		"config_revision": h.revision,
		"backup_set_id":   "production/nothing-here",
	})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", status, http.StatusNotFound, body)
	}
	if code := errorCodeOf(t, body); code != "BACKUP_SET_NOT_FOUND" {
		t.Errorf("error code = %q, want BACKUP_SET_NOT_FOUND", code)
	}
}

// TestEngine_RunBackupSetRefusesASubmissionWithNoSet is the other half of
// that: an action that names no set at all is a malformed request, not a
// missing resource, and the two are different fixes.
func TestEngine_RunBackupSetRefusesASubmissionWithNoSet(t *testing.T) {
	configPath, _ := twoBackupSets(t)
	h := newRunHarness(t, configPath, openGate{})

	status, body := h.submit(t, "per-set-run-empty", map[string]any{
		"action":          "run_backup_set",
		"config_revision": h.revision,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", status, http.StatusBadRequest, body)
	}
	if code := errorCodeOf(t, body); code != "INVALID_REQUEST" {
		t.Errorf("error code = %q, want INVALID_REQUEST", code)
	}
	// The message too, and not for its own sake: INVALID_REQUEST is also
	// what an action this release does not recognise gets, so a code-only
	// assertion here would have passed before run_backup_set existed at
	// all.
	if !strings.Contains(body, "has to say which backup set to run") {
		t.Errorf("refusal does not name the missing field, so this test would pass against a build that does not know the action at all: %s", body)
	}
}

// TestEngine_RunCycleRefusesABodyThatNamesOneSet is the other direction.
// A run_cycle is deployment-wide by definition, so a body naming one set
// has confused the two actions, and ignoring the field would leave an
// operator believing one set ran when every set did.
func TestEngine_RunCycleRefusesABodyThatNamesOneSet(t *testing.T) {
	configPath, _ := twoBackupSets(t)
	h := newRunHarness(t, configPath, openGate{})

	status, body := h.submit(t, "cycle-with-a-set", map[string]any{
		"action":          "run_cycle",
		"config_revision": h.revision,
		"backup_set_id":   "production/alpha",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", status, http.StatusBadRequest, body)
	}
	if code := errorCodeOf(t, body); code != "INVALID_REQUEST" {
		t.Errorf("error code = %q, want INVALID_REQUEST", code)
	}
	if !strings.Contains(body, "run_backup_set") {
		t.Errorf("refusal does not name the action that would have done what was asked: %s", body)
	}
}

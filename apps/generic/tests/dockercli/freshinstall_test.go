// This file is issue #206's live evidence: a fresh install brings up the
// web UI, driven through `docker compose` against the real built image
// rather than reasoned about from the YAML.
//
// The defect it pins is a conjunction, which is why nothing in this repo
// caught it. Every adapter gates its web UI on
// `depends_on: rclone-manager: condition: service_healthy`, and the
// engine's health check was `backup-manager status`, which is FR-24's
// backup-freshness verdict and exits non-zero on any DEGRADED, STALE or
// FAILING set - a fresh install included, by design, because a fresh
// install has never backed anything up. So the one container an operator
// installed the app to reach never started, and every suite in the tree
// happened to test against a stack whose backup set had already been
// seeded HEALTHY (workingRemoteConfig, and its own doc says why it has
// to be).
//
// Two tests, and the second is what gives the first its meaning. The
// first brings up a genuinely fresh install and requires the web UI to
// serve. The second puts the old health verdict back as the engine's
// start gate, on the same fixture, and requires the web UI NOT to come
// up - so a run that passes is a run that would have failed against
// what shipped, rather than one that only ever tested a healthy stack.

package dockercli_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freshInstallConfig is a fresh install exactly as the shipped
// acceptance procedures leave one: step 0 writes config.yaml into the
// configuration DIRECTORY (issue #196), the state directory is empty,
// the backup directory is empty, and no backup set has ever run.
//
// `backup-manager status` is non-zero in that state and is supposed to
// be: nothing has been backed up, so nothing is fresh. That is the whole
// of #206. The fixture deliberately does NOT seed an artifact the way
// workingRemoteConfig does, because seeding one is what hid the defect.
func freshInstallConfig(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	// 0o777 for the same reason degradedConfig uses it: the runtime
	// image runs as a fixed non-root uid that does not own a
	// t.TempDir() subdirectory, and on a Linux daemon that is a hard
	// permission failure rather than the leniency a macOS Docker Desktop
	// daemon shows. See degradedConfig's own comment.
	for _, sub := range []string{"state", "backups", filepath.Join("backups", "remote"), filepath.Join("backups", "local"), "config"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
		if err := os.Chmod(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatalf("Chmod %s: %v", sub, err)
		}
	}

	// The same shape workingRemoteConfig writes, minus the seeded
	// artifact, plus a one-second staleness window so the set is
	// unambiguously not fresh the moment the engine reads it rather than
	// eventually. remote_path and local_path both live under
	// /data/backups because that is the one writable directory
	// container/compose.yaml actually mounts.
	config := "" +
		"poll_interval: 1h\n" +
		"state:\n" +
		"  database: /data/state/state.db\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: never-backed-up\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: /data/backups/remote\n" +
		"        local_path: /data/backups/local\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 1s\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(filepath.Join(dir, "config", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile config.yaml: %v", err)
	}
	return dir
}

// statusExitCode runs `backup-manager status` inside a running container
// and returns its exit code with the output it printed.
//
// This is the positive control the whole file turns on. Without it, a
// green run proves only that some stack came up, and a fixture that had
// quietly become healthy would pass while saying nothing at all about
// the defect.
func statusExitCode(t *testing.T, containerID string) (int, string) {
	t.Helper()
	cmd := exec.Command("docker", "exec", containerID, "/backup-manager", "status")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exit *exec.ExitError
	if !asExitError(err, &exit) {
		t.Fatalf("docker exec %s /backup-manager status: %v\n%s", containerID, err, out)
	}
	return exit.ExitCode(), string(out)
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// containerState is `docker inspect`'s State.Status for one container.
func containerState(t *testing.T, id string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect -f State.Status %s: %v", id, err)
	}
	return strings.TrimSpace(string(out))
}

// TestComposeStack_FreshInstallBringsUpTheWebUI is issue #206's
// acceptance criterion, live: no backup has ever run, so the engine's
// backup-freshness verdict is negative, and the web UI still starts and
// serves.
func TestComposeStack_FreshInstallBringsUpTheWebUI(t *testing.T) {
	image := buildImage(t)
	dir := freshInstallConfig(t)

	project := mustStartComposeStack(t, image, 0, dir)

	engineID := project.containerID(t, "rclone-manager")

	// The engine's container health is a liveness answer now, so a fresh
	// install reaches "healthy" even though it has backed nothing up.
	if got := healthStatus(t, engineID, 90*time.Second); got != "healthy" {
		logs, _ := exec.Command("docker", "logs", engineID).CombinedOutput()
		t.Fatalf("the engine's container health is %q on a fresh install, want %q; the web UI waits on this, so anything else means it never starts. The probe's last output was %q. Engine logs:\n%s",
			got, "healthy", lastHealthOutput(t, engineID), logs)
	}

	// The control that makes the line above mean something. If this
	// exits 0 the fixture is not a fresh install at all, and the test
	// would pass just as happily against the engine health check that
	// shipped.
	code, statusOut := statusExitCode(t, engineID)
	if code == 0 {
		t.Fatalf("`backup-manager status` exited 0 inside the fresh install this test is built on, so the fixture is already healthy and proves nothing about #206; status said:\n%s", statusOut)
	}
	t.Logf("`backup-manager status` exited %d on this fresh install, as FR-24 intends:\n%s", code, statusOut)

	// The web UI container: it exists, it is running, and its own health
	// check passes.
	uiID := project.containerID(t, "web-ui")
	if got := containerState(t, uiID); got != "running" {
		logs, _ := exec.Command("docker", "logs", uiID).CombinedOutput()
		t.Fatalf("the web UI container is %q on a fresh install, want %q; logs:\n%s", got, "running", logs)
	}
	if got := healthStatus(t, uiID, 90*time.Second); got != "healthy" {
		logs, _ := exec.Command("docker", "logs", uiID).CombinedOutput()
		t.Fatalf("the web UI container's health is %q, want %q; logs:\n%s", got, "healthy", logs)
	}

	// And it serves, over its published port, which is the only thing
	// the operator can actually open. Transport errors are retried for
	// the same reason
	// TestComposeStack_WebUIProxiesToTheEngineEndToEnd retries its own
	// first request: Docker's published-port proxy accepts the TCP
	// connection from the moment the port exists and resets it until the
	// process behind it is listening. A response is never retried.
	base := "http://127.0.0.1:" + project.publishedPort(t, "web-ui", "8080/tcp")
	client := &http.Client{Timeout: 10 * time.Second}
	resp := getWithTransportRetry(t, client, base+"/", 30*time.Second)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s/ on a fresh install status = %d, want %d (the web UI's own static shell, which is what the first-run flow is served from)", base, resp.StatusCode, http.StatusOK)
	}

	// The proxy half, which is what makes it a working page rather than
	// a container that merely started: /health/ is forwarded to the
	// engine unchanged (apps/common/webhost/serve.NewUI), so a 200 here
	// is the engine answering through the UI container on a fresh
	// install.
	live := getWithTransportRetry(t, client, base+"/health/live", 30*time.Second)
	live.Body.Close()
	if live.StatusCode != http.StatusOK {
		t.Errorf("GET %s/health/live (proxied to the engine) status = %d, want %d", base, live.StatusCode, http.StatusOK)
	}
}

// getWithTransportRetry retries only the transport error, never a
// response: whatever finally answers still has to answer with the status
// the caller requires.
func getWithTransportRetry(t *testing.T, client *http.Client, url string, timeout time.Duration) *http.Response {
	t.Helper()
	deadline := time.Now().Add(timeout)
	attempts := 0
	for {
		attempts++
		resp, err := client.Get(url)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s never succeeded after %d attempts: %v", url, attempts, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// statusGateOverride writes a compose override that restores the engine
// health check this repository shipped: `backup-manager status`, the
// backup-freshness verdict.
//
// The timings are compressed and nothing else is. What is under test is
// the COMMAND the start gate asks, not how long Docker waits before
// believing it, and the shipped 30s interval times three retries would
// make this control a 95-second wait for an answer that is already
// decided at the first check.
func statusGateOverride(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "status-gate-override.yaml")
	content := "" +
		"services:\n" +
		"  rclone-manager:\n" +
		"    healthcheck:\n" +
		"      test: [\"CMD\", \"/backup-manager\", \"status\"]\n" +
		"      interval: 2s\n" +
		"      timeout: 5s\n" +
		"      start_period: 1s\n" +
		"      retries: 2\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile status-gate override: %v", err)
	}
	return path
}

// TestComposeStack_TheBackupFreshnessVerdictAsAStartGateKeepsTheWebUIDown
// is the other half of the pair, and the reason the test above is
// evidence rather than a green tick: the exact health check that shipped,
// put back on the exact same fresh install, must keep the web UI from
// ever starting.
//
// Without it, a passing run says only that this stack came up. With it, a
// passing run says the stack came up BECAUSE the start gate stopped
// asking about backup freshness.
func TestComposeStack_TheBackupFreshnessVerdictAsAStartGateKeepsTheWebUIDown(t *testing.T) {
	image := buildImage(t)
	dir := freshInstallConfig(t)
	override := statusGateOverride(t, dir)

	project, out, err := upComposeStack(t, image, 0, dir, override)
	if err == nil {
		t.Fatalf("`docker compose up -d` succeeded with `backup-manager status` as the engine's start gate on a fresh install, so this control cannot see the defect it exists to reproduce:\n%s", out)
	}

	// Assert WHY it failed, not merely that it did. A typo in the
	// override, an image that is not there, or a missing env file all
	// fail `up` too, and none of them would be this defect.
	if !strings.Contains(out, "rclone-manager") || !strings.Contains(strings.ToLower(out), "unhealthy") {
		t.Fatalf("`docker compose up -d` failed, but not with the engine reporting unhealthy, so the failure is something other than the start gate:\n%s", out)
	}

	// The symptom #206 is actually about: no web UI an operator can
	// reach. Existence is not the assertion, because compose CREATES the
	// dependent container and then never starts it, which is a container
	// that answers nothing on a port it never published.
	if id := project.containerIDIfAny(t, "web-ui"); id != "" {
		state := containerState(t, id)
		if state == "running" {
			t.Errorf("the web UI container is running despite the engine never reporting healthy; the dependency gate is not what this control thinks it is")
		} else {
			t.Logf("the web UI container exists in state %q and was never started, which is the state a fresh install shipped in", state)
		}
		if out, err := exec.Command("docker", "port", id).CombinedOutput(); err == nil && strings.TrimSpace(string(out)) != "" {
			t.Errorf("the web UI publishes %q despite never having started; the symptom this control reproduces is that there is no page to open", strings.TrimSpace(string(out)))
		}
	}

	// And the engine itself is running and serving, which is what makes
	// the gate's verdict wrong rather than merely unlucky: the container
	// the UI was waiting on was up the whole time.
	engineID := project.containerIDIfAny(t, "rclone-manager")
	if engineID == "" {
		t.Fatal("the engine has no container either, so this control reproduces a stack that never started rather than a start gate that never released")
	}
	if got := containerState(t, engineID); got != "running" {
		t.Errorf("the engine container is %q, want %q: the point of this control is that a LIVE engine was reported unhealthy", got, "running")
	}
	code, statusOut := statusExitCode(t, engineID)
	if code == 0 {
		t.Errorf("`backup-manager status` exited 0 inside the engine this control declared unhealthy, so the two disagree and one of them is not measuring what it claims; status said:\n%s", statusOut)
	}
}

// dockerHealthLog is one entry of Docker's own health history, read only
// to put the engine's last health output into a failure message.
type dockerHealthLog struct {
	State struct {
		Health struct {
			Status string `json:"Status"`
			Log    []struct {
				ExitCode int    `json:"ExitCode"`
				Output   string `json:"Output"`
			} `json:"Log"`
		} `json:"Health"`
	} `json:"State"`
}

// lastHealthOutput is what the container's most recent health check
// printed, so a failure names what the probe actually said rather than
// only that it was unhappy.
func lastHealthOutput(t *testing.T, id string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", id).Output()
	if err != nil {
		return ""
	}
	var entries []dockerHealthLog
	if err := json.Unmarshal(out, &entries); err != nil || len(entries) != 1 {
		return ""
	}
	log := entries[0].State.Health.Log
	if len(log) == 0 {
		return ""
	}
	last := log[len(log)-1]
	return strings.TrimSpace(last.Output)
}

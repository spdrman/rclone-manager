// Package dockercli_test is issue #82 (B4.1)'s Docker CLI integration
// suite (docs/EPIC-B-multi-nas.md §67): it builds the real
// container/Dockerfile image and drives it with the actual `docker` CLI,
// the same way an operator would, rather than unit-testing
// container/Dockerfile's text.
//
// This is the RED half of "swap the HEALTHCHECK CMD from version to
// status": before that change, container/Dockerfile's HEALTHCHECK always
// reports "healthy" (it only proves the binary can start), no matter how
// unhealthy the configured backup sets actually are. This suite forces a
// DEGRADED backup set into a running container and asserts the
// HEALTHCHECK reports "unhealthy" — a claim that is false against the
// pre-fix Dockerfile and true against the fixed one.
//
// Skipped automatically wherever the `docker` CLI or daemon is not
// available (this project has no other test that depends on either), so
// a checkout without Docker still runs the rest of the suite.
package dockercli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// dockerCLIImage is the tag every test in this file builds once and
// shares; RunMain builds it before any test runs so a build failure is
// reported once, clearly, rather than once per test.
const dockerCLIImage = "backup-manager:dockercli-test"

// repoRoot is this file's own directory, three levels up
// (core/tests/dockercli -> core/tests -> core -> repo root): the same
// directory container/compose.yaml's `build.context: ..` resolves to from
// container/, and what `docker build` needs as its context so
// container/Dockerfile's `COPY core/...` lines resolve.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root, err := filepath.Abs(filepath.Join(wd, "..", "..", ".."))
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	return root
}

// requireDocker skips the calling test unless a real, running Docker
// daemon answers `docker info`. This is not this project's usual "no
// external dependency" testing style (see core/tests/sftpfixture's
// in-process fake server for the alternative this project prefers
// wherever it's possible) — it is deliberately impossible here, since
// this suite's entire point (docs/EPIC-B-multi-nas.md §67) is exercising
// the real `docker` CLI against the real built image, not a stand-in for
// either.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available on PATH; skipping Docker CLI integration suite")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable; skipping Docker CLI integration suite")
	}
}

var imageBuilt bool

// buildImage builds container/Dockerfile once per test process (subsequent
// calls are a cheap no-op thanks to Docker's own layer cache, but this
// still avoids paying even that cost more than once per `go test` run)
// and returns its tag. A single amd64/arm64-native load (not a multi-arch
// buildx invocation) is enough here: architecture parity is CI's
// ugreen-cross-compile job's job, not this suite's.
func buildImage(t *testing.T) string {
	t.Helper()
	requireDocker(t)

	if !imageBuilt {
		root := repoRoot(t)
		cmd := exec.Command("docker", "build",
			"-f", filepath.Join(root, "container", "Dockerfile"),
			"-t", dockerCLIImage,
			"--load",
			root,
		)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker build failed: %v\n%s", err, out.String())
		}
		imageBuilt = true
	}
	return dockerCLIImage
}

// degradedConfig writes a config whose one backup set has never had an
// artifact discovered for it: internal/health's own decideState (see that
// package's doc) reports this as DEGRADED, and `backup-manager status`
// (cmd/backup-manager/status.go) exits 1 for anything short of HEALTHY.
// This is real backup-set evidence, not a synthetic health override, so
// it exercises exactly what a container healthcheck would see in
// production the day a backup set actually falls behind.
func degradedConfig(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	for _, sub := range []string{"remote", "backups", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "remote", ".keep"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile .keep: %v", err)
	}

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
		"        remote_path: /data/remote\n" +
		"        local_path: /data/backups\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 1s\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile config.yaml: %v", err)
	}
	return dir
}

// runDaemonContainer starts image in the background with `daemon` as its
// command against dir's config (see degradedConfig), overriding the
// image's own baked-in HEALTHCHECK timing with much shorter values so
// this test does not have to wait out the real 30s interval / 3 retries.
// It registers cleanup to remove the container regardless of test
// outcome, and returns the container name.
func runDaemonContainer(t *testing.T, image, dir string) string {
	t.Helper()
	name := "backup-manager-dockercli-" + t.Name() + "-" + time.Now().Format("150405.000000")

	args := []string{
		"run", "-d", "--name", name,
		"-v", filepath.Join(dir, "config.yaml") + ":/etc/backup-manager/config.yaml:ro",
		"-v", filepath.Join(dir, "state") + ":/data/state",
		"-v", filepath.Join(dir, "remote") + ":/data/remote:ro",
		"-v", filepath.Join(dir, "backups") + ":/data/backups",
		"--health-interval=2s", "--health-timeout=2s", "--health-retries=1", "--health-start-period=1s",
		image, "daemon", "--config", "/etc/backup-manager/config.yaml",
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", name).Run()
	})
	return name
}

type dockerHealth struct {
	Status string `json:"Status"`
}

type dockerInspectState struct {
	Health dockerHealth `json:"Health"`
}

type dockerInspectEntry struct {
	State dockerInspectState `json:"State"`
}

// healthStatus polls `docker inspect` until the container reports a
// terminal health status ("healthy" or "unhealthy") or timeout elapses,
// returning whatever it last observed. Docker's own health state machine
// has a transient "starting" phase, so a single inspect right after
// `docker run` would be flaky by construction; polling is what makes this
// deterministic instead of racing the health monitor's own timing.
func healthStatus(t *testing.T, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect", name).Output()
		if err != nil {
			t.Fatalf("docker inspect: %v", err)
		}
		var entries []dockerInspectEntry
		if err := json.Unmarshal(out, &entries); err != nil {
			t.Fatalf("unmarshal docker inspect output: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("docker inspect returned %d entries, want 1", len(entries))
		}
		last = entries[0].State.Health.Status
		if last == "healthy" || last == "unhealthy" {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}

// TestHealthCheckTracksStatusExitCode is this issue's RED/GREEN pivot:
// against container/Dockerfile as EPIC A left it, HEALTHCHECK runs
// `backup-manager version`, which exits 0 unconditionally, so a container
// whose one backup set is DEGRADED still reports "healthy" — the bug this
// issue's item 1 fixes. Once HEALTHCHECK runs `backup-manager status`
// instead, the same DEGRADED backup set makes it report "unhealthy".
func TestHealthCheckTracksStatusExitCode(t *testing.T) {
	image := buildImage(t)
	dir := degradedConfig(t)
	name := runDaemonContainer(t, image, dir)

	got := healthStatus(t, name, 30*time.Second)
	if got != "unhealthy" {
		t.Errorf("container health status = %q, want %q (backup-manager status must exit non-zero for a DEGRADED backup set, and HEALTHCHECK must run status, not version)", got, "unhealthy")
	}
}

// TestDaemonStaysRunningWithValidConfig is this issue's regression check
// for the Given/When/Then in the issue body: `daemon` against a valid
// config runs headless indefinitely as a normal container, rather than
// exiting immediately the way `command: ["version"]` used to (see
// container/compose.yaml's own former TODO(daemon) comment for exactly
// that failure mode).
func TestDaemonStaysRunningWithValidConfig(t *testing.T) {
	image := buildImage(t)
	dir := degradedConfig(t)
	name := runDaemonContainer(t, image, dir)

	// Give the process a moment to either still be running or to have
	// crash-looped; `command: ["version"]` would already have exited by
	// this point.
	time.Sleep(3 * time.Second)

	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	if got := string(bytes.TrimSpace(out)); got != "true" {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		t.Fatalf("container %s is not running (State.Running=%s); logs:\n%s", name, got, logs)
	}
}

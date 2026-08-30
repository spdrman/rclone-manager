// Package dockercli_test is issue #82 (B4.1)'s Docker CLI integration
// suite (docs/EPIC-B-multi-nas.md §67): it builds the real
// container/Dockerfile image and drives it with the actual `docker` CLI,
// the same way an operator would, rather than unit-testing
// container/Dockerfile's text.
//
// This lives under apps/generic/, not core/, even though half of it
// (TestHealthCheckTracksStatusExitCode, TestDaemonStaysRunningWithValidConfig)
// only exercises the plain /backup-manager binary core/ alone produces:
// container/Dockerfile's frontend-build and build-web stages COPY apps/
// and ui/shared/ to build /backup-manager-web, so a test package that
// builds this Dockerfile at all cannot live inside core/'s own module
// without breaking "core/ builds and its full test suite passes with
// apps/ deleted entirely" (§7.1, WP1.1's own acceptance criterion,
// scripts/architecture/verify-core-without-apps.sh) - caught by actually
// running that script against an earlier version of this file that DID
// live under core/tests/dockercli, not by reasoning about it in advance.
// apps/generic is the right home instead: it already assumes core/ and
// apps/common/ are present as sibling directories (see its own go.mod's
// replace directives), so a test exercising the full assembled image
// adds no new assumption beyond what apps/generic's own build already
// requires.
//
// The HEALTHCHECK-tracks-status behavior this suite is built around: RED
// was container/Dockerfile's HEALTHCHECK always reporting "healthy" (it
// only proved the binary could start), no matter how unhealthy the
// configured backup sets actually were. TestHealthCheckTracksStatusExitCode
// forces a DEGRADED backup set into a running container and asserts the
// HEALTHCHECK reports "unhealthy" - true against the fixed Dockerfile,
// false against the version this suite was first written against.
//
// Skipped automatically wherever the `docker` CLI or daemon is not
// available, so a checkout without Docker still runs the rest of the
// suite.
package dockercli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// dockerCLIImage is the tag every test in this file builds once and
// shares; RunMain builds it before any test runs so a build failure is
// reported once, clearly, rather than once per test.
const dockerCLIImage = "backup-manager:dockercli-test"

// repoRoot is this file's own directory, four levels up
// (apps/generic/tests/dockercli -> apps/generic/tests -> apps/generic ->
// apps -> repo root): the same directory container/compose.yaml's
// `build.context: ..` resolves to from container/, and what `docker
// build` needs as its context so container/Dockerfile's `COPY core/...`,
// `COPY apps/...` and `COPY ui/shared/...` lines all resolve.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root, err := filepath.Abs(filepath.Join(wd, "..", "..", "..", ".."))
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
	// 0o777, not 0o755: container/Dockerfile's runtime image
	// (gcr.io/distroless/static-debian12:nonroot) runs as a fixed
	// non-root uid (65532), which on a real Linux Docker daemon has no
	// write access to a host directory it doesn't own unless "other" has
	// write permission too - t.TempDir()'s own subdirectories are owned
	// by whichever uid runs `go test`, essentially never 65532. This
	// went unnoticed against a macOS Docker Desktop daemon, which does
	// not enforce host bind-mount ownership/permission checks the same
	// way (see docs/deployment.md's "Non-root and the NAS uid/gid"
	// section, which documents this exact leniency directly), and only
	// surfaced running this suite for real on a Linux CI runner:
	// `state: PRAGMA journal_mode = WAL: unable to open database file
	// (14)` - SQLITE_CANTOPEN, from failing to create state.db in a
	// directory the container's own uid cannot write into.
	// container/compose.yaml's real deployments solve the equivalent
	// problem with an explicit PUID/PGID plus an operator pre-chowning
	// the host directory (see that file's own "Non-root and the NAS
	// uid/gid" comment); a throwaway per-test temp directory has no
	// equivalent operator step to do that, so it's world-writable
	// instead - the simplest fix that works regardless of which uid any
	// given container image happens to default to.
	for _, sub := range []string{"remote", "backups", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
		if err := os.Chmod(filepath.Join(dir, sub), 0o777); err != nil {
			// MkdirAll on an already-existing directory does not change
			// its mode, and (unlike Unix) does not necessarily apply the
			// requested mode verbatim on first creation either once a
			// umask is involved - Chmod afterward makes the final mode
			// unconditional rather than depending on either.
			t.Fatalf("Chmod %s: %v", sub, err)
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
		// Full path, not just "daemon": container/Dockerfile deliberately
		// sets no ENTRYPOINT (it now ships two binaries, and a fixed
		// ENTRYPOINT can only ever prefix one of them - see that file's
		// own doc comment), so `command:`/`docker run` args are the whole
		// argv, exactly as container/compose.yaml's own `command:` does.
		image, "/backup-manager", "daemon", "--config", "/etc/backup-manager/config.yaml",
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

	// Asserted BEFORE checking the health value itself, deliberately: a
	// crashed container (main process exited, e.g. because it could not
	// open its own state database - see degradedConfig's own doc for
	// exactly that failure mode, caught here the hard way) can also end
	// up reporting Docker health "unhealthy", which would make this
	// assertion pass for entirely the wrong reason - a false positive
	// that proves nothing about whether HEALTHCHECK actually tracks
	// `backup-manager status`'s exit code. Requiring State.Running is
	// what makes "unhealthy" mean "the DEGRADED backup set was detected
	// by a live container", not "the container is not there to be
	// healthy or not".
	running, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	if got := string(bytes.TrimSpace(running)); got != "true" {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		t.Fatalf("container %s is not running (State.Running=%s), so its health status cannot mean what this test needs it to mean; logs:\n%s", name, got, logs)
	}

	if got != "unhealthy" {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		t.Errorf("container health status = %q, want %q (backup-manager status must exit non-zero for a DEGRADED backup set, and HEALTHCHECK must run status, not version); logs:\n%s", got, "unhealthy", logs)
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

// TestServeCommandExposesTheEngineAPIOnly is the Docker CLI-level
// regression check for the engine half of the two-container split
// (project-owner requirement folded into issue #82/B4.1 before merge):
// `/backup-manager-web serve`, run standalone in a real container exactly
// as `rclone-manager`'s own compose `command` does, exposes the
// versioned API unauthenticated-refused, and serves NO static UI at all
// - that is `web-ui`'s job now (see
// TestComposeStack_WebUIProxiesToTheEngineEndToEnd for the real
// two-container, proxied version of this same proof).
func TestServeCommandExposesTheEngineAPIOnly(t *testing.T) {
	image := buildImage(t)
	dir := degradedConfig(t)
	name := "backup-manager-dockercli-" + t.Name() + "-" + time.Now().Format("150405.000000")

	args := []string{
		"run", "-d", "--name", name,
		"-p", "0:8080", // publish --listen's :8080 to an ephemeral host port
		"-v", filepath.Join(dir, "config.yaml") + ":/etc/backup-manager/config.yaml:ro",
		"-v", filepath.Join(dir, "state") + ":/data/state",
		"-v", filepath.Join(dir, "remote") + ":/data/remote:ro",
		"-v", filepath.Join(dir, "backups") + ":/data/backups",
		image, "/backup-manager-web", "serve", "--config", "/etc/backup-manager/config.yaml", "--listen", ":8080",
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })

	hostPort := publishedPort(t, name, "8080/tcp")
	base := "http://127.0.0.1:" + hostPort

	// The container's own HTTP listener can take a moment to come up
	// after `docker run -d` returns; retry briefly rather than racing it.
	deadline := time.Now().Add(15 * time.Second)
	var resp *http.Response
	for {
		resp, err = http.Get(base + "/api/v1/system/version")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("GET %s/api/v1/system/version never succeeded: %v; logs:\n%s", base, err, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /api/v1/system/version status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	staticResp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET %s/: %v", base, err)
	}
	defer staticResp.Body.Close()
	body, err := io.ReadAll(staticResp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if staticResp.StatusCode == http.StatusOK {
		t.Errorf("GET / on the engine directly status = %d, want NOT 200 (the engine must not serve the static UI - see apps/generic/server.NewEngine); body=%q", staticResp.StatusCode, body)
	}
}

// publishedPort returns the host port Docker assigned container's
// containerPort ("8080/tcp") to, via `docker port`, when it was started
// with `-P` (publish every exposed port to an ephemeral host port).
func publishedPort(t *testing.T, container, containerPort string) string {
	t.Helper()
	out, err := exec.Command("docker", "port", container, containerPort).Output()
	if err != nil {
		t.Fatalf("docker port %s %s: %v", container, containerPort, err)
	}
	// Output looks like "0.0.0.0:54321\n" (and, on some hosts, a second
	// "[::]:54321" line for IPv6) - the port after the last ':' on the
	// first line is what's needed.
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	idx := strings.LastIndex(line, ":")
	if idx < 0 || idx == len(line)-1 {
		t.Fatalf("could not parse host port out of %q", out)
	}
	return line[idx+1:]
}


// composeFile is just enough of container/compose.yaml's shape to check
// which services publish ports - not a general compose-file model.
type composeFile struct {
	Services map[string]struct {
		Ports []any `yaml:"ports"`
	} `yaml:"services"`
}

// TestComposeConfig_EngineHasNoPublishedPortWebUIDoes is a static check
// (no Docker needed) of the actual network-isolation requirement in
// container/compose.yaml: the project-owner requirement folded into this
// issue before merge is that `rclone-manager` (the engine) has NO
// published port at all - reachable only from `web-ui`, over the
// internal Docker network - and `web-ui` is the only service with one.
// Reading the real compose file directly, rather than re-deriving the
// same claim from a live `docker compose up`, is what makes this catch a
// regression in the file itself (a `ports:` line accidentally restored
// on the engine service, say) independent of whether the live end-to-end
// test below happens to still pass despite it.
func TestComposeConfig_EngineHasNoPublishedPortWebUIDoes(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "container", "compose.yaml"))
	if err != nil {
		t.Fatalf("ReadFile compose.yaml: %v", err)
	}

	var cf composeFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("yaml.Unmarshal compose.yaml: %v", err)
	}

	engine, ok := cf.Services["rclone-manager"]
	if !ok {
		t.Fatal(`compose.yaml has no "rclone-manager" service`)
	}
	if len(engine.Ports) != 0 {
		t.Errorf(`services.rclone-manager.ports = %v, want none (the engine must not be reachable from the LAN/host directly)`, engine.Ports)
	}

	ui, ok := cf.Services["web-ui"]
	if !ok {
		t.Fatal(`compose.yaml has no "web-ui" service`)
	}
	if len(ui.Ports) == 0 {
		t.Error(`services.web-ui.ports is empty, want the published LAN-facing port`)
	}
}

// workingRemoteConfig is like degradedConfig, but seeds a real, matching
// artifact into the remote directory before the container ever starts,
// so the very first scheduled cycle finds a genuine, fresh backup and
// `backup-manager status` reports HEALTHY - required here because
// container/compose.yaml's `web-ui` service has
// `depends_on: rclone-manager: condition: service_healthy`, so
// TestComposeStack_WebUIProxiesToTheEngineEndToEnd's stack would never
// finish starting against a permanently-DEGRADED backup set the way
// degradedConfig deliberately produces for the healthcheck tests above.
func workingRemoteConfig(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	for _, sub := range []string{"state", "backups", filepath.Join("backups", "remote"), filepath.Join("backups", "local")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
		if err := os.Chmod(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatalf("Chmod %s: %v", sub, err)
		}
	}
	// remote_path/local_path both live under /data/backups (BACKUP_DIR),
	// NOT a dedicated /data/remote mount: container/compose.yaml has no
	// such volume (a real "sftp" remote lives on the network, never on a
	// host bind mount), so this "local" remote type test has to reuse
	// the one writable directory compose.yaml actually mounts, exactly
	// as manually verified against a real compose-driven stack before
	// writing this test.
	if err := os.WriteFile(filepath.Join(dir, "backups", "remote", "backup.dump"), []byte("compose stack test payload"), 0o666); err != nil {
		t.Fatalf("WriteFile backup.dump: %v", err)
	}

	config := "" +
		"poll_interval: 1h\n" +
		"state:\n" +
		"  database: /data/state/state.db\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: /data/backups/remote\n" +
		"        local_path: /data/backups/local\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile config.yaml: %v", err)
	}
	return dir
}

// composeEnvFile writes an .env file (container/.env.example's own
// shape) into dir, pointed at test fixtures instead of real host paths,
// and returns its path. SSH_KEY_FILE/KNOWN_HOSTS_FILE point at
// config.yaml itself - unused by this suite's "local" remote type, but
// compose.yaml's `${VAR:?...}` guards require SOME readable file to
// exist at each path regardless of remote type.
func composeEnvFile(t *testing.T, dir string, listenPort int) string {
	t.Helper()
	envPath := filepath.Join(dir, ".env")
	content := "STATE_DIR=" + filepath.Join(dir, "state") + "\n" +
		"BACKUP_DIR=" + filepath.Join(dir, "backups") + "\n" +
		"CONFIG_FILE=" + filepath.Join(dir, "config.yaml") + "\n" +
		"SSH_KEY_FILE=" + filepath.Join(dir, "config.yaml") + "\n" +
		"KNOWN_HOSTS_FILE=" + filepath.Join(dir, "config.yaml") + "\n" +
		"LISTEN_PORT=" + strconv.Itoa(listenPort) + "\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile .env: %v", err)
	}
	return envPath
}

// composeProject drives `docker compose` for one test's whole stack
// lifecycle: `up -d` in the constructor, `down -v` registered via
// t.Cleanup, and a couple of convenience methods tests actually call.
type composeProject struct {
	name    string
	envFile string
}

func startComposeStack(t *testing.T, image string, listenPort int) *composeProject {
	t.Helper()
	root := repoRoot(t)
	dir := workingRemoteConfig(t)
	envFile := composeEnvFile(t, dir, listenPort)

	p := &composeProject{
		name:    "backup-manager-dockercli-" + sanitizeProjectName(t.Name()),
		envFile: envFile,
	}

	composeFilePath := filepath.Join(root, "container", "compose.yaml")

	// Pin both services to the already-built image (see buildImage) so
	// this test proves compose.yaml's OWN topology/network/healthcheck
	// wiring, not a second build of the same Dockerfile compose would
	// otherwise trigger on its own.
	up := exec.Command("docker", "compose",
		"--env-file", envFile,
		"-f", composeFilePath,
		"-p", p.name,
		"up", "-d",
		"--no-build",
	)
	up.Env = append(os.Environ(), "VERSION=dockercli-test")
	// Compose resolves `image: backup-manager:${VERSION:-dev}` against
	// VERSION; buildImage's own tag is "backup-manager:dockercli-test",
	// so VERSION has to match that tag exactly for `--no-build` to find
	// it rather than trying (and failing, with no `build:` context error)
	// to build a fresh one under a name nothing already built.
	if !strings.HasSuffix(image, ":dockercli-test") {
		t.Fatalf("startComposeStack assumes buildImage's own tag ends in \":dockercli-test\", got %q", image)
	}

	var out bytes.Buffer
	up.Stdout = &out
	up.Stderr = &out
	if err := up.Run(); err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, out.String())
	}

	t.Cleanup(func() {
		down := exec.Command("docker", "compose",
			"--env-file", envFile,
			"-f", composeFilePath,
			"-p", p.name,
			"down", "-v", "--remove-orphans",
		)
		down.Env = up.Env
		down.Run()
	})

	return p
}

func sanitizeProjectName(name string) string {
	r := strings.NewReplacer("/", "-", " ", "-", "_", "-")
	return strings.ToLower(r.Replace(name)) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func (p *composeProject) containerID(t *testing.T, service string) string {
	t.Helper()
	root := repoRoot(t)
	out, err := exec.Command("docker", "compose",
		"--env-file", p.envFile,
		"-f", filepath.Join(root, "container", "compose.yaml"),
		"-p", p.name,
		"ps", "-q", service,
	).Output()
	if err != nil {
		t.Fatalf("docker compose ps -q %s: %v", service, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		t.Fatalf("docker compose ps -q %s returned no container", service)
	}
	return id
}

func (p *composeProject) publishedPort(t *testing.T, service, containerPort string) string {
	t.Helper()
	return publishedPort(t, p.containerID(t, service), containerPort)
}

// TestComposeStack_WebUIProxiesToTheEngineEndToEnd is this issue's live
// proof of the two-container split, driven through `docker compose`
// exactly as an operator would (not a hand-assembled `docker run`
// replicating the same topology): brings up BOTH services from
// container/compose.yaml, waits for web-ui's dependency-gated startup
// (it will not even start until rclone-manager reports healthy - see
// compose.yaml's own `depends_on: condition: service_healthy`), enrolls
// and logs in entirely through web-ui's published port, confirms an
// authenticated request proxies through to the real engine and
// succeeds, and confirms the engine itself has no port reachable
// directly from the host at all.
func TestComposeStack_WebUIProxiesToTheEngineEndToEnd(t *testing.T) {
	image := buildImage(t)
	// buildImage's own tag doesn't carry a "dockercli-test" VERSION build
	// stamp the way compose's `image:` resolution needs; retag it so
	// compose's `image: backup-manager:${VERSION:-dev}` (VERSION=
	// dockercli-test, set in startComposeStack) resolves to the exact
	// image buildImage already built, instead of compose trying to build
	// a second one under a tag nothing produced.
	retag := exec.Command("docker", "tag", image, "backup-manager:dockercli-test")
	if out, err := retag.CombinedOutput(); err != nil {
		t.Fatalf("docker tag: %v\n%s", err, out)
	}

	project := startComposeStack(t, image, 0)

	hostPort := project.publishedPort(t, "web-ui", "8080/tcp")
	base := "http://127.0.0.1:" + hostPort

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}

	// Seed the CSRF cookie exactly as a browser's first page load would -
	// served entirely by web-ui's own static handler, never touching the
	// engine.
	seedResp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET %s/: %v", base, err)
	}
	seedResp.Body.Close()
	if seedResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/ status = %d, want %d (web-ui's own static shell)", base, seedResp.StatusCode, http.StatusOK)
	}

	baseURL, err := url.Parse(base)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	var csrf string
	for _, c := range jar.Cookies(baseURL) {
		if c.Name == "bm_csrf" {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("no bm_csrf cookie present after seeding GET / through web-ui")
	}

	engineID := project.containerID(t, "rclone-manager")
	logs, err := exec.Command("docker", "logs", engineID).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs %s: %v", engineID, err)
	}
	token := bootstrapTokenFromLogs(t, string(logs))

	enrollBody := strings.NewReader(`{"username":"bm-admin","password":"correct-horse-battery"}`)
	enrollReq, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/enroll", enrollBody)
	if err != nil {
		t.Fatalf("NewRequest enroll: %v", err)
	}
	enrollReq.Header.Set("Content-Type", "application/json")
	enrollReq.Header.Set("X-CSRF-Token", csrf)
	enrollReq.Header.Set("X-Bootstrap-Token", token)
	enrollResp, err := client.Do(enrollReq)
	if err != nil {
		t.Fatalf("POST %s/api/v1/auth/enroll (via web-ui proxy): %v", base, err)
	}
	enrollBodyBytes, _ := io.ReadAll(enrollResp.Body)
	enrollResp.Body.Close()
	if enrollResp.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll via web-ui proxy status = %d, want %d; body=%s", enrollResp.StatusCode, http.StatusNoContent, enrollBodyBytes)
	}

	versionResp, err := client.Get(base + "/api/v1/system/version")
	if err != nil {
		t.Fatalf("GET %s/api/v1/system/version (via web-ui proxy): %v", base, err)
	}
	versionBody, _ := io.ReadAll(versionResp.Body)
	versionResp.Body.Close()
	if versionResp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /api/v1/system/version via web-ui proxy status = %d, want %d; body=%s", versionResp.StatusCode, http.StatusOK, versionBody)
	}
	if !strings.Contains(string(versionBody), "api_version") {
		t.Errorf("proxied response body = %q, want the engine's real /api/v1/system/version JSON", versionBody)
	}

	// The actual network-isolation claim, proven live rather than only
	// statically (TestComposeConfig_EngineHasNoPublishedPortWebUIDoes
	// already proves the compose file itself declares no port; this
	// proves Docker actually honored that): the engine container has no
	// port Docker will report as published.
	portOut, portErr := exec.Command("docker", "port", engineID).CombinedOutput()
	if portErr == nil && strings.TrimSpace(string(portOut)) != "" {
		t.Errorf("docker port %s = %q, want no published ports at all", engineID, portOut)
	}
}

// bootstrapTokenFromLogs extracts the single-use enrollment token from a
// real container log line - the same string an operator reads to open
// the enrollment link, not a test-only shortcut.
func bootstrapTokenFromLogs(t *testing.T, logs string) string {
	t.Helper()
	const marker = "token="
	i := strings.Index(logs, marker)
	if i < 0 {
		t.Fatalf("no bootstrap token found in engine logs: %q", logs)
	}
	rest := logs[i+len(marker):]
	fields := strings.FieldsFunc(rest, func(r rune) bool { return r == ' ' || r == '\n' || r == '\r' })
	if len(fields) == 0 {
		t.Fatalf("could not parse token out of engine logs: %q", logs)
	}
	return fields[0]
}

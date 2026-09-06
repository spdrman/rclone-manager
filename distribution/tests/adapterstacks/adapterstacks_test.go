// Package adapterstacks_test brings every derived runtime definition up
// on a real fresh install and requires its web UI to serve.
//
// # Why this is the evidence issue #206 needed
//
// Every adapter gates its web UI on `depends_on: <engine>: condition:
// service_healthy`, and every adapter derived the engine's health check
// from canonical.json, which said `backup-manager status`. That is FR-24's
// backup-freshness verdict, and it exits non-zero on a fresh install by
// design, because a fresh install has backed nothing up. So on every
// adapter the one container an operator installed the app to reach never
// started, and nothing in the tree noticed, because no suite ever ran a
// stack that had not already been made healthy first.
//
// The canonical definition had been fixed and the adapters had not. A
// test of the canonical stack is therefore not evidence about the
// adapters, which is why this exists as well as
// apps/generic/tests/dockercli's own fresh-install pair.
//
// # Why it lives in the distribution module
//
// It reads apps/<platform>/compose/*, which is the distribution ADAPTER
// tree, and #81's dependency rule says core/, apps/common and apps/generic
// must build and pass their tests with that tree deleted entirely
// (scripts/architecture/verify-core-without-distribution.sh proves it by
// deleting exactly those paths). A test in apps/generic that reads an
// adapter file breaks that rule however carefully it degrades, so the
// adapter half of the fresh-install evidence belongs to the layer that
// owns the adapters. The canonical image and the canonical Compose
// definition are distribution-layer artifacts too, so building one here
// adds no dependency this layer did not already have.
//
// # What is substituted, and what is not
//
// Three things, all of them the operator's own input rather than the
// runtime's: the image reference (an adapter names the published one and
// this run has to test the image it just built), the HOST side of every
// mount (a NAS pool path does not exist on a test machine) and the host
// side of the published port (so concurrent runs do not collide). The
// TrueNAS catalog template's `{{ }}` expressions are rendered from a table
// for the same reason, and an expression the table does not know fails the
// test rather than being left in.
//
// Everything that decides whether the web UI starts is the adapter's own:
// both commands, both health checks, the dependency condition, the
// container-side mounts, the read-only rootfs, the dropped capabilities
// and the user.
//
// Skipped automatically wherever the `docker` CLI or daemon is not
// available, exactly as apps/generic/tests/dockercli is.
package adapterstacks_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/distribution/compose"
)

// ---------------------------------------------------------------------
// The image
// ---------------------------------------------------------------------

// repoRoot is three levels up from this package
// (distribution/tests/adapterstacks), which is the build context
// container/Dockerfile's `COPY core/...`, `COPY apps/...` and
// `COPY ui/shared/...` lines all resolve against.
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

// requireDocker decides whether an absent docker takes this suite out of
// the run quietly or loudly. Both arms are docker-availability checks, so
// both go through dockergate_test.go's verdict (#456): a skip on a laptop
// with no docker, and an INFRA: failure inside the gate, where docker is a
// declared prerequisite and skipping would empty this suite while the gate
// still printed ok.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		dockerUnavailable(t, "%q not available on PATH: %v", "docker", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		dockerUnavailable(t, "docker daemon not reachable: %v", err)
	}
}

// imageReference is unique to this test process, never a fixed name.
//
// A shared tag is what issue #185 was: a second checkout building at the
// same time takes the name over, and this run then quietly tests that
// checkout's image instead of its own. The tag carries the pid and the
// process start time so two runs on one machine cannot collide either.
var imageReference = "backup-manager:adapterstacks-" +
	strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)

var build struct {
	once  sync.Once
	err   error
	log   string
	built bool
}

// buildImage builds container/Dockerfile once per test process and hands
// every caller the same outcome, success or failure. An exit from the
// closure that never reaches the assignment leaves the error in place, so
// an abandoned build cannot read as a successful one.
func buildImage(t *testing.T) string {
	t.Helper()
	requireDocker(t)
	root := repoRoot(t)

	build.once.Do(func() {
		build.err = errBuildAbandoned
		cmd := exec.Command("docker", "build",
			"-f", filepath.Join(root, "container", "Dockerfile"),
			"-t", imageReference,
			"--label", "com.rclone-manager.test=adapterstacks",
			"--load",
			root,
		)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		build.log, build.err = out.String(), err
		build.built = err == nil
	})
	if build.err != nil {
		t.Fatalf("docker build failed: %v\n%s", build.err, build.log)
	}
	return imageReference
}

// errBuildAbandoned is what a build that never recorded an outcome
// reports. Without it a t.Fatalf out of the closure would leave the error
// at its zero value, which reads as success, and every later caller would
// run against an image that does not exist.
var errBuildAbandoned = errAbandoned{}

type errAbandoned struct{}

func (errAbandoned) Error() string {
	return "the one-shot image build exited before recording an outcome, so nothing was built"
}

func TestMain(m *testing.M) {
	code := m.Run()
	if build.built {
		// This run's own tag, and only ever this run's: see
		// imageReference for why nothing here may touch a shared name.
		_ = exec.Command("docker", "image", "rm", "-f", imageReference).Run()
	}
	os.Exit(code)
}

// ---------------------------------------------------------------------
// The fresh install
// ---------------------------------------------------------------------

// freshInstall lays out the host side of a fresh install, literally: an
// EMPTY configuration directory with no config.yaml in it, an empty state
// directory and an empty backup directory.
//
// A bind mount cannot express "not there yet" for a file, since Docker
// creates a directory at a source path that does not exist. That is why
// the configuration mount is a directory at all (issue #196), and an
// empty one is the only honest shape for an install nobody has
// configured. The engine serves the first-run setup flow from it, and
// `backup-manager status` exits non-zero in it, which every test below
// reads back rather than assumes.
func freshInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// 0o777 because the runtime image runs as a fixed non-root uid that
	// does not own a t.TempDir() subdirectory. On a Linux daemon that is a
	// hard permission failure at the first write; a macOS Docker Desktop
	// daemon is lenient about it, which is exactly how a fixture like this
	// passes locally and fails on a real runner.
	for _, sub := range []string{"state", "backups", "config"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
		if err := os.Chmod(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatalf("Chmod %s: %v", sub, err)
		}
	}
	return dir
}

// freshInstallHostPaths lays a fixture directory out as the host side of
// the five canonical storage roles, keyed by the CONTAINER path an
// adapter mounts them at.
//
// Keyed by the container side because that is the half the binaries fix
// and every adapter therefore agrees on. The host side is exactly what
// this rewrite replaces.
func freshInstallHostPaths(t *testing.T, dir string) map[string]string {
	t.Helper()
	keyFile := filepath.Join(dir, "id_ed25519")
	knownHosts := filepath.Join(dir, "known_hosts")
	for _, f := range []string{keyFile, knownHosts} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", f, err)
		}
	}
	return map[string]string{
		"/data/state":                     filepath.Join(dir, "state"),
		"/data/backups":                   filepath.Join(dir, "backups"),
		"/etc/backup-manager/config":      filepath.Join(dir, "config"),
		"/etc/backup-manager/id_ed25519":  keyFile,
		"/etc/backup-manager/known_hosts": knownHosts,
	}
}

// ---------------------------------------------------------------------
// Rewriting one adapter's runtime definition
// ---------------------------------------------------------------------

// templateValues renders the TrueNAS catalog template's expressions. Only
// the ones the structural rewrite below cannot reach are here: the storage
// host paths, the image and the web port never get this far.
var templateValues = map[string]string{
	".Values.runtime.puid":     "1000",
	".Values.runtime.pgid":     "100",
	".Values.runtime.timezone": "UTC",
	".Values.network.webPort":  "8080",
}

var templateExpr = regexp.MustCompile(`\{\{([^}]*)\}\}`)

// renderTemplateExpressions substitutes a straight-line catalog template's
// expressions. An expression the table does not know fails the test:
// leaving it in produces a file that still parses as YAML and deploys
// something nobody chose, which is precisely what the catalog template's
// own header comment is written against.
func renderTemplateExpressions(t *testing.T, raw []byte, rel, image string) []byte {
	t.Helper()
	values := map[string]string{".Values.image.reference": image}
	for k, v := range templateValues {
		values[k] = v
	}

	var unknown []string
	out := templateExpr.ReplaceAllFunc(raw, func(m []byte) []byte {
		expr := strings.TrimSpace(string(templateExpr.FindSubmatch(m)[1]))
		if v, ok := values[expr]; ok {
			return []byte(v)
		}
		if strings.HasPrefix(expr, ".Values.storage.") {
			// Replaced structurally below, so any placeholder will do.
			return []byte("/replaced-by-the-mount-rewrite")
		}
		unknown = append(unknown, expr)
		return m
	})
	if len(unknown) > 0 {
		t.Fatalf("%s uses template expressions this test has no value for: %v; rendering it with them left in would deploy something nobody chose", rel, unknown)
	}
	return out
}

// splitMountSpec splits HOST:CONTAINER[:MODE] without splitting inside a
// ${...} reference.
//
// It has to. Compose's fail-closed form is written
// ${STATE_DIR:?set STATE_DIR ...}, and three of the five shipped adapters
// use it for every host path. A plain strings.Split on ":" reads the text
// after the first colon INSIDE the variable as the container path, which
// is how a rewrite like this one silently produces a mount nobody wrote.
func splitMountSpec(spec string) []string {
	var parts []string
	var current strings.Builder
	depth := 0
	for i := 0; i < len(spec); i++ {
		switch {
		case strings.HasPrefix(spec[i:], "${"):
			depth++
			current.WriteString("${")
			i++
		case spec[i] == '}' && depth > 0:
			depth--
			current.WriteByte('}')
		case spec[i] == ':' && depth == 0:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(spec[i])
		}
	}
	return append(parts, current.String())
}

// rewriteAdapterCompose reads one derived runtime definition, replaces the
// three operator-supplied things this package's own doc names, and writes
// the result next to the fixture. It returns the path to run.
func rewriteAdapterCompose(t *testing.T, rel, image, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	raw = renderTemplateExpressions(t, raw, rel, image)

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 {
		t.Fatalf("%s declares no services", rel)
	}

	hostPaths := freshInstallHostPaths(t, dir)
	mounts, ports := 0, 0
	for _, name := range sortedServiceNames(services) {
		svc, ok := services[name].(map[string]any)
		if !ok {
			t.Fatalf("%s: service %q is not a mapping", rel, name)
		}
		svc["image"] = image

		if vols, ok := svc["volumes"].([]any); ok {
			for i, rawVol := range vols {
				spec, ok := rawVol.(string)
				if !ok {
					t.Fatalf("%s: service %q declares a mount this rewrite cannot read: %v", rel, name, rawVol)
				}
				parts := splitMountSpec(spec)
				if len(parts) < 2 {
					t.Fatalf("%s: service %q declares mount %q with no container side", rel, name, spec)
				}
				host, known := hostPaths[parts[1]]
				if !known {
					t.Fatalf("%s: service %q mounts %s, which is not one of the canonical storage roles this fixture provides (%v); the container side is the half the binaries fix, so a new one is a finding rather than something to substitute",
						rel, name, parts[1], sortedKeys(hostPaths))
				}
				parts[0] = host
				vols[i] = strings.Join(parts, ":")
				mounts++
			}
		}

		if declared, ok := svc["ports"].([]any); ok {
			for i, rawPort := range declared {
				spec, ok := rawPort.(string)
				if !ok {
					t.Fatalf("%s: service %q declares a port this rewrite cannot read: %v", rel, name, rawPort)
				}
				parts := strings.Split(spec, ":")
				// The container side is the adapter's and it stays. The
				// host side becomes 0 so concurrent runs cannot collide.
				declared[i] = "0:" + parts[len(parts)-1]
				ports++
			}
		}
	}
	if mounts == 0 {
		t.Fatalf("%s: no mount was rewritten, so this stack would start against host paths that do not exist here", rel)
	}
	if ports != 1 {
		t.Fatalf("%s: %d published ports were rewritten, want exactly 1 (the web UI's); the engine must publish none", rel, ports)
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal %s: %v", rel, err)
	}
	path := filepath.Join(dir, strings.NewReplacer("/", "-").Replace(rel))
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}

func sortedServiceNames(services map[string]any) []string {
	out := make([]string, 0, len(services))
	for k := range services {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------
// Driving one stack
// ---------------------------------------------------------------------

// projectNameUnsafe is every character compose refuses in a project name.
// A subtest named after a file path carries dots straight into `-p`, which
// fails the whole invocation with a message about the name rather than
// about the stack.
var projectNameUnsafe = regexp.MustCompile(`[^a-z0-9_-]+`)

type stack struct {
	name string
	file string
}

func (s *stack) args(rest ...string) []string {
	return append([]string{"compose", "-f", s.file, "-p", s.name}, rest...)
}

// up brings the stack up and registers its teardown BEFORE running, so a
// stack that fails its dependency gate still leaves nothing behind: the
// engine's container and the project network exist either way.
func up(t *testing.T, file string) (*stack, string, error) {
	t.Helper()
	s := &stack{
		name: strings.Trim(projectNameUnsafe.ReplaceAllString(strings.ToLower(t.Name()), "-"), "-") +
			"-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		file: file,
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", s.args("down", "-v", "--remove-orphans")...).Run()
	})

	cmd := exec.Command("docker", s.args("up", "-d", "--no-build")...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	return s, out.String(), cmd.Run()
}

func (s *stack) containerIDIfAny(t *testing.T, service string) string {
	t.Helper()
	cmd := exec.Command("docker", s.args("ps", "-a", "-q", service)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose ps -q %s: %v\n%s", service, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func (s *stack) containerID(t *testing.T, service string) string {
	t.Helper()
	id := s.containerIDIfAny(t, service)
	if id == "" {
		t.Fatalf("docker compose ps -q %s returned no container", service)
	}
	return id
}

type inspectEntry struct {
	State struct {
		Status string `json:"Status"`
		Health struct {
			Status string `json:"Status"`
			Log    []struct {
				Output string `json:"Output"`
			} `json:"Log"`
		} `json:"Health"`
	} `json:"State"`
}

func inspect(t *testing.T, id string) inspectEntry {
	t.Helper()
	out, err := exec.Command("docker", "inspect", id).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", id, err)
	}
	var entries []inspectEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("unmarshal docker inspect: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("docker inspect returned %d entries, want 1", len(entries))
	}
	return entries[0]
}

// healthStatus polls until the container reaches a settled health verdict
// or the timeout expires, returning whatever it last saw. Docker's health
// state machine has a transient "starting" phase, so a single inspect
// would be flaky by construction.
func healthStatus(t *testing.T, id string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = inspect(t, id).State.Health.Status
		if last == "healthy" || last == "unhealthy" {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}

// lastHealthOutput is what the container's most recent probe printed, so a
// failure names what the check actually said rather than only that it was
// unhappy.
func lastHealthOutput(t *testing.T, id string) string {
	t.Helper()
	log := inspect(t, id).State.Health.Log
	if len(log) == 0 {
		return ""
	}
	return strings.TrimSpace(log[len(log)-1].Output)
}

func containerState(t *testing.T, id string) string {
	t.Helper()
	return inspect(t, id).State.Status
}

// statusExitCode runs `backup-manager status` inside a running container.
//
// This is the control the whole file turns on: without it a green run
// proves only that some stack came up, and a fixture that had quietly
// become healthy would pass while saying nothing at all about the defect.
func statusExitCode(t *testing.T, id string) (int, string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", id, "/backup-manager", "status").CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("docker exec %s /backup-manager status: %v\n%s", id, err, out)
	}
	return exit.ExitCode(), string(out)
}

func publishedPort(t *testing.T, id, containerPort string) string {
	t.Helper()
	out, err := exec.Command("docker", "port", id, containerPort).Output()
	if err != nil {
		t.Fatalf("docker port %s %s: %v", id, containerPort, err)
	}
	first := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	i := strings.LastIndex(first, ":")
	if i < 0 {
		t.Fatalf("docker port %s %s returned %q, which has no host port", id, containerPort, first)
	}
	return first[i+1:]
}

// getWithTransportRetry retries only the transport error, never a
// response: whatever finally answers still has to answer with the status
// the caller requires. Docker's published-port proxy accepts the TCP
// connection from the moment the port exists and resets it until the
// process behind it is listening, which is a race and not a verdict.
func getWithTransportRetry(t *testing.T, url string, timeout time.Duration) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
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

// serviceRunning names a rewritten definition's service by the COMMAND it
// runs, never by what it is called: the adapters call them
// backup-manager/backup-manager-ui and the canonical definition calls them
// rclone-manager/web-ui, so a lookup keyed on the name would stop finding
// them the moment one was renamed.
func serviceRunning(t *testing.T, file, subcommand string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var doc struct {
		Services map[string]struct {
			Command []string `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var found []string
	for name, svc := range doc.Services {
		for _, arg := range svc.Command {
			if arg == subcommand {
				found = append(found, name)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s declares %d services running %q (%v), want exactly 1", file, len(found), subcommand, found)
	}
	return found[0]
}

// ---------------------------------------------------------------------
// The claim
// ---------------------------------------------------------------------

// TestEveryDerivedAdapterBringsUpTheWebUIOnAFreshInstall is issue #206's
// acceptance criterion across the adapters: no backup has ever run, so the
// engine's backup-freshness verdict is negative on every one of them, and
// every one of them still has to bring up the container the operator
// installed the app to reach.
//
// The list comes out of the runtime contract's own `derived` array rather
// than being typed here, so an adapter added there is covered on the same
// commit and one that is missing is a finding the contract's completeness
// guard already raises.
func TestEveryDerivedAdapterBringsUpTheWebUIOnAFreshInstall(t *testing.T) {
	image := buildImage(t)

	artifacts := compose.MustLoadContract().Derived
	if len(artifacts) == 0 {
		t.Fatal("the runtime contract lists no derived artifact, so this suite would bring up nothing and report success")
	}

	for _, rel := range artifacts {
		t.Run(rel, func(t *testing.T) {
			dir := freshInstall(t)
			file := rewriteAdapterCompose(t, rel, image, dir)

			project, out, err := up(t, file)
			if err != nil {
				t.Errorf("%s does not come up on a fresh install: %v\n%s", rel, err, out)
				engine := project.containerIDIfAny(t, serviceRunning(t, file, "serve"))
				if engine == "" {
					t.FailNow()
				}
				logs, _ := exec.Command("docker", "logs", engine).CombinedOutput()
				t.Fatalf("the engine's last health probe said %q; engine logs:\n%s", lastHealthOutput(t, engine), logs)
			}

			engineName := serviceRunning(t, file, "serve")
			uiName := serviceRunning(t, file, "serve-ui")

			engineID := project.containerID(t, engineName)
			if got := healthStatus(t, engineID, 90*time.Second); got != "healthy" {
				t.Fatalf("%s: the engine's container health is %q on a fresh install, want %q; the web UI waits on it, so anything else means it never starts. The probe's last output was %q",
					rel, got, "healthy", lastHealthOutput(t, engineID))
			}

			// The control, per adapter: this fresh install really is one
			// the backup-freshness verdict refuses, so the web UI came up
			// THROUGH a negative verdict rather than because the fixture
			// happened to be healthy.
			code, statusOut := statusExitCode(t, engineID)
			if code == 0 {
				t.Fatalf("%s: `backup-manager status` exited 0 inside this fixture, so it is not the fresh install this test claims to run:\n%s", rel, statusOut)
			}

			uiID := project.containerID(t, uiName)
			if got := containerState(t, uiID); got != "running" {
				logs, _ := exec.Command("docker", "logs", uiID).CombinedOutput()
				t.Fatalf("%s: the web UI container is %q, want %q; logs:\n%s", rel, got, "running", logs)
			}

			base := "http://127.0.0.1:" + publishedPort(t, uiID, "8080/tcp")
			resp := getWithTransportRetry(t, base+"/", 30*time.Second)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: GET %s/ on a fresh install status = %d, want %d (the web UI's own static shell, which is what the first-run flow is served from)",
					rel, base, resp.StatusCode, http.StatusOK)
			}

			// The proxy half, which is what makes it a working page rather
			// than a container that merely started: /health/ is forwarded
			// to the engine unchanged, so a 200 here is the engine
			// answering through the UI container on a fresh install.
			live := getWithTransportRetry(t, base+"/health/live", 30*time.Second)
			live.Body.Close()
			if live.StatusCode != http.StatusOK {
				t.Errorf("%s: GET %s/health/live (proxied to the engine) status = %d, want %d", rel, base, live.StatusCode, http.StatusOK)
			}
		})
	}
}

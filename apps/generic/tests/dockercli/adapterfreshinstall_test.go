// This file is the other half of issue #206's evidence: the canonical
// stack coming up on a fresh install says nothing about the NINE
// adapters, and the adapters are where the defect actually shipped. The
// canonical definition was fixed by #167; every adapter went on deriving
// the health verdict it replaced.
//
// So every derived runtime definition is brought up here, for real,
// against the real image, on a fresh install, and its web UI has to
// serve. The list is read from the runtime contract's own `derived`
// array rather than typed out, because a hard-coded list is how an
// adapter stops being covered without anybody deciding that it should.
//
// # What is substituted, and what is not
//
// Three things, all of them the operator's own input rather than the
// runtime's: the image reference (an adapter names the published one and
// this run has to test the image it just built), the HOST side of every
// mount (a NAS pool path does not exist on a test machine) and the host
// side of the published port (so runs do not collide). The catalog
// template's `{{ }}` expressions are substituted from a table for the
// same reason, and an expression the table does not know fails the test
// rather than being left in.
//
// Everything that decides whether the web UI starts is the adapter's
// own: both commands, both health checks, the dependency condition, the
// container-side mounts, the read-only rootfs, the dropped capabilities
// and the user.

package dockercli_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// derivedRuntimeArtifacts is every adapter runtime definition the
// contract says derives from the canonical one.
//
// Read from distribution/compose/runtime-contract.json, never typed out
// here: that file is what the derivation and prohibition gates already
// iterate, so an adapter added there is covered here on the same commit,
// and one that is not there is a finding those gates raise rather than a
// silent gap in this one.
func derivedRuntimeArtifacts(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "distribution", "compose", "runtime-contract.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the runtime contract: %v", err)
	}
	var contract struct {
		Derived []string `json:"derived"`
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatalf("parse the runtime contract: %v", err)
	}
	if len(contract.Derived) == 0 {
		t.Fatal("the runtime contract lists no derived artifact, so this suite would bring up nothing and report success")
	}
	return contract.Derived
}

// templateValues is what a catalog template's `{{ }}` expressions are
// rendered with. Only the ones this test cannot route around are here:
// the storage host paths and the web port are rewritten structurally
// below, so they never reach the table.
var templateValues = map[string]string{
	".Values.runtime.puid":     "1000",
	".Values.runtime.pgid":     "100",
	".Values.runtime.timezone": "UTC",
	".Values.network.webPort":  "8080",
	".Values.image.reference":  "", // filled in per run: the image this run built
}

var templateExpr = regexp.MustCompile(`\{\{([^}]*)\}\}`)

// renderTemplateExpressions substitutes a straight-line catalog
// template's expressions. An expression the table does not know fails
// the test: leaving it in would produce a file that still parses as YAML
// and deploys something nobody chose, which is exactly the failure mode
// the catalog template's own header comment is written against.
func renderTemplateExpressions(t *testing.T, raw []byte, rel, image string) []byte {
	t.Helper()
	values := map[string]string{}
	for k, v := range templateValues {
		values[k] = v
	}
	values[".Values.image.reference"] = image

	var unknown []string
	out := templateExpr.ReplaceAllFunc(raw, func(m []byte) []byte {
		expr := strings.TrimSpace(string(templateExpr.FindSubmatch(m)[1]))
		if v, ok := values[expr]; ok {
			return []byte(v)
		}
		if strings.HasPrefix(expr, ".Values.storage.") {
			// Rewritten structurally below, so any placeholder will do.
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

// freshInstallHostPaths lays out one fixture directory as the host side
// of the five canonical storage roles, keyed by the CONTAINER path the
// adapter mounts them at. Keyed by the container side because that is
// the half the binaries fix and every adapter therefore agrees on; the
// host side is exactly what this rewrite is replacing.
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

// rewriteAdapterCompose reads one derived runtime definition, replaces
// the three operator-supplied things named in this file's header, and
// writes the result next to the fixture. It returns the path to run.
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
	rewrittenMounts := 0
	rewrittenPorts := 0
	for name, rawSvc := range services {
		svc, ok := rawSvc.(map[string]any)
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
				container := parts[1]
				host, known := hostPaths[container]
				if !known {
					t.Fatalf("%s: service %q mounts %s, which is not one of the canonical storage roles this fixture provides (%v); the container side is the half the binaries fix, so a new one is a finding rather than something to substitute", rel, name, container, sortedKeys(hostPaths))
				}
				parts[0] = host
				vols[i] = strings.Join(parts, ":")
				rewrittenMounts++
			}
		}

		if ports, ok := svc["ports"].([]any); ok {
			for i, rawPort := range ports {
				spec, ok := rawPort.(string)
				if !ok {
					t.Fatalf("%s: service %q declares a port this rewrite cannot read: %v", rel, name, rawPort)
				}
				parts := strings.Split(spec, ":")
				// The container side is the adapter's, and it stays. The
				// host side becomes 0 so concurrent runs cannot collide
				// on it.
				ports[i] = "0:" + parts[len(parts)-1]
				rewrittenPorts++
			}
		}
	}
	if rewrittenMounts == 0 {
		t.Fatalf("%s: no mount was rewritten, so this stack would be started against host paths that do not exist here", rel)
	}
	if rewrittenPorts != 1 {
		t.Fatalf("%s: %d published ports were rewritten, want exactly 1 (the web UI's); the engine must publish none", rel, rewrittenPorts)
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

// splitMountSpec splits HOST:CONTAINER[:MODE] without splitting inside a
// ${...} reference.
//
// It has to: compose's fail-closed form is written ${STATE_DIR:?set
// STATE_DIR ...}, and three of the five shipped adapters use it for
// every host path. A plain strings.Split on ":" reads the text after the
// first colon INSIDE the variable as the container path, which is how a
// rewrite like this one silently produces a mount nobody wrote.
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
	parts = append(parts, current.String())
	return parts
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestEveryDerivedAdapterBringsUpTheWebUIOnAFreshInstall is issue #206's
// acceptance criterion across the adapters rather than only the
// canonical definition: no backup has ever run, so the engine's
// backup-freshness verdict is negative on every one of them, and every
// one of them still has to bring up the container the operator installed
// the app to reach.
func TestEveryDerivedAdapterBringsUpTheWebUIOnAFreshInstall(t *testing.T) {
	image := buildImage(t)
	artifacts := derivedRuntimeArtifacts(t)

	for _, rel := range artifacts {
		t.Run(rel, func(t *testing.T) {
			dir := freshInstallConfig(t)
			file := rewriteAdapterCompose(t, rel, image, dir)

			project, out, err := upComposeFiles(t, image, 0, dir, []string{file})
			if err != nil {
				t.Errorf("%s does not come up on a fresh install: %v\n%s", rel, err, out)
				engine := project.containerIDIfAny(t, engineServiceOf(t, file))
				if engine == "" {
					t.FailNow()
				}
				logs, _ := exec.Command("docker", "logs", engine).CombinedOutput()
				t.Fatalf("the engine's last health probe said %q; engine logs:\n%s", lastHealthOutput(t, engine), logs)
			}

			engineName := engineServiceOf(t, file)
			uiName := webUIServiceOf(t, file)

			engineID := project.containerID(t, engineName)
			if got := healthStatus(t, engineID, 90*time.Second); got != "healthy" {
				t.Fatalf("%s: the engine's container health is %q on a fresh install, want %q; the web UI waits on it. The probe's last output was %q", rel, got, "healthy", lastHealthOutput(t, engineID))
			}

			// The control, per adapter: this fresh install really is one
			// the backup-freshness verdict refuses.
			code, statusOut := statusExitCode(t, engineID)
			if code == 0 {
				t.Fatalf("%s: `backup-manager status` exited 0 inside this fixture, so it is not the fresh install this test claims to run:\n%s", rel, statusOut)
			}

			uiID := project.containerID(t, uiName)
			if got := containerState(t, uiID); got != "running" {
				logs, _ := exec.Command("docker", "logs", uiID).CombinedOutput()
				t.Fatalf("%s: the web UI container is %q, want %q; logs:\n%s", rel, got, "running", logs)
			}

			base := "http://127.0.0.1:" + project.publishedPort(t, uiName, "8080/tcp")
			client := &http.Client{Timeout: 10 * time.Second}
			resp := getWithTransportRetry(t, client, base+"/", 30*time.Second)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: GET %s/ on a fresh install status = %d, want %d", rel, base, resp.StatusCode, http.StatusOK)
			}
			live := getWithTransportRetry(t, client, base+"/health/live", 30*time.Second)
			live.Body.Close()
			if live.StatusCode != http.StatusOK {
				t.Errorf("%s: GET %s/health/live (proxied to the engine) status = %d, want %d", rel, base, live.StatusCode, http.StatusOK)
			}
		})
	}
}

// engineServiceOf and webUIServiceOf name a rewritten definition's two
// services by the COMMAND each one runs, never by what it is called: the
// adapters call them backup-manager/backup-manager-ui and the canonical
// definition calls them rclone-manager/web-ui, and a lookup keyed on the
// name would stop finding them the moment one was renamed.
func engineServiceOf(t *testing.T, file string) string {
	return serviceRunning(t, file, "serve")
}

func webUIServiceOf(t *testing.T, file string) string {
	return serviceRunning(t, file, "serve-ui")
}

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

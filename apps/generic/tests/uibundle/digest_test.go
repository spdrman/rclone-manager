// Package uibundle_test is issue #180's proof, and the reason issue #167
// owns it: the shared web host has to be able to serve a UI bundle chosen
// at RUN time, and choosing a different bridge must never change the
// binary's digest. Section 3.7 requires every provider package to carry
// the exact same core binary, so a per-provider bridge that needed a
// per-provider build would breach the rule the whole distribution model
// rests on.
//
// The test is deliberately end-to-end against a real built binary rather
// than against ResolveUIBundle in isolation: the claim is about an
// artifact anyone can hash, not about a function.
package uibundle_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// buildWebHost builds the one canonical web-host binary. GOWORK=off on
// purpose: the release image builds each module against its own go.mod,
// and a test that only passes under the repo-root workspace is not
// testing the artifact that ships.
func buildWebHost(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "backup-manager-web")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/backup-manager-web")
	cmd.Dir = filepath.Join(root, "apps", "generic")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build backup-manager-web: %v\n%s", err, out)
	}
	return bin
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeBundle(t *testing.T, dir, marker string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "<!doctype html><title>bridge:" + marker + "</title>"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	return dir
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// serveAndFetch starts the binary with args, waits for it to answer, and
// returns the body of GET /.
func serveAndFetch(t *testing.T, bin string, args ...string) string {
	t.Helper()

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	full := append([]string{"serve-ui", "--listen", addr, "--upstream", "http://127.0.0.1:1"}, args...)

	cmd := exec.Command(bin, full...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", full, err)
	}
	// exited closes as soon as the process is gone, so a start failure
	// reports the exit status straight away instead of being reported
	// twenty seconds later as "never answered", which reads like a
	// timeout and is not one.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// reaped records that the loop below already took the value off
	// exited, so this cleanup does not wait for a second one that is
	// never coming (issue #87). Both run on the test's own goroutine -
	// t.Fatalf calls runtime.Goexit, which runs cleanups - so this needs
	// no synchronisation of its own.
	//
	// Without it a serve-ui that refuses to start deadlocks: the loop
	// receives the exit status, reports it with t.Fatalf, and the
	// cleanup then blocks forever on a channel nothing will send to
	// again, so the whole package dies on the 10-minute go test timeout
	// instead of printing the exit status the loop had already worked
	// out. Ten minutes of nothing in place of one line, which is how it
	// was found.
	reaped := false
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if reaped {
			return
		}
		<-exited
	})

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-exited:
			reaped = true
			t.Fatalf("serve-ui %v exited before answering: %v", full, err)
		default:
		}
		resp, err := client.Get("http://" + addr + "/")
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read body: %v", readErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET / = %d: %s", resp.StatusCode, body)
			}
			return string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve-ui %v never answered: %v", full, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOneBinaryServesEveryProviderBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real binary")
	}

	root := repoRoot(t)
	bin := buildWebHost(t, root)
	before := sha256File(t, bin)

	// A bundle root holds one directory per runtime profile, which is the
	// route a deployment of the canonical image takes. --ui-dir is the
	// other route, and it is the one a provider PACKAGE takes: a .spk or
	// a .UPK ships its own bridge beside the binary and names it, without
	// needing a profile row of its own.
	bundleRoot := t.TempDir()
	writeBundle(t, filepath.Join(bundleRoot, "generic"), "generic-from-root")
	writeBundle(t, filepath.Join(bundleRoot, "ugos"), "ugos")
	explicit := writeBundle(t, filepath.Join(t.TempDir(), "synology"), "synology")

	cases := []struct {
		name   string
		args   []string
		marker string
	}{
		{
			name:   "the compile-time bundle, when nothing selects another",
			args:   nil,
			marker: "", // whatever go:embed carries; asserted only for being served
		},
		{
			name:   "a profile-selected bundle out of a bundle root",
			args:   []string{"--ui-root", bundleRoot, "--profile", "generic"},
			marker: "bridge:generic-from-root",
		},
		{
			// --trusted-gateway is not decoration here (issue #87):
			// serve-ui is the container with the LAN-facing port, so a
			// gateway profile has to declare where its platform gateway
			// is at this hop or the process refuses to start rather than
			// serve a console that can never sign anyone in. Selecting
			// the ugos BUNDLE and declaring the ugos TRUST BOUNDARY are
			// one command now, which is what this line records.
			name:   "a different profile, same binary, same root",
			args:   []string{"--ui-root", bundleRoot, "--profile", "ugos", "--trusted-gateway", "10.0.0.0/8"},
			marker: "bridge:ugos",
		},
		{
			name:   "a package-supplied bundle for a platform with no profile row of its own",
			args:   []string{"--ui-dir", explicit},
			marker: "bridge:synology",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := serveAndFetch(t, bin, tc.args...)
			if tc.marker == "" {
				if !strings.Contains(strings.ToLower(body), "<html") && !strings.Contains(strings.ToLower(body), "<!doctype") {
					t.Fatalf("the embedded fallback served something that is not an app shell: %q", body)
				}
				return
			}
			if !strings.Contains(body, tc.marker) {
				t.Fatalf("served %q, want the bundle carrying %q", body, tc.marker)
			}
		})
	}

	// The claim §3.7 rests on: after serving four different bridges, the
	// artifact on disk is byte-for-byte the one that was built.
	if after := sha256File(t, bin); after != before {
		t.Fatalf("the binary's digest changed across bridge selection: %s -> %s", before, after)
	}
	t.Logf("one binary, four bridges, sha256 %s unchanged", before)
}

// TestBridgeSelectionIsNotACompileTimeInput is the negative side of the
// same claim, with the positive control built in: if a provider name were
// still a build input, two builds selecting two providers would differ.
// They must not, because there is no such build input any more.
func TestBridgeSelectionIsNotACompileTimeInput(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a real binary twice")
	}

	root := repoRoot(t)

	build := func(env ...string) string {
		bin := filepath.Join(t.TempDir(), "backup-manager-web")
		cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/backup-manager-web")
		cmd.Dir = filepath.Join(root, "apps", "generic")
		cmd.Env = append(append(os.Environ(), "GOWORK=off"), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
		return sha256File(t, bin)
	}

	base := build()
	withPlatform := build("VITE_PLATFORM=synology", "BACKUP_MANAGER_PLATFORM=synology")
	if base != withPlatform {
		t.Fatalf("naming a provider at build time changed the binary: %s vs %s", base, withPlatform)
	}

	// Positive control: this comparison has to be able to see a real
	// difference, or "unchanged" means nothing. A different ldflag value
	// is a genuine content change and must produce a different digest.
	stamped := build()
	if stamped != base {
		t.Fatalf("two identical builds already differ (%s vs %s), so this test cannot distinguish a change from build noise", base, stamped)
	}
	changed := func() string {
		bin := filepath.Join(t.TempDir(), "backup-manager-web")
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version=deliberately-different", "-o", bin, "./cmd/backup-manager-web")
		cmd.Dir = filepath.Join(root, "apps", "generic")
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
		return sha256File(t, bin)
	}()
	if changed == base {
		t.Fatal("changing the version ldflag did not change the digest, so this test would not notice a per-provider build either")
	}
}

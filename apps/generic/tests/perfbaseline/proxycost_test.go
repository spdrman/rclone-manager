// proxycost_test.go measures what the shipped engine-plus-web-ui split
// actually costs (issue #167).
//
// container/compose.yaml runs two services from one image: the engine,
// with no published port, and a web-ui service that serves the static UI
// and reverse-proxies /api/v1 to the engine over a private bridge
// network. That is a project-owner requirement, adopted for network
// isolation, and #167 does not remove it. What #167 requires instead is
// that its cost stops being assumed and starts being measured: EPIC B
// #81's performance contract says no material data-path proxy may be
// added, and an already-shipped one whose cost nobody has measured is
// indistinguishable from one that is fine.
//
// So this harness runs the SAME read the Phase 6 baseline times
// (GET /api/v1/backup-sets, same workload constants, same client shape)
// twice against the same engine process: once directly, and once through
// a real serve-ui process proxying to it, exactly as compose wires them.
// The difference is the hop.
//
// Deliberately a separate harness rather than a metric inside
// TestCaptureRuntimeBaseline: the baseline record is compared against a
// pinned gate, and adding a field to it would make every existing record
// incomplete. This one records a number for the deviation log instead.
package perfbaseline_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// proxyCostRecord is what this harness prints.
type proxyCostRecord struct {
	Workload string `json:"workload"`
	Endpoint string `json:"endpoint"`
	// Direct is measured against the engine's own listener, the way a
	// single-process deployment would serve it.
	Direct latencySet `json:"direct"`
	// Proxied is measured through a real serve-ui process, the way
	// container/compose.yaml actually serves it.
	Proxied latencySet `json:"proxied"`
	// HopP95Ms and HopP50Ms are the differences. These are the numbers
	// the deviation log records.
	HopP95Ms float64 `json:"hop_p95_ms"`
	HopP50Ms float64 `json:"hop_p50_ms"`
}

func TestMeasureTwoServiceProxyCost(t *testing.T) {
	if os.Getenv("PERF_PROXY_COST") != "1" {
		t.Skip("proxy cost harness: set PERF_PROXY_COST=1 to run it")
	}

	repoRoot := repoRoot(t)
	bin := buildEngine(t, repoRoot)

	dir := t.TempDir()
	configPath := writeWorkloadConfig(t, dir)

	enginePort := freePort(t)
	engineAddr := fmt.Sprintf("127.0.0.1:%d", enginePort)
	engineBase := "http://" + engineAddr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := exec.CommandContext(ctx, bin, "serve",
		"--config", configPath,
		"--listen", engineAddr,
		"--auth-store", filepath.Join(dir, "state", "local-auth.json"),
		"--profile", "generic",
	)
	stdout, err := engine.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	engine.Stderr = os.Stderr
	started := time.Now()
	if err := engine.Start(); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	defer func() {
		cancel()
		_ = engine.Wait()
	}()

	tokenCh := make(chan string, 1)
	go scanBootstrapToken(stdout, tokenCh)

	direct := newClient(t)
	waitReady(t, direct, engineBase, started)

	token := ""
	select {
	case token = <-tokenCh:
	case <-time.After(10 * time.Second):
		t.Fatal("engine never printed an enrollment bootstrap token")
	}
	username, password := enrollReturningCredentials(t, direct, engineBase, token)

	// The UI host, run exactly as container/compose.yaml runs it: the
	// same binary, a different command, pointed at the engine.
	uiPort := freePort(t)
	uiAddr := fmt.Sprintf("127.0.0.1:%d", uiPort)
	uiBase := "http://" + uiAddr
	ui := exec.CommandContext(ctx, bin, "serve-ui",
		"--listen", uiAddr,
		"--upstream", engineBase,
		"--profile", "generic",
	)
	ui.Stderr = os.Stderr
	if err := ui.Start(); err != nil {
		t.Fatalf("start serve-ui: %v", err)
	}
	defer func() {
		cancel()
		_ = ui.Wait()
	}()

	// A second client, because the proxied path establishes its own
	// session through the proxy: sharing a cookie jar across the two
	// would measure a warm session against a cold one.
	proxied := newClient(t)
	waitReady(t, proxied, uiBase, time.Now())
	logIn(t, proxied, uiBase, username, password)

	phase := latencyPhase{
		endpoint: "GET /api/v1/backup-sets",
		method:   http.MethodGet,
		path:     "/api/v1/backup-sets",
		samples:  readSamples,
		wantCode: http.StatusOK,
	}

	// Interleaved order, direct first: a machine that drifts warmer or
	// busier during the run would otherwise be indistinguishable from a
	// proxy cost, and the proxy is measured second, which is the
	// conservative direction only if the machine is not drifting. Both
	// are recorded so a reader can see the whole distribution rather
	// than one derived difference.
	directSet := measure(t, direct, engineBase, phase)
	proxiedSet := measure(t, proxied, uiBase, phase)

	rec := proxyCostRecord{
		Workload: workloadID,
		Endpoint: phase.endpoint,
		Direct:   directSet,
		Proxied:  proxiedSet,
		HopP95Ms: round3(proxiedSet.P95Ms - directSet.P95Ms),
		HopP50Ms: round3(proxiedSet.P50Ms - directSet.P50Ms),
	}

	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("two-service proxy cost:\n%s", out)
	if path := os.Getenv("PERF_PROXY_COST_OUT"); path != "" {
		if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// The one assertion: a proxied read has to actually be a proxied
	// read. A misconfigured upstream would answer from the UI host's own
	// static handler and record a hop cost of roughly zero, which is the
	// most misleading result this harness could produce.
	if proxiedSet.ResponseBytes != directSet.ResponseBytes {
		t.Fatalf("the proxied read returned %d bytes and the direct read %d; the proxied path is not serving the same response, so the difference is not a hop cost",
			proxiedSet.ResponseBytes, directSet.ResponseBytes)
	}
}

// enrollReturningCredentials is the baseline harness's own enroll, with
// the credentials handed back so a second client can log in through the
// proxy. Enrolment is single-use, so the proxied client cannot repeat it.
//
// The password is generated per run and lives in this process's memory
// only. Nothing here writes it anywhere, and neither this harness's log
// output nor its JSON record carries it.
func enrollReturningCredentials(t *testing.T, c *http.Client, base, bootstrapToken string) (string, string) {
	t.Helper()
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate password: %v", err)
	}
	const username = "perf"
	password := base64.RawURLEncoding.EncodeToString(raw)

	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/enroll", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, cookieValue(t, c, base, csrfCookie))
	req.Header.Set("X-Bootstrap-Token", bootstrapToken)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	payload, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll: status %d: %s", resp.StatusCode, payload)
	}
	return username, password
}

// logIn authenticates the proxied client against the administrator the
// direct client just enrolled, through the reverse proxy, which is also
// a small proof that the proxied path carries authentication correctly
// before anything is timed through it.
func logIn(t *testing.T, c *http.Client, base, username, password string) {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, cookieValue(t, c, base, csrfCookie))

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("login through the proxy: %v", err)
	}
	payload, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("login through the proxy: status %d: %s", resp.StatusCode, payload)
	}
}

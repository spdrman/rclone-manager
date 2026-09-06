// Package perfbaseline_test captures the runtime half of the Phase 6
// pre-refactor performance baseline (issue #165, EPIC B #81's performance
// contract).
//
// Five of the contract's seven metrics are properties of the running
// process and are measured here, against the real `backup-manager-web
// serve` binary over real HTTP, never against an in-process httptest
// handler: idle RSS, startup-to-healthy time, /api/v1 read latency,
// configuration write latency, and idle CPU. The remaining two live
// elsewhere because they are not properties of this process:
// core/tests/perfbaseline measures transfer throughput through the
// transport adapter (which is core's, not this app's), and
// scripts/perf/capture-baseline.sh measures the OCI image size by
// building it.
//
// This is a harness, not a gate. It is skipped unless PERF_BASELINE=1, so
// an ordinary `go test ./...` (and every CI job that runs one) never pays
// for it and never goes red on a noisy number. scripts/perf/capture-baseline.sh
// is the supported way to run it; docs/perf/README.md defines the host,
// the workload and the threshold the recorded numbers are compared
// against.
//
// # Nothing here writes a credential to disk
//
// The engine prints a single-use enrollment bootstrap token on its own
// stdout. That token is a credential, so this harness reads it from a
// pipe attached to the child process and keeps it in memory; it is never
// redirected to a file, and the password the harness enrolls with is
// generated per run and likewise never leaves memory. The emitted JSON
// record carries measurements only.
package perfbaseline_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The workload constants below ARE the workload definition. Changing any
// of them makes a later run incomparable with a recorded baseline, which
// is why workloadID carries them: docs/perf/README.md pins the identifier,
// and scripts/perf/check-baseline.sh refuses a record whose workload id
// does not match the one it was told to compare against.
const (
	// workloadID names this exact shape. Bump the version suffix on any
	// change to the constants below or to what the phases measure.
	workloadID = "phase6-baseline-v1"

	// backupSetCount is how many backup sets the config declares, spread
	// over sourceCount sources. GET /api/v1/backup-sets serialises all of
	// them, so this is the size of the read being timed. Fifteen is a
	// plausible small-NAS deployment and large enough that the response
	// is not a single object.
	backupSetCount = 15
	sourceCount    = 3

	// readSamples and writeSamples are how many requests each latency
	// phase issues, after warmupSamples discarded ones. 400 reads gives a
	// p95 estimated from 20 tail samples; a configuration write rewrites
	// the YAML file and bumps the config revision, so it is both slower
	// and more side-effecting, and 60 is enough for a stable p50/p95
	// without rewriting the file thousands of times.
	readSamples   = 400
	writeSamples  = 60
	warmupSamples = 40

	// idleSettle is how long the process is left alone after it reports
	// ready before idle RSS and idle CPU are sampled: long enough for
	// startup allocations to be done and for the scheduler's first tick
	// to have happened or not, short enough to keep a capture under a
	// minute.
	idleSettle = 20 * time.Second

	// startupTimeout bounds the wait for the first ready response. A
	// process that has not become healthy in this long has failed, and
	// recording that as a "startup time" would be a lie.
	startupTimeout = 60 * time.Second
)

// runtimeRecord is the JSON this harness prints. scripts/perf/capture-baseline.sh
// merges it with the other two harnesses' records into one baseline file.
type runtimeRecord struct {
	Workload             string  `json:"workload"`
	GOOS                 string  `json:"goos"`
	GOARCH               string  `json:"goarch"`
	NumCPU               int     `json:"num_cpu"`
	BackupSets           int     `json:"backup_sets"`
	StartupToHealthyMs   float64 `json:"startup_to_healthy_ms"`
	IdleRSSBytes         int64   `json:"idle_rss_bytes"`
	IdleCPUPercent       float64 `json:"idle_cpu_percent"`
	IdleCPUWindowSeconds float64 `json:"idle_cpu_window_seconds"`
	// IdleCPUFloorPercent is the smallest non-zero idle CPU this
	// measurement can distinguish: ps reports cumulative CPU in
	// hundredths of a second, so one tick over the window is the floor. A
	// recorded IdleCPUPercent of 0 means "below this", never "none".
	IdleCPUFloorPercent float64 `json:"idle_cpu_floor_percent"`
	// IdleCPUSecondsTotal is cumulative process CPU at the end of the
	// idle window, startup included. It has no floor problem, so it is
	// the number to compare when IdleCPUPercent reads zero on both sides.
	IdleCPUSecondsTotal float64    `json:"idle_cpu_seconds_total"`
	APIRead             latencySet `json:"api_read_latency_ms"`
	ConfigWrite         latencySet `json:"config_write_latency_ms"`
}

// latencySet is one timed phase. p95 is the number the Phase 6 gate
// compares; the rest are recorded so a later reader can see whether a
// change moved the whole distribution or only its tail.
type latencySet struct {
	Endpoint string `json:"endpoint"`
	Samples  int    `json:"samples"`
	// ResponseBytes is the size of the last timed response body. It is
	// recorded so a reader can tell at a glance whether the timed read
	// actually serialised the workload's fifteen backup sets or answered
	// with an empty list, which is the difference between a baseline
	// worth comparing and one that measures routing alone.
	ResponseBytes int     `json:"response_bytes"`
	MinMs         float64 `json:"min_ms"`
	P50Ms         float64 `json:"p50_ms"`
	P95Ms         float64 `json:"p95_ms"`
	P99Ms         float64 `json:"p99_ms"`
	MaxMs         float64 `json:"max_ms"`
}

func TestCaptureRuntimeBaseline(t *testing.T) {
	if os.Getenv("PERF_BASELINE") != "1" {
		t.Skip("perf baseline harness: set PERF_BASELINE=1 to run it (scripts/perf/capture-baseline.sh does)")
	}

	repoRoot := repoRoot(t)
	bin := buildEngine(t, repoRoot)

	dir := t.TempDir()
	configPath := writeWorkloadConfig(t, dir)

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	base := "http://" + addr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "serve",
		"--config", configPath,
		"--listen", addr,
		"--auth-store", filepath.Join(dir, "state", "local-auth.json"),
	)
	// The bootstrap token is a credential. It is read out of this pipe
	// and held in memory; it is never written anywhere.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	started := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	tokenCh := make(chan string, 1)
	go scanBootstrapToken(stdout, tokenCh)

	client := newClient(t)

	// Startup-to-healthy: from exec to the first successful readiness
	// response. /health/ready, not /health/live: live only proves a
	// listener answered, ready is what the container HEALTHCHECK and the
	// compose dependency condition actually wait for.
	healthy := waitReady(t, client, base, started)
	startupMs := healthy.Sub(started).Seconds() * 1000

	token := ""
	select {
	case token = <-tokenCh:
	case <-time.After(10 * time.Second):
		t.Fatal("engine never printed an enrollment bootstrap token")
	}

	enroll(t, client, base, token)

	// Idle phase first, before any load: RSS and CPU measured after a
	// burst of API traffic would describe the burst, not the idle
	// process, and idle RSS is the metric the performance contract names.
	time.Sleep(idleSettle)
	rss := processRSSBytes(t, cmd.Process.Pid)
	idleCPU, cpuWindow, cpuTotal := processIdleCPUPercent(t, cmd.Process.Pid)

	read := measure(t, client, base, latencyPhase{
		endpoint: "GET /api/v1/backup-sets",
		method:   http.MethodGet,
		path:     "/api/v1/backup-sets",
		samples:  readSamples,
		wantCode: http.StatusOK,
	})

	write := measure(t, client, base, latencyPhase{
		endpoint: "PATCH /api/v1/settings",
		method:   http.MethodPatch,
		path:     "/api/v1/settings",
		body:     []byte(`{"retention":{"timezone":"UTC"}}`),
		samples:  writeSamples,
		wantCode: http.StatusOK,
		csrf:     true,
	})

	rec := runtimeRecord{
		Workload:             workloadID,
		GOOS:                 runtime.GOOS,
		GOARCH:               runtime.GOARCH,
		NumCPU:               runtime.NumCPU(),
		BackupSets:           backupSetCount,
		StartupToHealthyMs:   round3(startupMs),
		IdleRSSBytes:         rss,
		IdleCPUPercent:       round3(idleCPU),
		IdleCPUWindowSeconds: round3(cpuWindow),
		IdleCPUFloorPercent:  round3(psTickSeconds / cpuWindow * 100),
		IdleCPUSecondsTotal:  round3(cpuTotal),
		APIRead:              read,
		ConfigWrite:          write,
	}
	emit(t, rec)
}

// emit writes the record to PERF_BASELINE_OUT if set, and always echoes
// it to the test log so a hand run shows its own result.
func emit(t *testing.T, rec runtimeRecord) {
	t.Helper()
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	t.Logf("runtime baseline:\n%s", out)
	if path := os.Getenv("PERF_BASELINE_OUT"); path != "" {
		if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

type latencyPhase struct {
	endpoint string
	method   string
	path     string
	body     []byte
	samples  int
	wantCode int
	csrf     bool
}

// measure issues warmupSamples discarded requests and then phase.samples
// timed ones, and reports the distribution. Warmup matters here: the
// first read pays for lazily-built handler state and the first write
// pays for the config file's first rewrite, and neither is what a steady
// deployment experiences.
func measure(t *testing.T, c *http.Client, base string, phase latencyPhase) latencySet {
	t.Helper()

	do := func() (time.Duration, int) {
		var body io.Reader
		if phase.body != nil {
			body = bytes.NewReader(phase.body)
		}
		req, err := http.NewRequest(phase.method, base+phase.path, body)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if phase.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if phase.csrf {
			req.Header.Set(csrfHeader, cookieValue(t, c, base, csrfCookie))
		}
		start := time.Now()
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", phase.endpoint, err)
		}
		payload, _ := io.ReadAll(resp.Body)
		elapsed := time.Since(start)
		_ = resp.Body.Close()
		if resp.StatusCode != phase.wantCode {
			t.Fatalf("%s: status %d (want %d): %s", phase.endpoint, resp.StatusCode, phase.wantCode, payload)
		}
		return elapsed, len(payload)
	}

	for i := 0; i < warmupSamples && i < phase.samples; i++ {
		do()
	}

	ms := make([]float64, 0, phase.samples)
	var lastSize int
	for i := 0; i < phase.samples; i++ {
		d, size := do()
		ms = append(ms, d.Seconds()*1000)
		lastSize = size
	}
	sort.Float64s(ms)

	return latencySet{
		Endpoint:      phase.endpoint,
		Samples:       len(ms),
		ResponseBytes: lastSize,
		MinMs:         round3(ms[0]),
		P50Ms:         round3(percentile(ms, 50)),
		P95Ms:         round3(percentile(ms, 95)),
		P99Ms:         round3(percentile(ms, 99)),
		MaxMs:         round3(ms[len(ms)-1]),
	}
}

// percentile is the nearest-rank percentile of an already-sorted slice.
// Nearest-rank rather than an interpolating variant deliberately: every
// reported value is a real observed sample, so a recorded p95 is a
// latency that actually happened rather than a number between two that
// did.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*p/100 + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

const (
	csrfCookie = "bm_csrf"
	csrfHeader = "X-CSRF-Token"
)

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		// One reused keep-alive connection, matching what the shared UI
		// does. A fresh TCP handshake per request would put connection
		// setup inside every latency sample.
		Transport: &http.Transport{MaxIdleConnsPerHost: 4},
	}
}

func cookieValue(t *testing.T, c *http.Client, base, name string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/health/live", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for _, ck := range c.Jar.Cookies(req.URL) {
		if ck.Name == name {
			return ck.Value
		}
	}
	t.Fatalf("no %s cookie yet", name)
	return ""
}

// waitReady polls /health/ready until it answers 2xx, and returns the
// instant it did. The first poll also seeds the CSRF cookie, since
// EnsureCSRFCookie wraps the engine's whole mux.
func waitReady(t *testing.T, c *http.Client, base string, started time.Time) time.Time {
	t.Helper()
	deadline := started.Add(startupTimeout)
	for time.Now().Before(deadline) {
		resp, err := c.Get(base + "/health/ready")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return time.Now()
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("engine never became ready within %s", startupTimeout)
	return time.Time{}
}

func enroll(t *testing.T, c *http.Client, base, bootstrapToken string) {
	t.Helper()
	// Generated per run, held in memory, never written anywhere.
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate password: %v", err)
	}
	password := base64.RawURLEncoding.EncodeToString(raw)

	body := fmt.Sprintf(`{"username":"perf","password":%q}`, password)
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
}

var bootstrapTokenRE = regexp.MustCompile(`Enrollment bootstrap token: (\S+)`)

// scanBootstrapToken reads the engine's stdout, forwards the first
// bootstrap token it sees, and then keeps draining so the child never
// blocks on a full pipe.
func scanBootstrapToken(r io.Reader, out chan<- string) {
	sc := bufio.NewScanner(r)
	sent := false
	for sc.Scan() {
		if sent {
			continue
		}
		if m := bootstrapTokenRE.FindStringSubmatch(sc.Text()); m != nil {
			out <- m[1]
			sent = true
		}
	}
}

// processRSSBytes reads the child's resident set size out of ps. Reading
// the number the operating system reports for the real process is the
// point: runtime.ReadMemStats would describe this test binary, and the Go
// heap is not what an operator sees in `docker stats`.
func processRSSBytes(t *testing.T, pid int) int64 {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps rss: %v", err)
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("parse rss %q: %v", out, err)
	}
	return kb * 1024
}

// processIdleCPUPercent samples cumulative process CPU time twice across
// a fixed window and reports the fraction of one core consumed, the
// window it used, and the cumulative CPU at the end.
//
// ps reports hundredths of a second, so a genuinely idle process reports
// a delta of exactly zero and the percentage says only "below one tick
// over this window". That floor is recorded alongside the number rather
// than hidden, and the cumulative total is recorded too, because two
// zeroes compared against each other prove nothing while two cumulative
// totals do.
func processIdleCPUPercent(t *testing.T, pid int) (percent, windowSeconds, cumulativeSeconds float64) {
	t.Helper()
	const window = 30 * time.Second
	before := processCPUSeconds(t, pid)
	start := time.Now()
	time.Sleep(window)
	after := processCPUSeconds(t, pid)
	elapsed := time.Since(start).Seconds()
	return (after - before) / elapsed * 100, elapsed, after
}

// psTickSeconds is the resolution of ps's cumulative CPU column.
const psTickSeconds = 0.01

func processCPUSeconds(t *testing.T, pid int) float64 {
	t.Helper()
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps time: %v", err)
	}
	return parseCPUTime(t, strings.TrimSpace(string(out)))
}

// parseCPUTime reads ps's cumulative CPU column, which is
// [[DD-]HH:]MM:SS[.ff].
func parseCPUTime(t *testing.T, s string) float64 {
	t.Helper()
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		v, err := strconv.ParseFloat(d, 64)
		if err != nil {
			t.Fatalf("parse cpu time %q: %v", s, err)
		}
		days = v
		s = rest
	}
	parts := strings.Split(s, ":")
	total := 0.0
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			t.Fatalf("parse cpu time %q: %v", s, err)
		}
		total = total*60 + v
	}
	return days*86400 + total
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// buildEngine builds the real binary the OCI image runs, from the module
// it lives in, with GOWORK=off so the build resolves exactly as CI's own
// per-module jobs do.
func buildEngine(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "backup-manager-web")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/backup-manager-web")
	cmd.Dir = filepath.Join(repoRoot, "apps", "generic")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build backup-manager-web: %v\n%s", err, out)
	}
	return bin
}

// writeWorkloadConfig writes the workload's config file: sourceCount
// sources holding backupSetCount local-remote backup sets between them,
// with real (empty) directories behind each one so the engine's own
// startup checks pass. Local remotes rather than sftp deliberately: this
// harness measures the API and the process, and an sftp set would make
// every capture depend on a reachable fixture host.
func writeWorkloadConfig(t *testing.T, dir string) string {
	t.Helper()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	var b strings.Builder
	b.WriteString("poll_interval: 15m\n")
	b.WriteString("state:\n")
	b.WriteString("  database: " + filepath.Join(stateDir, "state.db") + "\n")
	b.WriteString("sources:\n")

	perSource := backupSetCount / sourceCount
	for s := 0; s < sourceCount; s++ {
		fmt.Fprintf(&b, "  - id: source-%02d\n", s)
		b.WriteString("    backup_sets:\n")
		for i := 0; i < perSource; i++ {
			remote := filepath.Join(dir, fmt.Sprintf("remote-%02d-%02d", s, i))
			local := filepath.Join(dir, fmt.Sprintf("local-%02d-%02d", s, i))
			for _, p := range []string{remote, local} {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
			}
			fmt.Fprintf(&b, "      - id: set-%02d\n", i)
			b.WriteString("        remote:\n")
			b.WriteString("          type: local\n")
			b.WriteString("        remote_path: " + remote + "\n")
			b.WriteString("        local_path: " + local + "\n")
			b.WriteString("        include:\n")
			b.WriteString("          - \"*.dump\"\n")
			b.WriteString("        completion:\n")
			b.WriteString("          strategy: rename\n")
			b.WriteString("        stale_after: 24h\n")
		}
	}
	b.WriteString("retention:\n")
	b.WriteString("  timezone: UTC\n")
	b.WriteString("  week_starts_on: monday\n")

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

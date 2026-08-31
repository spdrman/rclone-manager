// Package cliapi_test is issue #167's CLI-versus-API equivalence check.
//
// "The CLI and the web/API must operate on the same application and
// service layer. Two code paths that both delete restore points is the
// failure mode this rule exists to prevent." The acceptance criterion is
// explicit that this has to be proven by an equivalence check rather than
// by inspection, so this drives both real surfaces against the same
// on-disk state and compares what they decide.
//
// Retention is the path worth proving. It is the one place in the product
// where a decision maps directly onto deleting a restore point, so a
// divergence between the two surfaces here would be the exact defect the
// rule names. Both are exercised against real artifacts rather than an
// empty backup set: an equivalence check where both sides answer "nothing
// to decide" is a check that passes for the wrong reason, and the fixture
// below deliberately produces one KEEP and one DELETE.
package cliapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
	"github.com/spdrman/rclone-manager/core/service"
)

// verdict is the shape both surfaces have to agree on: which artifact,
// and whether it is kept.
type verdict struct {
	Artifact string
	Keep     bool
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// buildCLI builds core's own executable, the one the container image
// carries as /backup-manager.
func buildCLI(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "backup-manager")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/backup-manager")
	cmd.Dir = filepath.Join(root, "core")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build backup-manager: %v\n%s", err, out)
	}
	return bin
}

// writeFixture lays down a config with two real remote objects behind one
// backup set, so a retention preview has something to decide about.
func writeFixture(t *testing.T) (dir, configPath string) {
	t.Helper()
	dir = t.TempDir()
	remote := filepath.Join(dir, "remote")
	local := filepath.Join(dir, "local")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, name := range []string{"db-2026-08-01.dump", "db-2026-08-02.dump"} {
		if err := os.WriteFile(filepath.Join(remote, name), bytes.Repeat([]byte(name[:4]), 256), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	configPath = filepath.Join(dir, "config.yaml")
	body := "poll_interval: 1h\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: pg\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remote + "\n" +
		"        local_path: " + local + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir, configPath
}

// cliVerdictRe parses one line of `backup-manager retention --dry-run`.
var cliVerdictRe = regexp.MustCompile(`^\s+(KEEP|DELETE)\s+(\S+)\s+tiers=`)

func cliRetentionVerdicts(t *testing.T, bin, configPath string) []verdict {
	t.Helper()
	cmd := exec.Command(bin, "retention", "--dry-run", "--config", configPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("backup-manager retention --dry-run: %v\n%s", err, out)
	}
	var got []verdict
	for _, line := range strings.Split(string(out), "\n") {
		m := cliVerdictRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		got = append(got, verdict{Artifact: m[2], Keep: m[1] == "KEEP"})
	}
	if len(got) == 0 {
		t.Fatalf("the CLI printed no retention verdicts at all, so there is nothing to compare:\n%s", out)
	}
	return normalise(got)
}

func apiRetentionVerdicts(t *testing.T, configPath string) []verdict {
	t.Helper()

	backend, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	authSvc, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	adapter, err := profile.Generic.Profile().Adapter(profile.AdapterConfig{LocalAuth: authSvc.Authenticator()})
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}
	srv := httptest.NewServer(serve.NewEngine(serve.EngineConfig{
		Platform:   adapter,
		AuthRoutes: authSvc.Handler(),
		Backend:    backend,
	}))
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}
	enroll(t, authSvc, client, srv.URL)

	resp, err := client.Get(srv.URL + "/api/v1/backup-sets/production/pg/retention/preview")
	if err != nil {
		t.Fatalf("GET retention preview: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retention preview returned %d: %s", resp.StatusCode, body)
	}

	var plan struct {
		Verdicts []struct {
			Artifact string `json:"artifact"`
			Action   string `json:"action"`
		} `json:"verdicts"`
	}
	if err := json.Unmarshal(body, &plan); err != nil {
		t.Fatalf("unmarshal retention plan: %v\n%s", err, body)
	}
	if len(plan.Verdicts) == 0 {
		t.Fatalf("the API returned no retention verdicts at all, so there is nothing to compare:\n%s", body)
	}

	got := make([]verdict, 0, len(plan.Verdicts))
	for _, v := range plan.Verdicts {
		// An unrecognised action is a hard failure, not a false Keep.
		// The first draft of this test read a "keep" boolean that the
		// contract does not have, so every artifact unmarshalled as
		// false and the check reported a divergence that was entirely
		// its own: a JSON field that is not there reads as the zero
		// value, silently, which for a boolean is a decision.
		var keep bool
		switch v.Action {
		case "KEEP":
			keep = true
		case "DELETE":
			keep = false
		default:
			t.Fatalf("the API reported action %q for %q, which is neither KEEP nor DELETE; this comparison must not guess what a third action means", v.Action, v.Artifact)
		}
		// The API reports the artifact by its full identity; the CLI
		// prints the basename. Comparing the basename is the common
		// denominator, and artifact identity IS a basename today
		// (EPIC A's inherited constraint), so nothing is lost.
		got = append(got, verdict{Artifact: filepath.Base(v.Artifact), Keep: keep})
	}
	return normalise(got)
}

func enroll(t *testing.T, authSvc *local.Service, client *http.Client, base string) {
	t.Helper()

	seed, err := client.Get(base + "/health/live")
	if err != nil {
		t.Fatalf("seed GET: %v", err)
	}
	seed.Body.Close()

	csrf := ""
	u, _ := http.NewRequest(http.MethodGet, base, nil)
	for _, c := range client.Jar.Cookies(u.URL) {
		if c.Name == local.CSRFCookieName {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("no CSRF cookie after the seed request")
	}

	var notice bytes.Buffer
	if err := authSvc.PrintBootstrapNotice(&notice, ""); err != nil {
		t.Fatalf("PrintBootstrapNotice: %v", err)
	}
	const marker = "token: "
	i := strings.Index(notice.String(), marker)
	if i < 0 {
		t.Fatalf("no bootstrap token in notice: %q", notice.String())
	}
	token := strings.Fields(notice.String()[i+len(marker):])[0]

	// Generated nowhere near disk: this password exists for the length of
	// this request and is never written, logged or asserted on.
	payload := []byte(`{"username":"cliapi","password":"correct-horse-battery-staple"}`)
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/enroll", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(local.CSRFHeaderName, csrf)
	req.Header.Set(local.BootstrapTokenHeader, token)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("enroll returned %d: %s", resp.StatusCode, b)
	}
}

func normalise(in []verdict) []verdict {
	out := append([]verdict(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Artifact < out[j].Artifact })
	return out
}

func format(in []verdict) string {
	parts := make([]string, 0, len(in))
	for _, v := range in {
		decision := "DELETE"
		if v.Keep {
			decision = "KEEP"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", v.Artifact, decision))
	}
	return strings.Join(parts, " ")
}

func TestCLIAndAPIAgreeOnEveryRetentionVerdict(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI")
	}

	root := repoRoot(t)
	bin := buildCLI(t, root)
	_, configPath := writeFixture(t)

	// One real cycle through the CLI, which is what puts committed
	// artifacts in the journal for either surface to reason about. It
	// also means the state both surfaces read was written by the CLI and
	// read by the API, which is the direction a divergence would show up
	// in first.
	fetch := exec.Command(bin, "fetch", "--source", "production", "--backup-set", "pg", "--config", configPath)
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Fatalf("backup-manager fetch: %v\n%s", err, out)
	}

	// Sequential, not parallel: the state journal takes an exclusive
	// startup lock, so the CLI has to have exited before the API opens
	// the same database. That is a property of the shared service layer,
	// and needing it is itself a small piece of evidence that there is
	// only one.
	fromCLI := cliRetentionVerdicts(t, bin, configPath)
	fromAPI := apiRetentionVerdicts(t, configPath)

	if len(fromCLI) < 2 {
		t.Fatalf("the fixture produced %d verdict(s); it is meant to produce at least one KEEP and one DELETE, and an equivalence check over a single trivial answer proves nothing: %s", len(fromCLI), format(fromCLI))
	}
	var keeps, deletes int
	for _, v := range fromCLI {
		if v.Keep {
			keeps++
		} else {
			deletes++
		}
	}
	if keeps == 0 || deletes == 0 {
		t.Fatalf("the fixture produced %d KEEP and %d DELETE; both surfaces agreeing on an all-KEEP answer would not distinguish two implementations: %s", keeps, deletes, format(fromCLI))
	}

	if format(fromCLI) != format(fromAPI) {
		t.Fatalf("the CLI and the API disagree about which restore points retention would delete\n  CLI: %s\n  API: %s", format(fromCLI), format(fromAPI))
	}

	// Positive control: the comparison has to be able to see a
	// disagreement. Flipping one verdict must make it fail, or the
	// assertion above passes for any pair of answers.
	flipped := append([]verdict(nil), fromAPI...)
	flipped[0].Keep = !flipped[0].Keep
	if format(fromCLI) == format(flipped) {
		t.Fatal("flipping a verdict did not change the comparison, so the equality check above cannot detect a divergence")
	}
}

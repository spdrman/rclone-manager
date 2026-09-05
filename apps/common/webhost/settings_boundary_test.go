// This file is issue #140's INTEGRATION requirement: config file, CLI and
// UI have to stay in agreement about one policy, so a write through the
// HTTP settings endpoint is proven visible to a subsequent CLI read, and
// an edit made on the config-file/CLI side is proven visible through the
// endpoint.
//
// It is a genuine boundary test, not a mock handshake: it builds the real
// `backup-manager` binary, runs a real cycle so there is a real artifact
// to decide about, drives the real chi router over a real
// service.BackupService opened from a real config file, and then reads
// the policy back by executing the real CLI and parsing what an operator
// would actually see on their terminal. Nothing in the loop is stubbed,
// which is what makes a disagreement between the three surfaces something
// this test can actually catch.
//
// `backup-manager retention` is the CLI read used because it is the only
// command that renders the whole FR-18/FR-19 policy observably: each
// verdict lists the tier names that claimed an artifact (upper-cased by
// internal/retention), and the trailing last-known-good line states
// FR-19's resolved reading in words. A tier the endpoint wrote therefore
// shows up by name, and protect_last_known_good shows up as the sentence
// internal/retention writes for it.
package webhost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

var (
	cliBuildOnce sync.Once
	cliBinary    string
	cliBuildErr  error
)

// backupManagerCLI builds core/cmd/backup-manager once per test binary
// and returns the path to it. The build is shared because it is the
// expensive part and the binary is immutable; each test still gets its
// own config, journal and directories.
//
// Building rather than shelling out to a PATH lookup is deliberate: the
// claim under test is that THIS tree's CLI and THIS tree's HTTP endpoint
// agree, which a stale binary installed somewhere on the machine would
// quietly not be evidence of.
func backupManagerCLI(t *testing.T) string {
	t.Helper()
	cliBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bm-cli-*")
		if err != nil {
			cliBuildErr = err
			return
		}
		bin := filepath.Join(dir, "backup-manager")
		cmd := exec.Command("go", "build", "-o", bin, "github.com/spdrman/rclone-manager/core/cmd/backup-manager")
		if out, err := cmd.CombinedOutput(); err != nil {
			cliBuildErr = fmtBuildError(err, out)
			return
		}
		cliBinary = bin
	})
	if cliBuildErr != nil {
		t.Fatalf("building the backup-manager CLI: %v", cliBuildErr)
	}
	return cliBinary
}

func fmtBuildError(err error, out []byte) error {
	return &buildError{err: err, out: string(out)}
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }
func (e *buildError) Unwrap() error { return e.err }

// runCLI executes the built binary and returns its combined output,
// failing the test on a non-zero exit: every invocation below is one an
// operator would expect to succeed, so a failure is the finding, not a
// case to branch on.
func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(backupManagerCLI(t), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("backup-manager %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// writeBoundaryConfig builds a minimal, valid config against real temp
// directories, wired through the "local" transport so a real cycle needs
// no network and no Docker — the same fixture shape
// core/cmd/backup-manager's own main_test.go uses.
func writeBoundaryConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("settings boundary test payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// openBoundaryRouter opens a real BackupService against configPath and
// wires it through the real NewRouter, exactly as a provider's own
// main.go does. The returned close must run before the config file is
// re-opened by another BackupService.
func openBoundaryRouter(t *testing.T, configPath string) (http.Handler, func()) {
	t.Helper()
	svc, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       svc,
		Gate:          NotYetImplementedGate{}, // the shipped default
		BinaryVersion: "test",
		Commit:        "test",
	})
	return router, func() { _ = cleanup() }
}

// TestSettingsWriteIsVisibleToASubsequentCLIRead is issue #140's
// INTEGRATION case in the UI-to-CLI direction.
func TestSettingsWriteIsVisibleToASubsequentCLIRead(t *testing.T) {
	configPath := writeBoundaryConfig(t)

	// One real cycle, so the retention preview below has a real artifact
	// to render verdicts for. Without it the CLI prints "(no managed,
	// completed backups yet)" and every assertion after it would be
	// measuring an empty report.
	runCLI(t, "run", "--config", configPath)

	// The positive control, taken before anything is written: the CLI
	// currently reports the DEFAULT policy. Without this, "the CLI shows
	// FORTNIGHTLY afterwards" could not distinguish a settings write that
	// worked from a fixture that always said so.
	before := runCLI(t, "retention", "--dry-run", "--config", configPath)
	if !strings.Contains(before, "DAILY") {
		t.Fatalf("the CLI does not report the default chain before the write, so nothing below is being measured:\n%s", before)
	}
	if strings.Contains(before, "FORTNIGHTLY") {
		t.Fatalf("the CLI already reports the tier this test is about to write:\n%s", before)
	}
	if !strings.Contains(before, "holds FR-19 last-known-good protection") {
		t.Fatalf("last-known-good protection is not active before the write:\n%s", before)
	}

	router, closeSvc := openBoundaryRouter(t, configPath)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"retention":{"timezone":"Europe/Berlin","tiers":[
		{"name":"fortnightly","granularity":"days","period_days":14,"keep":6},
		{"name":"annual","granularity":"year","keep":5}
	],"protect_last_known_good":false}}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		closeSvc()
		t.Fatalf("PATCH /api/v1/settings: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Read back through the CLI while the service that performed the
	// write is still running, which is the situation an operator is
	// actually in: a daemon serving the UI, and a terminal next to it.
	after := runCLI(t, "retention", "--dry-run", "--config", configPath)
	closeSvc()

	for _, want := range []string{"FORTNIGHTLY", "ANNUAL"} {
		if !strings.Contains(after, want) {
			t.Errorf("the CLI does not report tier %s that the endpoint just wrote:\n%s", want, after)
		}
	}
	for _, gone := range []string{"DAILY", "WEEKLY", "MONTHLY"} {
		if strings.Contains(after, gone) {
			t.Errorf("the CLI still reports the replaced default tier %s:\n%s", gone, after)
		}
	}
	// FR-19, in the words internal/retention itself writes for an
	// explicit false — the exact fact the UI's confirmation dialog warns
	// about, observed here on the other side of the boundary.
	if !strings.Contains(after, "materially more dangerous configuration") {
		t.Errorf("the CLI does not report last-known-good protection as disabled:\n%s", after)
	}

	// The file the endpoint wrote is one the daemon's own next start
	// accepts. `check` is exactly that question, asked by the CLI.
	runCLI(t, "check", "--config", configPath)
}

// TestConfigFileEditIsVisibleThroughTheSettingsEndpoint is the same case
// in the other direction. A config-file edit is what the CLI side of this
// product actually is for retention: `backup-manager retention`'s own
// override flags are preview-only and never persisted (that command's own
// doc), so "an operator changed the policy outside the UI" means they
// edited the YAML file and restarted, which is FR-5's documented model.
// service.Open here is that restart.
func TestConfigFileEditIsVisibleThroughTheSettingsEndpoint(t *testing.T) {
	configPath := writeBoundaryConfig(t)

	// The control: before the edit, the endpoint reports the default
	// chain and protection on.
	router, closeSvc := openBoundaryRouter(t, configPath)
	got := getBoundarySettings(t, router)
	closeSvc()
	if len(got.Retention.Tiers) != 3 || got.Retention.Tiers[0].Name != "daily" {
		t.Fatalf("tiers = %+v, want the resolved default chain before the edit", got.Retention.Tiers)
	}
	if !got.Retention.ProtectLastKnownGood {
		t.Fatal("protect_last_known_good is already false before the edit")
	}

	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	edited := strings.Replace(string(original),
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n",
		"retention:\n"+
			"  timezone: Europe/Berlin\n"+
			"  week_starts_on: sunday\n"+
			"  protect_last_known_good: false\n"+
			"  tiers:\n"+
			"    - name: hourly_ish\n"+
			"      granularity: days\n"+
			"      period_days: 1\n"+
			"      keep: 30\n"+
			"    - name: annual\n"+
			"      granularity: year\n"+
			"      keep: 7\n", 1)
	if edited == string(original) {
		t.Fatal("the hand edit changed nothing, so this test would prove nothing")
	}
	if err := os.WriteFile(configPath, []byte(edited), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The CLI agrees the edited file is loadable, before the endpoint is
	// asked about it: a failure here is a bad fixture, not a bug in the
	// endpoint.
	runCLI(t, "check", "--config", configPath)

	router, closeSvc = openBoundaryRouter(t, configPath)
	defer closeSvc()
	got = getBoundarySettings(t, router)

	if got.Retention.Timezone != "Europe/Berlin" || got.Retention.WeekStartsOn != "sunday" {
		t.Errorf("retention = %+v, want the edited Europe/Berlin + sunday", got.Retention)
	}
	if got.Retention.ProtectLastKnownGood {
		t.Error("protect_last_known_good = true, but the file says false")
	}
	wantTiers := []retentionTierBody{
		{Name: "hourly_ish", Granularity: "days", PeriodDays: 1, Keep: 30},
		{Name: "annual", Granularity: "year", Keep: 7},
	}
	if len(got.Retention.Tiers) != len(wantTiers) {
		t.Fatalf("tiers = %+v, want %+v", got.Retention.Tiers, wantTiers)
	}
	for i := range wantTiers {
		if got.Retention.Tiers[i] != wantTiers[i] {
			t.Errorf("tiers[%d] = %+v, want %+v", i, got.Retention.Tiers[i], wantTiers[i])
		}
	}
}

// TestSettingsWriteAndCLIOverrideResolveTheSameSubmissionIdentically is
// the third leg of the agreement: the UI's write path and the CLI's own
// -tier override both have to clear the legacy scalars when they take a
// chain, or the same submission produces a config one of them refuses.
// Proved by round-tripping through the real endpoint on a legacy config
// and then asking the CLI to load the result.
func TestSettingsWriteAndCLIOverrideResolveTheSameSubmissionIdentically(t *testing.T) {
	configPath := writeBoundaryConfig(t)
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	legacy := strings.Replace(string(original),
		"  week_starts_on: monday\n",
		"  week_starts_on: monday\n  daily_days: 7\n  weekly_months: 3\n  monthly_months: 12\n", 1)
	if err := os.WriteFile(configPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runCLI(t, "check", "--config", configPath)

	router, closeSvc := openBoundaryRouter(t, configPath)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(
		`{"retention":{"tiers":[{"name":"annual","granularity":"year","keep":5}]}}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	closeSvc()
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/v1/settings: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	got := string(written)
	// Positive control for the three absence checks below.
	if !strings.Contains(got, "name: annual") {
		t.Fatalf("the written config does not carry the submitted chain:\n%s", got)
	}
	for _, key := range []string{"daily_days", "weekly_months", "monthly_months"} {
		if strings.Contains(got, key) {
			t.Errorf("the written config carries %s alongside the submitted tiers list:\n%s", key, got)
		}
	}
	// And the CLI, which applies the identical mutual-exclusion rule in
	// applyRetentionOverrides, still loads it.
	runCLI(t, "check", "--config", configPath)
	runCLI(t, "retention", "--dry-run", "--config", configPath)
}

func getBoundarySettings(t *testing.T, router http.Handler) settingsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	return decodeSettings(t, rec)
}

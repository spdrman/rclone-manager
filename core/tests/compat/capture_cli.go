package compat

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
)

// buildCLI builds backup-manager from this working tree.
//
// The binary, not run() called in-process: FR-35's CLI clause is about
// what an operator sees in a terminal after an upgrade, and the only way
// to see that is to look at a process's real stdout, real stderr and real
// exit status. An in-process call would also have had to live in package
// main, where every other lane is editing.
func buildCLI(coreRoot, outDir string) (string, error) {
	bin := filepath.Join(outDir, "backup-manager")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/backup-manager")
	cmd.Dir = coreRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building backup-manager: %w\n%s", err, out)
	}
	return bin, nil
}

// cliCase is one invocation to pin.
type cliCase struct {
	label string
	args  []string
}

// captureCLI runs the fixed argv table against the seeded, medium-free
// deployment and records exit status, stdout and stderr for each.
//
// The table is chosen for what EPIC E is about to touch: the artifact read
// surfaces (which FR-34 wants to grow an access state on), the per-artifact
// detail (where a placement would show up), the settings surface (which
// FR-27 wants to grow a medium block on), the refusals, and the usage text
// (which grows a line for every new subcommand). `retention` is not here;
// it is its own cell below, for the clock reason in this package's doc.
func captureCLI(ctx context.Context, bin, cfgPath, root string) (Cell, error) {
	cases := []cliCase{
		{"version", []string{"version"}},
		{"no arguments at all", []string{}},
		{"an unknown subcommand", []string{"definitely-not-a-command"}},
		{"sources", []string{"sources", "--config", cfgPath}},
		{"artifacts, unfiltered", []string{"artifacts", "--config", cfgPath}},
		{"artifacts, filtered to the source", []string{"artifacts", "--config", cfgPath, "--source", "production"}},
		{"artifacts, filtered to a source nobody configured", []string{"artifacts", "--config", cfgPath, "--source", "nope"}},
		{"artifacts, one artifact's detail", []string{"artifacts", "--config", cfgPath, "production/postgres-primary/recent-daily.dump"}},
		{"artifacts, a quarantined artifact's detail", []string{"artifacts", "--config", cfgPath, "production/postgres-primary/quarantined-newest.dump"}},
		{"artifacts, an artifact that does not exist", []string{"artifacts", "--config", cfgPath, "production/postgres-primary/no-such.dump"}},
		{"artifacts, a filter combined with an operand", []string{"artifacts", "--config", cfgPath, "--source", "production", "production/postgres-primary/recent-daily.dump"}},
		{"settings", []string{"settings", "--config", cfgPath}},
		{"check", []string{"check", "--config", cfgPath}},
		{"a config path that is not there", []string{"check", "--config", filepath.Join(root, "absent.yaml")}},
	}

	var lines []string
	for _, c := range cases {
		res, err := runCLI(ctx, bin, c.args, root)
		if err != nil {
			return Cell{}, err
		}
		lines = append(lines, res...)
	}

	return Cell{
		Certifies: "FR-35 clause 4: every one of these commands prints exactly what it printed before EPIC E, and exits the same way. A medium-free deployment has no non-local placement, so FR-35 allows this surface no additive column either.",
		Rule:      RuleIdentical,
		Lines:     lines,
	}, nil
}

// captureCLIRetention pins `retention --dry-run` against a second
// deployment, seeded at whole-day offsets from the current day under a
// daily-only chain.
//
// The reason this is not folded into the table above is in the package
// doc: backup-manager exposes no way to pin its clock, so a multi-tier
// chain's attribution genuinely depends on the calendar date the gate
// runs on. Rather than normalize the verdicts away and keep a cell that
// certifies nothing, this narrows the chain until the verdicts are the
// same on every day of the year: with a single seven-day daily tier and
// records at 1, 2, 40 and 400 whole days back, the first two are inside
// the window and the last two are outside it, no matter what today is.
func captureCLIRetention(ctx context.Context, bin, root string) (Cell, error) {
	dir := filepath.Join(root, "cli-retention")
	for _, sub := range []string{"backups", "exports"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return Cell{}, err
		}
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	midnight := time.Now().UTC().Truncate(24 * time.Hour).Add(9 * time.Hour)
	specs := []seedSpec{
		{name: "one-day-old.dump", state: lifecycle.Complete, discoveredAt: midnight.AddDate(0, 0, -1), content: "inside the daily window"},
		{name: "two-days-old.dump", state: lifecycle.Complete, discoveredAt: midnight.AddDate(0, 0, -2), content: "inside the daily window"},
		{name: "forty-days-old.dump", state: lifecycle.Complete, discoveredAt: midnight.AddDate(0, 0, -40), content: "outside the daily window"},
		{name: "four-hundred-days-old.dump", state: lifecycle.Complete, discoveredAt: midnight.AddDate(0, 0, -400), content: "outside the daily window"},
	}
	if _, _, err := seedDeployment(ctx, dir, dailyOnlyConfigYAML(dir), specs); err != nil {
		return Cell{}, err
	}

	lines, err := runCLI(ctx, bin, []string{"retention", "--dry-run", "--config", cfgPath}, root)
	if err != nil {
		return Cell{}, err
	}
	more, err := runCLI(ctx, bin, []string{"retention", "--config", cfgPath}, root)
	if err != nil {
		return Cell{}, err
	}
	lines = append(lines, more...)

	return Cell{
		Certifies: "FR-20 and FR-35: the retention preview an operator types still reads the same, KEEP/DELETE and tier attribution included, and still says out loud that it deletes nothing.",
		Rule:      RuleIdentical,
		Lines:     lines,
	}, nil
}

func dailyOnlyConfigYAML(root string) string {
	return fmt.Sprintf(`poll_interval: 15m

state:
  database: %s/state.db

sources:
  - id: production
    backup_sets:
      - id: postgres-primary
        remote:
          type: local
        remote_path: %s/exports
        local_path: %s/backups
        completion:
          strategy: stable
          stable_for: 10m
        stale_after: 30h

retention:
  timezone: UTC
  week_starts_on: monday
  protect_last_known_good: false
  tiers:
    - name: daily
      granularity: day
      keep: 7
`, root, root, root)
}

// runCLI executes one invocation and renders it as corpus lines.
//
// Both streams and the exit status, every time. A command that starts
// printing a warning it did not print before has changed what an operator
// sees, and a cell that only looked at stdout would call that identical.
func runCLI(ctx context.Context, bin string, args []string, root string) ([]string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TZ=UTC"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); !ok {
			return nil, fmt.Errorf("running %v: %w", args, err)
		}
		code = exitErr.ExitCode()
	}

	label := "backup-manager " + strings.Join(redactArgs(args, root), " ")
	lines := []string{fmt.Sprintf("$ %s -> exit %d", label, code)}
	for _, l := range splitStream(stdout.String(), root) {
		lines = append(lines, "  out| "+l)
	}
	for _, l := range splitStream(stderr.String(), root) {
		lines = append(lines, "  err| "+l)
	}
	return lines, nil
}

func redactArgs(args []string, root string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, normalizeRoot(a, root))
	}
	return out
}

// splitStream turns a captured stream into corpus lines, keeping trailing
// whitespace visible rather than trimming it: column padding is part of
// what an operator sees, and a gate that trims it would not notice a
// widened column.
func splitStream(s, root string) []string {
	s = normalizeRoot(s, root)
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

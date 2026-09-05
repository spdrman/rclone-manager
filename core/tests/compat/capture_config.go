package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// captureConfigValidation runs every fixture under testdata/configs
// through the exact path the daemon uses (config.Load, then Validate) and
// writes down what came back.
//
// For a refusal that is the error text, because the words a refusal comes
// in are the surface: an operator upgrading into a build that still
// refuses, but refuses differently, has had their runbook changed under
// them.
//
// For an acceptance it is the resolved policy, per backup set, not the
// word "ok". Validate's whole job on this struct is filling in defaults,
// so "it validated" is the one thing about it that could not regress
// interestingly, and #111's promise is specifically about what those
// defaults resolve TO.
func captureConfigValidation(fixtureDir string) (Cell, error) {
	names, err := filepath.Glob(filepath.Join(fixtureDir, "*.yaml"))
	if err != nil {
		return Cell{}, err
	}
	sort.Strings(names)
	if len(names) == 0 {
		return Cell{}, fmt.Errorf("no config fixtures under %s", fixtureDir)
	}

	var lines []string
	for _, path := range names {
		base := filepath.Base(path)
		cfg, err := config.Load(path)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s load: REFUSED %s", base, oneLine(err.Error())))
			continue
		}
		if err := cfg.Validate(); err != nil {
			lines = append(lines, fmt.Sprintf("%s validate: REFUSED %s", base, oneLine(err.Error())))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s validate: accepted", base))
		lines = append(lines, fmt.Sprintf("%s   deployment policy: %s", base, policyLine(cfg.Retention)))
		for _, src := range cfg.Sources {
			for _, bs := range src.BackupSets {
				lines = append(lines, fmt.Sprintf("%s   set %s/%s: %s local_path=%s read_only=%v",
					base, src.Name, bs.Name, policyLine(bs.Retention), bs.LocalPath, bs.ReadOnly))
			}
		}
		lines = append(lines, fmt.Sprintf("%s   storage_mediums declared: %d", base, len(cfg.StorageMediums)))
	}
	return Cell{
		Certifies: "FR-35 clause 1: a medium-free config validates to the same outcome, with the same resolved policy per backup set, and the refusals it can hit still come in the same words.",
		Rule:      RuleIdentical,
		Lines:     lines,
	}, nil
}

// policyLine renders a resolved config.Retention as one deterministic
// line: the chain it decides with, spelled tier by tier, plus the
// calendar and the last-known-good switch.
//
// Every field a tier carries is printed, the medium included. On a
// medium-free config that prints as "-" for every tier, and it is here
// precisely so that a build which starts resolving an absent medium to
// something other than the implicit local one is a red cell rather than a
// silent behavior change.
func policyLine(r config.Retention) string {
	tiers := r.EffectiveTiers()
	parts := make([]string, 0, len(tiers))
	for _, t := range tiers {
		medium := t.EffectiveMedium()
		if medium == "" {
			medium = "-"
		}
		window := t.WindowUnit
		if window == "" {
			window = "-"
		}
		parts = append(parts, fmt.Sprintf("%s(gran=%s,keep=%d,period_days=%d,window=%s,medium=%s)",
			t.Name, t.Granularity, t.Keep, t.PeriodDays, window, medium))
	}
	protect := "unset"
	if r.ProtectLastKnownGood != nil {
		protect = fmt.Sprintf("%v", *r.ProtectLastKnownGood)
	}
	return fmt.Sprintf("tz=%s week_starts=%s protect_lkg=%s chain=[%s]",
		r.Timezone, r.WeekStartsOn, protect, strings.Join(parts, " "))
}

// oneLine folds a multi-line error into one line. Validate reports every
// problem it found at once, and a multi-line value in a line-oriented
// corpus turns one changed message into a shapeless diff.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " | ")), " ")
}

// writeConfig materialises a config for the CLI cells, with the backup
// set's local_path and the state database pointed at a real directory.
func writeConfig(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/service"
)

// cmdSettings is `backup-manager settings` (report the live retention and
// capacity settings FR-18/FR-19/FR-21 are currently deciding with) and
// `backup-manager settings patch [flags]` (change one of them in place,
// hot-reloaded into a running process exactly as `PATCH /api/v1/settings`
// already does -- see core/service.BackupService.UpdateSettings's own
// doc). Issue #277's own investigation confirmed this is not fully
// covered by "edit config.yaml and validate", the answer that already
// covers creating a backup set: GET is a discovery surface a config file
// has no equivalent of, since it reports the RESOLVED policy (defaults
// included) rather than the file's own possibly-omitted keys, and PATCH
// hot-reloads a running daemon without a restart.
//
// PATCH deliberately does not expose a full retention tier-chain
// replacement (core/service.RetentionUpdate.Tiers): replacing the whole
// chain is still, and remains, a config-file edit, exactly like every
// other case the config-file answer already covers (README.md's "Backup
// sets, first-run and enable/disable" note). Every other retention and
// capacity field is here.
func cmdSettings(args []string) int {
	fs, cfgPath := newFlagSet("settings")
	timezone := fs.String("timezone", "", "patch: retention.timezone (an IANA name)")
	weekStartsOn := fs.String("week-starts-on", "", "patch: retention.week_starts_on (a weekday name)")
	protect := fs.Bool("protect-last-known-good", true,
		"patch: retention.protect_last_known_good; pass =false to explicitly disable FR-19 protection, "+
			"which LastKnownGoodDecide treats as a materially more dangerous configuration")
	capBytes := fs.Int64("cap-bytes", 0, "patch: capacity.cap_bytes (0 means no cap)")
	warningFreeBytes := fs.Int64("warning-free-bytes", 0, "patch: capacity.warning_free_bytes (0 means no warning line)")
	criticalFreeBytes := fs.Int64("critical-free-bytes", 0, "patch: capacity.critical_free_bytes (0 means no critical line)")
	safetyMarginBytes := fs.Int64("safety-margin-bytes", 0, "patch: capacity.safety_margin_bytes")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) > 1 || (len(operands) == 1 && operands[0] != "patch") {
		return usageError(`settings: expected no argument, or exactly one argument "patch"`)
	}
	patching := len(operands) == 1

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	if !patching {
		settings, err := svc.Settings(ctx)
		if err != nil {
			return fail(err)
		}
		printSettings(settings)
		return 0
	}

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	req := buildSettingsPatch(fs, timezone, weekStartsOn, protect, capBytes, warningFreeBytes, criticalFreeBytes, safetyMarginBytes)
	settings, err := svc.UpdateSettings(ctx, req)
	if err != nil {
		return fail(err)
	}
	printSettings(settings)
	return 0
}

// buildSettingsPatch reads fs's parsed flag values into an
// UpdateSettingsRequest, using fs.Visit to tell "this flag was never
// passed" apart from "this flag was passed as its zero value" -- load
// bearing for every capacity field (zero is a real meaning: "remove this
// line") and for --protect-last-known-good (an explicit =false has to
// survive), the identical reason retention_flags.go's own
// resolveRetentionFlags reads --protect-last-known-good through fs.Visit
// rather than the flag's own zero value.
func buildSettingsPatch(fs *flag.FlagSet, timezone, weekStartsOn *string, protect *bool, capBytes, warningFreeBytes, criticalFreeBytes, safetyMarginBytes *int64) service.UpdateSettingsRequest {
	var retention service.RetentionUpdate
	var retentionNamed bool
	if *timezone != "" {
		retention.Timezone = timezone
		retentionNamed = true
	}
	if *weekStartsOn != "" {
		retention.WeekStartsOn = weekStartsOn
		retentionNamed = true
	}

	var capacity service.CapacityUpdate
	var capacityNamed bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "protect-last-known-good":
			v := *protect
			retention.ProtectLastKnownGood = &v
			retentionNamed = true
		case "cap-bytes":
			v := *capBytes
			capacity.CapBytes = &v
			capacityNamed = true
		case "warning-free-bytes":
			v := *warningFreeBytes
			capacity.WarningFreeBytes = &v
			capacityNamed = true
		case "critical-free-bytes":
			v := *criticalFreeBytes
			capacity.CriticalFreeBytes = &v
			capacityNamed = true
		case "safety-margin-bytes":
			v := *safetyMarginBytes
			capacity.SafetyMarginBytes = &v
			capacityNamed = true
		}
	})

	req := service.UpdateSettingsRequest{}
	if retentionNamed {
		req.Retention = &retention
	}
	if capacityNamed {
		req.Capacity = &capacity
	}
	return req
}

// printSettings renders the RESOLVED policy service.BackupService.Settings
// (or UpdateSettings) returned: defaults included, a legacy
// daily_days/weekly_months/monthly_months file already expanded into its
// three-tier chain. This is deliberately not a re-serialization of
// config.yaml, which is the whole reason this command exists alongside
// the config-file answer (see cmdSettings' own doc).
func printSettings(s service.Settings) {
	fmt.Println("retention:")
	fmt.Printf("  timezone: %s\n", s.Retention.Timezone)
	fmt.Printf("  week_starts_on: %s\n", s.Retention.WeekStartsOn)
	fmt.Printf("  protect_last_known_good: %v\n", s.Retention.ProtectLastKnownGood)
	fmt.Println("  tiers:")
	for _, t := range s.Retention.Tiers {
		fmt.Printf("    - name=%s granularity=%s keep=%d", t.Name, t.Granularity, t.Keep)
		if t.PeriodDays != 0 {
			fmt.Printf(" period_days=%d", t.PeriodDays)
		}
		if t.WindowUnit != "" {
			fmt.Printf(" window_unit=%s", t.WindowUnit)
		}
		fmt.Println()
	}
	fmt.Println("capacity:")
	fmt.Printf("  cap_bytes: %d\n", s.Capacity.CapBytes)
	fmt.Printf("  warning_free_bytes: %d\n", s.Capacity.WarningFreeBytes)
	fmt.Printf("  critical_free_bytes: %d\n", s.Capacity.CriticalFreeBytes)
	fmt.Printf("  safety_margin_bytes: %d\n", s.Capacity.SafetyMarginBytes)
	fmt.Printf("  backup_root: %s (configured=%v)\n", s.Capacity.BackupRoot, s.Capacity.BackupRootConfigured)
}

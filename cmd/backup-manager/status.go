package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/internal/app"
)

// cmdStatus is `backup-manager status`: FR-24's health surface, rendered
// for a terminal (and, per container/Dockerfile's TODO(#26), for a
// container healthcheck: see this exit-code convention's own doc below).
//
// # Exit code
//
// status exits 0 only when every configured backup set reports HEALTHY
// (health.State.OK()); any DEGRADED, STALE or FAILING set, or an error
// computing the report at all, exits 1. This is deliberate, not
// incidental: it is exactly the signal container/Dockerfile's HEALTHCHECK
// needs once it stops running `version` (which only proves the binary can
// start) and starts running `status` instead (which proves backups are
// actually healthy, the distinction failure-safety invariant 14 insists
// on). See this PR's description for the precise HEALTHCHECK/CMD change
// this enables in container/Dockerfile and docs/deployment.md, both out
// of this PR's file scope.
func cmdStatus(args []string) int {
	fs, cfgPath := newFlagSet("status")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	info := app.BuildVersionInfo(version, commit)
	report, err := svc.BuildHealthReport(ctx, info)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("process: backup-manager %s (commit %s), go %s, rclone %s\n",
		report.Process.BinaryVersion, commit, info.GoVersion, report.Process.RcloneVersion)

	healthy := true
	for _, bs := range report.BackupSets {
		if !bs.State.OK() {
			healthy = false
		}
		fmt.Printf("\n%s: %s\n  %s\n", bs.Set, bs.State, bs.Reason)
		fmt.Printf("  newest known-good backup: %s\n", ageOrNever(bs.NewestGoodBackupAge))
		fmt.Printf("  stale threshold: %s\n", bs.StaleThreshold)
		fmt.Printf("  current transfers: %d, pending deletes: %d, failures: %d\n", len(bs.CurrentTransfers), bs.PendingDeletes, bs.Failures)
		fmt.Printf("  quarantined: %d (of which unrecoverable: %d)\n", bs.QuarantinedCount, bs.QuarantinedLostCount)
		if bs.FreeBytes != nil {
			fmt.Printf("  free space: %d bytes\n", *bs.FreeBytes)
		}
	}

	if !healthy {
		return 1
	}
	return 0
}

func ageOrNever(age *time.Duration) string {
	if age == nil {
		return "never"
	}
	return age.String()
}

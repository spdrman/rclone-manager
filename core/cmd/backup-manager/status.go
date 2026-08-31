package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
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
		// Issue #227. Printed only when there are any, because for most
		// deployments this is permanently zero and a line that always
		// reads zero is a line an operator stops seeing. When it is not
		// zero it needs a sentence, not a number: the count means storage
		// is accumulating on a machine this manager does not measure, and
		// the reader has to be told both that this manager will never
		// reclaim it and that nobody here knows how much it is.
		if bs.ReinstatedRemoteRetainedCount > 0 {
			fmt.Printf("  reinstated, remote source kept: %d\n", bs.ReinstatedRemoteRetainedCount)
			fmt.Printf("    these were re-trusted after quarantine, so this manager will never delete their remote copies.\n")
			fmt.Printf("    how much they occupy on the source is not known here; releasing them is your decision, made there.\n")
		}
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

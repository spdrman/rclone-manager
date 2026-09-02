package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdFetch is `backup-manager fetch --source S --backup-set B [--dry-run]`:
// an operator-triggered, on-demand run of exactly one configured backup
// set's cycle share. See internal/app.Service.Fetch's doc for exactly what
// --dry-run does and does not do (it never touches the journal at all;
// without the flag, Fetch runs the same reconcile/discover/transfer/
// verify/commit/delete sequence RunCycle would for this one backup set).
func cmdFetch(args []string) int {
	fs, cfgPath := newFlagSet("fetch")
	sourceFlag := fs.String("source", "", "the source to fetch (required)")
	setFlag := fs.String("backup-set", "", "the backup set to fetch (required)")
	dryRun := fs.Bool("dry-run", false, "list what discovery would find, without transferring or recording anything")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sourceFlag == "" || *setFlag == "" {
		return usageError("fetch requires --source and --backup-set")
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	if !*dryRun {
		logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))
	}

	result, err := svc.Fetch(ctx, *sourceFlag, *setFlag, *dryRun)
	if err != nil {
		return fail(err)
	}

	if result.DryRun {
		for _, p := range result.Preview {
			known := ""
			if p.Known {
				known = "  (already known)"
			}
			fmt.Printf("%-60s %12d bytes%s\n", p.RemotePath, p.Size, known)
		}
		fmt.Printf("%d object(s) on the remote\n", len(result.Preview))
		return 0
	}

	fmt.Printf("discovered=%d already_known=%d pending=%d rejected=%d conflicts=%d errors=%d failed=%d\n",
		len(result.Discovery.Discovered), len(result.Discovery.AlreadyKnown), len(result.Discovery.Pending),
		len(result.Discovery.Rejected), len(result.Discovery.Conflicts), len(result.Discovery.Errors), result.FailedArtifacts)
	fmt.Printf("reconciliation: %d finding(s), %d error(s)\n", len(result.Reconcile.Findings), len(result.Reconcile.Errors))
	// failed counts artifacts that ended this call in FAILED, QUARANTINED
	// or QUARANTINED_LOST: either a this-cycle transfer/verify/commit
	// failure, or a previously-durable artifact reconciliation (above)
	// found rotten on its own and quarantined before this cycle's own
	// pipeline ever touched it. Neither case is what
	// discoveryOrReconcileFailed sees, since that is a systemic discover/
	// reconcile failure, not a per-artifact outcome, and a reconciliation
	// pass that successfully finds and records rot returns no error at
	// all. Checking it here, from the same count the line above just
	// printed, is what issue #283 asks for: the exit code and the
	// reported number cannot drift apart, because they are the same
	// number. cycleFailed (setup.go) is the identical check `run`
	// (run.go) makes, so the two commands cannot disagree about what a
	// failed cycle is.
	discoveryOrReconcileFailed := len(result.Discovery.Errors) > 0 || len(result.Reconcile.Errors) > 0
	if cycleFailed(discoveryOrReconcileFailed, result.FailedArtifacts) {
		return 1
	}
	return 0
}

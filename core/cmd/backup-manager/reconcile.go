package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdReconcile is `backup-manager reconcile`: an on-demand, operator-
// triggered run of FR-17's reconciliation pass for every configured
// backup set, the same pass `run` and `daemon` already perform first in
// every cycle (see internal/app.RunCycle's doc). It exists for an
// operator who wants to force reconciliation right now (for example
// right after restoring the state database from a backup of its own)
// without waiting for the next scheduled cycle.
func cmdReconcile(args []string) int {
	fs, cfgPath := newFlagSet("reconcile")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	reports := svc.ReconcileAll(ctx)
	exitCode := 0
	for _, r := range reports {
		if r.Err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", r.Set, r.Err)
			exitCode = 1
			continue
		}
		for _, f := range r.Report.Findings {
			if f.Changed() || f.NeedsInvestigation {
				fmt.Printf("%s: %s -> %s: %s\n", f.Artifact, f.From, f.To, f.Reason)
			}
		}
		for _, e := range r.Report.Errors {
			fmt.Fprintf(os.Stderr, "%s: %v\n", e.Artifact, e.Err)
			exitCode = 1
		}
	}
	if exitCode == 0 {
		fmt.Println("reconciliation complete; no unresolved findings")
	}
	return exitCode
}

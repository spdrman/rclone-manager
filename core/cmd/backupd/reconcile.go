package main

import (
	"context"
	"fmt"
	"os"

	"github.com/backupdproject/backupd/core/internal/app"
)

// cmdReconcile is `backupd reconcile`: an on-demand, operator-
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
	needsInvestigation := 0
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
			if f.NeedsInvestigation {
				needsInvestigation++
			}
		}
		for _, e := range r.Report.Errors {
			fmt.Fprintf(os.Stderr, "%s: %v\n", e.Artifact, e.Err)
			exitCode = 1
		}
	}
	// The summary line is conditional on there being nothing left for an
	// operator to read above it (issue #663). "no unresolved findings"
	// used to print whenever exitCode was 0, which is also true of a run
	// that just printed a NeedsInvestigation finding: a settled
	// self-contradictory row is not an error (exitCode stays exactly
	// where two-machine-exit-status.test.sh pins it, moved only by r.Err
	// and Report.Errors), but it is very much an unresolved finding, and
	// a run that says "journal row needs repair" and then "no unresolved
	// findings" in the next breath contradicts itself rather than merely
	// under-reporting.
	switch {
	case exitCode != 0:
		// Already reported above, one line per failure; the exit status
		// carries the signal and nothing more needs saying.
	case needsInvestigation > 0:
		fmt.Printf("reconciliation complete; %d finding(s) need investigation, see above\n", needsInvestigation)
	default:
		fmt.Println("reconciliation complete; no unresolved findings")
	}
	return exitCode
}

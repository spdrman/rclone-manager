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
	// walked/through is issue #361's pair: how many artifacts this fetch
	// had a reason to touch and how many of them ended it with their bytes
	// on local disk. It is the one line above that can tell "there was
	// nothing to fetch" apart from "nothing got through", which the
	// discovery counters cannot, since both of those read discovered=0 on
	// a pass whose candidates were all refused.
	fmt.Printf("walked=%d through=%d\n", result.Walked, result.Durable)
	// The verdict comes from result.Outcome(), built from the same numbers
	// the two lines above just printed, so the exit code and the report an
	// operator reads cannot drift apart. cycleFailed (setup.go) is the
	// identical check `run` (run.go) makes, so the two commands cannot
	// disagree about what a failed cycle is (issue #283, and issue #361's
	// fourth acceptance criterion).
	//
	// Note what is no longer here: this used to fail the cycle on any
	// per-candidate discovery error or per-artifact reconcile error, which
	// `run` never did. That was the two commands disagreeing, in the
	// direction that pages someone because one remote object out of ten
	// was briefly unreadable. Those errors now count towards Walked
	// instead (see app.FetchResult.Outcome), so they still fail the cycle
	// when they are the whole story and no longer do when the pass got
	// real work done around them.
	outcome := result.Outcome()
	if cycleFailed(outcome) {
		reportCycleFailure(outcome)
		return 1
	}
	return 0
}

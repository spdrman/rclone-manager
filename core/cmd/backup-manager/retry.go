package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
)

// cmdRetry is `backup-manager retry <source/backup-set/artifact>`: put one
// FAILED artifact back into the pipeline so it is attempted again (issue
// #419).
//
// A command of its own rather than a fourth `quarantine` verb, for the
// reason internal/app keeps the two methods apart: FAILED is not
// quarantine. Quarantine means a human has to decide whether a backup is
// trustworthy; FAILED means an attempt did not finish. Filing the recovery
// for the second under the vocabulary of the first would put an operator
// looking for a stuck backup on a screen about suspect ones.
//
// It needs a transport, and issue #662 is why: an artifact whose own
// durable local copy sits at its final name can never be re-fetched past
// FR-12's collision guard, so the retry completes that ingestion in place
// by comparing the copy with the remote object it is supposed to be
// (internal/app's completeIngestionInPlace). Opening the Service without
// one is what left `validate` refusing every moved artifact in every
// deployment for a reason no operator could act on; the same mistake here
// would put this artifact straight back into #662's loop.
func cmdRetry(args []string) int {
	fs, cfgPath := newFlagSet("retry")
	note := fs.String("note", "", "operator note recorded with the retry, so a later failure of the same artifact carries what was tried last time")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 1 {
		return usageError("retry: expected <source/backup-set/artifact>")
	}

	id, err := app.ParseArtifactID(operands[0])
	if err != nil {
		return fail(err)
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	if err := svc.RetryFailedIngestion(ctx, id, *note); err != nil {
		return fail(err)
	}

	// The row is read back rather than announced from memory, because
	// there are two outcomes now (#662) and printing the wrong one is a
	// small version of the defect this fix is about: a confident sentence
	// about a state nothing checked. A read that fails is not a failure of
	// the retry, which has already happened, so it degrades to the
	// sentence that was here before.
	settled := lifecycle.Discovered
	if detail, err := svc.GetArtifactDetail(ctx, id); err == nil {
		settled = lifecycle.State(detail.State)
	}
	if settled == lifecycle.Discovered {
		fmt.Printf("%s: re-entering the pipeline (FAILED -> %s)\n", id, settled)
		return 0
	}
	fmt.Printf("%s: its durable local copy was verified against the remote object and trusted in place (FAILED -> %s)\n", id, settled)
	fmt.Println("  this artifact's remote source can never be deleted by this manager again")
	return 0
}

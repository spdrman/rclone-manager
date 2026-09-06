package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
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
// It needs no transport: the whole operation is one journal write, and the
// re-attempt happens on the next cycle like every other DISCOVERED
// artifact.
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
	svc, _, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	if err := svc.RetryFailedIngestion(ctx, id, *note); err != nil {
		return fail(err)
	}
	fmt.Printf("%s: re-entering the pipeline (FAILED -> DISCOVERED)\n", id)
	return 0
}

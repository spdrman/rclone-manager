package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdValidate is `backup-manager validate <source/backup-set/artifact>`:
// an on-demand re-check of one already-committed artifact's durable local
// copy. See internal/app.ValidateArtifact's doc for exactly what it
// checks and why a failure quarantines the artifact with no --dry-run
// guard (the consequence is protective, never destructive).
func cmdValidate(args []string) int {
	fs, cfgPath := newFlagSet("validate")
	// Flags may come before or after the operand; see
	// parseFlagsAroundOperands in setup.go for why.
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 1 {
		return usageError("validate takes exactly one argument: <source/backup-set/artifact>")
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

	result, err := svc.ValidateArtifact(ctx, id)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("%s: checked=%v passed=%v\n  %s\n", id, result.Checked, result.Passed, result.Reason)
	if result.NewState != "" {
		fmt.Printf("  -> %s\n", result.NewState)
	}
	if !result.Passed {
		return 1
	}
	return 0
}

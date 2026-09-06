package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/mediumcheck"
)

// cmdMedium is `backup-manager medium preflight <medium-id>`: prove one
// declared storage medium actually works, before a cycle carrying a real
// backup finds out for the operator (issue #443).
//
// It is the CLI half of POST /api/v1/storage-mediums/{id}/preflight and
// goes through the same internal/app use case, so the two surfaces cannot
// disagree about what a probe found (FR-34). They can still disagree about
// which mediums are declared, because this command reads config.yaml for
// itself and an engine that is already running is serving whatever it
// loaded; see the process-boundary note in backupset.go, and #539 for why
// that distinction is now spelled out rather than left to be inferred.
// One command with a verb,
// like `catalog rebuild` and `quarantine <verb>`, because "preflight" will
// not be the only thing this product ever wants to do to a named medium.
//
// It needs a real transport, unlike most commands here, because reaching a
// bucket is the entire point.
func cmdMedium(args []string) int {
	fs, cfgPath := newFlagSet("medium")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 {
		return usageError("medium: expected preflight <medium-id>")
	}
	verb, id := operands[0], operands[1]
	if verb != "preflight" {
		return usageError("medium: unknown subcommand %q (expected \"preflight\")", verb)
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	report, err := svc.PreflightMedium(ctx, id)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("storage medium %s: %s\n", report.Medium, verdictWord(report.OK))
	for _, c := range report.Checks {
		fmt.Printf("  %-14s %-8s %s\n", c.Step, outcomeWord(c), c.Detail)
	}
	if !report.OK {
		// A non-zero exit, so `medium preflight` composes into a script
		// the way `check` and `validate` already do: an operator wiring
		// this into a deployment step needs the shell to know.
		return 1
	}
	return 0
}

// verdictWord renders the whole report's answer as something an operator
// reads rather than as a boolean.
func verdictWord(ok bool) string {
	if ok {
		return "ready for a backup"
	}
	return "NOT ready; see the failing checks below"
}

// outcomeWord renders one check's outcome, folding in the transport
// category where there is one. The category is the machine-readable half
// and belongs beside the word rather than buried in the sentence: an
// operator scanning this column is deciding whose problem it is.
func outcomeWord(c mediumcheck.Check) string {
	if c.Category == "" {
		return string(c.Outcome)
	}
	return fmt.Sprintf("%s(%s)", c.Outcome, c.Category)
}

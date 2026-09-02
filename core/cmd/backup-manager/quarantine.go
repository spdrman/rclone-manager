package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// cmdQuarantine is `backup-manager quarantine <revalidate|retry|reinstate>
// <source/backup-set/artifact>`: the three operator-triggered quarantine
// actions issue #277's own investigation found had no CLI path at all,
// unlike `validate` (which only ever re-checks a healthy restore point,
// and refuses a QUARANTINED or QUARANTINED_LOST artifact outright -- see
// internal/app.ValidateArtifact's doc). Each verb is a thin wrapper over
// the internal/app.Service method the API layer
// (apps/common/webhost/handlers_artifacts.go) already calls; nothing new
// is decided here.
//
// One command with three verbs, rather than three commands, mirrors
// `catalog rebuild`'s own shape (see cmdCatalog's doc for why flags are
// parsed before the subcommand is resolved).
func cmdQuarantine(args []string) int {
	fs, cfgPath := newFlagSet("quarantine")
	note := fs.String("note", "", "operator note recorded with a reinstatement (reinstate only; ignored otherwise)")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 {
		return usageError("quarantine: expected <revalidate|retry|reinstate> <source/backup-set/artifact>")
	}
	verb, idArg := operands[0], operands[1]
	switch verb {
	case "revalidate", "retry", "reinstate":
	default:
		return usageError("quarantine: unknown subcommand %q (expected \"revalidate\", \"retry\" or \"reinstate\")", verb)
	}

	id, err := app.ParseArtifactID(idArg)
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

	switch verb {
	case "revalidate":
		return cmdQuarantineRevalidate(ctx, svc, id)
	case "retry":
		return cmdQuarantineRetry(ctx, svc, id)
	default:
		return cmdQuarantineReinstate(ctx, svc, id, *note)
	}
}

// cmdQuarantineRevalidate re-runs the durable-local-copy checks against a
// QUARANTINED or QUARANTINED_LOST artifact and reports the verdict,
// writing nothing (internal/app.Service.RevalidateQuarantined's own doc
// explains why a verdict here never moves the artifact anywhere, unlike
// `validate`, which is the same check on a healthy artifact and DOES
// quarantine it on a failure).
func cmdQuarantineRevalidate(ctx context.Context, svc *app.Service, id model.ArtifactID) int {
	result, err := svc.RevalidateQuarantined(ctx, id)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%s: checked=%v passed=%v\n  %s\n", id, result.Checked, result.Passed, result.Reason)
	if !result.Passed {
		return 1
	}
	return 0
}

// cmdQuarantineRetry puts a QUARANTINED artifact back into DISCOVERED so
// the ordinary pipeline can attempt it again. A QUARANTINED_LOST artifact
// is refused (app.ErrQuarantineIrrecoverable): there is nothing left to
// re-fetch from, and `reinstate` is the action that serves that case
// instead.
func cmdQuarantineRetry(ctx context.Context, svc *app.Service, id model.ArtifactID) int {
	if err := svc.RetryQuarantinedIngestion(ctx, id); err != nil {
		return fail(err)
	}
	fmt.Printf("%s: re-entering the pipeline (QUARANTINED -> DISCOVERED)\n", id)
	return 0
}

// cmdQuarantineReinstate re-checks a quarantined artifact's durable local
// copy and, if the evidence is enough, returns it to the state it already
// held (QUARANTINED -> COMMITTED, or QUARANTINED_LOST -> COMPLETE). See
// internal/app.Service.ReinstateQuarantined's own doc for the full
// contract, in particular that a reinstated artifact never authorises a
// remote delete again.
func cmdQuarantineReinstate(ctx context.Context, svc *app.Service, id model.ArtifactID, note string) int {
	result, err := svc.ReinstateQuarantined(ctx, id, note)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%s: checked=%v passed=%v\n  %s\n", id, result.Checked, result.Passed, result.Reason)
	if !result.Reinstated {
		return 1
	}
	fmt.Printf("  -> %s (reinstated; this artifact's remote source can never be deleted by this manager again)\n", result.NewState)
	return 0
}

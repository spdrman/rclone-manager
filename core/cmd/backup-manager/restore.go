package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/service"
)

// defaultRestoreWindowDays is the window a caller who names none asks for.
//
// Seven days, because the two failure modes are not symmetric. A window
// that expires while somebody is still copying the data out means paying
// for the whole restore again from the start; a window that is longer than
// it needed to be means a few days of the restored copy being billed as
// additional storage. The first is worse, and a week is long enough to
// cover a restore that finishes overnight and gets acted on the next
// working day.
const defaultRestoreWindowDays = 7

// cmdRestore is `backup-manager restore <source/backup-set/artifact>
// --medium M [--days N] --acknowledge`: EPIC E, FR-34's explicit restore,
// on a terminal.
//
// # Why the CLI has this at all
//
// FR-34 says the CLI mirrors the same vocabulary as the UI, so a person on
// a terminal and a person in a browser read the same truth about the same
// backup. `artifacts <id>` already prints each copy's access state and
// says, for an archived one, that its bytes cannot be read until a restore
// is asked for and finishes. This is the verb that answers that sentence,
// and its absence would have left the CLI able to state a problem it had
// no way to act on.
//
// # --acknowledge, and why it is not --force
//
// It is a required flag, and it is the whole "make an accidental restore
// hard" mechanism. A restore costs money and takes hours, and neither is
// visible from the shell history afterwards. Spelled as --force the
// default would be the cheap one only for people who remembered to think
// about it; spelled this way round, the operator who ran the command
// without reading it gets a refusal and a sentence explaining what they
// were about to buy.
func cmdRestore(args []string) int {
	fs, cfgPath := newFlagSet("restore")
	medium := fs.String("medium", "", "id of the storage medium holding the copy to restore (required)")
	days := fs.Int("days", defaultRestoreWindowDays, "how many days the restored copy stays readable (1 to 30)")
	acknowledge := fs.Bool("acknowledge", false, "confirm that this is billed by the provider and takes hours (required)")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 1 {
		return usageError("restore: expected <source/backup-set/artifact>")
	}
	if *medium == "" {
		return usageError("restore: --medium is required; `backup-manager artifacts %s` lists this backup's copies and the medium each one is on", operands[0])
	}
	if !*acknowledge {
		return usageError("restore: --acknowledge is required. A restore is billed by the storage provider and takes hours to finish, and neither shows up in your shell history afterwards, so this asks once rather than assuming")
	}
	if _, err := app.ParseArtifactID(operands[0]); err != nil {
		return fail(err)
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	sub, err := svc.SubmitRestorePlacement(ctx, service.RestorePlacementRequest{
		// A fresh key per invocation. A CLI run is a deliberate act by a
		// person at a keyboard, so two runs are two requests, and reusing
		// a key across them would silently return the first run's
		// operation to somebody who meant to start another.
		IdempotencyKey: "cli-restore:" + uuid.NewString(),
		Actor:          "cli",
		ConfigRevision: svc.ConfigRevision(),
		ArtifactID:     operands[0],
		Medium:         *medium,
		WindowDays:     *days,
		Acknowledged:   true,
	})
	if err != nil {
		return fail(err)
	}

	printRestoreSubmission(sub)
	return 0
}

// printRestoreSubmission says what was started, in the words FR-34 allows.
//
// No percentage, no finishing time, no amount. What it does say is the
// class's own published figure for how long a restore takes, worded so it
// cannot be read as a countdown, and the fact that a bill exists.
func printRestoreSubmission(sub service.RestoreSubmission) {
	if !sub.Created {
		fmt.Printf("operation:           %s (already submitted; nothing new was started and nothing new was billed)\n", sub.Operation.ID)
	} else {
		fmt.Printf("operation:           %s\n", sub.Operation.ID)
	}
	fmt.Printf("status:              %s\n", sub.Operation.Status)
	fmt.Printf("window:              %d days\n", sub.WindowDays)
	if sub.Wait != "" {
		fmt.Printf("how long:            %s\n", sub.Wait)
	}
	if sub.Billing != "" {
		fmt.Printf("cost:                %s\n", sub.Billing)
	}
	fmt.Printf("check on it with:    backup-manager status, or GET /api/v1/operations/%s\n", sub.Operation.ID)
}

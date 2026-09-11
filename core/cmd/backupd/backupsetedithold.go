package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spdrman/backupd/core/apicontract"
	"github.com/spdrman/backupd/core/internal/config"
)

// cmdBackupSetEditHold is `backupd backup-set edit-hold
// <source/backup-set> [--release]`.
//
// # The gap this closes
//
// The three edit-hold routes have existed since #350 and nothing outside a
// browser could reach any of them. So a hold could be taken from the Web
// UI and released from nowhere else, and the one action an operator most
// needs from a terminal, giving back a hold somebody left behind, had no
// command at all. That also meant the docked terminal (#599) could print
// no equivalent command for it, which is the parity rule failing visibly
// rather than at an audit nobody runs.
//
// # Why this is routed, and refuses without a route
//
// A hold is a lease held IN MEMORY by the process that is serving this
// deployment (core/service's edithold.go says why it is deliberately not
// durable: a hold that survived a restart would leave a set paused after a
// crash with no way out but a second restart). So with nothing serving,
// there is no hold, and there is nothing on disk that could be consulted
// or changed. This refuses rather than answering "not held" from a world
// where the question has no meaning, for the same reason
// `activity --follow` refuses: an answer from the wrong place is worse
// than no answer, because it looks like an answer.
//
// # Report by default, release on a flag
//
// The default is a read, so an operator can see whether a set is held and
// what taking the hold interrupted before deciding to do anything about
// it. --release is the write, and it is a flag rather than a second verb
// because the two are the same question asked with and without an action:
// releasing prints the resulting state as well, so one invocation both
// acts and reports what it left behind.
//
// TAKING a hold has no command, and that is a decision rather than an
// omission. A hold protects an edit SESSION from a cycle running
// underneath it, and a CLI edit is one `backup-set patch` that either runs
// or does not, so there is no session to protect; a verb that could take
// one would only be able to pause a backup set with nothing to notice it
// had meant to.
func cmdBackupSetEditHold(args []string) int {
	fs, cfgPath := newFlagSet("backup-set edit-hold")
	release := fs.Bool("release", false,
		"give the hold back, so the scheduler may run this backup set again rather than waiting for the lease to lapse")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	// The verb itself is operands[0]: cmdBackupSet hands every handler
	// the whole argument list, so a flag written before the verb is not
	// silently dropped. See backupSetVerbs' own doc.
	if len(operands) != 2 || operands[0] != "edit-hold" {
		return usageError(`backup-set edit-hold: expected "edit-hold <source/backup-set>" and exactly one backup set id`)
	}
	id := operands[1]
	source, set, ok := splitBackupSetID(id)
	if !ok {
		return usageError("backup-set edit-hold: %q is not a backup set id; a backup set id is exactly source/name", id)
	}

	cfg, err := config.LoadAndValidate(*cfgPath)
	if err != nil {
		return fail(fmt.Errorf("config: %w", err))
	}

	ctx := context.Background()
	// The same door every read goes through: it announces which world this
	// answer is about and refuses the three disagreements before anything
	// is printed. It takes no claim, which is right here: an edit hold is
	// not configuration, so there is nothing on disk for a claim to
	// protect.
	mode, err := enterReadMode(ctx, *cfgPath, cfg, os.Stderr)
	if err != nil {
		return fail(err)
	}
	if !mode.attached() {
		return fail(fmt.Errorf("an edit hold is held in memory by the process serving this deployment, and this command has no route to one, so there is nothing to report and nothing to release. Set BACKUP_MANAGER_API_URL and the two credential variables beside it to reach that process; with nothing serving at all there is no hold, because a hold does not survive the process that took it"))
	}

	if *release {
		if err := mode.client.ReleaseBackupSetEditHold(ctx, source, set); err != nil {
			return fail(fmt.Errorf("releasing the edit hold on %s: %w", id, err))
		}
	}

	state, err := mode.client.GetBackupSetEditHold(ctx, source, set)
	if err != nil {
		return fail(mode.unanswered("whether "+id+" is held for editing", err))
	}
	printEditHold(state, *release)
	return 0
}

// printEditHold is the report, and it says what is true rather than what
// was asked for.
//
// After a --release it prints the state read back from the engine, not an
// assumed one: a release that succeeded and was immediately followed by
// somebody else taking the hold is a real sequence, and a command that
// printed "held: false" from its own optimism would be lying about the
// deployment at the moment it was asked.
func printEditHold(state apicontract.BackupSetEditHoldState, released bool) {
	if released {
		fmt.Println("released")
	}
	fmt.Printf("%s\n", state.BackupSetID)
	fmt.Printf("  held: %v\n", state.Held)
	if state.ExpiresAt != "" {
		fmt.Printf("  expires_at: %s\n", state.ExpiresAt)
	}
	// Absent rather than zeroed, on purpose: the contract makes `running`
	// null when no cycle is inside this set, and printing an empty
	// artifact and an empty stage would read as a pass that is running and
	// has not said what it is doing.
	if state.Running == nil {
		fmt.Printf("  running: nothing\n")
		return
	}
	fmt.Printf("  running: %s (%s)\n", state.Running.Artifact, state.Running.Stage)
}

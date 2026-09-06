package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdStatus is `backup-manager status`: FR-24's health surface, rendered
// for a terminal (and, per container/Dockerfile's TODO(#26), for a
// container healthcheck: see this exit-code convention's own doc below).
//
// # Exit code
//
// status exits 0 only when every configured backup set reports HEALTHY
// (health.State.OK()); any DEGRADED, STALE or FAILING set, or an error
// computing the report at all, exits 1. This is deliberate, not
// incidental: it is exactly the signal container/Dockerfile's HEALTHCHECK
// needs once it stops running `version` (which only proves the binary can
// start) and starts running `status` instead (which proves backups are
// actually healthy, the distinction failure-safety invariant 14 insists
// on). See this PR's description for the precise HEALTHCHECK/CMD change
// this enables in container/Dockerfile and docs/deployment.md, both out
// of this PR's file scope.
func cmdStatus(args []string) int {
	fs, cfgPath := newFlagSet("status")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	info := app.BuildVersionInfo(version, commit)
	report, err := svc.BuildHealthReport(ctx, info)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("process: backup-manager %s (commit %s), go %s, rclone %s\n",
		report.Process.BinaryVersion, commit, info.GoVersion, report.Process.RcloneVersion)

	healthy := true
	for _, bs := range report.BackupSets {
		if !bs.State.OK() {
			healthy = false
		}
		fmt.Printf("\n%s: %s\n  %s\n", bs.Set, bs.State, bs.Reason)
		fmt.Printf("  newest known-good backup: %s\n", ageOrNever(bs.NewestGoodBackupAge))
		fmt.Printf("  stale threshold: %s\n", bs.StaleThreshold)
		fmt.Printf("  current transfers: %d, pending deletes: %d, failures: %d\n", len(bs.CurrentTransfers), bs.PendingDeletes, bs.Failures)
		fmt.Printf("  quarantined: %d (of which unrecoverable: %d)\n", bs.QuarantinedCount, bs.QuarantinedLostCount)
		// Issue #227. Printed only when there are any, because for most
		// deployments this is permanently zero and a line that always
		// reads zero is a line an operator stops seeing. When it is not
		// zero it needs a sentence, not a number: the count means storage
		// is accumulating on a machine this manager does not measure, and
		// the reader has to be told both that this manager will never
		// reclaim it and that nobody here knows how much it is.
		if bs.ReinstatedRemoteRetainedCount > 0 {
			fmt.Printf("  reinstated, remote source kept: %d\n", bs.ReinstatedRemoteRetainedCount)
			fmt.Printf("    these were re-trusted after quarantine, so this manager will never delete their remote copies.\n")
			fmt.Printf("    how much they occupy on the source is not known here; releasing them is your decision, made there.\n")
		}
		// Issue #282. Printed only when there are any, for the same reason
		// as the reinstated line above: most deployments never set
		// read_only and this stays permanently zero. When it is not zero,
		// this manager is doing exactly what a read-only source was
		// declared for, and the line says so rather than looking like an
		// oddly-stalled pending-delete count.
		if bs.ReadOnlyRetainedCount > 0 {
			fmt.Printf("  remote retained, read-only source: %d\n", bs.ReadOnlyRetainedCount)
			fmt.Printf("    this backup set is declared read-only: this manager will never delete these remote copies.\n")
		}
		// Issue #444, FR-24's placement half. Printed only when there is
		// something to say, for the reason the two lines above are: most
		// deployments declare no storage medium at all and a line that
		// permanently reads zero is a line an operator stops seeing.
		//
		// When there IS something, the failing relocation comes first and
		// gets the age, because the count on its own is the half of the
		// sentence that was already visible for one pass in the cycle
		// report. How long it has been failing is the half that was not,
		// and it is what separates a transient upload error from a bucket
		// policy nobody has looked at since a Tuesday in July.
		if bs.Placement.FailedMoves > 0 {
			fmt.Printf("  relocations failing: %d, the oldest for %s\n",
				bs.Placement.FailedMoves, bs.Placement.OldestFailedMoveAge.Round(time.Minute))
			fmt.Printf("    %s\n", bs.Placement.FailedMoveReason)
			fmt.Printf("    the backups themselves are fine; they are not on the medium your retention chain asks for.\n")
		}
		if bs.Placement.AwayFromHome > 0 {
			fmt.Printf("  away from home: %d (oldest copy %s)\n",
				bs.Placement.AwayFromHome, ageOrUnknown(bs.Placement.OldestAwayFromHomeAge))
		}
		if bs.Placement.OpenMoves > 0 {
			fmt.Printf("  relocations open: %d, the oldest for %s\n",
				bs.Placement.OpenMoves, ageOrUnknown(bs.Placement.OldestOpenMoveAge))
		}
		if bs.Placement.UnconfirmedLocation > 0 {
			fmt.Printf("  location not confirmed: %d\n", bs.Placement.UnconfirmedLocation)
			fmt.Printf("    these are mid-move or have no durable copy recorded yet; neither is a claim that they are where they belong.\n")
		}
		if bs.FreeBytes != nil {
			fmt.Printf("  free space: %d bytes\n", *bs.FreeBytes)
		}
	}

	// Issue #418: the backup sets no configuration names, which FR-24's
	// report cannot carry because health is computed per configured set
	// and a removed set has no freshness to assess.
	//
	// It belongs on this screen anyway. `status` is where an operator
	// asks "is anything wrong here", and a category of backup that is
	// growing, ungoverned and invisible to every maintenance pass is the
	// kind of thing that answer should mention. It does NOT change the
	// exit code: nothing has failed, and a healthcheck that started
	// flapping because an operator removed a backup set would teach
	// people to ignore it. Same reasoning, and the same shape, as the
	// reinstated and read-only lines above.
	unconfigured, err := svc.UnconfiguredSets(ctx)
	if err != nil {
		// Said out loud rather than swallowed, and still not fatal. This
		// read is an addition to a report that has already been built
		// successfully, so failing the whole command over it would turn
		// a healthy deployment's healthcheck red for a question nobody
		// asked before this issue existed.
		fmt.Fprintf(os.Stderr, "could not check for backups outside every configured set: %v\n", err)
	}
	if len(unconfigured) > 0 {
		fmt.Printf("\nretained outside every backup set this configuration names: %d set(s)\n", len(unconfigured))
		for _, u := range unconfigured {
			fmt.Printf("  %s: %d artifact(s), %d byte(s), under no retention policy\n", u.Set, u.Artifacts, u.Bytes)
		}
		fmt.Println("  nothing collects, retains, reconciles or deletes these; `backup-manager unconfigured` says what to do about it.")
	}

	if !healthy {
		return 1
	}
	return 0
}

// ageOrUnknown is ageOrNever's counterpart for an age that is missing
// because this pass could not read it, rather than because the thing it
// would measure has never happened. "never" and "unknown" are different
// claims and only one of them is available here (see
// health.PlacementHealth.OldestAwayFromHomeAge, which is nil in a race
// between two reads of a live database).
func ageOrUnknown(age *time.Duration) string {
	if age == nil {
		return "age unknown"
	}
	return age.Round(time.Minute).String()
}

// ageOrNever renders an age whose absence means the thing never happened,
// which is a different absence from ageOrUnknown's above.
//
// Nil here is a fact about the deployment and not about the read: nothing has
// ever been verified, or nothing has ever been backed up. Rendering it as
// "age unknown" would tell an operator the number could not be obtained when
// what is true is that there is no number to obtain.
func ageOrNever(age *time.Duration) string {
	if age == nil {
		return "never"
	}
	return age.String()
}

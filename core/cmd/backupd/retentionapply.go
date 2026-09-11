package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/spdrman/backupd/core/cliecho"
	"github.com/spdrman/backupd/core/internal/app"
	"github.com/spdrman/backupd/core/service"
)

// cmdRetentionApply is `backupd retention apply
// <source/backup-set> --acknowledge`: FR-20's deletion, from a terminal
// (issue #602).
//
// # Why this verb exists now and did not before
//
// `retention` previews in both its modes and says so, and the note it
// prints has been right about where the deletion lives: core/service's
// preview/apply envelope, over HTTP, against a plan_id somebody reviewed.
// What that note did not say is that the route carrying it sits behind a
// destructive gate whose only shipped implementation returns false, so
// FR-20's deletion has never run in any deployment this project has
// shipped. Local copies accumulate until FR-21's capacity refusal starts
// refusing transfers and backups stop landing, which is a reliability
// failure produced by a gate being shut.
//
// `retention` itself is still a preview and this is deliberately a
// separate verb rather than a flag on it, for the reason that command's
// own doc gives about --dry-run: a surface where the destructive form is
// the one you get by leaving something out is the wrong way round.
//
// # The confirmation, and what it is and is not
//
// --acknowledge is required, and it is the same mechanism `restore`
// already uses for the same kind of reason: the cost is invisible from
// the shell history afterwards. It is emphatically NOT the API's
// confirmation. Over HTTP a plan_id is issued, rendered for an
// administrator, and applied against a fingerprint of exactly what they
// were shown; here the preview and the apply are one invocation and the
// person is the one who typed it. What the two share is the refusal
// underneath: this verb applies through the identical
// PreviewRetention/ApplyRetentionPlan pair, so a plan whose inventory,
// configuration or civil date moved between the preview and the apply is
// refused with RETENTION_PLAN_STALE and zero files are deleted, exactly
// as it would be over the wire. The window is small and it is real: a
// cycle finishing in another process writes the very journal rows the
// comparison is computed over.
//
// # readsConfig, beside a running engine
//
// This is `restore`'s position and it is taken for `restore`'s reason: a
// retention apply writes a durable operation row into the journal, which
// is SQLite and shared, so a running engine sees it exactly as it sees an
// artifact, and nothing here rewrites config.yaml. Refusing beside a live
// engine would take the verb away from every deployment that has one,
// which is every deployment that is working.
//
// What that leaves is one process deleting files out of a directory
// another process writes into, and the answer to it is not this file's:
// internal/retention re-derives every FR-20 safety check against the real
// disk immediately before each os.Remove, so a file that arrived, moved
// or became something else since the plan was made is refused rather than
// removed. See internal/retention/prune.go's PruneApply.
func cmdRetentionApply(args []string) int {
	fs, cfgPath := newFlagSet("retention apply")
	acknowledge := fs.Bool("acknowledge", false,
		"confirm that this deletes local restore points that no retention tier keeps (required)")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	// operands[0] is the verb itself: cmdRetention hands every verb the
	// whole argument list, the way cmdBackupSet does, so that a flag
	// written before the verb is not silently dropped.
	if len(operands) != 2 || operands[0] != "apply" {
		return usageError(`retention apply: expected "apply <source/backup-set>" and exactly one backup set id`)
	}
	source, name, ok := splitBackupSetID(operands[1])
	if !ok {
		return usageError("retention apply: %q is not a backup set id; a backup set id is exactly source/name", operands[1])
	}
	if !*acknowledge {
		return usageError("retention apply: --acknowledge is required. This deletes the local copy of every backup no retention tier keeps, and a deleted restore point is not recoverable from here, so this asks once rather than assuming. `"+cliecho.Binary+" retention %s` prints what it would delete", operands[1])
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	// The preview is not decoration and it is not a second decision: it
	// is what issues the plan_id the apply below is checked against, and
	// printing it is what leaves an operator a record of the reasoning
	// the deletion was authorised under. A refusal here has deleted
	// nothing, because nothing has been applied yet.
	plan, err := svc.PreviewRetention(ctx, source, name)
	if err != nil {
		return fail(err)
	}
	printRetentionPlan("about to apply", plan)

	applied, err := svc.ApplyRetentionPlan(ctx, service.ApplyRetentionRequest{
		PlanID: plan.PlanID,
		Source: source,
		Set:    name,
		Actor:  "cli",
	})
	if err != nil {
		return failRetentionApply(err)
	}

	printRetentionPlan("applied", applied)
	if applied.OperationID != "" {
		fmt.Printf("operation: %s\n", applied.OperationID)
	}
	return 0
}

// printRetentionPlan renders one plan's verdicts.
//
// Deliberately its own renderer rather than `retention`'s printVerdictLine
// (retention.go), because the two are about different things and the
// difference matters at exactly the moment somebody is deciding whether
// to delete. That one prints FR-18/FR-19 classification, which has two
// outcomes; this one prints FR-20's verdict, which has three, and REFUSE
// is the whole reason: "no tier keeps this" and "no tier keeps this and
// it is not safe to remove" render identically otherwise, and the second
// is the one an operator has to go and look at.
//
// The reason travels with a REFUSE and with nothing else. A KEEP's reason
// names the tier, which the tiers column beside it already says, and a
// DELETE's is the same sentence on every line; a REFUSE's is the only one
// carrying a fact this deployment did not already know.
func printRetentionPlan(what string, plan service.RetentionPlan) {
	fmt.Printf("%s: %s plan %s\n", plan.BackupSetID, what, plan.PlanID)
	verdicts := append([]service.RetentionArtifactVerdict(nil), plan.Verdicts...)
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].Artifact < verdicts[j].Artifact })
	if len(verdicts) == 0 {
		fmt.Println("  (no managed, completed backups yet)")
		return
	}
	for _, v := range verdicts {
		fmt.Printf("  %-6s %-40s%s\n", v.Action, v.Artifact, retentionMediumSuffix(v.Medium))
		if v.Action == "REFUSE" {
			fmt.Printf("    ! %s\n", v.Reason)
		}
	}
	fmt.Printf("  %d kept, %d to delete, %d byte(s)\n", plan.KeepCount, plan.DeleteCount, plan.ReclaimBytes)
}

// retentionMediumSuffix is the ` medium=` half of a verdict line, and it
// is absent for a local copy for the reason retention.go's own
// mediumSuffix gives: "DELETE 40 artifacts" needs qualifying when half of
// them are objects in a bucket somebody else pays for, and needs nothing
// when they are files on this machine.
func retentionMediumSuffix(medium string) string {
	switch medium {
	case "", "local":
		return ""
	default:
		return " medium=" + medium
	}
}

// failRetentionApply renders the refusals this verb can meet, and exists
// because the default rendering of two of them is actively misleading.
//
// core/service deliberately does not wrap an internal error with its
// cause on this path, so a caller that printed whatever came back would
// show "an internal error occurred" for a plan that went stale, which
// reads as a broken deployment rather than as "look again and re-run",
// and sends the operator somewhere useless. The two sentences here say
// the one thing that is true of both and is the reason an operator can
// stop worrying: nothing was deleted.
func failRetentionApply(err error) int {
	switch {
	case errors.Is(err, service.ErrRetentionPlanStale):
		fmt.Fprintf(os.Stderr,
			cliecho.Binary+": this backup set changed between the preview above and the apply, so nothing was deleted. That is the guard working: what would run is no longer what was printed. Run the command again to decide against what it holds now (%v)\n", err)
		return exitFailure
	case errors.Is(err, service.ErrRetentionApplyBusy):
		fmt.Fprintf(os.Stderr,
			cliecho.Binary+": this deployment is running a cycle right now, so nothing was deleted. A cycle writes the journal rows a retention decision is made from, so an apply waits for it rather than deciding against a moving inventory. Try again once it is finished (%v)\n", err)
		return exitFailure
	default:
		return fail(err)
	}
}

// retentionVerbs is what `retention` dispatches on its first non-flag
// word, and it is a table for the reason mediumVerbs is one: a verb added
// here is a verb TestUsage_NamesEveryRetentionVerb requires usage() to
// list, which then makes TestUsage_EveryRegisteredCommandIsPinned require
// its entry line to be captured. Registering it is the only step somebody
// has to remember; the other two are checked.
var retentionVerbs = map[string]func([]string) int{
	"apply": cmdRetentionApply,
}

// retentionVerbNames is that table's keys, sorted, for usage() and for
// the test that holds usage() to it.
func retentionVerbNames() []string {
	out := make([]string, 0, len(retentionVerbs))
	for name := range retentionVerbs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

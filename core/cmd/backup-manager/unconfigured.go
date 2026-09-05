package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// cmdUnconfigured is `backup-manager unconfigured` and `backup-manager
// unconfigured clear <source/backup-set>`: issue #418's operator-facing
// half.
//
// Removing a backup set (#391) keeps its backups on storage and keeps
// them listed, which is what the confirmation promises. What it also does
// is take them outside every maintenance pass this manager runs, because
// retention, reconcile, health, capacity and discovery all walk the
// configuration. internal/app/unconfigured.go argues that lifecycle in
// full; this command is where a person can actually see it.
//
// # Why this exists as a command rather than only a log line
//
// The failure this issue is really about is the one #411 names for the
// adoption event: something important happened, it was written down, and
// nobody read it. A category of backup that is invisible, ungoverned and
// growing needs a surface an operator can ask, not an event in a stream
// they may or may not be shipping. So the list is a command, the same
// numbers reach `retention` (which is where "under what policy" is asked)
// and `status` (which is where "is anything wrong" is asked), and the
// event stream carries it too rather than instead.
//
// # What clear does, and what it deliberately will not do
//
// It removes the .partial residue of rows a stopped cycle stranded, and
// ends those rows. It does not touch a backup, and it does not offer to:
// there is no policy behind a removed set that could authorise a
// deletion, and this product does not destroy data because a
// configuration file changed. An operator who wants those bytes aged out
// creates the set again and lets its own retention chain do it under
// FR-20's identity checks; an operator who wants them gone deletes them.
// See internal/app/unconfigured.go for why clearing a .partial destroys
// nothing at all.
//
// It previews by default and changes nothing without --acknowledge, the
// same shape `restore` uses and for the same reason: the flag is there so
// the operator says the destructive half out loud, not so they can skip a
// warning.
func cmdUnconfigured(args []string) int {
	// The verb is found anywhere in the argument list, not only at the
	// front, so a flag may sit on either side of it: `unconfigured
	// --config X clear a/b` runs against X, exactly as cmdBackupSet's own
	// verb dispatch already allows. Slicing the verb off the front
	// instead would silently drop every flag written before it, which
	// reads as a command that ran against the wrong configuration file
	// rather than as an error.
	for _, a := range args {
		if a == "clear" {
			return cmdUnconfiguredClear(args)
		}
	}

	fs, cfgPath := newFlagSet("unconfigured")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return usageError("unconfigured takes no arguments; did you mean `unconfigured clear %s`?", fs.Arg(0))
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	sets, err := svc.UnconfiguredSets(ctx)
	if err != nil {
		return fail(err)
	}
	if len(sets) == 0 {
		fmt.Println("every backup set on record is configured; nothing is retained outside a retention policy")
		return 0
	}

	for _, u := range sets {
		printUnconfiguredSet(u)
	}
	fmt.Printf("\n%d backup set(s) the journal remembers and the configuration does not.\n", len(sets))
	return 0
}

// printUnconfiguredSet renders one set's block.
//
// It names the policy explicitly rather than leaving it to be inferred
// from the absence of one, which is issue #418's third acceptance
// criterion: "the surface that lists those backups says which policy
// governs them, including none". An operator reading a list of backups
// with no policy line has no way to tell "kept under the deployment's
// chain" from "kept because nothing is looking".
func printUnconfiguredSet(u app.UnconfiguredSet) {
	fmt.Printf("%s: %d artifact(s), %d retained, %d stranded, %d quarantined, %d byte(s) on storage\n",
		u.Set, u.Artifacts, u.Retained, u.Stranded, u.Quarantined, u.Bytes)
	fmt.Println("  retention policy: none. This backup set's configuration was removed, so no policy ages these")
	fmt.Println("  backups out and nothing here will ever delete them. Create the set again to put them back")
	fmt.Println("  under a policy, or remove the files yourself.")
	if u.Quarantined > 0 {
		fmt.Printf("  %d quarantined artifact(s) cannot be revalidated, retried or reinstated while the set is unconfigured.\n", u.Quarantined)
	}
	if u.Stranded > 0 {
		fmt.Printf("  %d row(s) were left mid-acquisition and no cycle will advance them: `backup-manager unconfigured clear %s`\n", u.Stranded, u.Set)
	}
}

// cmdUnconfiguredClear is `backup-manager unconfigured clear
// <source/backup-set> [--acknowledge]`.
func cmdUnconfiguredClear(args []string) int {
	fs, cfgPath := newFlagSet("unconfigured clear")
	acknowledge := fs.Bool("acknowledge", false, "actually clear the residue; without it this prints what would be cleared and changes nothing")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 || operands[0] != "clear" {
		return usageError("unconfigured clear takes exactly one argument: <source/backup-set>")
	}

	source, set, ok := splitBackupSetID(operands[1])
	if !ok {
		return usageError("unconfigured clear: %q is not a <source/backup-set> id", operands[1])
	}
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		return fail(err)
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	var found []app.StrandedArtifact
	if *acknowledge {
		found, err = svc.ClearStranded(ctx, id)
	} else {
		found, err = svc.StrandedArtifacts(ctx, id)
	}
	if err != nil {
		// The one refusal worth rewording. "This backup set is
		// configured" is the whole answer, and an operator who reads it
		// needs to know that the rows it is refusing to touch are not
		// abandoned: the cycle owns them and will resume them.
		if errors.Is(err, app.ErrBackupSetConfigured) {
			fmt.Fprintf(os.Stderr, "%s is still configured. Its in-flight artifacts belong to the processing cycle, which resumes them on its own; there is nothing stranded to clear.\n", id)
			return 1
		}
		return fail(err)
	}

	if len(found) == 0 {
		fmt.Printf("%s: nothing stranded; every artifact on record is either a finished backup or already terminal\n", id)
		return 0
	}

	cleared, refused := 0, 0
	for _, s := range found {
		switch {
		case s.Err != nil:
			fmt.Fprintf(os.Stderr, "%s (%s): left alone: %v\n", s.Artifact, s.State, s.Err)
			refused++
		case s.Cleared:
			fmt.Printf("%s (%s): cleared %d byte(s) of .partial residue at %s; the row is now FAILED\n",
				s.Artifact, s.State, s.PartialBytes, s.PartialPath)
			cleared++
		default:
			fmt.Printf("%s (%s): would clear %d byte(s) of .partial residue at %s\n",
				s.Artifact, s.State, s.PartialBytes, s.PartialPath)
		}
	}

	if !*acknowledge {
		fmt.Printf("\n%d stranded row(s). Nothing has been changed; pass --acknowledge to clear them.\n", len(found))
		return 0
	}
	fmt.Printf("\n%d row(s) cleared, %d left alone. The backups this set collected are untouched and still listed.\n", cleared, refused)
	if refused > 0 {
		return 1
	}
	return 0
}

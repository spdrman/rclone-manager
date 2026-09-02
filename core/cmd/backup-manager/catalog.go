package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdCatalog is `backup-manager catalog`, so far just one subcommand,
// section 71 Work Package 3.3's `catalog rebuild` / `catalog rebuild
// --dry-run` (EPIC-B section 19.3, issue #102).
//
// The flags are parsed BEFORE the subcommand is resolved, and the
// subcommand is whichever operand that parse left behind, so
// `catalog --config X rebuild` and `catalog rebuild --config X` are the
// same command (issue #213). Reading args[0] first made the second form
// the only one that worked and answered the first with `unknown
// subcommand "--config"`, a message that names the wrong thing: the
// subcommand was fine, the parser had simply not reached it. That is
// #188's finding on `validate` restated, and it is fixed with #188's own
// helper, parseFlagsAroundOperands (setup.go), which every other command
// in this binary already resolves its flags through.
//
// One flag set carries both `catalog`'s --config and `rebuild`'s
// --dry-run, rather than one per level. Resolving a subcommand out of the
// operands means telling an operand apart from a flag's VALUE
// (`--config X rebuild`: X is a value and rebuild is the subcommand), and
// only a flag set that knows every flag can do that. With one subcommand
// that is the whole story; a second one carrying a flag of its own would
// register it here too.
func cmdCatalog(args []string) int {
	fs, cfgPath := newFlagSet("catalog")
	dryRun := fs.Bool("dry-run", false, "report what would be reconstructed; write nothing")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	if len(operands) == 0 {
		return usageError("catalog: missing subcommand (expected \"rebuild\")")
	}
	switch operands[0] {
	case "rebuild":
		return cmdCatalogRebuild(*cfgPath, *dryRun)
	default:
		return usageError("catalog: unknown subcommand %q (expected \"rebuild\")", operands[0])
	}
}

// cmdCatalogRebuild reconstructs a lost or corrupted FR-9 journal from the
// non-secret sidecar recovery manifests EPIC-B section 19.3 requires every
// committed artifact to carry, for every configured backup set.
//
// --dry-run reports exactly what a real run would reconstruct and writes
// nothing; internal/app.RebuildCatalog's own doc explains why the two
// modes share one code path and, absent a crash between them, predict
// each other exactly.
//
// Like `check`, this never contacts a configured remote: rebuild only
// ever reads sidecar manifests already on disk and writes to the local
// journal, so openService is called with withTransport=false.
func cmdCatalogRebuild(configPath string, dryRun bool) int {
	ctx := context.Background()
	svc, cfg, cleanup, err := openService(ctx, configPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	verb := "reconstructed"
	if dryRun {
		verb = "would reconstruct"
	}

	exitCode := 0
	total := 0
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			report, err := svc.RebuildCatalog(ctx, bs.ID, dryRun)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", bs.ID, err)
				exitCode = 1
				continue
			}
			for _, f := range report.Findings {
				switch f.Action {
				case app.CatalogRebuildReconstructed:
					fmt.Printf("%s: %s %s\n", bs.ID, verb, f.Artifact)
					for _, n := range f.Notes {
						fmt.Printf("    %s\n", n)
					}
					total++
				case app.CatalogRebuildAlreadyPresent:
					fmt.Printf("%s: %s already has a journal row, left untouched\n", bs.ID, f.Artifact)
				case app.CatalogRebuildConflict:
					// Reported, never resolved (FR-32): the journal row is
					// left exactly as untouched as in the plain
					// already-present case above, in a dry run and in a
					// real one alike. It goes to stderr and sets a
					// non-zero exit because a sidecar disagreeing with the
					// journal is something an operator has to look at, and
					// a line on stdout in the middle of a long rebuild is
					// how that gets missed.
					fmt.Fprintf(os.Stderr, "%s: %s already has a journal row and the sidecar disagrees with it; nothing was changed:\n", bs.ID, f.Artifact)
					for _, c := range f.Conflicts {
						fmt.Fprintf(os.Stderr, "    %s\n", c)
					}
					exitCode = 1
				}
			}
			for _, e := range report.Errors {
				fmt.Fprintf(os.Stderr, "%s: %s: %v\n", bs.ID, e.Path, e.Err)
				exitCode = 1
			}
		}
	}

	if dryRun {
		fmt.Printf("dry-run complete: would reconstruct %d artifact(s); nothing was written\n", total)
	} else {
		fmt.Printf("catalog rebuild complete: reconstructed %d artifact(s)\n", total)
	}
	return exitCode
}

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
func cmdCatalog(args []string) int {
	if len(args) == 0 {
		return usageError("catalog: missing subcommand (expected \"rebuild\")")
	}
	switch args[0] {
	case "rebuild":
		return cmdCatalogRebuild(args[1:])
	default:
		return usageError("catalog: unknown subcommand %q (expected \"rebuild\")", args[0])
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
func cmdCatalogRebuild(args []string) int {
	fs, cfgPath := newFlagSet("catalog rebuild")
	dryRun := fs.Bool("dry-run", false, "report what would be reconstructed; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	svc, cfg, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	verb := "reconstructed"
	if *dryRun {
		verb = "would reconstruct"
	}

	exitCode := 0
	total := 0
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			report, err := svc.RebuildCatalog(ctx, bs.ID, *dryRun)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", bs.ID, err)
				exitCode = 1
				continue
			}
			for _, f := range report.Findings {
				switch f.Action {
				case app.CatalogRebuildReconstructed:
					fmt.Printf("%s: %s %s\n", bs.ID, verb, f.Artifact)
					total++
				case app.CatalogRebuildAlreadyPresent:
					fmt.Printf("%s: %s already has a journal row, left untouched\n", bs.ID, f.Artifact)
				}
			}
			for _, e := range report.Errors {
				fmt.Fprintf(os.Stderr, "%s: %s: %v\n", bs.ID, e.Path, e.Err)
				exitCode = 1
			}
		}
	}

	if *dryRun {
		fmt.Printf("dry-run complete: would reconstruct %d artifact(s); nothing was written\n", total)
	} else {
		fmt.Printf("catalog rebuild complete: reconstructed %d artifact(s)\n", total)
	}
	return exitCode
}

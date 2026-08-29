package main

import (
	"context"
	"fmt"
)

// cmdRetention is `backup-manager retention` / `backup-manager retention
// --dry-run`: FR-20's mandatory dry-run, wired to internal/retention's
// classification (GFS + last-known-good) via internal/app.
//
// # Both flags behave identically today, and this says so
//
// internal/retention contains no deletion function at all as of this PR:
// FR-20 (the actual, positively-identified local file removal these
// verdicts would drive) is issue #21, open and being worked concurrently.
// So `retention` and `retention --dry-run` print the exact same
// KEEP/DELETE preview either way, and this command says that explicitly
// rather than let the absence of --dry-run silently imply a destructive
// action that does not exist in this codebase yet. Once issue #21 lands a
// real delete function, wiring `retention` (without --dry-run) to actually
// call it is a small, well-scoped follow-up this comment is deliberately
// easy to find and delete.
func cmdRetention(args []string) int {
	fs, cfgPath := newFlagSet("retention")
	dryRun := fs.Bool("dry-run", false, "preview only; see this command's note below about the other mode")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	reports, err := svc.RetentionPreviewAll(ctx)
	if err != nil {
		return fail(err)
	}

	for _, r := range reports {
		fmt.Printf("%s:\n", r.Set)
		if len(r.Verdicts) == 0 {
			fmt.Println("  (no managed, completed backups yet)")
			continue
		}
		for _, v := range r.Verdicts {
			decision := "DELETE"
			if v.Keep {
				decision = "KEEP"
			}
			fmt.Printf("  %-6s %-40s tiers=%v\n", decision, v.Artifact.Name, v.Tiers)
		}
		fmt.Printf("  last-known-good: %s\n", r.LastKnownGood.Reason)
	}

	if !*dryRun {
		fmt.Println("\nnote: local deletion (FR-20) is not implemented anywhere in this codebase yet (issue #21 is open); this is a preview only, identical to --dry-run.")
	}
	return 0
}

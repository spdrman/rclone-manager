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
//
// # Retention override flags (issue #111, B3.6)
//
// This command also accepts one optional flag per FR-18/FR-19 field
// (--timezone, --week-starts-on, --daily-days, --weekly-months,
// --monthly-months, --protect-last-known-good): see
// registerRetentionFlags and applyRetentionOverrides (retention_flags.go)
// for exactly how each is folded onto the loaded config's own resolved
// retention.Config and re-validated. None of them are ever persisted back
// to the config file; they change only this one invocation's preview, the
// same way --dry-run already only ever affects this one invocation. An
// operator who wants a policy change to survive past one preview still
// edits the YAML file, exactly as before this issue.
func cmdRetention(args []string) int {
	fs, cfgPath := newFlagSet("retention")
	dryRun := fs.Bool("dry-run", false, "preview only; see this command's note below about the other mode")
	rf := registerRetentionFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	svc, cfg, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	// Issue #111 (B3.6): fold in whichever of the six FR-18/FR-19 flags
	// the operator actually passed, validated through the identical
	// config.ValidateRetention path the YAML file's own retention block
	// goes through, so an invalid override is refused for the same
	// reason an invalid file value would be. An operator who passes none
	// of these flags gets cfg.Retention completely untouched: exactly
	// today's file-only behavior. cfg is the same *config.Config pointer
	// svc was built from (see openService's doc), so this mutation is
	// this command's own, one-time, explicit preview-input step, not
	// ambient state Service itself ever reads or writes.
	overrides := resolveRetentionFlags(rf)
	if err := applyRetentionOverrides(&cfg.Retention, overrides); err != nil {
		return fail(fmt.Errorf("retention flags: %w", err))
	}

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

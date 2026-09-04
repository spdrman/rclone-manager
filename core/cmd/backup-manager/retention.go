package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
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
//
// These flags override the deployment's policy. A backup set that
// declares its own (issue #333) is not moved by them: see the
// re-resolution step below for why that is a decision rather than an
// accident.
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

	// Folding onto cfg.Retention is only half the step, and on its own it
	// is a silent no-op: since issue #333 every decision reads a backup
	// set's own resolved bs.Retention, which Validate computed from the
	// pre-override global policy, so nothing downstream would ever see
	// these flags. Re-running Validate re-resolves each set from the
	// folded policy. It is safe to run twice by its own doc (every default
	// it fills in is only applied to a field still at its zero value), and
	// TestPerSetRetention_OverrideSurvivesRepeatedValidate pins that for
	// exactly this field.
	//
	// A set that declares its own retention block keeps it, because
	// resolution reads that block again on this pass. That is the intended
	// answer, not a side effect: these flags override the DEPLOYMENT's
	// policy for one invocation, and an operator who wrote a retention
	// block against one specific backup set wrote it about that set, not
	// about this command line. An operator who wants to preview a
	// different chain for such a set edits the set.
	if err := cfg.Validate(); err != nil {
		return fail(fmt.Errorf("retention flags: %w", err))
	}

	reports, err := svc.RetentionPreviewAll(ctx)
	if err != nil {
		return fail(err)
	}

	for _, r := range reports {
		// Issue #333: name the policy only when the set overrides the
		// deployment's, and name what that policy actually IS: "this
		// set's own policy" tells an operator where to go and edit, and
		// the chain tells them what they will find when they get there.
		//
		// An inheriting set prints exactly what it printed before this
		// field existed, which matters because this output is pinned by
		// the black-box contract suite in spdrman/rclone-manager-tests
		// (suites/cli/cases/retention/), and inheriting is what every case
		// there does. That asymmetry is a real limitation, not a design:
		// absence of a marker is the only signal for the common case, and
		// an inheriting set's chain is not named at all. Naming it on both
		// branches means moving those pinned cases in lockstep with this
		// change, which is a cross-repo move worth making on its own
		// rather than folded into this one.
		if r.RetentionIsOverride {
			fmt.Printf("%s: (retained under this set's own policy: %s)\n", r.Set, retentionPolicySummary(r.Retention))
		} else {
			fmt.Printf("%s:\n", r.Set)
		}
		if len(r.Verdicts) == 0 {
			fmt.Println("  (no managed, completed backups yet)")
			continue
		}
		for _, v := range r.Verdicts {
			decision := "DELETE"
			if v.Keep {
				decision = "KEEP"
			}
			// Each entry of Tiers is a retention.GFSTierSelection, whose
			// String renders the tier and the placement that selected it,
			// so this line reads `tiers=[DAILY(discovery) MONTHLY(both)]`
			// (issue #218). The rendering deliberately lives on that type
			// rather than here: FR-20's own KEEP reason sentence spells a
			// selection the same way, and two renderers would eventually
			// spell it differently. This line is pinned by the black-box
			// contract suite in spdrman/rclone-manager-tests
			// (suites/cli/cases/retention/), so changing its shape means
			// moving those cases in lockstep.
			fmt.Printf("  %-6s %-40s tiers=%v\n", decision, v.Artifact.Name, v.Tiers)
			// Issue #292: tiers=[] alone cannot tell an operator "no tier
			// claimed this because it is older than every window" apart
			// from "no tier claimed this because a sibling in the same
			// bucket won" -- both render identically otherwise. Every
			// GFSSiblingCollision GFSDecide recorded against this
			// artifact prints as its own indented line right under the
			// verdict it belongs to, so the distinction is visible before
			// anything is deleted, which is this issue's whole ask.
			for _, line := range v.SiblingCollisionLines() {
				fmt.Printf("    ! %s\n", line)
			}
		}
		fmt.Printf("  last-known-good: %s\n", r.LastKnownGood.Reason)
		printPlacementPlan(cfg, r)
	}

	if !*dryRun {
		fmt.Println("\nnote: local deletion (FR-20) is not implemented anywhere in this codebase yet (issue #21 is open); this is a preview only, identical to --dry-run.")
	}
	return 0
}

// printPlacementPlan is FR-27's half of the mandatory dry-run (EPIC E,
// issue #239): every artifact this pass would MOVE, and where to, before
// a cycle carries it there.
//
// # It prints nothing in a deployment with no storage medium
//
// That is a compatibility decision with a reason outside this repository.
// This command's output is pinned by the black-box contract suite in
// spdrman/rclone-manager-tests (suites/cli/cases/retention/), and every
// case there is a medium-free deployment; adding a line to those means
// moving them in lockstep with this change, across two repositories. It
// is also the honest answer: a deployment with exactly one place to put
// anything has nothing to say about placement, and the "could not confirm
// where this is" line in particular would fire for every artifact whose
// journal row predates FR-29's placement table while meaning nothing,
// because there is nowhere else it could be.
//
// The same asymmetry the RetentionIsOverride line above already uses, for
// the same reason, and with the same real limitation: absence is the only
// signal for the common case.
func printPlacementPlan(cfg *config.Config, r app.RetentionSetReport) {
	if len(cfg.StorageMediums) == 0 {
		return
	}
	plan := r.HomePlan
	if len(plan.Moves) == 0 && len(plan.Unconfirmed) == 0 {
		return
	}
	fmt.Println("  placement:")
	for _, m := range plan.Moves {
		// The same column shape as the verdict lines above, so the two
		// read as one table rather than as a report with an appendix.
		fmt.Printf("    %-6s %-40s %s -> %s\n", "MOVE", m.Artifact.Name, m.From, m.To)
	}
	for _, a := range plan.Unconfirmed {
		// Deliberately its own line rather than a MOVE with a blank
		// source. "I could not confirm where this is" and "this is
		// already where it belongs" produce the same silence otherwise,
		// and they are different facts: one of them is a move already in
		// flight, and the other is a journal row with no placement at
		// all. Neither is moved, and an operator acts differently on
		// each.
		fmt.Printf("    %-6s %-40s nothing could confirm where its durable copy is, so it stays put\n", "?", a.Name)
	}
}

// retentionPolicySummary renders a resolved policy as one line: the chain
// it decides with, and the calendar it reckons that chain in.
//
// Tier names are spelled the way the config file spells them, lower case,
// rather than the way the per-artifact tiers= line spells them (upper
// case, because those strings are API surface that reaches a client). This line is not a verdict, it is a pointer at the block an
// operator would go and edit, so it should read like that block.
//
// The timezone is on this line rather than left implicit because it is
// the field an override is most likely to get wrong: omitting it used to
// resolve a set to UTC inside a deployment that had deliberately set
// something else, which silently moves which civil day a restore point
// belongs to.
func retentionPolicySummary(r config.Retention) string {
	tiers := r.EffectiveTiers()
	parts := make([]string, 0, len(tiers))
	for _, t := range tiers {
		parts = append(parts, fmt.Sprintf("%s/%d", t.Name, t.Keep))
	}
	return fmt.Sprintf("tiers=[%s] timezone=%s", strings.Join(parts, " "), r.Timezone)
}

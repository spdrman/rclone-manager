package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
)

// cmdRetention is `backup-manager retention` / `backup-manager retention
// --dry-run`: FR-20's mandatory dry-run, wired to internal/retention's
// classification (GFS + last-known-good) via internal/app.
//
// # Both modes are previews, and the note says so
//
// This command computes and prints. It deletes nothing with --dry-run and
// nothing without it, so the flag is accepted and inert here, and the note
// printed when it is absent says exactly that rather than letting the
// missing flag imply a destructive run.
//
// That is a decision, not a gap, and the note has to be read that way
// (issue #431). FR-20's deletion is real and has been since issue #21
// landed: internal/retention.PruneApply removes the positively identified
// local file, and since #239 the object on a storage medium beside it,
// driven by internal/app's PruneApply/PruneApplySnapshot. What runs that
// is core/service's preview/apply envelope over HTTP, where
// PreviewRetention issues a plan_id and ApplyRetentionPlan refuses to
// delete anything unless the plan it re-derives still fingerprints as the
// one that plan_id was issued for.
//
// Wiring `retention` without --dry-run to PruneApply would put a second
// authorisation path beside that one, and a weaker one: a bare
// `retention` would start deleting backups with no plan for anyone to
// have read and nothing to compare against at the moment of deletion.
// FR-20's whole design is that a deletion is authorised against a plan
// somebody reviewed, so this command staying a preview is the answer
// rather than an unfinished follow-up. If a CLI apply is ever wanted, it
// needs a confirmation story of its own first.
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
//
// # The operand, and what happens when it names nothing (issue #568)
//
// With no argument this previews every configured backup set, which is
// what it has always done. With one <source/backup-set> it previews that
// one set, and an id that names no configured set is refused with nothing
// printed at all.
//
// The refusal is the half that matters most. Previewing the wrong set is
// a wrong answer an operator can catch, because the heading names a set
// they did not ask about; answering about a configured set for an id that
// resolved to nothing is a wrong answer with nothing in it to catch. This
// is the command somebody reads to decide whether a deletion is safe, so
// an id it cannot place is a refusal rather than a best effort.
func cmdRetention(args []string) int {
	fs, cfgPath := newFlagSet("retention")
	dryRun := fs.Bool("dry-run", false, "accepted and inert: this command previews in both modes, and says so on its own output")
	rf := registerRetentionFlags(fs)
	// The operand may be written on either side of the flags, exactly
	// like every other command's (see parseFlagsAroundOperands in
	// setup.go for why). A plain fs.Parse stops at the first argument
	// that is not a flag and leaves it in fs.Args() for a caller to read,
	// which is how this command came to accept an id and then quietly
	// preview something else (issue #568).
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	only, code := retentionOperand(operands)
	if code != exitOK {
		return code
	}

	ctx := context.Background()
	svc, cfg, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	// Issue #544, and the order here is load-bearing: the mode is decided
	// against the configuration as LOADED, before the override flags below
	// are folded onto it. The revision this comparison rests on is a hash
	// of the configuration's content, so deciding after the fold would
	// make `retention --daily-days 5` refuse beside every engine, for the
	// difference the operator asked for.
	overrides := resolveRetentionFlags(rf)
	mode, err := enterReadMode(ctx, *cfgPath, cfg, os.Stderr)
	if err != nil {
		return fail(err)
	}

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

	reports, err := retentionReports(ctx, svc, only)
	if err != nil {
		return fail(err)
	}

	// A preview under flags this command supplied is a hypothetical, and
	// asking the serving process to agree with a hypothetical it was never
	// told about would refuse every overridden preview. So the engine is
	// asked about the verdicts only when the policy in force is the
	// deployment's own, and when it is not, the reader is told once that
	// nothing checked these.
	//
	// The configuration itself was still compared, above, before the fold:
	// an overridden preview beside an engine holding a different
	// configuration is refused like any other read.
	hypothetical := overridden(overrides)
	if hypothetical && mode.attached() {
		fmt.Fprintln(os.Stderr, "note: these verdicts are under a retention policy this command line supplied, not the one this deployment is configured with, so the process serving it was not asked to agree with them.")
	}
	if !hypothetical {
		for _, r := range reports {
			var mine []string
			for _, v := range r.Verdicts {
				action := "DELETE"
				if v.Keep {
					action = "KEEP"
				}
				// The artifact's NAME, not its full id: a retention plan
				// is scoped to one backup set and the wire spells its
				// verdicts that way (core/service's summarizeRetentionPlan),
				// which is also how the line printed below spells them.
				mine = append(mine, v.Artifact.Name+" "+action)
			}
			if err := mode.agreeOnRetention(ctx, r.Set, mine); err != nil {
				return fail(err)
			}
		}
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
			printVerdictLine(v, r.Locations)
		}
		fmt.Printf("  last-known-good: %s\n", r.LastKnownGood.Reason)
		printPlacementPlan(cfg, r)
	}

	// Printed only when --dry-run is absent, because that is the only
	// invocation whose shape suggests something else happened. An
	// operator who typed --dry-run already knows what they asked for.
	// See this command's doc for why both modes preview and why that is
	// a decision rather than a gap (issue #431).
	// Issue #418: the backup sets no configuration names at all.
	//
	// This command's whole subject is which policy applies to what, and
	// it walks the configuration, so a removed set's backups were absent
	// from it entirely. Absent reads as "nothing here to think about",
	// which is the opposite of the truth: those backups are retained by
	// nothing, aged out by nothing, and pinned until somebody deletes
	// them by hand.
	//
	// Printed only when there is something to print, so a deployment
	// that has never removed a backup set gets byte-identical output to
	// the one it got before. That is not only taste: this command's
	// output is pinned by the black-box contract suite in
	// spdrman/rclone-manager-tests (suites/cli/cases/retention/), and
	// every case there is a configured-sets-only deployment.
	//
	// Only for the whole-deployment form. An operator who named one set
	// asked about that set, and a list of other sets nothing governs is
	// an answer to a question they did not ask; the form with no operand
	// is where that list belongs, and `unconfigured` is the command that
	// is entirely about it.
	unconfigured, err := unconfiguredForPreview(ctx, svc, only)
	if err != nil {
		return fail(err)
	}
	if len(unconfigured) > 0 {
		fmt.Println("\nretained under no policy at all (issue #418):")
		for _, u := range unconfigured {
			fmt.Printf("  %s: %d artifact(s), %d retained, %d byte(s) on storage; this backup set's configuration was removed,\n", u.Set, u.Artifacts, u.Retained, u.Bytes)
			fmt.Println("    so no retention chain selects or expires these and nothing here will ever delete them.")
			fmt.Printf("    Create %s again to put them back under a policy: `backup-manager unconfigured` explains the rest.\n", u.Set)
		}
	}

	// The other side of that decision, which the decision on its own left
	// open. Keeping the list off the one-set form is right, and it means
	// an operator who types `retention prod/db` out of habit and never
	// types it bare never meets the list at all. What the list is about is
	// backups nothing retains, reconciles or expires, which are the ones
	// most worth knowing about and the least likely to announce
	// themselves, so silence here is a worse answer than a line.
	//
	// A pointer rather than the list, so the shape of the answer still
	// matches the shape of the question: it says the list exists, says how
	// many are on it and says how to see it, and names none of them.
	//
	// Printed only when there is something behind it, which is the rule
	// the appendix above already follows and for the same reason: a
	// deployment that has never removed a backup set prints exactly what
	// it printed before this line existed.
	ungoverned, err := ungovernedElsewhere(ctx, svc, only)
	if err != nil {
		return fail(err)
	}
	if ungoverned > 0 {
		fmt.Printf("\nthis preview is about one backup set, so it leaves out %d backup set(s) whose configuration was removed and which no retention policy governs at all. `backup-manager retention` with no argument lists those (issue #418).\n", ungoverned)
	}
	if !*dryRun {
		fmt.Println("\nnote: this command only previews. It deletes nothing in either mode, so --dry-run changes nothing here. FR-20 deletion runs through the API's retention preview/apply pair, which will not delete without the plan_id of a plan an administrator reviewed.")
	}
	return 0
}

// retentionOperand reads the optional <source/backup-set> this command
// takes, returning the zero id when there is none and an exit code when
// what was typed is not one (issue #568).
//
// Two refusals, and they are different failures on purpose. Two ids, or
// one thing that is not shaped like an id at all, is a usage mistake and
// exits 2: nothing ran and what has to change is the command line. A
// well-formed id that names no configured backup set is a set that is not
// there, which the usage block's own exit-code table gives to 1, so that
// one is left to the service a step later and arrives in the same
// sentence every other command refuses an unknown set with. `backup-set
// retention` splits the two the same way, which matters because it is the
// same operand, spelled the same way, on a command an operator moves to
// and from.
//
// # The rule the split comes from
//
// Two rows of a published table are not enough on their own: 2 is
// "nothing ran, the command line was wrong" and 1 is "an ordinary
// failure", and read as prose those overlap. The line between them is
// whether this deployment had to be consulted to know. A string that is
// not shaped like a backup set id is wrong on every deployment there will
// ever be, so it is answered here, before a configuration is loaded or a
// journal is opened, and it is a 2. A set the configuration does not have
// is only wrong on this one, so it is a 1 and it waits for the service. An
// answer that is true and empty is a 0 and always was: a configured set
// with no finished backups under it yet prints that it has none and exits
// 0, which is what keeps "there are none yet" from reading as "they are
// gone".
//
// Two places that rule is applied narrowly rather than literally, and both
// times because the narrow reading is what the majority of this binary's
// other commands already do. A malformed ARTIFACT id is a 1: `validate`,
// `retry`, `quarantine`, `restore` and `artifacts <id>` have all answered
// that way since long before the table existed, and moving five commands
// is a bigger change than settling this one. A flag VALUE that parses and
// then fails validation is a 1 too, which is what --daily-days -1 and
// --timezone Mars/Phobos get here: they go through the identical
// config.ValidateRetention the YAML file's own retention block goes
// through, and `settings patch --timezone Mars/Phobos` and `backup-set
// retention --daily-days -1` both answer with whatever that validation
// says, on the same row.
//
// An exit code returned rather than an error, like buildRetentionOverride
// (backupsetretention.go): these are argument problems this package owns
// and prints itself, not service refusals for fail() to render.
func retentionOperand(operands []string) (model.BackupSetID, int) {
	switch len(operands) {
	case 0:
		return model.BackupSetID{}, exitOK
	case 1:
		source, name, ok := splitBackupSetID(operands[0])
		if !ok {
			return model.BackupSetID{}, usageError("retention: %q is not a backup set id; a backup set id is exactly source/name", operands[0])
		}
		return model.BackupSetID{Source: source, Set: name}, exitOK
	default:
		return model.BackupSetID{}, usageError("retention takes at most one argument: <source/backup-set>")
	}
}

// retentionReports is the preview this invocation is about: every
// configured backup set, or the one the operand named.
//
// The single-set path goes through the same RetentionPreview
// RetentionPreviewAll calls for each of its own sets, so one set's report
// is computed the identical way whether it was asked for by name or
// reached by walking the configuration. It is also what refuses an id
// naming no configured set, with the *app.NotFoundError every other
// command's unknown-set refusal already carries, so this command grows no
// second vocabulary for the same fact.
func retentionReports(ctx context.Context, svc *app.Service, only model.BackupSetID) ([]app.RetentionSetReport, error) {
	if only.IsZero() {
		return svc.RetentionPreviewAll(ctx)
	}
	report, err := svc.RetentionPreview(ctx, only)
	if err != nil {
		return nil, err
	}
	return []app.RetentionSetReport{report}, nil
}

// unconfiguredForPreview is the issue #418 appendix, and nothing at all
// when this invocation is about one named backup set.
func unconfiguredForPreview(ctx context.Context, svc *app.Service, only model.BackupSetID) ([]app.UnconfiguredSet, error) {
	if !only.IsZero() {
		return nil, nil
	}
	return svc.UnconfiguredSets(ctx)
}

// ungovernedElsewhere is how many backup sets the appendix above would
// have listed, for the one invocation shape that does not get the
// appendix, and zero for the one that does.
//
// The mirror image of the function above rather than a widening of it, so
// each shape asks the journal exactly one question and the two cannot
// both run. Keeping them apart is also what keeps #418's decision legible:
// that one decides what the LIST is, this one decides what the pointer at
// the list is, and the reason the one-set form gets a pointer instead of a
// list is unchanged by having one.
func ungovernedElsewhere(ctx context.Context, svc *app.Service, only model.BackupSetID) (int, error) {
	if only.IsZero() {
		return 0, nil
	}
	sets, err := svc.UnconfiguredSets(ctx)
	if err != nil {
		return 0, err
	}
	return len(sets), nil
}

// printVerdictLine renders one artifact's verdict.
//
// Each entry of Tiers is a retention.GFSTierSelection, whose String
// renders the tier and the placement that selected it, so this line reads
// `tiers=[DAILY(discovery) MONTHLY(both)]` (issue #218). The rendering
// deliberately lives on that type rather than here: FR-20's own KEEP
// reason sentence spells a selection the same way, and two renderers
// would eventually spell it differently. This line is pinned by the
// black-box contract suite in spdrman/rclone-manager-tests
// (suites/cli/cases/retention/), so changing its shape means moving those
// cases in lockstep.
//
// # medium=, and why it is absent rather than "local"
//
// FR-30 asks this dry-run to explain per-artifact WHERE a deletion would
// happen, not only whether, and the answer is where the copy is. It is
// appended only when that is somewhere other than the implicit local
// medium, so an artifact on local prints exactly the line it printed
// before this field existed, which is every artifact in every one of
// those pinned cases. It is also the honest asymmetry: "DELETE 40
// artifacts" needs qualifying when half of them are objects in a bucket
// somebody else pays for, and needs nothing when they are files on this
// machine.
//
// A location nothing could confirm prints `medium=?`, because a deletion
// this manager cannot place is a different thing from one it can, and
// both are different from one on local.
func printVerdictLine(v retention.GFSVerdict, locations map[model.ArtifactID]retention.Location) {
	decision := "DELETE"
	if v.Keep {
		decision = "KEEP"
	}
	fmt.Printf("  %-6s %-40s tiers=%v%s\n", decision, v.Artifact.Name, v.Tiers, mediumSuffix(locations[v.Artifact]))
	// Issue #292: tiers=[] alone cannot tell an operator "no tier claimed
	// this because it is older than every window" apart from "no tier
	// claimed this because a sibling in the same bucket won" -- both
	// render identically otherwise. Every GFSSiblingCollision GFSDecide
	// recorded against this artifact prints as its own indented line right
	// under the verdict it belongs to, so the distinction is visible
	// before anything is deleted, which is that issue's whole ask.
	for _, line := range v.SiblingCollisionLines() {
		fmt.Printf("    ! %s\n", line)
	}
}

// mediumSuffix is the ` medium=...` half of the line above, or nothing.
func mediumSuffix(loc retention.Location) string {
	switch {
	case loc.Status == retention.LocationConfirmed && loc.Medium != config.MediumLocal:
		return " medium=" + loc.Medium
	case loc.Status == retention.LocationContested:
		return " medium=?"
	default:
		return ""
	}
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

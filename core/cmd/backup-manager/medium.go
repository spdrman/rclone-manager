package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/mediumcheck"
)

// mediumVerbs is every verb `medium` dispatches, keyed by the word an
// operator types in front of the medium id.
//
// A table rather than the string literal this command used to compare
// against, because a literal is invisible to everything that checks this
// binary's surface. TestUsage_EveryRegisteredCommandIsPinned reads the
// dispatch map in main.go, which holds one entry for `medium` and cannot
// see a level below it, so a second verb added here with no line in
// usage() would be undiscoverable to an operator and pinned by nothing,
// which is the failure #549 was filed about and the very example that
// test's own doc reaches for. `backup-set` has had backupSetVerbNames
// since #391 for the same reason. TestUsage_NamesEveryMediumVerb holds
// this table against usage(), so the next verb is caught without anybody
// remembering that test is there.
//
// A handler takes the opened service rather than the raw argument list,
// which is where this differs from backupSetVerbs. Every medium verb
// names one medium and has to reach it, so the flag set, the operand
// shape and the open are shared and a verb decides only what to do once
// it is there. A verb that wants flags of its own gets backupSetVerbs'
// treatment instead, and the two shapes can sit side by side the way
// they already do under `backup-set`.
var mediumVerbs = map[string]func(ctx context.Context, svc *app.Service, id string) int{
	"preflight": mediumPreflight,
}

// mediumVerbNames is every verb `medium` dispatches, sorted, so a refusal
// that lists them reads the same way twice.
func mediumVerbNames() []string {
	names := make([]string, 0, len(mediumVerbs))
	for verb := range mediumVerbs {
		names = append(names, verb)
	}
	sort.Strings(names)
	return names
}

// quotedMediumVerbs renders the table the way this command's refusal has
// always named it: `"preflight"` with one verb, `"a" or "b"` with two,
// `"a", "b" or "c"` with more, which is the shape quarantine.go spells
// out by hand. Rendered rather than typed, so a verb added above turns up
// in the refusal without a second edit somebody has to remember.
func quotedMediumVerbs() string {
	names := mediumVerbNames()
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, strconv.Quote(name))
	}
	switch len(quoted) {
	case 0:
		// Unreachable while mediumVerbs has an entry, and a sentence is a
		// better way to find out it does not than an index panic.
		return "no verb at all, which means mediumVerbs is empty"
	case 1:
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

// cmdMedium is `backup-manager medium <verb> <medium-id>`, which today is
// `medium preflight <medium-id>`: prove one declared storage medium
// actually works, before a cycle carrying a real backup finds out for the
// operator (issue #443).
//
// It is the CLI half of POST /api/v1/storage-mediums/{id}/preflight and
// goes through the same internal/app use case, so the two surfaces cannot
// disagree about what a probe found (FR-34). They can still disagree about
// which mediums are DECLARED, because this command reads config.yaml for
// itself while an engine that is already running is serving whatever it
// loaded at startup. What can no longer put them into that state is this
// binary: a configuration write from here either goes to the process
// serving the deployment, which writes it and holds it (#543), or is
// refused with nothing written (#538), which leaves a hand-edited
// config.yaml as the remaining way in. This command is a read either way, is never
// refused beside a live engine, and announces no mode, since it is not
// one of the four surfaces #544 checks against a serving process. See the
// process-boundary note in backupset.go, and #539 for why the distinction
// is spelled out rather than left to be inferred. One command with a verb,
// like `catalog rebuild` and `quarantine <verb>`, because "preflight" will
// not be the only thing this product ever wants to do to a named medium,
// and the verb comes out of mediumVerbs above so the second one is
// discoverable and pinned rather than merely dispatchable.
//
// It needs a real transport, unlike most commands here, because reaching a
// bucket is the entire point.
func cmdMedium(args []string) int {
	fs, cfgPath := newFlagSet("medium")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 {
		// preflight|compact|... rather than a hardcoded "preflight", so
		// the arity refusal names the verbs that exist rather than the
		// one that existed when it was written.
		return usageError("medium: expected %s <medium-id>", strings.Join(mediumVerbNames(), "|"))
	}
	verb, id := operands[0], operands[1]
	run, ok := mediumVerbs[verb]
	if !ok {
		return usageError("medium: unknown subcommand %q (expected %s)", verb, quotedMediumVerbs())
	}

	// Both refusals above happen before anything is opened, which is what
	// TestMediumPreflightIsRefusedBeforeAnythingIsOpened checks by running
	// them against a config path that does not exist. Looking the verb up
	// before the open rather than after keeps that true for a mistyped
	// verb as well as a missing one.
	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	return run(ctx, svc, id)
}

// mediumPreflight is the `preflight` verb's half of the command above:
// everything from the opened service onward, once cmdMedium has decided
// which verb it is.
func mediumPreflight(ctx context.Context, svc *app.Service, id string) int {
	report, err := svc.PreflightMedium(ctx, id)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("storage medium %s: %s\n", report.Medium, verdictWord(report.OK))
	for _, c := range report.Checks {
		fmt.Printf("  %-14s %-8s %s\n", c.Step, outcomeWord(c), c.Detail)
	}
	if !report.OK {
		// A non-zero exit, so `medium preflight` composes into a script
		// the way `check` and `validate` already do: an operator wiring
		// this into a deployment step needs the shell to know.
		return 1
	}
	return 0
}

// verdictWord renders the whole report's answer as something an operator
// reads rather than as a boolean.
func verdictWord(ok bool) string {
	if ok {
		return "ready for a backup"
	}
	return "NOT ready; see the failing checks below"
}

// outcomeWord renders one check's outcome, folding in the transport
// category where there is one. The category is the machine-readable half
// and belongs beside the word rather than buried in the sentence: an
// operator scanning this column is deciding whose problem it is.
func outcomeWord(c mediumcheck.Check) string {
	if c.Category == "" {
		return string(c.Outcome)
	}
	return fmt.Sprintf("%s(%s)", c.Outcome, c.Category)
}

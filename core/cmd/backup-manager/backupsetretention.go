package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/service"
)

// cmdBackupSetRetention is `backup-manager backup-set retention
// <source/backup-set> [flags]` (issue #333): read which retention policy
// one backup set is retained under, give that set a whole policy of its
// own, or take that policy back off so it inherits the deployment's
// again.
//
// # Three operations, one verb, and no fourth state
//
// With no policy flag at all this SHOWS: which policy is in force, where
// it came from, and, when the set overrides, the deployment's policy
// beside it so an operator can see what clearing would return them to.
//
// --inherit CLEARS. It is its own flag rather than "pass no chain",
// because passing no chain is the show case, and a surface where the
// operator expresses "remove this set's policy" by omitting something is
// the same confusion config.BackupSet.RetentionConfig's pointer exists to
// prevent, moved up to a command line.
//
// Anything else SETS, and what it sets is the WHOLE policy. There is no
// merge with what the set declared before and none with the deployment's
// chain, because merging two chains produces a policy nobody wrote and
// nobody can predict from reading either half.
//
// # What it refuses to guess
//
// Two spellings of a chain reach this command: the three scalars as
// flags, and --policy-file for a tiers chain, which is the only structured
// way to name one without inventing a second grammar for something this
// project already has exactly one grammar for. Passing both is a usage
// error rather than a precedence rule, on the same reasoning
// config.Validate already refuses a policy that writes both spellings at
// once: an operator who wrote both is asking two different questions, and
// picking one silently is how a retention policy ends up deleting on terms
// nobody wrote.
//
// Passing two of the three scalars is NOT refused here, and that is
// deliberate. It is refused by config.Validate, which names the ones that
// are missing, and that refusal is the same one a hand-edited config.yaml
// gets. A copy of the rule here would be a second rule that could
// disagree with the first, which is worse than no second rule.
func cmdBackupSetRetention(args []string) int {
	fs, cfgPath := newFlagSet("backup-set retention")
	inherit := fs.Bool("inherit", false,
		"remove this backup set's own retention policy, so it is retained under the deployment's policy again")
	policyFile := fs.String("policy-file", "",
		`set this backup set's whole policy from a file holding the CONTENTS of a config.yaml "retention:" block (the key itself omitted); "-" reads standard input`)
	timezone := fs.String("timezone", "", "set: this policy's own IANA timezone; omitted inherits the deployment's")
	weekStartsOn := fs.String("week-starts-on", "", "set: this policy's own week start; omitted inherits the deployment's")
	dailyDays := fs.Int("daily-days", 0, "set: daily_days; all three of daily/weekly/monthly are needed to name a whole chain")
	weeklyMonths := fs.Int("weekly-months", 0, "set: weekly_months")
	monthlyMonths := fs.Int("monthly-months", 0, "set: monthly_months")
	protect := fs.Bool("protect-last-known-good", true,
		"set: this policy's own FR-19 protection; omitted inherits the deployment's, and an explicit =false is a materially more dangerous configuration")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	// The verb itself is operands[0], because cmdBackupSet hands every
	// handler the whole argument list: see backupSetVerbs' own doc for
	// why slicing it off would silently drop a flag written before it.
	if len(operands) != 2 || operands[0] != "retention" {
		return usageError(`backup-set retention: expected "retention <source/backup-set>" and exactly one backup set id`)
	}
	id := operands[1]
	if !isBackupSetID(id) {
		return usageError("backup-set retention: %q is not a backup set id; a backup set id is exactly source/name", id)
	}

	named := visitedRetentionPolicyFlags(fs)
	if *inherit && len(named) > 0 {
		return usageError(
			"backup-set retention: --inherit removes this set's own policy and --%s writes one; pass one or the other",
			strings.Join(named, ", --"))
	}
	if contains(named, "policy-file") && *policyFile == "" {
		return usageError(`backup-set retention: --policy-file needs a path, or "-" to read the policy from standard input`)
	}
	if *policyFile != "" && len(named) > 1 {
		return usageError(
			"backup-set retention: --policy-file carries the whole policy, so it cannot be combined with --%s; "+
				"put those values in the file instead",
			strings.Join(without(named, "policy-file"), ", --"))
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	switch {
	case *inherit:
		logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))
		got, err := svc.ClearBackupSetRetention(ctx, id)
		if err != nil {
			return fail(err)
		}
		printBackupSetRetention(got)
		return 0

	case len(named) > 0:
		override, code := buildRetentionOverride(fs, *policyFile, timezone, weekStartsOn, dailyDays, weeklyMonths, monthlyMonths, protect)
		if code != 0 {
			return code
		}
		logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))
		got, err := svc.SetBackupSetRetention(ctx, id, override)
		if err != nil {
			return fail(err)
		}
		printBackupSetRetention(got)
		return 0

	default:
		got, err := svc.BackupSetRetention(ctx, id)
		if err != nil {
			return fail(err)
		}
		printBackupSetRetention(got)
		return 0
	}
}

// retentionPolicyFlagNames lists every flag that WRITES a policy, shared
// by the "did the operator ask for a write at all" check and by the
// builder, so the two cannot drift on which flags are writes. --inherit
// is deliberately not here: it is the other write, and the two are
// mutually exclusive.
var retentionPolicyFlagNames = []string{
	"policy-file", "timezone", "week-starts-on",
	"daily-days", "weekly-months", "monthly-months", "protect-last-known-good",
}

// visitedRetentionPolicyFlags returns the policy-writing flags actually
// passed, in the order fs.Visit reports them.
//
// fs.Visit rather than each flag's own zero value, for the reason
// buildSettingsPatch reads its flags that way: an explicitly passed
// --daily-days=0 or --protect-last-known-good=false (whose zero value is
// already true) is a value an operator can type and has to count as
// named. A zero here is not a legal policy and config.Validate will say
// so, which is the point: it has to REACH config.Validate to be refused.
func visitedRetentionPolicyFlags(fs *flag.FlagSet) []string {
	writes := make(map[string]bool, len(retentionPolicyFlagNames))
	for _, name := range retentionPolicyFlagNames {
		writes[name] = true
	}
	var named []string
	fs.Visit(func(f *flag.Flag) {
		if writes[f.Name] {
			named = append(named, f.Name)
		}
	})
	return named
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func without(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}

// buildRetentionOverride turns the passed flags into the whole policy
// that will be written. It returns an exit code rather than an error for
// the one failure it owns, reading the policy file, which is a usage
// problem and not a service refusal.
func buildRetentionOverride(
	fs *flag.FlagSet, policyFile string,
	timezone, weekStartsOn *string, dailyDays, weeklyMonths, monthlyMonths *int, protect *bool,
) (service.RetentionOverride, int) {
	if policyFile != "" {
		data, err := readPolicyFile(policyFile)
		if err != nil {
			return service.RetentionOverride{}, fail(err)
		}
		// Parsed by core/service through config's own schema, strictly:
		// this command never learns what a retention block may contain,
		// so a key added there needs no change here.
		override, err := service.ParseRetentionOverride(data)
		if err != nil {
			return service.RetentionOverride{}, fail(err)
		}
		return override, 0
	}

	var o service.RetentionOverride
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "timezone":
			o.Timezone = *timezone
		case "week-starts-on":
			o.WeekStartsOn = *weekStartsOn
		case "daily-days":
			o.DailyDays = *dailyDays
		case "weekly-months":
			o.WeeklyMonths = *weeklyMonths
		case "monthly-months":
			o.MonthlyMonths = *monthlyMonths
		case "protect-last-known-good":
			v := *protect
			o.ProtectLastKnownGood = &v
		}
	})
	return o, 0
}

// readPolicyFile reads the policy from a path, or from standard input
// when the path is "-", so a policy can be piped in without ever touching
// the filesystem.
func readPolicyFile(path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("reading the policy from standard input: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the policy file: %w", err)
	}
	return data, nil
}

// printBackupSetRetention renders which policy is in force and where it
// came from.
//
// The attribution line is printed for BOTH cases, not only for an
// override. `retention --dry-run`'s own itemisation still names the
// policy only when a set overrides, with absence standing in for "the
// deployment's", and that asymmetry is a recorded limitation of a line
// pinned by a black-box contract suite rather than a design (see
// cmdRetention's own note). This command is new, nothing pins its output
// yet, and the question it exists to answer is exactly the one that
// asymmetry leaves half-answered, so it says the whole thing every time.
//
// The chain is rendered in printSettings' format on purpose: an operator
// moving between `settings` and this command reads one layout for one
// kind of thing.
func printBackupSetRetention(r service.BackupSetRetention) {
	fmt.Printf("backup set: %s\n", r.BackupSetID)
	if r.IsOverride {
		fmt.Println("  retained under: this backup set's own policy")
	} else {
		fmt.Println("  retained under: the deployment's policy (inherited)")
	}
	printRetentionPolicyBlock("  ", r.Effective)

	if r.IsOverride {
		// What clearing would return this set to. Printed only for an
		// overriding set, because for an inheriting one it is the same
		// policy twice, and a surface that shows one policy under two
		// headings teaches an operator that the two headings mean the
		// same thing.
		fmt.Println("  the deployment's policy, which --inherit would return this set to:")
		printRetentionPolicyBlock("    ", r.Deployment)
	}
}

func printRetentionPolicyBlock(indent string, p service.RetentionSettings) {
	fmt.Printf("%stimezone: %s\n", indent, p.Timezone)
	fmt.Printf("%sweek_starts_on: %s\n", indent, p.WeekStartsOn)
	fmt.Printf("%sprotect_last_known_good: %v\n", indent, p.ProtectLastKnownGood)
	fmt.Printf("%stiers:\n", indent)
	for _, t := range p.Tiers {
		fmt.Printf("%s  - name=%s granularity=%s keep=%d", indent, t.Name, t.Granularity, t.Keep)
		if t.PeriodDays != 0 {
			fmt.Printf(" period_days=%d", t.PeriodDays)
		}
		if t.WindowUnit != "" {
			fmt.Printf(" window_unit=%s", t.WindowUnit)
		}
		if t.Medium != "" {
			fmt.Printf(" medium=%s", t.Medium)
		}
		fmt.Println()
	}
}

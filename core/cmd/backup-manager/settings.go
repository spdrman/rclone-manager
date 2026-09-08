package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/service"
)

// cmdSettings is `backup-manager settings` (report the live retention and
// capacity settings FR-18/FR-19/FR-21 are currently deciding with) and
// `backup-manager settings patch [flags]` (change one of them in place).
// Issue #277's own investigation confirmed this is not fully covered by
// "edit config.yaml and validate", the answer that already covers
// creating a backup set: GET is a discovery surface a config file has no
// equivalent of, since it reports the RESOLVED policy (defaults included)
// rather than the file's own possibly-omitted keys, and the API's PATCH
// hot-reloads the process that served it without a restart.
//
// The patch goes one of three ways and prints which on a `mode:` line
// (mode.go, liveengine.go). With something serving this deployment and a
// route to it, the patch IS `PATCH /api/v1/settings` against that engine
// (#543), so the hot reload is the serving process's own and there is
// nothing to restart. With nothing serving, it is
// core/service.BackupService.UpdateSettings called in this process, the
// same method that route is built on: the file is written, this process
// reloads its own view of it and exits, and an engine started afterwards
// reads the new file when it starts. With something serving and no route,
// the patch is REFUSED, nothing is written, and the operator is told what
// was found and where the change can be made instead. That last one is a
// refusal rather than a write because there is no watcher over
// config.yaml, so a patch left in the file is one the serving process
// would never read.
//
// The sentence all of that replaces said a patch here was hot-reloaded
// "into a running process exactly as PATCH /api/v1/settings already
// does", which is the claim issue #535 cost a real install. #539
// corrected it, #538 and #542 turned the corrected sentence into
// behaviour, and #543 made the original claim true for the one case it
// was ever meant to describe, an engine that is really there and that
// this command has been told how to reach.
//
// The READ is not routed and announces no mode at all. `settings` on its
// own answers from this host's configuration file, which is a fact about
// the file rather than about what the engine loaded, and it is not one of
// the four surfaces #544 checks against a serving process.
//
// One more thing an operator meets before any of that: `settings patch`
// with no patch flag at all is a usage error (exit 2) rather than a
// refusal, because the command line is the thing that is wrong and
// complaining about a running engine over a forgotten --timezone sends
// somebody off to stop a daemon for nothing.
//
// # The whole chain, which this command used to refuse (issue #595)
//
// This doc used to say the opposite: that a full retention tier-chain
// replacement (core/service.RetentionUpdate.Tiers) "is still, and
// remains, a config-file edit, exactly like every other case the
// config-file answer already covers". --policy-file is the reversal, and
// it is spelled exactly the way `backup-set retention` already spells it,
// because an operator moves between the two and should not meet two
// grammars for one thing: the file holds the CONTENTS of a `retention:`
// block, and "-" reads standard input.
//
// Two things undid the original argument, and only the second is new.
//
// A chain stopped being purely a policy about time. Since EPIC E a tier
// names a `medium:` (config.RetentionTier.Medium), so a chain says where
// the bytes live, and EPIC G requires every capability to be reachable
// from `backup-manager` because a browser-only destination change is one
// nobody can automate across a fleet.
//
// And the config-file answer was never an equal-power route here. The
// three write modes above are this command's own: beside a serving
// process a file edit is REFUSED, precisely because nothing watches
// config.yaml and a change left in the file is one that process would
// never read (#543). So "edit the file" was advice that does not work in
// the one deployment where a fleet-wide destination change matters, and
// the deployment plan was effectively browser-only there. Every other
// retention and capacity field was already here; this was the hole.
//
// Nothing below it needed building. RetentionUpdate.Tiers, the FR-27
// disclosure gate and the empty-chain refusal all already existed and the
// browser already drove them over PATCH /settings, and engineRoute's own
// mapping already carried Tiers across "so that the one type doing the
// translating does not have a hole in it the day something else fills
// that field in". This is that day.
//
// The three legacy daily_days/weekly_months/monthly_months scalars are
// the one thing --policy-file will not take, and it refuses them rather
// than dropping them. RetentionUpdate has no field for them and neither
// does the wire type, so accepting the file and applying the rest would
// report success for a chain that never changed. They are sugar for the
// default chain, so the chain is what to write.
func cmdSettings(args []string) int {
	fs, cfgPath := newFlagSet("settings")
	timezone := fs.String("timezone", "", "patch only; ignored otherwise: retention.timezone (an IANA name)")
	weekStartsOn := fs.String("week-starts-on", "", "patch only; ignored otherwise: retention.week_starts_on (a weekday name)")
	protect := fs.Bool("protect-last-known-good", true,
		"patch only; ignored otherwise: retention.protect_last_known_good; pass =false to explicitly disable "+
			"FR-19 protection, which LastKnownGoodDecide treats as a materially more dangerous configuration")
	capBytes := fs.Int64("cap-bytes", 0, "patch only; ignored otherwise: capacity.cap_bytes (0 means no cap)")
	warningFreeBytes := fs.Int64("warning-free-bytes", 0,
		"patch only; ignored otherwise: capacity.warning_free_bytes (0 means no warning line)")
	criticalFreeBytes := fs.Int64("critical-free-bytes", 0,
		"patch only; ignored otherwise: capacity.critical_free_bytes (0 means no critical line)")
	safetyMarginBytes := fs.Int64("safety-margin-bytes", 0, "patch only; ignored otherwise: capacity.safety_margin_bytes")
	policyFile := fs.String("policy-file", "",
		`patch only; ignored otherwise: replace the deployment's whole retention chain from a file holding the CONTENTS of a config.yaml "retention:" block (the key itself omitted); "-" reads standard input`)
	acknowledge := fs.Bool("acknowledge-medium-disclosure", false,
		"patch only; ignored otherwise: acknowledge the storage-medium disclosure, which a chain needs the first time it sends one of its tiers to a non-local medium; without it that write is refused, and the refusal is the disclosure")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) > 1 || (len(operands) == 1 && operands[0] != "patch") {
		return usageError(`settings: expected no argument, or exactly one argument "patch"`)
	}
	patching := len(operands) == 1

	// A patch-only flag given without the "patch" operand must never be
	// silently accepted and dropped: that is indistinguishable, in its
	// output and its exit code, from a plain read, and an operator who
	// forgot the word "patch" would walk away believing they changed a
	// setting on a live daemon when nothing happened. This mirrors, in
	// the opposite direction, buildSettingsPatch's own refusal of a
	// `patch` operand that names no flag at all.
	if !patching {
		if named := visitedSettingsPatchFlags(fs); len(named) > 0 {
			return usageError(
				"settings: --%s only take(s) effect with the \"patch\" operand, and would otherwise be silently ignored; did you mean \"settings patch --%s ...\"?",
				strings.Join(named, ", --"), named[0])
		}
	}

	// A patch that names nothing is a usage mistake, and it has to be
	// caught here rather than left to UpdateSettings to refuse. Beside a
	// running engine everything downstream of this point answers with the
	// engine refusal, so an operator who forgot a flag was sent off to go
	// and stop a daemon over a missing --timezone. `backup-set patch`
	// has always refused its empty patch before it opens anything, and
	// this is the same rule: complain about the command line while the
	// command line is still the thing that is wrong.
	if patching && len(visitedSettingsPatchFlags(fs)) == 0 {
		return usageError("settings patch: name at least one setting to change (see --help); a patch that changes nothing would rewrite and reload the configuration to no effect")
	}

	// The three rules `backup-set retention` already applies to the
	// identically spelled flag, in the same order and for the same
	// reasons. Each one is about the command line rather than about the
	// deployment, so each is decided here, before anything is opened.
	if patching {
		named := visitedRetentionSectionFlags(fs)
		if contains(named, "policy-file") && *policyFile == "" {
			return usageError(`settings patch: --policy-file needs a path, or "-" to read the policy from standard input`)
		}
		if *policyFile != "" && len(named) > 1 {
			return usageError(
				"settings patch: --policy-file carries the whole retention policy, so it cannot be combined with --%s; "+
					"put those values in the file instead",
				strings.Join(without(named, "policy-file"), ", --"))
		}
		// The acknowledgment consents to a policy write. On a command
		// line that writes no policy it acknowledges nothing, and
		// accepting it silently would teach an operator the flag did
		// something.
		if *acknowledge && len(named) == 0 {
			return usageError("settings patch: --acknowledge-medium-disclosure acknowledges a retention policy write, and this command line writes no policy; pass it alongside --policy-file")
		}
	}

	ctx := context.Background()

	// The read and the write go through different doors, and which one is
	// decided from the operand before anything opens.
	//
	// `settings` on its own reads, and a read beside a live engine is
	// ordinary use of this binary that #538 was careful not to narrow. It
	// answers from this host's configuration file, which is a fact about
	// the file rather than about what the engine loaded, and it still
	// does: #544 routed `sources`, `status`, `artifacts` and the
	// retention preview and stopped there, so this read announces no
	// mode and is checked against no engine. Two surfaces can still
	// disagree about the settings in force, and the way that happens is a
	// hand-edited config.yaml rather than anything this binary writes.
	//
	// `settings patch` writes, and a write left in the file beside a
	// running engine is a change that process would never see, so it goes
	// through openConfigWriteRoute, which hands it to that process where
	// it can and refuses where it cannot (#543).
	if !patching {
		svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
		if err != nil {
			return fail(err)
		}
		defer cleanup()

		settings, err := svc.Settings(ctx)
		if err != nil {
			return fail(err)
		}
		printSettings(settings)
		return 0
	}

	// The policy is read BEFORE the route is opened, and the ordering is
	// the point rather than tidiness. It is cmdBackupSetRetention's own
	// argument, over the identical flag: --policy-file "-" finishes
	// whenever whatever is on the other end finishes, so reading it after
	// the engine check would put an operator-controlled pause between
	// that check and the write, and an engine that started during the
	// pause was written straight over (#535). Read first, then claim the
	// deployment, then write, with nothing that can block in between.
	var chain *service.RetentionUpdate
	if *policyFile != "" {
		var code int
		chain, code = readDeploymentChain(*policyFile)
		if code != exitOK {
			return code
		}
	}

	route, cleanup, err := openConfigWriteRoute(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	req := buildSettingsPatch(fs, timezone, weekStartsOn, protect, capBytes, warningFreeBytes, criticalFreeBytes, safetyMarginBytes)
	if chain != nil {
		req.Retention = chain
	}
	req.AcknowledgeMediumDisclosure = *acknowledge
	settings, err := route.UpdateSettings(ctx, req)
	if err != nil {
		return fail(err)
	}
	printSettings(settings)
	return 0
}

// settingsPatchFlagNames lists every flag cmdSettings declares that is
// meaningful only under the "patch" operand -- shared between
// visitedSettingsPatchFlags (which refuses them when "patch" is absent)
// and buildSettingsPatch (which reads them once it is present), so the
// two lists cannot drift apart on which flags are patch-only.
var settingsPatchFlagNames = []string{
	"timezone", "week-starts-on", "protect-last-known-good", "policy-file",
	"acknowledge-medium-disclosure",
	"cap-bytes", "warning-free-bytes", "critical-free-bytes", "safety-margin-bytes",
}

// settingsRetentionSectionFlagNames is the subset of the above that
// writes the RETENTION section, which is the section --policy-file
// carries whole.
//
// --acknowledge-medium-disclosure is deliberately not here: it is a
// consent that rides beside a policy rather than a field of one, exactly
// as RetentionOverride.AcknowledgeMediumDisclosure's own doc has it, and
// counting it would make it acknowledge itself.
var settingsRetentionSectionFlagNames = []string{
	"policy-file", "timezone", "week-starts-on", "protect-last-known-good",
}

// visitedRetentionSectionFlags returns the retention-section flags
// actually passed, in the order fs.Visit reports them, so the two
// mutual-exclusion refusals above can name the ones that clashed.
func visitedRetentionSectionFlags(fs *flag.FlagSet) []string {
	section := make(map[string]bool, len(settingsRetentionSectionFlagNames))
	for _, name := range settingsRetentionSectionFlagNames {
		section[name] = true
	}
	var named []string
	fs.Visit(func(f *flag.Flag) {
		if section[f.Name] {
			named = append(named, f.Name)
		}
	})
	return named
}

// readDeploymentChain turns --policy-file's contents into the retention
// section of a settings patch.
//
// It parses through core/service, which parses through config's own
// schema, strictly: this command never learns what a retention block may
// contain, so a key added there needs no change here. It resolves nothing
// and completes nothing, exactly like the flag it mirrors.
//
// The one thing it decides itself is the legacy spelling, and it decides
// it here rather than one layer down because this is where the surface
// that cannot carry it lives. RetentionUpdate has no
// daily_days/weekly_months/monthly_months field, so a policy file written
// that way would apply its other keys, report success and leave the chain
// exactly as the file already had it, which is silent data loss dressed
// as a 200.
//
// An exit code rather than an error, like buildRetentionOverride: reading
// a file named on the command line is a usage problem this package owns
// and prints itself, not a service refusal for fail() to render.
func readDeploymentChain(policyFile string) (*service.RetentionUpdate, int) {
	data, err := readPolicyFile(policyFile)
	if err != nil {
		return nil, fail(err)
	}
	o, err := service.ParseRetentionOverride(data)
	if err != nil {
		return nil, fail(err)
	}
	if o.DailyDays != 0 || o.WeeklyMonths != 0 || o.MonthlyMonths != 0 {
		return nil, fail(fmt.Errorf("settings patch: the policy names daily_days/weekly_months/monthly_months, and the deployment's chain is patched in the tiers spelling only; those three are sugar for the default chain, so write the chain itself as a tiers list (a tier is name, granularity and keep, plus medium to say where its copies go)"))
	}

	u := &service.RetentionUpdate{Tiers: o.Tiers}
	// Pointers for the reason engineRoute.UpdateSettings goes to the same
	// trouble: a field the file did not name has to stay "leave this
	// alone" rather than becoming "set it to the zero value", or a patch
	// that only replaces a chain would also blank the timezone.
	if o.Timezone != "" {
		u.Timezone = &o.Timezone
	}
	if o.WeekStartsOn != "" {
		u.WeekStartsOn = &o.WeekStartsOn
	}
	if o.ProtectLastKnownGood != nil {
		u.ProtectLastKnownGood = o.ProtectLastKnownGood
	}
	return u, exitOK
}

// visitedSettingsPatchFlags returns the names of every patch-only flag
// actually passed on the command line, in the order fs.Visit reports
// them. This is fs.Visit, not the flags' own zero values, for the same
// reason buildSettingsPatch reads them that way: an explicitly-passed
// --cap-bytes=0 or --protect-last-known-good=false (its zero value is
// already true) has to count as "named" too.
func visitedSettingsPatchFlags(fs *flag.FlagSet) []string {
	patchOnly := make(map[string]bool, len(settingsPatchFlagNames))
	for _, name := range settingsPatchFlagNames {
		patchOnly[name] = true
	}
	var named []string
	fs.Visit(func(f *flag.Flag) {
		if patchOnly[f.Name] {
			named = append(named, f.Name)
		}
	})
	return named
}

// buildSettingsPatch reads fs's parsed flag values into an
// UpdateSettingsRequest, using fs.Visit to tell "this flag was never
// passed" apart from "this flag was passed as its zero value" -- load
// bearing for every capacity field (zero is a real meaning: "remove this
// line") and for --protect-last-known-good (an explicit =false has to
// survive), the identical reason retention_flags.go's own
// resolveRetentionFlags reads --protect-last-known-good through fs.Visit
// rather than the flag's own zero value.
func buildSettingsPatch(fs *flag.FlagSet, timezone, weekStartsOn *string, protect *bool, capBytes, warningFreeBytes, criticalFreeBytes, safetyMarginBytes *int64) service.UpdateSettingsRequest {
	var retention service.RetentionUpdate
	var retentionNamed bool
	if *timezone != "" {
		retention.Timezone = timezone
		retentionNamed = true
	}
	if *weekStartsOn != "" {
		retention.WeekStartsOn = weekStartsOn
		retentionNamed = true
	}

	var capacity service.CapacityUpdate
	var capacityNamed bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "protect-last-known-good":
			v := *protect
			retention.ProtectLastKnownGood = &v
			retentionNamed = true
		case "cap-bytes":
			v := *capBytes
			capacity.CapBytes = &v
			capacityNamed = true
		case "warning-free-bytes":
			v := *warningFreeBytes
			capacity.WarningFreeBytes = &v
			capacityNamed = true
		case "critical-free-bytes":
			v := *criticalFreeBytes
			capacity.CriticalFreeBytes = &v
			capacityNamed = true
		case "safety-margin-bytes":
			v := *safetyMarginBytes
			capacity.SafetyMarginBytes = &v
			capacityNamed = true
		}
	})

	req := service.UpdateSettingsRequest{}
	if retentionNamed {
		req.Retention = &retention
	}
	if capacityNamed {
		req.Capacity = &capacity
	}
	return req
}

// printSettings renders the RESOLVED policy service.BackupService.Settings
// (or UpdateSettings) returned: defaults included, a legacy
// daily_days/weekly_months/monthly_months file already expanded into its
// three-tier chain. This is deliberately not a re-serialization of
// config.yaml, which is the whole reason this command exists alongside
// the config-file answer (see cmdSettings' own doc).
func printSettings(s service.Settings) {
	fmt.Println("retention:")
	fmt.Printf("  timezone: %s\n", s.Retention.Timezone)
	fmt.Printf("  week_starts_on: %s\n", s.Retention.WeekStartsOn)
	fmt.Printf("  protect_last_known_good: %v\n", s.Retention.ProtectLastKnownGood)
	fmt.Println("  tiers:")
	for _, t := range s.Retention.Tiers {
		fmt.Printf("    - name=%s granularity=%s keep=%d", t.Name, t.Granularity, t.Keep)
		if t.PeriodDays != 0 {
			fmt.Printf(" period_days=%d", t.PeriodDays)
		}
		if t.WindowUnit != "" {
			fmt.Printf(" window_unit=%s", t.WindowUnit)
		}
		// Where this tier's copies go, so an operator can read the
		// destination without opening YAML (#595). Appended only when the
		// tier names one, which is printBackupSetRetention's own rule for
		// the identical field and is what keeps a medium-free deployment
		// printing exactly the line it printed before this existed: every
		// case in core/tests/compat is one.
		if t.Medium != "" {
			fmt.Printf(" medium=%s", t.Medium)
		}
		fmt.Println()
	}
	fmt.Println("capacity:")
	fmt.Printf("  cap_bytes: %d\n", s.Capacity.CapBytes)
	fmt.Printf("  warning_free_bytes: %d\n", s.Capacity.WarningFreeBytes)
	fmt.Printf("  critical_free_bytes: %d\n", s.Capacity.CriticalFreeBytes)
	fmt.Printf("  safety_margin_bytes: %d\n", s.Capacity.SafetyMarginBytes)
	fmt.Printf("  backup_root: %s (configured=%v)\n", s.Capacity.BackupRoot, s.Capacity.BackupRootConfigured)
}

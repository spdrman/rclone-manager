package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/service"
)

// cmdBackupSet is `backup-manager backup-set patch <source/backup-set>
// [flags]` (issue #350): change one already-configured backup set in
// place, persisted and hot-reloaded exactly as PATCH
// /api/v1/backup-sets/{source}/{set} does, since both call the same
// core/service.BackupService.UpdateBackupSet.
//
// # Why the CLI gets this at all
//
// Until this existed, hand-editing config.yaml was the ONLY way to change
// a configured backup set, which is what an operator standing at a real
// NAS had to do. A verb here is what replaces that for anyone driving
// this over SSH or from a script, and it is also what keeps the two
// surfaces from diverging: suites/equivalence exists to catch a
// capability that lands on the Web UI and nowhere else, and shipping the
// inline editor without this would have added exactly that.
//
// # The shape, and why it mirrors `settings patch`
//
// A noun then a verb, the same as `catalog rebuild`, `quarantine
// revalidate` and `settings patch`, and every flag read through fs.Visit
// rather than through its own zero value. That second part is load
// bearing rather than tidy: `--port 0` selects the default port and
// `--user ""` is a value an operator can type and must be refused rather
// than ignored, so "this flag was never passed" and "this flag was passed
// as its zero value" have to stay distinguishable all the way down to
// service.UpdateBackupSetRequest's own pointers. A patch that named no
// flag at all is a usage error for the same reason buildSettingsPatch
// refuses one: it would rewrite and hot-reload a whole configuration to
// achieve nothing, and report success for it.
//
// What it deliberately cannot change: the set's identity, its SSH key
// reference and its trusted host-key line. See
// core/service/backupsetupdate.go's own package doc for each.
//
// # backup-set retention show|set|clear
//
// Issue #333's other half. Retention was global only: every backup set
// was retained under the one top-level policy, and the only way to give
// one set its own chain was, again, an editor on the NAS. These three
// verbs are the CLI answer, and they call the same three service methods
// the API routes do, so the two surfaces cannot answer differently.
//
// `set` writes a TIER CHAIN and never the legacy
// daily_days/weekly_months/monthly_months scalars, spelled with the same
// repeatable -tier flag `retention` already accepts, so one tier is
// written one way whichever command is holding it. See
// core/service/backupsetretention.go for why a chain rather than the
// scalars is the only spelling that cannot express half a policy.
func cmdBackupSet(args []string) int {
	fs, cfgPath := newFlagSet("backup-set")
	host := fs.String("host", "", "patch: remote.host")
	port := fs.Int("port", 0, "patch: remote.port (0 selects the default port, and is a real value here, not an unset one)")
	user := fs.String("user", "", "patch: remote.user")
	remotePath := fs.String("remote-path", "", "patch: remote_path (absolute)")
	localPath := fs.String("local-path", "", "patch: local_path (absolute)")
	include := fs.String("include", "", "patch: include patterns, comma separated; an empty value clears the list")
	completionStrategy := fs.String("completion-strategy", "", `patch: completion.strategy ("rename", "marker" or "stable")`)
	stableFor := fs.Duration("stable-for", 0, `patch: completion.stable_for; required when the strategy in effect is "stable"`)
	staleAfter := fs.Duration("stale-after", 0, "patch: stale_after (FR-24's freshness budget)")
	validatorID := fs.String("validator-id", "", `patch: validation.validator_id, an id the validator catalog lists, or "" for none`)
	acknowledgeRepoint := fs.Bool("acknowledge-repoint", false,
		"patch: confirm an edit that moves this set to different data. Needed only when --host, --remote-path or --local-path actually change on a set that already has artifacts on record; the refusal without it says what it costs")
	tiers := &retentionTierFlag{}
	fs.Var(tiers, "tier",
		"retention set: one tier of this set's own chain, as name:granularity:keep[:window_unit], with an optional @medium on the name. Repeat in chain order")
	timezone := fs.String("timezone", "", "retention set: the IANA timezone this set's chain is reckoned in; omit to inherit the deployment's")
	weekStartsOn := fs.String("week-starts-on", "", `retention set: "monday" or "sunday"; omit to inherit the deployment's`)
	protect := fs.Bool("protect-last-known-good", true, "retention set: FR-19's last-known-good protection for this set; omit to inherit the deployment's posture")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	verb, id, code := backupSetVerbAndID(operands)
	if code != 0 {
		return code
	}

	// Every flag this command owns belongs to exactly one verb, and a
	// flag passed to the wrong one is refused rather than ignored.
	// Silently parsing flags a command then does nothing with is a real
	// trap on a command that mutates a live configuration: the operator
	// reads exit 0 as "that landed" (issue #323 fixed the same shape on
	// `settings`).
	if wrong := flagsNotOwnedBy(fs, verb); wrong != "" {
		return usageError("backup-set %s: --%s is not a flag this verb accepts", verb, wrong)
	}

	ctx := context.Background()

	var (
		req      service.UpdateBackupSetRequest
		override service.BackupSetRetentionOverride
	)
	switch verb {
	case "patch":
		var named bool
		req, named = buildBackupSetPatch(fs, host, port, user, remotePath, localPath, include, completionStrategy, stableFor, staleAfter, validatorID)
		req.AcknowledgeRepoint = *acknowledgeRepoint
		if !named {
			return usageError("backup-set patch: name at least one field to change (see --help); a patch that changes nothing would rewrite and reload the configuration to no effect")
		}
	case "retention set":
		if len(tiers.tiers) == 0 {
			return usageError("backup-set retention set: name at least one --tier; to go back to the deployment's policy, use `backup-set retention clear` instead")
		}
		override = service.BackupSetRetentionOverride{
			Tiers:        serviceTiers(tiers.tiers),
			Timezone:     *timezone,
			WeekStartsOn: *weekStartsOn,
		}
		// Read through fs.Visit for the same reason every patch flag is:
		// an explicit --protect-last-known-good=false is a real request
		// and its own zero value, and inheriting the deployment's
		// posture is what NOT passing it means.
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "protect-last-known-good" {
				v := *protect
				override.ProtectLastKnownGood = &v
			}
		})
	}

	svc, cleanup, err := openBackupService(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	switch verb {
	case "patch":
		updated, err := svc.UpdateBackupSet(ctx, id, req)
		if err != nil {
			return fail(err)
		}
		printBackupSet(updated)
	case "retention show":
		got, err := svc.GetBackupSetRetention(ctx, id)
		if err != nil {
			return fail(err)
		}
		printBackupSetRetention(got)
	case "retention set":
		got, err := svc.SetBackupSetRetention(ctx, id, override)
		if err != nil {
			return fail(err)
		}
		printBackupSetRetention(got)
	case "retention clear":
		got, err := svc.ClearBackupSetRetention(ctx, id)
		if err != nil {
			return fail(err)
		}
		printBackupSetRetention(got)
	}
	return 0
}

// backupSetVerbAndID reads the operand list into the verb this invocation
// names and the backup set it names it about, or returns the exit code a
// malformed invocation gets. The verb is the whole phrase ("retention
// set", not "retention"), because every branch below acts on the pair and
// a half-read verb is how `retention` alone would end up doing something.
func backupSetVerbAndID(operands []string) (verb, id string, code int) {
	switch {
	case len(operands) == 2 && operands[0] == "patch":
		verb, id = "patch", operands[1]
	case len(operands) == 3 && operands[0] == "retention" &&
		(operands[1] == "show" || operands[1] == "set" || operands[1] == "clear"):
		verb, id = "retention "+operands[1], operands[2]
	default:
		return "", "", usageError(`backup-set: expected "patch <source/backup-set>" or "retention show|set|clear <source/backup-set>"`)
	}
	if strings.Count(id, "/") != 1 || strings.HasPrefix(id, "/") || strings.HasSuffix(id, "/") {
		return "", "", usageError("backup-set: %q is not a backup set id; a backup set id is exactly source/name", id)
	}
	return verb, id, 0
}

// backupSetVerbFlags names which flags each verb owns. --config is left
// out because every command accepts it, and a verb with no entry (a read,
// like `retention show`) owns no flags at all.
var backupSetVerbFlags = map[string]map[string]bool{
	"patch": {
		"host": true, "port": true, "user": true, "remote-path": true, "local-path": true,
		"include": true, "completion-strategy": true, "stable-for": true, "stale-after": true,
		"validator-id": true, "acknowledge-repoint": true,
	},
	"retention set": {
		"tier": true, "timezone": true, "week-starts-on": true, "protect-last-known-good": true,
	},
}

// flagsNotOwnedBy returns the name of the first flag this invocation
// passed that the named verb does not own, or "" when every flag belongs.
func flagsNotOwnedBy(fs *flag.FlagSet, verb string) string {
	owned := backupSetVerbFlags[verb]
	wrong := ""
	fs.Visit(func(f *flag.Flag) {
		if wrong != "" || f.Name == "config" || owned[f.Name] {
			return
		}
		wrong = f.Name
	})
	return wrong
}

// serviceTiers converts the -tier flag's parsed chain into the shape
// core/service takes. It is a straight field copy: the two types are the
// same schema either side of the core/ boundary (core/service/settings.go's
// RetentionTier doc), and nothing here interprets a value, so a bad tier
// is refused by config.Validate with the text a bad tier in the YAML file
// would get.
func serviceTiers(in []config.RetentionTier) []service.RetentionTier {
	out := make([]service.RetentionTier, 0, len(in))
	for _, t := range in {
		out = append(out, service.RetentionTier{
			Name:        t.Name,
			Granularity: t.Granularity,
			PeriodDays:  t.PeriodDays,
			Keep:        t.Keep,
			WindowUnit:  t.WindowUnit,
			Medium:      t.Medium,
		})
	}
	return out
}

// printBackupSetRetention reports what a set is retained under and where
// that comes from.
//
// "from" is named on both branches, not only on an override, and that is
// a deliberate difference from `retention`'s own preview line, which
// marks an override and stays silent otherwise. A preview is a long list
// where a marker on the exception is the readable choice; this command
// answers one question about one set, and an answer that said nothing
// about provenance in the common case would leave the reader unable to
// tell "inherits, and here is the chain" from "overrides with a chain
// that happens to match".
func printBackupSetRetention(r service.BackupSetRetention) {
	fmt.Printf("backup set: %s\n", r.BackupSetID)
	if r.IsOverride {
		fmt.Printf("  from: this set's own retention policy\n")
	} else {
		fmt.Printf("  from: the deployment's retention policy (inherited)\n")
	}
	fmt.Printf("  policy: %s\n", service.DescribeRetentionPolicy(r.Policy))
	if r.IsOverride {
		// What clearing would go back to, so an operator deciding
		// whether to clear does not have to run a second command to find
		// out what they would get.
		fmt.Printf("  deployment policy: %s\n", service.DescribeRetentionPolicy(r.DeploymentPolicy))
	}
}

// buildBackupSetPatch reads fs's parsed values into an
// UpdateBackupSetRequest through fs.Visit, so only the flags actually
// passed become non-nil pointers. It reports whether any of them were,
// which is what the caller refuses on.
//
// One switch over fs.Visit rather than a per-flag "is it non-zero" test,
// for the reason this file's own doc gives: an explicitly passed
// --port=0, --stable-for=0 or --user="" has to count as named.
func buildBackupSetPatch(
	fs *flag.FlagSet,
	host *string, port *int, user, remotePath, localPath, include, completionStrategy *string,
	stableFor, staleAfter *time.Duration, validatorID *string,
) (service.UpdateBackupSetRequest, bool) {
	var req service.UpdateBackupSetRequest
	named := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			v := *host
			req.Host = &v
		case "port":
			v := *port
			req.Port = &v
		case "user":
			v := *user
			req.User = &v
		case "remote-path":
			v := *remotePath
			req.RemotePath = &v
		case "local-path":
			v := *localPath
			req.LocalPath = &v
		case "include":
			req.Include = splitIncludePatterns(*include)
		case "completion-strategy":
			v := *completionStrategy
			req.CompletionStrategy = &v
		case "stable-for":
			v := *stableFor
			req.StableFor = &v
		case "stale-after":
			v := *staleAfter
			req.StaleAfter = &v
		case "validator-id":
			v := service.ValidatorID(*validatorID)
			req.ValidatorID = &v
		case "acknowledge-repoint":
			// Read by the caller straight off its own flag, because it is
			// not a field of the backup set: it answers a refusal about
			// the fields above. Naming only this one changes nothing, so
			// it must not make an otherwise-empty patch look like a
			// patch.
			return
		default:
			// --config, or anything else this command does not own. It is
			// not a field of the backup set, so it must not make an
			// otherwise-empty patch look like one.
			return
		}
		named = true
	})
	return req, named
}

// splitIncludePatterns turns the comma-separated --include value into the
// slice the request carries. An empty string is a request to CLEAR the
// list (a non-nil, empty slice), not an absent field: --include "" is a
// thing an operator can type and mean, and the caller has already decided
// the flag was passed at all through fs.Visit. Whitespace around each
// pattern is trimmed because a shell-quoted list is usually written with
// spaces after the commas, and a leading space in an include pattern is
// never what anyone meant.
func splitIncludePatterns(raw string) *[]string {
	patterns := []string{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	return &patterns
}

// printBackupSet renders the set as it now stands, after the edit, so an
// operator sees what was actually persisted rather than an echo of what
// they asked for. It reports every field this command can change plus the
// identity, and nothing that would leak this deployment's own filesystem
// layout beyond the paths the operator already configured (there is no
// key file path here, the same rule service.SSHKeyRef.KeyFile follows for
// the API).
func printBackupSet(s service.BackupSet) {
	fmt.Printf("backup set: %s\n", s.ID)
	fmt.Printf("  host: %s\n", s.Host)
	fmt.Printf("  port: %d\n", s.Port)
	fmt.Printf("  user: %s\n", s.User)
	fmt.Printf("  remote_path: %s\n", s.RemotePath)
	fmt.Printf("  local_path: %s\n", s.LocalPath)
	fmt.Printf("  include: %s\n", strings.Join(s.Include, ", "))
	fmt.Printf("  completion_strategy: %s\n", s.CompletionStrategy)
	// stable_for and stale_after are both patchable, so both are
	// reported: a command that can change a field and then prints a set
	// without it leaves an operator unable to confirm what it did.
	// stable_for is printed only under the strategy it belongs to,
	// because it is zero for every other one and a "0s" line invites the
	// reader to think it means something.
	if s.CompletionStrategy == "stable" {
		fmt.Printf("  stable_for: %s\n", s.StableFor)
	}
	fmt.Printf("  stale_after: %s\n", s.StaleAfter)
	fmt.Printf("  validator_id: %s\n", string(s.ValidatorID))
	fmt.Printf("  disabled: %v\n", s.Disabled)
	fmt.Printf("  read_only: %v\n", s.ReadOnly)
}

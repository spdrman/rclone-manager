package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
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

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 || operands[0] != "patch" {
		return usageError(`backup-set: expected "patch <source/backup-set>" and exactly one backup set id`)
	}
	id := operands[1]
	if strings.Count(id, "/") != 1 || strings.HasPrefix(id, "/") || strings.HasSuffix(id, "/") {
		return usageError("backup-set: %q is not a backup set id; a backup set id is exactly source/name", id)
	}

	req, named := buildBackupSetPatch(fs, host, port, user, remotePath, localPath, include, completionStrategy, stableFor, staleAfter, validatorID)
	if !named {
		return usageError("backup-set patch: name at least one field to change (see --help); a patch that changes nothing would rewrite and reload the configuration to no effect")
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	updated, err := svc.UpdateBackupSet(ctx, id, req)
	if err != nil {
		return fail(err)
	}
	printBackupSet(updated)
	return 0
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
	fmt.Printf("  validator_id: %s\n", string(s.ValidatorID))
	fmt.Printf("  disabled: %v\n", s.Disabled)
	fmt.Printf("  read_only: %v\n", s.ReadOnly)
}

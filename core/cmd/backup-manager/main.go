// Command backup-manager is the entry point for every execution mode this
// project supports (FR-1, FR-26). It is deliberately thin: every command
// below does nothing but parse its own flags, build (or reuse) an
// internal/app.Service, call exactly one of that package's exported use
// cases, and format the result for a terminal. No business rule lives
// here; see internal/app's package doc for why, and for what "business
// rule" means in this project.
package main

import (
	"fmt"
	"os"
)

// Set at build time with -ldflags (see container/Dockerfile).
var (
	version = "dev"
	commit  = "none"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is main's own logic pulled out into a function so it can return an
// exit code instead of calling os.Exit directly, which would skip every
// deferred cleanup (closing the state journal, flushing output) between
// here and wherever a subcommand's own os.Exit might otherwise have been
// tempted to live.
func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}

	name, rest := args[0], args[1:]
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "backup-manager: unknown command %q\n\n", name)
		usage()
		return 2
	}
	return cmd(rest)
}

var commands = map[string]func([]string) int{
	"run":        cmdRun,
	"daemon":     cmdDaemon,
	"check":      cmdCheck,
	"status":     cmdStatus,
	"sources":    cmdSources,
	"backup-set": cmdBackupSet,
	"artifacts":  cmdArtifacts,
	"fetch":      cmdFetch,
	"retention":  cmdRetention,
	"reconcile":  cmdReconcile,
	"validate":   cmdValidate,
	"catalog":    cmdCatalog,
	"quarantine": cmdQuarantine,
	"restore":    cmdRestore,
	"settings":   cmdSettings,
	"version":    cmdVersion,
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: backup-manager <command> [flags]

commands:
  run                                            perform one processing cycle and exit
  daemon                                         repeat the processing cycle at poll_interval
  check                                          validate config and the state database, then exit
  status                                         report process and backup-set health (FR-24)
  sources                                        list configured sources and backup sets
  backup-set create <source/backup-set> --host H --user U --remote-path P --local-path P
                    --ssh-key-file K|--ssh-key-id ID --known-hosts-line L|--trust-host-key
                    --completion-strategy rename|marker|stable [--include A,B] [--stable-for D]
                    [--stale-after D] [--validator-id V] [--disabled] [--read-only] [--run]
                                                  create a backup set, the same operation POST /api/v1/backup-sets
                                                  performs and through the same service layer. On an instance with
                                                  no config.yaml yet this writes the first one (#176), and
                                                  --state-database names the journal it points at
  backup-set patch <source/backup-set> [--host H] [--port N] [--user U] [--remote-path P] [--local-path P]
                    [--include "A,B"] [--completion-strategy S] [--stable-for D] [--stale-after D] [--validator-id ID]
                                                  change one configured backup set in place; only the flags you pass are
                                                  changed, and the change is persisted and hot-reloaded (#350)
  backup-set remove <source/backup-set>          take one backup set out of the configuration, the same operation
                                                  DELETE /api/v1/backup-sets/{source}/{set} performs. Configuration
                                                  only: the backups it collected stay on storage and stay listed by
                                                  artifacts, and creating the set again with the same source and
                                                  name takes them back (#391)
  artifacts [--source S] [--backup-set B]        list journal artifacts
  artifacts <source/backup-set/name>             print one artifact's full detail, including the reason
                                                  recorded for a FAILED/QUARANTINED/QUARANTINED_LOST one (#284)
  fetch --source S --backup-set B [--dry-run]    run one backup set's cycle on demand
  retention [--dry-run] [--timezone T] [--week-starts-on D] [--daily-days N] [--weekly-months N] [--monthly-months N] [--protect-last-known-good]
                                                  preview GFS/last-known-good retention decisions; each retention flag
                                                  overrides the loaded config's own resolved value for this preview only
  reconcile                                      run FR-17 reconciliation for every backup set
  validate <source/backup-set/artifact>          re-check one artifact's durable local copy
  validate <source/backup-set/artifact> [--content]
                                                  where that copy is on a storage medium instead, check it there: the
                                                  strongest class that costs nothing, by default, and with --content a
                                                  full download and re-hash, which costs egress, so FR-31 makes it
                                                  something an operator asks for rather than something that happens
                                                  (#435)
  catalog rebuild [--dry-run]                    reconstruct a lost/corrupted state database from sidecar recovery manifests
  quarantine <revalidate|retry|reinstate> <source/backup-set/artifact> [--note T]
                                                  act on one quarantined artifact: revalidate re-checks it and moves
                                                  nothing; retry re-enters the pipeline from DISCOVERED; reinstate
                                                  trusts it again in place and forfeits any future remote delete
  restore <source/backup-set/artifact> --medium M [--days N] --acknowledge
                                                  ask the storage provider to make one archived copy readable again
                                                  (EPIC E, FR-34). --acknowledge is required rather than a --force
                                                  to skip, because a restore is billed and takes hours; --days
                                                  defaults to 7 and is bounded to 1..30. artifacts <id> lists
                                                  which medium each copy is on
  settings [patch [--timezone T] [--week-starts-on D] [--protect-last-known-good=BOOL]
                   [--cap-bytes N] [--warning-free-bytes N] [--critical-free-bytes N] [--safety-margin-bytes N]]
                                                  report the live retention/capacity settings, or change one in place;
                                                  a full retention tier-chain replacement is still a config-file edit
  backup-set retention <source/backup-set> [--inherit] [--policy-file F]
                       [--timezone T] [--week-starts-on D]
                       [--daily-days N] [--weekly-months N] [--monthly-months N]
                       [--protect-last-known-good=BOOL]
                                                  report which retention policy this backup set is retained under and
                                                  where it came from; with a policy flag, give the set a whole policy of
                                                  its own; with --inherit, remove that policy so it is retained under the
                                                  deployment's again. An override replaces the deployment's whole chain
                                                  and is never merged with it, so it has to name a whole one
  version                                        report version information

every command except version accepts --config (default /etc/backup-manager/config/config.yaml;
a directory resolves to config.yaml inside it, which is what packaging mounts)
`)
}

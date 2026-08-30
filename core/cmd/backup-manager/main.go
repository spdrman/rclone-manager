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
	"run":       cmdRun,
	"daemon":    cmdDaemon,
	"check":     cmdCheck,
	"status":    cmdStatus,
	"sources":   cmdSources,
	"artifacts": cmdArtifacts,
	"fetch":     cmdFetch,
	"retention": cmdRetention,
	"reconcile": cmdReconcile,
	"validate":  cmdValidate,
	"version":   cmdVersion,
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: backup-manager <command> [flags]

commands:
  run                                            perform one processing cycle and exit
  daemon                                         repeat the processing cycle at poll_interval
  check                                          validate config and the state database, then exit
  status                                         report process and backup-set health (FR-24)
  sources                                        list configured sources and backup sets
  artifacts [--source S] [--backup-set B]        list journal artifacts
  fetch --source S --backup-set B [--dry-run]    run one backup set's cycle on demand
  retention [--dry-run] [--timezone T] [--week-starts-on D] [--daily-days N] [--weekly-months N] [--monthly-months N] [--protect-last-known-good]
                                                  preview GFS/last-known-good retention decisions; each retention flag
                                                  overrides the loaded config's own resolved value for this preview only
  reconcile                                      run FR-17 reconciliation for every backup set
  validate <source/backup-set/artifact>          re-check one artifact's durable local copy
  version                                        report version information

every command except version accepts --config (default /etc/backup-manager/config.yaml)
`)
}

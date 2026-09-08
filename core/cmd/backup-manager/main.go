// Command backup-manager is the entry point for every execution mode this
// project supports (FR-1, FR-26). It is deliberately thin: every command
// below does nothing but parse its own flags, reach exactly one use case,
// and format the result for a terminal. Most reach it by building (or
// reusing) an internal/app.Service; the configuration writes go through
// core/service.BackupService instead; and four of those writes and the
// four read surfaces put their question to a running engine over its HTTP
// API when one is serving this deployment and this host has been told
// where it is (#543, #544). No business rule lives on any of those paths;
// see internal/app's package doc for why, and for what "business rule"
// means in this project.
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
		return exitUsage
	}

	name, rest := args[0], args[1:]
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "backup-manager: unknown command %q\n\n", name)
		usage()
		return exitUsage
	}
	return cmd(rest)
}

// commands is every verb this binary dispatches.
//
// A map rather than a switch because it is read as data as well as walked:
// the discoverability tests ask it whether a verb exists at all, which is
// half of the check that a verb an operator cannot find has not shipped.
// The other half is usage below, and the two are separate lists on purpose.
// A verb here and not there is dispatchable and undiscoverable, which is how
// `backup-set remove` went out, so the gap between them is something a test
// can see rather than something only a reader would notice.
//
// There is a third list, and it is the one that had nothing watching it:
// core/tests/compat's corpus, which pins what each verb prints. A verb can
// be in both lists here and in none of that, which is how `unconfigured`
// and `medium preflight` shipped with FR-35 protecting not one word of
// their text. TestUsage_EveryRegisteredCommandIsPinned holds all three
// against each other now (#549).
var commands = map[string]func([]string) int{
	"run":          cmdRun,
	"daemon":       cmdDaemon,
	"check":        cmdCheck,
	"status":       cmdStatus,
	"sources":      cmdSources,
	"backup-set":   cmdBackupSet,
	"activity":     cmdActivity,
	"artifacts":    cmdArtifacts,
	"fetch":        cmdFetch,
	"retention":    cmdRetention,
	"reconcile":    cmdReconcile,
	"validate":     cmdValidate,
	"catalog":      cmdCatalog,
	"quarantine":   cmdQuarantine,
	"unconfigured": cmdUnconfigured,
	"medium":       cmdMedium,
	"retry":        cmdRetry,
	"restore":      cmdRestore,
	"settings":     cmdSettings,
	"version":      cmdVersion,
}

// usage is the text an operator reads when they type nothing, type
// something wrong, or ask for help, and it is the only place most of these
// verbs are described at all.
//
// Every line of it is pinned. core/tests/compat captures this block and
// compares it against a checked-in corpus under FR-35's fourth clause, so a
// line that is reworded, reflowed or re-indented here is a compatibility
// break somebody has to justify, not a tidy-up. The one thing that is
// allowed is growth: that cell is compared additive-only, because a new
// subcommand adds lines and changes nothing an operator was already reading.
//
// So a new verb is two edits, here and in commands above, and the usage half
// is the one that is easy to skip and impossible to notice missing from the
// inside. The flags are spelled out per verb rather than summarised because
// this is the only reference an operator on a terminal has. The trailing
// paragraphs carry the flag they all share, where a configuration change
// actually goes now that it can go two places, and how to tell this
// binary where the engine is, which is the only place those three
// environment variables are written down for somebody who is not reading
// route.go, and the exit codes, which #551 put here for the same reason:
// a status a wrapper script branches on is a contract, and a contract read
// off setup.go by whoever thought to look is not one.
func usage() {
	fmt.Fprint(os.Stderr, `usage: backup-manager <command> [flags]

commands:
  run                                            perform one processing cycle and exit
  daemon                                         repeat the processing cycle at poll_interval. One engine per
                                                  deployment: it is refused rather than started if another
                                                  process is already serving that state database (#536)
  check                                          validate config and the state database, then exit
  status                                         report process and backup-set health (FR-24)
  sources                                        list configured sources and backup sets
  backup-set create <source/backup-set> --host H --user U --remote-path P --local-path P
                    --ssh-key-file K|--ssh-key-id ID --known-hosts-line L|--trust-host-key
                    --completion-strategy rename|marker|stable [--include A,B] [--stable-for D]
                    [--stale-after D] [--validator-id V] [--disabled] [--read-only] [--run]
                                                  create a backup set. Beside a serving engine this command has a
                                                  route to, that is POST /api/v1/backup-sets against the engine
                                                  (#543); with nothing serving, it is the same service layer that
                                                  route is built on, called here. On an instance with no config.yaml
                                                  yet this writes the first one instead (#176), and --state-database
                                                  names the journal it points at; that create has no route, so it is
                                                  refused while something serves that deployment (#536)
  backup-set patch <source/backup-set> [--host H] [--port N] [--user U] [--remote-path P] [--local-path P]
                    [--include "A,B"] [--completion-strategy S] [--stable-for D] [--stale-after D] [--validator-id ID]
                    [--ssh-key-file K|--ssh-key-id ID] [--known-hosts-line L|--trust-host-key]
                    [--acknowledge-repoint] [--acknowledge-host-key-change]
                                                  change one configured backup set in place; only the flags you pass are
                                                  changed. Beside a serving engine this command has a route to, that is
                                                  PATCH /api/v1/backup-sets/{source}/{set} against the engine and takes
                                                  effect with no restart; with no route it is refused and the file is
                                                  left untouched (#350, #536, #543)
  backup-set remove <source/backup-set>          take one backup set out of the configuration: DELETE
                                                  /api/v1/backup-sets/{source}/{set} against a serving engine this
                                                  command has a route to, and the same service layer that route is
                                                  built on when nothing is serving. Configuration only: the backups it
                                                  collected stay on storage and stay listed by artifacts, and creating
                                                  the set again with the same source and name takes them back (#391)
  artifacts [--source S] [--backup-set B]        list journal artifacts
                                                  --backup-set takes the source/backup-set id sources, status and
                                                  retention name a backup set by, and a plain set name where one
                                                  source configures it. Not this list's own first column: that one
                                                  is the whole artifact id, a field longer, so drop the file name
                                                  off the end of it. A name two sources share names neither of
                                                  them, so it is refused with both ids rather than answered for one
                                                  of them (#569)
  artifacts <source/backup-set/name>             print one artifact's full detail, including the reason
                                                  recorded for a FAILED/QUARANTINED/QUARANTINED_LOST one (#284)
  activity [--backup-set S] [--severity warn|error] [--limit N] [--json]
                                                  list recorded lifecycle events, newest first: the same durable
                                                  transition log the Web UI's Activity page draws. Beside a serving
                                                  engine this reads GET /api/v1/activity from it; with nothing
                                                  serving it reads this host's own journal (#598). --limit counts
                                                  MATCHING events, so a filter that narrows still fills it. --json
                                                  emits the wire objects unchanged, so a script parses the contract
                                                  rather than this table
  fetch --source S --backup-set B [--dry-run]    run one backup set's cycle on demand
                                                  --backup-set takes the source/backup-set id here too, and names
                                                  the source itself when it carries one, so --source is only
                                                  needed for the plain set name (#569)
  retention [--dry-run] [--timezone T] [--week-starts-on D] [--daily-days N] [--weekly-months N] [--monthly-months N] [--protect-last-known-good]
                                                  preview GFS/last-known-good retention decisions. It deletes nothing in
                                                  either mode, so --dry-run is accepted and inert here; FR-20 deletion runs
                                                  through the API's retention preview/apply pair, against a reviewed plan_id.
                                                  Each retention flag overrides the loaded config's own resolved value for
                                                  this preview only
  retention <source/backup-set> [--dry-run] [the same retention override flags]
                                                  preview that one backup set's decisions instead of every configured
                                                  set's. An id that names no configured backup set is refused and
                                                  nothing is printed, rather than answered about a set nobody asked
                                                  about (#568)
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
  unconfigured                                   list the backup sets the journal remembers and the configuration no
                                                  longer names, what they still hold on storage, and the retention
                                                  policy governing them, which is none (#418)
  unconfigured clear <source/backup-set> [--acknowledge]
                                                  clear the .partial residue a removal stranded mid-transfer, and end
                                                  the journal rows nothing will ever advance. It never touches a
                                                  retained backup; without --acknowledge it only prints what it would do
  medium preflight <medium-id>                   prove one declared storage medium actually works before a cycle
                                                  carrying a real backup does: it writes a probe object with the
                                                  medium's own storage class, reads it back byte for byte, checks
                                                  the class it landed in against the one the configuration claims,
                                                  asks whether the medium's declared upload_verification can
                                                  actually be achieved there, and deletes the probe. Exits non-zero
                                                  when any check fails (#443)
  retry <source/backup-set/artifact> [--note T]   put one FAILED backup back into the pipeline so it is attempted
                                                  again. FAILED means an attempt did not finish, which is not the
                                                  same thing as quarantine, so this is its own command and not a
                                                  fourth quarantine verb. Nothing does this automatically: a blind
                                                  re-transfer for a cause nothing has classified is a cost this
                                                  manager does not take on its own (#419)
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

a configuration write goes one of three ways, and says which on a "mode:" line. With nothing
serving this deployment it is written here, and an engine started afterwards reads it when it
starts. With something serving and a route to it, backup-set create, patch and remove and
settings patch hand the change to that process over its API, so it takes effect at once and
there is nothing to restart. With something serving and no route, the write is refused and
nothing is written: there is still no config watcher and no SIGHUP reload in this build, so a
change left in the file is one the serving process would never read. Two writes have no route
at all and are refused beside a serving engine either way: a backup-set retention that sets
or clears a policy, and the first config.yaml a create writes on an instance that has none
yet (#535, #543)

the route is three environment variables. BACKUP_MANAGER_API_URL is the engine's address,
either its own listener from inside its container (http://127.0.0.1:8080) or the published
Web UI port from a shell on the host; BACKUP_MANAGER_API_USERNAME and
BACKUP_MANAGER_API_PASSWORD are the administrator credentials the Web UI takes, held in
memory for the one invocation and written nowhere. Not flags, because a password on a command
line is in every process listing on the host. A routed create or patch reports every field it
wrote, stale_after included: the API carries stale_after_seconds now, so both routes print the
same set (#555). Only an engine older than that field serves none, and against one of those a
routed create prints stale_after as not reported rather than inventing a value

status, sources, artifacts and retention read rather than write, so a missing route never
refuses them; they announce which world the answer is about instead. "mode: engine-attached"
is a serving process that holds the same configuration and was asked the same question,
"mode: direct" is nothing serving, and "mode: unconfirmed" is an answer taken from
config.yaml that could not be checked against the process serving this deployment and can
disagree with it. A read refuses in three cases, and then nothing at all is printed: that
process holds a DIFFERENT configuration, the engine at the address it was given turns out to
be serving a different deployment, or it answers this command's own question differently
(#544, #555)

everything else is ordinary beside a running engine and announces no mode at all: run, fetch,
check, validate and the rest, settings and a backup-set retention that only reports included.
The one command a running engine refuses is daemon, for the reason its entry above gives

exit codes, so a script can branch on what happened rather than on the sentence it happened
to print:

  0   the command did what it was asked
  1   an ordinary failure: a configuration that will not load, a state database that will
      not open, a cycle that backed nothing up, a set or an artifact that is not there, a
      status short of HEALTHY, a route that was named and did not answer, and a route that
      answered for a DIFFERENT deployment than the one this command was typed at
      a --backup-set naming a set name two sources share is an ordinary failure too, and
      not a 2: it takes the loaded configuration to know it is ambiguous at all, and the
      same command line is exactly right on a deployment where one source has that name
      (#569)
  2   nothing ran: the command line was wrong (an unknown command, an unknown flag, a
      missing or surplus argument), or it asked for help rather than for work. A -h on a
      subcommand is here too and it is not a mistake: 2 says no command was carried out,
      and the reason is on stderr either way
      a value that is not shaped like a backup set id is here too, everywhere one is taken:
      retention's operand, backup-set create/patch/remove/retention, unconfigured clear, and
      --backup-set on artifacts and fetch all refuse it before they open anything. That is
      where the line between this row and the one above it falls: source/name with a file
      name still on the end is wrong wherever it is typed, and a name this configuration
      does not have is only wrong here (#569)
  3   another process is serving this deployment, so nothing was done: a configuration write
      refused because it would never reach that process, or a daemon refused rather than
      started beside one. backup-manager-web serve answers the same way for the same
      reason, which matters because that is the binary this deployment's compose file
      runs, so a supervisor reads one code from either (#557). Read the sentence beside
      it before retrying in a loop. A
      supervisor still rolling the outgoing process gets this until it has let go, and
      waiting is the right answer; an engine this host was never given a route to goes on
      refusing for as long as it serves, and the answer there is to set the route or stop
      that process. Everything that only looks like it is 1, including a probe that could
      not be performed at all, a route that was named and did not answer, and a route that
      answered for a different deployment, because none of those gets better by waiting
      (#551, #555)
`)
}

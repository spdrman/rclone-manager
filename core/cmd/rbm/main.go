// Command rbm is the entry point for every execution mode this project
// supports (FR-1, FR-26). It is `rbm` to an operator and this directory is
// still cmd/rbm, because a Go package path is not something
// anybody types; the constant that decides what this binary prints itself
// as, and the whole of that argument, are in core/cliecho/cliname.go.
//
// It is deliberately thin: every command
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

	"github.com/spdrman/rclone-manager/core/cliecho"
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
		fmt.Fprintf(os.Stderr, cliecho.Binary+": unknown command %q\n\n", name)
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
	// The first line names the command and is therefore the one line of
	// this block that is not a literal. It is printed on its own rather
	// than spliced into the raw string below, so that the index of
	// commands stays one uninterrupted backticked chunk: readers of this
	// file include distribution/packaging's README-parity test, which
	// pulls the command list straight out of the source, and a raw string
	// broken in half by a concatenation is a list it silently reads as
	// empty.
	fmt.Fprintf(os.Stderr, "usage: %s <command> [flags]\n", cliecho.Binary)
	fmt.Fprint(os.Stderr, `
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
                    [--no-verify]
                                                  create a backup set. Beside a serving engine this command has a
                                                  route to, that is POST /api/v1/backup-sets against the engine
                                                  (#543); with nothing serving, it is the same service layer that
                                                  route is built on, called here. On an instance with no config.yaml
                                                  yet this writes the first one instead (#176), and --state-database
                                                  names the journal it points at; that create has no route, so it is
                                                  refused while something serves that deployment (#536)
                                                  The connection is proven before anything is written, and the create is
                                                  refused when it cannot be: --no-verify writes it anyway, says in so many
                                                  words that nothing was proven, and leaves the set marked as unverified
                                                  until a connection test passes (#624)
  backup-set patch <source/backup-set> [--host H] [--port N] [--user U] [--remote-path P] [--local-path P]
                    [--include "A,B"] [--completion-strategy S] [--stable-for D] [--stale-after D] [--validator-id ID]
                    [--ssh-key-file K|--ssh-key-id ID] [--known-hosts-line L|--trust-host-key]
                    [--acknowledge-repoint] [--acknowledge-host-key-change]
                    [--no-verify]
                                                  change one configured backup set in place; only the flags you pass are
                                                  changed. Beside a serving engine this command has a route to, that is
                                                  PATCH /api/v1/backup-sets/{source}/{set} against the engine and takes
                                                  effect with no restart; with no route it is refused and the file is
                                                  left untouched (#350, #536, #543)
                                                  An edit that changes the host, port, user, key, trusted host key or
                                                  remote path is proven before it is written and refused when it cannot
                                                  be; --no-verify writes it anyway and marks the set unverified (#624)
  backup-set remove <source/backup-set>          take one backup set out of the configuration: DELETE
                                                  /api/v1/backup-sets/{source}/{set} against a serving engine this
                                                  command has a route to, and the same service layer that route is
                                                  built on when nothing is serving. Configuration only: the backups it
                                                  collected stay on storage and stay listed by artifacts, and creating
                                                  the set again with the same source and name takes them back (#391)
  backup-set test-connection <source/backup-set>
                                                  prove one configured backup set's source: resolve the host, connect,
                                                  check its host key against what this set trusts, offer the configured
                                                  key, authenticate, and list the remote folder. Six named outcomes, and
                                                  a non-zero exit when any of them fails. Beside a serving engine this
                                                  command has a route to, the check is made BY that engine, so its steps
                                                  reach the live feed rather than only this terminal; with nothing
                                                  serving it is made here. A check that passes clears the unverified mark
                                                  a --no-verify create or patch left on the set. "preflight" is the same
                                                  verb under the name the storage-destination side spells it (#596, #624)
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
  activity --follow [--backup-set S | --scope deployment] [--severity warn|error] [--limit N] [--json]
                                                  stream the LIVE feed instead, until interrupted: the serving
                                                  process's own event stream, which is what the Web UI's docked
                                                  terminal shows. A different feed from the one above rather than a
                                                  mode of it, so it needs a route to that process and refuses
                                                  without one instead of quietly reading the journal. The feed
                                                  starts again, and says so, if that process restarts (#573, #598).
                                                  --scope deployment narrows it to the log that names no backup
                                                  set (a cycle starting, a capacity check, what somebody just
                                                  clicked), which is the whole feed on a deployment with nothing
                                                  configured yet (#593)
  fetch --source S --backup-set B [--dry-run]    run one backup set's cycle on demand
                                                  --backup-set takes the source/backup-set id here too, and names
                                                  the source itself when it carries one, so --source is only
                                                  needed for the plain set name (#569)
  retention [--dry-run] [--timezone T] [--week-starts-on D] [--daily-days N] [--weekly-months N] [--monthly-months N] [--protect-last-known-good]
            [--tier NAME:GRANULARITY:KEEP[:WINDOW_UNIT]] [--tier-medium NAME=MEDIUM_ID]
                                                  preview GFS/last-known-good retention decisions. It deletes nothing in
                                                  either mode, so --dry-run is accepted and inert here; FR-20 deletion runs
                                                  through the API's retention preview/apply pair, against a reviewed plan_id.
                                                  Each retention flag overrides the loaded config's own resolved value for
                                                  this preview only.
                                                  --tier is repeatable and replaces the whole chain; --tier-medium says
                                                  where one of those tiers' copies go, so a supplied chain is previewed
                                                  against the destinations it names rather than silently against the local
                                                  backup root. A supplied chain that names none, beside a deployment whose
                                                  own chain does, says so before the plan (#595)
  retention <source/backup-set> [--dry-run] [the same retention override flags]
                                                  preview that one backup set's decisions instead of every configured
                                                  set's. An id that names no configured backup set is refused and
                                                  nothing is printed, rather than answered about a set nobody asked
                                                  about (#568)
  retention apply <source/backup-set> --acknowledge
                                                  delete the local copy of every backup in that set no retention
                                                  tier keeps, against the plan it prints first. FR-19's last known
                                                  good is never among them, and a set whose inventory or
                                                  configuration moved between the two is refused with nothing
                                                  deleted. --acknowledge is required (#602)
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
  medium list [--json]                           list every storage destination this deployment declares, in
                                                  declaration order. No credential is reported, not even which of
                                                  the three sources a destination reads (#594)
  medium show <medium-id> [--json]               one destination, plus what the journal says is currently on it:
                                                  how many copies, which backup sets they belong to, and how many
                                                  of them are the only confirmed copy of their artifact anywhere
  medium import-credentials --stdin              read AWS shared-credentials text from standard input, write it
                                                  0600 beside this deployment's configuration, and print an id to
                                                  name it with. This is the only command here that ever holds a
                                                  secret and it takes it on stdin: there is deliberately no
                                                  --access-key-id and no --secret-access-key flag anywhere on this
                                                  surface, because a secret on a command line is in ps output for
                                                  every user on the box and in shell history
  medium add <medium-id> --bucket B [--type s3] [--region R] [--endpoint URL] [--prefix P]
             [--storage-class C] [--upload-verification readback|attested] [--no-verify]
             (--credentials-id ID | --credentials-file PATH | --credentials-env VAR | --credentials-command 'prog arg ...')
                                                  declare a storage destination without editing config.yaml. It
                                                  VERIFIES FIRST and writes nothing when verification fails, which
                                                  is what makes it scriptable across a fleet; --no-verify skips
                                                  that and says in its own output that nothing was proven. Every
                                                  credential flag names a REFERENCE and never material
                                                  a --no-verify destination is MARKED unverified in config.yaml and
                                                  stays marked until a test connection passes, so an operator who did
                                                  not type the command can still tell it apart from a proven one (#636)
  medium edit <medium-id> [the same flags]       replace a destination's description. A flag left off keeps what
                                                  the destination already says, and leaving the credential flags
                                                  off keeps the credential already configured, which it has to:
                                                  nothing here ever reports one back to be resubmitted
  medium remove <medium-id>                      un-declare a destination. Refused while any copy names it, and
                                                  the refusal lists the backup sets: removing the declaration would
                                                  not delete those copies, it would leave this deployment with no
                                                  bucket, no endpoint and no credential to reach them with (FR-30)
  medium preflight <medium-id>                   prove one declared storage medium actually works before a cycle
                                                  carrying a real backup does: it writes a probe object with the
                                                  medium's own storage class, reads it back byte for byte, checks
                                                  the class it landed in against the one the configuration claims,
                                                  asks whether the medium's declared upload_verification can
                                                  actually be achieved there, and deletes the probe. Exits non-zero
                                                  when any check fails (#443)
                                                  a check that PASSES clears a --no-verify destination's unverified
                                                  mark, which is a configuration write, so this verb goes where the
                                                  writes go: beside a serving engine it is carried out there, and is
                                                  refused when nothing says how to reach it. A failing check leaves
                                                  the mark alone (#636)
  medium preflight --candidate <medium-id> [the add flags]
                                                  the same eight checks against a destination that is NOT declared,
                                                  so a setup flow proves one before it is written down. It writes
                                                  nothing whatever the report says
  medium test-connection <medium-id>             the same check as medium preflight, under the name the rest of this
                                                  product uses for it. One check, two verbs, one implementation:
                                                  preflight is kept as an alias so anything scripted against it goes on
                                                  working, and this is the name the web UI's button and the command it
                                                  echoes now carry. It also answers for "local", the drive this
                                                  deployment's backups land on, reporting a missing path, a directory
                                                  the service user cannot write to, and a full filesystem as three
                                                  different failures rather than being unavailable because no network
                                                  is involved (#622)
  medium default <medium-id>                     make this the destination a NEWLY CREATED retention tier starts on.
                                                  It moves that and nothing else: every tier that already names a
                                                  destination goes on naming it and no backup is relocated. "local" is
                                                  a legal id here and is what a deployment that has chosen nothing
                                                  already has, so moving the default to a bucket is reversible. The
                                                  destination that is the default cannot be removed, and removing down
                                                  to one destination makes that one the default (#622)
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
                   [--policy-file F] [--acknowledge-medium-disclosure]
                   [--cap-bytes N] [--warning-free-bytes N] [--critical-free-bytes N] [--safety-margin-bytes N]]
                                                  report the live retention/capacity settings, or change one in place.
                                                  --policy-file replaces the deployment's whole retention chain from a
                                                  file holding the contents of a config.yaml "retention:" block, with "-"
                                                  reading standard input, spelled the way backup-set retention spells it.
                                                  A chain that sends a tier somewhere new needs
                                                  --acknowledge-medium-disclosure, and without it the refusal carries the
                                                  disclosure (#595)
  settings patch --tier-medium NAME=MEDIUM_ID [--acknowledge-medium-disclosure]
                                                  point one retention tier at a storage destination and leave the rest of
                                                  the chain exactly as it is. Repeatable, at most once per tier, and
                                                  refused when the chain has no tier of that name rather than inventing
                                                  one. MEDIUM_ID is a declared destination, or "local" for the drive
                                                  this deployment's backups already land on, which moves a tier back.
                                                  Sending a tier somewhere other than local for the first time needs
                                                  --acknowledge-medium-disclosure. This is the command the picker under a
                                                  tier in the web UI echoes (#622)
  backup-set edit-hold <source/backup-set> [--release]
                                                  report whether a backup set is held for editing, what taking the
                                                  hold stopped, and when the lease expires; --release gives it back
                                                  so the scheduler may run the set again rather than waiting for it
                                                  to lapse. A hold lives in the memory of the process serving this
                                                  deployment and does not survive it, so this needs a route to that
                                                  process and refuses without one. There is no verb that TAKES a
                                                  hold: a hold protects an editing session and a CLI edit is one
                                                  backup-set patch that either runs or does not (#350, #600)
  backup-set retention <source/backup-set> [--inherit] [--policy-file F]
                       [--timezone T] [--week-starts-on D]
                       [--daily-days N] [--weekly-months N] [--monthly-months N]
                       [--protect-last-known-good=BOOL] [--acknowledge-medium-disclosure]
                                                  report which retention policy this backup set is retained under and
                                                  where it came from; with a policy flag, give the set a whole policy of
                                                  its own; with --inherit, remove that policy so it is retained under the
                                                  deployment's again. An override replaces the deployment's whole chain
                                                  and is never merged with it, so it has to name a whole one.
                                                  --acknowledge-medium-disclosure is needed when the policy sends one of
                                                  this set's tiers somewhere new, and without it the refusal carries the
                                                  disclosure. The show form prints each tier's destination
  version                                        report version information

every command except version accepts --config (default /etc/rclone-manager/config/config.yaml;
a directory resolves to config.yaml inside it, which is what packaging mounts)

a configuration write goes one of three ways, and says which on a "mode:" line. With nothing
serving this deployment it is written here, and an engine started afterwards reads it when it
starts. With something serving and a route to it, backup-set create, patch and remove,
settings patch and medium import-credentials, add, edit and remove hand the change to that
process over its API, so it takes effect at once and there is nothing to restart. medium
preflight --candidate goes the same way, because it is the check medium add runs before it
writes and has to happen where the write will. With something serving and no route, the
write is refused and nothing is written: there is still no config watcher and no SIGHUP reload in this build, so a
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
      started beside one. `+cliecho.WebBinary+` serve answers the same way for the same
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

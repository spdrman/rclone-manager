package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/cliecho"
	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/service"
)

// backupSetVerbs is every verb that brings its OWN flag set, dispatched
// before this command parses anything (issue #333).
//
// create and patch share one flag set and one operand shape, so they keep
// the switch below. retention cannot: its flags are --daily-days,
// --policy-file and friends, which declareBackupSetFlags has never heard
// of, so parsing them against that set would fail before any dispatcher
// ran. Finding the verb first is what lets the three coexist, and adding
// a fourth one that owns its flags is a map entry rather than a fourth
// argument convention.
//
// The handler is given the WHOLE argument list, its own verb included,
// and finds that verb as its first operand. That is what lets a flag
// appear on either side of it, so `backup-set --config X retention a/b`
// runs against X exactly as `settings --config X patch` already does. A
// dispatcher that sliced the verb off would silently drop every flag
// written before it, which reads as a command that ran against the wrong
// configuration file rather than as an error.
var backupSetVerbs = map[string]func([]string) int{
	"retention": cmdBackupSetRetention,
	"edit-hold": cmdBackupSetEditHold,
}

// backupSetSharedFlagVerbs are the verbs that share declareBackupSetFlags
// and go through the switch in cmdBackupSet, in the order usage() lists
// them. The switch dispatches on these literals; this list exists so the
// usage messages and the test that checks usage() against the verbs read
// them from one place rather than three.
var backupSetSharedFlagVerbs = []string{"create", "patch", "remove", "test-connection"}

// backupSetVerbAliases are the spellings that reach a verb under another
// name (issue #624).
//
// There is one, and it is here rather than as a second entry in the list
// above because an alias is not a verb: usage() lists what a command DOES,
// and two entries for one action would be a menu that reads as two
// actions. The alias is documented in `test-connection`'s own usage entry,
// which is where somebody who typed the other word will look.
//
// Why it exists at all: the destination noun has spelled this check
// `medium preflight` since #443, and #622 is renaming that to
// `test-connection` while keeping `preflight` working. Both nouns
// answering to both spellings means an operator who learned either word is
// right on either noun, which is the whole of what "call one thing one
// name" is supposed to buy, and nothing anybody scripted stops working.
var backupSetVerbAliases = map[string]string{"preflight": "test-connection"}

// backupSetVerbNames is every verb `backup-set` dispatches, sorted: the
// shared-flag ones above and the ones with a flag set of their own.
func backupSetVerbNames() []string {
	names := append([]string{}, backupSetSharedFlagVerbs...)
	for verb := range backupSetVerbs {
		names = append(names, verb)
	}
	sort.Strings(names)
	return names
}

// cmdBackupSet is `backup-manager backup-set <verb> <source/backup-set>
// [flags]`, the CLI's own half of the backup-set write surface. Three
// verbs share this command's flag set, and they arrived from three
// issues:
//
//	create   issue #356
//	patch    issue #350
//	remove   issue #391
//
// (retention, issue #333, lives under the same noun with a flag set of
// its own; see backupSetVerbs.) One command rather than several
// top-level ones, because they are the same noun over the same operand
// and a reader who has learned one has learned the others. It also means
// one place splits an id, one place turns a comma-separated --include
// into a list, and one place prints a set back.
//
// # Why the CLI gets either of these at all
//
// Before create, a backup set could be made two ways and neither was
// reachable from a terminal: POST /api/v1/backup-sets, and the Web UI
// wizard that calls it. So the one claim a user cares about, that a fresh
// install can be pointed at a machine and pull a backup off it, could not
// be driven end to end without a browser, and suites/equivalence recorded
// creation as a UI-only gap rather than as parity. Issue #356's
// two-machine end-to-end test is what needs it first: it installs onto a
// throwaway machine with the real installer and then has to say what to
// back up, over ssh, with no browser anywhere.
//
// Before patch, hand-editing config.yaml was the ONLY way to change a
// configured set, which is what an operator standing at a real NAS had to
// do. Before remove, there was no way at all, on any surface: the Web
// UI's confirmation closed and called nothing. All three verbs exist so
// the surfaces cannot diverge, which is what suites/equivalence is there
// to catch.
//
// # They share the service layer, and that is the point
//
// Everything below is argument handling. What a backup set may be lives
// in core/service (validateCreateRequest, newBackupSetFor,
// validateUpdatedBackupSet, config.Validate) and is reached through the
// exact same methods apps/common/webhost's handlers call: ImportSSHKey,
// ProbeHostKey, and then BackupService.CreateBackupSet,
// FirstRun.CreateInitialConfig or BackupService.UpdateBackupSet. A second
// set of rules growing here is what backupsetcreate_test.go's refusal
// table exists to catch.
//
// # Sharing the service layer is not reaching the running one
//
// Worth saying next to the paragraph above, because that paragraph is
// exactly what makes the mistake easy. Sharing a service layer is a claim
// about code, not about liveness: the methods are the same methods, and
// calling one of them here still moves only this process's own view of
// the deployment.
//
// So a verb that is about to change config.yaml (create, patch, remove,
// and the retention forms that set or clear a policy) does not just call
// it. It takes core/service's ConfigWriteGuard, asks whether anything has
// announced itself as serving this deployment, and settles from inside
// that claim where the change is going to go. There are three answers and
// mode.go prints which one this invocation got.
//
// With nothing serving, this process is the deployment's only authority:
// it opens its own BackupService over the same config.yaml and state
// database, writes, reloads its OWN view of the file and exits, and an
// engine started afterwards reads that file when it starts. With
// something serving and a route to it, create, patch and remove hand the
// change to that engine over its own API (#543, route.go and
// engineroute.go), so the change is made BY the process that will serve
// it and there is nothing to restart. With something serving and no
// route, nothing is written, the exit is non-zero, and the operator is
// told what was found and where the change can be made instead. There is
// still no watcher and no SIGHUP reload, which is why a change left in
// the file for a restart to find is the one outcome that never happens.
//
// Two writes under this noun have no route, and both are decisions rather
// than gaps. `backup-set retention` could have had one: the client
// carries setBackupSetRetention, clearBackupSetRetention and
// getBackupSetRetention, and apicontract.RetentionOverride carries a
// whole tier chain, so the wire is not what stops it. What #543 would not
// do is route the write and leave the report beside it reading this
// host's own file, because one verb answering out of two worlds is the
// failure this EPIC exists to close, and #544 did not take that read half
// either. So the verb stands whole on the direct path, and moving it is
// one piece of work rather than two halves.
//
// The first configuration a create writes on an instance that has no
// config.yaml yet has no route for a different reason: the only request
// that would carry it is POST /system/first-run, which an engine accepts
// once and only while it is still unconfigured, and a process found
// serving the journal --state-database names is either past that moment or
// standing in the setup flow the operator can finish themselves. Both
// refuse beside a serving process, exactly as every configuration write did
// before #543.
//
// That is issue #535: a `backup-set create` through docker exec against a
// live server succeeded, `sources` listed both sets, and the Web UI showed
// nothing. The help text and the doc comments here had said these verbs
// were "the same operation POST /api/v1/backup-sets performs", which is
// true inside one process and reads as a promise about the running one, so
// #539 corrected every one of them. EPIC #536 is what changed the
// behaviour: phase 1 refuses a write aimed at a configuration a running
// engine holds (#538) and names the mode it decided (#542), and phase 2
// gave this tree an API client (#541) and the route that carries these
// three verbs to the engine rather than turning the operator away (#543).
//
// # The two create paths, and why one verb covers both
//
// A configured instance folds a new set into the file it already has
// (BackupService.CreateBackupSet). An instance with NO config.yaml has
// nothing to fold into, and writing the first one is a different
// operation with a different write primitive (FirstRun.CreateInitialConfig,
// an exclusive create rather than a replace-by-rename). The HTTP layer
// exposes those as two routes because a fresh install serves a different
// surface entirely (issue #176). An operator at a terminal has no such
// split to observe: they want to say what to back up. So this verb asks
// the filesystem which case it is in and calls the matching method,
// exactly as the provider app does at startup.
//
// # Why patch reads every flag through fs.Visit
//
// Load bearing rather than tidy: `--port 0` selects the default port and
// `--user ""` is a value an operator can type and must be refused rather
// than ignored, so "this flag was never passed" and "this flag was passed
// as its zero value" have to stay distinguishable all the way down to
// service.UpdateBackupSetRequest's own pointers. A patch that named no
// field at all is a usage error for the same reason buildSettingsPatch
// refuses one: it would rewrite and hot-reload a whole configuration to
// achieve nothing, and report success for it.
//
// What patch deliberately cannot change: the set's identity. Its SSH key
// and its trusted host-key line it can, since issue #572, and the second
// of those has a refusal in front of it. See
// core/service/backupsetupdate.go's own package doc for the identity
// argument and core/service/backupsethostkey.go for the refusal.
func cmdBackupSet(args []string) int {
	for _, a := range args {
		if verb, ok := backupSetVerbs[a]; ok {
			return verb(args)
		}
	}

	f := declareBackupSetFlags()

	operands, err := parseFlagsAroundOperands(f.fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 {
		return usageError(`backup-set: expected "create <source/backup-set>", "patch <source/backup-set>", "remove <source/backup-set>" or "retention <source/backup-set>", a verb and exactly one backup set id`)
	}
	sourceName, name, ok := splitBackupSetID(operands[1])
	if !ok {
		return usageError("backup-set: %q is not a backup set id; a backup set id is exactly source/name", operands[1])
	}

	verb := operands[0]
	if canonical, ok := backupSetVerbAliases[verb]; ok {
		verb = canonical
	}

	switch verb {
	case "create":
		if code := f.refuseFlagsOfTheOtherVerb("create", backupSetPatchOnlyFlags); code != 0 {
			return code
		}
		return backupSetCreate(f, sourceName, name)
	case "patch":
		if code := f.refuseFlagsOfTheOtherVerb("patch", backupSetCreateOnlyFlags); code != 0 {
			return code
		}
		return backupSetPatch(f, operands[1])
	case "remove":
		// remove names one set and nothing else, so it is said as an
		// allow-list of the one flag it takes rather than a deny-list of
		// the eighteen it does not. A deny-list is only complete while
		// somebody remembers to extend it, and a flag declared without
		// being listed would be one remove quietly accepted and ignored,
		// which is `backup-set remove a/b --read-only` exiting 0 having
		// removed the set: a command that did something other than what
		// it was told. An allow-list cannot rot that way.
		if code := f.refuseEveryFlagBut("remove", "config"); code != 0 {
			return code
		}
		return backupSetRemove(f, operands[1])
	case "test-connection":
		// The same allow-list `remove` uses, and for the same reason: this
		// verb names one set and nothing else, so a flag from another verb
		// passed here would be parsed, ignored, and exit 0 having checked
		// something other than what the operator thought they had asked
		// about. --no-verify is refused with the rest, and refusing it is
		// the point: "test the connection without testing the connection"
		// is not an instruction anybody can mean.
		if code := f.refuseEveryFlagBut("test-connection", "config"); code != 0 {
			return code
		}
		return backupSetTestConnection(f, operands[1])
	default:
		return usageError("backup-set: %q is not a backup-set verb; the verbs are %s", operands[0], strings.Join(backupSetVerbNames(), ", "))
	}
}

// backupSetFlags is every flag this command declares. One struct rather
// than a dozen parameters per verb, because create and patch share ten
// of them and passing the shared ones positionally to both is how they
// drift apart. remove takes none of them.
type backupSetFlags struct {
	fs      *flag.FlagSet
	cfgPath *string

	// Shared by create and patch. Each is a field of the backup set itself.
	host               *string
	port               *int
	user               *string
	remotePath         *string
	localPath          *string
	include            *string
	completionStrategy *string
	stableFor          *time.Duration
	staleAfter         *time.Duration
	validatorID        *string

	// Shared by create and patch since issue #572. Each settles one of
	// the two SSH-facing things a backup set has: which key it
	// authenticates with, and which host key it trusts. Within each pair
	// the two flags are alternatives, never a preference order.
	keyFile        *string
	keyID          *string
	knownHostsLine *string
	trustHostKey   *bool

	// create only.
	disabled      *bool
	readOnly      *bool
	runNow        *bool
	stateDatabase *string

	// Shared by create and patch, and not a field of the backup set: it
	// says what this invocation is allowed to skip (issue #624). Spelled
	// exactly as the destination side spells it, because it is the same
	// decision about the other half of the same product and two words for
	// it would be two things to learn.
	noVerify *bool

	// Shared by create and patch, and not a field of the backup set: it
	// answers one refusal, on either verb. Removing a set frees its id up
	// (issue #391), so creating one over an id that already has artifacts
	// on record is the same move an edit makes and asks the same question
	// (issue #411).
	acknowledgeRepoint *bool

	// patch only, and not a field of the backup set either: it answers
	// the OTHER refusal, that this edit means to trust a different host
	// key for the same host. There is nothing on record for a set that
	// does not exist yet, so it means nothing on create and is refused
	// there rather than parsed and dropped.
	acknowledgeHostKeyChange *bool
}

// create's and patch's own flags, by name, so each verb can refuse the
// other's rather than silently ignoring it. Silently ignoring is the
// worse failure by a distance: `backup-set patch ... --read-only` would
// exit 0 having changed nothing about the posture the operator just
// asked for. remove has no list of its own: it refuses everything but
// --config by construction (refuseEveryFlagBut).
var (
	// The four SSH-facing flags left this list in issue #572, which is
	// what made a key rotation and a host-key re-trust reachable from a
	// terminal at all. What stays is the set of things that only make
	// sense while a set is being brought into existence: its initial
	// posture, whether to run it straight away, and the journal path a
	// FIRST configuration names.
	backupSetCreateOnlyFlags = []string{"disabled", "read-only", "run", "state-database"}

	// Not empty since issue #572. The guard was kept through #411 with
	// nothing in it precisely so that the next patch-only flag would
	// refuse on create automatically rather than being silently ignored
	// there, and this is that flag.
	backupSetPatchOnlyFlags = []string{"acknowledge-host-key-change"}
)

func declareBackupSetFlags() *backupSetFlags {
	fs, cfgPath := newFlagSet("backup-set")
	f := &backupSetFlags{fs: fs, cfgPath: cfgPath}

	f.host = fs.String("host", "", "create, patch: remote.host, the machine being backed up")
	f.port = fs.Int("port", 0, "create, patch: remote.port; 0 leaves it unset, which is the default SSH port. On patch that 0 is a real value rather than an unset one")
	f.user = fs.String("user", "", "create, patch: remote.user")
	f.remotePath = fs.String("remote-path", "", "create, patch: remote_path (absolute), the directory on the source to pull from")
	f.localPath = fs.String("local-path", "", "create, patch: local_path (absolute), where artifacts land on this machine")
	f.include = fs.String("include", "", "create, patch: include patterns, comma separated; empty matches everything, and on patch an empty value clears the list")
	f.completionStrategy = fs.String("completion-strategy", "", `create, patch: completion.strategy ("rename", "marker" or "stable")`)
	f.stableFor = fs.Duration("stable-for", 0, `create, patch: completion.stable_for; required when the strategy in effect is "stable"`)
	f.staleAfter = fs.Duration("stale-after", 0, "create, patch: stale_after (FR-24's freshness budget); on create, unset takes the service's own default")
	f.validatorID = fs.String("validator-id", "", `create, patch: validation.validator_id, an id the validator catalog lists, or "" for none`)

	f.keyFile = fs.String("ssh-key-file", "", "create, patch: path to the SSH PRIVATE KEY to use for this set. Read once, validated, and copied into this deployment's own key store; the original is left alone. On patch this is a key rotation")
	f.keyID = fs.String("ssh-key-id", "", "create, patch: the id of a key this deployment has already imported, instead of importing another copy")
	f.knownHostsLine = fs.String("known-hosts-line", "", "create, patch: the exact known_hosts line to trust for this host, as `ssh-keyscan` prints it. On patch, one that pins a different key from the one on record needs --acknowledge-host-key-change")
	f.trustHostKey = fs.Bool("trust-host-key", false, "create, patch: probe the host now and trust whatever key answers. Trust on first use, and a real trust decision: use --known-hosts-line when the key is already known. On patch it needs --host to say what to probe")
	f.disabled = fs.Bool("disabled", false, "create: save the set disabled, so no cycle runs it until it is enabled")
	f.readOnly = fs.Bool("read-only", false, "create: this set's remote source must never be deleted from (issue #282)")
	f.runNow = fs.Bool("run", false, "create: submit a run cycle immediately after the set is persisted")
	f.stateDatabase = fs.String("state-database", service.StateDatabaseDefault(),
		"create: the SQLite journal path a FIRST configuration names, and the deployment this command asks about when there is no config.yaml to read one out of. Defaults to $STATE_DATABASE, or the packaged /data/state/state.db, which is the same default the web host serves under. Used only when there is no config.yaml yet; ignored, never applied, against an instance that already has one")

	f.acknowledgeRepoint = fs.Bool("acknowledge-repoint", false,
		"create, patch: confirm pointing this set at different data. On patch, needed only when --host, --remote-path or --local-path actually change on a set that already has artifacts on record; on create, only when this id already has artifacts on record and the set is being created somewhere other than where they came from. The refusal without it says what it costs")

	f.acknowledgeHostKeyChange = fs.Bool("acknowledge-host-key-change", false,
		"patch: confirm trusting a DIFFERENT host key for the same host. Needed only when the line being pinned is not the one on record. The refusal without it names both fingerprints, which is the whole of what there is to compare")

	f.noVerify = fs.Bool("no-verify", false,
		"create, patch: write the backup set without proving the connection first. The output says in so many words that nothing was proven, and the set is marked as unverified until a connection test passes. On patch it means something only for an edit that changes the host, port, user, key, trusted host key or remote path, because nothing else is a claim a connection test can settle. It exists for building configuration offline, against a host this machine cannot currently reach")

	return f
}

// refuseFlagsOfTheOtherVerb returns a usage exit code when the caller
// passed a flag that belongs to the verb they did not ask for. It reads
// what was actually PASSED (fs.Visit), never a zero value, so a create
// that legitimately leaves --acknowledge-repoint alone is untouched.
func (f *backupSetFlags) refuseFlagsOfTheOtherVerb(verb string, notMine []string) int {
	wrong := ""
	f.fs.Visit(func(fl *flag.Flag) {
		for _, name := range notMine {
			if fl.Name == name && wrong == "" {
				wrong = name
			}
		}
	})
	if wrong == "" {
		return 0
	}
	return usageError("backup-set %s: --%s is not a %s flag; passing it here would change nothing and exit 0", verb, wrong, verb)
}

// refuseEveryFlagBut is refuseFlagsOfTheOtherVerb turned inside out, for
// a verb that takes almost nothing: a usage exit code for any flag that
// was PASSED (fs.Visit) and is not one of mine. remove takes only
// --config, and saying that is one word that cannot go stale, where the
// list of everything it does not take went stale the moment anyone
// declared a flag without also listing it.
func (f *backupSetFlags) refuseEveryFlagBut(verb string, mine ...string) int {
	allowed := map[string]bool{}
	for _, name := range mine {
		allowed[name] = true
	}
	wrong := ""
	f.fs.Visit(func(fl *flag.Flag) {
		if !allowed[fl.Name] && wrong == "" {
			wrong = fl.Name
		}
	})
	if wrong == "" {
		return 0
	}
	return usageError("backup-set %s: --%s is not a %s flag; passing it here would change nothing and exit 0", verb, wrong, verb)
}

// backupSetCreate is the `create` verb. Where the set is actually made
// depends on what is serving this deployment and on whether this command
// has been told how to reach it: POST /api/v1/backup-sets against that
// engine when it can be reached, and the same service layer that route is
// built on, called in this process, when nothing is serving. See the note
// on the package's process boundary above cmdBackupSet for the third
// answer, and for the one create that has no route at all.
func backupSetCreate(f *backupSetFlags, sourceName, name string) int {
	// Both pairs below are alternatives, not a preference order. A caller
	// who passed both has not said which one they meant, and picking one
	// silently is how a set ends up trusting a key nobody looked at.
	if *f.keyFile != "" && *f.keyID != "" {
		return usageError("backup-set create: --ssh-key-file and --ssh-key-id are alternatives; pass one")
	}
	if *f.keyFile == "" && *f.keyID == "" {
		return usageError("backup-set create: name a key with --ssh-key-file (to import one) or --ssh-key-id (to reuse one already imported)")
	}
	if *f.knownHostsLine != "" && *f.trustHostKey {
		return usageError("backup-set create: --known-hosts-line and --trust-host-key are alternatives; pass one")
	}
	if *f.knownHostsLine == "" && !*f.trustHostKey {
		return usageError("backup-set create: settle the host key with --known-hosts-line (the line you already trust) or --trust-host-key (probe and trust whatever answers now)")
	}

	req := service.CreateBackupSetRequest{
		SourceName:         sourceName,
		Name:               name,
		Host:               *f.host,
		Port:               *f.port,
		User:               *f.user,
		SSHKeyID:           *f.keyID,
		KnownHostsLine:     *f.knownHostsLine,
		RemotePath:         *f.remotePath,
		LocalPath:          *f.localPath,
		Include:            splitIncludePatterns(*f.include),
		CompletionStrategy: *f.completionStrategy,
		StableFor:          *f.stableFor,
		StaleAfter:         *f.staleAfter,
		ValidatorID:        service.ValidatorID(*f.validatorID),
		Disabled:           *f.disabled,
		ReadOnly:           *f.readOnly,
		RunImmediately:     *f.runNow,
		Actor:              cliActor,
		AcknowledgeRepoint: *f.acknowledgeRepoint,
		// Issue #624. The service runs the check itself in front of the
		// write and marks the set when told not to, so this is the same
		// flag patch sends and it means the same thing there. The check
		// this command runs first (proveSourceConnection) is for the
		// operator's screen: it prints all six steps, where the service's
		// refusal names only the step that failed. An earlier shape sent
		// the MARK from here instead, as a claim about what this command
		// had done, which the service could not check and any other client
		// could omit (PR #628 review).
		SkipConnectionCheck: *f.noVerify,
	}

	ctx := context.Background()
	// Resolved before the stat, never as supplied: --config may name the
	// configuration DIRECTORY the packaging mounts (#196), and statting
	// the directory would find it present on a completely empty install,
	// so the one shape that most needs the first-run path is the one
	// shape that would never take it. This is OpenConfigAndJournal's own
	// reasoning, applied one layer out because this command has to make
	// the same decision before it opens anything.
	configFile := config.ResolvePath(*f.cfgPath)
	if _, statErr := os.Stat(configFile); errors.Is(statErr, os.ErrNotExist) {
		return createFirstConfig(ctx, configFile, *f.stateDatabase, *f.keyFile, *f.trustHostKey, *f.noVerify, req)
	}
	return createIntoExistingConfig(ctx, *f.cfgPath, *f.keyFile, *f.trustHostKey, *f.noVerify, req)
}

// backupSetPatch is the `patch` verb, and openConfigWriteRoute settles
// which of two shapes it takes before any of it runs.
//
// With nothing serving this deployment it is BackupService.UpdateBackupSet
// called in this process, the same method PATCH
// /api/v1/backup-sets/{source}/{set} is built on, and the reload it
// triggers is this process's own view of the file, which this process then
// exits with. With a serving engine this command can reach, it IS that
// PATCH, made against the process that will go on serving the result.
// With a serving engine it cannot reach, it is refused with the file
// untouched.
func backupSetPatch(f *backupSetFlags, id string) int {
	// The same "one of each pair, never both" rule create states, for the
	// same reason: a caller who passed both has not said which one they
	// meant, and picking one silently is how a set ends up trusting a key
	// nobody looked at.
	if *f.keyFile != "" && *f.keyID != "" {
		return usageError("backup-set patch: --ssh-key-file and --ssh-key-id are alternatives; pass one")
	}
	if *f.knownHostsLine != "" && *f.trustHostKey {
		return usageError("backup-set patch: --known-hosts-line and --trust-host-key are alternatives; pass one")
	}
	if *f.trustHostKey && *f.host == "" {
		return usageError("backup-set patch: --trust-host-key needs a --host to probe; pass this set's own host to re-trust it where it is")
	}

	req, named := buildBackupSetPatch(f)
	req.AcknowledgeRepoint = *f.acknowledgeRepoint
	req.AcknowledgeHostKeyChange = *f.acknowledgeHostKeyChange
	// Issue #624. The check itself is the service's rather than this
	// command's, unlike create's, and the reason is that a sparse edit
	// cannot be turned into a candidate here: the fields this patch does
	// not name come off the persisted set, and one of them is that set's
	// trusted known_hosts line, which the API deliberately never
	// publishes. So the only place holding everything the check needs is
	// the process about to do the write, and this flag is how an operator
	// tells it not to. See core/service's UpdateBackupSetRequest.
	req.SkipConnectionCheck = *f.noVerify
	// --ssh-key-file and --trust-host-key name a field to change just as
	// surely as --ssh-key-id and --known-hosts-line do; they simply have a
	// step to run first. Counted here rather than in buildBackupSetPatch,
	// which reads flags into the request and has nothing to resolve them
	// with.
	if !named && *f.keyFile == "" && !*f.trustHostKey {
		return usageError("backup-set patch: name at least one field to change (see --help); a patch that changes nothing would rewrite and reload the configuration to no effect")
	}

	ctx := context.Background()
	route, cleanup, err := openConfigWriteRoute(ctx, *f.cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	// Import and probe happen against the same route the edit does, so a
	// key imported for a routed patch lands in the key store of the
	// process that will go on serving the set rather than this one's.
	if *f.keyFile != "" {
		keyID, err := importKeyFile(ctx, route, *f.keyFile)
		if err != nil {
			return fail(err)
		}
		req.SSHKeyID = &keyID
	}
	if *f.trustHostKey {
		line, err := probeAndTrust(ctx, route, *f.host, *f.port)
		if err != nil {
			return fail(err)
		}
		req.KnownHostsLine = &line
	}

	// Said before the write is attempted rather than after it lands, so
	// an operator reads it whether the write succeeds or not, and reads it
	// in the same order the destination side prints it. The set printed
	// below carries the mark too, which is the durable half; this is the
	// half that says what this particular command chose not to do.
	//
	// Only when the edit names something a connection test could have had
	// an opinion about. A `--no-verify` beside a `--local-path` change
	// skips nothing, because nothing would have run, and announcing a
	// skip that did not happen teaches an operator to read the line as
	// noise. The flags are what this command can see, so this can
	// over-report by one case, an edit that names a connection field and
	// sets it to the value it already had; that edit really was written
	// without a check, so the sentence stays true.
	if *f.noVerify && namesAConnectionField(f.fs) {
		fmt.Println("not verified: --no-verify was given, so this edit is written without the connection being proven")
		fmt.Printf("  prove it afterwards with: "+cliecho.Binary+" backup-set test-connection %s\n", id)
	}

	updated, err := route.UpdateBackupSet(ctx, id, req)
	if err != nil {
		return fail(err)
	}
	printBackupSet(updated)
	printPatchedKeyAndTrust(req)
	return 0
}

// printPatchedKeyAndTrust says what the edit did to the two things the
// backup set on the wire does not carry.
//
// printBackupSet reports every field it can read back, and neither the key
// reference nor the trusted host key is one of them: both resolve to a
// server-side path, and this package never prints one (service.SSHKeyRef's
// own rule). So this reports them from what was SENT and accepted, which
// is the honest version of the same confirmation, and reports the host key
// as its fingerprint rather than as the line, because a fingerprint is
// what an operator compares.
func printPatchedKeyAndTrust(req service.UpdateBackupSetRequest) {
	if req.SSHKeyID != nil {
		fmt.Printf("  ssh_key_id: %s\n", *req.SSHKeyID)
	}
	if req.KnownHostsLine == nil {
		return
	}
	algorithm, fingerprint, err := service.DescribeKnownHostsLine(*req.KnownHostsLine)
	if err != nil {
		// Unreachable: the service refuses a line it cannot parse, so a
		// request that got this far carries one that parses. Said rather
		// than swallowed, because a silent nothing here would read as
		// "the host key was not changed".
		fmt.Printf("  trusted_host_key: changed (this build could not describe the line it pinned: %v)\n", err)
		return
	}
	fmt.Printf("  trusted_host_key: %s %s\n", algorithm, fingerprint)
}

// backupSetRemove is the `remove` verb: DELETE
// /api/v1/backup-sets/{source}/{set} against a serving engine this command
// can reach, and the same BackupService.RemoveBackupSet that route is
// built on, called in this process, when nothing is serving.
//
// It asks for no confirmation, and that is a decision rather than an
// omission. Nothing this removes is a backup: every artifact the set
// collected stays on local storage and stays in the journal, `artifacts`
// still lists them, and creating the set again with the same source and
// name takes all of it back. A prompt in front of an operation that
// deletes nothing is the kind of ceremony people learn to type through,
// which is exactly what makes it useless in front of one that does.
//
// It prints what stayed, because that is the half a caller cannot see for
// itself once the set is out of the configuration.
func backupSetRemove(f *backupSetFlags, id string) int {
	ctx := context.Background()
	route, cleanup, err := openConfigWriteRoute(ctx, *f.cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	return backupSetRemoveWith(ctx, route, id, os.Stdout)
}

// backupSetRemover is the two calls the remove verb makes, as an
// interface for the same reason backupSetCreatePrereqs is one: so the
// one failure the real service cannot be made to produce on demand (the
// journal read failing while the configuration write works) can be
// driven in a test.
type backupSetRemover interface {
	ListArtifacts(ctx context.Context, filter service.ArtifactFilter) ([]service.Artifact, error)
	RemoveBackupSet(ctx context.Context, id string) error
}

// backupSetRemoveWith is the remove verb once the service is open.
//
// The count of what stays is read BEFORE the removal, because afterwards
// this filter names a set the configuration no longer has and is refused
// (#187). It is a courtesy, not a precondition: a read failing here must
// not turn into "the removal failed", because the operator asked for a
// set to stop collecting and the configuration can be written, and
// leaving the set running for the sake of a number in a sentence would be
// the wrong trade. A count that could not be taken is said to be exactly
// that, never printed as 0, which is a specific and reassuring claim
// about a thing that was not looked at. This mirrors
// BackupService.artifactCountFor's own -1.
//
// Two things now reach that branch rather than one, which is why the
// sentence no longer names the journal. Directly it is a journal read that
// failed; over the engine route it is a count this build does not take at
// all (engineroute.go's ErrArtifactsNotRouted, whose own doc says why).
// Both are honestly "nobody counted", and an operator has one thing to do
// about either.
func backupSetRemoveWith(ctx context.Context, svc backupSetRemover, id string, out io.Writer) int {
	kept := -1
	if listed, err := svc.ListArtifacts(ctx, service.ArtifactFilter{BackupSetID: id}); err == nil {
		kept = len(listed)
	} else if errors.Is(err, service.ErrBackupSetNotFound) {
		// Not a read failure: the set is not configured, and the removal
		// below is the call whose refusal says so.
		return fail(err)
	}

	if err := svc.RemoveBackupSet(ctx, id); err != nil {
		return fail(err)
	}

	// The removal has already happened by here, so a write that fails
	// because the terminal went away must not turn a successful removal
	// into a non-zero exit. Same reasoning as setup.go's cycle summary.
	_, _ = fmt.Fprintf(out, "removed the configuration for %s\n", id)
	if kept < 0 {
		_, _ = fmt.Fprintf(out, "could not count the backups that stay on storage; they are still there, and `"+cliecho.Binary+" artifacts` lists them\n")
	} else {
		_, _ = fmt.Fprintf(out, "%d backup(s) stay on storage and stay listed by `"+cliecho.Binary+" artifacts`\n", kept)
	}
	_, _ = fmt.Fprintf(out, "creating %s again takes all of them back\n", id)
	return 0
}

// cliActor is what a set created from a terminal records as the actor on
// the run cycle --run submits. The CLI has no authenticated identity to
// carry (there is no login here; reaching this binary already means
// reaching the host it runs on), so it says what it is rather than
// inventing a username.
const cliActor = "cli"

// createIntoExistingConfig folds one new backup set into a configuration
// that already exists, through the same BackupService method POST
// /api/v1/backup-sets calls, in this process.
func createIntoExistingConfig(ctx context.Context, configPath, keyFile string, trustHostKey, noVerify bool, req service.CreateBackupSetRequest) int {
	route, cleanup, err := openConfigWriteRoute(ctx, configPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	if err := resolveKeyAndTrust(ctx, route, keyFile, trustHostKey, &req); err != nil {
		return fail(err)
	}

	// After the key and the trust anchor are settled and before anything
	// is written, because those two are what the check needs and the write
	// is what it is there to stop (issue #624).
	if code := proveSourceConnection(ctx, route, noVerify, connectionTestFor(req), cliecho.Binary+" backup-set test-connection "+req.SourceName+"/"+req.Name); code != 0 {
		return code
	}

	result, err := route.CreateBackupSet(ctx, req)
	if err != nil {
		return fail(err)
	}
	printBackupSet(result.Set)
	if result.Operation != nil {
		fmt.Printf("  run submitted: operation %s\n", result.Operation.ID)
	}
	return 0
}

// createFirstConfig writes this deployment's first configuration, through
// the same FirstRun method POST /api/v1/system/first-run calls.
//
// The installer deliberately leaves the configuration directory empty
// (issue #176: a fresh install serves a setup flow rather than refusing
// to start), so this is the path a machine the installer has just set up
// actually takes, and the two-machine end-to-end test walks it.
func createFirstConfig(ctx context.Context, configFile, stateDatabase, keyFile string, trustHostKey, noVerify bool, req service.CreateBackupSetRequest) int {
	if req.RunImmediately {
		// The service ignores RunImmediately here, because a first-run
		// instance has no BackupService to submit an operation to yet.
		// Saying so is better than accepting the flag and doing nothing
		// with it: an operator who asked for a run and got silence has
		// been told the backup started.
		return usageError("backup-set create: --run cannot be honoured while writing the first configuration, because there is no running service to submit a cycle to yet. Create the set, then `" + cliecho.Binary + " run`")
	}

	// This path writes a configuration without ever going through
	// openBackupService, so it has to ask openBackupService's question
	// itself, and it has to ask it about the journal rather than about
	// the configuration: there is no configuration here to read a journal
	// path out of, which is the entire reason this branch was taken.
	//
	// Three things land here against a LIVE deployment, and all three used
	// to exit 0 after writing a configuration nothing would ever read: a
	// mistyped --config, a config.yaml renamed out from under a running
	// engine, and a genuinely fresh install whose engine is serving the
	// first-run setup flow, which is #571. --state-database is what
	// identifies the deployment in all three, because it carries the same
	// packaged default apps/generic's own --state-database does, so a
	// create that does not name one is still asking about the right
	// journal, and a first-run engine is now announcing about exactly that
	// journal (core/service's AnnounceServingFirstRun).
	//
	// A genuine first run is untouched by this, and that is the half that
	// had to stay true: a bare host has no serving lock file beside a
	// journal that does not exist, so the question comes back "nothing is
	// serving" and the first configuration is written from here, which is
	// the whole reason this path exists.
	//
	// It announces its mode too (#542), for the same reason it has to
	// ask at all: this was the one configuration write in the binary with
	// no route through openBackupService, so leaving it out would leave
	// exactly one write that never says which world it believed it was
	// in.
	guard, err := enterFirstConfigWriteMode(configFile, stateDatabase, os.Stdout, os.Stderr)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = guard.Release() }()

	firstRun, err := service.NewFirstRun(service.FirstRunDefaults{
		ConfigPath:    configFile,
		StateDatabase: stateDatabase,
	})
	if err != nil {
		return fail(err)
	}

	if err := resolveKeyAndTrust(ctx, firstRun, keyFile, trustHostKey, &req); err != nil {
		return fail(err)
	}

	// The same check the configured path runs, through FirstRun's own
	// TestConnection, which exists for the wizard that walks this same
	// flow in a browser. There is no engine to route to here by
	// construction (this branch was taken because there is no config.yaml
	// at all), so this is the durable path in the only world it has.
	if code := proveSourceConnection(ctx, firstRun, noVerify, connectionTestFor(req), cliecho.Binary+" backup-set test-connection "+req.SourceName+"/"+req.Name); code != 0 {
		return code
	}

	created, err := firstRun.CreateInitialConfig(ctx, req)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("wrote the first configuration to %s\n", configFile)
	printBackupSet(created)
	return 0
}

// backupSetCreatePrereqs is the pair of steps both create paths need and
// both surfaces already expose: importing the private key, and settling
// the host key. Naming them as an interface rather than branching twice is
// what keeps the CLI from having a first-run-shaped copy of either.
//
// Since issue #572 `patch` runs the same two, so the name is now narrower
// than the thing: it is renamed nowhere because backupSetRoute embeds it
// and the seam it exists for (making the awkward failures drivable in a
// test) is unchanged. What patch does with them differs only in what it
// puts the answers into.
type backupSetCreatePrereqs interface {
	ImportSSHKey(ctx context.Context, raw []byte, passphrase string) (service.SSHKeyRef, error)
	ProbeHostKey(ctx context.Context, host string, port int) (service.HostKeyProbe, error)
}

// resolveKeyAndTrust turns --ssh-key-file into an imported key id and
// --trust-host-key into a real known_hosts line, filling both into req.
// Either may already be settled by --ssh-key-id / --known-hosts-line, in
// which case the corresponding step does nothing. This is create's half;
// backupSetPatch runs the same two steps through the same functions.
//
// The key file is read by importKeyFile below and handed straight to
// ImportSSHKey, which is the only thing that reads key material anywhere
// in this binary. It is never logged, never echoed and never written
// anywhere but the key store ImportSSHKey owns.
func resolveKeyAndTrust(ctx context.Context, svc backupSetCreatePrereqs, keyFile string, trustHostKey bool, req *service.CreateBackupSetRequest) error {
	if keyFile != "" {
		id, err := importKeyFile(ctx, svc, keyFile)
		if err != nil {
			return err
		}
		req.SSHKeyID = id
	}

	if trustHostKey {
		if req.Host == "" {
			return errors.New("--trust-host-key needs a --host to probe")
		}
		line, err := probeAndTrust(ctx, svc, req.Host, req.Port)
		if err != nil {
			return err
		}
		req.KnownHostsLine = line
	}
	return nil
}

// importKeyFile reads a private key off disk, hands it to the import step,
// and reports the id the set will carry. Split out of resolveKeyAndTrust
// so `patch` performs the identical step rather than a rotation-shaped
// copy of it (issue #572).
func importKeyFile(ctx context.Context, svc backupSetCreatePrereqs, keyFile string) (string, error) {
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return "", fmt.Errorf("reading the SSH key at %s: %w", keyFile, err)
	}
	ref, err := svc.ImportSSHKey(ctx, raw, "")
	if err != nil {
		return "", err
	}
	// The fingerprint, never the path: what an operator needs to
	// confirm is which key was adopted, and where this deployment
	// keeps its copy is not theirs to have to know (SSHKeyRef's own
	// doc makes the same distinction for the HTTP layer).
	fmt.Printf("imported %s key %s\n", ref.Algorithm, ref.Fingerprint)
	return ref.ID, nil
}

// probeAndTrust asks host:port what key it is offering and reports the
// known_hosts line for it.
//
// The fingerprint is printed, always, and before anything is written. On
// create that is trust on first use, which is only defensible if the thing
// being trusted is stated where somebody can compare it afterwards. On
// patch there is already a key on record, so this line is a candidate
// rather than a decision: the service refuses to pin it over a different
// one until the operator has acknowledged the change, and this print is
// what they read while deciding.
func probeAndTrust(ctx context.Context, svc backupSetCreatePrereqs, host string, port int) (string, error) {
	probe, err := svc.ProbeHostKey(ctx, host, probePortFor(port))
	if err != nil {
		return "", fmt.Errorf("probing %s for its host key: %w", host, err)
	}
	fmt.Printf("trusting %s host key %s on first use\n", probe.Algorithm, probe.Fingerprint)
	return probe.KnownHostsLine, nil
}

// probePortFor resolves the port a host-key probe should dial.
//
// A backup set stores port 0 to mean "whatever the default SSH port is",
// which is what config.Remote.Port has always meant and what an operator
// who does not pass --port is saying. A probe cannot dial that: it opens a
// real TCP connection, so it needs a number, and
// internal/transport/rclone.ProbeHostKey refuses 0 as out of range rather
// than guessing. Resolving it HERE, for the probe only, keeps both true:
// the connection is made to 22 and the persisted set still says 0, so a
// future release that changed the default would carry this set with it
// rather than freezing today's answer into the configuration.
//
// The trust anchor comes out the same either way: the probe formats its
// known_hosts line through knownhosts.Line, which normalises away an
// explicit :22, so the line this produces is exactly the line a set with
// no port configured verifies against.
func probePortFor(port int) int {
	if port == 0 {
		return defaultSSHPort
	}
	return port
}

// defaultSSHPort is what a backup set with no configured port connects to.
const defaultSSHPort = 22

// isBackupSetID reports whether id has a backup set id's shape.
//
// A backup set id is exactly source/name (core/internal/model's own rule),
// so a value with no separator, two of them, or an empty half is refused
// by the caller with a message that says what the shape is, rather than
// reaching the service and coming back as a not-found for something that
// was never an id at all.
//
// splitBackupSetID below answers the same question and returns the halves
// with it; this is for the one caller that needs only the answer.
func isBackupSetID(id string) bool {
	_, _, ok := splitBackupSetID(id)
	return ok
}

// splitBackupSetID splits "source/name" into its two halves, reporting
// false for anything that is not exactly that. An id with no slash, two
// slashes, or an empty half names nothing, and guessing which half was
// meant is worse than saying so.
func splitBackupSetID(id string) (source, name string, ok bool) {
	source, name, found := strings.Cut(id, "/")
	if !found || source == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return source, name, true
}

// buildBackupSetPatch reads the parsed flag values into an
// UpdateBackupSetRequest through fs.Visit, so only the flags actually
// passed become non-nil pointers. It reports whether any of them were,
// which is what the caller refuses on.
//
// One switch over fs.Visit rather than a per-flag "is it non-zero" test,
// for the reason this file's own doc gives: an explicitly passed
// --port=0, --stable-for=0 or --user="" has to count as named.
func buildBackupSetPatch(f *backupSetFlags) (service.UpdateBackupSetRequest, bool) {
	var req service.UpdateBackupSetRequest
	named := false
	f.fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "host":
			v := *f.host
			req.Host = &v
		case "port":
			v := *f.port
			req.Port = &v
		case "user":
			v := *f.user
			req.User = &v
		case "remote-path":
			v := *f.remotePath
			req.RemotePath = &v
		case "local-path":
			v := *f.localPath
			req.LocalPath = &v
		case "include":
			patterns := splitIncludePatterns(*f.include)
			req.Include = &patterns
		case "completion-strategy":
			v := *f.completionStrategy
			req.CompletionStrategy = &v
		case "stable-for":
			v := *f.stableFor
			req.StableFor = &v
		case "stale-after":
			v := *f.staleAfter
			req.StaleAfter = &v
		case "validator-id":
			v := service.ValidatorID(*f.validatorID)
			req.ValidatorID = &v
		case "ssh-key-id":
			v := *f.keyID
			req.SSHKeyID = &v
		case "known-hosts-line":
			v := *f.knownHostsLine
			req.KnownHostsLine = &v
		case "acknowledge-repoint", "acknowledge-host-key-change":
			// Read by the caller straight off their own flags, because
			// neither is a field of the backup set: each answers a
			// refusal about the fields above. Naming only these changes
			// nothing, so they must not make an otherwise-empty patch
			// look like a patch.
			return
		default:
			// --config, or one of create's own flags, which this verb has
			// already refused by name. Neither is a field of the backup
			// set, so neither may make an otherwise-empty patch look like
			// one.
			return
		}
		named = true
	})
	return req, named
}

// splitIncludePatterns turns the comma-separated --include value into the
// slice the request carries. Whitespace around each pattern is trimmed
// because a shell-quoted list is usually written with spaces after the
// commas, and a leading space in an include pattern is never what anyone
// meant.
//
// An empty value is no patterns at all, which core reads as "no filter"
// rather than as "match nothing" (internal/discovery's includeMatches).
// On patch that is a request to CLEAR the list rather than an absent
// field: `--include ""` is a thing an operator can type and mean, and the
// caller has already decided through fs.Visit that the flag was passed at
// all. That is why this returns an empty slice and never a nil one, so
// the pointer the patch takes to it is never a pointer to nil.
func splitIncludePatterns(raw string) []string {
	patterns := []string{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

// printBackupSet renders the set as it was actually persisted, rather
// than echoing the request back. It reports the identity and every field
// these verbs can set or change, and nothing that would leak this
// deployment's own filesystem layout beyond the paths the operator
// already typed: no key file path and no known_hosts path, the same rule
// service.SSHKeyRef follows for the API.
func printBackupSet(s service.BackupSet) {
	fmt.Printf("backup set: %s\n", s.ID)
	fmt.Printf("  host: %s\n", s.Host)
	fmt.Printf("  port: %d\n", s.Port)
	fmt.Printf("  user: %s\n", s.User)
	fmt.Printf("  remote_path: %s\n", s.RemotePath)
	fmt.Printf("  local_path: %s\n", s.LocalPath)
	fmt.Printf("  include: %s\n", strings.Join(s.Include, ", "))
	fmt.Printf("  completion_strategy: %s\n", s.CompletionStrategy)
	// stable_for and stale_after are both writable by both verbs, so both
	// are reported: a command that can change a field and then prints a
	// set without it leaves an operator unable to confirm what it did.
	// stable_for is printed only under the strategy it belongs to,
	// because it is zero for every other one and a "0s" line invites the
	// reader to think it means something.
	if s.CompletionStrategy == "stable" {
		fmt.Printf("  stable_for: %s\n", s.StableFor)
	}
	// stale_after used to be the one field that survived a direct write
	// and not a routed one. The API accepted it on the way in
	// (BackupSetSpec) and UpdateBackupSetRequest could change it, and the
	// BackupSet the engine answered with carried no such property at all,
	// so a routed create printed "not reported" for a value the operator
	// had typed on that very command line. #555 put stale_after_seconds on
	// the wire and both routes now report it.
	//
	// Zero is still not a value, it is the absence of one:
	// config.Validate refuses a configuration whose stale_after is not
	// positive, so nothing that came out of a validated configuration can
	// reach the second branch, and what can is an engine older than that
	// field. Printing "0s" would be a specific claim about FR-24's
	// freshness budget made from a field nothing looked at, which is the
	// class of quiet wrongness this whole EPIC is about.
	if s.StaleAfter > 0 {
		fmt.Printf("  stale_after: %s\n", s.StaleAfter)
	} else {
		fmt.Printf("  stale_after: not reported (the engine that answered serves no stale_after; `" + cliecho.Binary + " sources` reads it from the configuration)\n")
	}
	fmt.Printf("  validator_id: %s\n", string(s.ValidatorID))
	fmt.Printf("  disabled: %v\n", s.Disabled)
	fmt.Printf("  read_only: %v\n", s.ReadOnly)
	// Issue #624, and printed ONLY when it is set, unlike every line above
	// it. That asymmetry is the field's own meaning rather than an
	// omission: true says a surface deliberately skipped a check it could
	// have run, and false has two readings this build cannot tell apart,
	// a set proven at creation and a set written before this deployment
	// recorded the difference at all. Printing "verified" for the second
	// would be a claim about a connection nobody ever made.
	//
	// So it says the one thing it knows and stays quiet about the one it
	// does not, which is the same call the detail page's banner makes.
	if s.ConnectionUnverified {
		fmt.Println("  connection: not verified (nothing has proven this source; `" + cliecho.Binary + " backup-set test-connection " + s.ID + "` clears this)")
	}
}

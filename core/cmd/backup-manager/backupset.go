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

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/service"
)

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
// What patch deliberately cannot change: the set's identity, its SSH key
// reference and its trusted host-key line. See
// core/service/backupsetupdate.go's own package doc for each.
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
}

// backupSetSharedFlagVerbs are the verbs that share declareBackupSetFlags
// and go through the switch in cmdBackupSet, in the order usage() lists
// them. The switch dispatches on these literals; this list exists so the
// usage messages and the test that checks usage() against the verbs read
// them from one place rather than three.
var backupSetSharedFlagVerbs = []string{"create", "patch", "remove"}

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

	switch operands[0] {
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

	// create only.
	keyFile        *string
	keyID          *string
	knownHostsLine *string
	trustHostKey   *bool
	disabled       *bool
	readOnly       *bool
	runNow         *bool
	stateDatabase  *string

	// Shared by create and patch, and not a field of the backup set: it
	// answers one refusal, on either verb. Removing a set frees its id up
	// (issue #391), so creating one over an id that already has artifacts
	// on record is the same move an edit makes and asks the same question
	// (issue #411).
	acknowledgeRepoint *bool
}

// create's and patch's own flags, by name, so each verb can refuse the
// other's rather than silently ignoring it. Silently ignoring is the
// worse failure by a distance: `backup-set patch ... --read-only` would
// exit 0 having changed nothing about the posture the operator just
// asked for. remove has no list of its own: it refuses everything but
// --config by construction (refuseEveryFlagBut).
var (
	backupSetCreateOnlyFlags = []string{"ssh-key-file", "ssh-key-id", "known-hosts-line", "trust-host-key", "disabled", "read-only", "run", "state-database"}

	// Empty since issue #411 made --acknowledge-repoint mean something on
	// create too, and kept rather than deleted along with its call site:
	// the guard is what makes a future patch-only flag refuse on create
	// automatically instead of being silently ignored there, which is the
	// failure this pair of lists exists to prevent.
	backupSetPatchOnlyFlags []string
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

	f.keyFile = fs.String("ssh-key-file", "", "create: path to the SSH PRIVATE KEY to import for this set. Read once, validated, and copied into this deployment's own key store; the original is left alone")
	f.keyID = fs.String("ssh-key-id", "", "create: the id of a key this deployment has already imported, instead of importing another copy")
	f.knownHostsLine = fs.String("known-hosts-line", "", "create: the exact known_hosts line to trust for this host, as `ssh-keyscan` prints it")
	f.trustHostKey = fs.Bool("trust-host-key", false, "create: probe the host now and trust whatever key answers. Trust on first use, and a real trust decision: use --known-hosts-line when the key is already known")
	f.disabled = fs.Bool("disabled", false, "create: save the set disabled, so no cycle runs it until it is enabled")
	f.readOnly = fs.Bool("read-only", false, "create: this set's remote source must never be deleted from (issue #282)")
	f.runNow = fs.Bool("run", false, "create: submit a run cycle immediately after the set is persisted")
	f.stateDatabase = fs.String("state-database", defaultStateDatabase,
		"create: the SQLite journal path a FIRST configuration names. Used only when there is no config.yaml yet; ignored, never applied, against an instance that already has one")

	f.acknowledgeRepoint = fs.Bool("acknowledge-repoint", false,
		"create, patch: confirm pointing this set at different data. On patch, needed only when --host, --remote-path or --local-path actually change on a set that already has artifacts on record; on create, only when this id already has artifacts on record and the set is being created somewhere other than where they came from. The refusal without it says what it costs")

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

// backupSetCreate is the `create` verb: the same operation POST
// /api/v1/backup-sets performs, through the same service layer.
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
		return createFirstConfig(ctx, configFile, *f.stateDatabase, *f.keyFile, *f.trustHostKey, req)
	}
	return createIntoExistingConfig(ctx, *f.cfgPath, *f.keyFile, *f.trustHostKey, req)
}

// backupSetPatch is the `patch` verb: the same operation PATCH
// /api/v1/backup-sets/{source}/{set} performs, through the same
// BackupService.UpdateBackupSet.
func backupSetPatch(f *backupSetFlags, id string) int {
	req, named := buildBackupSetPatch(f)
	req.AcknowledgeRepoint = *f.acknowledgeRepoint
	if !named {
		return usageError("backup-set patch: name at least one field to change (see --help); a patch that changes nothing would rewrite and reload the configuration to no effect")
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *f.cfgPath)
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

// backupSetRemove is the `remove` verb: the same operation DELETE
// /api/v1/backup-sets/{source}/{set} performs, through the same
// BackupService.RemoveBackupSet.
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
	svc, cleanup, err := openBackupService(ctx, *f.cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	return backupSetRemoveWith(ctx, svc, id, os.Stdout)
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
// (#187). It is a courtesy, not a precondition: a journal read failing
// here must not turn into "the removal failed", because the operator
// asked for a set to stop collecting and the configuration can be
// written, and leaving the set running for the sake of a number in a
// sentence would be the wrong trade. A count that could not be taken is
// said to be exactly that, never printed as 0, which is a specific and
// reassuring claim about a thing that was not looked at. This mirrors
// BackupService.artifactCountFor's own -1.
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
		_, _ = fmt.Fprintf(out, "could not count the backups that stay on storage (the journal read failed); they are still there, and `backup-manager artifacts` lists them\n")
	} else {
		_, _ = fmt.Fprintf(out, "%d backup(s) stay on storage and stay listed by `backup-manager artifacts`\n", kept)
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

// defaultStateDatabase is the SQLite journal path a FIRST configuration
// names when --state-database is not given. It is the packaged mount from
// container/compose.yaml, the same literal and for the same reason
// defaultConfigPath above is: this is the value an operator on the
// machine the installer just set up should never have to type.
// apps/generic's own --state-database carries the same default, which is
// what makes a config written from here and one written through the
// first-run wizard name the same file.
const defaultStateDatabase = "/data/state/state.db"

// createIntoExistingConfig folds one new backup set into a configuration
// that already exists, through the same BackupService method POST
// /api/v1/backup-sets calls.
func createIntoExistingConfig(ctx context.Context, configPath, keyFile string, trustHostKey bool, req service.CreateBackupSetRequest) int {
	svc, cleanup, err := openBackupService(ctx, configPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	if err := resolveKeyAndTrust(ctx, svc, keyFile, trustHostKey, &req); err != nil {
		return fail(err)
	}

	result, err := svc.CreateBackupSet(ctx, req)
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
func createFirstConfig(ctx context.Context, configFile, stateDatabase, keyFile string, trustHostKey bool, req service.CreateBackupSetRequest) int {
	if req.RunImmediately {
		// The service ignores RunImmediately here, because a first-run
		// instance has no BackupService to submit an operation to yet.
		// Saying so is better than accepting the flag and doing nothing
		// with it: an operator who asked for a run and got silence has
		// been told the backup started.
		return usageError("backup-set create: --run cannot be honoured while writing the first configuration, because there is no running service to submit a cycle to yet. Create the set, then `backup-manager run`")
	}

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

	created, err := firstRun.CreateInitialConfig(ctx, req)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("wrote the first configuration to %s\n", configFile)
	printBackupSet(created)
	return 0
}

// backupSetCreatePrereqs is the pair of pre-create steps both create
// paths need and both surfaces already expose: importing the private key,
// and settling the host key. Naming them as an interface rather than
// branching twice is what keeps the CLI from having a first-run-shaped
// copy of either.
type backupSetCreatePrereqs interface {
	ImportSSHKey(ctx context.Context, raw []byte, passphrase string) (service.SSHKeyRef, error)
	ProbeHostKey(ctx context.Context, host string, port int) (service.HostKeyProbe, error)
}

// resolveKeyAndTrust turns --ssh-key-file into an imported key id and
// --trust-host-key into a real known_hosts line, filling both into req.
// Either may already be settled by --ssh-key-id / --known-hosts-line, in
// which case the corresponding step does nothing.
//
// The key file is read here and handed straight to ImportSSHKey, which is
// the only thing that reads key material anywhere in this binary. It is
// never logged, never echoed and never written anywhere but the key store
// ImportSSHKey owns.
func resolveKeyAndTrust(ctx context.Context, svc backupSetCreatePrereqs, keyFile string, trustHostKey bool, req *service.CreateBackupSetRequest) error {
	if keyFile != "" {
		raw, err := os.ReadFile(keyFile)
		if err != nil {
			return fmt.Errorf("reading the SSH key at %s: %w", keyFile, err)
		}
		ref, err := svc.ImportSSHKey(ctx, raw, "")
		if err != nil {
			return err
		}
		// The fingerprint, never the path: what an operator needs to
		// confirm is which key was adopted, and where this deployment
		// keeps its copy is not theirs to have to know (SSHKeyRef's own
		// doc makes the same distinction for the HTTP layer).
		fmt.Printf("imported %s key %s\n", ref.Algorithm, ref.Fingerprint)
		req.SSHKeyID = ref.ID
	}

	if trustHostKey {
		if req.Host == "" {
			return errors.New("--trust-host-key needs a --host to probe")
		}
		probe, err := svc.ProbeHostKey(ctx, req.Host, probePortFor(req.Port))
		if err != nil {
			return fmt.Errorf("probing %s for its host key: %w", req.Host, err)
		}
		// Printed, always, and before the set is written. Trust on first
		// use is only defensible if the thing being trusted is stated
		// where somebody can compare it afterwards.
		fmt.Printf("trusting %s host key %s on first use\n", probe.Algorithm, probe.Fingerprint)
		req.KnownHostsLine = probe.KnownHostsLine
	}
	return nil
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
		case "acknowledge-repoint":
			// Read by the caller straight off its own flag, because it is
			// not a field of the backup set: it answers a refusal about
			// the fields above. Naming only this one changes nothing, so
			// it must not make an otherwise-empty patch look like a
			// patch.
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
	fmt.Printf("  stale_after: %s\n", s.StaleAfter)
	fmt.Printf("  validator_id: %s\n", string(s.ValidatorID))
	fmt.Printf("  disabled: %v\n", s.Disabled)
	fmt.Printf("  read_only: %v\n", s.ReadOnly)
}

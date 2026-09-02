package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/service"
)

// cmdBackupSet is `backup-manager backup-set <verb> <source/backup-set>
// [flags]`, the CLI's own half of the backup-set write surface.
//
// # Why the CLI gets this at all (issue #356)
//
// Until this existed, a backup set could be created two ways and neither
// was reachable from a terminal: POST /api/v1/backup-sets, and the Web
// UI wizard that calls it. So the one claim a user cares about, that a
// fresh install can be pointed at a machine and pull a backup off it,
// could not be driven end to end without a browser, and
// suites/equivalence recorded creation as a UI-only gap rather than as
// parity. Issue #356's two-machine end-to-end test is what needs this
// first: it installs onto a throwaway machine with the real installer and
// then has to say what to back up, over ssh, with no browser anywhere.
//
// # It shares the service layer, and that is the point
//
// Everything below is argument handling. The rules about what a backup
// set may be live in core/service (validateCreateRequest,
// newBackupSetFor, config.Validate) and are reached through the exact
// same methods apps/common/webhost's handlers call: ImportSSHKey,
// ProbeHostKey, and then either BackupService.CreateBackupSet or
// FirstRun.CreateInitialConfig. A second set of rules growing here is
// what backupsetcreate_test.go's refusal table exists to catch.
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
// # The shape, and why it mirrors `settings patch`
//
// A noun then a verb, the same as `catalog rebuild`, `quarantine
// revalidate` and `settings patch`. Issue #350's edit path is landing the
// same shape (`backup-set patch <source/backup-set>`) into this same
// command, which is deliberate: create and update are siblings, and a
// reader who has learned one has learned the other.
func cmdBackupSet(args []string) int {
	fs, cfgPath := newFlagSet("backup-set")

	host := fs.String("host", "", "create: remote.host, the machine being backed up")
	port := fs.Int("port", 0, "create: remote.port; 0 leaves it unset, which is the default SSH port")
	user := fs.String("user", "", "create: remote.user")

	keyFile := fs.String("ssh-key-file", "", "create: path to the SSH PRIVATE KEY to import for this set. Read once, validated, and copied into this deployment's own key store; the original is left alone")
	keyID := fs.String("ssh-key-id", "", "create: the id of a key this deployment has already imported, instead of importing another copy")

	knownHostsLine := fs.String("known-hosts-line", "", "create: the exact known_hosts line to trust for this host, as `ssh-keyscan` prints it")
	trustHostKey := fs.Bool("trust-host-key", false, "create: probe the host now and trust whatever key answers. Trust on first use, and a real trust decision: use --known-hosts-line when the key is already known")

	remotePath := fs.String("remote-path", "", "create: remote_path (absolute), the directory on the source to pull from")
	localPath := fs.String("local-path", "", "create: local_path (absolute), where artifacts land on this machine")
	include := fs.String("include", "", "create: include patterns, comma separated; empty matches everything")

	completionStrategy := fs.String("completion-strategy", "", `create: completion.strategy ("rename", "marker" or "stable")`)
	stableFor := fs.Duration("stable-for", 0, `create: completion.stable_for; required when the strategy is "stable"`)
	staleAfter := fs.Duration("stale-after", 0, "create: stale_after (FR-24's freshness budget); unset takes the service's own default")
	validatorID := fs.String("validator-id", "", `create: validation.validator_id, an id the validator catalog lists, or "" for none`)

	disabled := fs.Bool("disabled", false, "create: save the set disabled, so no cycle runs it until it is enabled")
	readOnly := fs.Bool("read-only", false, "create: this set's remote source must never be deleted from (issue #282)")
	runNow := fs.Bool("run", false, "create: submit a run cycle immediately after the set is persisted")

	stateDatabase := fs.String("state-database", defaultStateDatabase,
		"create: the SQLite journal path a FIRST configuration names. Used only when there is no config.yaml yet; ignored, never applied, against an instance that already has one")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 {
		return usageError(`backup-set: expected "create <source/backup-set>", a verb and exactly one backup set id`)
	}
	if operands[0] != "create" {
		return usageError("backup-set: %q is not a backup-set verb; today there is only create", operands[0])
	}
	sourceName, name, ok := splitBackupSetID(operands[1])
	if !ok {
		return usageError("backup-set: %q is not a backup set id; a backup set id is exactly source/name", operands[1])
	}

	// Both pairs below are alternatives, not a preference order. A caller
	// who passed both has not said which one they meant, and picking one
	// silently is how a set ends up trusting a key nobody looked at.
	if *keyFile != "" && *keyID != "" {
		return usageError("backup-set create: --ssh-key-file and --ssh-key-id are alternatives; pass one")
	}
	if *keyFile == "" && *keyID == "" {
		return usageError("backup-set create: name a key with --ssh-key-file (to import one) or --ssh-key-id (to reuse one already imported)")
	}
	if *knownHostsLine != "" && *trustHostKey {
		return usageError("backup-set create: --known-hosts-line and --trust-host-key are alternatives; pass one")
	}
	if *knownHostsLine == "" && !*trustHostKey {
		return usageError("backup-set create: settle the host key with --known-hosts-line (the line you already trust) or --trust-host-key (probe and trust whatever answers now)")
	}

	req := service.CreateBackupSetRequest{
		SourceName:         sourceName,
		Name:               name,
		Host:               *host,
		Port:               *port,
		User:               *user,
		SSHKeyID:           *keyID,
		KnownHostsLine:     *knownHostsLine,
		RemotePath:         *remotePath,
		LocalPath:          *localPath,
		Include:            splitIncludePatterns(*include),
		CompletionStrategy: *completionStrategy,
		StableFor:          *stableFor,
		StaleAfter:         *staleAfter,
		ValidatorID:        service.ValidatorID(*validatorID),
		Disabled:           *disabled,
		ReadOnly:           *readOnly,
		RunImmediately:     *runNow,
		Actor:              cliActor,
	}

	ctx := context.Background()
	// Resolved before the stat, never as supplied: --config may name the
	// configuration DIRECTORY the packaging mounts (#196), and statting
	// the directory would find it present on a completely empty install,
	// so the one shape that most needs the first-run path is the one
	// shape that would never take it. This is OpenConfigAndJournal's own
	// reasoning, applied one layer out because this command has to make
	// the same decision before it opens anything.
	configFile := config.ResolvePath(*cfgPath)
	if _, statErr := os.Stat(configFile); errors.Is(statErr, os.ErrNotExist) {
		return createFirstConfig(ctx, configFile, *stateDatabase, *keyFile, *trustHostKey, req)
	}
	return createIntoExistingConfig(ctx, *cfgPath, *keyFile, *trustHostKey, req)
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
		probe, err := svc.ProbeHostKey(ctx, req.Host, req.Port)
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

// splitIncludePatterns turns the comma-separated --include value into the
// slice the request carries. Whitespace around each pattern is trimmed
// because a shell-quoted list is usually written with spaces after the
// commas, and a leading space in an include pattern is never what anyone
// meant. An empty value is no patterns at all, which core reads as "no
// filter" rather than as "match nothing" (internal/discovery's
// includeMatches).
func splitIncludePatterns(raw string) []string {
	var patterns []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

// printBackupSet renders the set as it was actually persisted, rather
// than echoing the request back. It reports the identity and every field
// this command can set, and nothing that would leak this deployment's own
// filesystem layout beyond the paths the operator already typed: no key
// file path and no known_hosts path, the same rule service.SSHKeyRef
// follows for the API.
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

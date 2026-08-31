// firstrun.go is issue #176's answer to a fresh app-store install: the
// narrow surface an instance with NO configuration at all can safely
// expose, and the one operation that turns it into a configured one.
//
// # Why an absent configuration is not the same failure as a broken one
//
// Open (service.go) refuses to construct a BackupService without a valid
// config.yaml, and that is correct for a configuration that exists and
// does not validate: it is an operator's declared intent, possibly a
// deployment that has been backing up for months, and both "run degraded
// against it" and "offer to replace it" are worse than refusing. It was
// never correct for a configuration that is merely ABSENT. On every
// packaged platform (TrueNAS, Unraid, OpenMediaVault, Synology, UGOS) an
// administrator installs from a catalog and opens a web UI, and an
// install whose first mandatory step is `ssh` and a text editor is not
// that. So Open now reports ErrConfigAbsent for the absent case
// specifically, and a provider app reads that one error as "serve the
// setup flow" rather than "exit".
//
// # What this type deliberately is not
//
// FirstRun is NOT a second, parallel implementation of anything
// BackupService does. It resolves imported keys and known_hosts through
// the same helpers CreateBackupSet uses (backupsets.go's keysDirIn,
// resolveSSHKeyFileIn, newBackupSetFor), validates through the same
// config.Validate a hand-edited YAML file goes through at boot, and hands
// what it wrote straight to the same production Open. The moment
// CreateInitialConfig returns, this type has nothing further to do: the
// instance is configured, and every subsequent read and write goes
// through BackupService exactly as it always has.
//
// # What an API caller may decide, and what it may not
//
// Everything a caller supplies describes a BACKUP SET. Where this
// deployment's journal, configuration file and key store live comes from
// FirstRunDefaults, which the provider app fills in from its own flags
// and environment (the same values its packaging already fixes) and which
// no request can reach. A setup flow that let a caller name the state
// database would let whoever completes it decide where this process
// writes.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// ErrConfigAbsent is what Open returns when configPath does not exist at
// all. It is the ONLY startup failure that means "not configured yet"
// rather than "configured wrongly", and a provider app is expected to
// branch on it (errors.Is) to serve the first-run experience instead of
// exiting. Every other load failure — unparseable YAML, a config that
// does not validate, an unreadable file, an unusable state directory —
// stays exactly as fatal as it has always been.
var ErrConfigAbsent = errors.New("service: no configuration file")

// ErrAlreadyConfigured is returned by CreateInitialConfig when a
// configuration file already exists. Setup is a one-time door: once
// anything is on disk, however it got there (a second wizard submission
// racing the first, a hand-written file, a restored backup), the only way
// to change a configuration is the ordinary, authenticated write path.
var ErrAlreadyConfigured = errors.New("service: this instance is already configured")

// DefaultFirstRunPollInterval is the poll_interval a first-run
// configuration is written with when the provider app does not name one.
// One hour matches what scripts/deploy/deploy_generic.py has always
// rendered, so an instance set up through the UI schedules exactly like
// one deployed by that script rather than to a second, quietly different
// default.
const DefaultFirstRunPollInterval = time.Hour

// FirstRunDefaults is the deployment-shaped half of a first configuration:
// the values the provider app already owns (its own flags, environment and
// packaging), as opposed to the backup-set-shaped half an API caller
// supplies. See this file's package doc for why the split is a security
// boundary rather than a convenience.
type FirstRunDefaults struct {
	// ConfigPath is where the first config.yaml is written. It also
	// decides where imported SSH keys and per-set known_hosts files land,
	// exactly as it does for a configured instance (backupsets.go's
	// keysDirIn), so nothing has to move once setup completes.
	ConfigPath string

	// StateDatabase is the SQLite journal path the new configuration will
	// name (config.State.Database). Must be absolute: it is validated
	// here, at construction, so a misconfigured deployment fails at
	// process start where an operator reads the log, rather than at the
	// end of a wizard they have already filled in.
	StateDatabase string

	// PollInterval is the new configuration's top-level poll_interval.
	// Zero means DefaultFirstRunPollInterval.
	PollInterval time.Duration
}

// FirstRun serves the setup flow of an instance that has no configuration
// yet. Construct one with NewFirstRun, hand it to the HTTP layer
// (apps/common/webhost's RouterConfig.FirstRun), and stop using it the
// moment Configured reports true.
type FirstRun struct {
	defaults FirstRunDefaults

	// transport backs TestConnection. A configured instance reads its
	// transport off the wrapped internal/app.Service; there is no such
	// service here, so this type wires its own real rclone transport,
	// which is what makes the wizard's pre-save reachability check work
	// on a fresh install rather than being greyed out until after setup.
	transport transport.Transport

	// createMu serializes CreateInitialConfig against itself. The
	// exclusive create below (writeConfigExclusively) is what actually
	// makes a lost race impossible even across processes; this only keeps
	// two concurrent submissions in ONE process from both doing the work
	// that precedes it.
	createMu sync.Mutex
}

// NewFirstRun validates defaults and returns the first-run surface for
// them. It refuses a deployment it could never write a valid
// configuration for, rather than discovering that at the end of setup.
func NewFirstRun(defaults FirstRunDefaults) (*FirstRun, error) {
	if defaults.ConfigPath == "" {
		return nil, errors.New("service: first run needs a config path to write to")
	}
	if !filepath.IsAbs(defaults.ConfigPath) {
		return nil, fmt.Errorf("service: first run config path %q must be absolute", defaults.ConfigPath)
	}
	// Issue #196 made the packaged configuration mount a DIRECTORY, so
	// `--config /etc/backup-manager/config` is a spelling an operator is
	// actively invited to type (config.ResolvePath's own doc). Resolving
	// it here, once, at the boundary where a deployment's answer becomes
	// this type's, is what keeps everything derived from ConfigPath
	// agreeing with the file service.Open actually opens: the
	// configuration itself, and the ssh_keys/ and known_hosts.d/ stores
	// beside it.
	//
	// Without it the directory spelling fails in the direction that
	// reports success. Configured() would stat a directory that always
	// exists and call a completely empty install configured, and
	// ImportSSHKey derives filepath.Dir(ConfigPath), so a private key an
	// operator imports during setup would be written to the mount's
	// PARENT -- outside the volume, onto the read-only rootfs on every
	// shipped adapter.
	defaults.ConfigPath = config.ResolvePath(defaults.ConfigPath)
	if info, err := os.Stat(defaults.ConfigPath); err == nil && info.IsDir() {
		return nil, fmt.Errorf("service: first run config path %q is a directory, not a configuration file", defaults.ConfigPath)
	}
	if defaults.StateDatabase == "" {
		return nil, errors.New("service: first run needs a state database path")
	}
	if !filepath.IsAbs(defaults.StateDatabase) {
		return nil, fmt.Errorf("service: first run state database %q must be absolute", defaults.StateDatabase)
	}
	if defaults.PollInterval <= 0 {
		defaults.PollInterval = DefaultFirstRunPollInterval
	}
	return &FirstRun{defaults: defaults, transport: rclone.New()}, nil
}

// Configured reports whether a configuration file exists at this
// deployment's ConfigPath. It deliberately asks nothing about whether
// that file is VALID: an invalid configuration is still a configuration,
// and the one thing setup must never do is offer to replace one.
//
// A stat error other than "does not exist" (an unreadable directory, say)
// also reads as configured, on the same principle: the honest answer to
// "may I create the first configuration here" when the answer cannot be
// established is no.
func (f *FirstRun) Configured() bool {
	_, err := os.Stat(f.defaults.ConfigPath)
	return !errors.Is(err, os.ErrNotExist)
}

// ImportSSHKey is BackupService.ImportSSHKey for an instance that has no
// BackupService yet: the same validation, the same 0600 file, in the same
// directory beside the config file, so the key an operator imports during
// setup is the key the configuration written seconds later points at.
func (f *FirstRun) ImportSSHKey(_ context.Context, raw []byte) (SSHKeyRef, error) {
	return importSSHKeyInto(f.defaults.ConfigPath, raw)
}

// ProbeHostKey is BackupService.ProbeHostKey for an unconfigured
// instance. It needs nothing from a configuration at all — it fetches a
// host key and neither trusts nor persists anything — so this is the same
// call, reachable earlier.
func (f *FirstRun) ProbeHostKey(ctx context.Context, host string, port int) (HostKeyProbe, error) {
	res, err := rclone.ProbeHostKey(ctx, host, port)
	if err != nil {
		return HostKeyProbe{}, err
	}
	return HostKeyProbe{Algorithm: res.Algorithm, Fingerprint: res.Fingerprint, KnownHostsLine: res.KnownHostsLine}, nil
}

// TestConnection is BackupService.TestConnection for an unconfigured
// instance, against this type's own transport. Read-only in effect
// (docs/EPIC-B-multi-nas.md §50's "test SSH"), and the reason an operator
// can find out their credentials are wrong before writing a
// configuration around them rather than after.
func (f *FirstRun) TestConnection(ctx context.Context, req ConnectionTestRequest) (ConnectionTestResult, error) {
	return testConnectionVia(ctx, f.transport, f.defaults.ConfigPath, req)
}

// CreateInitialConfig writes this deployment's FIRST configuration: the
// deployment-owned state database and poll interval from
// FirstRunDefaults, plus one source holding the one backup set req
// describes. It returns the persisted backup set.
//
// It is related to CreateBackupSet (backupsets.go) but deliberately not
// the same operation, and the difference is the whole reason this
// function exists. CreateBackupSet re-reads an existing file, folds a set
// into it, and REPLACES it by rename; there is nothing to re-read here,
// and a rename is exactly the wrong primitive, because it would silently
// overwrite whatever appeared at that path in the meantime. So the write
// is an exclusive create (writeConfigExclusively below), which makes
// "somebody else got there first" a refusal rather than a data loss, with
// no lock and no check-then-write window.
//
// The ordering matches CreateBackupSet's, for the same reason: everything
// that can fail happens before anything is persisted. The request is
// validated, the key is resolved, the whole assembled configuration goes
// through the same config.Validate a hand-edited file goes through at
// boot, the state directory is checked (§46.1's own first step, run here
// because this is the moment the path stops being a default and becomes
// this deployment's committed answer), and any selected validator is
// resolved against the catalog. Only then is the file created.
//
// RunImmediately is ignored. A first-run instance has no BackupService to
// submit an operation to yet, and starting a backup during setup is not
// something this function can honestly promise; the operator runs the
// first cycle from the application once it is up.
func (f *FirstRun) CreateInitialConfig(_ context.Context, req CreateBackupSetRequest) (BackupSet, error) {
	f.createMu.Lock()
	defer f.createMu.Unlock()

	if f.Configured() {
		return BackupSet{}, ErrAlreadyConfigured
	}
	if err := validateCreateRequest(req); err != nil {
		return BackupSet{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	keyFile, err := resolveSSHKeyFileIn(f.defaults.ConfigPath, req.SSHKeyID)
	if err != nil {
		return BackupSet{}, err
	}

	sourceName := req.SourceName
	if sourceName == "" {
		sourceName = defaultSourceName
	}

	newSet, err := newBackupSetFor(f.defaults.ConfigPath, sourceName, keyFile, req)
	if err != nil {
		return BackupSet{}, err
	}

	cfg := &config.Config{
		PollInterval: config.Duration(f.defaults.PollInterval),
		State:        config.State{Database: f.defaults.StateDatabase},
		Sources:      []config.Source{{Name: sourceName, BackupSets: []config.BackupSet{newSet}}},
	}
	// Retention and Alerts are left at their zero values on purpose:
	// config.Validate resolves both to this product's documented defaults
	// (FR-18's tier chain, alerting off), which is exactly what a
	// configuration nobody has edited yet should mean. Writing today's
	// resolved numbers into the file instead would freeze them, so an
	// operator who never touched retention would silently keep an old
	// release's policy across an upgrade.
	if err := cfg.Validate(); err != nil {
		// cfg.Validate's message is built from internal/config's own field
		// descriptions and this caller's own values, never from a state or
		// rclone error, so it is safe to hand back to an API caller —
		// exactly as CreateBackupSet already does.
		return BackupSet{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// §46.1's "validate state directory", run here rather than left to
	// the Open that follows: this is the moment the deployment's state
	// path stops being a default and becomes a committed part of a
	// configuration file, and a read-only or misconfigured volume mount
	// is the single most likely thing to be wrong about it on a NAS. An
	// operator gets told that now, with the wizard still on screen,
	// rather than by an instance that reports setup succeeded and then
	// will not come up.
	if err := validateStateDir(cfg.State.Database); err != nil {
		return BackupSet{}, err
	}

	// Same ordering, and the same reasoning, as CreateBackupSet: every
	// fallible part of validator resolution runs before the write, so a
	// configuration this process refuses to finish setting up is never
	// the configuration left on disk.
	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return BackupSet{}, err
	}

	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: encoding the first configuration: %w", err)
	}
	if err := writeConfigExclusively(f.defaults.ConfigPath, encoded); err != nil {
		return BackupSet{}, err
	}
	applyValidators()

	return toServiceBackupSet(sourceName, findBackupSet(cfg, sourceName, req.Name)), nil
}

// writeConfigPayload is the file-write half of the two create paths
// below, behind a package variable so a test can inject the failure the
// whole shape of those paths exists to survive: an ENOSPC on a NAS data
// volume, an EIO, a quota. Production never replaces it, and neither
// path is honest about "nothing partial is left behind" unless something
// can actually make the write fail.
var writeConfigPayload = func(f *os.File, b []byte) error {
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

// writeConfigExclusively creates path and writes b to it, failing with
// ErrAlreadyConfigured if anything is already there.
//
// This is deliberately NOT writeConfigBytesAtomically (backupsets.go).
// That function's temp-file-plus-rename is right for REPLACING a
// configuration whose current content the caller has just read; here
// there is no current content, and rename would happily clobber a file
// that appeared in between.
//
// It is not a bare O_CREATE|O_EXCL either, and that is the second half of
// the shape. An exclusive create makes the claim indivisible but says
// nothing about what is at path once the write that follows fails, and
// what it leaves is a zero-length or truncated config.yaml. That file is
// the worst possible outcome of a failed setup on the deployment this
// whole issue is for: Configured() reports true for any file that exists,
// so setup answers 409 from then on; OpenConfigAndJournal finds a file
// and so never returns ErrConfigAbsent, so the provider app treats it as
// a broken configuration and exits; and the container crash-loops until
// somebody with a shell on the NAS deletes a file nobody told them about.
//
// So the write goes to a temp file in the target's OWN directory, is
// synced there, and is only then given the real name with os.Link.
// link(2) fails with EEXIST if the target exists, so "somebody else got
// there first" is still one indivisible filesystem decision, and no
// reader can ever observe a partial config.yaml at path, not even for an
// instant. A filesystem with no hard links falls back to the exclusive
// create, which removes what it created on any failure.
//
// The file and then its directory are fsynced for the same reason the
// atomic path does it: a configuration an operator was told was saved has
// to still be there after the crash that follows.
func writeConfigExclusively(path string, b []byte) error {
	dir := filepath.Dir(path)

	// os.CreateTemp creates with 0600, which is the mode this file has to
	// land on anyway: it names a host, a user and the path to a private
	// key, exactly as writeConfigBytesAtomically's replacement does.
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.partial")
	if err != nil {
		return fmt.Errorf("service: creating the first configuration: %w", err)
	}
	tmpPath := tmp.Name()
	// Removed on EVERY path, success included: a successful link leaves
	// one inode reachable under two names, and the dot-prefixed one is
	// not something anybody should later find beside a live config.
	defer func() { _ = os.Remove(tmpPath) }()

	if err := writeConfigPayload(tmp, b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("service: writing the first configuration: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("service: closing the first configuration: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyConfigured
		}
		// Anything else is treated as "this filesystem will not link"
		// rather than classified by errno, because the fallback re-runs
		// the same claim through a different primitive and reports its
		// own failure if the real problem was the directory. Nothing is
		// swallowed: a read-only mount fails again, one line later, with
		// the error that names it.
		return createConfigExclusivelyRemovingOnError(path, b)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("service: syncing the configuration directory: %w", err)
	}
	return nil
}

// createConfigExclusivelyRemovingOnError is writeConfigExclusively for a
// filesystem that has no hard links (a FAT-formatted external volume on a
// NAS, some network mounts). It keeps the exclusive claim and buys back
// all-or-nothing with a deferred remove instead of a second name: a
// concurrent reader can see a partial file for the moment between the
// failed write and the remove, which the link path never allows, but the
// install is not left bricked either way.
func createConfigExclusivelyRemovingOnError(path string, b []byte) (retErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyConfigured
		}
		return fmt.Errorf("service: creating the first configuration: %w", err)
	}
	// The file this function created is removed again on every error
	// path, so a retry after a full disk is not refused by the truncated
	// remains of the attempt that failed.
	defer func() {
		if retErr != nil {
			_ = os.Remove(path)
		}
	}()

	if err := writeConfigPayload(file, b); err != nil {
		_ = file.Close()
		return fmt.Errorf("service: writing the first configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("service: closing the first configuration: %w", err)
	}
	if err := fsyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("service: syncing the configuration directory: %w", err)
	}
	return nil
}

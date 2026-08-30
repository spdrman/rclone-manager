// This file is issue #146 (B2.7)'s backup-set persistence surface: the
// create-backup-set, SSH-key-import, host-key-probe and connection-test
// capabilities the add-backup-set wizard (#98) needs and, until now,
// had nothing on the backend to call.
//
// # Storage mechanism
//
// FR-5 (internal/config's package doc) already says configuration
// "must never require recompilation" and changing it "is an edit and a
// restart, never a rebuild" — that "edit" has, until now, only ever been
// a human opening the YAML file directly. CreateBackupSet is the first
// thing in this codebase that performs that edit programmatically: it
// reads the current config file, appends the new backup set, validates
// the WHOLE resulting config through the exact same config.Validate a
// hand-edited file goes through at boot, writes it back atomically, and
// then does the in-process equivalent of "and a restart" by rebuilding
// this BackupService's internal/app.Service from the new config and
// recomputing ConfigRevision — so the change is visible to every other
// method on this BackupService (ListBackupSets, GetBackupSet,
// SubmitRunCycle) immediately, without an operator restarting the
// process, and visible to `backup-manager sources`/any other CLI
// invocation the next time one runs, since that command already reads
// the same file fresh on every invocation (core/cmd/backup-manager/
// sources.go).
//
// This was previously out of core/service's scope by design (see
// ConfigRevision's doc in service.go and ActionRunCycle's in
// operations.go): this file is what closes that gap.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// defaultSourceName groups every backup set created through the API under
// one config.Source, "api", when the caller does not name one. The
// wizard (#98) has no UI concept of "source" yet — BackupSetWizardPage.tsx
// collects a backup set name only — so every set it creates lands here.
// A caller that DOES have a source concept (a future multi-tenant UI, the
// CLI) may set CreateBackupSetRequest.SourceName to group its own sets
// under a different one; existing config-file-defined sources are left
// alone either way.
const defaultSourceName = "api"

// defaultStaleAfter is used when a request does not set StaleAfter. The
// wizard has no field for this yet (BackupSetWizardPage.tsx's "Discovery"
// step does not collect it); config.Validate refuses a zero StaleAfter
// outright (there is no config-file default either, by that package's own
// documented decision), so a caller-facing default has to live somewhere,
// and here is that one place, not duplicated at the HTTP layer.
const defaultStaleAfter = 48 * time.Hour

// ErrBackupSetNotFound is returned by GetBackupSet when no backup set
// matches the given id.
var ErrBackupSetNotFound = errors.New("service: backup set not found")

// ErrConfigNotFileBacked is returned by CreateBackupSet when this
// BackupService was built with New directly (every core/ test) rather
// than Open, and so has no configPath to persist a change to. Every
// production BackupService goes through Open and never hits this.
var ErrConfigNotFileBacked = errors.New("service: this backup service has no configuration file to persist to")

// ErrSSHKeyNotFound is returned by CreateBackupSet when
// CreateBackupSetRequest.SSHKeyID does not match a key ImportSSHKey
// previously persisted.
var ErrSSHKeyNotFound = errors.New("service: imported SSH key not found")

// BackupSet is the plain, provider-agnostic shape of one configured
// backup set (mirrors config.BackupSet the same way Operation mirrors
// state.Operation): a caller outside core/ never sees a config.BackupSet
// or a model.BackupSetID directly.
type BackupSet struct {
	ID         string // "source/name", model.BackupSetID.String()
	SourceName string
	Name       string

	Host string
	Port int
	User string

	RemotePath string
	LocalPath  string
	Include    []string

	CompletionStrategy string // "rename", "marker" or "stable"

	Disabled bool
}

// CreateBackupSetRequest is what a caller submits to persist one new
// backup set. Every SSH-facing field here (Host/Port/User/SSHKeyID/
// KnownHostsLine) names something the wizard already collected through
// ImportSSHKey and ProbeHostKey below — CreateBackupSetRequest never
// accepts raw key material or an unverified fingerprint itself, on the
// same "carry a reference, not the secret" principle config.Key already
// follows for every other key source in this codebase.
type CreateBackupSetRequest struct {
	// SourceName groups this backup set with any other sharing the same
	// value. Optional; defaults to defaultSourceName.
	SourceName string
	// Name is this backup set's own id within its source (FR-7).
	// Required.
	Name string

	Host string
	Port int
	User string

	// SSHKeyID is the ID an earlier ImportSSHKey call returned. Required:
	// this issue's scope is the import path only (see ImportSSHKey's own
	// doc); "generate a key for me" and "reuse a managed key" are wizard
	// options #98 shipped that have no backend yet, and a caller that
	// reaches this method for either has nothing valid to put here. A
	// caller outside core/ never learns, or has to carry around, the
	// server-side file path that ID resolves to (see SSHKeyRef's doc);
	// this method resolves it internally.
	SSHKeyID string

	// KnownHostsLine is the exact known_hosts line an earlier
	// ProbeHostKey call returned for this Host/Port. Required: this is
	// what makes "the wizard's Verify-server step showed a fingerprint
	// and the operator clicked Trust" become the actual trust anchor a
	// real connection checks against, rather than this method silently
	// re-probing (and re-trusting whatever answers) at save time.
	KnownHostsLine string

	RemotePath string
	LocalPath  string
	Include    []string

	// CompletionStrategy is "rename", "marker" or "stable"
	// (config.Completion.Strategy; FR-8).
	CompletionStrategy string
	// StableFor is required, and only used, when CompletionStrategy is
	// "stable".
	StableFor time.Duration

	// StaleAfter defaults to defaultStaleAfter when zero.
	StaleAfter time.Duration

	// Disabled excludes this backup set from RunCycle from the moment
	// it is created ("Save disabled", the wizard's third save tier).
	Disabled bool

	// RunImmediately submits a run_cycle operation (the same one
	// SubmitRunCycle exposes) immediately after this backup set is
	// durably persisted and hot-reloaded — "Save, enable & run", the
	// wizard's first save tier. Ignored (never runs anything) when
	// Disabled is true: a set saved disabled is, by definition, not
	// meant to run yet. There is no per-backup-set-scoped run in this
	// codebase yet (see operations.go's ActionRunCycle doc); this runs
	// the same whole-config cycle SubmitRunCycle always has, which
	// necessarily also covers this new set as long as it is enabled.
	RunImmediately bool

	// Actor is the authenticated caller's identity, recorded on the
	// run_cycle operation RunImmediately submits. Unused when
	// RunImmediately is false or Disabled is true.
	Actor string
}

// CreateBackupSetResult is what CreateBackupSet returns: the persisted
// backup set, plus the run_cycle Operation it kicked off when
// RunImmediately was set and honoured (nil otherwise — including when
// Disabled made RunImmediately a no-op).
type CreateBackupSetResult struct {
	Set       BackupSet
	Operation *Operation
}

// ListBackupSets returns every backup set this BackupService currently
// has configured, across every source, in config order. Like Sources
// (internal/app), this is a pure read of the currently-loaded Config: no
// journal, no remote.
func (b *BackupService) ListBackupSets(_ context.Context) ([]BackupSet, error) {
	// b.state.Load() is a single atomic read of the current {inner,
	// revision} pair — safe with no lock, even while CreateBackupSet is
	// concurrently hot-reloading it (see BackupService.state's own doc).
	st := b.state.Load()

	var out []BackupSet
	for _, src := range st.inner.Config.Sources {
		for _, bs := range src.BackupSets {
			out = append(out, toServiceBackupSet(src.Name, bs))
		}
	}
	return out, nil
}

// GetBackupSet returns one backup set by its "source/name" id, or
// ErrBackupSetNotFound.
func (b *BackupService) GetBackupSet(_ context.Context, id string) (BackupSet, error) {
	st := b.state.Load()

	for _, src := range st.inner.Config.Sources {
		for _, bs := range src.BackupSets {
			if src.Name+"/"+bs.Name == id {
				return toServiceBackupSet(src.Name, bs), nil
			}
		}
	}
	return BackupSet{}, ErrBackupSetNotFound
}

// CreateBackupSet validates req, persists it into the config file this
// BackupService was opened from, and hot-reloads this BackupService so
// the new backup set is immediately live — see this file's package doc
// for the full sequence and why it is safe to do in-process.
func (b *BackupService) CreateBackupSet(ctx context.Context, req CreateBackupSetRequest) (CreateBackupSetResult, error) {
	if b.configPath == "" {
		return CreateBackupSetResult{}, ErrConfigNotFileBacked
	}
	if err := validateCreateRequest(req); err != nil {
		return CreateBackupSetResult{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	keyFile, err := b.resolveSSHKeyFile(req.SSHKeyID)
	if err != nil {
		return CreateBackupSetResult{}, err
	}

	sourceName := req.SourceName
	if sourceName == "" {
		sourceName = defaultSourceName
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	// Re-read from disk, not b.state.Load().inner.Config: this is the same "always
	// read fresh" discipline `backup-manager sources` already uses
	// (core/cmd/backup-manager/sources.go), and it is what makes this
	// method safe even if configPath was edited by hand (or by a second
	// process) since this BackupService last loaded it — the write below
	// is always based on the file's actual current content, never a
	// possibly-stale in-memory copy.
	cfg, err := config.Load(b.configPath)
	if err != nil {
		return CreateBackupSetResult{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	newSet := config.BackupSet{
		Name: req.Name,
		Remote: config.Remote{
			Type:       "sftp",
			Host:       req.Host,
			Port:       req.Port,
			User:       req.User,
			Key:        config.Key{File: keyFile},
			KnownHosts: "", // filled in below, once the known_hosts file exists
		},
		RemotePath: req.RemotePath,
		LocalPath:  req.LocalPath,
		Include:    req.Include,
		Completion: config.Completion{Strategy: req.CompletionStrategy},
		StaleAfter: config.Duration(orDefault(req.StaleAfter, defaultStaleAfter)),
		// Hash is deliberately left "" (transfer verification alone),
		// never a caller-facing option yet (the wizard's step 5
		// "Checksum verification" toggle is still decorative, per #98).
		// This is not a weaker default chosen for convenience: docs/
		// ssh-setup.md's whole recommended account shape is a chrooted,
		// forced internal-sftp account with no shell, and
		// internal/transport/rclone.RemoteHash cannot compute a hash at
		// all against that shape (see
		// core/tests/sftpintegration.TestSFTPHashCapability, proven
		// while writing this file's own docker-backed integration test:
		// Hash: "sha256" against a real, correctly-hardened deployment
		// fails every artifact at VERIFYING, every time, not just
		// sometimes). internal/lifecycle/verify.go's own doc calls
		// Hash == "" trusting transfer verification alone the honest
		// posture when hash capability is absent, which for this
		// account shape is always.
		Validation: config.Validation{Hash: ""},
		Disabled:   req.Disabled,
	}
	if req.CompletionStrategy == "stable" {
		newSet.Completion.StableFor = config.Duration(req.StableFor)
	}

	knownHostsPath, err := b.writeKnownHosts(sourceName, req.Name, req.KnownHostsLine)
	if err != nil {
		return CreateBackupSetResult{}, fmt.Errorf("service: persisting known_hosts: %w", err)
	}
	newSet.Remote.KnownHosts = knownHostsPath

	found := false
	for i := range cfg.Sources {
		if cfg.Sources[i].Name == sourceName {
			cfg.Sources[i].BackupSets = append(cfg.Sources[i].BackupSets, newSet)
			found = true
			break
		}
	}
	if !found {
		cfg.Sources = append(cfg.Sources, config.Source{Name: sourceName, BackupSets: []config.BackupSet{newSet}})
	}

	if err := cfg.Validate(); err != nil {
		// cfg.Validate's ValidationError text is built entirely from this
		// package's own field descriptions and the caller's own request
		// values (see config/validate.go) — never from an internal/state
		// or rclone error string — so it is safe to echo back to an API
		// caller as ErrInvalidRequest, exactly like every other
		// request-validation failure this package returns.
		return CreateBackupSetResult{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	if err := writeConfigAtomically(b.configPath, cfg); err != nil {
		return CreateBackupSetResult{}, fmt.Errorf("service: persisting configuration: %w", err)
	}

	// b.state.Store below is the one atomic swap that makes this new
	// config take effect: every concurrent reader (ListBackupSets,
	// ConfigRevision, SubmitRunCycle, the scheduler's tick, ...) either
	// still sees the PREVIOUS {inner, revision} pair or the new one in
	// full, never a mix of old inner with new revision or vice versa
	// (see BackupService.state's own doc). prevInner is read once, before
	// the swap, purely to carry the already-wired Transport forward —
	// this method is the only writer of b.state while configMu is held,
	// so this read cannot itself race the swap below.
	prevInner := b.state.Load().inner
	newInner := app.New(cfg, b.journal, prevInner.Transport, b.logger)
	// Alerting is re-decided from the config file this method just
	// re-read, then carried across the swap. This is the one moment an
	// edited alerts.enabled can take effect in a running process, so it
	// is the one moment it must not be ignored: an administrator who set
	// alerts.enabled: false and then added a backup set kept getting
	// notified until the next restart, and one who turned it on stayed
	// silent, while repeated_failure_threshold from the same block did
	// hot-reload. AdoptAlerts re-reads the opt-in and carries the
	// dispatcher only if it is still on, because the dispatcher holds
	// which conditions are currently firing (internal/alert's
	// de-duplication state) and rebuilding it would re-alert every
	// still-unresolved condition the next time a cycle ran, purely
	// because somebody added a backup set. When it declines (alerting was
	// off before this reload, or has just been turned off), the question
	// is settled from b.alertSink instead, which is what makes turning
	// alerting ON take effect here too.
	if !newInner.AdoptAlerts(prevInner.Alerts) && b.alertSink != nil {
		newInner.EnableAlerts(sinkAdapter{sink: b.alertSink})
	}
	newRevision := computeConfigRevision(cfg)
	b.state.Store(&configState{inner: newInner, revision: newRevision})

	created := toServiceBackupSet(sourceName, findBackupSet(cfg, sourceName, req.Name))
	result := CreateBackupSetResult{Set: created}

	if req.RunImmediately && !req.Disabled {
		op, err := b.SubmitRunCycle(ctx, RunCycleRequest{
			IdempotencyKey: "create:" + created.ID + ":" + uuid.NewString(),
			Actor:          req.Actor,
			ConfigRevision: newRevision,
		})
		if err != nil {
			// The backup set is already durably created and live at this
			// point (the config write and hot-reload above already
			// succeeded); a failure to kick off its first run is reported
			// back so a caller/UI can surface it, but it is never treated
			// as the creation itself having failed — retrying the whole
			// create would collide on the now-persisted backup set id.
			return result, fmt.Errorf("service: backup set created, but starting the requested run failed: %w", err)
		}
		result.Operation = &op
	}

	return result, nil
}

// writeKnownHosts writes line to a dedicated known_hosts file for
// sourceName/name, under the same directory as the state journal
// (b.configPath's sibling "known_hosts" directory, mirroring
// docs/ssh-setup.md's own convention of one known_hosts file the
// operator maintains by hand). One file per backup set, not one shared
// file for every API-created set, so trusting (or later, rotating) one
// set's host key can never collide with another's.
//
// # Path safety (mandatory review finding M2, PR #155)
//
// sourceName/name are concatenated into ONE filename token
// (sourceName+"_"+name+"_known_hosts"), then filepath.Join'd onto dir.
// filepath.Join calls Clean, so an embedded "/" or ".." in either value
// resolves as a real path, not a literal character in a filename —
// verified empirically before this fix: dir=".../known_hosts.d",
// name="../../../../tmp/evil" produced a path outside both the
// known_hosts sandbox and the config directory. validateCreateRequest
// (below) is CreateBackupSet's very first call and already refuses any
// such Name/SourceName before this method is ever reached (its own
// validPathSegment check), so this is defense in depth, not the primary
// guard: even if some future caller reached this method with a value
// validateCreateRequest never saw, the filepath.Rel check below refuses
// to write outside dir regardless of what already let sourceName/name
// through.
func (b *BackupService) writeKnownHosts(sourceName, name, line string) (string, error) {
	dir := filepath.Join(filepath.Dir(b.configPath), "known_hosts.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, sourceName+"_"+name+"_known_hosts")
	if rel, err := filepath.Rel(dir, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: source_name/name must not resolve outside the known_hosts directory", ErrInvalidRequest)
	}
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// validPathSegment reports whether v is safe to fold into one path
// segment/filename token on this process's own filesystem: no path
// separator (which would let a later filepath.Join resolve v as more
// than one segment), no bare "." or ".." directory reference, no control
// character, and no leading/trailing whitespace. writeKnownHosts (above)
// is what actually needs this — sourceName and name both become part of
// one filename it writes — but the check lives in validateCreateRequest
// (below), CreateBackupSet's very first call, so a path-unsafe value is
// refused before ANY filesystem write happens, not discovered by
// whichever write site happens to be reached first.
func validPathSegment(what, v string) error {
	switch {
	case v == "":
		return fmt.Errorf("%s must not be empty", what)
	case strings.ContainsAny(v, "/\\\x00\n\r"):
		return fmt.Errorf("%s %q must not contain a path separator or control character", what, v)
	case v == "." || v == "..":
		return fmt.Errorf("%s %q must not be a directory reference", what, v)
	case strings.TrimSpace(v) != v:
		return fmt.Errorf("%s %q must not have leading or trailing whitespace", what, v)
	}
	return nil
}

func validateCreateRequest(req CreateBackupSetRequest) error {
	var problems []string
	if req.Name == "" {
		problems = append(problems, "name is required")
	} else if err := validPathSegment("name", req.Name); err != nil {
		problems = append(problems, err.Error())
	}
	if req.SourceName != "" {
		if err := validPathSegment("source_name", req.SourceName); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if req.Host == "" {
		problems = append(problems, "host is required")
	}
	if req.User == "" {
		problems = append(problems, "user is required")
	}
	if req.SSHKeyID == "" {
		problems = append(problems, "ssh_key_id is required (import an SSH key first)")
	}
	if req.KnownHostsLine == "" {
		problems = append(problems, "known_hosts_line is required (probe and trust the host key first)")
	}
	if req.RemotePath == "" {
		problems = append(problems, "remote_path is required")
	}
	if req.LocalPath == "" {
		problems = append(problems, "local_path is required")
	}
	switch req.CompletionStrategy {
	case "rename", "marker", "stable":
	default:
		problems = append(problems, `completion_strategy must be "rename", "marker" or "stable"`)
	}
	if req.CompletionStrategy == "stable" && req.StableFor <= 0 {
		problems = append(problems, `stable_for must be positive when completion_strategy is "stable"`)
	}
	if len(problems) == 0 {
		return nil
	}
	msg := problems[0]
	for _, p := range problems[1:] {
		msg += "; " + p
	}
	return errors.New(msg)
}

func toServiceBackupSet(sourceName string, bs config.BackupSet) BackupSet {
	return BackupSet{
		ID:                 sourceName + "/" + bs.Name,
		SourceName:         sourceName,
		Name:               bs.Name,
		Host:               bs.Remote.Host,
		Port:               bs.Remote.Port,
		User:               bs.Remote.User,
		RemotePath:         bs.RemotePath,
		LocalPath:          bs.LocalPath,
		Include:            bs.Include,
		CompletionStrategy: bs.Completion.Strategy,
		Disabled:           bs.Disabled,
	}
}

func findBackupSet(cfg *config.Config, sourceName, name string) config.BackupSet {
	for _, src := range cfg.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == name {
				return bs
			}
		}
	}
	return config.BackupSet{}
}

func orDefault(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// writeConfigAtomically marshals cfg as YAML and writes it to path via a
// temp-file-plus-rename, so a reader (this process's own next config.Load
// re-read, or an operator's own `cat`) never observes a partially-written
// file. It fsyncs the temp file before the rename and the containing
// directory after it (via snapshot.go's fsyncDir), because os.Rename's
// atomicity on a POSIX filesystem only promises no reader sees a half-file
// — it promises nothing about the rename itself surviving a power loss,
// and a backup set an operator was told was saved has to still be there
// after the crash that follows.
func writeConfigAtomically(path string, cfg *config.Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding configuration: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing configuration file: %w", err)
	}
	if err := fsyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("syncing the configuration directory: %w", err)
	}
	return nil
}

// SSHKeyRef identifies one imported SSH private key, without ever
// carrying its bytes. ID is the opaque reference a caller outside core/
// carries around and later passes back as CreateBackupSetRequest.SSHKeyID
// or ConnectionTestRequest.SSHKeyID; KeyFile is the resolved server-side
// path those two methods use internally and is never sent back over the
// wire by the HTTP layer (apps/common/webhost), so an API caller never
// learns this process's own filesystem layout. Algorithm/Fingerprint is
// what a caller/UI displays instead ("Key imported / SHA256:…"), never
// the key itself.
type SSHKeyRef struct {
	ID          string
	KeyFile     string
	Algorithm   string
	Fingerprint string
}

// keysDir is where ImportSSHKey persists imported private key files:
// b.configPath's sibling "ssh_keys" directory, so an imported key mounts
// (or backs up) alongside the config file it is referenced from, the
// same locality docs/ssh-setup.md already recommends for a manually
// provisioned key.
func (b *BackupService) keysDir() (string, error) {
	if b.configPath == "" {
		return "", ErrConfigNotFileBacked
	}
	dir := filepath.Join(filepath.Dir(b.configPath), "ssh_keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// resolveSSHKeyFile turns an SSHKeyRef.ID an earlier ImportSSHKey call
// returned back into the server-side path ImportSSHKey wrote it to,
// confirming the file actually exists (ErrSSHKeyNotFound if not) rather
// than handing config.Validate a path that will only fail much later, at
// connection time. id is checked for a path separator before it is ever
// joined onto keysDir()'s own path: ImportSSHKey only ever generates a
// bare uuid.NewString() as an ID (never containing "/"), so a caller-
// supplied id containing one is never a legitimate reference and is
// refused outright rather than let filepath.Join resolve it somewhere
// else on disk.
func (b *BackupService) resolveSSHKeyFile(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		return "", fmt.Errorf("%w: invalid ssh_key_id", ErrInvalidRequest)
	}
	dir, err := b.keysDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, id)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%w: %s", ErrSSHKeyNotFound, id)
	}
	return path, nil
}

// ImportSSHKey validates raw as an unencrypted SSH private key (reusing
// internal/transport/rclone.ValidateImportedPrivateKey — the exact check
// a key.env/key.command resolver's own output already goes through, see
// that function's doc) and persists it to a dedicated file with 0600
// permissions, the same trust level docs/ssh-setup.md already assumes
// for a manually provisioned key.file.
//
// This is the backend half of the wizard's "Import key" step (#98):
// BackupSetWizardPage.tsx already collects the pasted key client-side and
// discards it from the page the instant this call returns; raw is never
// logged, never echoed back, and this process's own copy of it is
// overwritten immediately after the file write below.
func (b *BackupService) ImportSSHKey(_ context.Context, raw []byte) (SSHKeyRef, error) {
	// The validated obs.Secret wrapper is not itself what gets persisted:
	// raw (below) is, once validation confirms it is safe to write.
	_, algorithm, fingerprint, err := rclone.ValidateImportedPrivateKey(raw)
	if err != nil {
		return SSHKeyRef{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	dir, err := b.keysDir()
	if err != nil {
		return SSHKeyRef{}, err
	}

	id := uuid.NewString()
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return SSHKeyRef{}, fmt.Errorf("service: persisting imported key: %w", err)
	}

	return SSHKeyRef{ID: id, KeyFile: path, Algorithm: algorithm, Fingerprint: fingerprint}, nil
}

// HostKeyProbe is the plain shape of rclone.HostKeyProbeResult this
// package exposes to a caller outside core/.
type HostKeyProbe struct {
	Algorithm      string
	Fingerprint    string
	KnownHostsLine string
}

// ProbeHostKey fetches host:port's current SSH host key, without
// trusting or persisting anything — see
// internal/transport/rclone.ProbeHostKey's doc for the full contract.
// This is read-only (docs/EPIC-B-multi-nas.md §50 lists "probe host key"
// under read-only/low-risk actions): the wizard's "Verify server" step
// calls this to show a real fingerprint, and a later CreateBackupSet call
// carries the resulting KnownHostsLine forward as the actual trust
// anchor once an operator has confirmed it.
func (b *BackupService) ProbeHostKey(ctx context.Context, host string, port int) (HostKeyProbe, error) {
	res, err := rclone.ProbeHostKey(ctx, host, port)
	if err != nil {
		return HostKeyProbe{}, err
	}
	return HostKeyProbe{Algorithm: res.Algorithm, Fingerprint: res.Fingerprint, KnownHostsLine: res.KnownHostsLine}, nil
}

// ConnectionTestRequest is what TestConnection checks: the same
// Host/Port/User/SSHKeyID/KnownHostsLine/RemotePath a CreateBackupSetRequest
// would carry, checked BEFORE anything is persisted.
type ConnectionTestRequest struct {
	Host           string
	Port           int
	User           string
	SSHKeyID       string
	KnownHostsLine string
	RemotePath     string
}

// ConnectionTestResult reports whether TestConnection could actually
// reach and authenticate against the configured source. Message is set
// only when OK is false, and is always one of this package's own
// sanitized strings (see TestConnection) — never a raw rclone/x/crypto
// error, which could otherwise embed a path or a stack-shaped string
// this API's callers must never have to treat as untrusted-safe-to-render
// by accident.
type ConnectionTestResult struct {
	OK      bool
	Message string
}

// TestConnection performs a real, non-destructive reachability/auth
// check against a candidate source: it lists RemotePath (defaulting to
// "/") over the real sftp transport this BackupService already uses for
// production transfers (internal/transport.Transport.List), the same
// call a real RunCycle's discovery step makes, just discarding the
// result. Nothing is written to the remote; nothing from this call is
// persisted locally either — KnownHostsLine is written to a temporary
// file for the duration of this one check and removed before returning
// (see the wizard's pre-save "Verify server"/review flow, where no trust
// decision is final until CreateBackupSet actually runs).
func (b *BackupService) TestConnection(ctx context.Context, req ConnectionTestRequest) (ConnectionTestResult, error) {
	if req.Host == "" || req.User == "" || req.SSHKeyID == "" || req.KnownHostsLine == "" {
		return ConnectionTestResult{}, fmt.Errorf("%w: host, user, ssh_key_id and known_hosts_line are all required", ErrInvalidRequest)
	}

	keyFile, err := b.resolveSSHKeyFile(req.SSHKeyID)
	if err != nil {
		return ConnectionTestResult{}, err
	}

	tmp, err := os.CreateTemp("", "backup-manager-test-connection-known-hosts-*")
	if err != nil {
		return ConnectionTestResult{}, fmt.Errorf("service: preparing connection test: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(req.KnownHostsLine + "\n"); err != nil {
		tmp.Close()
		return ConnectionTestResult{}, fmt.Errorf("service: preparing connection test: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return ConnectionTestResult{}, fmt.Errorf("service: preparing connection test: %w", err)
	}

	root := req.RemotePath
	if root == "" {
		root = "/"
	}

	testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	src := transport.Source{
		ID:         "connection-test",
		Type:       "sftp",
		Host:       req.Host,
		Port:       req.Port,
		User:       req.User,
		KeyFile:    keyFile,
		KnownHosts: tmpPath,
		Root:       root,
	}

	if _, err := b.state.Load().inner.Transport.List(testCtx, src); err != nil {
		// Not %w-wrapped, and not returned as a Go error at all: a failed
		// connection test is an expected, ordinary OUTCOME (a typo'd
		// hostname, a not-yet-authorized key), not a service failure, so
		// it is reported through ConnectionTestResult.OK/Message, exactly
		// like every other "this is what an operator did wrong, not what
		// broke" case in this package. err's own text may embed rclone
		// internals (a dial error, an sftp protocol string), so it is
		// deliberately not put in Message: TestConnection's caller (the
		// HTTP layer) gets a generic, safe-to-render reason instead.
		return ConnectionTestResult{OK: false, Message: "could not connect and list the remote path"}, nil
	}

	return ConnectionTestResult{OK: true}, nil
}

package service

import (
	"context"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// connectionTestTimeout bounds one reachability check. It is the same
// ten seconds TestConnection uses for a candidate source: a test that can
// hang indefinitely is a request an operator cannot cancel.
const connectionTestTimeout = 10 * time.Second

// SetBackupSetEnabled turns one configured backup set on or off, persists
// the change to the configuration file this BackupService was opened from,
// and hot-reloads so it takes effect immediately.
//
// # What "disabled" means, and what it does not
//
// A disabled backup set is excluded from every run cycle: nothing is
// discovered, transferred, verified, committed or retained for it while it
// stays off. Nothing already backed up is touched. Turning a set off
// deletes no artifact, releases no remote source, and does not run
// retention; turning it back on resumes the ordinary pipeline from
// whatever the journal already holds. That is why this is a
// state-changing but NON-destructive operation in
// docs/EPIC-B-multi-nas.md §50's terms, in the same bucket as
// create-backup-set, and why the API layer wraps it in CSRF protection but
// not the destructive-operations gate.
//
// It is worth being explicit about the direction that sounds dangerous:
// turning a set OFF stops new restore points being made, which degrades
// freshness over time, and FR-24's health computation reports that
// honestly as the set goes stale. It is not hidden, and it is reversible
// by the same call.
//
// # Persist, then reload
//
// This follows CreateBackupSet's sequence exactly, for the same reasons
// recorded there: re-read the file fresh rather than trusting the running
// in-memory copy, encode the bytes BEFORE config.Validate resolves
// defaults in place (so an unrelated toggle does not freeze this
// release's defaults into the operator's file), resolve the validator
// catalog before the write so the only step after it cannot fail, then
// one atomic state.Store so no concurrent reader ever sees a torn
// {inner, revision} pair.
func (b *BackupService) SetBackupSetEnabled(_ context.Context, id string, enabled bool) (BackupSet, error) {
	if b.configPath == "" {
		return BackupSet{}, ErrConfigNotFileBacked
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	found := false
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name != setName {
				continue
			}
			cfg.Sources[i].BackupSets[j].Disabled = !enabled
			found = true
		}
	}
	if !found {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	// Encoded before Validate, which resolves defaults in place; see
	// UpdateSettings' own comment for the full reasoning and for what an
	// unrelated edit would otherwise silently freeze into the file.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: encoding configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return BackupSet{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return BackupSet{}, err
	}

	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		return BackupSet{}, fmt.Errorf("service: persisting configuration: %w", err)
	}

	applyValidators()

	b.adoptConfig(cfg)

	return toServiceBackupSet(sourceName, findBackupSet(cfg, sourceName, setName)), nil
}

// SetBackupSetReadOnly turns issue #282's read-only declaration on or off
// for one already-persisted backup set, through the API/wizard rather
// than by hand-editing config.yaml (issue #316). It persists the change
// to the configuration file this BackupService was opened from and
// hot-reloads, following exactly the sequence SetBackupSetEnabled above
// documents in full and for the same reasons.
//
// # What this always sets, and what it never touches
//
// This always writes this ONE backup set's own explicit override
// (config.BackupSet.ReadOnlyConfig), never its source's ReadOnly default:
// an API caller names one backup set, and there is no "every set under
// this source" concept anywhere in the CRUD surface for it to mean
// instead. A source-level default set by hand in config.yaml is left
// exactly as it is; this only ever adds or changes THIS set's own
// override on top of it, the same way a hand-edited per-set `read_only:`
// line would.
//
// # What "read-only" means, and what turning it off does not do
//
// See config.BackupSet.ReadOnly's own doc for the full contract: while
// true, FR-15's delete step is structurally never reached for this set
// (core/internal/app's pipeline routes it to lifecycle.RetainRemote
// instead of lifecycle.DeleteRemote), and an artifact that already
// reached REMOTE_RETAINED under it stays retained — turning this back off
// does not reach back and make an already-retained artifact eligible for
// deletion again, the same one-way-per-artifact shape #227's
// reinstatement already established for this codebase. It only changes
// what happens to artifacts THIS backup set commits from here on.
func (b *BackupService) SetBackupSetReadOnly(_ context.Context, id string, readOnly bool) (BackupSet, error) {
	if b.configPath == "" {
		return BackupSet{}, ErrConfigNotFileBacked
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	found := false
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name != setName {
				continue
			}
			// A pointer to a fresh local, per iteration: reusing one
			// variable's address across the loop (or across calls) would
			// have every backup set's ReadOnlyConfig alias the same bool,
			// silently rewriting an earlier match's answer whenever a
			// later one is set.
			ro := readOnly
			cfg.Sources[i].BackupSets[j].ReadOnlyConfig = &ro
			found = true
		}
	}
	if !found {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	// Encoded before Validate, which resolves defaults in place; see
	// SetBackupSetEnabled's own comment above for the full reasoning.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: encoding configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return BackupSet{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return BackupSet{}, err
	}

	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		return BackupSet{}, fmt.Errorf("service: persisting configuration: %w", err)
	}

	applyValidators()

	b.adoptConfig(cfg)

	return toServiceBackupSet(sourceName, findBackupSet(cfg, sourceName, setName)), nil
}

// TestBackupSetConnection runs the same non-destructive reachability and
// authentication check TestConnection performs, against an ALREADY
// PERSISTED backup set rather than a candidate a caller is still filling
// in.
//
// The two share one route (POST /api/v1/backup-sets/test-connection) and
// differ only in where the connection details come from: a candidate
// carries its own, and a persisted set has them in the configuration
// already. That matters for more than tidiness. A client asking to test
// set "nas-a/photos" does not know, and must never have to send back, the
// key reference and known-hosts line that set is configured with; making
// it echo them would turn a read-only "does this still work" button into
// a request that could quietly test something else.
//
// Everything reachable from here is read-only: it lists the configured
// remote path over the transport this service already uses and discards
// the result. Nothing is written locally or remotely, and no trust
// decision is made or revised.
func (b *BackupService) TestBackupSetConnection(ctx context.Context, id string) (ConnectionTestResult, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return ConnectionTestResult{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	st := b.state.Load()
	var found *config.BackupSet
	for _, src := range st.inner.Config.Sources {
		if src.Name != sourceName {
			continue
		}
		for i := range src.BackupSets {
			if src.BackupSets[i].Name == setName {
				found = &src.BackupSets[i]
			}
		}
	}
	if found == nil {
		return ConnectionTestResult{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	root := found.RemotePath
	if root == "" {
		root = "/"
	}

	testCtx, cancel := context.WithTimeout(ctx, connectionTestTimeout)
	defer cancel()

	r := found.Remote
	src := transport.Source{
		ID:         "connection-test",
		Type:       r.Type,
		Host:       r.Host,
		Port:       r.Port,
		User:       r.User,
		KeyFile:    r.Key.File,
		KeyEnv:     r.Key.Env,
		KeyCommand: r.Key.Command,
		// The passphrase's own three sources travel too. Without them a
		// set whose key is passphrase-protected cannot be tested at all:
		// the adapter is handed a key it has no way to open, and the
		// operator is told their host is unreachable.
		PassphraseFile:    r.Key.Passphrase.File,
		PassphraseEnv:     r.Key.Passphrase.Env,
		PassphraseCommand: r.Key.Passphrase.Command,
		// #355: this is the operator's REAL configured remote, so the
		// ceiling they set on it has to come with it. A reachability
		// check that runs uncapped against the one host they capped can
		// fail where a cycle succeeds, or pass where a cycle fails, which
		// makes the button worse than not having one.
		MaxConnections:       r.MaxConnections,
		KeyEncryptionFile:    st.inner.Config.KeyEncryption.File,
		KeyEncryptionEnv:     st.inner.Config.KeyEncryption.Env,
		KeyEncryptionCommand: st.inner.Config.KeyEncryption.Command,
		KnownHosts:           r.KnownHosts,
		Root:                 root,
	}

	if _, err := st.inner.Transport.List(testCtx, src); err != nil {
		// Deliberately not %w-wrapped and deliberately not put in
		// Message, for exactly the reason TestConnection gives: a failed
		// connection test is an ordinary outcome, and err's own text can
		// embed transport internals a caller must not have to treat as
		// safe to render.
		return ConnectionTestResult{OK: false, Message: "could not connect and list the remote path"}, nil
	}
	return ConnectionTestResult{OK: true}, nil
}

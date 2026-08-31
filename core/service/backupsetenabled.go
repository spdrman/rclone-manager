package service

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

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

	prevInner := b.state.Load().inner
	newInner := app.New(cfg, b.journal, prevInner.Transport, b.logger)
	if !newInner.AdoptAlerts(prevInner.Alerts) && b.alertSink != nil {
		newInner.EnableAlerts(sinkAdapter{sink: b.alertSink})
	}
	b.state.Store(&configState{inner: newInner, revision: computeConfigRevision(cfg)})

	return toServiceBackupSet(sourceName, findBackupSet(cfg, sourceName, setName)), nil
}

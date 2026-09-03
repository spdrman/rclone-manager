// This file is issue #350's update path: the half of backup-set CRUD that
// #146 never built. Until it existed a backup set could be created,
// enabled, disabled, declared read-only and read back, and after that the
// only way to change what it actually backs up was to open config.yaml in
// an editor on the NAS itself. The Web UI's Edit button opened a form and
// then told the operator, honestly, that nothing had been saved.
//
// # Why this is a sparse update, and what that structurally buys
//
// Every field on UpdateBackupSetRequest is a pointer, and nil means
// "leave this alone" rather than "set it to the zero value". That is not
// a convenience: it is what makes the issue's "a per-box Save writes only
// that box" true at the layer that persists, rather than a promise the UI
// makes and could quietly break. A request that names one field cannot
// move another, whatever the caller intended, so an operator who changed
// two things and saved one has not silently shipped both.
//
// # One mechanism, not a parallel one
//
// The persist-then-reload sequence here is CreateBackupSet's, step for
// step, and deliberately so: re-read the file fresh rather than trusting
// the in-memory copy, encode the bytes BEFORE config.Validate resolves
// defaults in place, resolve the validator catalog before the write so
// the only step after it cannot fail, write through
// writeConfigBytesAtomically (temp file in the same directory, fsync,
// rename, fsync the directory), then one atomic state.Store so no
// concurrent reader ever sees a torn {inner, revision} pair. See
// CreateBackupSet's own doc and backupsetenabled.go for the full
// reasoning each of those steps carries; nothing here restates it, and
// nothing here reimplements it.
//
// # What is deliberately not editable
//
// Identity: SourceName and Name. A backup set's id is what every journal
// row, every artifact id (model.NewArtifactID is source/set/name), every
// recovery manifest on disk and every retained local directory is keyed
// by. Renaming one through this method would leave every artifact it has
// ever produced pointing at a set that no longer exists, which is a
// migration with its own design, not a field on an edit form. The Web UI
// shows the name as a fixed heading beside the editable fields for that
// reason, rather than an input that would refuse on save.
//
// Also not here: the SSH key reference and the trusted host-key line.
// Those are not values an operator types, they are the results of the
// import and probe steps the wizard already owns (ImportSSHKey,
// ProbeHostKey), and rotating either is a trust decision rather than an
// edit. CreateBackupSetRequest carries a reference to each rather than
// the material itself for the same reason; an edit path that accepted a
// raw known_hosts line would be a way to re-trust a host without ever
// being shown a fingerprint.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// UpdateBackupSetRequest is a sparse edit of one already-persisted backup
// set. Every field is a pointer: nil leaves that setting exactly as it is
// on disk, and a non-nil pointer replaces it. See this file's own doc for
// why that shape is load-bearing rather than stylistic, and for what is
// deliberately absent from it.
type UpdateBackupSetRequest struct {
	// Host, Port and User are the sftp remote's connection details
	// (config.Remote). Port may legitimately be set to 0, which selects
	// the default port, which is exactly why these are pointers and not
	// "zero means unset".
	Host *string
	Port *int
	User *string

	RemotePath *string
	LocalPath  *string
	Include    *[]string

	// CompletionStrategy is "rename", "marker" or "stable"
	// (config.Completion.Strategy; FR-8).
	CompletionStrategy *string
	// StableFor is required, and only meaningful, when the strategy in
	// effect after this update is "stable". Moving off "stable" clears
	// whatever value was there, so the file never keeps a number nothing
	// reads.
	StableFor *time.Duration

	StaleAfter *time.Duration

	// ValidatorID selects this backup set's FR-13 application validator
	// from the registered catalog, or is "" for none. An id, never a
	// path, and refused unless the catalog lists it, exactly as on the
	// create path (validator.go, docs/EPIC-B-multi-nas.md §26 Step 5).
	ValidatorID *ValidatorID

	// AcknowledgeRepoint confirms that the caller means to move this set
	// to different data. It is not a field of the backup set and nothing
	// persists it: it answers one refusal, for one request.
	//
	// It is required only when the request actually changes remote.host,
	// remote_path or local_path on a set that already has artifacts on
	// record, and backupsetrepoint.go has the whole argument for why
	// those three and why an acknowledgement rather than a refusal or a
	// warning. Deliberately NOT a pointer like everything above it: this
	// is not a sparse edit of a stored value, it is a yes/no about this
	// one call, and false is the honest default for a caller that did not
	// mention it.
	AcknowledgeRepoint bool
}

// isEmpty reports whether this request names nothing at all. An update
// that changes nothing is refused rather than persisted: it would rewrite
// the configuration file and hot-reload the whole service to achieve
// exactly nothing, and it would let a client send {} and read the 200
// back as though something had happened.
func (r UpdateBackupSetRequest) isEmpty() bool {
	// AcknowledgeRepoint is deliberately not counted: it names no field
	// to change, so a request carrying only it changes nothing and is
	// refused exactly like an empty one.
	return r.Host == nil && r.Port == nil && r.User == nil &&
		r.RemotePath == nil && r.LocalPath == nil && r.Include == nil &&
		r.CompletionStrategy == nil && r.StableFor == nil &&
		r.StaleAfter == nil && r.ValidatorID == nil
}

// UpdateBackupSet applies req to the backup set named by id ("source/name"),
// persists the result into the configuration file this BackupService was
// opened from, and hot-reloads so the change is live immediately. See this
// file's package doc for the sequence and for what it deliberately reuses.
//
// It is state-changing but NOT destructive, in docs/EPIC-B-multi-nas.md
// §50's terms: §50 puts "create/edit backup set" in one bucket, and
// nothing reachable from here touches, moves or deletes a byte of backup
// data. The API layer therefore wraps the route in requireCSRF and not
// requireDestructiveGate, following POST /api/v1/backup-sets' own
// precedent rather than the gate's.
func (b *BackupService) UpdateBackupSet(ctx context.Context, id string, req UpdateBackupSetRequest) (BackupSet, error) {
	if b.configPath == "" {
		return BackupSet{}, ErrConfigNotFileBacked
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}
	if req.isEmpty() {
		return BackupSet{}, fmt.Errorf("%w: an update must name at least one field to change", ErrInvalidRequest)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	// Re-read from disk rather than trusting b.state: the same "always
	// read fresh" discipline CreateBackupSet documents, and the same
	// reason. It is also what makes this method safe against a config.yaml
	// hand-edited since this process last loaded it, which for an edit
	// path is not a hypothetical: hand-editing is what operators have had
	// to do until now.
	cfg, err := config.Load(b.configPath)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	target := findBackupSetPointer(cfg, sourceName, setName)
	if target == nil {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	// Refused before anything is applied, for the same reason validation
	// is: a refusal must leave the file and this process's configuration
	// exactly as they were. See backupsetrepoint.go.
	if err := b.requireRepointAcknowledgement(ctx, sourceName, setName, *target, req); err != nil {
		return BackupSet{}, err
	}

	// Applied to a copy, and validated, before the real one is touched:
	// a refused update must leave both the file and this process's
	// in-memory configuration exactly as they were, which the tests in
	// backupsetupdate_test.go check by comparing the file byte for byte.
	edited := applyBackupSetUpdate(*target, req)
	if err := validateUpdatedBackupSet(edited, req); err != nil {
		return BackupSet{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	*target = edited

	// Encoded before cfg.Validate, which resolves Retention and Alerts in
	// place; see UpdateSettings' and CreateBackupSet's own comments for
	// what encoding afterwards would silently freeze into an operator's
	// file.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: encoding configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		// Safe to echo: config.ValidationError's text is built from this
		// package's own field descriptions and the caller's own values,
		// never from an internal/state or rclone error string.
		return BackupSet{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// Everything fallible about validator resolution happens before the
	// write, for the reason CreateBackupSet records at length: a failure
	// after writeConfigBytesAtomically would report an error for a change
	// that is already durably on disk.
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

// findBackupSetPointer returns a pointer INTO cfg for the named backup
// set, so a caller can edit it in place, or nil when there is none.
// findBackupSet (backupsets.go) beside it returns a copy, which is the
// right answer for reading one back after a write and the wrong one for
// changing it.
func findBackupSetPointer(cfg *config.Config, sourceName, setName string) *config.BackupSet {
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name == setName {
				return &cfg.Sources[i].BackupSets[j]
			}
		}
	}
	return nil
}

// applyBackupSetUpdate returns bs with every field req names replaced and
// every field it does not left exactly as it was. It is a pure function
// over a copy on purpose: the caller validates the result before it
// writes it back, so a refused update never leaves a partially-applied
// set behind.
func applyBackupSetUpdate(bs config.BackupSet, req UpdateBackupSetRequest) config.BackupSet {
	if req.Host != nil {
		bs.Remote.Host = *req.Host
	}
	if req.Port != nil {
		bs.Remote.Port = *req.Port
	}
	if req.User != nil {
		bs.Remote.User = *req.User
	}
	if req.RemotePath != nil {
		bs.RemotePath = *req.RemotePath
	}
	if req.LocalPath != nil {
		bs.LocalPath = *req.LocalPath
	}
	if req.Include != nil {
		// Copied, never aliased: bs is about to be written into the
		// caller's config, and letting it share a backing array with the
		// caller's request would make a later append by either one visible
		// to the other.
		bs.Include = append([]string(nil), (*req.Include)...)
	}
	if req.CompletionStrategy != nil {
		bs.Completion.Strategy = *req.CompletionStrategy
	}
	if req.StableFor != nil {
		bs.Completion.StableFor = config.Duration(*req.StableFor)
	}
	// stable_for only means anything under the "stable" strategy
	// (config.Completion). Creation never writes one for any other
	// strategy (newBackupSetFor), so an edit that moves off "stable"
	// clears it rather than leaving a number in the operator's file that
	// nothing reads and the next reader has to work out is dead.
	if bs.Completion.Strategy != "stable" {
		bs.Completion.StableFor = 0
	}
	if req.StaleAfter != nil {
		bs.StaleAfter = config.Duration(*req.StaleAfter)
	}
	if req.ValidatorID != nil {
		bs.Validation.ValidatorID = string(*req.ValidatorID)
		// The resolved config.Command is deliberately NOT set here, for
		// the reason newBackupSetFor records: the resolved path is this
		// deployment's own materialized script directory, and a
		// config.yaml holding a stale copy of it fails every artifact in
		// the set after the next restart. planValidatorCatalog fills the
		// in-memory copy in after the write.
		bs.Validation.Command = nil
	}
	return bs
}

// validateUpdatedBackupSet applies, to the result of an edit, exactly the
// field rules validateCreateRequest applies to a creation. It shares the
// actual checks with that function rather than restating them (see
// backupsets.go's hostProblem/completionProblems and friends), which is
// what makes the issue's "validation equal to creation's" a structural
// property instead of two lists that agree today.
//
// It checks the RESULT of the edit, not the request, for the fields where
// those differ: setting the strategy to "stable" on a set that has no
// stable_for is refused even though the request named only the strategy,
// because the configuration it would leave behind is the one creation
// would have refused.
//
// req is passed alongside so a rule can distinguish "the caller said
// this" from "this is what was already on disk". host and user are the
// case that matters: a `remote.type: local` set legitimately has neither,
// and refusing every update to such a set because a field it never had is
// empty would make this method unusable for exactly the fixtures this
// repository tests with. So an empty host is refused when the caller sent
// one, and left to config.Validate otherwise.
func validateUpdatedBackupSet(bs config.BackupSet, req UpdateBackupSetRequest) error {
	var problems []string
	if req.Host != nil {
		problems = appendProblem(problems, requiredFieldProblem("host", bs.Remote.Host))
	}
	if req.User != nil {
		problems = appendProblem(problems, requiredFieldProblem("user", bs.Remote.User))
	}
	if req.RemotePath != nil {
		problems = appendProblem(problems, requiredFieldProblem("remote_path", bs.RemotePath))
	}
	if req.LocalPath != nil {
		problems = appendProblem(problems, requiredFieldProblem("local_path", bs.LocalPath))
	}
	problems = append(problems, completionProblems(bs.Completion.Strategy, bs.Completion.StableFor.Duration())...)
	if req.StaleAfter != nil && bs.StaleAfter.Duration() <= 0 {
		problems = append(problems, "stale_after must be a positive duration")
	}
	problems = appendProblem(problems, validatorIDProblem(ValidatorID(bs.Validation.ValidatorID)))
	return joinProblems(problems)
}

func appendProblem(problems []string, p string) []string {
	if p == "" {
		return problems
	}
	return append(problems, p)
}

func joinProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	msg := problems[0]
	for _, p := range problems[1:] {
		msg += "; " + p
	}
	return errors.New(msg)
}

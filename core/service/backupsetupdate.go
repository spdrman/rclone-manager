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
// What IS here since issue #572, and used not to be: the SSH key
// reference and the trusted host-key line. Leaving them out was defensible
// as a scope line and wrong as a product: a key replaced on the source
// host, or a server rebuilt with a new host key, left an operator with no
// route but to remove the backup set and create it again, and the one
// field whose whole purpose is to be updated when something changes was
// the one that could not be. Both are still references produced by
// ImportSSHKey and ProbeHostKey rather than material typed here, so
// nothing on this path ever holds key bytes. Re-trusting a host is still a
// trust decision, and backupsethostkey.go is where that is asked about
// rather than assumed.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

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

	// SSHKeyID replaces the key this backup set authenticates with, by
	// the id an earlier ImportSSHKey call returned (issue #572). A
	// reference, never key material, exactly as on the create path: a
	// caller outside core/ never learns the server-side path it resolves
	// to, and an id no import produced is refused with ErrSSHKeyNotFound
	// before anything is written.
	SSHKeyID *string

	// KnownHostsLine replaces the host key this backup set trusts, as the
	// exact known_hosts line an earlier ProbeHostKey call returned. It is
	// the one field on this request that can be refused for what it MEANS
	// rather than for how it is spelled; see backupsethostkey.go for the
	// whole argument, and AcknowledgeHostKeyChange below for the way out.
	KnownHostsLine *string

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

	// AcknowledgeHostKeyChange confirms that the caller means to change
	// what host keys this backup set trusts for its own address. Required
	// when KnownHostsLine pins a different key from the one on record, and
	// equally when it would stop pinning one that IS on record (a host
	// answering with two key algorithms has a line each, and this field
	// carries one). Re-sending the line already trusted, or trusting a key
	// for a HOST this set has nothing pinned for, asks nothing.
	//
	// A second flag rather than a second meaning for AcknowledgeRepoint,
	// for the reason backupsethostkey.go states: the two answer different
	// questions, and one flag for both would let an operator moving a set
	// to a new path grant a re-trust they never looked at. Not a pointer,
	// for the same reason AcknowledgeRepoint is not.
	AcknowledgeHostKeyChange bool
}

// isEmpty reports whether this request names nothing at all. An update
// that changes nothing is refused rather than persisted: it would rewrite
// the configuration file and hot-reload the whole service to achieve
// exactly nothing, and it would let a client send {} and read the 200
// back as though something had happened.
func (r UpdateBackupSetRequest) isEmpty() bool {
	// Neither acknowledgement is counted: each names no field to change,
	// so a request carrying only one changes nothing and is refused
	// exactly like an empty one.
	return r.Host == nil && r.Port == nil && r.User == nil &&
		r.RemotePath == nil && r.LocalPath == nil && r.Include == nil &&
		r.CompletionStrategy == nil && r.StableFor == nil &&
		r.StaleAfter == nil && r.ValidatorID == nil &&
		r.SSHKeyID == nil && r.KnownHostsLine == nil
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
//
// # Why re-trusting a host key did not move it into the gate
//
// Issue #572 made this route able to change a backup set's trust anchor,
// which is the most consequential thing it can now do, so the bucket was
// looked at again rather than inherited. It stays where it is, for a
// reason that is about what the gate IS rather than about how serious a
// re-trust is.
//
// requireDestructiveGate is not a per-request confirmation. It is one
// deployment-wide switch that reports whether the trusted-proxy
// authentication gate (issue #92) has been verified for this deployment,
// and it is open on every deployment that has been through setup. Putting
// this route behind it would refuse edits on a deployment that has not,
// while POST /api/v1/backup-sets stayed open on the same deployment, and
// POST takes an arbitrary known_hosts_line for a brand new set with no
// acknowledgement at all, because there is nothing on record to compare
// it against. Anyone who can reach the PATCH can reach the POST. So the
// gate would cost an operator their edit form without taking anything
// away from a caller that meant harm, which is a check that reads like
// protection and is not.
//
// What actually stands in front of a re-trust is in backupsethostkey.go:
// the request has to name the fingerprint's own acknowledgement, and the
// refusal that asks for it names both keys. That is a decision about this
// one edit, made by whoever is making it, which is the thing the gate
// cannot be.
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

	// The key reference resolves to a real file here rather than in
	// applyBackupSetUpdate, because it can fail and that function is a
	// pure edit of a copy. Resolved before the trust question below and
	// well before any write, so a rotation naming an id nothing imported
	// costs the caller an attempt and not a set (issue #572).
	//
	// The whole Key is replaced rather than only its File, and KeyFile,
	// the deprecated alias, is cleared with it. config.Validate refuses a
	// remote naming two key sources, so an edit that set File beside an
	// existing key.env would leave a configuration this process could not
	// reload. A passphrase configured for the OLD key goes with it for the
	// same reason it would not work: it is the passphrase for a key this
	// set no longer uses.
	if req.SSHKeyID != nil {
		keyFile, err := b.resolveSSHKeyFile(*req.SSHKeyID)
		if err != nil {
			return BackupSet{}, err
		}
		edited.Remote.Key = config.Key{File: keyFile}
		edited.Remote.KeyFile = ""
	}

	// The trust questions, and then the new line written under a name of
	// its own. Every refusal happens before the configuration is touched,
	// for the same reason the repoint refusal above does: a refusal must
	// leave both the configuration and the trusted line exactly as they
	// were, and prepareTrustChange removes what it staged before it
	// returns one.
	var trust *stagedKnownHosts
	if req.KnownHostsLine != nil {
		staged, err := prepareTrustChange(b.configPath, sourceName, setName, *target, edited, req)
		if err != nil {
			return BackupSet{}, err
		}
		defer staged.discard()
		edited.Remote.KnownHosts = staged.Path
		trust = staged
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

	// Nothing below this line can fail, and that is the invariant rather
	// than a happy accident. The trusted line is already written, already
	// fsynced and already named by the configuration that just landed, so
	// keeping it is a flag. See stagedKnownHosts for what the version that
	// renamed a file here used to do to a caller when the rename lost.
	if trust != nil {
		trust.commit()
	}

	applyValidators()

	b.adoptConfig(cfg)

	return toServiceBackupSet(b.configPath, sourceName, findBackupSet(cfg, sourceName, setName)), nil
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
//
// SSHKeyID and KnownHostsLine are deliberately not applied here even
// though they are fields of the same request. Both resolve to something
// on this deployment's own filesystem, both can fail doing it, and one of
// them writes a file; none of that belongs in a pure function over a copy.
// UpdateBackupSet applies them itself, in the order its own comments give.
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
	//
	// This clearing is why validateUpdatedBackupSet refuses a request that
	// NAMES a window this line would then throw away. The two belong
	// together: without the refusal, the honest bookkeeping here becomes a
	// 200 for a value the caller watched vanish.
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
	// The key and the trusted line only mean anything for an sftp remote:
	// a local-transport set has no host to trust and nothing to
	// authenticate to, and config.Validate refuses one that carries
	// either. Said here rather than left to that refusal so the message
	// names what the caller actually asked for.
	if req.SSHKeyID != nil || req.KnownHostsLine != nil {
		if bs.Remote.Type != "sftp" {
			problems = append(problems, fmt.Sprintf(
				"ssh_key_id and known_hosts_line only mean something for a remote of type \"sftp\", and this backup set's remote is type %q",
				bs.Remote.Type))
		}
	}
	if req.SSHKeyID != nil && *req.SSHKeyID == "" {
		problems = append(problems, "ssh_key_id must not be empty (import a key first, or omit the field to leave the key alone)")
	}
	if req.KnownHostsLine != nil {
		problems = appendProblem(problems, knownHostsLineProblem(*req.KnownHostsLine))
	}
	problems = append(problems, completionProblems(bs.Completion.Strategy, bs.Completion.StableFor.Duration())...)
	// A window the resulting configuration would not keep is refused
	// rather than quietly dropped.
	//
	// applyBackupSetUpdate clears stable_for whenever the strategy in
	// effect is not "stable", which is right: a dead number in an
	// operator's file is worse than none. What was wrong was answering the
	// caller 200 for it. A request naming stable_for on a set that stays
	// on "rename" was accepted, cleared, and reported as a success, so the
	// Web UI, which re-reads the server's own answer, showed the operator
	// the 45 they had typed coming back as 0 with nothing to explain it. A
	// success for a discarded write is the kind of answer other things get
	// built on.
	//
	// It is written as "what you sent is not what the file would hold"
	// rather than as "you may not send stable_for off the stable
	// strategy", and the difference is deliberate. The second is a rule
	// about a field being present, and it would refuse a caller who moves
	// a set off "stable" and spells out that the window goes to zero,
	// which is a request that gets exactly what it asked for. Comparing
	// against the edited set also means this cannot drift from the
	// clearing rule above: whatever that line decides, this compares the
	// caller's value against its result.
	if req.StableFor != nil && bs.Completion.StableFor.Duration() != *req.StableFor {
		problems = append(problems, fmt.Sprintf(
			"stable_for %s would not be kept: it is only stored under the \"stable\" completion strategy, and this edit leaves completion_strategy as %q. Send completion_strategy \"stable\" in the same edit, or leave stable_for out of it",
			*req.StableFor, bs.Completion.Strategy))
	}
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

// joinProblems turns the collected field problems into one error, or nil
// when there are none.
//
// One error rather than a typed list of them, because the only consumer
// is a caller that renders a message, and a structure nobody destructures
// is a shape to keep in step for nothing. Everything joined in here is a
// string this package wrote, which is the property ErrInvalidRequest's
// own doc leans on when it says the message is safe to echo back.
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

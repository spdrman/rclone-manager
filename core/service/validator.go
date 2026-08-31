// This file is WP3.2's registered-validator catalog (docs/EPIC-B-multi-
// nas.md §71 Work Package 3.2, §26 Step 5: "Do not permit arbitrary shell
// commands from the browser").
//
// internal/config.Validation.Command (internal/lifecycle/verify.go's
// FR-13 application-validator step) already lets the trusted CLI/YAML
// config path name any absolute executable path it wants, and that stays
// exactly as-is: this file changes nothing about it, and does not add a
// second validation engine. What it adds is the one thing that path never
// had to have and the API/UI layer must never be handed anyway: a way to
// select a validator by a fixed, code-defined identifier instead of by
// naming an executable at all. resolveValidator is the only function in
// this package that turns a ValidatorID into an internal/config.Command,
// and it can only ever return one of the catalog entries this file itself
// defines, never anything built from caller input. A caller that hands it
// a raw path, or any other string outside RegisteredValidators, gets
// errUnregisteredValidator, exactly like a typo would: this package does
// not special-case "that looks like a path" detection, because it does
// not need to -- an unrecognized identifier is refused structurally,
// regardless of what it happens to look like.
//
// # What crosses this package's boundary, and what does not
//
// resolveValidator is deliberately unexported. This package's own doc
// promises it exposes "only plain, provider-agnostic types and functions,
// never a config.Config, a state.Record, or anything else an internal
// package owns", and a config.Command is exactly that. Exporting it also
// made it useless to the layer it exists for: apps/common/webhost is a
// separate module and cannot name config.Command, so it could not have
// stored the return value, put it in a request struct, or passed it
// anywhere typed even if it wanted to.
//
// So ValidatorID, Validator and RegisteredValidators are the whole
// exported surface: an id, a human summary to render beside it, and the
// list of both. Issue #162 wired the selection those exist for -- a
// ValidatorID field on CreateBackupSetRequest, resolved inside
// CreateBackupSet, persisted as an id in config.yaml, resolved back to a
// Command at load time, and served to the wizard's step 5 through
// apps/common/webhost's GET /api/v1/validators -- and the resolution
// stayed on this side of the boundary throughout. Only the id crosses it,
// in either direction.
//
// # Where the scripts live, and why not TMPDIR
//
// internal/lifecycle/verify.go's runValidator invokes Command.Executable
// as a plain argv[0], never through a shell, so a registered validator
// has to be a real file on disk with its own shebang and execute bit.
// materializeValidators writes the embedded scripts into a "validators"
// directory beside the state database (cfg.State.Database, already
// established as writable by §46.1's startup sequence), NOT into
// os.MkdirTemp:
//
//   - /tmp is mounted noexec on plenty of the NAS platforms this ships
//     to, which would make every registered validator unrunnable with an
//     error pointing at a temp path rather than at the cause;
//   - a temp directory is per-process, so nothing would ever remove the
//     one left behind by a process that exited;
//   - and /tmp is swept. The state directory is this deployment's own
//     data directory: nothing else prunes it.
//
// Every resolve re-checks the scripts and rewrites any that is missing or
// has drifted from the embedded copy, rather than materializing once and
// trusting the result forever. Nothing latches: a materialization failure
// is returned, not cached, so one transient full or read-only filesystem
// does not disable every validator for the rest of the process's life.
//
// That re-check only repairs anything if it runs at the moment the script
// is about to be used, so BackupService re-materializes at the start of
// every run cycle (refreshValidatorScripts, called by executeRunCycle and
// by the scheduler's tick) as well as at load and create time. Both
// directions of "the file on disk is not the embedded script" matter, and
// they fail in opposite ways: a reaped script is fail-closed and merely
// wedges the backup set, while a script that was replaced or truncated is
// fail-OPEN -- a two-line "exit 0" passes every artifact, and passing is
// exactly what authorizes deleting the remote source. A per-cycle rewrite
// is what closes that, and a failure to rewrite refuses the cycle rather
// than running it with whatever is on disk.
//
// Nothing removes the directory. It is deployment state, shared by every
// process that opens this state database (the journal lock is a SHARED
// one, and a container restart racing an old process's shutdown against a
// new one's start is a supported case), so a process that deleted it on
// its way out would be deleting a running successor's resolved scripts.
// The scripts are rewritten from the embedded copies whenever they are
// needed, and what is left behind is one directory per deployment rather
// than one per process start, which is what the old os.MkdirTemp
// implementation leaked.
//
// # Casing
//
// ValidatorIDs are lowercase kebab-case ("trailer-marker"), deliberately,
// and validator_test.go enforces it. UPPER_SNAKE_CASE is this codebase's
// convention for API error codes (webhost's errors.go) and lifecycle
// states, neither of which is what this is: an id is a backup set's own
// configuration value, the same class as completion.strategy
// ("rename"/"marker"/"stable") and storage's unavailable_reason
// ("not_created"/"unreadable"/"misconfigured"), which are lowercase for
// the same reason. It is also written by hand into a YAML file whose
// every other key and value is lowercase, and it doubles as its own
// script's basename -- "trailer-marker" IS "trailer-marker.sh" -- so an
// UPPER_SNAKE id would either break that identity or need a translation
// table nothing else here wants.
package service

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

//go:embed validators/*.sh
var validatorScripts embed.FS

// ValidatorID names one entry in FR-13's registered application-validator
// catalog. This is the only shape the API/UI layer may ever use to select
// a backup set's application validator; see this file's package doc.
type ValidatorID string

// ValidatorTrailerMarker checks that the artifact's own content ends with
// a fixed completion trailer (validators/trailer-marker.sh). A producer
// that wants this extra confirmation appends the trailer after it
// finishes writing the artifact.
const ValidatorTrailerMarker ValidatorID = "trailer-marker"

// catalogScripts maps every registered ValidatorID onto the embedded
// script that implements it. This is the entire catalog: nothing outside
// this file may add an entry, and nothing in this package ever builds a
// map entry from a caller-supplied value.
var catalogScripts = map[ValidatorID]string{
	ValidatorTrailerMarker: "trailer-marker.sh",
}

// validatorTimeout is fixed, not caller-configurable, for the same reason
// the catalog itself is fixed: nothing about how a registered validator
// runs is something a caller outside this package gets to choose.
const validatorTimeout = 30 * time.Second

// errUnregisteredValidator is returned by resolveValidator for any id not
// in RegisteredValidators, including a raw executable path: see this
// file's package doc for why this package does not attempt to detect a
// path shape specifically.
//
// Unexported alongside resolveValidator: an exported sentinel for an error
// no caller outside this package can provoke is API that does nothing.
var errUnregisteredValidator = errors.New("service: not a registered validator identifier")

// Validator is one catalog entry as the API/UI layer sees it: the id a
// create request may send, and one line of operator-facing prose to label
// it with. Nothing here names, hints at, or can be turned into the script
// it resolves to -- see this file's package doc for why the resolution
// never crosses this boundary, and validator_test.go's
// TestRegisteredValidatorsNeverExposeAPath for the structural check that
// keeps it that way.
type Validator struct {
	ID ValidatorID
	// Summary is what a picklist renders beside the id. One sentence,
	// describing what the validator checks rather than how.
	Summary string
}

// RegisteredValidators lists every ValidatorID resolveValidator accepts,
// in a fixed, deterministic order, for a caller that wants to offer this
// as a picklist (apps/common/webhost's GET /api/v1/validators, which the
// add-backup-set wizard's step 5 reads).
func RegisteredValidators() []Validator {
	return []Validator{{
		ID:      ValidatorTrailerMarker,
		Summary: "Confirms the artifact's own content ends with the completion trailer its producer appends when it has finished writing.",
	}}
}

// resolveValidator maps id onto the internal/config.Command FR-13's
// application-validator step (internal/lifecycle/verify.go's
// runValidator) actually runs, materializing the catalog's scripts into
// dir first. The returned Command flows into exactly the same
// config.Validation.Command field, and exactly the same runValidator, the
// trusted CLI/YAML config path already uses: this function only ever
// narrows what a caller may put there, it never adds a second way of
// running one.
//
// dir is the caller's to choose only in the sense that BackupService
// derives it from cfg.State.Database (validatorScriptDir); it is never
// caller input from outside core/, and neither is id.
func resolveValidator(dir string, id ValidatorID) (config.Command, error) {
	cmd, err := lookupValidator(dir, id)
	if err != nil {
		return config.Command{}, err
	}
	if err := materializeValidators(dir); err != nil {
		return config.Command{}, err
	}
	return cmd, nil
}

// lookupValidator is resolveValidator's pure half: the catalog lookup and
// the path it implies, with no filesystem access at all. Split out so
// applyValidatorCatalog can materialize once for a whole config rather
// than once per backup set, and so an unregistered id is refused before
// anything is written anywhere.
func lookupValidator(dir string, id ValidatorID) (config.Command, error) {
	scriptName, ok := catalogScripts[id]
	if !ok {
		return config.Command{}, fmt.Errorf("%w: %q", errUnregisteredValidator, id)
	}
	if dir == "" {
		return config.Command{}, errNoValidatorDir
	}
	return config.Command{
		Executable: filepath.Join(dir, scriptName),
		Timeout:    config.Duration(validatorTimeout),
	}, nil
}

// errNoValidatorDir is returned when a validator has to be resolved but
// there is nowhere to materialize it: a *config.Config with no
// state.database, which config.Validate already refuses, or a
// BackupService built with New from one. It is deliberately an error
// rather than a fallback to TMPDIR or the working directory -- writing an
// executable to a guessed location is exactly what this file exists to
// stop.
var errNoValidatorDir = errors.New("service: no state directory to materialize validator scripts into")

// validatorScriptDirName is the state directory's own subdirectory for
// materialized validator scripts. See this file's package doc for why it
// lives there and not in TMPDIR.
const validatorScriptDirName = "validators"

// validatorScriptDir is where cfg's validators materialize: beside the
// state database. It returns "" for a config with no state.database (a
// core/ test's in-memory *config.Config), which lookupValidator turns
// into errNoValidatorDir rather than resolving anything relative to
// whatever the process's working directory happens to be.
func validatorScriptDir(cfg *config.Config) string {
	if cfg == nil || cfg.State.Database == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.State.Database), validatorScriptDirName)
}

// materializeValidators writes every embedded validator script into dir,
// creating dir if it is missing and rewriting any script that is missing
// or whose content has drifted from the embedded copy.
//
// It is called on every resolution AND at the start of every run cycle
// (BackupService.refreshValidatorScripts), not once per process. That is
// the point: the previous implementation cached its result (and its
// error) under a sync.Once, so a script removed or edited after the fact
// was never noticed and one transient failure disabled every validator
// until a restart. Resolution alone would not have fixed that, since
// nothing on the run path re-resolves -- internal/lifecycle execs the
// command it was handed at load time -- which is why the cycle calls this
// directly. The cost of checking is a stat and a read per script, against
// a catalog that has one entry.
//
// A rewrite goes through a temp file and a rename rather than truncating
// in place, so a validator that happens to be executing while this runs
// is never handed a half-written script.
func materializeValidators(dir string) error {
	if dir == "" {
		return errNoValidatorDir
	}
	// 0o700: these are executables this process runs, and nothing else on
	// the host has any business reading or replacing them.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("service: creating validator script directory %s: %w", dir, err)
	}

	entries, err := validatorScripts.ReadDir("validators")
	if err != nil {
		return fmt.Errorf("service: reading embedded validator scripts: %w", err)
	}

	for _, entry := range entries {
		want, err := validatorScripts.ReadFile("validators/" + entry.Name())
		if err != nil {
			return fmt.Errorf("service: reading embedded validator script %s: %w", entry.Name(), err)
		}
		path := filepath.Join(dir, entry.Name())
		if upToDate(path, want) {
			continue
		}
		if err := writeScriptAtomically(path, want); err != nil {
			return fmt.Errorf("service: writing validator script %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// upToDate reports whether path already holds exactly want AND is
// executable. Anything else -- missing, truncated, edited, or left
// non-executable by a restrictive umask on an older release -- is a
// rewrite.
func upToDate(path string, want []byte) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		return false
	}
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, want)
}

// writeScriptAtomically writes data to path via a temp file and a rename.
// 0o755 on the file: this manager's own process is what execs these
// scripts, and the 0o700 directory above already keeps them out of reach
// of anything else on the host.
func writeScriptAtomically(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// planValidatorCatalog resolves every backup set's
// Validation.ValidatorID into the Validation.Command
// internal/lifecycle/verify.go actually runs, and is the ONE place a
// persisted id becomes a runnable path. It does every fallible part of
// that (the catalog lookup and materializing the scripts) and returns the
// in-memory assignment as a func that cannot fail, so a caller that is
// about to write cfg back to disk can prove the whole resolution works
// BEFORE the write and assign afterwards (CreateBackupSet, backupsets.go).
// applyValidatorCatalog below is the same thing in one call, for a caller
// with nothing to write.
//
// Call it AFTER config.Validate, never before: Validate refuses a
// Validation that names both a validator_id and a command, which is
// exactly the shape the returned func produces. And write cfg out BEFORE
// applying, never after: a config written back out with the resolved
// command in it would persist the path this whole design exists to keep
// out of the file.
//
// A config with no backup set using a registered validator does nothing
// at all here -- no directory is created -- so a deployment that has
// never selected one never grows the scaffolding for it.
func planValidatorCatalog(cfg *config.Config) (func(), error) {
	noop := func() {}
	if cfg == nil || !usesRegisteredValidator(cfg) {
		return noop, nil
	}

	dir := validatorScriptDir(cfg)
	if err := materializeValidators(dir); err != nil {
		return nil, err
	}

	// Indices, not *config.BackupSet pointers: the plan outlives this
	// loop, and a pointer into a slice a caller may still append to is
	// how the assignment would silently land on the wrong backup set.
	type resolution struct {
		source, set int
		cmd         config.Command
	}
	var plan []resolution

	for i := range cfg.Sources {
		for j := range cfg.Sources[i].BackupSets {
			bs := &cfg.Sources[i].BackupSets[j]
			id := ValidatorID(bs.Validation.ValidatorID)
			if id == "" {
				continue
			}
			if bs.Validation.Command != nil {
				return nil, fmt.Errorf("service: backup set %q/%q names both a validator_id and a command", cfg.Sources[i].Name, bs.Name)
			}
			cmd, err := lookupValidator(dir, id)
			if err != nil {
				return nil, fmt.Errorf("service: backup set %q/%q: %w", cfg.Sources[i].Name, bs.Name, err)
			}
			plan = append(plan, resolution{source: i, set: j, cmd: cmd})
		}
	}

	return func() {
		for _, r := range plan {
			cmd := r.cmd
			cfg.Sources[r.source].BackupSets[r.set].Validation.Command = &cmd
		}
	}, nil
}

// applyValidatorCatalog is planValidatorCatalog plus its assignment, for
// the callers that are not writing cfg back to disk: Open (via
// OpenConfigAndJournal) at load time. See planValidatorCatalog for the
// ordering rules, which apply here unchanged.
func applyValidatorCatalog(cfg *config.Config) error {
	apply, err := planValidatorCatalog(cfg)
	if err != nil {
		return err
	}
	apply()
	return nil
}

// checkValidatorCatalogMembership refuses a config naming a validator id
// this build does not know. It reads nothing but cfg -- no directory is
// created, no script is written, no journal is touched -- which is the
// whole reason it exists separately from planValidatorCatalog: startup
// runs it BEFORE §46.1's sequence, so a typo'd or retired validator_id
// can never abort a process that has already applied a schema migration
// on its way past. A migration that has been applied cannot be walked
// back by downgrading the binary (state.ErrUnknownSchemaVersion), so the
// one refusal that needs nothing from disk belongs in front of it.
func checkValidatorCatalogMembership(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	for i := range cfg.Sources {
		for j := range cfg.Sources[i].BackupSets {
			bs := &cfg.Sources[i].BackupSets[j]
			id := ValidatorID(bs.Validation.ValidatorID)
			if id == "" {
				continue
			}
			if !isRegisteredValidator(id) {
				return fmt.Errorf("service: backup set %q/%q: %w: %q", cfg.Sources[i].Name, bs.Name, errUnregisteredValidator, id)
			}
		}
	}
	return nil
}

// refreshValidatorScripts rewrites this BackupService's registered
// validator scripts from the embedded copies, and is called at the start
// of every run cycle (executeRunCycle in operations.go, runScheduledCycle
// in scheduler.go) before anything execs one.
//
// Resolution happens at load and create time, and internal/lifecycle
// execs the Command it was handed then, so without this nothing would
// ever re-check the file between one process start and the next. That
// gap is not symmetric. A script that was reaped fails closed: every
// artifact in the set fails at exec and no remote is deleted. A script
// that was replaced or truncated fails OPEN: "#!/bin/sh\nexit 0" passes
// every artifact, and a passing validator is precisely what authorizes
// deleting the remote source. Rewriting once per cycle closes both.
//
// The directory is derived from the live config at each call rather than
// cached on the BackupService, so there is exactly one source for it and
// a hot reload cannot leave this pointing somewhere the resolution does
// not.
//
// A config where no backup set selects a validator does nothing here, so
// the overwhelmingly common deployment pays a map walk per cycle and
// nothing else.
func (b *BackupService) refreshValidatorScripts() error {
	cfg := b.state.Load().inner.Config
	if cfg == nil || !usesRegisteredValidator(cfg) {
		return nil
	}
	return materializeValidators(validatorScriptDir(cfg))
}

// usesRegisteredValidator reports whether any backup set in cfg selects a
// validator by id.
func usesRegisteredValidator(cfg *config.Config) bool {
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			if bs.Validation.ValidatorID != "" {
				return true
			}
		}
	}
	return false
}

// isRegisteredValidator reports whether id is one this package will
// resolve. CreateBackupSet uses it to refuse an unregistered id as an
// invalid request, before anything is persisted -- the same structural
// refusal resolveValidator gives, moved early enough to be a 400 rather
// than a half-written config.
func isRegisteredValidator(id ValidatorID) bool {
	_, ok := catalogScripts[id]
	return ok
}

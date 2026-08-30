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
// naming an executable at all. ResolveValidator is the only function in
// this package that turns a ValidatorID into an internal/config.Command,
// and it can only ever return one of the catalog entries this file itself
// defines, never anything built from caller input. A caller that hands it
// a raw path, or any other string outside RegisteredValidators, gets
// ErrUnregisteredValidator, exactly like a typo would: this package does
// not special-case "that looks like a path" detection, because it does
// not need to -- an unrecognized identifier is refused structurally,
// regardless of what it happens to look like.
package service

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// ErrUnregisteredValidator is returned by ResolveValidator for any id not
// in RegisteredValidators, including a raw executable path: see this
// file's package doc for why this package does not attempt to detect a
// path shape specifically.
var ErrUnregisteredValidator = errors.New("service: not a registered validator identifier")

// RegisteredValidators lists every ValidatorID ResolveValidator currently
// accepts, in a fixed, deterministic order, for a caller (a future
// backup-set-creation UI) that wants to offer this as a picklist.
func RegisteredValidators() []ValidatorID {
	return []ValidatorID{ValidatorTrailerMarker}
}

// ResolveValidator maps id onto the internal/config.Command FR-13's
// application-validator step (internal/lifecycle/verify.go's
// runValidator) actually runs. The returned Command flows into exactly
// the same config.Validation.Command field, and exactly the same
// runValidator, the trusted CLI/YAML config path already uses: this
// function only ever narrows what a caller may put there, it never adds a
// second way of running one.
func ResolveValidator(id ValidatorID) (config.Command, error) {
	scriptName, ok := catalogScripts[id]
	if !ok {
		return config.Command{}, fmt.Errorf("%w: %q", ErrUnregisteredValidator, id)
	}

	dir, err := materializedScriptDir()
	if err != nil {
		return config.Command{}, err
	}

	return config.Command{
		Executable: filepath.Join(dir, scriptName),
		Timeout:    config.Duration(validatorTimeout),
	}, nil
}

var (
	materializeOnce sync.Once
	materializeDir  string
	materializeErr  error
)

// materializedScriptDir writes every embedded validator script out to a
// fixed, process-lifetime temp directory, once, so ResolveValidator can
// hand out a stable absolute path config.Validation.Command.Executable can
// run directly (internal/lifecycle/verify.go's runValidator invokes
// Executable as a plain argv[0], never through a shell, so it has to be a
// real file on disk with its own shebang and execute bit, not a string
// this package could hand over any other way).
func materializedScriptDir() (string, error) {
	materializeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rclone-manager-validators-")
		if err != nil {
			materializeErr = fmt.Errorf("service: creating validator script directory: %w", err)
			return
		}

		entries, err := validatorScripts.ReadDir("validators")
		if err != nil {
			materializeErr = fmt.Errorf("service: reading embedded validator scripts: %w", err)
			return
		}

		for _, entry := range entries {
			data, err := validatorScripts.ReadFile("validators/" + entry.Name())
			if err != nil {
				materializeErr = fmt.Errorf("service: reading embedded validator script %s: %w", entry.Name(), err)
				return
			}
			// 0o755: this manager's own process is what will exec these
			// scripts (via runValidator), so they need to be readable and
			// executable by their own owner; MkdirTemp's default 0o700
			// directory permission already keeps them out of reach of
			// anything else on the host.
			if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0o755); err != nil {
				materializeErr = fmt.Errorf("service: writing validator script %s: %w", entry.Name(), err)
				return
			}
		}

		materializeDir = dir
	})
	return materializeDir, materializeErr
}

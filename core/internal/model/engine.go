// Which machinery produces a backup set's restore points, in this
// product's own vocabulary.
//
// This is the one decision EPIC K adds to a backup set, and the only one
// that is allowed to change what a run does end to end: an artifact set
// copies a finished file and verifies it, an incremental set hands a source
// tree to the embedded engine and stores a snapshot. Everything else in
// this file exists to make sure that decision can only ever be made
// explicitly.
//
// # Why omission is not a choice
//
// Every backup set configured before this type existed says nothing about
// an engine, and every one of them means the artifact engine. So the
// resolver below reads "" as the artifact engine, by name, in one place,
// rather than leaving each caller to compare against the zero value of a
// string. The alternative -- a BackupEngine whose zero value happens to be
// the artifact engine -- looks identical until somebody adds a third engine
// and the zero value becomes whichever constant sorts first in whoever's
// head is holding it. Silence has to resolve to a NAMED engine, and the
// naming has to happen somewhere a test can point at.
//
// # Why the vendor's name is a value here and not a type
//
// The incremental engine's configured identifier is the string "kopia",
// because that is what the operator writes and what the API will carry.
// That is a VALUE in this product's namespace, not a borrowed type: no
// package outside core/internal/backupengine/kopia may import the vendor,
// and core/internal/backupengine/boundary_test.go enforces that for the
// import path and for the package qualifier across the whole repository,
// including this package. The distinction matters because the two look
// similar and only one of them is a leak: an enum value an operator typed
// costs nothing if the engine is ever replaced, while an upstream struct in
// a signature is a fork waiting to happen.

package model

import (
	"fmt"
	"slices"
)

// BackupEngine names the machinery that produces one backup set's restore
// points (EPIC K). It is deliberately independent of where those restore
// points are stored and of how the source is reached: an engine is not an
// rclone backend, and a repository's storage is not an engine. Listing the
// three as one set of interchangeable choices is the specific modelling
// mistake EPIC K's architectural rules name.
type BackupEngine string

const (
	// EngineArtifact is the engine this product shipped with: discovery
	// finds a COMPLETED artifact on a source, the artifact is copied to
	// durable storage, verified, committed, and the source copy is
	// optionally deleted afterwards (FR-8, FR-15).
	//
	// It is what every configuration written before the engine key
	// existed means, and it stays the default for every new one that
	// does not say otherwise.
	EngineArtifact BackupEngine = "artifact"

	// EngineKopia is the embedded incremental engine: a source TREE is
	// scanned, unchanged content is referenced rather than stored again,
	// and the result is a snapshot in a repository
	// (core/internal/backupengine).
	//
	// A completed snapshot NEVER deletes anything on the source. That is
	// not a property of this constant, it is a property of the lifecycle
	// this engine selects, and it is the single most important difference
	// between the two engines: the artifact engine's whole point is that a
	// committed copy releases the source's copy, and applying that to a
	// live source tree would delete the operator's data.
	EngineKopia BackupEngine = "kopia"
)

// backupEngines is every engine, in the order a surface should offer them:
// the one that is already running everywhere first.
//
// A third entry is an operator-visible decision with a lifecycle, a
// catalog shape and a verification story attached, which is what the count
// assertion in the tests is defending.
var backupEngines = []BackupEngine{EngineArtifact, EngineKopia}

// BackupEngines returns the vocabulary, in the order a surface should
// offer it, as a copy the caller owns.
//
// A copy rather than the slice itself, following
// backend.CapabilityKeys(): a closed set a caller can assign through is
// not closed, and the assignment would change what ResolveBackupEngine
// accepts for every goroutine in the process.
func BackupEngines() []BackupEngine { return slices.Clone(backupEngines) }

// ResolveBackupEngine turns what configuration said -- including having
// said nothing -- into the engine a backup set actually runs under.
//
// An empty string resolves to EngineArtifact. That is the whole promise
// EPIC K makes to every deployment already running: a set with no engine
// key is an artifact set, decided here, so no caller ever has to decide it
// again and no upgrade can silently reinterpret an existing job.
//
// Anything else this package does not recognise is an error rather than a
// fall-back to either engine. Defaulting would be wrong in both
// directions: reading a typo as the artifact engine would silently ignore
// an operator who asked for snapshots, and reading it as the incremental
// engine would silently change what a run does to a source.
func ResolveBackupEngine(declared string) (BackupEngine, error) {
	if declared == "" {
		return EngineArtifact, nil
	}

	for _, e := range backupEngines {
		if string(e) == declared {
			return e, nil
		}
	}

	return "", fmt.Errorf("unknown backup engine %q (expected %q or %q; omit the key for %q)",
		declared, EngineArtifact, EngineKopia, EngineArtifact)
}

func (e BackupEngine) String() string { return string(e) }

// UsesRepository reports whether this engine stores its output in a
// repository, and therefore whether a backup set running under it has to
// name a Repository Domain.
//
// It is a method rather than an equality test at each call site because
// "which engines have a repository" is exactly the kind of fact that
// acquires a second answer the day a third engine lands, and a comparison
// spelled out in a validator is not somewhere anybody will look for it.
//
// An engine this method has not been taught reports false, which is the
// conservative direction: a repository reference is refused as
// meaningless rather than a repository being silently invented for an
// engine nobody taught this package about.
func (e BackupEngine) UsesRepository() bool { return e == EngineKopia }

// Describe is the operator-facing sentence for this engine. Both the
// wizard's engine picker and the run report are renderings of these, so an
// engine without one would render as a blank row somewhere.
func (e BackupEngine) Describe() string {
	switch e {
	case EngineArtifact:
		return "copies completed artifacts and verifies each copy; a committed copy may release the source's own"
	case EngineKopia:
		return "snapshots a source tree incrementally into a repository; never deletes anything on the source"
	default:
		return fmt.Sprintf("unknown backup engine %q", string(e))
	}
}

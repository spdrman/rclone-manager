// Package discovery is the FR-8 implementation: it turns a raw transport
// listing into artifacts proven complete, and journals each one exactly
// once through the FR-10 state machine.
//
// # The three strategies
//
// A backup set's config.Completion.Strategy picks one of three ways to
// decide a candidate is finished being written, never in-flight:
//
//   - "rename": the producer writes under a name this package recognizes as
//     in-progress and atomically renames to its final name only once done
//     (see isProducerTempName). Once a name survives that filter, the
//     producer's own atomic rename is what makes it visible under that name
//     at all, so there is nothing further to prove. FR-8 calls this the
//     preferred strategy, because it is the only one of the three that asks
//     the producer to make a positive, atomic assertion rather than asking
//     this package to infer one.
//   - "marker": a sibling completion marker (see markerSuffix) or a
//     directory-level manifest marker (see effectiveManifestMarker,
//     config.Completion.ManifestMarker, issue #291) tells this package the
//     producer finished writing.
//   - "stable": config.Completion.StableFor says how long a candidate's
//     remote modification time must already be in the past before it is
//     presumed done. This is the weakest of the three: it is inference, not
//     an assertion, and a same-second rewrite after the window elapses
//     would not be caught here (that is a discovery-time judgement call,
//     independent of the FR-16 identity comparison a later delete recheck
//     performs against a very different question: did this exact object
//     change after it was already trusted).
//
// See complete.go for exactly how each of those is checked.
//
// # Recursion and the basename collision it creates
//
// internal/transport/rclone's Adapter.List now recurses the whole remote
// tree (see its doc comment), so this package sees every artifact
// regardless of how many directories deep a producer nested it. That
// surfaces a real hazard the flat-listing world never had to face:
// model.ArtifactID names an artifact by its basename alone, and two
// different remote paths in the same backup set can share one, for example
// gitea-runs/run-1/backup.dump and gitea-runs/run-2/backup.dump if a
// producer reuses the same filename every run. Silently letting the second
// one's discovery collapse onto the first's journal row would mean the
// journal ends up pointing a stale remote path at an artifact that was
// actually replaced, which is precisely the kind of quiet corruption this
// project exists to prevent (see model.RemoteIdentity's TOCTOU comment for
// the sibling failure mode this one rhymes with).
//
// Discover refuses to let that happen silently. Every discovery attempt's
// idempotency key is derived from the full remote path, not just the
// artifact identity (see discoverKey), so a second path colliding on the
// same basename cannot be mistaken for a retry of the first: the underlying
// journal's own UNIQUE(source, backup_set, artifact_name) constraint refuses
// the second insert (state.ErrAlreadyDiscovered), and Discover turns that
// into a reported Conflict rather than an error that aborts the whole
// batch, or worse, a silently dropped artifact.
//
// # Untrusted input
//
// Every string this package reads from a remote listing is untrusted (FR-8).
// Nothing here ever reaches a shell. A relative path is rejected outright if
// it does not clean to itself (isCleanRelativePath catches "..", a leading
// "/", an embedded NUL, and similar), and the basename of every surviving
// candidate must pass model.NewArtifactID, which independently refuses
// anything that is not a plain, traversal-free filename, before it is
// treated as an identity for anything. A name that fails either check is
// reported in Result.Rejected, never merely dropped.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Deps is what Discover needs from the rest of the manager. It mirrors
// lifecycle.Deps deliberately (same field names and the same nil-means-
// time.Now().UTC() clock convention): Discover forwards it directly into
// lifecycle.Advance, and a test can build one Deps value and use it for both
// this package's assertions and any lifecycle-level ones in the same table.
type Deps struct {
	Transport transport.Transport
	Journal   lifecycle.Journal

	// Now is injectable so a test can control both the "has this been
	// stable long enough" judgement and the OccurredAt every newly
	// discovered artifact is stamped with, from one fixed instant. Nil
	// means time.Now.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

func (d Deps) lifecycleDeps() lifecycle.Deps {
	return lifecycle.Deps{Journal: d.Journal, Transport: d.Transport, Now: d.Now}
}

// PendingCandidate is a remote object Discover saw, matched against the
// backup set's include patterns, but could not yet prove complete.
type PendingCandidate struct {
	RemotePath string
	Reason     string
}

// RejectedEntry is a remote object Discover refused to treat as a candidate
// at all: a hostile or malformed name, or a path shaped like a traversal
// attempt. See the package doc's "Untrusted input" section.
type RejectedEntry struct {
	RemotePath string
	Reason     string
}

// Conflict is a basename collision: a second remote path that would resolve
// to a model.ArtifactID the journal already holds under a different remote
// path. See the package doc's "Recursion and the basename collision it
// creates" section. Discover records the first path it sees for a given
// identity and reports every later collision here instead of overwriting or
// silently ignoring it.
type Conflict struct {
	Artifact model.ArtifactID

	// RemotePath is the path this Discover call saw that collided.
	RemotePath string

	// RecordedPath is the path already holding this identity in the
	// journal, i.e. the one that won the race and was actually discovered.
	RecordedPath string
}

// CandidateError is a per-candidate failure that did not stop the rest of
// the batch: Discover keeps going so one bad remote object cannot hide every
// other artifact's result behind it.
type CandidateError struct {
	RemotePath string
	Err        error
}

func (e CandidateError) Error() string { return fmt.Sprintf("%s: %v", e.RemotePath, e.Err) }
func (e CandidateError) Unwrap() error { return e.Err }

// Result is everything one Discover call found, partitioned by what
// happened to each remote object it saw. Nothing List returned disappears
// from this without a reason attached somewhere below: that is the whole
// point of this package existing (see the package doc).
type Result struct {
	// Discovered lists every artifact this call proved complete and
	// recorded as DISCOVERED for the first time.
	Discovered []state.Record

	// AlreadyKnown lists candidates proven complete again that had already
	// been discovered at the exact same remote path by an earlier call:
	// lifecycle.Advance recognised the same deterministic key and changed
	// nothing (Outcome.Applied == false).
	AlreadyKnown []state.Record

	Pending   []PendingCandidate
	Rejected  []RejectedEntry
	Conflicts []Conflict
	Errors    []CandidateError
}

// Discover lists source through deps.Transport, decides which of the
// results are candidates for set (matching its include patterns, and not
// one of this package's own marker/temp-file names), proves each candidate
// complete under set's configured strategy, and journals every newly
// complete one via lifecycle.Advance. See the package doc for the full
// design.
//
// Discover returns a non-nil error only for something systemic: deps is
// incomplete, set has not been through config.Validate, or listing the
// source itself failed. A problem with one specific candidate is recorded
// in the returned Result instead (Pending, Rejected, Conflicts or Errors),
// so that one bad remote object never hides every other artifact's result.
func Discover(ctx context.Context, deps Deps, source transport.Source, set config.BackupSet) (Result, error) {
	if deps.Transport == nil {
		return Result{}, fmt.Errorf("discovery: Deps needs a Transport")
	}
	if deps.Journal == nil {
		return Result{}, fmt.Errorf("discovery: Deps needs a Journal")
	}
	if set.ID.IsZero() {
		return Result{}, fmt.Errorf("discovery: backup set %q has no id; run it through config.Validate first", set.Name)
	}

	artifacts, err := deps.Transport.List(ctx, source)
	if err != nil {
		return Result{}, fmt.Errorf("discovery: listing %s: %w", set.ID, err)
	}

	known := make(map[string]transport.RemoteArtifact, len(artifacts))
	for _, a := range artifacts {
		known[a.Path] = a
	}

	now := deps.now()

	var res Result
	for _, a := range artifacts {
		relPath := a.Path

		if !isCleanRelativePath(relPath) {
			res.Rejected = append(res.Rejected, RejectedEntry{
				RemotePath: relPath,
				Reason:     "path does not clean to itself; refusing a traversal-shaped remote name",
			})
			continue
		}

		base := path.Base(relPath)

		artifactID, err := model.NewArtifactID(set.ID, base)
		if err != nil {
			res.Rejected = append(res.Rejected, RejectedEntry{RemotePath: relPath, Reason: err.Error()})
			continue
		}

		if isMarkerObject(base, set.Completion) {
			// A completion signal, not a payload. Never a candidate in its
			// own right, in any strategy: reporting it as Pending/Rejected
			// would just be noise every discovery pass repeats forever.
			continue
		}
		if isProducerTempName(base) {
			// Still being written under a recognized in-progress name.
			// Expected and routine, not a discovery gap.
			continue
		}
		if !includeMatches(set.Include, base) {
			continue
		}

		complete, reason := isComplete(a, set.Completion, known, now)
		if !complete {
			res.Pending = append(res.Pending, PendingCandidate{RemotePath: relPath, Reason: reason})
			continue
		}

		identity, err := captureRemoteIdentity(ctx, deps.Transport, source, relPath)
		if err != nil {
			res.Errors = append(res.Errors, CandidateError{RemotePath: relPath, Err: fmt.Errorf("capturing identity: %w", err)})
			continue
		}

		outcome, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
			Artifact:   artifactID,
			Key:        discoverKey(set.ID, relPath),
			From:       "",
			To:         string(lifecycle.Discovered),
			OccurredAt: now,
			RemotePath: relPath,
			Remote:     &identity,
		})
		switch {
		case err == nil:
			if outcome.Applied {
				res.Discovered = append(res.Discovered, outcome.Record)
			} else {
				res.AlreadyKnown = append(res.AlreadyKnown, outcome.Record)
			}
		case errors.Is(err, state.ErrAlreadyDiscovered):
			existing, getErr := deps.Journal.Get(ctx, artifactID)
			if getErr != nil {
				res.Errors = append(res.Errors, CandidateError{
					RemotePath: relPath,
					Err:        fmt.Errorf("%s already discovered under a different attempt, and re-reading it failed: %w", artifactID, getErr),
				})
				continue
			}
			if existing.RemotePath == relPath {
				res.AlreadyKnown = append(res.AlreadyKnown, existing)
			} else {
				res.Conflicts = append(res.Conflicts, Conflict{
					Artifact:     artifactID,
					RemotePath:   relPath,
					RecordedPath: existing.RemotePath,
				})
			}
		default:
			res.Errors = append(res.Errors, CandidateError{RemotePath: relPath, Err: fmt.Errorf("recording as discovered: %w", err)})
		}
	}

	return res, nil
}

// discoverKey derives Discover's idempotency key for one remote path.
//
// It is built from the full path, not just the artifact identity that path
// resolves to, precisely so that two different paths colliding on the same
// basename are never mistaken for a retry of each other (see the package
// doc). The three parts are joined with NUL, which none of them can contain:
// model.NewBackupSetID already refuses NUL in either half, and
// isCleanRelativePath refuses it in relPath, so there is no encoding this
// could ambiguate that a plain "/"-or-":"-joined string would not (a backup
// set literally named with a colon in it is valid per model.NewBackupSetID,
// which would make a naive separator-joined key collide across two
// different (set, path) pairs).
func discoverKey(set model.BackupSetID, relPath string) string {
	return "discover\x00" + set.Source + "\x00" + set.Set + "\x00" + relPath
}

// isCleanRelativePath reports whether p is a well-formed, traversal-free
// relative path: no leading slash, no "." or ".." segment, no empty segment,
// and none of the control characters model's own validation refuses
// elsewhere (NUL, CR, LF). Remote listings are untrusted input (FR-8); this
// runs before anything derived from p is trusted for anything, including
// being used as half of an idempotency key.
func isCleanRelativePath(p string) bool {
	switch {
	case p == "":
		return false
	case strings.HasPrefix(p, "/"):
		return false
	case strings.ContainsAny(p, "\x00\n\r"):
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// captureRemoteIdentity builds the FR-16 identity to persist alongside a
// newly discovered artifact, mirroring internal/transport/contract.Capture's
// degrade-honestly rule without this package depending on that test-support
// package: a hash is attempted best-effort and simply left off when the
// backend cannot produce one. A hardened, shell-less SFTP account (FR-6's
// recommended posture) is the expected case for that, not a failure.
//
// It calls Stat rather than reusing the transport.RemoteArtifact Discover
// already has from List, on purpose: this identity is meant to describe the
// object as of the moment it was proven complete and is about to be
// journaled, which is a later, more relevant instant than whenever the
// original listing happened to run.
func captureRemoteIdentity(ctx context.Context, tr transport.Transport, source transport.Source, relPath string) (state.RemoteIdentity, error) {
	art, err := tr.Stat(ctx, source, relPath)
	if err != nil {
		return state.RemoteIdentity{}, err
	}

	size := art.Size
	ri := state.RemoteIdentity{Size: &size, BackendID: art.ID}
	if art.ModTime != 0 {
		t := time.Unix(art.ModTime, 0).UTC()
		ri.ModTime = &t
	}

	if hash, hashErr := tr.RemoteHash(ctx, source, relPath, transport.SHA256); hashErr == nil && hash != "" {
		ri.Hash = hash
		ri.HashAlg = string(transport.SHA256)
	}

	return ri, nil
}

package discovery

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file answers the one question FR-8 turns on: is this remote object
// finished being written, or am I looking at a producer mid-flight?
//
// Everything else in this package is bookkeeping around that answer, and
// the answer is worth isolating because getting it wrong is not
// symmetrical. Saying "not yet" about a finished artifact costs a delay
// until the next pass. Saying "done" about a half-written one journals a
// truncated backup as a real one, and every later stage, verification,
// retention, the eventual delete of the remote original, then acts on that
// claim.
//
// So the three strategies here are ordered by how much they ask this
// package to INFER, and the code follows that order rather than
// alphabetical: rename asks nothing (the producer's own atomic rename is
// the proof), marker asks for a second object to exist, and stable asks
// this package to guess from a clock. The package doc says why stable is
// the weakest, and isComplete's default branch says why a fourth,
// unrecognised strategy is never treated as complete.
//
// The literal conventions in here (a ".complete" sibling, the ".tmp"
// family, "_SUCCESS") are this package's own choices, not FR-8's words,
// because a real producer is usually something an operator cannot
// reconfigure. Each constant says so where it is defined, and
// ManifestMarker is the one an operator can override.

// markerSuffix is this package's convention for FR-8's "producer completion
// marker" strategy variant: a sibling object at <artifact-path>+markerSuffix
// signals that specific artifact is finished being written. FR-8 does not
// name a literal marker filename (config.Completion has no field for one),
// so this is a fixed, documented choice rather than something an operator
// configures.
const markerSuffix = ".complete"

// effectiveManifestMarker is FR-8's "manifest marker" strategy variant's
// directory-level completion signal: an object with exactly this name, in
// the same directory as a group of artifacts, signals that every artifact
// the producer intended to write to that directory has been written.
//
// c.ManifestMarker is the operator's configured name (issue #291): a real
// read-only producer is not always able to be reconfigured to write
// "_SUCCESS" (the well-known Hadoop/Spark convention this package
// recognized unconditionally before that field existed), so the name is
// now the operator's to choose. An unset ManifestMarker falls back to
// config.DefaultManifestMarker here, independent of whether c has already
// been through config.Validate (which resolves the same default): this
// package's own tests, and BackupSet values service.CreateBackupSet
// builds, construct a config.Completion directly without always calling
// Validate first, so resolving here too is what keeps an unset field
// meaning "_SUCCESS" everywhere, not just on the path that happens to
// validate first.
func effectiveManifestMarker(c config.Completion) string {
	if c.ManifestMarker == "" {
		return config.DefaultManifestMarker
	}
	return c.ManifestMarker
}

// inProgressSuffixes are basename suffixes this package treats as "still
// being written under a recognized temporary name", universally, regardless
// of which strategy a backup set configures. FR-8 does not name a literal
// convention for this either; these are common enough across producers
// (rsync, borg, ad hoc scripts) to be a safe, documented default. A
// candidate matching one of these is never even considered a candidate: it
// is not reported as Pending, since it usually will not survive a sensible
// include pattern anyway, and treating an obviously in-flight name as
// "maybe complete" would defeat the point of any strategy.
var inProgressSuffixes = []string{".tmp", ".partial", ".inprogress"}

// isMarkerObject reports whether base names one of this package's own
// completion signals rather than a payload artifact, using c's configured
// (or defaulted, see effectiveManifestMarker) manifest marker name. This is
// called for every candidate regardless of c.Strategy (see Discover's
// caller), the same way it always has been: a directory-level manifest
// marker is never itself a payload artifact, whether or not this backup
// set's own strategy is what would go looking for one.
func isMarkerObject(base string, c config.Completion) bool {
	return base == effectiveManifestMarker(c) || strings.HasSuffix(base, markerSuffix)
}

// isProducerTempName reports whether base carries one of
// inProgressSuffixes.
//
// It is deliberately a suffix test on the BASENAME only, which means a
// producer that stages into a directory ("run.tmp/backup.dump") is not
// caught here. That is the right split rather than an oversight: this test
// exists to recognise the one convention that says "these bytes are still
// arriving", and a staging directory that gets renamed as a unit makes the
// artifact appear at its final path atomically, which is the rename
// strategy's case and needs no help from this.
func isProducerTempName(base string) bool {
	for _, suf := range inProgressSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

// includeMatches reports whether base matches at least one of patterns.
// An empty patterns list matches everything: config.Validate does not
// require a backup set to configure include, and the minimal documented
// config (internal/config/testdata/minimal.yaml) omits it entirely, so
// "no patterns configured" has to mean "no filter", not "match nothing".
//
// Matching uses path.Match rather than filepath.Match on purpose: these are
// remote basenames, never local filesystem paths, and path.Match's
// behaviour does not shift with GOOS the way filepath.Match's can. Patterns
// are also guaranteed syntactically valid by config.Validate before this
// ever runs (it calls filepath.Match(pat, "") purely to check the pattern
// compiles), so a match error here is defensive rather than expected.
func includeMatches(patterns []string, base string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if ok, err := path.Match(p, base); err == nil && ok {
			return true
		}
	}
	return false
}

// isComplete decides whether a already satisfies set's configured
// completion strategy. known is every remote object List returned for this
// source, keyed by path, so the "marker" strategy can look for a sibling or
// directory-level marker without a second List call.
//
// It returns a human-readable reason whenever it answers false to
// (deliberately) support Result.Pending: FR-8 discovery not finding a
// candidate complete yet is routine, expected, and worth being specific
// about, not silent.
func isComplete(a transport.RemoteArtifact, c config.Completion, known map[string]transport.RemoteArtifact, now time.Time) (bool, string) {
	switch c.Strategy {
	case "rename":
		// Reaching here at all means isProducerTempName already let this
		// name through: the producer's own atomic rename is what makes an
		// object visible under its final name in the first place, so there
		// is nothing further to check (see the package doc).
		return true, ""

	case "marker":
		if _, ok := known[a.Path+markerSuffix]; ok {
			return true, ""
		}
		manifestMarker := effectiveManifestMarker(c)
		if _, ok := known[path.Join(path.Dir(a.Path), manifestMarker)]; ok {
			return true, ""
		}
		return false, fmt.Sprintf("no %s sibling marker and no %s manifest marker in its directory yet", a.Path+markerSuffix, manifestMarker)

	case "stable":
		if a.ModTime == 0 {
			return false, "backend reported no modification time; cannot prove stability"
		}
		age := now.Sub(time.Unix(a.ModTime, 0))
		stableFor := c.StableFor.Duration()
		if age < stableFor {
			return false, fmt.Sprintf("modified %s ago, needs %s of stability", age.Round(time.Second), stableFor)
		}
		return true, ""

	default:
		// config.Validate refuses any strategy other than these three
		// before a Config is ever used, so this is defensive: a caller that
		// skipped Validate gets an explicit, reported reason rather than
		// this package guessing which of the three was intended.
		return false, fmt.Sprintf("unknown completion strategy %q", c.Strategy)
	}
}

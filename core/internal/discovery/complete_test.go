// These cover complete.go's predicates one at a time, below the level
// discovery_test.go works at.
//
// The split is deliberate. discovery_test.go drives whole passes against a
// real rclone adapter, which is what proves the strategies work on a real
// listing, but it can only reach each predicate through everything in front
// of it, so a case like "a path that does not clean to itself" arrives there
// as one rejection among many. These tests go at the predicates directly,
// where the interesting inputs are the ones a real producer would have to be
// hostile or broken to produce and which are therefore awkward to stage in a
// temp directory.
//
// Nothing here touches the journal or the transport, so every case is a pure
// input/output pair and the tables can be read as the specification of each
// rule.
package discovery

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// TestIsCleanRelativePath is the untrusted-input gate from the package
// doc's own section, and the bad list is the point rather than the good one.
//
// Each entry is a distinct escape rather than a variation on one: an
// absolute path, a parent reference at the front, a parent reference in the
// middle, a "." segment, an empty segment from a doubled slash, and the
// three control characters that would let a name lie about where it ends
// once it reaches a log line or a path join. ".." on its own is separate
// from "../escape.txt" because it is the whole string rather than a prefix,
// which is exactly the case a HasPrefix-based check would miss.
func TestIsCleanRelativePath(t *testing.T) {
	good := []string{"a.txt", "a/b.txt", "a/b/c.txt", "a-b_c.txt"}
	for _, p := range good {
		if !isCleanRelativePath(p) {
			t.Errorf("isCleanRelativePath(%q) = false, want true", p)
		}
	}

	bad := []string{
		"", "/abs.txt", "../escape.txt", "a/../b.txt", "a/./b.txt",
		"a//b.txt", "a\x00b.txt", "a\nb.txt", "a\rb.txt", "..",
	}
	for _, p := range bad {
		if isCleanRelativePath(p) {
			t.Errorf("isCleanRelativePath(%q) = true, want false", p)
		}
	}
}

// TestIncludeMatches pins the empty-list case first, because it is the one
// that is a judgement call rather than a mechanism: nil and an empty slice
// both mean "no filter configured", and both have to match everything.
//
// Reading them as "match nothing" would be defensible in the abstract and
// catastrophic here, since config.Validate does not require include and the
// documented minimal config omits it, so a backup set with no include would
// silently discover nothing at all and look healthy while doing it.
//
// "*.dump" against "backup.dump.zst" is the second deliberate case: glob
// matching, not substring matching, so a compressed variant is not swept up
// by a pattern that did not ask for it.
func TestIncludeMatches(t *testing.T) {
	cases := []struct {
		patterns []string
		base     string
		want     bool
	}{
		{nil, "anything", true},
		{[]string{}, "anything", true},
		{[]string{"*.dump"}, "backup.dump", true},
		{[]string{"*.dump"}, "backup.dump.zst", false},
		{[]string{"*.dump.zst", "*.dump"}, "backup.dump", true},
		{[]string{"*.txt"}, "backup.dump", false},
	}
	for _, tc := range cases {
		if got := includeMatches(tc.patterns, tc.base); got != tc.want {
			t.Errorf("includeMatches(%v, %q) = %v, want %v", tc.patterns, tc.base, got, tc.want)
		}
	}
}

// TestIsMarkerObject pins that a completion signal is never itself treated
// as a payload artifact, and that recognising one is exact rather than
// fuzzy.
//
// "_SUCCESSFUL" and "success" are in the not-markers list for that reason:
// the manifest marker is an equality test, so a producer that happens to
// write a similarly named file does not accidentally declare a directory
// finished. The sibling marker is a suffix test instead, because it is
// derived from the artifact's own name.
//
// The custom-marker half is issue #291's replace-not-add rule, asserted from
// both directions. Only asserting that SHA256SUMS is recognised would leave
// an implementation that recognised BOTH names passing, and that
// implementation would let one producer's _SUCCESS convention satisfy a
// different producer's backup set by coincidence.
func TestIsMarkerObject(t *testing.T) {
	defaultCompletion := config.Completion{Strategy: "marker"}

	markers := []string{"_SUCCESS", "backup.dump.complete", "x.complete"}
	for _, m := range markers {
		if !isMarkerObject(m, defaultCompletion) {
			t.Errorf("isMarkerObject(%q, default) = false, want true", m)
		}
	}
	notMarkers := []string{"backup.dump", "_SUCCESSFUL", "success"}
	for _, m := range notMarkers {
		if isMarkerObject(m, defaultCompletion) {
			t.Errorf("isMarkerObject(%q, default) = true, want false", m)
		}
	}

	// A configured manifest_marker replaces "_SUCCESS" as the recognized
	// directory-level signal; it does not additionally recognize it
	// (issue #291).
	customCompletion := config.Completion{Strategy: "marker", ManifestMarker: "SHA256SUMS"}
	if !isMarkerObject("SHA256SUMS", customCompletion) {
		t.Errorf(`isMarkerObject("SHA256SUMS", custom) = false, want true`)
	}
	if isMarkerObject("_SUCCESS", customCompletion) {
		t.Errorf(`isMarkerObject("_SUCCESS", custom) = true, want false: a configured manifest_marker should replace the default, not add to it`)
	}
}

// TestIsProducerTempName pins that the match is on the END of the name.
//
// "backup.tmp.dump" is the case that matters: a producer's finished artifact
// can perfectly well have ".tmp" somewhere in the middle of its name, and a
// substring test would then quietly skip it on every pass for ever, which is
// an artifact that is never backed up and never reported as pending
// either.
func TestIsProducerTempName(t *testing.T) {
	temp := []string{"backup.dump.tmp", "backup.dump.partial", "backup.dump.inprogress"}
	for _, name := range temp {
		if !isProducerTempName(name) {
			t.Errorf("isProducerTempName(%q) = false, want true", name)
		}
	}
	notTemp := []string{"backup.dump", "backup.tmp.dump"}
	for _, name := range notTemp {
		if isProducerTempName(name) {
			t.Errorf("isProducerTempName(%q) = true, want false", name)
		}
	}
}

// TestIsComplete_Rename pins that the rename strategy proves nothing
// further, which is the whole reason FR-8 prefers it.
//
// The known map is nil and the artifact has no ModTime, so this also pins
// what rename does NOT consult: an implementation that grew a stability
// check or a marker lookup here would fail on the nil map rather than
// silently making the cheapest strategy the slowest one.
func TestIsComplete_Rename(t *testing.T) {
	a := transport.RemoteArtifact{Path: "x/backup.dump"}
	complete, reason := isComplete(a, config.Completion{Strategy: "rename"}, nil, time.Now())
	if !complete {
		t.Errorf("rename strategy: complete = false, reason %q, want true", reason)
	}
}

// TestIsComplete_Marker covers the two shapes of marker separately, because
// they answer different producer conventions: a sibling ".complete" is per
// artifact, and a directory "_SUCCESS" is per batch.
//
// The no_marker case asserts a non-empty reason rather than a particular
// string. That is the pattern throughout this file: the reason exists so an
// operator can see what a pass is waiting for, and pinning the wording would
// make the message unimprovable while pinning its existence keeps the
// property that a pending candidate always says why.
func TestIsComplete_Marker(t *testing.T) {
	now := time.Now()

	t.Run("sibling_marker", func(t *testing.T) {
		a := transport.RemoteArtifact{Path: "x/backup.dump"}
		known := map[string]transport.RemoteArtifact{"x/backup.dump.complete": {}}
		complete, _ := isComplete(a, config.Completion{Strategy: "marker"}, known, now)
		if !complete {
			t.Errorf("sibling marker present, want complete")
		}
	})

	t.Run("directory_manifest_marker", func(t *testing.T) {
		a := transport.RemoteArtifact{Path: "x/backup.dump"}
		known := map[string]transport.RemoteArtifact{"x/_SUCCESS": {}}
		complete, _ := isComplete(a, config.Completion{Strategy: "marker"}, known, now)
		if !complete {
			t.Errorf("directory manifest marker present, want complete")
		}
	})

	t.Run("no_marker", func(t *testing.T) {
		a := transport.RemoteArtifact{Path: "x/backup.dump"}
		complete, reason := isComplete(a, config.Completion{Strategy: "marker"}, map[string]transport.RemoteArtifact{}, now)
		if complete {
			t.Errorf("no marker present, want incomplete")
		}
		if reason == "" {
			t.Errorf("expected a non-empty reason for a not-yet-complete marker candidate")
		}
	})
}

// TestIsComplete_Marker_CustomManifestMarker is issue #291's own reproduction:
// a producer (the Gitea backup example in the issue) signals a finished run
// directory with a file it names itself (SHA256SUMS), not "_SUCCESS", and
// this manager cannot ask the producer to rename it. Before this issue's
// fix, config.Completion had no field for that name at all, so
// isComplete's "marker" case only ever looked for the hardcoded literal
// "_SUCCESS" -- a directory containing SHA256SUMS was never treated as
// complete no matter what a caller wrote into a Completion value, because
// nothing in the type could carry a different name to check for.
func TestIsComplete_Marker_CustomManifestMarker(t *testing.T) {
	now := time.Now()
	a := transport.RemoteArtifact{Path: "run/backup.dump"}
	custom := config.Completion{Strategy: "marker", ManifestMarker: "SHA256SUMS"}

	t.Run("configured marker present makes it complete", func(t *testing.T) {
		known := map[string]transport.RemoteArtifact{"run/SHA256SUMS": {}}
		complete, reason := isComplete(a, custom, known, now)
		if !complete {
			t.Errorf("configured manifest marker SHA256SUMS present: complete = false, reason %q, want true", reason)
		}
	})

	t.Run("only the default _SUCCESS present is not enough once a custom name is configured", func(t *testing.T) {
		// The directory contains the OLD hardcoded default, not the
		// configured name. A backup set that asked for SHA256SUMS must
		// not be satisfied by a completely different producer's _SUCCESS
		// convention showing up in the same directory by coincidence:
		// the configured name replaces the default, it does not add to
		// it.
		known := map[string]transport.RemoteArtifact{"run/_SUCCESS": {}}
		complete, reason := isComplete(a, custom, known, now)
		if complete {
			t.Errorf("only _SUCCESS present with manifest_marker=SHA256SUMS configured: complete = true, want false")
		}
		if reason == "" {
			t.Errorf("expected a non-empty reason")
		}
	})

	t.Run("neither present is not complete", func(t *testing.T) {
		complete, reason := isComplete(a, custom, map[string]transport.RemoteArtifact{}, now)
		if complete {
			t.Errorf("no marker present: complete = true, want false")
		}
		if reason == "" {
			t.Errorf("expected a non-empty reason")
		}
	})

	t.Run("an unset manifest_marker still means _SUCCESS, unchanged from before this field existed", func(t *testing.T) {
		known := map[string]transport.RemoteArtifact{"run/_SUCCESS": {}}
		complete, reason := isComplete(a, config.Completion{Strategy: "marker"}, known, now)
		if !complete {
			t.Errorf("default manifest marker: complete = false, reason %q, want true", reason)
		}
	})
}

// TestIsComplete_Stable uses a fixed now and modification times expressed
// relative to it, so the boundary is exact rather than approximately right
// on a fast machine.
//
// no_modtime is the case worth defending. A backend that reports no
// modification time gives this strategy nothing to reason from, and the
// tempting reading, "no mtime means it has not been touched recently", is
// exactly backwards: it would declare every artifact on such a backend
// complete the instant it appeared, which is the truncated-backup outcome
// the whole file exists to avoid.
func TestIsComplete_Stable(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	stableFor := config.Duration(10 * time.Minute)

	t.Run("old_enough", func(t *testing.T) {
		a := transport.RemoteArtifact{ModTime: now.Add(-time.Hour).Unix()}
		complete, reason := isComplete(a, config.Completion{Strategy: "stable", StableFor: stableFor}, nil, now)
		if !complete {
			t.Errorf("old enough object: complete = false, reason %q", reason)
		}
	})

	t.Run("too_fresh", func(t *testing.T) {
		a := transport.RemoteArtifact{ModTime: now.Add(-time.Second).Unix()}
		complete, reason := isComplete(a, config.Completion{Strategy: "stable", StableFor: stableFor}, nil, now)
		if complete {
			t.Errorf("fresh object: complete = true, want false")
		}
		if reason == "" {
			t.Errorf("expected a non-empty reason for a too-fresh object")
		}
	})

	t.Run("no_modtime", func(t *testing.T) {
		a := transport.RemoteArtifact{ModTime: 0}
		complete, reason := isComplete(a, config.Completion{Strategy: "stable", StableFor: stableFor}, nil, now)
		if complete {
			t.Errorf("object with no modtime: complete = true, want false")
		}
		if reason == "" {
			t.Errorf("expected a non-empty reason for a missing modtime")
		}
	})
}

// TestIsComplete_UnknownStrategyIsNeverComplete pins the default branch,
// which config.Validate should make unreachable and which therefore has to
// be defended by a test rather than by use.
//
// The failure it prevents is a strategy name that is added to config but not
// to isComplete, or the reverse. Falling through to "complete" would treat
// every artifact in that backup set as finished the moment it is listed; the
// refusal plus a reason means the misconfiguration shows up as every
// candidate pending with a message naming the strategy.
func TestIsComplete_UnknownStrategyIsNeverComplete(t *testing.T) {
	a := transport.RemoteArtifact{Path: "x.dump"}
	complete, reason := isComplete(a, config.Completion{Strategy: "not-a-real-strategy"}, nil, time.Now())
	if complete {
		t.Fatalf("an unrecognized strategy must never be treated as satisfied")
	}
	if reason == "" {
		t.Errorf("expected a non-empty reason for an unknown strategy")
	}
}

// discoverKey must never let two distinct (set, path) pairs collide, even
// when a backup set's own name contains characters that would ambiguate a
// naively separator-joined key. See discoverKey's doc comment.
func TestDiscoverKey_NoCrossPairCollision(t *testing.T) {
	setA := mustSet(t, "production", "postgres:a")
	setB := mustSet(t, "production", "postgres")

	keyA := discoverKey(setA, "b")
	keyB := discoverKey(setB, "a:b")

	if keyA == keyB {
		t.Fatalf("discoverKey(%v, %q) == discoverKey(%v, %q): %q; the two distinct (set,path) pairs collided",
			setA, "b", setB, "a:b", keyA)
	}
}

// TestDiscoverKey_SamePathIsDeterministic is the positive control for the
// collision test above it. The key is an idempotency key, so a second pass
// over an unchanged remote has to produce a byte-identical one or every
// artifact would be re-discovered every cycle; the no-collision test alone
// would be satisfied by a key that included a random value.
func TestDiscoverKey_SamePathIsDeterministic(t *testing.T) {
	set := mustSet(t, "production", "postgres-primary")
	if discoverKey(set, "a/b.dump") != discoverKey(set, "a/b.dump") {
		t.Fatalf("discoverKey is not deterministic for the same input")
	}
}

// mustSet builds a backup set id through the real constructor and fails the
// test if it is refused. Going through model.NewBackupSetID is what makes
// the collision test above meaningful: "postgres:a" has to be a genuinely
// legal set name for a colon-joined key to be a genuine hazard, and a
// composite literal would prove nothing about what the product accepts.
func mustSet(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

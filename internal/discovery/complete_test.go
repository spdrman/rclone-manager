package discovery

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/internal/config"
	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/transport"
)

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

func TestIsMarkerObject(t *testing.T) {
	markers := []string{"_SUCCESS", "backup.dump.complete", "x.complete"}
	for _, m := range markers {
		if !isMarkerObject(m) {
			t.Errorf("isMarkerObject(%q) = false, want true", m)
		}
	}
	notMarkers := []string{"backup.dump", "_SUCCESSFUL", "success"}
	for _, m := range notMarkers {
		if isMarkerObject(m) {
			t.Errorf("isMarkerObject(%q) = true, want false", m)
		}
	}
}

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

func TestIsComplete_Rename(t *testing.T) {
	a := transport.RemoteArtifact{Path: "x/backup.dump"}
	complete, reason := isComplete(a, config.Completion{Strategy: "rename"}, nil, time.Now())
	if !complete {
		t.Errorf("rename strategy: complete = false, reason %q, want true", reason)
	}
}

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

func TestDiscoverKey_SamePathIsDeterministic(t *testing.T) {
	set := mustSet(t, "production", "postgres-primary")
	if discoverKey(set, "a/b.dump") != discoverKey(set, "a/b.dump") {
		t.Fatalf("discoverKey is not deterministic for the same input")
	}
}

func mustSet(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

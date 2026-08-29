package app

import (
	"runtime"
	"testing"

	// Imported so this test binary's build info includes
	// github.com/rclone/rclone, exactly as cmd/backup-manager's real binary
	// does (it blank-imports internal/transport/rclone for backend
	// registration; see cmd/backup-manager/main.go). Without some test in
	// this package pulling rclone into the build closure,
	// embeddedRcloneVersion would have nothing to find and this test could
	// only ever assert "unknown", which would not prove anything.
	_ "github.com/spdrman/rclone-manager/internal/transport/rclone"
)

func TestBuildVersionInfo_ReportsEverySection(t *testing.T) {
	info := BuildVersionInfo("1.2.3", "abc123")

	if info.BinaryVersion != "1.2.3" {
		t.Errorf("BinaryVersion = %q, want %q", info.BinaryVersion, "1.2.3")
	}
	if info.Commit != "abc123" {
		t.Errorf("Commit = %q, want %q", info.Commit, "abc123")
	}
	if info.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
	}
	if info.RcloneVersion == "" || info.RcloneVersion == "unknown" {
		t.Errorf("RcloneVersion = %q, want a real pinned version (this test binary imports internal/transport/rclone precisely so this is discoverable)", info.RcloneVersion)
	}
}

package main

import (
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdVersion is FR-26's `version` command: it reports the backup-manager
// version, the embedded rclone version, the Go version and the build
// commit, in that order, exactly as the EPIC's CLI section lists them.
// See internal/app.BuildVersionInfo's doc for how the rclone version is
// obtained without this binary, or internal/app, ever importing rclone
// directly.
func cmdVersion(args []string) int {
	info := app.BuildVersionInfo(version, commit)
	fmt.Printf("backup-manager %s\n", info.BinaryVersion)
	fmt.Printf("rclone %s\n", info.RcloneVersion)
	fmt.Printf("go %s\n", info.GoVersion)
	fmt.Printf("commit %s\n", info.Commit)
	return 0
}

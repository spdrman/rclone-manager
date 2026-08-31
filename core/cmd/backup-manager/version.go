package main

import (
	"flag"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdVersion is FR-26's `version` command: it reports the backup-manager
// version, the embedded rclone version, the Go version and the build
// commit, in that order, exactly as the EPIC's CLI section lists them.
// See internal/app.BuildVersionInfo's doc for how the rclone version is
// obtained without this binary, or internal/app, ever importing rclone
// directly.
//
// # Why a flag set with no flags in it
//
// version is the one command the usage banner excludes from --config, and
// it takes no operand either. It used to have no flag set at all, which
// meant it ignored whatever it was given and succeeded, while every other
// command in this binary answers a flag it does not know with exit 2
// (issue #189). An empty flag.FlagSet, with the same ContinueOnError
// newFlagSet uses, gives version that same refusal and the same message
// shape without giving it a flag it does not want. "Does not accept
// --config" and "ignores --config" are different promises, and only the
// first one is documented: a script that guesses at `version --json` used
// to get exit 0 and human-readable text, and it is better told here than
// left to fail wherever it tries to parse that.
func cmdVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return usageError("version takes no arguments")
	}

	info := app.BuildVersionInfo(version, commit)
	fmt.Printf("backup-manager %s\n", info.BinaryVersion)
	fmt.Printf("rclone %s\n", info.RcloneVersion)
	fmt.Printf("go %s\n", info.GoVersion)
	fmt.Printf("commit %s\n", info.Commit)
	return 0
}

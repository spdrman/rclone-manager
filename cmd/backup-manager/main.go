// Command backup-manager is the entry point for every execution mode.
package main

import (
	"fmt"
	"os"
	"runtime"

	_ "github.com/spdrman/rclone-manager/internal/transport/rclone"
)

// Set at build time with -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("backup-manager %s\ncommit %s\ngo %s\n", version, commit, runtime.Version())
		return
	}
	fmt.Fprintln(os.Stderr, "backup-manager: no command given")
	os.Exit(2)
}

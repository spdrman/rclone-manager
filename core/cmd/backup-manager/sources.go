package main

import (
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// cmdSources is `backup-manager sources`: a read-only dump of every
// configured source and backup set. It never opens the state journal or
// touches a remote, since internal/app.Service.Sources reads only Config.
func cmdSources(args []string) int {
	fs, cfgPath := newFlagSet("sources")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.LoadAndValidate(*cfgPath)
	if err != nil {
		return fail(fmt.Errorf("config: %w", err))
	}

	svc := app.New(cfg, nil, nil, nil)
	for _, src := range svc.Sources() {
		fmt.Printf("%s\n", src.Name)
		for _, bs := range src.BackupSets {
			status := "enabled"
			if bs.Disabled {
				status = "disabled"
			}
			// Issue #316: read-only-ness is a second, independent axis
			// from enabled/disabled (a set can be read-only and still
			// run, discovering and transferring new artifacts, just
			// never deleting the remote original), so it is appended
			// rather than folded into status above.
			if bs.ReadOnly {
				status += ",read_only"
			}
			fmt.Printf("  %-40s remote=%-6s remote_path=%-30s local_path=%-30s stale_after=%-8s status=%s\n",
				bs.ID, bs.RemoteType, bs.RemotePath, bs.LocalPath, bs.StaleAfter, status)
		}
	}
	return 0
}

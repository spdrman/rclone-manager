package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// cmdSources is `backup-manager sources`: a read-only dump of every
// configured source and backup set. It never opens the state journal or
// touches a remote, since internal/app.Service.Sources reads only Config.
//
// # This is the command #535 was reported through
//
// It listed two backup sets while the Web UI showed none, because it reads
// config.yaml now and the engine read it when it started. So it is a pure
// configuration read, which readmode.go's doc calls the place the
// divergence actually lives, and it is the first surface here to get a
// mode: it says which world this list is about, and beside an engine
// holding a different configuration it refuses rather than printing one
// that deployment does not have.
func cmdSources(args []string) int {
	fs, cfgPath := newFlagSet("sources")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.LoadAndValidate(*cfgPath)
	if err != nil {
		return fail(fmt.Errorf("config: %w", err))
	}

	ctx := context.Background()
	mode, err := enterReadMode(ctx, *cfgPath, cfg, os.Stderr)
	if err != nil {
		return fail(err)
	}

	svc := app.New(cfg, nil, nil, nil)
	sources := svc.Sources()

	// Asked before anything is printed, and asked about every set at once
	// rather than per line: a listing that printed three correct rows and
	// then refused would have already shown an operator a world the
	// serving process does not have.
	var mine []string
	for _, src := range sources {
		for _, bs := range src.BackupSets {
			mine = append(mine, describeBackupSet(bs.ID.String(), bs.Disabled, bs.ReadOnly))
		}
	}
	if err := mode.agreeOnBackupSets(ctx, mine); err != nil {
		return fail(err)
	}

	for _, src := range sources {
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

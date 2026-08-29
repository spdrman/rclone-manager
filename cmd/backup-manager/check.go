package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/internal/app"
)

// cmdCheck is `backup-manager check`: a pre-flight answer to "can this
// deployment actually start" (see internal/app.Check's doc for exactly
// what it validates and, just as importantly, what it deliberately does
// not: it never contacts a configured remote).
func cmdCheck(args []string) int {
	fs, cfgPath := newFlagSet("check")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := app.Check(context.Background(), *cfgPath)
	if err != nil {
		return fail(err)
	}

	sets := 0
	for _, src := range cfg.Sources {
		sets += len(src.BackupSets)
	}
	fmt.Printf("config OK: %d source(s), %d backup set(s)\n", len(cfg.Sources), sets)
	fmt.Printf("state database OK: %s\n", cfg.State.Database)
	return 0
}

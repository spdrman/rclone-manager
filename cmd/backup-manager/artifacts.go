package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/internal/app"
)

// cmdArtifacts is `backup-manager artifacts`: every journal record for
// every backup set --source/--backup-set select (both optional; omitting
// either widens the filter, see internal/app.ArtifactFilter's doc).
func cmdArtifacts(args []string) int {
	fs, cfgPath := newFlagSet("artifacts")
	sourceFlag := fs.String("source", "", "only artifacts from this source")
	setFlag := fs.String("backup-set", "", "only artifacts from this backup set")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	records, err := svc.ListArtifacts(ctx, app.ArtifactFilter{Source: *sourceFlag, Set: *setFlag})
	if err != nil {
		return fail(err)
	}

	for _, r := range records {
		fmt.Printf("%-60s %-22s remote=%-40s local=%s\n", r.Artifact, r.State, r.RemotePath, r.LocalPath)
	}
	fmt.Printf("%d artifact(s)\n", len(records))
	return 0
}

package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdValidate is `backup-manager validate <source/backup-set/artifact>`:
// an on-demand re-check of one already-committed artifact's durable copy,
// wherever that copy actually is. See internal/app.ValidateArtifact's doc
// for exactly what it checks and why a failure quarantines the artifact
// with no --dry-run guard (the consequence is protective, never
// destructive).
//
// # Why this one opens a transport and the other read-only commands do not
//
// openService's withTransport argument is what fills in
// Service.MediumStore, because FR-28's medium boundary IS the embedded
// rclone adapter (see app.Service.MediumStore). Since issue #435 this
// command can be asked about an artifact whose only durable copy is an
// object on a storage medium, and without that adapter it has nothing to
// ask, so it would refuse every moved artifact in every deployment,
// forever, for a reason no operator could act on. rclone.New() allocates
// an empty struct and opens no connection, so a `validate` against a
// local copy pays nothing for holding one.
func cmdValidate(args []string) int {
	fs, cfgPath := newFlagSet("validate")
	// --content is FR-31's operator-initiated content check. It is a flag
	// rather than the default because it downloads the object from the
	// storage medium, and egress is a bill; see app.ValidateOptions.
	content := fs.Bool("content", false,
		"re-verify a copy on a storage medium at the content class: download the object and re-hash it against the hash recorded at ingestion. This costs egress. A local copy is always content-checked, so this changes nothing for one")
	// Flags may come before or after the operand; see
	// parseFlagsAroundOperands in setup.go for why.
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 1 {
		return usageError("validate takes exactly one argument: <source/backup-set/artifact>")
	}

	id, err := app.ParseArtifactID(operands[0])
	if err != nil {
		return fail(err)
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	result, err := svc.ValidateArtifact(ctx, id, app.ValidateOptions{Content: *content})
	if err != nil {
		return fail(err)
	}

	fmt.Printf("%s: checked=%v passed=%v\n  %s\n", id, result.Checked, result.Passed, result.Reason)
	if result.NewState != "" {
		fmt.Printf("  -> %s\n", result.NewState)
	}
	if !result.Passed {
		return 1
	}
	return 0
}

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
)

// cmdArtifacts is `backup-manager artifacts`.
//
// With no operand it lists every journal record for every backup set
// --source/--backup-set select (both optional; omitting either widens the
// filter, see internal/app.ArtifactFilter's doc): the terse, one-line-per-
// artifact form an operator scans to see what state everything is in.
//
// With exactly one operand, <source/backup-set/name> (the same id form
// `validate` takes), it switches to a detail view of that one artifact:
// every field api/v1/openapi.json's Artifact schema exposes, printed the
// way the API names it, plus one the API does not have at all: the
// literal diagnostic sentence internal/lifecycle recorded on the
// transition that left a FAILED, QUARANTINED or QUARANTINED_LOST artifact
// where it is (issue #284). That sentence lives only in the journal's
// append-only transition log (state_transitions.detail); until this
// existed, reading it required opening the state database by hand with
// sqlite3.
func cmdArtifacts(args []string) int {
	fs, cfgPath := newFlagSet("artifacts")
	sourceFlag := fs.String("source", "", "only artifacts from this source")
	setFlag := fs.String("backup-set", "", "only artifacts from this backup set")
	// Flags may come before or after the operand, exactly like validate's
	// own operand (see parseFlagsAroundOperands in setup.go for why).
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) > 1 {
		return usageError("artifacts takes at most one argument: <source/backup-set/name>")
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	if len(operands) == 1 {
		if *sourceFlag != "" || *setFlag != "" {
			return usageError("artifacts: --source/--backup-set cannot be combined with an artifact argument")
		}
		id, err := app.ParseArtifactID(operands[0])
		if err != nil {
			return fail(err)
		}
		detail, err := svc.GetArtifactDetail(ctx, id)
		if err != nil {
			return fail(err)
		}
		printArtifactDetail(detail)
		return 0
	}

	// The unfiltered list is the terminal's Backups page, and it carries
	// the artifacts of sets whose configuration has since been removed
	// (issue #391), which is what `backup-set remove` tells the operator
	// this command will still list. The app layer honours the flag only
	// for a filter naming nothing.
	records, err := svc.ListArtifacts(ctx, app.ArtifactFilter{Source: *sourceFlag, Set: *setFlag, IncludeUnconfigured: true})
	if err != nil {
		return fail(err)
	}

	for _, r := range records {
		fmt.Printf("%-60s %-22s remote=%-40s local=%s\n", r.Artifact, r.State, r.RemotePath, r.LocalPath)
	}
	fmt.Printf("%d artifact(s)\n", len(records))
	return 0
}

// printArtifactDetail renders d field by field against
// api/v1/openapi.json's Artifact schema (issue #284's acceptance
// criterion: the CLI's per-artifact output and the API schema compared
// field by field, with any gap either closed or written down as
// deliberate).
//
// Every field that schema marks required is printed unconditionally, in
// its own order; every field it marks `x-go-omitempty` is printed only
// when non-empty/non-zero, exactly matching when the API itself would
// omit it, with one deliberate exception: `quarantine_reason`, the
// schema's own LastError-falling-back-to-ValidationDetail guess, is never
// printed here at all. It reimplements exactly what issue #308 already
// flags as unreliable in core/service.toServiceArtifact -- often empty,
// and, worse, sometimes non-empty and wrong, since LastError only ever
// reflects a *previous* quarantine's release, not the one currently in
// effect -- and this command already prints something strictly better one
// field below: `reason`/`reason_at`, the literal sentence
// internal/lifecycle recorded on the transition that produced the
// artifact's *current* state, read from the journal directly rather than
// reconstructed. Printing both risked showing an operator two disagreeing
// explanations for the same quarantine with no way to tell which to
// trust; closing that gap on the API side too is issue #308, bigger than
// this command.
//
// `reason`/`reason_at` themselves are NOT in that schema at all: they are
// this command's whole reason for existing (issue #284).
func printArtifactDetail(d app.ArtifactDetail) {
	rec := d.Record
	const layout = time.RFC3339

	fmt.Printf("id:                  %s\n", rec.Artifact)
	fmt.Printf("backup_set_id:       %s\n", rec.Artifact.Set)
	fmt.Printf("source_name:         %s\n", rec.Artifact.Set.Source)
	fmt.Printf("set_name:            %s\n", rec.Artifact.Set.Set)
	fmt.Printf("name:                %s\n", rec.Artifact.Name)
	fmt.Printf("state:               %s\n", rec.State)
	fmt.Printf("remote_path:         %s\n", rec.RemotePath)
	fmt.Printf("local_path:          %s\n", rec.LocalPath)
	fmt.Printf("discovered_at:       %s\n", rec.DiscoveredAt.Format(layout))
	fmt.Printf("updated_at:          %s\n", rec.UpdatedAt.Format(layout))

	var sizeBytes int64
	if rec.Remote.Size != nil {
		sizeBytes = *rec.Remote.Size
	}
	fmt.Printf("size_bytes:          %d\n", sizeBytes)

	if rec.LocalHash != "" {
		fmt.Printf("checksum:            %s\n", rec.LocalHash)
		fmt.Printf("checksum_algorithm:  %s\n", rec.LocalHashAlg)
	}

	fmt.Printf("validation:          %s\n", validationString(rec.ValidationPassed))
	if rec.ValidationDetail != "" {
		fmt.Printf("validation_detail:   %s\n", rec.ValidationDetail)
	}

	if rec.RemoteDeletedAt != nil {
		fmt.Printf("remote_source_removed_at: %s\n", rec.RemoteDeletedAt.Format(layout))
	}

	quarantined, irrecoverable := quarantineFlags(lifecycle.State(rec.State))
	fmt.Printf("quarantined:         %v\n", quarantined)
	fmt.Printf("quarantine_irrecoverable: %v\n", irrecoverable)

	if rec.RetentionTier != "" {
		fmt.Printf("retention_tier:      %s\n", rec.RetentionTier)
	}

	if d.FailureReason != "" {
		fmt.Printf("reason:              %s\n", d.FailureReason)
		fmt.Printf("reason_at:           %s\n", d.FailureReasonAt.Format(layout))
	}
}

// validationString renders state.Record.ValidationPassed as the same
// "passed"/"failed"/"pending" tri-state core/service.Artifact.Validation
// uses, so the two read surfaces agree on vocabulary as well as on which
// states exist.
func validationString(passed *bool) string {
	switch {
	case passed == nil:
		return "pending"
	case *passed:
		return "passed"
	default:
		return "failed"
	}
}

// quarantineFlags mirrors core/service's toServiceArtifact: quarantined is
// true for either quarantine state, and irrecoverable narrows that to
// QUARANTINED_LOST, the one with no remote source left to re-ingest from.
func quarantineFlags(st lifecycle.State) (quarantined, irrecoverable bool) {
	switch st {
	case lifecycle.Quarantined:
		return true, false
	case lifecycle.QuarantinedLost:
		return true, true
	default:
		return false, false
	}
}

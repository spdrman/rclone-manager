package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// cmdArtifacts is `backup-manager artifacts`.
//
// With no operand it lists every journal record for every backup set
// --source/--backup-set select (both optional; omitting either widens the
// filter, see internal/app.ArtifactFilter's doc): the terse, one-line-per-
// artifact form an operator scans to see what state everything is in.
//
// --backup-set takes the "source/backup-set" id `sources`, `status` and
// `retention` name a backup set by, and a bare set name where exactly one
// source configures it (issue #569). Both spellings resolve in one place,
// in the app layer, so this command and the id it hands a running engine
// cannot end up disagreeing about which backup set was asked for.
//
// Not the id in this listing's own first column, which is a whole ARTIFACT
// id and one field longer. That is worth naming here rather than leaving
// implicit, because the note beside this flag used to point at exactly
// that column and it was the one string an operator must not paste in.
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
	setFlag := fs.String("backup-set", "", "only artifacts from this backup set, named <source/backup-set> or by set name alone where that is unambiguous")
	// Flags may come before or after the operand, exactly like validate's
	// own operand (see parseFlagsAroundOperands in setup.go for why).
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) > 1 {
		return usageError("artifacts takes at most one argument: <source/backup-set/name>")
	}
	// Issue #569. --backup-set takes the whole "source/backup-set" id now,
	// so it can carry a source of its own. Two things about that pair are
	// settled here, on the command line alone and before anything is
	// opened, because both are wrong on every deployment rather than on
	// this one, and the usage block's table gives that a 2: a value with
	// a separator in it that is not an id at all, and a --source naming a
	// different source than the id does. What is NOT settled here is
	// whether either name exists, which is a question about this
	// deployment and belongs where the configuration is
	// (internal/app.ArtifactFilter.resolve), on the table's row 1.
	//
	// The shape check goes through splitBackupSetID, the same function
	// `retention`, `backup-set create/patch/remove/retention` and
	// `unconfigured clear` all read this operand's shape with, so every
	// place an operator can type a backup set id agrees about what one is
	// and refuses the same things the same way. It used to reach
	// internal/app and come back through fail() as a 1 carrying "app:
	// backup set ...", which is a sentence about this deployment printed
	// after its journal was opened, for a command line no deployment
	// would have accepted.
	if named, _, ok := splitBackupSetID(*setFlag); ok {
		if *sourceFlag != "" && *sourceFlag != named {
			return usageError("artifacts: --backup-set %s names source %s, which --source %s contradicts", *setFlag, named, *sourceFlag)
		}
	} else if strings.Contains(*setFlag, "/") {
		return usageError("artifacts: --backup-set %q is not a backup set id; a backup set id is exactly source/name, and an artifact id pasted whole has the file name on the end of it", *setFlag)
	}

	ctx := context.Background()
	svc, cfg, cleanup, err := openService(ctx, *cfgPath, false)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	// Issue #544. Every row below is a journal row, and the journal is
	// read live by both processes, so the rows themselves cannot diverge.
	// What can is which backup sets are configured, which is what decides
	// whether a row is marked as belonging to one whose configuration was
	// removed, and that marker is the difference between "kept under
	// this deployment's retention chain" and "kept because nothing is
	// looking any more".
	mode, err := enterReadMode(ctx, *cfgPath, cfg, os.Stderr)
	if err != nil {
		return fail(err)
	}

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
		if err := mode.agreeOnArtifact(ctx, id, detail.Artifact.String()+" "+detail.State); err != nil {
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
	filter := app.ArtifactFilter{Source: *sourceFlag, Set: *setFlag, IncludeUnconfigured: true}
	records, err := svc.ListArtifacts(ctx, filter)
	if err != nil {
		return fail(err)
	}

	// The engine is asked about exactly the rows this listing is about
	// when the filter names one backup set the contract can express, and
	// about the whole journal otherwise. GET /backups filters on a
	// "source/set" id and on nothing else, so `--source` alone has no wire
	// equivalent; comparing the unfiltered listings in that case is a
	// wider question than the command asked, which is the safe direction
	// to be wrong in.
	filterID := filter.ResolvedSetID(cfg.Sources)
	compare := records
	if filterID == "" && (*sourceFlag != "" || *setFlag != "") {
		compare, err = svc.ListArtifacts(ctx, app.ArtifactFilter{IncludeUnconfigured: true})
		if err != nil {
			return fail(err)
		}
	}
	var mine []string
	for _, r := range compare {
		mine = append(mine, r.Artifact.String()+" "+r.State)
	}
	if err := mode.agreeOnArtifacts(ctx, filterID, mine); err != nil {
		return fail(err)
	}

	// Issue #418. Showing a removed set's backups here without saying so
	// is what this list has been doing since #391 widened it: they read
	// as ordinary rows of an ordinary backup set, and an operator cannot
	// tell "kept under the deployment's retention chain" from "kept
	// because nothing is looking any more". The marker is per row rather
	// than a footnote alone, because a footnote on a four-hundred-row
	// list does not tell anyone WHICH rows it is about.
	//
	// The marker only ever appears on those rows, so a deployment that
	// has never removed a backup set prints exactly what it printed
	// before. That keeps this command's pinned cases in
	// spdrman/rclone-manager-tests (suites/cli/) unchanged.
	ungoverned, err := unconfiguredSetIDs(ctx, svc)
	if err != nil {
		return fail(err)
	}

	for _, r := range records {
		marker := ""
		if ungoverned[r.Artifact.Set.String()] {
			marker = "  [configuration removed: no retention policy]"
		}
		fmt.Printf("%-60s %-22s remote=%-40s local=%s%s\n", r.Artifact, r.State, r.RemotePath, r.LocalPath, marker)
	}
	fmt.Printf("%d artifact(s)\n", len(records))
	if len(ungoverned) > 0 && listedAny(records, ungoverned) {
		fmt.Println("marked rows belong to backup sets whose configuration was removed: nothing retains, reconciles or")
		fmt.Println("advances them. `backup-manager unconfigured` says what they hold and what can be done about it.")
	}
	return 0
}

// unconfiguredSetIDs is the set of backup set ids the journal remembers
// and the configuration does not, as a lookup for the listing above.
func unconfiguredSetIDs(ctx context.Context, svc *app.Service) (map[string]bool, error) {
	sets, err := svc.UnconfiguredSets(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(sets))
	for _, u := range sets {
		out[u.Set.String()] = true
	}
	return out, nil
}

// listedAny reports whether any row actually printed carries the marker,
// so the explanation below the list is not offered for rows a --source or
// --backup-set filter left out.
func listedAny(records []state.Record, ungoverned map[string]bool) bool {
	for _, r := range records {
		if ungoverned[r.Artifact.Set.String()] {
			return true
		}
	}
	return false
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
//
// The second deliberate omission is `retention_policy` (issue #523),
// which says whether any retention chain still selects this artifact or
// whether its backup set's configuration was removed and nothing will
// ever delete it. It is absent here and it is NOT a gap in what a
// terminal can learn: the LIST form above marks exactly those rows and
// prints the same explanation under the list, `unconfigured` gives the
// whole picture per set, and `status` and `retention` both name it. What
// is missing is only this one per-artifact line, and it is missing for a
// reason worth stating rather than fixing quietly: this surface is
// compared line for line by core/tests/compat's `06-cli-surfaces` cell,
// whose own text is that FR-35 "allows this surface no additive column
// either". An extra line here is a re-capture of that corpus and a
// justification about upgrading operators, which is a decision of its
// own rather than a detail of issue #523, and #523's boundary was the
// HTTP API and the Web UI, neither of which had ANY way to tell the two
// kinds of backup apart.
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

	printArtifactCopies(d.Copies, layout)
}

// printArtifactCopies prints one block per durable copy, with the access
// state FR-34 defines.
//
// # Why a terminal operator gets this at all
//
// FR-34 says the CLI mirrors the same vocabulary as the UI, so that a
// person on a terminal and a person in a browser read the same truth about
// the same artifact. The truth that matters here is the one an archive
// class introduces: a copy can be durable, intact, and completely out of
// reach for the next several hours, and an operator who learns that during
// a restore rather than before one has learned it too late.
//
// It prints nothing at all when the artifact has one ordinary local copy
// and nothing else, which is every artifact in every deployment that has
// not configured a storage medium. That is FR-35's compatibility promise
// kept literally: an additive column that renders only when there is
// something additive to say.
func printArtifactCopies(copies []app.ArtifactCopy, layout string) {
	if !worthPrinting(copies) {
		return
	}
	for _, c := range copies {
		fmt.Printf("copy:                %s\n", c.Medium)
		fmt.Printf("  location:          %s\n", c.Location)
		fmt.Printf("  status:            %s\n", c.Status)
		fmt.Printf("  access:            %s\n", c.Access)
		if c.StorageClass != "" {
			fmt.Printf("  storage_class:     %s\n", c.StorageClass)
		}
		if c.VerificationClass != "" {
			fmt.Printf("  verified_as:       %s\n", c.VerificationClass)
		} else {
			fmt.Printf("  verified_as:       nothing has verified this copy\n")
		}
		if c.VerifiedAt != nil {
			fmt.Printf("  verified_at:       %s\n", c.VerifiedAt.Format(layout))
		}
		if c.CheckableAs != "" {
			fmt.Printf("  checkable_as:      %s\n", c.CheckableAs)
		} else {
			fmt.Printf("  checkable_as:      nothing, while this copy's medium is not answering\n")
		}
		if c.RetrievalBilled {
			fmt.Printf("  retrieval:         the provider bills to read this copy back; this product holds no price list and will not guess an amount\n")
		}
		if c.Detail != "" {
			fmt.Printf("  note:              %s\n", c.Detail)
		}
	}
}

// worthPrinting reports whether these copies say anything the lines above
// have not already said.
//
// One ACTIVE local copy is what local_path already printed, so repeating
// it as a block would be noise on every artifact of every deployment that
// never configured a medium. Anything else is worth a block: a copy
// somewhere other than local disk, a local copy that is no longer ACTIVE,
// or more than one copy at once, which is what an artifact mid-move looks
// like.
func worthPrinting(copies []app.ArtifactCopy) bool {
	if len(copies) == 0 {
		return false
	}
	if len(copies) > 1 {
		return true
	}
	only := copies[0]
	return only.Medium != state.MediumLocal || only.Status != state.PlacementActive
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

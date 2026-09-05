package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/reconcile"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// One backup set's share of a cycle, on an operator's word rather than a
// schedule.
//
// A real Fetch is not a smaller kind of cycle, it is the same cycle narrowed
// to one set: the same reconcile, the same discovery, the same walk over the
// same journal rows, through the same functions RunCycle calls. That is what
// makes it safe to run beside a daemon and what keeps `fetch` from being a
// second implementation of the pipeline that drifts.
//
// --dry-run is where the two genuinely part company, and it costs something
// worth knowing about. A dry run must not write, and discovery writes, so it
// cannot call discovery at all: it lists the remote and cross-references the
// journal instead. Every line it prints is a real object the remote reported
// just now, but the completion strategies, the include patterns and the
// producer-temp-name rules all live unexported inside internal/discovery, so
// the preview cannot say which of those objects discovery would actually
// take. It says what is there and whether the journal has seen it, and Fetch's
// own doc is explicit that this is coarser rather than pretending otherwise.
//
// FetchResult.Verdict (cycleoutcome.go) is what turns any of this into an
// exit status. A caller that checks only Reconcile.Errors and
// Discovery.Errors is asking whether the pass was systemically broken, which
// is a different question from whether a backup happened.

// FetchPreviewEntry is one remote object `fetch --dry-run` saw, before any
// decision about it is recorded anywhere.
type FetchPreviewEntry struct {
	RemotePath string
	Size       int64

	// Known reports whether this exact remote path already has a journal
	// row (from an earlier, real fetch or from the daemon's own regular
	// discovery), i.e. whether a real fetch would expect this object to
	// land in discovery.Result.AlreadyKnown rather than .Discovered.
	Known bool
}

// FetchResult is `backup-manager fetch`'s use case output: either a
// dry-run preview (Preview populated, everything else zero) or a real,
// on-demand run of one specific backup set's whole cycle share
// (Reconcile/Discovery populated, Preview nil).
type FetchResult struct {
	Set model.BackupSetID

	DryRun bool

	// Preview is set only when DryRun is true.
	Preview []FetchPreviewEntry

	// Reconcile and Discovery are set only when DryRun is false.
	Reconcile reconcile.Report
	Discovery discovery.Result

	// FailedArtifacts is set only when DryRun is false: how many of this
	// backup set's journal rows this call walked through processArtifacts
	// (internal/app/pipeline.go) ended in FAILED, QUARANTINED or
	// QUARANTINED_LOST. This is the other half of "did this fetch actually
	// succeed" (issue #283) alongside Reconcile.Errors/Discovery.Errors:
	// an artifact that discovers and reconciles cleanly and then fails
	// transfer, verification or commit is counted in neither of those, so
	// a caller that checked only them could report success for a cycle
	// that backed up nothing at all. It also covers a loss Reconcile
	// (above) discovered entirely on its own -- a previously-durable
	// artifact whose local copy is found corrupted or missing -- since
	// processArtifacts lists the journal after reconcileOne has already
	// written that verdict (see processArtifacts's own doc): a successful
	// reconciliation pass finding rot is not a systemic error, but it
	// must still count as this fetch failing. A dry-run never sets it,
	// honestly: --dry-run looks at the remote, never at the journal's
	// per-artifact outcomes, so it has nothing to report here.
	FailedArtifacts int

	// Progress is issue #361's count of what this fetch actually
	// achieved (see CycleProgress). It comes from the same walk `run`
	// counts, so the two commands cannot disagree about whether a cycle
	// got anything through. A dry-run never sets it, for the same reason
	// it never sets FailedArtifacts.
	Progress CycleProgress
}

// Fetch is `backup-manager fetch --source ... --backup-set ...`'s use
// case: an operator-triggered, on-demand run of exactly one backup set's
// share of the same cycle RunCycle performs for every configured backup
// set (reconcile, then discover, then drive every in-flight artifact
// forward), for the case where waiting for the next scheduled `daemon`
// cycle, or running a whole extra `run` invocation across every backup
// set, is not what the operator wants.
//
// # --dry-run does not touch the journal at all
//
// A real Fetch calls internal/discovery.Discover, which durably records
// every newly complete candidate as DISCOVERED (an additive, non-
// destructive write, but a write nonetheless), and then drives whatever is
// already in flight all the way through transfer/verify/commit/delete.
// --dry-run is meant as a safe look before that, so it does neither: it
// lists the remote directly (transport.Transport.List) and cross-
// references each object's path against what the journal already knows
// for this backup set, without ever calling discovery.Discover or any
// lifecycle step. This is coarser than discovery's own real
// completion-strategy evaluation (isProducerTempName, include-pattern
// matching and the three completion strategies in
// internal/discovery/complete.go are unexported, so this package cannot
// reuse them without a change to internal/discovery, out of this
// package's file scope; see this package's introducing PR description),
// but it is honest: every FetchPreviewEntry is a real object the
// configured remote reported, right now, and Known accurately reflects
// whether the journal has already seen that exact path.
func (s *Service) Fetch(ctx context.Context, sourceName, setName string, dryRun bool) (FetchResult, error) {
	src, bs, err := s.lookupBackupSet(sourceName, setName)
	if err != nil {
		return FetchResult{}, err
	}
	source := sourceFor(s.Config, src, bs)

	if dryRun {
		return s.fetchDryRun(ctx, source, bs.ID)
	}

	result := FetchResult{Set: bs.ID}

	recRep, err := s.reconcileOne(ctx, source, bs.ID)
	result.Reconcile = recRep
	if err != nil {
		return result, fmt.Errorf("app: fetch: reconcile: %w", err)
	}

	discRes, err := s.discoverOne(ctx, source, bs)
	result.Discovery = discRes
	if err != nil {
		return result, fmt.Errorf("app: fetch: discover: %w", err)
	}

	records, err := s.Journal.ListByBackupSet(ctx, bs.ID)
	if err != nil {
		return result, fmt.Errorf("app: fetch: listing %s: %w", bs.ID, err)
	}
	walk := s.processArtifacts(ctx, source, bs, records)
	result.FailedArtifacts = walk.Failed
	// Exactly the arithmetic RunCycle does, through exactly the same
	// function over exactly the same walk (issue #361), so the two
	// commands cannot report different numbers for the same cycle.
	result.Progress = foldDiscoveryErrors(walk, discRes)
	s.reportBarrenSet(ctx, result.Verdict())

	return result, nil
}

func (s *Service) fetchDryRun(ctx context.Context, source transport.Source, set model.BackupSetID) (FetchResult, error) {
	if s.Transport == nil {
		return FetchResult{}, fmt.Errorf("app: fetch --dry-run needs a Transport")
	}

	listed, err := s.Transport.List(ctx, source)
	if err != nil {
		return FetchResult{}, fmt.Errorf("app: fetch --dry-run: listing %s: %w", set, err)
	}

	known, err := s.Journal.ListByBackupSet(ctx, set)
	if err != nil {
		return FetchResult{}, fmt.Errorf("app: fetch --dry-run: listing journal for %s: %w", set, err)
	}
	knownPaths := make(map[string]bool, len(known))
	for _, rec := range known {
		knownPaths[rec.RemotePath] = true
	}

	result := FetchResult{Set: set, DryRun: true}
	for _, a := range listed {
		result.Preview = append(result.Preview, FetchPreviewEntry{
			RemotePath: a.Path,
			Size:       a.Size,
			Known:      knownPaths[a.Path],
		})
	}
	return result, nil
}

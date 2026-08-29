package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/reconcile"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

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
	source := sourceFor(src, bs)

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
	for _, rec := range records {
		if ctx.Err() != nil {
			break
		}
		s.processArtifact(ctx, source, bs, rec)
	}

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

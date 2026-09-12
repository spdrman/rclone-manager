package app

import (
	"context"
	"fmt"

	"github.com/backupdproject/backupd/core/internal/discovery"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/reconcile"
	"github.com/backupdproject/backupd/core/internal/transport"
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

	// State is the journal state of the row this object already has, and
	// empty when Known is false.
	//
	// It is here because of issue #662, whose operator's whole question
	// was "why does running it do nothing". All 28 objects came back
	// "(already known)", the plan was empty, and the exit code was 0,
	// over a set with three FAILED artifacts that no cycle will ever
	// re-attempt. "Already known" was true of every one of them and told
	// the truth about none of them: a settled backup set and a stuck one
	// printed identically. The state is what separates the two, this
	// function is already holding the rows it comes off, and a dry run's
	// entire job is to say what a real run would do.
	State string
}

// FetchResult is `backupd fetch`'s use case output: either a
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

// Fetch is `backupd fetch --source ... --backup-set ...`'s use
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

	// EPIC K (#780): the same refusal processBackupSet makes, at the other
	// entry point into this package's artifact pipeline.
	//
	// Fetch is not a shortcut into a cycle, it is a second, equal way in:
	// it calls reconcileOne, discoverOne and processArtifacts itself, so a
	// guard that only sat in the cycle's loop left `backupd fetch` (and
	// the fetch action on the API, and the button in the web UI) walking an
	// incremental set's source TREE and offering its files for deletion.
	// One operator click, the outcome EPIC K forbids.
	//
	// It is refused BEFORE the --dry-run branch, which lists the remote and
	// deletes nothing, because a preview that presented a source tree's
	// files as artifact candidates would be answering a question nobody can
	// act on -- and it would walk the tree to do it.
	if err := unrunnableEngine(bs.Engine); err != nil {
		// Returned unwrapped, unlike every other error below it. Those
		// wrap because they have to say WHICH step failed; here no step
		// ran, and "app: fetch: app: this build has no pipeline..." would
		// stutter the package name at an operator to say less.
		return FetchResult{Set: bs.ID}, err
	}

	source := sourceFor(s.Config, src, bs)

	if dryRun {
		return s.fetchDryRun(ctx, source, bs.ID)
	}

	// Live progress and the per-set feed, for a caller that installed an
	// observer (progress.go). Nothing here changes what `backupd
	// fetch` does in its own process: with no observer on ctx, beginCycle
	// returns ctx unchanged and every call below is a nil-receiver no-op.
	//
	// The denominator is 1 and is known before anything starts, which is
	// the one place a per-set run is honestly better off than a cycle:
	// Progress's own doc explains why a cycle cannot have one.
	//
	// enterSet is what puts this set's id on every reading, and that is
	// load-bearing rather than cosmetic. The per-set activity feed keys on
	// it, so a run whose readings carry no set id lands in no set's
	// terminal at all (issue #597).
	ctx = beginOneSetCycle(ctx, bs.ID.String())
	prog := progressFrom(ctx)

	result := FetchResult{Set: bs.ID}
	defer func() {
		prog.finishSet()
		if o, ok := ProgressObserverFrom(ctx).(SetOutcomeObserver); ok {
			o.ObserveSetOutcome(bs.ID.String(), result.Outcome())
		}
	}()

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
	knownState := make(map[string]string, len(known))
	for _, rec := range known {
		knownState[rec.RemotePath] = rec.State
	}

	result := FetchResult{Set: set, DryRun: true}
	for _, a := range listed {
		st, isKnown := knownState[a.Path]
		result.Preview = append(result.Preview, FetchPreviewEntry{
			RemotePath: a.Path,
			Size:       a.Size,
			Known:      isKnown,
			State:      st,
		})
	}
	return result, nil
}

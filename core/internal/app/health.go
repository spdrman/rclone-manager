package app

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/health"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// BuildHealthReport is `backup-manager status`' use case (FR-24). It calls
// internal/health.ComputeBackupSetHealth once per configured backup set,
// against that set's freshly-loaded journal rows, and bundles the result
// with the process-liveness half of FR-24 built from versionInfo.
//
// # Two of the injected inputs are always empty for a one-shot command
//
// internal/health.BackupSetInputs asks for LastSuccessfulPollAt and
// LastRetentionRunAt, and this package does track both, in memory, for
// backup sets a *running* Service has itself already cycled through (see
// app.go's recordSuccessfulPoll/recordRetentionRun, called from cycle.go).
// A `status` invocation is normally its own short-lived process, though,
// entirely separate from whatever `run` or `daemon` process last actually
// touched a backup set, and neither timestamp is persisted anywhere in the
// FR-9 journal schema. So in the common case, a freshly-constructed
// Service reports both as nil here, honestly reflecting "unknown from this
// process's own history" rather than fabricating a value.
//
// See this package's introducing PR description for the follow-up this
// implies: persisting a last-poll and last-retention-run timestamp
// somewhere durable (a small dedicated table, or per-backup-set columns)
// is a real gap this package cannot close on its own, since it would mean
// changing internal/state's schema, which is out of this package's file
// scope.
//
// FreeBytes, by contrast, never depends on process history: it is a live
// capacity.StatPath reading against the backup set's configured LocalPath,
// taken fresh on every call, exactly the way FR-24 names "free space" as
// something to be reported, not remembered.
//
// HaltReason is the fourth injected input and the only one that IS
// persisted (issue #245): it comes from internal/state's own
// backup_set_halts rows, so a `status` invocation in a separate process
// reports the same refusal the daemon recorded, which is exactly what the
// follow-up above asks for and does not yet do for the two timestamps.
func (s *Service) BuildHealthReport(ctx context.Context, versionInfo VersionInfo) (health.Report, error) {
	now := s.now()
	process := health.NewProcessHealth(health.ProcessInputs{
		BinaryVersion: versionInfo.BinaryVersion,
		RcloneVersion: versionInfo.RcloneVersion,
	})

	// Every backup set currently carrying a connection refusal, read once
	// for the whole report rather than per set (issue #245).
	//
	// A failure here fails the whole report, the same way the
	// reinstatement history below does and unlike FreeBytes. The
	// reassuring answer is "no set is refused", and a database that could
	// not be asked must not produce the same output as a deployment where
	// everything connects: that collapse is the exact defect this field
	// exists to end.
	halts, err := s.Journal.ListBackupSetHalts(ctx)
	if err != nil {
		return health.Report{}, fmt.Errorf("app: health: connection refusals: %w", err)
	}
	haltReasons := make(map[model.BackupSetID]string, len(halts))
	for _, h := range halts {
		haltReasons[h.Set] = h.Reason
	}

	// Every relocation this journal still has open, indexed by backup
	// set, loaded at most once for the whole report and only if some set
	// actually needs it. See placementEvidence and movesBySet for why it
	// is lazy: a deployment that has never declared a storage medium must
	// not start requiring a journal that can answer a question it will
	// never ask.
	var moves movesBySet

	var sets []health.BackupSetHealth
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			records, err := s.Journal.ListByBackupSet(ctx, bs.ID)
			if err != nil {
				return health.Report{}, fmt.Errorf("app: health: listing %s: %w", bs.ID, err)
			}

			// The reinstatement history (issue #227), read once for the
			// whole set from the append-only transition log, via the same
			// edge set FR-15's delete gate refuses on.
			//
			// A failure here fails the whole report rather than leaving
			// the count at zero. Zero is the reassuring answer, and the
			// entire complaint issue #227 makes is that a permanently
			// growing population was being reported as nothing at all; a
			// database that could not be asked must not produce the same
			// output as a deployment that has never reinstated anything.
			// This is different from FreeBytes below, which is a live
			// reading of something outside the journal and is genuinely
			// allowed to be unavailable.
			reinstated, err := lifecycle.ReinstatedArtifacts(ctx, s.Journal, bs.ID)
			if err != nil {
				return health.Report{}, fmt.Errorf("app: health: reinstatement history for %s: %w", bs.ID, err)
			}

			// FR-24's placement half (issue #444). It is computed here
			// rather than inside internal/health because it needs the
			// set's resolved retention chain, and internal/health must
			// not acquire a second way to answer "where does this
			// artifact belong": internal/retention.HomeMedium is the ONE
			// derivation of that rule, and a second one would eventually
			// disagree with the first about a deletion.
			evidence, err := s.placementEvidence(ctx, bs, records, &moves, now)
			if err != nil {
				return health.Report{}, err
			}

			in := health.BackupSetInputs{
				LastSuccessfulPollAt: s.lastPollAt(bs.ID),
				LastRetentionRunAt:   s.lastRetentionAt(bs.ID),
				// Absent from the map means no refusal is on record, which
				// is the honest empty rather than a claim that this set is
				// reachable.
				HaltReason: haltReasons[bs.ID],
			}
			if stat, statErr := capacity.StatPath(bs.LocalPath); statErr == nil {
				free := stat.AvailableBytes
				in.FreeBytes = &free
			}

			sets = append(sets, health.ComputeBackupSetHealth(bs.ID, records, reinstated, evidence, bs.StaleAfter.Duration(), in, now))
		}
	}
	return health.NewReport(process, sets, now), nil
}

// moveReader is the half of the move journal a health pass needs: the
// read, and nothing that could write one.
//
// It is a type assertion on Service.Journal rather than a method on the
// Journal interface, the same shape moveEngine already uses for
// placement.MoveJournal. The reason is the one placementEvidence's own
// gate gives: a deployment with no medium in play never reaches this, so
// requiring it of every journal implementation would make a question
// nobody asks into a compile-time obligation for every one of them.
type moveReader interface {
	ListMoves(ctx context.Context, phases ...string) ([]state.Move, error)
}

// movesBySet is every non-terminal move row in the journal, grouped by
// the backup set its artifact belongs to. A nil map means "not loaded
// yet", which is why
// this is a named type rather than a bare map: loaded-and-empty is a real
// and common answer (a deployment with mediums configured that has never
// had to move anything), and it must not re-trigger the load.
type movesBySet map[model.BackupSetID][]state.Move

// placementEvidence answers FR-24's placement question for one backup
// set: which of its artifacts are not on the medium its chain names, and
// what the move journal says about getting them there (issue #444).
//
// # The gate, and why it is not a short circuit
//
// The whole computation is skipped for a backup set whose chain names no
// medium other than local AND none of whose artifacts sits on one. That
// is not "assume the reassuring answer for deployments we would rather
// not ask about", which is the shape this issue exists to remove. It is
// exact: every tier's EffectiveMedium is local, so every artifact's home
// is local, so an artifact can only be away from home by being on
// something other than local, and the second half of the gate is a direct
// read of exactly that. The zero PlacementEvidence really is the answer,
// and it is asserted rather than argued
// (TestBuildHealthReport_ADeploymentWithNoMediumAsksNoPlacementQuestion).
//
// What the gate buys is that no deployment predating EPIC E changes
// behaviour at all: it runs no retention classification on a status call,
// and it does not have to have a journal that can read the move table.
// Both of those would be new ways for `backup-manager status` to fail for
// a deployment that has never asked for any of this.
//
// # Why a failure here fails the whole report
//
// A retention classification that will not resolve, or a chain naming a
// tier it does not contain, is a disagreement between two things this
// function computed together, and there is no honest partial answer to
// give. Reporting zero artifacts away from home because the question
// could not be asked produces exactly the output of a deployment where
// everything is where it belongs, which is the collapse this whole field
// exists to end. That is the same call BuildHealthReport already makes
// for the reinstatement history and the connection refusals, and the
// opposite of the one it makes for FreeBytes, which is a live reading of
// something outside the journal and is genuinely allowed to be missing.
func (s *Service) placementEvidence(ctx context.Context, bs config.BackupSet, records []state.Record, moves *movesBySet, now time.Time) (health.PlacementEvidence, error) {
	if !placementQuestionApplies(bs, records) {
		return health.PlacementEvidence{}, nil
	}

	if *moves == nil {
		reader, ok := s.Journal.(moveReader)
		if !ok {
			return health.PlacementEvidence{}, fmt.Errorf(
				"app: health: this deployment stores artifacts on a medium other than local, but its journal (%T) cannot read the move journal, "+
					"so a relocation that has been failing for weeks would report as nothing at all", s.Journal)
		}
		// The engine's own resume population, asked for the same way it
		// asks for it (placement.NonTerminalPhaseStrings, derived from
		// the phase list rather than hand-written). A move that is over
		// is not an outstanding relocation, the phase column is indexed
		// for exactly this query, and the alternative is loading every
		// move row this deployment has ever written on every dashboard
		// poll. buildPlacementHealth still checks Terminal() on what
		// comes back rather than trusting the filter.
		all, err := reader.ListMoves(ctx, placement.NonTerminalPhaseStrings()...)
		if err != nil {
			return health.PlacementEvidence{}, fmt.Errorf("app: health: reading the move journal: %w", err)
		}
		loaded := make(movesBySet, len(all))
		for _, mv := range all {
			loaded[mv.Artifact.Set] = append(loaded[mv.Artifact.Set], mv)
		}
		*moves = loaded
	}

	// The same two calls RetentionPreview makes, in the same order,
	// against the same records, so the health report and the retention
	// preview cannot come to disagree about where an artifact belongs.
	// recordRetentionRun is deliberately NOT called: reading a health
	// report is not running retention, and claiming it was would put a
	// fabricated timestamp in LastRetentionRunAt on every dashboard poll.
	verdicts, _, err := retention.DecideKeep(now, bs.Retention, bs.ID, records)
	if err != nil {
		return health.PlacementEvidence{}, fmt.Errorf("app: health: classifying %s for placement: %w", bs.ID, err)
	}
	plan, err := retention.PlanHomeMoves(bs.Retention.EffectiveTiers(), verdicts, ActiveMediumFromRecords(records))
	if err != nil {
		return health.PlacementEvidence{}, fmt.Errorf("app: health: %s: %w", bs.ID, err)
	}

	evidence := health.PlacementEvidence{
		Unconfirmed: len(plan.Unconfirmed),
		Moves:       (*moves)[bs.ID],
	}
	for _, m := range plan.Moves {
		evidence.AwayFromHome = append(evidence.AwayFromHome, health.AwayFromHome{
			Artifact: m.Artifact, On: m.From, Home: m.To,
		})
	}
	return evidence, nil
}

// placementQuestionApplies reports whether this backup set can have an
// artifact away from home at all. See placementEvidence for why this is
// an exact test rather than a convenience.
func placementQuestionApplies(bs config.BackupSet, records []state.Record) bool {
	for _, t := range bs.Retention.EffectiveTiers() {
		if t.EffectiveMedium() != config.MediumLocal {
			return true
		}
	}
	// A tier that named a medium and no longer does leaves artifacts
	// stranded on it, and those artifacts ARE away from home now: their
	// home became local the moment the tier changed. Reading the config
	// alone would hide exactly the population an operator most needs to
	// be told about after that edit.
	for _, r := range records {
		for _, p := range r.Placements {
			if p.Status == state.PlacementActive && !p.IsLocal() {
				return true
			}
		}
	}
	return false
}

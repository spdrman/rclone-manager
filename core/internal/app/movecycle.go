package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/retention"
)

// This file is where FR-30's move engine is finally driven by something
// other than a test (issue #239, the acceptance line #238 handed over).
//
// The spec sentence it implements is one line of FR-30: "Moves are driven
// from the retention cycle: after a retention pass computes each
// artifact's home medium (FR-27), the engine plans moves for artifacts
// whose home differs from their ACTIVE placement's medium, bounded per
// cycle (a max_moves_per_cycle guard), and executes them under FR-27's
// already-given consent. There is no per-move confirmation, and there is
// also no move that a config change did not declare."
//
// # Where the consent is, since nothing here asks for one
//
// The consent is the tier. An operator who writes `medium: cold_offsite`
// against a monthly tier has said where monthly backups belong, and the
// settings flow that writes it discloses the deletion consequence
// (FR-27). This file cannot invent a destination: every plan it builds
// comes out of retention.PlanHomeMoves, whose To is a configured tier's
// EffectiveMedium and nothing else. That is the "no move a config change
// did not declare" half, and it is structural rather than checked.
//
// # It runs after every backup set, not inside one
//
// One cycle, one call, at the end. The bound is a DEPLOYMENT-wide number,
// and calling the engine once per backup set would let a deployment with
// six sets do six times the configured work while every individual call
// looked correct. The engine is deployment-scoped for a second reason
// too: RunCycle resumes every non-terminal move it finds in the journal,
// which is not a per-set list, so a per-set caller would resume other
// sets' moves anyway and spend their budget doing it.

// RunHomeMoves executes the moves a retention pass worked out, and
// reports what happened.
//
// plans is built from reports the caller already has, rather than by
// re-deriving anything: a second derivation would decide against a
// journal and a chain that the cycle's own work has since changed, and
// the moves would then describe a policy that did not produce the
// verdicts they were planned from. HomeMovePlans is the mapping.
//
// RunCycle is its one production caller. It is exported anyway because it
// IS the operation, and because the integration suite has to be able to
// run the real one against a real S3 API rather than reassemble an engine
// of its own and prove something about the reassembly.
func (s *Service) RunHomeMoves(ctx context.Context, plans []placement.Plan) (placement.CycleReport, error) {
	engine, err := s.moveEngine()
	if err != nil {
		return placement.CycleReport{}, err
	}
	if engine == nil {
		return placement.CycleReport{}, nil
	}
	return engine.RunCycle(ctx, plans)
}

// HomeMovePlans turns one backup set's retention report into the plans the
// engine takes.
//
// retention.HomeMove maps onto placement.Plan exactly, which is not a
// coincidence: #238 shaped Plan around this caller. Unconfirmed is
// deliberately dropped here rather than turned into anything: the planner
// already decided those artifacts must not move, and re-deciding it here
// would be a second answer to the question the planner owns.
func HomeMovePlans(plan retention.HomePlan) []placement.Plan {
	out := make([]placement.Plan, 0, len(plan.Moves))
	for _, m := range plan.Moves {
		out = append(out, placement.Plan{Artifact: m.Artifact, DestinationMedium: m.To})
	}
	return out
}

// moveEngine builds the FR-30 move engine this deployment runs, or
// reports that it cannot.
//
// It returns (nil, nil) for a deployment that declares no storage medium.
// That is not a refusal, it is the ordinary state of every deployment
// before EPIC E: there is nowhere to move anything to, so there is
// nothing to do and nothing to complain about.
//
// It returns an error when a medium IS declared and something needed to
// reach one is missing. The two cases are deliberately not the same
// answer. A deployment that declares a medium has asked for artifacts to
// live on it, and a cycle that quietly moves nothing looks exactly like a
// cycle with nothing to move, so the misconfiguration would sit there
// unnoticed while the artifacts an operator asked to be offsite stayed on
// one disk.
//
// # Every seam #238 left is filled here, and none of them is optional
//
// MediumResolver maps a declared medium onto somewhere reachable and onto
// the placement.Class a copy must ACHIEVE before a source may be deleted
// (mediums.go). TierGuard answers FR-30's last question before a source
// delete, from the same chain evaluation this issue already owns
// (tierguard.go). Both are concrete values rather than nils, and the
// engine treats a nil TierGuard as a refusal, so this constructor is the
// difference between an engine that can complete a move and one that
// cannot.
func (s *Service) moveEngine() (*placement.Engine, error) {
	if len(s.Config.StorageMediums) == 0 {
		return nil, nil
	}
	if s.MediumStore == nil {
		return nil, fmt.Errorf(
			"app: this deployment declares %d storage medium(s) but has no way to reach one, so no artifact can be moved to where its chain says it belongs; "+
				"refusing rather than running a cycle that would look identical to a deployment with nothing to move",
			len(s.Config.StorageMediums))
	}
	journal, ok := s.Journal.(placement.MoveJournal)
	if !ok {
		return nil, fmt.Errorf(
			"app: this deployment's journal (%T) cannot record a move, so FR-30's durable-intent-before-every-side-effect ordering has nowhere to live", s.Journal)
	}

	return &placement.Engine{
		Journal: journal,
		Store:   s.MediumStore,
		// The local end of a move addresses full paths that came from the
		// journal or from the destination backup set's OWN store
		// (placement.localArtifactPath), so this value's root is never
		// read. It is rootless on purpose: an engine that spans backup
		// sets has no single local root, and artifactstore.Local.Locator
		// refuses a rootless store by name, so a change that starts
		// computing a path from here fails loudly instead of writing an
		// artifact relative to the daemon's working directory.
		Local:            artifactstore.Local{},
		Mediums:          MediumResolver(s.Config.StorageMediums),
		Sets:             backupSetsOf{s},
		Tiers:            TierGuard{Service: s},
		Now:              s.now,
		MaxMovesPerCycle: s.Config.EffectiveMaxMovesPerCycle(),
	}, nil
}

// backupSetsOf implements placement.BackupSets over a Service's own loaded
// configuration.
//
// FR-20's containment proof is against the CONFIGURED root, re-read at the
// moment of the delete, and this is what re-reads it. A set the journal
// still remembers but the configuration no longer names is an error rather
// than a zero config.BackupSet, for the reason pruneInputsFor gives for
// the same question: there is no root to prove containment against, so
// there is nothing to answer with.
type backupSetsOf struct{ s *Service }

func (b backupSetsOf) Set(id model.BackupSetID) (config.BackupSet, error) {
	_, bs, ok := b.s.backupSetConfigFor(id)
	if !ok {
		return config.BackupSet{}, &NotFoundError{Kind: "backup set", Name: id.String()}
	}
	return bs, nil
}

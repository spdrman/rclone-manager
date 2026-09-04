package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The production placement.TierGuard (EPIC E, FR-30; issue #239,
// inherited from #238's acceptance line).
//
// FR-30's last question before a source copy is removed is "does any
// retention tier whose medium is the source still select this artifact",
// and #238 deliberately did not answer it: the answer is retention
// arithmetic, a second derivation of it would eventually disagree with the
// first about a deletion, and a nil guard is a refusal precisely so that
// the engine could land without one. This is the answer, and it is
// retention.TierMediumSelects, read off the same chain the same records
// would produce a verdict under.
//
// # Everything here fails towards preserving the source
//
// There are four ways this can fail to reach an answer: the journal will
// not talk, the backup set is not in the configuration any more, retention
// refuses the verdict set outright, or the artifact has no verdict at all.
// All four return an error, and placement's guardSourceDelete turns an
// error into a refusal. That is not conservatism for its own sake: the
// only thing a false answer authorises is deleting a copy of a backup, and
// the only thing an error costs is a source that is not reclaimed until
// the next cycle looks again.

// TierGuard answers FR-30's question against a Service's own journal and
// configuration.
//
// It holds the Service rather than a snapshot of records deliberately.
// The guard is asked immediately before an irreversible act, and the whole
// value of asking there is that the answer is derived from what is true
// NOW, not from what a plan observed when it was made. See internal/
// placement/sourcedelete.go's file comment for the same argument applied
// to every other clause of the same gate.
type TierGuard struct{ Service *Service }

// SourceStillSelected implements placement.TierGuard.
//
// The record the engine hands over is used for its artifact id and for
// nothing else. The verdict is recomputed from the backup set's whole
// current inventory, because GFS is a decision about a SET of artifacts:
// which backup represents a given day or month depends on the others, so
// an answer derived from one record alone would be an answer to a
// different question.
func (g TierGuard) SourceStillSelected(ctx context.Context, rec state.Record, medium string) (bool, string, error) {
	if g.Service == nil {
		return false, "", fmt.Errorf("app: the retention tier guard has no service to ask, so nothing here can prove a copy on %q is unwanted", medium)
	}
	set := rec.Artifact.Set

	_, bs, ok := g.Service.backupSetConfigFor(set)
	if !ok {
		return false, "", fmt.Errorf(
			"app: backup set %s is not in this configuration any more, so there is no retention chain that could say whether a copy on %q is still wanted",
			set, medium)
	}

	records, err := g.Service.Journal.ListByBackupSet(ctx, set)
	if err != nil {
		return false, "", fmt.Errorf("app: listing %s to decide whether a copy on %q is still wanted: %w", set, medium, err)
	}

	// The set's own resolved policy since #333, exactly as RetentionPreview
	// reads it. Reading the deployment's chain here would answer under a
	// policy that did not decide this set's verdicts.
	verdicts, _, err := retention.DecideKeep(g.Service.now(), bs.Retention, set, records)
	if err != nil {
		return false, "", fmt.Errorf("app: deciding retention for %s: %w", set, err)
	}

	for _, v := range verdicts {
		if v.Artifact != rec.Artifact {
			continue
		}
		return retention.TierMediumSelects(bs.Retention.EffectiveTiers(), v, medium)
	}

	// No verdict. GFSDecide classifies only managed-complete artifacts, so
	// this is an artifact that is no longer one: quarantined, back in
	// flight, or gone from the journal between the plan and here. The
	// permissive reading is "nothing selects it, so the source is free",
	// and that reading deletes a copy of an artifact whose standing this
	// manager could not establish.
	return false, "", fmt.Errorf(
		"app: retention has no verdict for %s (it is not a managed, completed artifact right now), so nothing here can prove a copy on %q is unwanted",
		rec.Artifact, medium)
}

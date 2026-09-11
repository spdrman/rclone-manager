package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/backupdproject/backupd/core/internal/state"
)

// What is actually sitting on a storage medium right now, so a surface can
// refuse to remove it and can list what a failed re-verification affects
// (G2.2, issue #594; FR-30).
//
// FR-30's invariant is that at no instant may an artifact have no confirmed
// readable copy, and the two questions this file answers are the two ways a
// settings page can violate it by accident.
//
// The first is removal. Today a medium removed from config.yaml leaves
// ErrMediumNotDeclared behind for every surface that touches the copies on
// it, and the copies read as unreachable, which is survivable and is
// honest. It is not something an operator should be able to do with one
// click from a list, because the operator doing it is exactly the operator
// who does not know 148 artifacts are there.
//
// The second is the count itself. "148 copies affected" with no list is not
// something anybody can act on, so this reports the backup sets, with a
// count each, and a surface renders them.
//
// It reads the journal and nothing else. In particular it asks no medium
// anything: this is what the deployment RECORDED, and whether the endpoint
// answers right now is mediumcheck's question, asked separately and
// rendered beside this. Collapsing the two would mean a removal refusal
// depended on a network round trip, so a bucket that was merely down would
// let an operator delete the declaration of the place their backups are.

// MediumUsage is what one storage medium currently holds, per backup set.
//
// It counts placements, not artifacts, which is the same number here and
// would stop being the same the day a placement stopped being one copy of
// one artifact. Naming it for what is counted is what keeps a later reader
// from having to guess.
type MediumUsage struct {
	// Medium is the medium id this is about.
	Medium string

	// Placements is how many copies name this medium and are not GONE.
	//
	// DELETE_PENDING rows are counted with ACTIVE ones deliberately. A
	// copy the journal has decided to delete and has not yet deleted is
	// still a copy on that medium, and removing the declaration
	// underneath it strands the delete as surely as it strands the read.
	Placements int

	// BackupSets is one entry per backup set with at least one such copy,
	// sorted by id so a refusal reads the same way twice.
	BackupSets []MediumUsageBySet
}

// MediumUsageBySet is one backup set's share of a medium.
type MediumUsageBySet struct {
	// Set is the "source/set" id.
	Set string

	// Placements is how many of that set's copies are on the medium.
	Placements int

	// OnlyCopyHere is how many of those are the artifact's ONLY
	// non-local copy AND have no local copy either, which is the
	// population FR-30's invariant is actually about. It is reported
	// separately from Placements because they call for different
	// sentences: "52 copies live here" is information, and "52 artifacts
	// have their only confirmed copy here" is the reason nothing may be
	// deleted on the strength of a check that failed.
	OnlyCopyHere int
}

// InUse reports whether anything at all names this medium, which is the
// question a removal asks. It exists so a caller states it once rather
// than comparing a count to zero at each call site.
func (u MediumUsage) InUse() bool { return u.Placements > 0 }

// MediumUsageOf reports what the journal says is on one storage medium.
//
// It walks the artifacts of configured AND unconfigured backup sets
// (IncludeUnconfigured), and the unconfigured half is the one worth
// justifying. A backup set that has been removed from the configuration
// keeps its retained backups, on this product's own promise, and those
// backups are still on the medium. Leaving them out would let a removal
// refusal answer "nothing names this" about a medium holding the artifacts
// of a set somebody removed last month, which is precisely the population
// least likely to be remembered by the person clicking Remove.
func (s *Service) MediumUsageOf(ctx context.Context, id string) (MediumUsage, error) {
	if id == "" {
		return MediumUsage{}, fmt.Errorf("app: medium usage: no medium id was given")
	}
	records, err := s.ListArtifacts(ctx, ArtifactFilter{IncludeUnconfigured: true})
	if err != nil {
		return MediumUsage{}, fmt.Errorf("app: medium usage: %w", err)
	}

	out := MediumUsage{Medium: id}
	bySet := map[string]*MediumUsageBySet{}
	for _, rec := range records {
		var here, otherDurable int
		for _, p := range rec.Placements {
			if p.Status == state.PlacementGone {
				continue
			}
			if p.Medium == id {
				here++
				continue
			}
			otherDurable++
		}
		if here == 0 {
			continue
		}
		set := rec.Artifact.Set.String()
		entry, seen := bySet[set]
		if !seen {
			entry = &MediumUsageBySet{Set: set}
			bySet[set] = entry
		}
		entry.Placements += here
		out.Placements += here
		if otherDurable == 0 {
			// Every copy this artifact has is on this medium, so a
			// deployment that cannot reach it cannot confirm the artifact
			// exists anywhere at all. That is the sentence FR-30 is about.
			entry.OnlyCopyHere += here
		}
	}

	out.BackupSets = make([]MediumUsageBySet, 0, len(bySet))
	for _, entry := range bySet {
		out.BackupSets = append(out.BackupSets, *entry)
	}
	sort.Slice(out.BackupSets, func(i, j int) bool { return out.BackupSets[i].Set < out.BackupSets[j].Set })
	return out, nil
}

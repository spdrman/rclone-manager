package archive

import (
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// State is FR-34's closed vocabulary for what can be done with one durable
// copy right now, and it is the ONLY definition of it in this repository.
//
// #241's review found the four words written down twice, here and in
// internal/placement, with each copy documenting the duplication in prose
// and neither collapsing it. This is the survivor, because the vocabulary
// is about what an archive class does to a copy's readability, which is
// what this package exists for. Nothing collapses on the other side in
// this branch: internal/placement/access.go has not landed on main, so
// there was no second copy here to delete. What keeps it that way once
// somebody's rebase brings one is
// TestTheAccessVocabularyIsDefinedInExactlyOnePlace (composition_test.go),
// which fails on a second declaration of any of the four strings anywhere
// under core/internal.
//
// Four values, and no fifth. In particular there is no "unknown": a
// surface that cannot work out which of these applies has a bug or a
// drifted class table, and printing a fifth word would turn that bug into
// a thing operators learn to ignore. Access returns an error in that case
// instead, and the caller decides what to say about its own failure rather
// than dressing it up as a fact about the artifact.
type State string

const (
	// Immediate means a read of this copy works now: it is the local
	// copy, or it is on a class that reads on demand, or it is an
	// archived copy whose restore has finished and has not yet expired.
	//
	// FR-34 spells this one as "local, or a non-archive class". The
	// restored case is folded in here rather than given a word of its
	// own because the vocabulary is closed and this is the only member
	// that is TRUE of a restored copy: its bytes are readable, which is
	// exactly what the word means to everything that reads it. The
	// distinction that matters, that this readability has an expiry, is
	// carried by RestoreState.ExpiresAt, which surfaces show.
	Immediate State = "immediate"

	// RequiresRestore means the object is on an archive class and no
	// restore of it is running: the bytes exist and nothing can read them
	// until somebody asks.
	RequiresRestore State = "requires_restore"

	// Restoring means a restore has been initiated and the provider says
	// it has not finished. There is no percentage and no ETA here, ever,
	// because S3 reports neither.
	Restoring State = "restoring"

	// Unreachable means the medium itself could not be reached. It is a
	// fact about the endpoint and NOT about the artifact: an unreachable
	// medium is not a missing object, and the two must never collapse
	// into one another, or a network partition starts quarantining
	// backups that are perfectly fine.
	Unreachable State = "unreachable"
)

// States is the closed vocabulary, in the order a surface should list
// them: best to worst for the operator asking "can I have my data".
var States = []State{Immediate, RequiresRestore, Restoring, Unreachable}

// Valid reports whether s is one of the four.
func (s State) Valid() bool {
	for _, known := range States {
		if s == known {
			return true
		}
	}
	return false
}

// Retrievable reports whether this copy's bytes can be read right now.
//
// It is the single question every cost-bearing and every destructive
// decision in this package asks, and it has exactly one true answer, which
// is why it is a method here rather than a comparison spelled out at each
// call site where somebody could write `!= Unreachable` and accidentally
// include an archived copy.
func (s State) Retrievable() bool { return s == Immediate }

// RestoreState is what a medium reports about a restore of one object.
//
// It is an ALIAS of transport.RestoreState, not a copy of it, and the
// alias is what makes *rclone.Adapter satisfy Store below with no
// conversion anywhere. Two structurally identical structs would have
// worked too, right up until somebody added a field to one of them, and
// the copy that would silently keep compiling is the one an operator
// reads.
//
// Its shape is the endpoint's, and transport.RestoreState's own doc is
// where the argument for that shape lives: a boolean and an optional
// expiry, with nowhere to put a percentage.
type RestoreState = transport.RestoreState

// Probe records what a caller learned by asking the medium about this
// object, which is the fact an access state cannot be derived without.
//
// It is an enum rather than a pair of booleans because the three cases are
// genuinely different and two booleans give four, one of which
// ("unreachable and also answered") is nonsense somebody would eventually
// construct by accident.
type Probe int

const (
	// NotAsked means nobody has asked the medium about this object. It is
	// the zero value on purpose: a caller that fills in nothing gets the
	// state that claims the least.
	NotAsked Probe = iota

	// Answered means the medium was asked and answered, so whatever is in
	// Observation.Restore is what it said.
	Answered

	// DidNotAnswer means a call to the medium failed for a reason other
	// than the object not being there. It is what Unreachable is derived
	// from, and it is the only thing that derives it: a medium nobody
	// asked is not a medium that is down.
	DidNotAnswer
)

// Observation is what is currently KNOWN about one copy, gathered from
// somewhere other than the journal.
//
// FR-34 says an access state is derived only from held facts, so this
// struct is the list of facts that are not already in the placement row.
// Its zero value means "I have not looked", and an archive copy nobody has
// looked at reads as RequiresRestore: that is the state an archived object
// is in unless something has been done about it, and it is also the safe
// direction, since it is the one that refuses to read.
type Observation struct {
	// Probe is what asking the medium produced. See Probe.
	Probe Probe

	// Restore is what the medium says about a restore of this object, or
	// nil when it reports no restore status at all, which is what S3
	// returns for an object nobody has ever asked to restore. It is only
	// meaningful when Probe is Answered.
	Restore *RestoreState
}

// Access derives the access state of a copy on medium, whose medium is
// configured with storage class class.
//
// The local medium is Immediate unconditionally and without consulting
// obs: a local file is readable by opening it, there is no endpoint to be
// unreachable, and the class of a local placement is meaningless. This is
// the branch that keeps FR-35's compatibility promise, since every
// artifact in every existing deployment has exactly one local placement
// and therefore reads exactly as it read before EPIC E.
func Access(medium, class string, obs Observation, now time.Time) (State, error) {
	if medium == state.MediumLocal {
		return Immediate, nil
	}

	b, err := Of(class)
	if err != nil {
		return "", err
	}

	if obs.Probe == DidNotAnswer {
		// A medium that was asked and did not answer is Unreachable
		// whatever class it holds. A STANDARD object on a bucket that is
		// down is no more readable than an archived one, and reporting it
		// as immediate would send a caller off to do a read that cannot
		// work.
		return Unreachable, nil
	}

	if !b.Archive {
		return Immediate, nil
	}

	// From here down the object is on an archive class, and the whole
	// question is whether a restore has made it readable.
	if obs.Probe == NotAsked || obs.Restore == nil {
		// Either nobody looked, or the medium answered and reported no
		// restore status at all, which for S3 means nobody has asked to
		// restore this object. Both are the same state: archived, with
		// nothing being done about it.
		return RequiresRestore, nil
	}
	if obs.Restore.InProgress {
		return Restoring, nil
	}
	if obs.Restore.ExpiresAt != nil && obs.Restore.ExpiresAt.After(now) {
		return Immediate, nil
	}
	// Not in progress, and either no expiry was reported or the one that
	// was has passed. Either way this cannot show that the bytes are
	// readable now, and the direction that is safe when it cannot show
	// that is the one that refuses to read them.
	return RequiresRestore, nil
}

// Describe is the plain-words sentence a surface prints beside an access
// state, or the empty string for a copy that needs no explaining.
//
// It never contains a percentage, a completion time or a price, and
// access_test.go asserts that against every state rather than trusting me
// to have remembered.
func Describe(s State, class string, r *RestoreState) string {
	switch s {
	case Immediate:
		if r != nil && r.ExpiresAt != nil {
			return fmt.Sprintf("restored from %s and readable until %s, after which it goes back to needing a restore",
				class, r.ExpiresAt.UTC().Format(time.RFC3339))
		}
		return ""
	case RequiresRestore:
		b, err := Of(class)
		if err != nil {
			return "this copy is on a storage class this build does not recognise, so nothing here will read it without a restore"
		}
		return fmt.Sprintf("this copy is on %s, so its bytes cannot be read until a restore is asked for and finishes; %s", class, b.RestoreWait)
	case Restoring:
		return "a restore of this copy is running; the provider reports whether a restore is finished and nothing else, so there is no progress to show and no finishing time to give"
	case Unreachable:
		return "the medium holding this copy did not answer, which says nothing about whether the copy is still there"
	default:
		return ""
	}
}

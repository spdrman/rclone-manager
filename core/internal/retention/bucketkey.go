package retention

import (
	"time"

	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file answers the question FR-18 used to leave open (issue #192):
// which of an artifact's timestamps decides which retention bucket it
// lands in.
//
// # Two timestamps, and why neither one alone is the answer
//
// A journal record carries two timestamps that could plausibly place an
// artifact on a calendar:
//
//   - the discovery timestamp (state.Record.DiscoveredAt), the moment this
//     manager first observed the artifact on the remote. It comes from
//     this manager's own clock and nothing outside the manager can move
//     it.
//   - the producer timestamp (state.Record's Remote.ModTime), the remote
//     object's own modification time as captured at discovery. It
//     describes when the backup was actually taken, and FR-8 requires it
//     to be treated as untrusted input.
//
// "Discovery timestamp" rather than "received timestamp" is deliberate,
// and the distinction is worth holding on to. internal/recovery's
// manifest already writes a received_timestamp field into every sidecar
// file, and that one is a different instant: it is rec.UpdatedAt, the
// moment the artifact finished committing locally, not the moment it was
// first seen on the remote. The two are usually the same day, which is
// exactly what makes confusing them dangerous under a calendar-bucketed
// policy. The manifest field this package's placement actually
// corresponds to is retention_timestamp, which carries rec.DiscoveredAt.
//
// Bucketing on the discovery timestamp alone is what this package used to
// do, and it collapses an ingested backlog. A new backup set pointed at a
// directory holding a year of dumps, a manager that was down for a week
// and caught up, a NAS restored from elsewhere: in every one of those the
// artifacts have genuinely different backup dates, and a discovery-only
// key puts all of them in one daily bucket, one weekly bucket and one
// monthly bucket. Each tier then selects one representative, FR-19 saves
// one more, and everything else is a delete candidate on the first pass.
// That is the situation GFS retention exists for, and it is the situation
// where a discovery-only key does the opposite of what it exists for.
//
// Bucketing on the producer timestamp alone is worse, and FR-8 is why. It
// hands a producer with a wrong clock, or a hostile one, the power to move
// artifacts *out* of every tier window: back-date a set to 1990 and every
// artifact in it is a delete candidate on the next pass. Refusing a
// future-dated producer timestamp does not help, because the direction
// that deletes is the past one.
//
// # The rule: both, unioned, so untrusted input can only ever add
//
// FR-18 therefore places every artifact twice, and KEEP is the union of
// what the chain selects under each placement:
//
//	KEEP = ⋃ over every tier t of ( selections_discovery(t) ∪ selections_producer(t) )
//	     ∪ protected
//
// The discovery pass is bit-for-bit the calculation this package always
// performed. The producer pass runs beside it, never instead of it. That
// is the entire safety argument: a producer timestamp can only ever move
// an artifact from DELETE to KEEP, never the reverse, so being wrong
// about one costs disk and never costs a backup. FR-8's untrusted input
// is admitted exactly where being wrong is survivable.
//
// The two passes are kept separate rather than folded into a single
// champion map per bucket, and that is load-bearing rather than
// stylistic. In a merged map a producer placement can out-compete a
// discovery placement inside a shared bucket and displace an artifact the
// discovery-only calculation had kept: an artifact produced late on Monday
// but discovered on Tuesday would compete for Monday's bucket against an
// artifact discovered early on Monday, and could win it. Separate passes,
// unioned afterwards, make that structurally impossible.
// TestGFSDecideProducerTimestampOnlyEverAddsToKeep holds the resulting
// invariant to every input it is given, and a mutation that merges the
// two passes fails it.

// gfsPlacement is one (calendar date, instant) at which an artifact is
// offered to a tier's buckets. The date decides which bucket, and the
// instant decides which artifact represents that bucket, so both travel
// together rather than being recomputed from a record twice.
type gfsPlacement struct {
	date     gfsCivilDate
	occurred time.Time
}

// gfsProducerRefusal says why a record's producer timestamp was not
// admitted, so FR-19's reason line can tell an operator which timestamp
// actually decided and, when it was not the producer's, why not. There is
// no "admitted" member: a resolved producer timestamp is represented by
// the ok return alongside it.
type gfsProducerRefusal int

const (
	// gfsProducerAbsent: the backend reported no modification time at all
	// (internal/discovery's captureRemoteIdentity leaves Remote.ModTime
	// nil in that case), or reported one that is the zero time, which is
	// a missing value that happens to be non-nil rather than a date.
	gfsProducerAbsent gfsProducerRefusal = iota

	// gfsProducerAfterDiscovery: the remote claims the object was modified
	// after the moment this manager first observed it. A completed
	// artifact cannot have been produced after it was discovered, so this
	// is a wrong or forged clock.
	gfsProducerAfterDiscovery
)

// gfsDiscoveryInstant is the timestamp this manager first observed the
// artifact. It is always available: internal/state's journal writes
// discovered_at on the row's very first transition and never clears it.
func gfsDiscoveryInstant(rec state.Record) time.Time { return rec.DiscoveredAt }

// gfsProducerInstant resolves the producer's own timestamp for rec and
// reports whether FR-18 admits it as a placement.
//
// Admissible means the backend reported a modification time, it is not
// the zero time, and it is not after the discovery timestamp. The last
// check is a refusal rather than a clamp on purpose: clamping a
// future-dated timestamp to the discovery timestamp would manufacture a
// date this manager has no evidence for and would silently give a wrong
// clock a placement it did not earn, whereas refusing it leaves the
// artifact placed by the discovery pass alone, which is where an artifact
// with no usable producer timestamp belongs anyway.
//
// Nothing here bounds how far into the past a producer timestamp may
// reach, and nothing needs to. An absurdly old one simply lands outside
// every tier window and contributes no placement, while the artifact's
// discovery placement is untouched; see this file's doc for why that is
// the whole point of the union.
func gfsProducerInstant(rec state.Record) (time.Time, gfsProducerRefusal, bool) {
	if rec.Remote.ModTime == nil || rec.Remote.ModTime.IsZero() {
		return time.Time{}, gfsProducerAbsent, false
	}
	producer := *rec.Remote.ModTime
	if producer.After(gfsDiscoveryInstant(rec)) {
		return producer, gfsProducerAfterDiscovery, false
	}
	return producer, 0, true
}

// gfsPlacementsFor returns the placements a single record contributes: its
// discovery placement always, and its producer placement when
// gfsProducerInstant admits one.
func gfsPlacementsFor(rec state.Record, loc *time.Location) (discovered gfsPlacement, producer gfsPlacement, hasProducer bool) {
	r := gfsDiscoveryInstant(rec)
	discovered = gfsPlacement{date: gfsCivilDateIn(r, loc), occurred: r}
	if p, _, ok := gfsProducerInstant(rec); ok {
		return discovered, gfsPlacement{date: gfsCivilDateIn(p, loc), occurred: p}, true
	}
	return discovered, gfsPlacement{}, false
}

// gfsRetentionInstant is the single date FR-19 orders candidates by: the
// producer's own timestamp when it is admissible, and the discovery
// timestamp otherwise. It is deliberately the same resolution FR-18's
// producer pass uses, so "the newest eligible restore point" and "the
// artifact the daily tier kept today" cannot disagree about what an
// artifact's date is.
func gfsRetentionInstant(rec state.Record) (ts time.Time, fromProducer bool, refusal gfsProducerRefusal) {
	p, why, ok := gfsProducerInstant(rec)
	if ok {
		return p, true, 0
	}
	return gfsDiscoveryInstant(rec), false, why
}

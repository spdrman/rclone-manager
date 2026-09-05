package retention

import (
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TierLastKnownGood marks a GFSVerdict kept under FR-19: the artifact holds
// last-known-good protection, not because any FR-18 GFS tier selected it.
//
// It deliberately is not spelled GFSProtected or similar: this protection is
// explicitly outside GFS's three tiers by the EPIC's own formula (daily ∪
// weekly ∪ monthly ∪ protected are four separate terms), and the name
// should not suggest it is a fourth GFS tier.
//
// # This file's job
//
// FR-18's own formula names this package's second job explicitly:
//
//	KEEP = daily ∪ weekly ∪ monthly ∪ protected
//
// GFSDecide (gfs.go) computes the first three terms and is deliberately
// unaware of the fourth: see its doc comment's "What this file does not
// do". This file computes "protected" -- the newest known-good restore
// point in a backup set -- and composes it into GFSDecide's own output, so
// the union above is a real, tested step in this package rather than
// something a caller has to remember to perform itself.
//
// # What "known-good" means here
//
// A backup is eligible as known-good only if it is a valid committed or
// complete restore point that has satisfied required verification: exactly
// gfsIsManagedComplete's Committed/RemoteDeletePending/Complete/
// RemoteRetained set, the same one gfs.go already uses for "managed,
// completed backup". That is four states, and writing the count out is
// deliberate. REMOTE_RETAINED is easy to leave off this list and costly
// to: it is the only state a read-only backup set ever reaches, so
// omitting it reads as "a read-only set has no restore points at all".
// The README said exactly that until #478. FAILED,
// QUARANTINED, QUARANTINED_LOST and every .partial (pre-Committed) state
// are excluded, matching FR-19's own wording. That is also, by
// construction, the identical state set internal/health's decideState
// calls knownGood for FR-24: this package cannot import that unexported
// map (health depends on nothing upstream of it, and importing it here
// would invert that), so the equivalence is enforced by review and by
// TestLastKnownGoodEligibilityMatchesGFSManagedComplete rather than by a
// shared symbol, but the four states involved are the same four states,
// on purpose, in both places.
//
// Reusing gfs.go's own eligibility check is not just convenient: it is
// what guarantees the protected artifact, whenever one exists, already has
// an entry in GFSDecide's returned verdicts (GFSDecide includes every
// managed-complete artifact whether or not any tier kept it). Composition
// in ApplyLastKnownGood can therefore always find that entry and flip it,
// rather than needing to fabricate one.
//
// # The quarantined-newest trap
//
// The newest *arrival* in a backup set is not necessarily the newest
// *eligible* one. A set whose only recent artifact is QUARANTINED must fall
// back to protecting an older genuinely-good artifact, never conclude the
// quarantined one counts (it is never eligible, full stop) and never
// conclude nothing is protected (an older good one is still there to
// protect). LastKnownGoodDecide handles this the same way GFSDecide picks a
// tier's representative: filter to eligible records first, then take the
// newest of what remains. See TestLastKnownGoodFallsBackPastQuarantinedNewest.
//
// # The config flag
//
// cfg.ProtectLastKnownGood is a *bool so config.Validate can default a truly
// absent key to true while leaving an explicit false alone (see that
// field's doc in config.go). LastKnownGoodDecide applies that same "absent
// means protect" reading to a nil pointer too, so a caller that bypasses
// Validate still gets the safe behaviour rather than an accidental
// protection-off. An explicit false is honoured exactly as written: the
// operator asked for a materially more dangerous configuration, and
// LastKnownGoodResult.Enabled is false with a Reason that says so in
// plain words, so that fact is visible to a caller or a log line rather
// than silently absent.
const TierLastKnownGood GFSTier = "LAST_KNOWN_GOOD"

// lkgSelection is the single entry FR-19's protection ever contributes to
// a verdict's tier list. It exists as one value rather than being built at
// each of ApplyLastKnownGood's two call sites so the two cannot come to
// disagree about how protection is attributed.
var lkgSelection = GFSTierSelection{Tier: TierLastKnownGood, By: GFSSelectedByProtection}

// LastKnownGoodResult is FR-19's answer for one backup set: whether
// protection is active, and if so, which single artifact currently holds
// it.
type LastKnownGoodResult struct {
	Set model.BackupSetID

	// Enabled reports the resolved protect_last_known_good reading: false
	// only when the operator explicitly set it to false, true for both an
	// explicit true and an absent/nil value. See config.go's field doc and
	// TierLastKnownGood's doc comment above.
	Enabled bool

	// Protected is true iff Enabled is true and at least one eligible
	// (known-good) artifact exists in this backup set. Check this field,
	// not just Artifact being non-zero, before treating "nothing is
	// protected" as established: Artifact is meaningless when this is
	// false.
	Protected bool

	// Artifact is the protected artifact. Only meaningful when Protected is
	// true; the zero model.ArtifactID{} otherwise.
	Artifact model.ArtifactID

	// Reason explains the result in one sentence, mirroring
	// internal/health's decideState(evidence) (State, string) pattern: a
	// human or a log line should be able to see why an artifact was, or was
	// not, protected without re-deriving it from cfg and records.
	Reason string
}

// LastKnownGoodDecide computes FR-19's last-known-good protection for one
// backup set.
//
// Unlike GFSDecide, this does not take a "now": whether the newest eligible
// artifact is protected does not depend on how old it is (that is exactly
// the point of FR-19 -- age alone must never disqualify it), so there is no
// clock reading for this calculation to need or to accidentally depend on.
//
// records must all belong to set, exactly like GFSDecide's own FR-7
// isolation rule; a record from another set is rejected rather than
// silently folded in.
func LastKnownGoodDecide(cfg config.Retention, set model.BackupSetID, records []state.Record) (LastKnownGoodResult, error) {
	if set.IsZero() {
		return LastKnownGoodResult{}, fmt.Errorf("retention: LastKnownGoodDecide needs a non-zero backup set id")
	}

	enabled := cfg.ProtectLastKnownGood == nil || *cfg.ProtectLastKnownGood
	result := LastKnownGoodResult{Set: set, Enabled: enabled}

	if !enabled {
		result.Reason = "protect_last_known_good is explicitly false: FR-19 last-known-good protection is disabled for this backup set, which is a materially more dangerous configuration"
		return result, nil
	}

	var newest *lkgCandidate

	for _, rec := range records {
		if rec.Artifact.Set != set {
			return LastKnownGoodResult{}, fmt.Errorf("retention: record %s does not belong to backup set %s (FR-7 isolation)", rec.Artifact, set)
		}
		if !gfsIsManagedComplete(rec.State) {
			// FAILED, QUARANTINED, QUARANTINED_LOST and every .partial
			// (pre-Committed) state land here and are never candidates,
			// regardless of how recent they are. This is the check that
			// keeps the quarantined-newest trap from succeeding.
			continue
		}
		cand := newLKGCandidate(rec)
		if newest == nil || cand.newerThan(*newest) {
			c := cand
			newest = &c
		}
	}

	if newest == nil {
		result.Reason = "no eligible restore point exists in this backup set (nothing is committed, remote-delete-pending or complete)"
		return result, nil
	}

	result.Protected = true
	result.Artifact = newest.artifact
	result.Reason = fmt.Sprintf("artifact %q is the newest eligible restore point in this backup set, dated %s %s, and holds FR-19 last-known-good protection",
		newest.artifact.Name, newest.retention.UTC().Format(time.RFC3339), newest.provenance())
	return result, nil
}

// lkgCandidate is one eligible artifact as FR-19 orders it.
//
// retention is the artifact's own retention date, resolved exactly the way
// FR-18's producer pass resolves it (bucketkey.go's gfsRetentionInstant):
// the producer's own timestamp when the backend reported an admissible
// one, and the discovery timestamp otherwise. Ordering on that rather than
// on the discovery timestamp is what makes "the newest eligible restore
// point" mean the newest backup instead of the newest arrival, which is
// the whole of issue #192 as it reaches FR-19: an ingested backlog whose
// six artifacts all arrived in the same cycle used to resolve entirely on
// the artifact-name tie-break, and cheerfully described the oldest file in
// the set as the newest restore point.
//
// # Why an untrusted producer timestamp is safe to order on here
//
// A producer can back-date its newest artifact so that an older one looks
// newest and takes the protection. What it cannot do is take the newest
// artifact out of KEEP: an artifact this manager discovered recently is
// placed by FR-18's discovery pass on today's date, today falls inside
// every enabled tier's window by construction, and so it is always some
// bucket's representative. The attack therefore costs a label, not a
// restore point. A forward-dated timestamp is refused outright before it
// gets this far (gfsProducerInstant), so a producer cannot simply claim
// tomorrow and take the protection that way.
type lkgCandidate struct {
	artifact     model.ArtifactID
	retention    time.Time
	discovered   time.Time
	fromProducer bool
	refusal      gfsProducerRefusal
}

func newLKGCandidate(rec state.Record) lkgCandidate {
	ts, fromProducer, refusal := gfsRetentionInstant(rec)
	return lkgCandidate{
		artifact:     rec.Artifact,
		retention:    ts,
		discovered:   gfsDiscoveryInstant(rec),
		fromProducer: fromProducer,
		refusal:      refusal,
	}
}

// newerThan orders two candidates: retention date first, then the
// discovery timestamp, then the artifact name. The name is last and is what makes
// the answer independent of the order records arrived in; the discovery
// timestamp sits ahead of it so that two artifacts sharing a retention
// date (a backend that reports whole-second or whole-day mtimes, say)
// still resolve on something meaningful before falling back to
// alphabetical order.
func (c lkgCandidate) newerThan(other lkgCandidate) bool {
	if !c.retention.Equal(other.retention) {
		return c.retention.After(other.retention)
	}
	if !c.discovered.Equal(other.discovered) {
		return c.discovered.After(other.discovered)
	}
	return c.artifact.Name > other.artifact.Name
}

// provenance says which of the two timestamps produced this candidate's
// date, and when it was not the producer's, why not. An operator reading
// a retention preview has to be able to tell those apart: "newest" means
// something different under each, and issue #192 was filed because the
// line said neither.
func (c lkgCandidate) provenance() string {
	if c.fromProducer {
		return "by the producer's own timestamp on the remote object"
	}
	switch c.refusal {
	case gfsProducerAfterDiscovery:
		return "by the time this manager discovered it (the remote's own timestamp is later than that, which a completed artifact cannot be, so it was refused as untrustworthy)"
	default:
		return "by the time this manager discovered it (the remote reported no usable modification time)"
	}
}

// ApplyLastKnownGood folds lkg into verdicts, producing the FR-18 ∪ FR-19
// KEEP union the EPIC formula names: daily ∪ weekly ∪ monthly ∪ protected.
// verdicts is expected to be GFSDecide's own output for the same backup set
// and the same records lkg was computed from; the input slice is never
// mutated, and its by-name sort order is preserved.
//
// When lkg.Protected is false (protection disabled, or nothing eligible
// exists), verdicts is returned unchanged: there is nothing to union in.
//
// When the protected artifact already sits inside a GFS tier, its Tiers
// gains TierLastKnownGood alongside whichever tiers GFSDecide already found,
// so a verdict can honestly show more than one reason it survived. When it
// sits outside every tier, this is the only mechanism that keeps it: Keep
// flips to true and Tiers becomes exactly [TierLastKnownGood].
//
// The entry it appends is attributed to GFSSelectedByProtection, which is
// the one member of that type that is not a placement (issue #218). FR-19
// protection is not a bucket selection at all, so neither of FR-18's two
// timestamps produced it and neither may be named for it.
func ApplyLastKnownGood(verdicts []GFSVerdict, lkg LastKnownGoodResult) []GFSVerdict {
	if !lkg.Protected {
		return verdicts
	}

	out := make([]GFSVerdict, len(verdicts))
	copy(out, verdicts)

	for i := range out {
		if out[i].Artifact == lkg.Artifact {
			out[i].Keep = true
			out[i].Tiers = append(append([]GFSTierSelection(nil), out[i].Tiers...), lkgSelection)
			// GFSVerdict.SiblingCollisions is documented as populated
			// only when Keep is false (issue #292): an artifact FR-19
			// protection just kept has nothing left to disambiguate, and
			// leaving a now-stale collision attached would say this
			// protected artifact is still a silently-split delete
			// candidate, which it no longer is.
			out[i].SiblingCollisions = nil
			return out
		}
	}

	// Defensive only: LastKnownGoodDecide draws its candidate from
	// gfsIsManagedComplete, the identical eligibility GFSDecide itself
	// uses, so the protected artifact is always already present in
	// verdicts when verdicts and lkg were computed from the same records
	// and backup set. Reaching here means a caller passed mismatched
	// inputs; append rather than silently drop the protection, and
	// re-sort so the documented by-name order still holds.
	out = append(out, GFSVerdict{
		Artifact: lkg.Artifact,
		Keep:     true,
		Tiers:    []GFSTierSelection{lkgSelection},
	})
	sortGFSVerdicts(out)
	return out
}

// DecideKeep runs FR-18's GFS calculation and FR-19's last-known-good
// protection together and returns the final KEEP union the EPIC formula
// names, plus the LastKnownGoodResult on its own for a caller (or FR-23
// observability) that needs to see why, separately from the merged
// verdicts. This is the function production callers should use; GFSDecide
// and LastKnownGoodDecide stay exported separately because each half's own
// reasoning is independently useful and independently tested.
func DecideKeep(now time.Time, cfg config.Retention, set model.BackupSetID, records []state.Record) ([]GFSVerdict, LastKnownGoodResult, error) {
	verdicts, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		return nil, LastKnownGoodResult{}, err
	}
	lkg, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		return nil, LastKnownGoodResult{}, err
	}
	return ApplyLastKnownGood(verdicts, lkg), lkg, nil
}

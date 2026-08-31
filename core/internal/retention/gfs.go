// Package retention implements FR-18's deterministic GFS (grandfather-
// father-son) retention policy.
//
// For each backup set, GFSDecide classifies every managed, completed
// backup into KEEP or "not kept by GFS": KEEP is the union of the daily,
// weekly and monthly tiers, each of which retains the newest valid backup
// in every calendar bucket that falls inside that tier's look-back window.
//
// # Which timestamp puts an artifact in a bucket
//
// Two of them, and each tier is evaluated once per placement with the
// results unioned: the discovery timestamp (when this manager first saw
// the artifact) always, and the producer's own timestamp (the remote
// object's modification time) where one is admissible. bucketkey.go holds
// that rule, why neither timestamp alone is the answer, and why the union
// is what keeps FR-8's untrusted-input requirement intact. Read it before
// changing anything in this file's tier loop.
//
// # Determinism
//
// The whole point of this package is that the same inputs always produce
// the same verdict, regardless of when the calculation runs, what the
// machine's local timezone happens to be, or what order records are
// listed in. That rules out time.Now: GFSDecide takes the current instant
// as a plain time.Time argument instead of a callback, so a caller resolves
// "now" exactly once and the calculation itself never reaches for the
// clock. Every other source of ordering-sensitivity (map iteration,
// timestamp ties, unsorted input) is handled explicitly; see the doc
// comments on GFSDecide and gfsIsNewerRepresentative.
//
// # Timezone: a documented conflict, resolved in config's favour
//
// FR-18's own prose gives an example default of America/Vancouver for the
// retention timezone. internal/config's actual validated default is UTC
// (see config/validate.go's validateRetention). Those two are genuinely
// different defaults, not two ways of writing the same thing, and this
// package resolves the conflict by reading whatever internal/config
// actually supplies (cfg.Timezone) rather than hardcoding the EPIC's
// example: config is the one place downstream code is meant to trust for
// "what does zero mean here" (see that package's Validate doc), and this
// package would rather be wrong the same way config is wrong than silently
// disagree with it. See this package's introducing PR for the same note.
//
// # What this file does not do
//
// GFSDecide itself decides only what the daily/weekly/monthly tiers keep.
// It does not know about last-known-good protection (FR-19: see
// lastknowngood.go's LastKnownGoodDecide, ApplyLastKnownGood and DecideKeep
// in this same package for that composition) and it does not delete
// anything (FR-20, local deletion safety, is deliberately a separate
// concern with its own mandatory dry-run). A GFSVerdict with Keep == false,
// straight out of GFSDecide, is a GFS delete *candidate*, not a delete
// order: FR-19 still has to union in the protected set before DELETE is
// final, and FR-20 still has to independently re-verify every artifact
// before removing anything from disk. Nothing in this file touches a
// filesystem.
package retention

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// GFSTier names one tier of FR-18's retention chain, as it appears on the
// wire: apps/common/webhost sends these strings straight through to the
// client as a verdict's tiers, so a tier name is API surface, not an
// internal label.
//
// A tier's name comes from configuration (config.RetentionTier.Name),
// upper-cased. The three constants below are the names FR-18's default
// chain has always used and are pinned here because renaming any of them
// would break every existing client: a tier configured as "daily" reports
// as DAILY, exactly as it did when there were only ever three tiers.
type GFSTier string

const (
	GFSDaily   GFSTier = "DAILY"
	GFSWeekly  GFSTier = "WEEKLY"
	GFSMonthly GFSTier = "MONTHLY"
)

// gfsTierName renders a configured tier's name for the wire. Uppercasing
// is the whole transformation, so "daily" lands on GFSDaily by
// construction rather than through a lookup table that could fall out of
// step with it.
//
// The set of strings this can produce is open, and a client has to treat
// it that way: FR-18's chain is operator-defined, so SEMI_ANNUAL, ANNUAL
// or any other name a config file spells arrives here. What bounds the
// shape of that string is config.Validate's lower_snake_case rule, which
// belongs to config alone and is not re-checked here; the one name this
// function's caller does refuse is FR-19's reserved LAST_KNOWN_GOOD (see
// gfsResolveTier), because that one is not merely unrecognised by a
// client, it means something else.
func gfsTierName(configured string) GFSTier {
	return GFSTier(strings.ToUpper(configured))
}

// GFSSelectedBy names which of FR-18's two placements put an artifact in
// one tier's bucket (issue #218).
//
// bucketkey.go's doc has the two placements and why there are two of
// them. What matters here is that they are not interchangeable evidence:
// the discovery placement comes from this manager's own clock and nothing
// outside the manager can move it, while the producer placement is the
// remote object's own modification time, which FR-8 requires be treated
// as untrusted. An operator asking "why is this being kept" is asking
// which of those two answered, and before this type existed the preview
// could not tell them.
//
// It is a string rather than an int for the same reason GFSTier is: these
// values reach a client through apps/common/webhost, so they are API
// surface and renaming one would break every reader of it.
type GFSSelectedBy string

const (
	// GFSSelectedByDiscovery: the discovery pass selected this artifact
	// for this tier and the producer pass did not. The selection rests
	// entirely on this manager's own record of when it first saw the
	// artifact, so nothing a producer reports can change it.
	GFSSelectedByDiscovery GFSSelectedBy = "DISCOVERY"

	// GFSSelectedByProducer: the producer pass selected it and the
	// discovery pass did not. This is the tier attribution that exists
	// only because FR-18 admits the remote's own timestamp, and therefore
	// the one an operator auditing an ingested backlog, a wrong clock or a
	// hostile one actually needs to see. It can only ever have ADDED a
	// KEEP (see bucketkey.go), never removed one.
	GFSSelectedByProducer GFSSelectedBy = "PRODUCER"

	// GFSSelectedByBoth: both passes selected it for this tier, which is
	// the ordinary case for an artifact whose producer timestamp and
	// discovery timestamp fall on the same calendar date. Reported as its
	// own value rather than collapsed into either single one, because
	// "the producer term is not load-bearing here" is a materially
	// different fact from "the producer term is the only thing keeping
	// this".
	GFSSelectedByBoth GFSSelectedBy = "BOTH"

	// GFSSelectedByProtection: not a placement at all. FR-19's
	// last-known-good term (TierLastKnownGood) is not a bucket selection,
	// so it has no placement to name, and dressing it as one would tell an
	// operator this manager had made a calendar decision it never made.
	// ApplyLastKnownGood is the only thing that ever produces it.
	GFSSelectedByProtection GFSSelectedBy = "PROTECTION"
)

// GFSTierSelection is one tier's claim on an artifact, together with which
// placement made it.
//
// The pairing is the whole point of issue #218 and is deliberately not
// two parallel lists or one attribution per verdict. A single artifact can
// be selected by DAILY through the discovery placement and by MONTHLY
// through the producer placement in the same calculation (see
// TestGFSDecideAttributesEachTierToThePlacementThatSelectedIt), so an
// attribution that hung off the verdict would be wrong in exactly the
// cases an operator is looking at the preview to understand.
type GFSTierSelection struct {
	Tier GFSTier
	By   GFSSelectedBy
}

// String renders one selection the way the CLI's per-artifact line and the
// web UI's badges both spell it: the tier, and the placement that selected
// it in parentheses.
//
// FR-19's protected term renders bare. It has no placement, and a
// parenthesised word after it would read as one; the tier name
// LAST_KNOWN_GOOD already says what kind of thing it is. Anything else
// carrying an unrecognised placement renders it verbatim rather than
// silently dropping to bare, so a value this build has never heard of can
// never be mistaken for FR-19's protection.
func (s GFSTierSelection) String() string {
	if s.By == GFSSelectedByProtection {
		return string(s.Tier)
	}
	return string(s.Tier) + "(" + strings.ToLower(string(s.By)) + ")"
}

// GFSVerdict is one backup set artifact's GFS classification.
//
// Keep reflects only this package's daily/weekly/monthly union: see the
// package doc's "What this package does not do" for why this is not yet
// the EPIC formula's final KEEP/DELETE call.
type GFSVerdict struct {
	Artifact model.ArtifactID

	// Keep is true if at least one GFS tier selected this artifact as a
	// bucket's representative.
	Keep bool

	// Tiers lists every tier that selected this artifact, in the order the
	// configured chain lists them (never reordered, so two runs over the
	// same inputs render it identically), each paired with the placement
	// that selected it there. Nil when Keep is false.
	//
	// For the default chain that is still Daily, Weekly, Monthly.
	// TierLastKnownGood (lastknowngood.go) can appear too, but only after
	// ApplyLastKnownGood composes FR-19's protected term into a GFSDecide
	// result; it is always appended after any GFS tiers already present,
	// so the configured chain's own ordering above is unaffected.
	//
	// The placement travels WITH the tier rather than beside it, so there
	// is no way to render a tier here without saying what selected it.
	// That is issue #218's actual requirement: see GFSTierSelection.
	Tiers []GFSTierSelection
}

// TierNames projects Tiers down to bare tier names, for the callers that
// genuinely only need the list (FR-20's KEEP sentence, the wire's own
// `tiers` field). It is a projection of Tiers and never a second stored
// copy of it, so the two cannot drift.
func (v GFSVerdict) TierNames() []GFSTier {
	if len(v.Tiers) == 0 {
		return nil
	}
	out := make([]GFSTier, 0, len(v.Tiers))
	for _, sel := range v.Tiers {
		out = append(out, sel.Tier)
	}
	return out
}

// gfsManagedCompleteStates are the lifecycle states FR-18's "managed
// complete backups" refers to: a durable local artifact the pipeline has
// finished producing, that has not (yet, or ever) been found bad.
//
// Committed is the earliest of the three included here. lifecycle's own
// package doc is explicit that "[f]rom here on the backup has already
// succeeded, regardless of what happens to the remote copy next", so
// waiting for RemoteDeletePending or Complete before considering an
// artifact for retention would make retention's view of "what backups
// exist" depend on how far FR-15's remote-delete step happens to have
// gotten, which has nothing to do with whether the local backup is any
// good.
//
// Everything before Committed (Discovered..Verified) is still in flight,
// not yet a completed backup, so it is out of scope here too, not just
// unselected. Failed and Quarantined never produced a valid backup.
// QuarantinedLost is deliberately excluded as well: it means the durable
// local copy was found corrupted after Complete, with no remote source
// left to recover from, and lifecycle's package doc is explicit that
// leaving it requires an operator to act rather than another automatic
// decision, GFS math included.
var gfsManagedCompleteStates = map[lifecycle.State]bool{
	lifecycle.Committed:           true,
	lifecycle.RemoteDeletePending: true,
	lifecycle.Complete:            true,
}

func gfsIsManagedComplete(raw string) bool {
	return gfsManagedCompleteStates[lifecycle.State(raw)]
}

// gfsWeekdaysByName mirrors config's own validWeekdays: any
// week_starts_on value config.Validate accepts must resolve here too.
var gfsWeekdaysByName = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

// GFSDecide computes the FR-18 GFS classification for one backup set.
//
// now is the instant the calculation runs "as of" (see the package doc's
// Determinism section). cfg is expected to have already been through
// config.Validate: GFSDecide re-parses Timezone and WeekStartsOn
// defensively, but does not itself apply config's zero-value defaults
// (daily_days: 7 and so on). A tier whose configured window is zero or
// negative is treated as disabled (it selects nothing), not as an error,
// since a caller that bypasses Validate has no other way to spell that.
// That reading is per tier only: a chain in which every tier is disabled
// is refused, because "keeps nothing" is not a retention policy (see
// gfsResolveChain).
//
// The chain GFSDecide decides with is cfg.EffectiveTiers(): the explicit
// cfg.Tiers list when one is configured, and otherwise the three legacy
// daily_days/weekly_months/monthly_months scalars expanded through
// config.DefaultTierChain. That expansion lives in internal/config rather
// than here so there is exactly one definition of what the old keys mean,
// and it is what makes a config file written before FR-18 was generalized
// produce the identical decisions it always has.
//
// A tier GFSDecide cannot evaluate at all (an unknown granularity, a
// custom period with no length, an empty name), and a whole chain in
// which no tier is enabled, are errors, not a chain that quietly selects
// nothing. The difference matters because this
// output feeds FR-20: a chain silently reduced to "keeps nothing" would
// turn a config typo into a proposal to delete every backup in the set.
//
// records must all belong to set: retention is calculated strictly per
// backup set (FR-7), and GFSDecide refuses a record from another set
// rather than silently folding it in. Records outside the managed-complete
// states (gfsManagedCompleteStates) are ignored: they are still in flight
// and outside GFS's remit, and never appear in the returned slice.
//
// The returned slice is sorted by artifact name, so its order never
// depends on the order records was passed in.
func GFSDecide(now time.Time, cfg config.Retention, set model.BackupSetID, records []state.Record) ([]GFSVerdict, error) {
	if set.IsZero() {
		return nil, fmt.Errorf("retention: GFSDecide needs a non-zero backup set id")
	}

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("retention: timezone %q: %w", cfg.Timezone, err)
	}

	weekStartDay, ok := gfsWeekdaysByName[strings.ToLower(cfg.WeekStartsOn)]
	if !ok {
		return nil, fmt.Errorf("retention: week_starts_on %q is not a day of the week", cfg.WeekStartsOn)
	}

	var eligible []gfsDated
	verdicts := make(map[model.ArtifactID]*GFSVerdict)

	for _, rec := range records {
		if rec.Artifact.Set != set {
			return nil, fmt.Errorf("retention: record %s does not belong to backup set %s (FR-7 isolation)", rec.Artifact, set)
		}
		if !gfsIsManagedComplete(rec.State) {
			continue
		}
		discovered, producer, hasProducer := gfsPlacementsFor(rec, loc)
		d := gfsDated{artifact: rec.Artifact, discovered: discovered}
		if hasProducer {
			p := producer
			d.producer = &p
		}
		eligible = append(eligible, d)
		verdicts[rec.Artifact] = &GFSVerdict{Artifact: rec.Artifact}
	}

	today := gfsCivilDateIn(now, loc)

	// tiers is a slice, not a map, deliberately: GFSVerdict.Tiers is built
	// by appending in this exact order as each tier is processed, which is
	// what makes its contents reproducible without a separate sort step.
	// The order is the order the administrator wrote the chain in.
	tiers, err := gfsResolveChain(cfg.EffectiveTiers(), today, weekStartDay)
	if err != nil {
		return nil, err
	}

	for _, tb := range tiers {
		// Two passes, unioned, never merged. See bucketkey.go's doc for
		// why folding them into one champion map per bucket would let an
		// untrusted producer timestamp displace an artifact the discovery
		// pass had kept, which is the one thing this design forbids.
		//
		// The two results are also kept apart here rather than unioned on
		// the spot, because which of them selected an artifact is the fact
		// issue #218 is about: it is recorded per tier, below, as the
		// union is formed.
		byDiscovery := gfsSelectRepresentatives(tb, eligible, gfsDiscoveryPlacement)
		byProducer := gfsSelectRepresentatives(tb, eligible, gfsProducerPlacement)

		// A tier is attributed to an artifact exactly once even when both
		// passes selected it, so a verdict never reads DAILY twice. The
		// walk is over eligible rather than over either map, because map
		// iteration order is random and this loop now decides the order
		// nothing later re-sorts: eligible is the caller's own record
		// order, and within one tier every artifact appends at most one
		// entry, so the resulting Tiers list is the chain's order for
		// every artifact regardless.
		for _, d := range eligible {
			inDiscovery, inProducer := byDiscovery[d.artifact], byProducer[d.artifact]
			if !inDiscovery && !inProducer {
				continue
			}
			by := GFSSelectedByBoth
			switch {
			case !inProducer:
				by = GFSSelectedByDiscovery
			case !inDiscovery:
				by = GFSSelectedByProducer
			}
			v := verdicts[d.artifact]
			v.Keep = true
			v.Tiers = append(v.Tiers, GFSTierSelection{Tier: tb.tier, By: by})
		}
	}

	out := make([]GFSVerdict, 0, len(verdicts))
	for _, v := range verdicts {
		out = append(out, *v)
	}
	sortGFSVerdicts(out)
	return out, nil
}

// gfsDated is one eligible artifact together with the placements FR-18
// offers it to a tier's buckets under: always the discovery placement, and
// the producer placement when one is admissible (nil otherwise). See
// bucketkey.go for what those two are and why there are two of them.
type gfsDated struct {
	artifact   model.ArtifactID
	discovered gfsPlacement
	producer   *gfsPlacement
}

// gfsDiscoveryPlacement and gfsProducerPlacement are the two placement
// selectors gfsSelectRepresentatives is run with. They exist as named
// functions rather than inline closures so the two passes in GFSDecide
// read as one calculation applied twice, which is exactly what they are.
func gfsDiscoveryPlacement(d gfsDated) (gfsPlacement, bool) { return d.discovered, true }

func gfsProducerPlacement(d gfsDated) (gfsPlacement, bool) {
	if d.producer == nil {
		return gfsPlacement{}, false
	}
	return *d.producer, true
}

// gfsSelectRepresentatives runs one tier's selection over one placement
// key: every artifact that place() offers a date for, that falls inside
// the tier's window, competes for its bucket, and the newest in each
// bucket is that bucket's representative. The result is the set of
// artifacts this pass selected, with each bucket contributing at most one
// of them, which is FR-18's "the newest valid backup in each of its own
// buckets" exactly as it always read.
func gfsSelectRepresentatives(tb gfsBoundTier, eligible []gfsDated, place func(gfsDated) (gfsPlacement, bool)) map[model.ArtifactID]bool {
	type champion struct {
		artifact model.ArtifactID
		occurred time.Time
	}
	champions := map[gfsCivilDate]champion{}
	for _, d := range eligible {
		p, ok := place(d)
		if !ok || !tb.inSpan(p.date) {
			continue
		}
		key := tb.bucket(p.date)
		cur, exists := champions[key]
		if !exists || gfsIsNewerRepresentative(d.artifact, p.occurred, cur.artifact, cur.occurred) {
			champions[key] = champion{artifact: d.artifact, occurred: p.occurred}
		}
	}
	out := make(map[model.ArtifactID]bool, len(champions))
	for _, c := range champions {
		out[c.artifact] = true
	}
	return out
}

// gfsIsNewerRepresentative reports whether candidate should replace
// current as a bucket's representative: FR-18 says "the newest valid
// backup in a bucket". The two times compared are the two placements'
// own instants (see bucketkey.go), so a discovery placement is compared on
// the discovery timestamp and a producer placement on the producer's own.
// Ties are broken on artifact name, which is unique within a backup set,
// so the winner never depends on which of the two GFSDecide happened to
// see first in the input slice.
func gfsIsNewerRepresentative(candidate model.ArtifactID, candidateTime time.Time, current model.ArtifactID, currentTime time.Time) bool {
	if !candidateTime.Equal(currentTime) {
		return candidateTime.After(currentTime)
	}
	return candidate.Name > current.Name
}

// sortGFSVerdicts orders out by artifact name, so GFSDecide's result never
// depends on map iteration order or on the order records were listed in.
func sortGFSVerdicts(out []GFSVerdict) {
	sort.Slice(out, func(i, j int) bool { return out[i].Artifact.Name < out[j].Artifact.Name })
}

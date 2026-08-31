// Package retention implements FR-18's deterministic GFS (grandfather-
// father-son) retention policy.
//
// For each backup set, GFSDecide classifies every managed, completed
// backup into KEEP or "not kept by GFS": KEEP is the union of the daily,
// weekly and monthly tiers, each of which retains the newest valid backup
// in every calendar bucket that falls inside that tier's look-back window.
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
	// same inputs render it identically). Nil when Keep is false.
	//
	// For the default chain that is still Daily, Weekly, Monthly.
	// TierLastKnownGood (lastknowngood.go) can appear too, but only after
	// ApplyLastKnownGood composes FR-19's protected term into a GFSDecide
	// result; it is always appended after any GFS tiers already present,
	// so the configured chain's own ordering above is unaffected.
	Tiers []GFSTier
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

	type gfsDated struct {
		artifact model.ArtifactID
		occurred time.Time // Record.DiscoveredAt, kept for representative tie-breaking
		date     gfsCivilDate
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
		eligible = append(eligible, gfsDated{
			artifact: rec.Artifact,
			occurred: rec.DiscoveredAt,
			date:     gfsCivilDateIn(rec.DiscoveredAt, loc),
		})
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
		type champion struct {
			artifact model.ArtifactID
			occurred time.Time
		}
		champions := map[gfsCivilDate]champion{}
		for _, d := range eligible {
			if !tb.inSpan(d.date) {
				continue
			}
			key := tb.bucket(d.date)
			cur, exists := champions[key]
			if !exists || gfsIsNewerRepresentative(d.artifact, d.occurred, cur.artifact, cur.occurred) {
				champions[key] = champion{artifact: d.artifact, occurred: d.occurred}
			}
		}
		for _, c := range champions {
			v := verdicts[c.artifact]
			v.Keep = true
			v.Tiers = append(v.Tiers, tb.tier)
		}
	}

	out := make([]GFSVerdict, 0, len(verdicts))
	for _, v := range verdicts {
		out = append(out, *v)
	}
	sortGFSVerdicts(out)
	return out, nil
}

// gfsIsNewerRepresentative reports whether candidate should replace
// current as a bucket's representative: FR-18 says "the newest valid
// backup in a bucket". Ties (equal DiscoveredAt) are broken on artifact
// name, which is unique within a backup set, so the winner never depends
// on which of the two GFSDecide happened to see first in the input slice.
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

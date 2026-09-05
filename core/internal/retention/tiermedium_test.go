package retention

import (
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// FR-30's last question before a source delete, and issue #239's second
// half of the one home-medium derivation: does any retention tier whose
// medium is THIS one still select this artifact?
//
// placement.Engine asks it through its TierGuard seam and treats a nil
// guard as a refusal, so until this exists the move engine physically
// cannot delete a source. What it must not become is a second answer to
// "where does this artifact belong": the protection skip and the
// unknown-tier refusal are the same two rules HomeMedium applies, read off
// the same chain, which is why they live in the same file.

func TestTierMediumSelects_AnswersPerMedium(t *testing.T) {
	chain := []config.RetentionTier{
		tierOn("daily", ""),
		tierOn("monthly", mediumWarm),
		tierOn("annual", mediumCold),
	}

	for _, tc := range []struct {
		name    string
		tiers   []GFSTierSelection
		medium  string
		want    bool
		wantWhy string // a substring of the explanation, when want is true
	}{
		{
			// The move that FR-27 exists to make: aged out of daily, so
			// nothing local wants it any more and the local source may go.
			name:   "no selecting tier names the source medium",
			tiers:  []GFSTierSelection{selection("MONTHLY", GFSSelectedByDiscovery)},
			medium: config.MediumLocal,
			want:   false,
		},
		{
			// Still inside the daily window. Its home is local, so a move
			// off local should never have been planned, and if one was,
			// this is the gate that stops it deleting the local copy.
			name:    "a selecting tier names the source medium",
			tiers:   []GFSTierSelection{selection("DAILY", GFSSelectedByDiscovery), selection("MONTHLY", GFSSelectedByDiscovery)},
			medium:  config.MediumLocal,
			want:    true,
			wantWhy: "daily",
		},
		{
			// The question is asked about the SOURCE, and the destination
			// tier selecting it is exactly the reason the move exists.
			// Answering true here would refuse every source delete there
			// has ever been.
			name:   "the destination tier selecting it is not an answer about the source",
			tiers:  []GFSTierSelection{selection("MONTHLY", GFSSelectedByDiscovery)},
			medium: mediumWarm,
			want:   true, // asked about warm_s3, and monthly IS warm_s3
		},
		{
			name:   "nothing selects it at all",
			tiers:  nil,
			medium: config.MediumLocal,
			want:   false,
		},
		{
			// FR-19's term names no window and no medium, so it is not a
			// tier saying "keep a copy HERE". HomeMedium skips it for the
			// same reason, and the two must not disagree: an artifact held
			// only by protection has no home, so if this said "yes, local
			// still wants it" the pair would be claiming both that it
			// belongs nowhere and that it must stay put.
			name:   "last-known-good protection names no medium",
			tiers:  []GFSTierSelection{selection(TierLastKnownGood, GFSSelectedByProtection)},
			medium: config.MediumLocal,
			want:   false,
		},
		{
			// Protection skipped, and a real tier behind it still read.
			name:    "protection is skipped rather than short-circuiting the rest",
			tiers:   []GFSTierSelection{selection(TierLastKnownGood, GFSSelectedByProtection), selection("DAILY", GFSSelectedByDiscovery)},
			medium:  config.MediumLocal,
			want:    true,
			wantWhy: "daily",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := GFSVerdict{Artifact: mustArtifactID(t, "production", "pg", "a.dump"), Keep: len(tc.tiers) > 0, Tiers: tc.tiers}
			got, why, err := TierMediumSelects(chain, v, tc.medium)
			if err != nil {
				t.Fatalf("TierMediumSelects: %v", err)
			}
			if got != tc.want {
				t.Fatalf("TierMediumSelects(%q) = %v (%q), want %v", tc.medium, got, why, tc.want)
			}
			if tc.want && !strings.Contains(why, tc.wantWhy) {
				t.Errorf("the explanation %q does not name %q, so an operator reading a preserved source cannot tell which tier preserved it", why, tc.wantWhy)
			}
			if !tc.want && why != "" {
				t.Errorf("a false answer carried the explanation %q; a reason for a refusal that did not happen reads as one that did", why)
			}
		})
	}
}

// TestTierMediumSelects_RefusesATierTheChainDoesNotContain pins the same
// refusal HomeMedium makes, for the same reason: a verdict naming a tier
// this chain does not have is a disagreement between two things the caller
// computed together, and the permissive reading ("no tier by that name, so
// nothing wants it here") is the one that ends in a delete.
func TestTierMediumSelects_RefusesATierTheChainDoesNotContain(t *testing.T) {
	chain := []config.RetentionTier{tierOn("daily", "")}
	v := GFSVerdict{
		Artifact: mustArtifactID(t, "production", "pg", "a.dump"),
		Keep:     true,
		Tiers:    []GFSTierSelection{selection("QUARTERLY", GFSSelectedByDiscovery)},
	}

	got, _, err := TierMediumSelects(chain, v, config.MediumLocal)
	if err == nil {
		t.Fatalf("TierMediumSelects accepted a verdict naming QUARTERLY against a daily-only chain and answered %v", got)
	}
	for _, want := range []string{"QUARTERLY", "DAILY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

// TestTierMediumSelects_AgreesWithHomeMedium is the anti-drift test the
// REFACTOR line asks for. Two derivations of "where does this belong"
// would disagree about a deletion, so this walks the same chain and the
// same verdicts through both and requires the one relationship that has to
// hold: an artifact whose home IS a medium is an artifact some tier
// selects on that medium.
func TestTierMediumSelects_AgreesWithHomeMedium(t *testing.T) {
	chain := []config.RetentionTier{
		tierOn("daily", ""),
		tierOn("monthly", mediumWarm),
		tierOn("annual", mediumCold),
	}

	for _, tiers := range [][]GFSTierSelection{
		{selection("DAILY", GFSSelectedByDiscovery)},
		{selection("DAILY", GFSSelectedByDiscovery), selection("MONTHLY", GFSSelectedByProducer)},
		{selection("MONTHLY", GFSSelectedByDiscovery)},
		{selection("ANNUAL", GFSSelectedByDiscovery)},
		{selection(TierLastKnownGood, GFSSelectedByProtection), selection("ANNUAL", GFSSelectedByDiscovery)},
		{selection(TierLastKnownGood, GFSSelectedByProtection)},
		nil,
	} {
		v := GFSVerdict{Artifact: mustArtifactID(t, "production", "pg", "a.dump"), Keep: len(tiers) > 0, Tiers: tiers}
		home, hasHome, err := HomeMedium(chain, v)
		if err != nil {
			t.Fatalf("HomeMedium(%v): %v", tiers, err)
		}
		if !hasHome {
			// No tier selects it, so no medium may claim it either.
			for _, m := range []string{config.MediumLocal, mediumWarm, mediumCold} {
				selected, why, err := TierMediumSelects(chain, v, m)
				if err != nil {
					t.Fatalf("TierMediumSelects(%v, %q): %v", tiers, m, err)
				}
				if selected {
					t.Errorf("HomeMedium says %v has no home, and TierMediumSelects says %q still wants it (%s); the two are reading the same list and must not disagree", tiers, m, why)
				}
			}
			continue
		}
		selected, _, err := TierMediumSelects(chain, v, home)
		if err != nil {
			t.Fatalf("TierMediumSelects(%v, %q): %v", tiers, home, err)
		}
		if !selected {
			t.Errorf("HomeMedium put %v on %q and TierMediumSelects says no tier there wants it; a source delete would then be allowed against the artifact's own home", tiers, home)
		}
	}
}

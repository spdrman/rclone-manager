package retention

import (
	"fmt"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// This file is FR-27's home-medium rule, and it is the ONE derivation of
// it (issue #239's REFACTOR line). The planner that decides whether an
// artifact has to move, the preview that shows an operator where its
// bytes belong, and the move engine that carries them there all read this
// function. A second answer to "where does this artifact live" is a
// second policy, and the two would disagree about a deletion.
//
// # The rule
//
// The first tier in chain order that currently selects an artifact names
// the medium that artifact belongs on. No selecting tier means no home
// and no move: the artifact stays exactly where it is.
//
// That gives chain order a second, load-bearing meaning it did not have
// before. It still cannot change WHICH artifacts are kept, because KEEP
// is a union over the whole chain and a union does not care about order.
// It now decides WHERE a multiply-selected artifact lives, and being
// selected by two tiers is the ordinary case rather than a corner: the
// first backup of a month is claimed by a daily tier and a monthly one at
// the same time. Operators write chains fine to coarse, so the first
// selecting tier is the warmest, which is what makes daily-on-local and
// monthly-offsite come out the way the story needs.
//
// # What is deliberately not a home
//
// FR-19's last-known-good protection is not a bucket selection. It is a
// fourth term in FR-18's formula, it names no calendar window, and an
// artifact held only by it has no tier saying where it belongs. Such an
// artifact stays put. Moving it would mean picking a medium from
// something that expressed no preference, and the protection exists to
// stop this manager deleting the last good copy, not to relocate it.
//
// An artifact no tier selects at all is the same answer for a blunter
// reason: it is on its way to being deleted, and moving bytes somewhere
// else first is work in the service of a delete.
//
// # It refuses a tier it cannot find
//
// A selection naming a tier the chain does not contain is a bug, not a
// default. The permissive reading would be "treat it as local", and that
// reading writes an artifact's home from a name this build did not
// understand, which is exactly how bytes end up somewhere nobody chose.
// So it is an error, and every caller has to say what it does with one.

// HomeMedium reports which medium an artifact belongs on under one
// retention chain, given the verdict that chain produced for it.
//
// hasHome is false when no tier selects the artifact, which is FR-27's
// "stays put": the caller plans no move and reads nothing into the empty
// medium string. It is deliberately a second return value rather than an
// empty medium, because config.MediumLocal is a real answer and "" is
// how a tier spells local in a config file, so a single string cannot
// carry both facts without one of them being guessed.
//
// The chain is the one that produced the verdict. Handing it a different
// chain answers a question nobody asked, which is why this takes the
// chain rather than reading one from anywhere: the caller holds the
// resolved policy the verdict was decided under (a backup set's own since
// issue #333, the deployment's otherwise) and passing it explicitly is
// what keeps those two from drifting apart.
func HomeMedium(chain []config.RetentionTier, v GFSVerdict) (medium string, hasHome bool, err error) {
	byName := make(map[GFSTier]config.RetentionTier, len(chain))
	for _, t := range chain {
		byName[gfsTierName(t.Name)] = t
	}

	for _, sel := range v.Tiers {
		if sel.By == GFSSelectedByProtection {
			// FR-19's term, which names no window and therefore no home.
			// Skipped rather than returned, because a later entry in the
			// same list can still be a real tier selection: an artifact
			// both protected and inside the daily window belongs on
			// daily's medium.
			continue
		}
		t, ok := byName[sel.Tier]
		if !ok {
			return "", false, fmt.Errorf(
				"retention: the verdict for %s names tier %q, which the chain (%s) does not contain; "+
					"refusing to guess where this artifact belongs rather than defaulting it to %s",
				v.Artifact, sel.Tier, tierNameList(chain), config.MediumLocal)
		}
		return t.EffectiveMedium(), true, nil
	}
	return "", false, nil
}

// TierMediumSelects answers FR-30's last question before a source delete:
// does any tier whose medium is `medium` still select this artifact?
//
// It is the other half of the one home-medium derivation, and it lives
// beside HomeMedium so the two cannot come to read the chain differently.
// The two rules that make this a policy rather than a lookup are both
// HomeMedium's own: FR-19's protection names no medium and is skipped, and
// a verdict naming a tier this chain does not contain is refused rather
// than read as "then nothing wants it here", which is the permissive
// reading that ends in a delete.
//
// The explanation is returned only with a true answer, and it names the
// tier and the medium in the config file's own spelling, because the one
// place it surfaces is a preserved source an operator has to understand.
//
// # Why this is not simply "the home is not this medium"
//
// FR-30 asks about ANY selecting tier, not about the first one, and the
// difference is real in a chain an operator wrote coarse to fine, or in
// one that changed under a move already in flight. If monthly (s3) is
// first and daily (local) second, an artifact both select has its home on
// s3 and a local copy that daily still wants. Answering from the home
// alone would say local is free to delete. So the whole list is walked,
// and the relationship that must hold, an artifact's own home always
// selecting it, is pinned by a test rather than assumed here.
func TierMediumSelects(chain []config.RetentionTier, v GFSVerdict, medium string) (selected bool, why string, err error) {
	byName := make(map[GFSTier]config.RetentionTier, len(chain))
	for _, t := range chain {
		byName[gfsTierName(t.Name)] = t
	}

	for _, sel := range v.Tiers {
		if sel.By == GFSSelectedByProtection {
			// FR-19's term, which names no window and therefore no medium.
			// Skipped rather than answered, for HomeMedium's reason: a
			// protected artifact also inside a real tier's window is still
			// wanted by that tier, and short-circuiting here would lose it.
			continue
		}
		t, ok := byName[sel.Tier]
		if !ok {
			return false, "", fmt.Errorf(
				"retention: the verdict for %s names tier %q, which the chain (%s) does not contain; "+
					"refusing to guess whether a copy on %q is still wanted rather than reading a name this build did not understand as a no",
				v.Artifact, sel.Tier, tierNameList(chain), medium)
		}
		if t.EffectiveMedium() == medium {
			return true, fmt.Sprintf("the %s tier selects it (%s) and its medium is %q", t.Name, sel.By, medium), nil
		}
	}
	return false, "", nil
}

// tierNameList renders a chain's tier names for the refusal above, in
// chain order, so the message says what WAS available rather than only
// what was not.
func tierNameList(chain []config.RetentionTier) string {
	if len(chain) == 0 {
		return "empty"
	}
	names := make([]string, 0, len(chain))
	for _, t := range chain {
		names = append(names, string(gfsTierName(t.Name)))
	}
	return strings.Join(names, ", ")
}

// HomeMove is one artifact that is not where the chain says it belongs.
//
// It is a statement about placement and nothing else. Planning a move
// never changes a verdict, never adds an artifact to KEEP and never
// removes one, which is FR-32's union direction applied to the one thing
// in this EPIC that touches bytes.
type HomeMove struct {
	Artifact model.ArtifactID

	// From is the medium the artifact's ACTIVE placement is on today,
	// and To is the medium its home tier names. They are always
	// different: an artifact already at home is not a move.
	From string
	To   string
}

// HomePlan is what one retention pass worked out about where a backup
// set's artifacts live, as opposed to whether they are kept.
type HomePlan struct {
	// Moves is every artifact whose home differs from where it is, in
	// verdict order.
	Moves []HomeMove

	// Unconfirmed is every artifact whose current placement could not be
	// read, in verdict order. No move is planned for one, and that is the
	// whole reason this field exists rather than the artifact being
	// silently skipped: "I could not confirm where this is" is a fact an
	// operator has to be shown, and it is different from "this is already
	// at home".
	//
	// See PlanHomeMoves for why an unconfirmed placement can never be
	// planned around.
	Unconfirmed []model.ArtifactID
}

// PlanHomeMoves works out which artifacts are not on the medium their
// chain says they belong on (FR-27's home rule, issue #239).
//
// where answers "which medium is this artifact's durable copy on right
// now", and the status it returns is the load-bearing part. A placement
// row means a DURABLE copy: an artifact still transferring deliberately
// has no row, so anything but LocationConfirmed means "I cannot confirm
// where this is", never "it is not there".
//
// Those two readings are not interchangeable, and the difference is a
// deletion. A move ends by deleting its source, so planning one from a
// location this manager could not confirm is planning a delete against a
// copy it never established exists. Reading a missing row as "not
// present" would additionally plan an upload of bytes that may already be
// on the destination, from a source that may be the only copy. So an
// artifact whose placement cannot be confirmed is reported and never
// moved, which leaves it exactly where it is, which is the direction that
// cannot lose data.
//
// It plans no move for an artifact with no home either: an artifact that
// no tier selects is on its way out, and copying bytes somewhere else
// first is work in the service of a delete.
func PlanHomeMoves(chain []config.RetentionTier, verdicts []GFSVerdict, where ArtifactLocator) (HomePlan, error) {
	if where == nil {
		return HomePlan{}, fmt.Errorf("retention: PlanHomeMoves needs a way to read where an artifact currently is; " +
			"without one every artifact would look unplaced, and an unplaced artifact is one this manager must not move")
	}

	var plan HomePlan
	for _, v := range verdicts {
		home, hasHome, err := HomeMedium(chain, v)
		if err != nil {
			return HomePlan{}, err
		}
		if !hasHome {
			continue
		}
		loc := where(v.Artifact)
		// Both non-confirmed statuses land here, and here they really are
		// the same answer, which is what makes this different from the
		// prune's own split (see LocationUnrecorded). A move ENDS by
		// deleting a source, so it needs a location it proved; FR-20's
		// local delete proves its own target from the path instead.
		// Different proofs available, different answers to the same
		// missing row.
		if loc.Status != LocationConfirmed {
			plan.Unconfirmed = append(plan.Unconfirmed, v.Artifact)
			continue
		}
		if loc.Medium == home {
			continue
		}
		plan.Moves = append(plan.Moves, HomeMove{Artifact: v.Artifact, From: loc.Medium, To: home})
	}
	return plan, nil
}

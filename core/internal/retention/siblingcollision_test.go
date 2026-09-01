package retention

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// Issue #292. GFSDecide selects at most one representative per bucket
// per tier, and that is FR-18 read exactly as written. It also assumes
// one artifact IS one restore point. When a producer writes a restore
// point as several files sharing one run timestamp (a `gitea dump`
// archive and a `pg_dump` of the same database, in the issue's own
// reproduction), the two files tie on that shared timestamp in every
// bucket they both land in, gfsIsNewerRepresentative's deterministic
// name tie-break silently promotes one of them, and the other comes back
// Keep == false, Tiers == nil: the exact same shape a genuinely
// superseded artifact has. Nothing before this file existed to tell the
// two apart.
//
// This file's job is narrow, matching the issue's own scoped-down ask:
// not to stop the split (that is a real remodel touching FR-19, `status`
// and the delete gate, and is explicitly out of scope), but to make the
// split impossible to mistake for an ordinary "older than every window"
// delete. GFSVerdict.SiblingCollisions is that signal.

// --- helpers (prefixed sc* so they cannot collide with this package's
// other test files' own helpers) ---

// scAt reuses taAt (tierattribution_test.go): both files live in this
// same package and there is no reason for two RFC3339 parsers.

// scCollisionSiblings extracts, for every verdict with at least one
// recorded collision, the set of sibling names it collided with, sorted,
// so a test can assert "these two collided with each other" without
// caring about tier/placement ordering.
func scCollisionSiblings(v GFSVerdict) []string {
	if len(v.SiblingCollisions) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, c := range v.SiblingCollisions {
		seen[c.Sibling.Name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestGFSDecideStillSplitsIdenticallyTimestampedSiblings is the baseline:
// it pins that the split itself is untouched by this issue's fix (the
// scope decision explicitly keeps GFSDecide's per-bucket "at most one
// representative" behaviour; only the visibility of the split changes).
// If this test starts failing, something started changing the KEEP/DELETE
// split itself, which is the bigger remodel this issue's fix deliberately
// does not build.
func TestGFSDecideStillSplitsIdenticallyTimestampedSiblings(t *testing.T) {
	now, cfg, set, specs := scGiteaRunFixture(t)
	verdicts, err := GFSDecide(now, cfg, set, taRecords(t, set, specs, ""))
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}

	gfsAssertKeptNames(t, verdicts, []string{"gitea-dump-20260901T033000Z.tar.gz"})

	var loser *GFSVerdict
	for i := range verdicts {
		if verdicts[i].Artifact.Name == "gitea-db-20260901T033000Z.dump" {
			loser = &verdicts[i]
		}
	}
	if loser == nil {
		t.Fatalf("no verdict at all for the losing sibling; want Keep=false, not absent")
	}
	if loser.Keep {
		t.Fatalf("gitea-db-20260901T033000Z.dump: Keep = true, want false (the tie-broken loser); the reproduction in issue #292 depends on this artifact losing")
	}
	if len(loser.Tiers) != 0 {
		t.Errorf("gitea-db-20260901T033000Z.dump: Tiers = %v, want empty (tiers=[] is the exact symptom the issue reports)", loser.Tiers)
	}
}

// TestGFSDecideFlagsTheSiblingThatLostTheTimestampTie is issue #292's
// actual acceptance criterion: the losing artifact's verdict must name
// the sibling it tied with and lost to, so `retention --dry-run` can
// print something other than a bare, indistinguishable tiers=[].
func TestGFSDecideFlagsTheSiblingThatLostTheTimestampTie(t *testing.T) {
	now, cfg, set, specs := scGiteaRunFixture(t)
	verdicts, err := GFSDecide(now, cfg, set, taRecords(t, set, specs, ""))
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}

	var winner, loser *GFSVerdict
	for i := range verdicts {
		switch verdicts[i].Artifact.Name {
		case "gitea-dump-20260901T033000Z.tar.gz":
			winner = &verdicts[i]
		case "gitea-db-20260901T033000Z.dump":
			loser = &verdicts[i]
		}
	}
	if winner == nil || loser == nil {
		t.Fatalf("fixture broken: winner=%v loser=%v", winner, loser)
	}

	gotLoser := scCollisionSiblings(*loser)
	wantLoser := []string{"gitea-dump-20260901T033000Z.tar.gz"}
	if !reflect.DeepEqual(gotLoser, wantLoser) {
		t.Errorf("gitea-db-20260901T033000Z.dump: SiblingCollisions names %v, want %v (full: %+v)", gotLoser, wantLoser, loser.SiblingCollisions)
	}

	for _, c := range loser.SiblingCollisions {
		if c.By != GFSSelectedByProducer {
			t.Errorf("collision %+v: By = %s, want %s (the fixture ties on the producer timestamp only)", c, c.By, GFSSelectedByProducer)
		}
	}

	// The winner survived on its own merits (it IS the tiebreak winner,
	// not a victim), so its own verdict must carry no collision: FR-18's
	// existing KEEP reporting (tiers=[DAILY(producer) ...]) already tells
	// the operator why it was kept, and attaching a collision here would
	// contradict this field's own "populated only when Keep is false"
	// contract.
	if got := scCollisionSiblings(*winner); got != nil {
		t.Errorf("gitea-dump-20260901T033000Z.tar.gz (the winner): SiblingCollisions names = %v, want nil", got)
	}
}

// TestGFSDecideDoesNotFlagAGenuinelyOlderArtifactAsASiblingCollision is
// the positive control the issue's own acceptance criteria demands:
// `retention --dry-run` has to distinguish "no tier claimed this because
// it is older than every window" from "no tier claimed this because a
// sibling in the same bucket won" — which means an artifact that lost a
// bucket to a genuinely newer, unrelated backup (not a timestamp tie)
// must NOT come back flagged as a sibling collision. Without this test,
// a mutant that flags every DELETE verdict would pass the two tests
// above for the wrong reason.
func TestGFSDecideDoesNotFlagAGenuinelyOlderArtifactAsASiblingCollision(t *testing.T) {
	now, cfg, set, specs := scGiteaRunFixture(t)
	// A third artifact, a real, distinct, older backup in the very same
	// daily/weekly/monthly buckets as the tied pair (same civil date,
	// same week, same month), but at a different instant, not a tie.
	// gfsIsNewerRepresentative's own ordering keeps it from ever winning
	// its bucket against the fresher pair, which is ordinary GFS
	// behaviour and must render exactly as it always has: tiers=[], no
	// collision.
	specs = append(specs, recSpecWithProducer{
		name:       "gitea-dump-20260901T010000Z.tar.gz",
		discovered: taAt(t, "2026-09-01T01:05:00Z"),
		producer:   taAt(t, "2026-09-01T01:00:00Z"),
	})

	verdicts, err := GFSDecide(now, cfg, set, taRecords(t, set, specs, ""))
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}

	var older *GFSVerdict
	for i := range verdicts {
		if verdicts[i].Artifact.Name == "gitea-dump-20260901T010000Z.tar.gz" {
			older = &verdicts[i]
		}
	}
	if older == nil {
		t.Fatalf("no verdict for the older, unrelated artifact")
	}
	if older.Keep {
		t.Fatalf("the older artifact unexpectedly won its bucket; fixture is broken")
	}
	if got := scCollisionSiblings(*older); got != nil {
		t.Errorf("gitea-dump-20260901T010000Z.tar.gz: SiblingCollisions names = %v, want nil (it lost to a genuinely newer backup, not a timestamp tie)", got)
	}
}

// scGiteaRunFixture is the issue's own reproduction, transcribed as a
// fixture: a Gitea backup run that writes a `gitea dump` archive and a
// `pg_dump` database dump, both carrying the same run timestamp, into one
// backup set. "now" and the chain mirror taDecisiveFixture so this stays
// inside every tier's window without needing its own arithmetic.
func scGiteaRunFixture(t *testing.T) (time.Time, config.Retention, model.BackupSetID, []recSpecWithProducer) {
	t.Helper()
	set := gfsMustSet(t, "cicd-pipeline", "gitea-forge")
	now := taAt(t, "2026-09-01T12:00:00Z")
	cfg := config.Retention{
		Timezone:      "UTC",
		WeekStartsOn:  "monday",
		DailyDays:     7,
		WeeklyMonths:  3,
		MonthlyMonths: 12,
	}
	runTime := taAt(t, "2026-09-01T03:30:00Z")
	specs := []recSpecWithProducer{
		// Both files: the same run timestamp on the remote (the producer
		// placement), and, per the issue, discovered together — the
		// discovery placement is left to differ slightly (a batch
		// discovery pass rarely reads the clock at literally the same
		// nanosecond for two files), which is deliberate: it proves the
		// collision this test is about comes from the producer tie, not
		// from also rigging the discovery timestamps to match.
		{name: "gitea-dump-20260901T033000Z.tar.gz", discovered: runTime.Add(2 * time.Second), producer: runTime},
		{name: "gitea-db-20260901T033000Z.dump", discovered: runTime.Add(1 * time.Second), producer: runTime},
	}
	return now, cfg, set, specs
}

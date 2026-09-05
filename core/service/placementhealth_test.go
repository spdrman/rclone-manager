package service

import (
	"reflect"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/health"
)

// Issue #444's third acceptance line: `backup-manager status` and the Web
// UI read the same computation, which is the property this package exists
// to keep.
//
// The computation itself is internal/health's, and internal/app's test
// suite drives it end to end through a real move engine. What can still
// go wrong is here: this package projects internal/health's value into a
// provider-agnostic one, by hand, field by field, and a field added to
// the first and forgotten in the second reads as a permanent zero on
// every UI and API surface while the CLI prints the truth. That is the
// same defect this issue is fixing, one layer out.

// fullPlacementHealth is a health.PlacementHealth with every field set to
// something distinguishable from its zero value.
//
// Every field non-zero is the point, not thoroughness: a fixture that
// leaves a field at zero cannot tell a mapper that carries it from one
// that drops it, so the test below would keep passing while the field
// quietly stopped reaching anybody.
func fullPlacementHealth() health.PlacementHealth {
	away := 40 * 24 * time.Hour
	open := 9 * 24 * time.Hour
	failed := 7 * 24 * time.Hour
	return health.PlacementHealth{
		AwayFromHome:          3,
		OldestAwayFromHomeAge: &away,
		UnconfirmedLocation:   2,
		OpenMoves:             4,
		OldestOpenMoveAge:     &open,
		FailedMoves:           1,
		OldestFailedMoveAge:   &failed,
		FailedMoveReason:      "the bucket policy denies PutObject for this key prefix",
	}
}

func TestToServiceBackupSetHealth_CarriesEveryPlacementFact(t *testing.T) {
	got := toServiceBackupSetHealth(health.BackupSetHealth{Placement: fullPlacementHealth()}).Placement

	// The control for the scan below: a mapper that dropped everything
	// would leave the whole struct zero, and a zero-vs-zero comparison
	// proves nothing about a mapper.
	if (got == PlacementHealth{}) {
		t.Fatal("the whole placement block is the zero value; nothing in it reached core/service")
	}

	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("PlacementHealth.%s is zero after mapping a fully-populated health.PlacementHealth: this fact reaches the CLI and not the UI",
				v.Type().Field(i).Name)
		}
	}

	if got.AwayFromHome != 3 || got.UnconfirmedLocation != 2 || got.OpenMoves != 4 || got.FailedMoves != 1 {
		t.Errorf("counts came through wrong: %+v", got)
	}
	if got.OldestFailedMoveAge != 7*24*time.Hour {
		t.Errorf("OldestFailedMoveAge = %s, want a week: the age is the difference between a blip and a wedge", got.OldestFailedMoveAge)
	}
}

// TestToServiceBackupSetHealth_LeavesAMissingAgeAtZero pins the other
// direction. internal/health reports an age as a pointer because there
// genuinely is no age when nothing is away from home, and this package
// reports plain durations. The rule that makes that safe is that a zero
// age always sits beside a zero count, so nothing has to guess.
func TestToServiceBackupSetHealth_LeavesAMissingAgeAtZero(t *testing.T) {
	got := toServiceBackupSetHealth(health.BackupSetHealth{}).Placement

	if got != (PlacementHealth{}) {
		t.Fatalf("Placement = %+v, want the zero value for a set with nothing to say", got)
	}
}

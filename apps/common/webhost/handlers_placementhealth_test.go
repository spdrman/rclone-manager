package webhost

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #444's wire half. The verdict itself is internal/health's and is
// driven end to end through a real move engine in core/internal/app; what
// can still go wrong here is the projection.
//
// GET /api/v1/system/health is hand-mapped from service.BackupSetHealth,
// field by field, and the way a hand-written projection breaks is by
// dropping one: the result still compiles, still serialises, and reports a
// confident zero for something the core computed correctly. For this block
// that is exactly the defect the issue is about, one layer out, because
// zero is the resting value of every count in it. A UI would render "no
// backups are out of place" for a deployment where relocations had been
// failing for a month.

// placementWireNames maps each field of service.PlacementHealth onto the
// response field that carries it.
//
// It is hand-written, which is the hole a forgotten field would hide in,
// so the test below closes it from the source side first: every field of
// service.PlacementHealth must appear here, and a field added there
// without an entry fails before anything else is checked. That is the
// same shape contractBindings uses for operations.
var placementWireNames = map[string]string{
	"AwayFromHome":          "AwayFromHome",
	"OldestAwayFromHomeAge": "AwayFromHomeOldestAgeSeconds",
	"UnconfirmedLocation":   "UnconfirmedLocation",
	"OpenMoves":             "OpenMoves",
	"OldestOpenMoveAge":     "OpenMoveOldestAgeSeconds",
	"FailedMoves":           "FailedMoves",
	"OldestFailedMoveAge":   "FailedMoveOldestAgeSeconds",
	"FailedMoveReason":      "FailedMoveReason",
}

// fullPlacementHealth is a service.PlacementHealth with every field set to
// something distinguishable from its zero value, and every number
// distinct, so a mapping that crossed two fields fails rather than passing
// on a pair that happened to match.
func fullPlacementHealth() service.PlacementHealth {
	return service.PlacementHealth{
		AwayFromHome:          3,
		OldestAwayFromHomeAge: 40 * 24 * time.Hour,
		UnconfirmedLocation:   2,
		OpenMoves:             4,
		OldestOpenMoveAge:     9 * 24 * time.Hour,
		FailedMoves:           1,
		OldestFailedMoveAge:   7 * 24 * time.Hour,
		FailedMoveReason:      "the bucket policy denies PutObject for this key prefix",
	}
}

func TestToBackupSetHealthResponse_CarriesEveryPlacementFact(t *testing.T) {
	full := fullPlacementHealth()

	// Source side first: a fact this package has never heard of cannot be
	// asserted about below, so the completeness of the map is checked
	// before the mapping is.
	src := reflect.ValueOf(full)
	for i := 0; i < src.NumField(); i++ {
		name := src.Type().Field(i).Name
		if src.Field(i).IsZero() {
			t.Fatalf("the fixture leaves service.PlacementHealth.%s at its zero value, so this test cannot tell a mapper that carries it from one that drops it", name)
		}
		if _, ok := placementWireNames[name]; !ok {
			t.Fatalf("service.PlacementHealth.%s has no entry in placementWireNames, so nothing here knows whether it reaches the API at all; add it to the map and to backupSetHealthResponse", name)
		}
	}

	got := reflect.ValueOf(toBackupSetHealthResponse(service.BackupSetHealth{Placement: full}))
	for i := 0; i < src.NumField(); i++ {
		name := src.Type().Field(i).Name
		field := got.FieldByName(placementWireNames[name])
		if !field.IsValid() {
			t.Errorf("backupSetHealthResponse has no field %q for service.PlacementHealth.%s", placementWireNames[name], name)
			continue
		}
		if field.IsZero() {
			t.Errorf("backupSetHealthResponse.%s is zero after mapping a fully-populated placement block: service.PlacementHealth.%s reaches the CLI and not this endpoint",
				placementWireNames[name], name)
		}
	}
}

// TestSystemHealth_ServesThePlacementBlock is the same question asked of
// the bytes on the wire rather than of the struct, because a correct
// mapping can still be invisible to a client: a count tagged omitempty
// disappears at its resting value, which is the one reading this block
// exists to distinguish from "this build does not know".
func TestSystemHealth_ServesThePlacementBlock(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.health = service.HealthReport{
		GeneratedAt: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
		BackupSets: []service.BackupSetHealth{
			{
				BackupSetID: "production/postgres", SourceName: "production", SetName: "postgres",
				State: "DEGRADED", Reason: "a relocation this backup set's retention chain asked for is failing",
				StaleAfter: 24 * time.Hour,
				Placement:  fullPlacementHealth(),
			},
			{
				// The set with nothing to say, in the same body, so the
				// two assertions below are about presence and absence
				// rather than about one shape.
				BackupSetID: "production/media", SourceName: "production", SetName: "media",
				State: "HEALTHY", Reason: "fresh", StaleAfter: 24 * time.Hour,
			},
		},
	}

	rec := rt.get(t, "/api/v1/system/health")
	mustStatus(t, rec, http.StatusOK)

	var body struct {
		BackupSets []map[string]any `json:"backup_sets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the health body: %v", err)
	}
	if len(body.BackupSets) != 2 {
		t.Fatalf("len(backup_sets) = %d, want 2", len(body.BackupSets))
	}
	degraded, healthy := body.BackupSets[0], body.BackupSets[1]

	for key, want := range map[string]float64{
		"away_from_home":                    3,
		"away_from_home_oldest_age_seconds": 40 * 24 * 3600,
		"unconfirmed_location":              2,
		"open_moves":                        4,
		"open_move_oldest_age_seconds":      9 * 24 * 3600,
		"failed_moves":                      1,
		"failed_move_oldest_age_seconds":    7 * 24 * 3600,
	} {
		got, ok := degraded[key]
		if !ok {
			t.Errorf("the body has no %q at all", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if degraded["failed_move_reason"] != "the bucket policy denies PutObject for this key prefix" {
		t.Errorf("failed_move_reason = %v; the count alone cannot tell an operator whether to look at a storage policy, a credential or a network", degraded["failed_move_reason"])
	}

	// The counts are served at their resting value, and that is the whole
	// polarity argument: an absent count reads as "this build does not
	// know", which is the state this issue exists to end. The ages are the
	// other way round, because an age only exists when there is something
	// to be the age of and the count beside it always says whether there
	// is.
	for _, key := range []string{"away_from_home", "unconfirmed_location", "open_moves", "failed_moves"} {
		got, ok := healthy[key]
		if !ok {
			t.Errorf("a backup set with nothing out of place omits %q; zero is a real reading here, not a missing one", key)
			continue
		}
		if got != float64(0) {
			t.Errorf("%s = %v for a set with nothing out of place, want 0", key, got)
		}
	}
	for _, key := range []string{"away_from_home_oldest_age_seconds", "open_move_oldest_age_seconds", "failed_move_oldest_age_seconds", "failed_move_reason"} {
		if _, ok := healthy[key]; ok {
			t.Errorf("a backup set with nothing out of place serves %q; there is nothing for it to be the age of, and a zero would read as \"just now\"", key)
		}
	}
}

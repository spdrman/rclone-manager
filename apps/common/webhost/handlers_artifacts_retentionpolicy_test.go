// Issue #523 at the HTTP boundary: GET /api/v1/backups has to tell an
// artifact under a retention chain apart from one under nothing at all.
//
// The list already carried both kinds of row (core/service.ListArtifacts
// widens an unfiltered read to the backup sets the configuration no
// longer names, #391), and until this field existed a client had no way
// to see the difference. So the rows an operator most needs to find, the
// ones nothing will ever delete, were the ones the response made look
// exactly like every healthy backup beside them.
//
// Every case here checks the two answers against each other, because a
// projection stuck on either value passes a one-sided test and both are
// broken in opposite ways.
package webhost

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// ungovernedArtifactFixture is a backup whose set was removed: still on
// storage, still listed, and under nothing.
var ungovernedArtifactFixture = service.Artifact{
	ID:              "production/retired/old.dump",
	BackupSetID:     "production/retired",
	SourceName:      "production",
	SetName:         "retired",
	Name:            "old.dump",
	RemotePath:      "/backups/old.dump",
	LocalPath:       "/data/backups/old.dump",
	State:           "COMPLETE",
	DiscoveredAt:    testArtifactFixture.DiscoveredAt,
	UpdatedAt:       testArtifactFixture.UpdatedAt,
	SizeBytes:       8192,
	Validation:      "passed",
	RetentionPolicy: service.RetentionPolicyNone,
}

// TestListArtifacts_TellsAGovernedBackupApartFromOneUnderNoPolicy serves
// both kinds in one response, which is what makes each half of it mean
// something: a mapper hard-wired to either value fails here, on the row
// that names the mistake.
func TestListArtifacts_TellsAGovernedBackupApartFromOneUnderNoPolicy(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.artifacts = []service.Artifact{testArtifactFixture, ungovernedArtifactFixture}

	rec := rt.get(t, "/api/v1/backups")
	mustStatus(t, rec, http.StatusOK)

	var body listArtifactsResponse
	decodeInto(t, rec, &body)
	if len(body.Artifacts) != 2 {
		t.Fatalf("len = %d, want 2; both rows are needed or only one direction is proved", len(body.Artifacts))
	}

	got := map[string]string{}
	for _, a := range body.Artifacts {
		got[a.ID] = a.RetentionPolicy
	}
	if want := "configured"; got[testArtifactFixture.ID] != want {
		t.Errorf("retention_policy for a backup whose set is configured = %q, want %q. Flagging a governed backup as ungoverned is how an operator learns to ignore the flag",
			got[testArtifactFixture.ID], want)
	}
	if want := "none"; got[ungovernedArtifactFixture.ID] != want {
		t.Errorf("retention_policy for a backup whose set was removed = %q, want %q. Nothing selects it, nothing expires it, and nothing here will ever delete it: the response has to say so",
			got[ungovernedArtifactFixture.ID], want)
	}
}

// TestGetArtifact_SaysWhichPolicyGovernsTheBackupItServes. The detail
// route is where an operator lands after clicking the flagged row, and it
// reads through the same projection, so it has to agree with the list it
// came from.
func TestGetArtifact_SaysWhichPolicyGovernsTheBackupItServes(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.artifacts = []service.Artifact{testArtifactFixture, ungovernedArtifactFixture}

	for _, tc := range []struct {
		artifact service.Artifact
		want     string
	}{
		{testArtifactFixture, "configured"},
		{ungovernedArtifactFixture, "none"},
	} {
		rec := rt.get(t, "/api/v1/backups/"+tc.artifact.ID)
		mustStatus(t, rec, http.StatusOK)

		var body artifactResponse
		decodeInto(t, rec, &body)
		if body.RetentionPolicy != tc.want {
			t.Errorf("GET /backups/%s retention_policy = %q, want %q", tc.artifact.ID, body.RetentionPolicy, tc.want)
		}
	}
}

// TestListArtifacts_AlwaysPutsRetentionPolicyOnTheWire. Not omitempty,
// for either value.
//
// An omitted field is a field a client has to invent an answer for, and
// the convenient invention is "governed", which is the one answer that
// hides the row this whole feature exists to surface. The contract makes
// it required; this proves the encoder agrees, including for the value
// that would be dropped if somebody ever reached for omitempty.
func TestListArtifacts_AlwaysPutsRetentionPolicyOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name     string
		artifact service.Artifact
	}{
		{"a governed backup", testArtifactFixture},
		{"a backup under no policy", ungovernedArtifactFixture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newReadSurfaceRouter(t)
			rt.backend.artifacts = []service.Artifact{tc.artifact}

			rec := rt.get(t, "/api/v1/backups")
			mustStatus(t, rec, http.StatusOK)

			if !strings.Contains(rec.Body.String(), `"retention_policy"`) {
				t.Fatalf("the body carries no retention_policy at all: %s", rec.Body.String())
			}
			// Decoded into a map rather than the struct, so this reads
			// what the encoder actually emitted rather than what a Go
			// zero value would supply on the way back in.
			var raw struct {
				Artifacts []map[string]any `json:"artifacts"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decoding %s: %v", rec.Body.String(), err)
			}
			if len(raw.Artifacts) != 1 {
				t.Fatalf("len = %d, want 1", len(raw.Artifacts))
			}
			if _, present := raw.Artifacts[0]["retention_policy"]; !present {
				t.Errorf("retention_policy is absent from the served object: %v", raw.Artifacts[0])
			}
		})
	}
}

// TestListArtifacts_AnUnansweredRetentionPolicyServesNoneRatherThanConfigured
// pins which way the handler's guard falls.
//
// core/service answers on every artifact it produces, so an empty string
// here means a value nobody set, and the two possible guesses are not
// symmetric: guessing "configured" hides a backup nothing will delete
// behind a healthy-looking row, and guessing "none" at worst points an
// operator at a backup that turns out to be fine. This repository has
// already shipped a mapper that resolved a missing answer to the
// reassuring one, so the direction is pinned rather than assumed.
func TestListArtifacts_AnUnansweredRetentionPolicyServesNoneRatherThanConfigured(t *testing.T) {
	unanswered := testArtifactFixture
	unanswered.RetentionPolicy = ""

	rt := newReadSurfaceRouter(t)
	rt.backend.artifacts = []service.Artifact{unanswered}

	rec := rt.get(t, "/api/v1/backups")
	mustStatus(t, rec, http.StatusOK)

	var body listArtifactsResponse
	decodeInto(t, rec, &body)
	if len(body.Artifacts) != 1 {
		t.Fatalf("len = %d, want 1", len(body.Artifacts))
	}
	if got := body.Artifacts[0].RetentionPolicy; got != "none" {
		t.Errorf("retention_policy for an artifact nobody answered for = %q, want \"none\"", got)
	}
}

// TestToArtifactResponse_CarriesAPolicyNameThroughRatherThanFlatteningIt.
// The guard above must stay a guard on the EMPTY value and not become a
// narrowing to the two words the contract happens to list today.
//
// core/service.Artifact.RetentionPolicy is a string precisely so that the
// eventual answer, the name of the chain a configured set is retained
// under, is a value change rather than a schema change. A handler that
// rewrote anything it did not recognise into "none" would turn that day's
// upgrade into every backup on the deployment being reported as
// abandoned.
func TestToArtifactResponse_CarriesAPolicyNameThroughRatherThanFlatteningIt(t *testing.T) {
	named := testArtifactFixture
	named.RetentionPolicy = "deployment_gfs"

	if got := toArtifactResponse(named).RetentionPolicy; got != "deployment_gfs" {
		t.Errorf("retention_policy = %q, want the value core/service set (\"deployment_gfs\") carried through", got)
	}
}

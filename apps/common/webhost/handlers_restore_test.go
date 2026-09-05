// Asking for a restore over HTTP, and the matcher that keeps the refusals
// honest.
//
// The forbidden-readings test is a positive control for another test in
// this file, and it is the reason both are trustworthy. One case asserts a
// refused restore never reads as something it is not; that assertion is
// only worth anything if the matcher behind it can actually match, so the
// matcher is driven against strings it must catch. Without that, a matcher
// that never matched would make the first test pass forever.
//
// The gate case pins that a restore is behind the destructive gate, which
// is the tier this route belongs in: it is the one read-shaped operation
// here that spends real money and moves real objects.
package webhost

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// TestARestoreCanActuallyBeAskedForOverHTTP is the route-level half of
// #241's honesty problem.
//
// Everything internal/archive builds is worth nothing if no operator can
// reach it, and before this route existed POST /api/v1/operations refused
// every action but run_cycle by name. So this asserts the whole path: the
// action is accepted, every field the operator typed reaches the service
// unchanged, and what comes back says how long it takes and that it is
// billed.
//
// The field-for-field check on lastRestore is the load-bearing part. A
// handler that returned 202 while dropping window_days would pass a
// status-code assertion and restore somebody's backup for a day instead of
// a fortnight.
func TestARestoreCanActuallyBeAskedForOverHTTP(t *testing.T) {
	env := newOperationsTestRouter(t, alwaysPassGate{})

	rec := submitOperation(t, env.router, "idem-restore-1", `{
		"action": "restore_placement",
		"config_revision": "`+env.backend.ConfigRevision()+`",
		"restore": {
			"artifact_id": "production/postgres/dump.zst",
			"medium": "cold-store",
			"window_days": 14,
			"acknowledged": true
		}
	}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}

	got := env.backend.lastRestore
	if got.ArtifactID != "production/postgres/dump.zst" {
		t.Errorf("artifact = %q, want production/postgres/dump.zst", got.ArtifactID)
	}
	if got.Medium != "cold-store" {
		t.Errorf("medium = %q, want cold-store", got.Medium)
	}
	if got.WindowDays != 14 {
		t.Errorf("window = %d days, want 14; a dropped window restores somebody's backup for the wrong length of time and bills for it", got.WindowDays)
	}
	if !got.Acknowledged {
		t.Error("the acknowledgement did not cross the boundary, so the one field that stops an accidental restore was dropped")
	}
	if got.IdempotencyKey != "idem-restore-1" {
		t.Errorf("idempotency key = %q, want idem-restore-1", got.IdempotencyKey)
	}
	if got.Actor != "alice" {
		t.Errorf("actor = %q, want the authenticated caller", got.Actor)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if body["action"] != service.ActionRestorePlacement {
		t.Errorf("action = %v, want %q", body["action"], service.ActionRestorePlacement)
	}
	restore, ok := body["restore"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no restore block: %s", rec.Body.String())
	}
	for _, field := range []string{"wait", "billing", "access", "medium"} {
		if s, _ := restore[field].(string); s == "" {
			t.Errorf("the restore block says nothing under %q", field)
		}
	}
	// FR-34: no percentage, no finishing time, no amount. Asserted over
	// every string the block actually SAYS, rather than over the raw JSON:
	// an earlier version matched the whole body and tripped on its own
	// "detail" key, which contains the letters of "eta" and says nothing
	// about a prediction. Matching the wrong thing and passing is the
	// failure this repository keeps finding, and matching the wrong thing
	// and failing is how it gets found.
	if _, hasProgress := body["progress"]; hasProgress {
		t.Error("a restore response carried a progress object, and there is no such thing for a restore")
	}
	forbidden := regexp.MustCompile(`(?i)\bETA\b|\bUSD\b|percent|[0-9]\s*%|\$[0-9]`)
	for field, value := range restore {
		text, isText := value.(string)
		if !isText {
			continue
		}
		if m := forbidden.FindString(text); m != "" {
			t.Errorf("the restore block's %q says %q, and this product knows neither a completion time nor a price", field, m)
		}
	}
}

// TestTheForbiddenReadingsMatcherActuallyMatches is the positive control
// for the assertion above.
//
// A regexp that matched nothing would make that check pass on any output
// at all, which is the exact shape of vacuous proof it exists to prevent.
// So the patterns are run against the strings a well-meaning future
// surface would actually produce.
func TestTheForbiddenReadingsMatcherActuallyMatches(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\bETA\b|\bUSD\b|percent|[0-9]\s*%|\$[0-9]`)
	for _, sample := range []string{
		"restoring, 40% complete",
		"ETA 3 hours",
		"about 12 USD",
		"roughly $4.20",
		"63 percent restored",
	} {
		if !forbidden.MatchString(sample) {
			t.Errorf("the matcher lets %q through, so the assertion built on it proves nothing", sample)
		}
	}
	for _, allowed := range []string{
		"a restore of this copy is running",
		"the provider bills for retrieving an object from DEEP_ARCHIVE",
		"detail",
	} {
		if forbidden.MatchString(allowed) {
			t.Errorf("the matcher rejects %q, which says nothing this product cannot know", allowed)
		}
	}
}

// TestARestoreThatWasRefusedSaysWhichKindOfRefusalItWas drives each
// refusal to the status and code it declares in the contract.
//
// They are separate codes rather than one because they call for opposite
// next steps: reload the page, fix the request, or configure a storage
// medium. A client that could not tell them apart would retry the one that
// can never succeed.
func TestARestoreThatWasRefusedSaysWhichKindOfRefusalItWas(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "no restore parameters at all",
			body:       `{"action":"restore_placement","config_revision":"REV"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name:       "nobody acknowledged the cost",
			body:       `{"action":"restore_placement","config_revision":"REV","restore":{"artifact_id":"a/b/c","medium":"cold-store","window_days":3,"acknowledged":false}}`,
			wantStatus: http.StatusConflict,
			wantCode:   "RESTORE_REFUSED",
		},
		{
			name:       "the screen has moved on",
			body:       `{"action":"restore_placement","config_revision":"rev-from-yesterday","restore":{"artifact_id":"a/b/c","medium":"cold-store","window_days":3,"acknowledged":true}}`,
			wantStatus: http.StatusConflict,
			wantCode:   "CONFIG_REVISION_STALE",
		},
		{
			name:       "there is no such backup",
			err:        service.ErrArtifactNotFound,
			body:       `{"action":"restore_placement","config_revision":"REV","restore":{"artifact_id":"a/b/c","medium":"cold-store","window_days":3,"acknowledged":true}}`,
			wantStatus: http.StatusNotFound,
			wantCode:   "ARTIFACT_NOT_FOUND",
		},
		{
			name:       "the backup is not on that medium",
			err:        service.ErrCopyNotFound,
			body:       `{"action":"restore_placement","config_revision":"REV","restore":{"artifact_id":"a/b/c","medium":"warm-store","window_days":3,"acknowledged":true}}`,
			wantStatus: http.StatusNotFound,
			wantCode:   "COPY_NOT_FOUND",
		},
		{
			name:       "this deployment cannot reach a medium at all",
			err:        service.ErrRestoreUnavailable,
			body:       `{"action":"restore_placement","config_revision":"REV","restore":{"artifact_id":"a/b/c","medium":"cold-store","window_days":3,"acknowledged":true}}`,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "RESTORE_UNAVAILABLE",
		},
		{
			name:       "a run cycle carrying restore parameters",
			body:       `{"action":"run_cycle","config_revision":"REV","restore":{"artifact_id":"a/b/c","medium":"cold-store","window_days":3,"acknowledged":true}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOperationsTestRouter(t, alwaysPassGate{})
			env.backend.errOnRestore = tc.err

			body := strings.ReplaceAll(tc.body, "REV", env.backend.ConfigRevision())
			rec := submitOperation(t, env.router, "idem-refused", body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decoding the refusal: %v", err)
			}
			if envelope.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", envelope.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestARestoreIsBehindTheDestructiveGate is worth stating explicitly
// because a restore destroys nothing, so the obvious reading says it does
// not belong behind that gate.
//
// It belongs there because of what the gate is actually for: operations an
// operator cannot take back. A restore is accepted by the provider,
// billed, and there is no call anywhere that cancels one. That is closer
// in consequence to a deletion than to a read.
func TestARestoreIsBehindTheDestructiveGate(t *testing.T) {
	env := newOperationsTestRouter(t, NotYetImplementedGate{})

	rec := submitOperation(t, env.router, "idem-gated", `{
		"action": "restore_placement",
		"config_revision": "`+env.backend.ConfigRevision()+`",
		"restore": {"artifact_id":"a/b/c","medium":"cold-store","window_days":3,"acknowledged":true}
	}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; a gated deployment let a billable, uncancellable operation through", rec.Code)
	}
	if env.backend.lastRestore.ArtifactID != "" {
		t.Fatal("the gate refused the request and the service was asked for the restore anyway")
	}
}

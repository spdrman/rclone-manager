package webhost

import (
	"net/http"
	"strings"
	"testing"
)

// This file covers the capacity half of GET and PATCH /api/v1/settings,
// added for issue #286: the route the Settings page's storage-cap field
// reads and writes.

// TestGetSettings_CarriesTheCapacityBlock is what lets a form render the
// cap at all, and it has to arrive in bytes: the MB/GB picker is display
// only, so a wire that carried a unit would be a wire where the number and
// the unit can get out of step.
func TestGetSettings_CarriesTheCapacityBlock(t *testing.T) {
	tr := newSettingsTestRouter(t)
	backend := tr.backend
	backend.settings.Capacity.CapBytes = 107_374_182_400
	backend.settings.Capacity.WarningFreeBytes = 21_474_836_480
	backend.settings.Capacity.CriticalFreeBytes = 10_737_418_240
	backend.settings.Capacity.BackupRoot = "/data/backups"

	got := decodeSettings(t, tr.get(t))
	if got.Capacity.CapBytes != 107_374_182_400 {
		t.Errorf("capacity.cap_bytes = %d, want 107374182400", got.Capacity.CapBytes)
	}
	if got.Capacity.BackupRoot != "/data/backups" {
		t.Errorf("capacity.backup_root = %q, want /data/backups: a form has to be able to say which filesystem the cap is measured on", got.Capacity.BackupRoot)
	}
	if got.Capacity.BackupRootConfigured {
		t.Error("capacity.backup_root_configured = true for a derived root: a form must not put a derived value in an editable box")
	}
}

// TestPatchSettings_WritesACapInBytes is the write half.
func TestPatchSettings_WritesACapInBytes(t *testing.T) {
	tr := newSettingsTestRouter(t)
	backend := tr.backend

	rec := tr.patch(t, `{"capacity":{"cap_bytes":53687091200}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if backend.lastUpdate.Capacity == nil {
		t.Fatal("the backend received no capacity section")
	}
	if backend.lastUpdate.Capacity.CapBytes == nil || *backend.lastUpdate.Capacity.CapBytes != 53687091200 {
		t.Errorf("CapBytes = %v, want 53687091200", backend.lastUpdate.Capacity.CapBytes)
	}
	if backend.lastUpdate.Retention != nil {
		t.Error("a capacity-only write carried a retention section: an omitted section must reach the backend as nil, not as an empty update")
	}
}

// TestPatchSettings_AnExplicitZeroCapIsARequestNotAnOmission is the whole
// reason the field is nullable on the wire. "Remove my cap" and "I did not
// mention the cap" are opposite requests that a plain number cannot tell
// apart.
func TestPatchSettings_AnExplicitZeroCapIsARequestNotAnOmission(t *testing.T) {
	tr := newSettingsTestRouter(t)
	backend := tr.backend

	rec := tr.patch(t, `{"capacity":{"cap_bytes":0}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if backend.lastUpdate.Capacity == nil || backend.lastUpdate.Capacity.CapBytes == nil {
		t.Fatal("an explicit zero reached the backend as an omission")
	}
	if *backend.lastUpdate.Capacity.CapBytes != 0 {
		t.Errorf("CapBytes = %d, want 0", *backend.lastUpdate.Capacity.CapBytes)
	}
}

// TestPatchSettings_AnEmptyCapacitySectionIsRefused: `{"capacity":{}}`
// asks for nothing, and honouring it would rewrite the operator's config
// file, move ConfigRevision and answer 200 for a request with no content.
func TestPatchSettings_AnEmptyCapacitySectionIsRefused(t *testing.T) {
	tr := newSettingsTestRouter(t)
	backend := tr.backend

	rec := tr.patch(t, `{"capacity":{}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	if backend.lastUpdate.Capacity != nil || backend.lastUpdate.Retention != nil {
		t.Error("a refused request reached the backend anyway")
	}
}

// TestPatchSettings_AnEmptySectionBesideARealChangeIsStillRefused is the
// case a per-section check would let through. Quietly dropping half a
// request is how a settings page reports success for an edit that never
// happened.
func TestPatchSettings_AnEmptySectionBesideARealChangeIsStillRefused(t *testing.T) {
	tr := newSettingsTestRouter(t)

	rec := tr.patch(t, `{"retention":{"timezone":"UTC"},"capacity":{}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSettings_BothSectionsInOneCall is the positive control for the
// refusals above and the reason this is one generic route.
func TestPatchSettings_BothSectionsInOneCall(t *testing.T) {
	tr := newSettingsTestRouter(t)
	backend := tr.backend

	rec := tr.patch(t, `{"retention":{"timezone":"UTC"},"capacity":{"cap_bytes":1024}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if backend.lastUpdate.Retention == nil || backend.lastUpdate.Capacity == nil {
		t.Fatal("one of the two named sections did not reach the backend")
	}
}

// TestPatchSettings_AMisspelledCapacityKeyIsRefused holds the new section
// to this route's existing DisallowUnknownFields rule. A dropped key here
// answers 200 for a cap that was never set, and the operator is left
// looking at a settings page reporting the old value with no error
// anywhere to explain it.
func TestPatchSettings_AMisspelledCapacityKeyIsRefused(t *testing.T) {
	tr := newSettingsTestRouter(t)

	rec := tr.patch(t, `{"capacity":{"cap_gb":100}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cap_gb") {
		t.Errorf("the refusal does not name the key that was not understood: %s", rec.Body.String())
	}
}

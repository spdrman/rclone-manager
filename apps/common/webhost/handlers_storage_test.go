package webhost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/core/service"
)

// storageFakeBackend is a BackupServiceClient double dedicated to this
// file's tests: it delegates every operations-surface method to an
// embedded syncFakeBackend (mirroring backupSetFakeBackend's own
// pattern) and lets a test directly control what ListStorageStatus
// returns.
type storageFakeBackend struct {
	*syncFakeBackend
	statuses  []service.StorageStatus
	errOnList error
}

func newStorageFakeBackend(statuses ...service.StorageStatus) *storageFakeBackend {
	return &storageFakeBackend{syncFakeBackend: newSyncFakeBackend(), statuses: statuses}
}

func (f *storageFakeBackend) ListStorageStatus(context.Context) ([]service.StorageStatus, error) {
	if f.errOnList != nil {
		return nil, f.errOnList
	}
	return f.statuses, nil
}

func newTestRouterForStorage(t *testing.T, backend BackupServiceClient) http.Handler {
	t.Helper()
	return NewRouter(RouterConfig{
		Platform:      fakePlatformAdapter{caps: capabilities.PlatformCapabilities{}, auth: fakeAuthenticator{authenticated: true, username: "alice"}},
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "9.9.9",
		Commit:        "deadbeef",
	})
}

func TestSystemStorage_ReportsEveryBackupSetsAssessment(t *testing.T) {
	backend := newStorageFakeBackend(
		service.StorageStatus{
			BackupSetID: "alpha/one", LocalPath: "/data/alpha",
			Available: true, TotalBytes: 1_000_000_000_000, FreeBytes: 500_000_000_000,
			WarningFreeBytes: 100_000_000_000, CriticalFreeBytes: 20_000_000_000,
			Level: "OK",
		},
		service.StorageStatus{
			BackupSetID: "alpha/two", LocalPath: "/data/alpha-two",
			Available: true, TotalBytes: 1_000_000_000_000, FreeBytes: 5_000_000_000,
			WarningFreeBytes: 100_000_000_000, CriticalFreeBytes: 20_000_000_000,
			Level: "CRITICAL",
		},
	)
	router := newTestRouterForStorage(t, backend)

	rec := doGet(t, router, "/api/v1/system/storage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body listStorageStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.BackupSets) != 2 {
		t.Fatalf("len(BackupSets) = %d, want 2", len(body.BackupSets))
	}
	if body.BackupSets[1].Level != "CRITICAL" {
		t.Errorf("BackupSets[1].Level = %q, want %q", body.BackupSets[1].Level, "CRITICAL")
	}
	if body.BackupSets[1].CriticalFreeBytes != 20_000_000_000 {
		t.Errorf("BackupSets[1].CriticalFreeBytes = %d, want %d", body.BackupSets[1].CriticalFreeBytes, 20_000_000_000)
	}
}

func TestSystemStorage_UnavailableBackupSetReportsFalseNotAnError(t *testing.T) {
	backend := newStorageFakeBackend(service.StorageStatus{
		BackupSetID: "alpha/fresh", LocalPath: "/data/not-created-yet",
		Available: false,
	})
	router := newTestRouterForStorage(t, backend)

	rec := doGet(t, router, "/api/v1/system/storage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body listStorageStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.BackupSets[0].Available {
		t.Error("Available = true, want false")
	}
	if body.BackupSets[0].Level != "" {
		t.Errorf("Level = %q, want empty for an unavailable assessment", body.BackupSets[0].Level)
	}
}

func TestSystemStorage_BackendErrorReturns500NotAPartialBody(t *testing.T) {
	backend := newStorageFakeBackend()
	backend.errOnList = errors.New("boom: statfs exploded")
	router := newTestRouterForStorage(t, backend)

	rec := doGet(t, router, "/api/v1/system/storage")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", rec.Code, rec.Body.String())
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Message == "boom: statfs exploded" {
		t.Error("raw backend error text leaked into the response — must be a generic, safe-to-render message")
	}
}

// TestSystemStorage_ResponseNeverCarriesADeleteAffordance is issue #104's
// structural claim made mechanical at the wire level: the JSON contract
// for this endpoint has no field that could name or trigger a deletion
// or a retention-apply call — docs/EPIC-B-multi-nas.md §56's "SHALL not
// provide an auto-delete... option" translated into "cannot even be
// asked for over this route", not just "the UI happens not to render a
// button for it".
func TestSystemStorage_ResponseNeverCarriesADeleteAffordance(t *testing.T) {
	backend := newStorageFakeBackend(service.StorageStatus{
		BackupSetID: "alpha/one", LocalPath: "/data/alpha", Available: true,
		TotalBytes: 100, FreeBytes: 1, WarningFreeBytes: 50, CriticalFreeBytes: 10, Level: "CRITICAL",
	})
	router := newTestRouterForStorage(t, backend)

	rec := doGet(t, router, "/api/v1/system/storage")

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	sets, ok := raw["backup_sets"].([]any)
	if !ok || len(sets) == 0 {
		t.Fatalf("backup_sets missing or empty in response: %s", rec.Body.String())
	}
	entry, ok := sets[0].(map[string]any)
	if !ok {
		t.Fatalf("backup_sets[0] is not an object: %s", rec.Body.String())
	}
	for key := range entry {
		if key == "delete" || key == "apply_retention" || key == "free_space" || key == "cleanup" {
			t.Errorf("response entry carries a %q field — this endpoint must be read-only", key)
		}
	}
}

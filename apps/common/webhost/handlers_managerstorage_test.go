package webhost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// This file covers the manager-wide half of GET /api/v1/system/storage,
// added for issue #286. The per-set list beside it is unchanged and is
// covered in handlers_storage_test.go.

func (f *storageFakeBackend) ManagerStorage(context.Context) (service.ManagerStorage, error) {
	if f.errOnManager != nil {
		return service.ManagerStorage{}, f.errOnManager
	}
	return f.manager, nil
}

// TestSystemStorage_CarriesTheManagerWideReading is the shape a dashboard
// gauge is drawn from, and every field it needs to draw it honestly: the
// denominator it is a fraction of, the path it was measured on, and this
// manager's own consumption separately from the volume's.
func TestSystemStorage_CarriesTheManagerWideReading(t *testing.T) {
	backend := newStorageFakeBackend()
	backend.manager = service.ManagerStorage{
		Known:             true,
		MeasuredPath:      "/data/backups",
		TotalBytes:        2_000_000_000_000,
		FreeBytes:         1_200_000_000_000,
		AvailableBytes:    1_100_000_000_000,
		CatalogBytes:      30_000_000_000,
		CatalogBytesKnown: true,
		OtherBytes:        770_000_000_000,
		OtherBytesKnown:   true,
		CapBytes:          100_000_000_000,
		Denominator:       service.DenominatorCap,
		BindingConstraint: service.DenominatorCap,
		LimitBytes:        100_000_000_000,
		UsedBytes:         30_000_000_000,
		HeadroomBytes:     70_000_000_000,
		WarningFreeBytes:  20_000_000_000,
		CriticalFreeBytes: 10_000_000_000,
		Level:             "OK",
	}
	router := newTestRouterForStorage(t, backend)

	rec := doGet(t, router, "/api/v1/system/storage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body listStorageStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	m := body.Manager
	if !m.Known {
		t.Fatal("manager.known = false")
	}
	if m.Denominator != "cap" {
		t.Errorf("manager.denominator = %q, want %q", m.Denominator, "cap")
	}
	if m.LimitBytes != 100_000_000_000 || m.UsedBytes != 30_000_000_000 {
		t.Errorf("gauge = %d of %d, want 30000000000 of 100000000000", m.UsedBytes, m.LimitBytes)
	}
	if m.MeasuredPath != "/data/backups" {
		t.Errorf("manager.measured_path = %q, want %q", m.MeasuredPath, "/data/backups")
	}
	if m.CatalogBytes != 30_000_000_000 || !m.CatalogBytesKnown {
		t.Errorf("manager.catalog_bytes = %d (known %v), want 30000000000 known", m.CatalogBytes, m.CatalogBytesKnown)
	}
	if m.OtherBytes != 770_000_000_000 {
		t.Errorf("manager.other_bytes = %d, want 770000000000: the gap between what we account for and what the volume holds is reported, not folded in", m.OtherBytes)
	}
}

// TestSystemStorage_AnUnknownManagerReadingCarriesNoNumbers is the honest
// zero on the wire. This is the exact state the live UGREEN install was in
// when it rendered "0 B of 0 B used · NaN%", and the body has to make it
// impossible for a client to mistake for a measurement.
func TestSystemStorage_AnUnknownManagerReadingCarriesNoNumbers(t *testing.T) {
	backend := newStorageFakeBackend()
	backend.manager = service.ManagerStorage{
		Known:         false,
		UnknownReason: service.StorageUnknownNoBackupRoot,
		Denominator:   service.DenominatorDisk,
	}
	router := newTestRouterForStorage(t, backend)

	rec := doGet(t, router, "/api/v1/system/storage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body listStorageStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	m := body.Manager
	if m.Known {
		t.Fatal("manager.known = true")
	}
	if m.UnknownReason != "no_backup_root" {
		t.Errorf("manager.unknown_reason = %q, want %q", m.UnknownReason, "no_backup_root")
	}
	if m.TotalBytes != 0 || m.LimitBytes != 0 || m.UsedBytes != 0 {
		t.Errorf("an unknown reading carries numbers (%d total, %d of %d used)", m.TotalBytes, m.UsedBytes, m.LimitBytes)
	}
	if m.Level != "" {
		t.Errorf("manager.level = %q on an unknown reading, want empty: an unread disk is not OK", m.Level)
	}
}

// TestSystemStorage_ManagerReadingFailureIs500NotAPartialBody keeps the
// route's existing all-or-nothing posture now that it makes two backend
// calls: a client must never get a 200 carrying half a body.
func TestSystemStorage_ManagerReadingFailureIs500NotAPartialBody(t *testing.T) {
	backend := newStorageFakeBackend()
	backend.errOnManager = errors.New("boom: statfs exploded")
	router := newTestRouterForStorage(t, backend)

	rec := doGet(t, router, "/api/v1/system/storage")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !json.Valid([]byte(body)) {
		t.Errorf("error body is not JSON: %s", body)
	}
}

// TestSystemStorage_TheManagerReadingNeverLeaksAnInternalError holds the
// new call to the same rule every other handler in this package follows:
// an unclassified failure gets this package's own sentence, never the
// underlying error's text.
func TestSystemStorage_TheManagerReadingNeverLeaksAnInternalError(t *testing.T) {
	backend := newStorageFakeBackend()
	backend.errOnManager = errors.New("statfs /volume1/backups: permission denied")
	router := newTestRouterForStorage(t, backend)

	rec := doGet(t, router, "/api/v1/system/storage")
	if got := rec.Body.String(); strings.Contains(got, "/volume1/backups") || strings.Contains(got, "permission denied") {
		t.Errorf("the response leaks the underlying error: %s", got)
	}
}

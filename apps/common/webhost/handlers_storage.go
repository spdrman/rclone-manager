// This file is issue #104 (B3.4)'s storage-pressure surfacing half:
// docs/EPIC-B-multi-nas.md §56 (Storage UX). It exposes
// core/service.BackupService.ListStorageStatus — itself a read of
// internal/capacity's (FR-21) existing Assess/AssessCurrent machinery,
// the same standing pipeline.go's admitCapacity already consults before
// every transfer — as one read-only route.
//
// There is deliberately no route, here or anywhere else in this
// package, that lets a "critical" result trigger anything: no deletion,
// no automatic call into internal/retention's apply path. That is not
// merely this handler's own restraint — apps/common/webhost is a
// SEPARATE Go module from core/ and cannot import core/internal/retention
// at all (Go's own "internal" import rule; see doc.go), so wiring
// "critical storage" into an automatic retention run is structurally
// unreachable from this package, not just avoided by convention.
package webhost

import (
	"net/http"

	"github.com/spdrman/rclone-manager/core/service"
)

// storageStatusResponse is the wire shape of one backup set's capacity
// assessment, a field-for-field mirror of core/service.StorageStatus
// (see that type's own doc for what each field means and why it carries
// no deletion affordance).
type storageStatusResponse struct {
	BackupSetID string `json:"backup_set_id"`
	LocalPath   string `json:"local_path"`
	Available   bool   `json:"available"`

	// UnavailableReason is "not_created", "unreadable" or "misconfigured"
	// when available is false, and "" when it is true. A client that shows
	// "not initialised yet" for every unavailable reading would be telling
	// an operator whose mount has vanished exactly the wrong thing, which
	// is what this field exists to prevent.
	UnavailableReason string `json:"unavailable_reason"`

	TotalBytes uint64 `json:"total_bytes"`

	// FreeBytes is observability only, for display beside total_bytes.
	// available_bytes, not this, is what level and FR-21's transfer
	// refusal are computed from; the two differ by whatever the filesystem
	// reserves for a privileged process (5% by default on ext4).
	FreeBytes uint64 `json:"free_bytes"`

	// AvailableBytes is the free space this process can actually use
	// (df's Avail), which is the number level below was decided from.
	AvailableBytes uint64 `json:"available_bytes"`

	// WarningFreeBytes and CriticalFreeBytes are both always 0 today:
	// internal/config carries no capacity thresholds yet, so nothing
	// populates them in a running process and level can only read "OK"
	// short of a genuinely full disk. See core/service.StorageStatus's own
	// doc for the full statement of that, and why FR-21's refusal is
	// nonetheless intact.
	WarningFreeBytes  uint64 `json:"warning_free_bytes"`
	CriticalFreeBytes uint64 `json:"critical_free_bytes"`

	// Level is "OK", "WARNING" or "CRITICAL" (internal/capacity.Level's
	// own String()), or "" when Available is false.
	Level string `json:"level"`
}

// listStorageStatusResponse is GET /api/v1/system/storage's body: an
// object with one array field, matching listBackupSetsResponse's own
// shape (handlers_backupsets.go) so a future field can be added without
// breaking a client parsing a bare top-level array.
type listStorageStatusResponse struct {
	BackupSets []storageStatusResponse `json:"backup_sets"`
}

func toStorageStatusResponse(s service.StorageStatus) storageStatusResponse {
	return storageStatusResponse{
		BackupSetID:       s.BackupSetID,
		LocalPath:         s.LocalPath,
		Available:         s.Available,
		UnavailableReason: string(s.UnavailableReason),
		TotalBytes:        s.TotalBytes,
		FreeBytes:         s.FreeBytes,
		AvailableBytes:    s.AvailableBytes,
		WarningFreeBytes:  s.WarningFreeBytes,
		CriticalFreeBytes: s.CriticalFreeBytes,
		Level:             s.Level,
	}
}

// systemStorage is GET /api/v1/system/storage: docs/EPIC-B-multi-nas.md
// §56's exact display list (backup root, total/free capacity, the
// configured warning/critical thresholds), one entry per configured
// backup set.
//
// Read the threshold caveat on storageStatusResponse before building a
// screen on this: the two threshold numbers are structurally zero until
// internal/config grows capacity fields, so today this route reports a
// level of "OK" for anything short of a completely full disk. The refusal
// FR-21 promises still happens; what does not exist yet is a warning
// level to display ahead of it. Read-only, like GET /system/version and
// GET /system/capabilities alongside it — no CSRF, no destructive gate,
// both structurally verified for every route in this package by
// TestNoAPIRouteBypassesAuthentication and
// TestNoMutatingAPIRouteBypassesTheDestructiveGate (router_test.go).
func (h *handlers) systemStorage(w http.ResponseWriter, r *http.Request) {
	statuses, err := h.backend.ListStorageStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not assess storage capacity")
		return
	}

	resp := make([]storageStatusResponse, len(statuses))
	for i, s := range statuses {
		resp[i] = toStorageStatusResponse(s)
	}
	writeJSON(w, http.StatusOK, listStorageStatusResponse{BackupSets: resp})
}

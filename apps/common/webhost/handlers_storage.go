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

	// WarningFreeBytes and CriticalFreeBytes are the configured
	// thresholds, from internal/config's capacity block (issue #286).
	// Before that block existed they were structurally zero in every
	// deployment and level could only read "OK" short of a genuinely full
	// disk. Both still default to zero, which means "no line here" rather
	// than a line at zero bytes.
	//
	// They are weighed against the BINDING headroom, which is the
	// filesystem's available space or the manager-wide cap's remaining
	// allowance, whichever is smaller. A set whose volume has a terabyte
	// free can therefore read CRITICAL because the cap is nearly spent.
	// See core/service.StorageStatus's own doc.
	WarningFreeBytes  uint64 `json:"warning_free_bytes"`
	CriticalFreeBytes uint64 `json:"critical_free_bytes"`

	// Level is "OK", "WARNING" or "CRITICAL" (internal/capacity.Level's
	// own String()), or "" when Available is false.
	Level string `json:"level"`
}

// managerStorageResponse is the manager-wide storage reading (issue #286),
// a field-for-field mirror of core/service.ManagerStorage.
//
// It sits BESIDE the per-set list rather than being derived from it,
// because summing that list cannot answer this question. A fresh instance
// has no backup sets, so the sum is zero, and a client turning zeros into
// a fraction produced the "0 B of 0 B used · NaN%" this issue opened on.
// Two sets on one volume are two entries reporting the same disk, so the
// sum is twice a number that exists once. And the storage cap is one
// ceiling for the whole manager, with no per-set entry to hang it on.
//
// Read core/service.ManagerStorage's own doc before building a screen on
// this. The short version a client must honour: when known is false, every
// byte count below is zero and level is empty, and the only correct
// rendering is "capacity is not known yet".
type managerStorageResponse struct {
	Known bool `json:"known"`

	// UnknownReason is "no_backup_root", "not_created", "unreadable" or
	// "misconfigured" when known is false, and "" when it is true. A
	// client that showed "not set up yet" for every unknown reading would
	// be telling an operator whose mount has vanished exactly the wrong
	// thing.
	UnknownReason string `json:"unknown_reason"`

	// MeasuredPath is the directory whose filesystem was statted, present
	// whether or not the reading succeeded. The engine runs in a
	// container: a reading taken from the container's own root filesystem
	// instead of the bind-mounted backup volume is a confident wrong
	// number, and naming the path is what makes it noticeable.
	MeasuredPath string `json:"measured_path"`

	TotalBytes     uint64 `json:"total_bytes"`
	FreeBytes      uint64 `json:"free_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`

	// CatalogBytes is this manager's own consumption, summed from the
	// state database rather than walked off the disk, so it counts only
	// what this manager put there.
	CatalogBytes      uint64 `json:"catalog_bytes"`
	CatalogBytesKnown bool   `json:"catalog_bytes_known"`

	// OtherBytes is how much of the volume's used space this manager does
	// not account for. Reported rather than folded into catalog_bytes: a
	// large gap means something else writes into the backup root.
	OtherBytes      uint64 `json:"other_bytes"`
	OtherBytesKnown bool   `json:"other_bytes_known"`

	// CapBytes is the configured ceiling, and 0 means no cap.
	CapBytes uint64 `json:"cap_bytes"`

	// Denominator ("disk" or "cap"), LimitBytes and UsedBytes are the
	// gauge, already resolved. A caption that does not name the
	// denominator is the defect this field exists to prevent: 80% of a
	// 2 TB volume and 80% of a 100 GB allowance draw the same bar.
	Denominator string `json:"denominator"`
	LimitBytes  uint64 `json:"limit_bytes"`
	UsedBytes   uint64 `json:"used_bytes"`

	// HeadroomBytes is what is left before the next transfer is refused,
	// and BindingConstraint says which of the two produced it. They differ
	// from Denominator on a capped deployment whose volume is nearly full.
	HeadroomBytes     uint64 `json:"headroom_bytes"`
	BindingConstraint string `json:"binding_constraint"`

	WarningFreeBytes  uint64 `json:"warning_free_bytes"`
	CriticalFreeBytes uint64 `json:"critical_free_bytes"`

	// Level is "OK", "WARNING" or "CRITICAL", or "" when known is false.
	Level string `json:"level"`
}

// listStorageStatusResponse is GET /api/v1/system/storage's body: the
// manager-wide reading, plus one entry per configured backup set. The
// object-with-fields shape (rather than a bare top-level array) is what
// let the manager reading be added here at all, and is the same shape
// listBackupSetsResponse uses for the same reason.
type listStorageStatusResponse struct {
	Manager    managerStorageResponse  `json:"manager"`
	BackupSets []storageStatusResponse `json:"backup_sets"`
}

func toManagerStorageResponse(m service.ManagerStorage) managerStorageResponse {
	return managerStorageResponse{
		Known:             m.Known,
		UnknownReason:     string(m.UnknownReason),
		MeasuredPath:      m.MeasuredPath,
		TotalBytes:        m.TotalBytes,
		FreeBytes:         m.FreeBytes,
		AvailableBytes:    m.AvailableBytes,
		CatalogBytes:      m.CatalogBytes,
		CatalogBytesKnown: m.CatalogBytesKnown,
		OtherBytes:        m.OtherBytes,
		OtherBytesKnown:   m.OtherBytesKnown,
		CapBytes:          m.CapBytes,
		Denominator:       string(m.Denominator),
		LimitBytes:        m.LimitBytes,
		UsedBytes:         m.UsedBytes,
		HeadroomBytes:     m.HeadroomBytes,
		BindingConstraint: string(m.BindingConstraint),
		WarningFreeBytes:  m.WarningFreeBytes,
		CriticalFreeBytes: m.CriticalFreeBytes,
		Level:             m.Level,
	}
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
// Since issue #286 it also carries the manager-wide reading a dashboard
// gauge is actually drawn from (managerStorageResponse), and the two
// threshold numbers are real: internal/config carries a capacity block
// now, so an operator who sets a warning level gets one ahead of FR-21's
// hard refusal. Both default to zero, which means "no line here" rather
// than a line at zero bytes. Read-only, like GET /system/version and
// GET /system/capabilities alongside it — no CSRF, no destructive gate,
// both structurally verified for every route in this package by
// TestNoAPIRouteBypassesAuthentication (router_test.go) and the gate walk
// in gate_redteam_test.go.
func (h *handlers) systemStorage(w http.ResponseWriter, r *http.Request) {
	manager, err := h.backend.ManagerStorage(r.Context())
	if err != nil {
		// All or nothing, deliberately, now that this route makes two
		// backend calls: a 200 carrying half a body would leave a client
		// deciding for itself what the missing half meant, which on a
		// capacity reading is exactly the guess this issue exists to stop.
		// The message is this package's own sentence rather than err's,
		// for the reason every other handler here gives: an unclassified
		// error can carry a mount path or an errno.
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not assess storage capacity")
		return
	}

	statuses, err := h.backend.ListStorageStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not assess storage capacity")
		return
	}

	resp := make([]storageStatusResponse, len(statuses))
	for i, s := range statuses {
		resp[i] = toStorageStatusResponse(s)
	}
	writeJSON(w, http.StatusOK, listStorageStatusResponse{
		Manager:    toManagerStorageResponse(manager),
		BackupSets: resp,
	})
}

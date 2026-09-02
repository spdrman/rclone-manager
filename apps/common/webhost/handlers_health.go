// This file is issue #211's authenticated health surface: FR-24's
// backup-freshness verdict for every configured backup set, the same
// computation `backup-manager status` prints.
//
// It is a different thing from /health/live and /health/ready
// (handlers_system.go), and the difference is the point. Those two are
// unauthenticated infrastructure probes that answer "should traffic come
// here", they sit outside /api/v1 and outside this contract, and they say
// nothing at all about whether backups are landing. Failure-safety
// invariant 14 is exactly that: process liveness is not evidence of
// backup freshness. A UI that showed a ready probe as "healthy" would
// keep saying so long after the last backup succeeded.
package webhost

import (
	"net/http"

	"github.com/spdrman/rclone-manager/core/service"
)

// backupSetHealthResponse is one backup set's verdict. It carries no
// process or build fact, deliberately: core/internal/health enforces that
// separation by giving its two halves no shared field at all, and GET
// /api/v1/system/version already reports the process half.
type backupSetHealthResponse struct {
	BackupSetID string `json:"backup_set_id"`
	SourceName  string `json:"source_name"`
	SetName     string `json:"set_name"`

	// State is "HEALTHY", "DEGRADED", "STALE" or "FAILING". HEALTHY
	// requires positive evidence; a set that has never produced anything
	// is DEGRADED, never healthy.
	State string `json:"state"`
	// Reason is one operator-facing sentence. Never empty.
	Reason string `json:"reason"`

	// HaltReason is why the manager could not connect to this backup set
	// the last time it tried, so nothing was backed up (issue #245).
	//
	// Omitted, never empty-stringed, when no refusal is on record. That
	// polarity is the whole reason the field is optional: absent means "no
	// refusal has been observed", which is a different claim from "this
	// set is reachable", and only the first one is ever available to make.
	// A required boolean here would force every producer to make the
	// second claim, which is exactly what issue #231 removed from the
	// client after it filled a `halted` field with a literal false on
	// every set.
	//
	// It sits beside State, not inside it: a set refused on every cycle
	// still gets its verdict from journal evidence alone, usually STALE,
	// which is true and half the story.
	HaltReason string `json:"halt_reason,omitempty"`

	NewestGoodBackupAt    string `json:"newest_good_backup_at,omitempty"`
	LastCompletedBackupAt string `json:"last_completed_backup_at,omitempty"`
	StaleAfterSeconds     int64  `json:"stale_after_seconds"`

	CurrentTransfers     int `json:"current_transfers"`
	PendingDeletes       int `json:"pending_deletes"`
	Failures             int `json:"failures"`
	QuarantinedCount     int `json:"quarantined_count"`
	QuarantinedLostCount int `json:"quarantined_lost_count"`

	// ReinstatedRemoteRetainedCount is how many backups here were
	// re-trusted after quarantine and still hold a remote source this
	// manager has undertaken never to delete (issue #227). It is NOT
	// omitempty: a zero is a real reading here, and an absent field would
	// read as "this build does not know", which is the state this issue
	// exists to end.
	//
	// There is no bytes figure beside it on purpose. This manager measured
	// those remote objects once, at discovery, and FR-8 treats remote
	// metadata as untrusted precisely because it may have changed since;
	// the reason the delete gate refuses at all is that the remote's
	// identity usually cannot be re-established with confidence. So the
	// count is served and the size is not, rather than a plausible sum
	// nobody has looked at.
	ReinstatedRemoteRetainedCount int `json:"reinstated_remote_retained_count"`

	// ReadOnlyRetainedCount is how many backups here currently hold a
	// remote source this manager will never delete because the SET
	// itself is declared read-only (issue #282), not because any one of
	// them was individually reinstated (see ReinstatedRemoteRetainedCount
	// above for that separate population). Also NOT omitempty, for the
	// identical reason: zero is a real, common reading.
	ReadOnlyRetainedCount int `json:"read_only_retained_count"`

	// FreeBytes is omitted when the reading could not be taken, and
	// FreeBytesKnown says which case a zero is. A bare 0 would read as
	// "the disk is full", which is the one thing an unavailable reading
	// must never be mistaken for.
	FreeBytes      uint64 `json:"free_bytes,omitempty"`
	FreeBytesKnown bool   `json:"free_bytes_known"`

	// TotalBytes and StorageLevel are the FR-21 capacity assessment GET
	// /api/v1/system/storage reports in full, carried here so one call
	// answers "are my backups healthy" without a caller having to join two
	// endpoints to find out whether the disk they land on is nearly full.
	// StorageLevel is omitted, not "OK", when no reading could be taken.
	TotalBytes   uint64 `json:"total_bytes,omitempty"`
	StorageLevel string `json:"storage_level,omitempty"`
}

type healthResponse struct {
	GeneratedAt string                    `json:"generated_at"`
	BackupSets  []backupSetHealthResponse `json:"backup_sets"`
}

// systemHealth is GET /api/v1/system/health. Read-only (§50), so no CSRF
// and no destructive gate.
//
// Nothing is cached. A cached health verdict is precisely the thing that
// keeps reporting green after a deployment has stopped backing anything
// up, so the capacity reading and the freshness comparison are both taken
// fresh on every call.
func (h *handlers) systemHealth(w http.ResponseWriter, r *http.Request) {
	report, err := h.backend.Health(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not compute backup health")
		return
	}

	resp := healthResponse{
		GeneratedAt: formatTime(report.GeneratedAt),
		BackupSets:  make([]backupSetHealthResponse, 0, len(report.BackupSets)),
	}
	for _, bs := range report.BackupSets {
		resp.BackupSets = append(resp.BackupSets, toBackupSetHealthResponse(bs))
	}
	writeJSON(w, http.StatusOK, resp)
}

func toBackupSetHealthResponse(bs service.BackupSetHealth) backupSetHealthResponse {
	return backupSetHealthResponse{
		BackupSetID:           bs.BackupSetID,
		SourceName:            bs.SourceName,
		SetName:               bs.SetName,
		State:                 bs.State,
		Reason:                bs.Reason,
		HaltReason:            bs.HaltReason,
		NewestGoodBackupAt:    formatTime(bs.NewestGoodBackupAt),
		LastCompletedBackupAt: formatTime(bs.LastCompletedBackupAt),
		StaleAfterSeconds:     int64(bs.StaleAfter.Seconds()),
		CurrentTransfers:      bs.CurrentTransfers,
		PendingDeletes:        bs.PendingDeletes,
		Failures:              bs.Failures,
		QuarantinedCount:      bs.QuarantinedCount,
		QuarantinedLostCount:  bs.QuarantinedLostCount,

		ReinstatedRemoteRetainedCount: bs.ReinstatedRemoteRetainedCount,
		ReadOnlyRetainedCount:         bs.ReadOnlyRetainedCount,
		FreeBytes:                     bs.FreeBytes,
		FreeBytesKnown:                bs.FreeBytesKnown,
		TotalBytes:                    bs.TotalBytes,
		StorageLevel:                  bs.StorageLevel,
	}
}

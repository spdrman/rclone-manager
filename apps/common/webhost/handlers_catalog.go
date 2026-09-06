// This file is issue #211's catalog-recovery surface: the API expression
// of `backup-manager catalog rebuild` and its --dry-run.
//
// The two routes return the same shape because they are the same code
// path with one flag different (see core/service.ScanCatalog). A preview
// computed by a second implementation would be a preview of something
// else, which is the whole reason the CLI shares one implementation too.
package webhost

import (
	"net/http"

	"github.com/spdrman/rclone-manager/core/service"
)

// catalogFailureResponse is one recovery manifest a pass could not use.
type catalogFailureResponse struct {
	BackupSetID string `json:"backup_set_id"`
	// Path is a server-side path, reported because it is the only thing
	// that identifies which manifest needs attention, and an operator with
	// server access is this failure's only audience. It is omitted when
	// the failure was not about one specific manifest.
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"`
}

type catalogReportResponse struct {
	DryRun         bool                     `json:"dry_run"`
	Scanned        int                      `json:"scanned"`
	Reconstructed  int                      `json:"reconstructed"`
	AlreadyPresent int                      `json:"already_present"`
	Failures       []catalogFailureResponse `json:"failures"`
}

// scanCatalog is POST /api/v1/catalog/scan: report what a rebuild would
// reconstruct, writing nothing.
//
// POST rather than GET even though it writes nothing, because it is not
// free and not cacheable: it walks every recovery manifest on disk for
// every configured backup set. It carries requireCSRF for the same reason
// host-key-probe does (real work against real files on a state-changing
// verb) and not requireDestructiveGate, because it cannot change anything
// at all.
func (h *handlers) scanCatalog(w http.ResponseWriter, r *http.Request) {
	report, err := h.backend.ScanCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to scan the backup catalog")
		return
	}
	writeJSON(w, http.StatusOK, toCatalogReportResponse(report))
}

// rebuildCatalog is POST /api/v1/catalog/rebuild: reconstruct missing
// records from the recovery manifests already on disk.
//
// It carries requireCSRF and NOT requireDestructiveGate. That is not an
// oversight about a route with "rebuild" in its name: rebuild only ever
// ADDS journal rows for artifacts whose manifests are on disk and whose
// rows are missing. It never removes or overwrites a row that already
// exists (core/internal/app's CatalogRebuildAlreadyPresent verdict, which
// core/service asserts against its own dry run), it never contacts a
// remote, and it deletes nothing anywhere. Running it against a healthy
// journal is a no-op rather than a reset.
func (h *handlers) rebuildCatalog(w http.ResponseWriter, r *http.Request) {
	report, err := h.backend.RebuildCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to rebuild the backup catalog")
		return
	}
	writeJSON(w, http.StatusOK, toCatalogReportResponse(report))
}

func toCatalogReportResponse(report service.CatalogReport) catalogReportResponse {
	resp := catalogReportResponse{
		DryRun:         report.DryRun,
		Scanned:        report.Scanned,
		Reconstructed:  report.Reconstructed,
		AlreadyPresent: report.AlreadyPresent,
		Failures:       make([]catalogFailureResponse, 0, len(report.Failures)),
	}
	for _, f := range report.Failures {
		resp.Failures = append(resp.Failures, catalogFailureResponse{
			BackupSetID: f.BackupSetID,
			Path:        f.Path,
			Reason:      f.Reason,
		})
	}
	return resp
}

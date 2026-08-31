// This file is issue #162's read-only half of the registered-validator
// catalog (docs/EPIC-B-multi-nas.md §71 Work Package 3.2, §26 Step 5).
//
// The catalog itself lives in core/service (validator.go), which is also
// the only thing that can turn one of its ids back into a runnable
// command. This route exists so the add-backup-set wizard's step 5 can
// render a real picklist instead of the decorative toggle #98 shipped: it
// hands out the ids a create request may send, plus one line of prose per
// id to label them with, and nothing else. In particular it never
// discloses where the scripts are materialized, which is a server-side
// detail an API client has no use for and every reason not to be handed
// (handlers_validators_test.go asserts that directly).
//
// There is deliberately no write counterpart. §26 Step 5's whole point is
// that the API/UI layer selects a validator by id and can never name an
// executable; a route that let a client add a catalog entry would be that
// same arbitrary-command surface wearing a different name.
package webhost

import (
	"net/http"

	"github.com/spdrman/rclone-manager/core/service"
)

// validatorResponse is one catalog entry on the wire: the id
// backupSetRequest.ValidatorID accepts, and a summary to render beside
// it. A field-for-field mirror of core/service.Validator, which carries
// nothing else for the same reason this does not.
type validatorResponse struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// listValidatorsResponse is GET /api/v1/validators' body: an object with
// one array field, matching listBackupSetsResponse
// (handlers_backupsets.go) and listStorageStatusResponse
// (handlers_storage.go), so a future field can be added without breaking
// a client parsing a bare top-level array.
type listValidatorsResponse struct {
	Validators []validatorResponse `json:"validators"`
}

// listValidators is GET /api/v1/validators: read-only
// (docs/EPIC-B-multi-nas.md §50's "view configuration" bucket), so no
// CSRF and no destructive gate, exactly like GET /system/capabilities and
// GET /backup-sets alongside it. Authentication is not optional and is
// not this handler's business either: router.go applies it to the whole
// /api/v1 group, and TestNoAPIRouteBypassesAuthentication walks every
// route to prove it.
//
// It takes no backend call at all. The catalog is code-defined and fixed
// at build time (core/service's own catalogScripts map), so there is
// nothing per-deployment to ask a BackupServiceClient for, and adding a
// method to that interface purely to forward a constant would make the
// seam wider for nothing.
func (h *handlers) listValidators(w http.ResponseWriter, _ *http.Request) {
	catalog := service.RegisteredValidators()
	resp := listValidatorsResponse{Validators: make([]validatorResponse, 0, len(catalog))}
	for _, v := range catalog {
		resp.Validators = append(resp.Validators, validatorResponse{
			ID:      string(v.ID),
			Summary: v.Summary,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

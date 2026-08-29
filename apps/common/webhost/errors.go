package webhost

import (
	"encoding/json"
	"net/http"
)

// errorResponse is the one error shape every handler in this package
// returns, so a client only ever has to parse one thing regardless of
// which endpoint or which failure it hit.
type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError writes a JSON errorResponse with the given HTTP status, code
// and message. code is a stable, machine-readable token (e.g.
// "CONFIG_REVISION_STALE"); message is human-readable and MAY change
// without notice.
func writeError(w http.ResponseWriter, status int, code, message string) {
	var resp errorResponse
	resp.Error.Code = code
	resp.Error.Message = message
	writeJSON(w, status, resp)
}

// configRevisionStaleResponse extends errorResponse with the current
// config_revision as its own structured, top-level field (issue #118 item
// 5): before this, the only place the current revision appeared anywhere
// in a response was embedded in this very error's message string, a field
// this package's own docs elsewhere describe as free to change without
// notice. A client handling CONFIG_REVISION_STALE needs a value it can
// actually rely on to retry against, not one it has to parse out of prose.
type configRevisionStaleResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	ConfigRevision string `json:"config_revision"`
}

// writeConfigRevisionStale writes a 409 CONFIG_REVISION_STALE body carrying
// current as a structured field, in addition to the usual error.code/
// error.message shape every other error in this package uses. See
// configRevisionStaleResponse's own doc for why this one error gets a
// dedicated writer instead of reusing writeError.
func writeConfigRevisionStale(w http.ResponseWriter, message, current string) {
	var resp configRevisionStaleResponse
	resp.Error.Code = "CONFIG_REVISION_STALE"
	resp.Error.Message = message
	resp.ConfigRevision = current
	writeJSON(w, http.StatusConflict, resp)
}

// writeJSON encodes v as the response body with the given status code and
// a JSON content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

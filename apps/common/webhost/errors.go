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

// writeJSON encodes v as the response body with the given status code and
// a JSON content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

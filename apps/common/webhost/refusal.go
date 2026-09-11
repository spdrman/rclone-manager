package webhost

import (
	"context"
	"log/slog"
	"net/http"
)

// Issue #598. The one place a 500 is written, and the reason there is a
// place rather than thirty call sites.
//
// Every INTERNAL refusal in this package used to be a bare
// `writeError(w, http.StatusInternalServerError, "INTERNAL", "...")` with
// the error it was refusing over bound, tested and dropped on the floor.
// There was nowhere to write it even if a handler had wanted to: the
// handlers struct held no logger and NewRouter installed no logging
// middleware, so a deployment refusing every read of its own activity feed
// produced a correlation id, a red banner, and not one line anywhere.
//
// That is not a logging-level problem, it is a structural one, and it made
// the frontend's own words untrue. api/failure.ts answers an INTERNAL code
// with "Backupd reported an internal error rather than a reason it
// could name. Its own log holds the detail, under this correlation id."
// The log held nothing, under any id, for any of these routes.
//
// # What is logged, and what is not
//
// The response and the log line are deliberately different. The client
// gets a code, a sentence written for an operator and the id; it never
// gets err, because an unclassified error here can carry a filesystem
// path, an rclone internal or a remote address, and several of the call
// sites below say so at their own default arm. The log gets err in full,
// under the same id, on the host where those strings were already visible
// to whoever can read the process's output.
//
// # Why the id is minted first
//
// writeError mints it and hands it back, so the line and the response
// carry one id rather than two. An id that matches nothing is the exact
// defect this package's frontend counterpart (#274) was written about, one
// hop over.

// internalError writes a 500 refusal and records what it refused over.
//
// The route is r.URL.Path rather than chi's route PATTERN on purpose: a
// pattern tells you which handler ran, and the path tells you which
// request an operator made, which is the one they can repeat.
func (h *handlers) internalError(w http.ResponseWriter, r *http.Request, code, message string, err error) {
	id := writeError(w, http.StatusInternalServerError, code, message)
	h.logRefusal(r, http.StatusInternalServerError, code, id, err)
}

// logRefusal is internalError's second half, split out so a refusal that
// has already written its own body (there is one: the first-run failure
// that appends the service's own sentence to the message) can still be
// recorded without writing a second response.
func (h *handlers) logRefusal(r *http.Request, status int, code, correlationID string, err error) {
	if h == nil || h.logger == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("route", routeOf(r)),
		slog.String("method", methodOf(r)),
		slog.Int("status", status),
		slog.String("code", code),
		slog.String("correlation_id", correlationID),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	h.logger.Event(contextOf(r), slog.LevelError, "http_refusal",
		"the web host refused a request and could not name a reason for the client", attrs...)
}

// The three readers below exist so a nil *http.Request cannot turn a
// diagnostic into a panic. A request is never nil in production, and a
// logger that took the process down when one was would be a worse failure
// than the one it is reporting.

func routeOf(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}

func methodOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.Method
}

func contextOf(r *http.Request) context.Context {
	if r == nil {
		return context.Background()
	}
	return r.Context()
}

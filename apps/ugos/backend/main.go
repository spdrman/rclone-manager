// Command backup-manager-upk-proof is the bare backend half of the B1.2 minimal UPK
// proof (issue #91). It does exactly two things: answer GET /health/live, and serve a
// static frontend bundle. It deliberately does not import apps/common/webhost or
// core/service — this proof only needs to show a packaged Docker App can answer a bare
// liveness probe on real UGOS hardware, not stand up the real API (that's #83) or touch
// core backup-engine logic, which this package has no dependency on at all.
package main

import (
	"log"
	"net/http"
	"os"
)

// newMux builds the handler this binary serves. webRoot is the directory the static
// frontend bundle is read from; it does not need to exist for /health/live to work (see
// TestHealthLive_SucceedsEvenWithoutAFrontendBundle) — a liveness probe that only passes
// because unrelated state happens to be present isn't proving what it claims to prove.
func newMux(webRoot string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthLive)
	mux.Handle("/", http.FileServer(http.Dir(webRoot)))
	return mux
}

// healthLive is a bare liveness probe: no authentication, no dependency on the frontend
// bundle or anything else this process serves. Same shape as apps/common/webhost's
// healthLive (`{"status":"ok"}`) on purpose, so the two don't drift if this proof's
// backend is ever folded into the real one later.
func healthLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	webRoot := os.Getenv("WEB_ROOT")
	if webRoot == "" {
		webRoot = "/app/www"
	}

	log.Printf("backup-manager-upk-proof listening on %s, serving frontend from %s", addr, webRoot)
	if err := http.ListenAndServe(addr, newMux(webRoot)); err != nil {
		log.Fatal(err)
	}
}

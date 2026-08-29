// Command backup-manager-web is the generic Web host's own executable
// (issue #82/B4.1, docs/EPIC-B-multi-nas.md §9.2): it runs alongside
// cmd/backup-manager (core/cmd/backup-manager, unchanged by this issue)
// inside the same canonical OCI image, and adds exactly one thing that
// binary does not have: a `serve` command combining the versioned
// /api/v1 API, local authentication, the shared UI, and the backup
// scheduler in one process (§9.3).
//
// Every other execution mode (`run`, `daemon`, `check`, `status`, ...)
// stays on cmd/backup-manager: this binary is deliberately narrow rather
// than a second, competing CLI, since core/cmd/backup-manager cannot be
// imported from here (it is an unexported `package main`, and even if it
// were, importing anything from apps/ back into a core/ package would be
// the exact dependency-direction violation §7.1 forbids - this binary
// only ever imports FROM core/, never the reverse) and duplicating its
// command surface would be two implementations of the same thing to keep
// in sync.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/generic/server"
	"github.com/spdrman/rclone-manager/apps/generic/webui"
	"github.com/spdrman/rclone-manager/core/service"
)

// Set at build time with -ldflags (see container/Dockerfile), exactly
// like core/cmd/backup-manager's own version/commit vars.
var (
	version = "dev"
	commit  = "none"
)

// defaultConfigPath matches core/cmd/backup-manager's own default and
// container/compose.yaml's mount point.
const defaultConfigPath = "/etc/backup-manager/config.yaml"

// defaultAuthStorePath lives inside the SAME already-writable state
// volume container/compose.yaml already mounts for the SQLite journal
// (STATE_DIR -> /data/state), so serving local authentication needs no
// additional volume of its own.
const defaultAuthStorePath = "/data/state/local-auth.json"

// defaultListenAddr is used only when neither --listen nor LISTEN_ADDR
// is set; container/compose.yaml always sets LISTEN_ADDR explicitly.
const defaultListenAddr = ":8080"

// shutdownGrace bounds how long serve waits for the HTTP server's
// graceful Shutdown and the scheduler loop's own exit before giving up
// on a clean stop and returning anyway - the process is exiting either
// way once ctx is canceled (SIGTERM/SIGINT); this only decides how long
// it waits first.
const shutdownGrace = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "backup-manager-web: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: backup-manager-web serve [flags]

serve runs the generic Web host: the shared UI, the versioned /api/v1
API, local authentication, and the backup scheduler, all in one process
sharing one shutdown context (docs/EPIC-B-multi-nas.md §9.2/§9.3).

flags:
  --config PATH       path to the manager's YAML config file
                       (default /etc/backup-manager/config.yaml)
  --listen ADDR        address to listen on
                       (default $LISTEN_ADDR, or :8080)
  --auth-store PATH    path to the local-auth administrator record
                       (default /data/state/local-auth.json)
  --auth-mode MODE     authentication mode; only "local" is implemented
                       today (default local)
`)
}

func cmdServe(args []string) int {
	fset := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "path to the manager's YAML config file")
	listenAddr := fset.String("listen", envOrDefault("LISTEN_ADDR", defaultListenAddr), "address to listen on")
	authStorePath := fset.String("auth-store", defaultAuthStorePath, "path to the local-auth administrator record")
	authMode := fset.String("auth-mode", "local", "authentication mode (only \"local\" is implemented)")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	if *authMode != "local" {
		fmt.Fprintf(os.Stderr, "backup-manager-web: unsupported --auth-mode %q (only \"local\" is implemented; a native-platform mode is a later work package)\n", *authMode)
		return 2
	}

	// The one signal handler in this binary, matching
	// core/cmd/backup-manager/daemon.go's own convention exactly: this is
	// what makes ctx the "process shutdown context" §9.3 requires the HTTP
	// server and the background scheduler to share (see below).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backend, cleanup, err := service.Open(ctx, *configPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	authSvc, err := local.New(local.Config{StorePath: *authStorePath})
	if err != nil {
		return fail(fmt.Errorf("open local-auth store: %w", err))
	}
	if err := authSvc.PrintBootstrapNotice(os.Stdout, displayBaseURL(*listenAddr)); err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web: printing bootstrap notice:", err)
	}

	staticFS, err := fs.Sub(webui.Assets, "dist")
	if err != nil {
		// webui.Assets is a compile-time go:embed of this module's own
		// webui/dist directory: this can only fail if that package was
		// edited to embed something else without updating this constant,
		// a programmer error to notice loudly, not a runtime condition.
		panic(fmt.Sprintf("backup-manager-web: webui.Assets has no \"dist\" subtree: %v", err))
	}

	handler := server.New(server.Config{
		Backend:       backend,
		Auth:          authSvc,
		BinaryVersion: version,
		Commit:        commit,
		StaticFS:      staticFS,
	})

	httpServer := &http.Server{Addr: *listenAddr, Handler: handler}

	// Both goroutines below are driven by the SAME ctx (§9.3: "the HTTP
	// server and background scheduler SHALL share a common application
	// service and process shutdown context"): backend is the one
	// BackupService both the scheduler loop and every /api/v1 handler
	// call into, and ctx is what tells both of them, independently, that
	// it is time to stop. Neither one tells the other to stop; both react
	// to the same cancellation, which is what "failure of the HTTP
	// listener MUST NOT bypass lifecycle safety" (§9.3) actually requires:
	// if the HTTP listener dies for its own reasons (a bind error, say),
	// that must not itself cancel ctx and tear down the scheduler - see
	// the select below, which treats a serverErr distinctly from ctx.Done().
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- httpServer.ListenAndServe() }()

	schedulerErrCh := make(chan error, 1)
	go func() { schedulerErrCh <- backend.RunOnSchedule(ctx, backend.PollInterval()) }()

	var exitErr error
	select {
	case <-ctx.Done():
		// Normal shutdown path: SIGTERM/SIGINT.
	case err := <-serverErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// The HTTP listener died on its own (e.g. the port is already
			// in use). Per §9.3, this must not silently take the scheduler
			// down with it without at least trying to shut down cleanly;
			// it also must not leave the process running with no HTTP
			// surface at all forever, so this DOES still initiate shutdown
			// (there's no HTTP server left to compose with, and running
			// scheduler-only forever would silently violate "generic Web
			// UI is authenticated" - it would just never come back). The
			// distinction from ctx.Done() is that this path reports the
			// failure as this command's own exit code.
			exitErr = fmt.Errorf("http server: %w", err)
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && exitErr == nil {
		exitErr = fmt.Errorf("http server shutdown: %w", err)
	}

	select {
	case err := <-schedulerErrCh:
		if err != nil && exitErr == nil {
			exitErr = fmt.Errorf("scheduler: %w", err)
		}
	case <-time.After(shutdownGrace):
		fmt.Fprintln(os.Stderr, "backup-manager-web: timed out waiting for the scheduler loop to stop")
	}

	if exitErr != nil {
		return fail(exitErr)
	}
	return 0
}

// displayBaseURL turns a --listen value (":8080", "0.0.0.0:8080",
// "127.0.0.1:8080") into a URL an operator could actually open, for the
// bootstrap-enrollment notice. A bare ":PORT" form (this binary's own
// default, and container/compose.yaml's LISTEN_ADDR) has no usable host
// part to print, so this substitutes "localhost" rather than printing a
// URL with an empty authority.
func displayBaseURL(listenAddr string) string {
	host := listenAddr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	return "http://" + host
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "backup-manager-web:", err)
	return 1
}

// envOrDefault returns the environment variable key's value if set and
// non-empty, or def otherwise. Used for --listen's default
// (container/compose.yaml sets LISTEN_ADDR; a bare `go run` invocation
// outside a container falls back to defaultListenAddr).
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Command backup-manager-web is the generic Web host's own executable
// (issue #82/B4.1, docs/EPIC-B-multi-nas.md §9.2): it runs alongside
// cmd/backup-manager (core/cmd/backup-manager, unchanged by this issue)
// inside the same canonical OCI image, and adds what that binary does
// not have: `serve` (the engine - core service/scheduler, local
// authentication, and the versioned /api/v1 API, sharing one process and
// one shutdown context per §9.3) and `serve-ui` (the shared static UI
// plus a reverse proxy to the engine).
//
// These two run as SEPARATE CONTAINERS in production
// (container/compose.yaml), from the SAME image: `serve` has no
// published port and is reachable only from the `serve-ui` container
// over the internal Docker network, and `serve-ui` is the only container
// with a LAN-facing published port. Splitting them into two commands of
// one binary, rather than two separate binaries or images, is the same
// "one canonical image, vary command" principle already applied to
// `/backup-manager` vs. `/backup-manager-web` themselves.
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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
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
// is set; container/compose.yaml always sets LISTEN_ADDR explicitly for
// both the `serve` and `serve-ui` containers.
const defaultListenAddr = ":8080"

// defaultUpstream matches container/compose.yaml's engine service name
// (`rclone-manager`) on its own internal default port - resolved through
// Docker's embedded DNS on the shared internal network, never a
// published host port (the engine has none).
const defaultUpstream = "http://rclone-manager:8080"

// shutdownGrace bounds how long `serve`/`serve-ui` wait for the HTTP
// server's graceful Shutdown (and, for `serve`, the scheduler loop's own
// exit) before giving up on a clean stop and returning anyway - the
// process is exiting either way once ctx is canceled (SIGTERM/SIGINT);
// this only decides how long it waits first.
const shutdownGrace = 10 * time.Second

// healthcheckTimeout bounds `healthcheck`'s own HTTP GET - short, since
// this runs on the HEALTHCHECK interval and a slow answer is itself a
// sign of trouble, not something worth waiting out.
const healthcheckTimeout = 3 * time.Second

// HTTP server timeouts shared by both `serve` and `serve-ui` (see
// newHTTPServer): issue #119's review flagged that neither http.Server in
// this binary set any request-level timeout at all - the standard Go
// "Slowloris" gap, where a client that opens a connection and trickles
// headers (or a request body) in slowly forever ties up a server
// goroutine indefinitely. `serve-ui` is the one binary in this whole
// image actually exposed to untrusted network input via its published
// port, but both get the same hardening: `serve` sharing a process with
// the backup scheduler (§9.3) means a resource exhausted here has a wider
// blast radius than just one stuck request either way.
const (
	serverReadHeaderTimeout = 10 * time.Second
	serverReadTimeout       = 30 * time.Second
	serverWriteTimeout      = 30 * time.Second
	serverIdleTimeout       = 120 * time.Second
)

// newHTTPServer builds the one *http.Server shape both `serve` and
// `serve-ui` use, so the timeouts above can never be set on one and
// forgotten on the other.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

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
	case "serve-ui":
		return cmdServeUI(args[1:])
	case "healthcheck":
		return cmdHealthcheck(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "backup-manager-web: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: backup-manager-web <command> [flags]

commands:
  serve       run the engine: local authentication, the versioned
              /api/v1 API, and the backup scheduler, sharing one process
              and shutdown context (docs/EPIC-B-multi-nas.md §9.2/§9.3).
              No static UI - this container is not meant to be reached
              directly from a LAN/browser, only from serve-ui.
  serve-ui    serve the shared static UI and reverse-proxy /api/v1 and
              /health requests to the engine (--upstream). This is the
              only one of the two meant to have a published port.
  healthcheck make a single HTTP GET against --url and exit 0 on a 2xx/3xx
              response, 1 otherwise - serve-ui's own HEALTHCHECK, since
              it has no state database to run backup-manager status
              against the way the engine container does.

serve flags:
  --config PATH               path to the manager's YAML config file
                               (default /etc/backup-manager/config.yaml)
  --listen ADDR                address to listen on
                               (default $LISTEN_ADDR, or :8080)
  --auth-store PATH            path to the local-auth administrator record
                               (default /data/state/local-auth.json)
  --auth-mode MODE             authentication mode; only "local" is
                               implemented today (default local)
  --trust-forwarded-headers    trust X-Forwarded-For/X-Forwarded-Proto
                               from the immediate caller (default
                               $TRUST_FORWARDED_HEADERS, or false) - only
                               safe when this process is reachable
                               EXCLUSIVELY through serve-ui's own reverse
                               proxy over an isolated network
                               (container/compose.yaml's shipped topology
                               sets this); never enable it if this
                               listener might also be reached directly by
                               an arbitrary client
  --public-base-url URL        externally-reachable base URL to print in
                               the one-time enrollment link (default
                               $PUBLIC_BASE_URL, or unset) - this
                               process's OWN --listen address is never
                               externally reachable (container/compose.yaml
                               gives it no published port at all), so
                               leaving this unset prints just the raw
                               bootstrap token instead of a clickable but
                               wrong link; set it to serve-ui's own
                               published address, e.g.
                               http://your-nas:8080

serve-ui flags:
  --listen ADDR    address to listen on (default $LISTEN_ADDR, or :8080)
  --upstream URL   the engine's base URL, reachable over the internal
                    Docker network (default $UPSTREAM_ADDR, or
                    http://rclone-manager:8080)

healthcheck flags:
  --url URL   URL to GET (default $LISTEN_ADDR turned into
               http://127.0.0.1:<port>/)
`)
}

func cmdServe(args []string) int {
	fset := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "path to the manager's YAML config file")
	listenAddr := fset.String("listen", envOrDefault("LISTEN_ADDR", defaultListenAddr), "address to listen on")
	authStorePath := fset.String("auth-store", defaultAuthStorePath, "path to the local-auth administrator record")
	authMode := fset.String("auth-mode", "local", "authentication mode (only \"local\" is implemented)")
	trustForwardedHeaders := fset.Bool("trust-forwarded-headers", envBoolOrDefault("TRUST_FORWARDED_HEADERS", false),
		"trust X-Forwarded-For/X-Forwarded-Proto from the immediate caller - only safe behind serve-ui's own reverse proxy over an isolated network (see this command's own --help)")
	publicBaseURL := fset.String("public-base-url", envOrDefault("PUBLIC_BASE_URL", ""),
		"externally-reachable base URL for the one-time enrollment link (default: print just the raw token, since this process's own --listen address is never externally reachable)")
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
	defer func() { _ = cleanup() }()

	authSvc, err := local.New(local.Config{
		StorePath:             *authStorePath,
		TrustForwardedHeaders: *trustForwardedHeaders,
	})
	if err != nil {
		return fail(fmt.Errorf("open local-auth store: %w", err))
	}
	// *publicBaseURL is empty unless an operator explicitly set
	// --public-base-url/$PUBLIC_BASE_URL: this process's OWN --listen
	// address (the fallback issue #119's review flagged) is never
	// externally reachable in the shipped topology
	// (container/compose.yaml gives the engine no published port at
	// all), so PrintBootstrapNotice prints just the raw token in that
	// case, per its own doc, rather than a clickable but wrong link.
	if err := authSvc.PrintBootstrapNotice(os.Stdout, *publicBaseURL); err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web: printing bootstrap notice:", err)
	}

	handler := server.NewEngine(server.EngineConfig{
		Backend:       backend,
		Auth:          authSvc,
		BinaryVersion: version,
		Commit:        commit,
	})

	httpServer := newHTTPServer(*listenAddr, handler)

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

// cmdServeUI runs the UI-host container's whole job: serve the shared
// static UI and reverse-proxy /api/v1 and /health requests to the
// engine. Deliberately much simpler than cmdServe - a plain HTTP server
// with no BackupService, no local-auth store, and no scheduler to
// coordinate a shutdown with, since none of those live in this
// container.
func cmdServeUI(args []string) int {
	fset := flag.NewFlagSet("serve-ui", flag.ContinueOnError)
	listenAddr := fset.String("listen", envOrDefault("LISTEN_ADDR", defaultListenAddr), "address to listen on")
	upstream := fset.String("upstream", envOrDefault("UPSTREAM_ADDR", defaultUpstream), "the engine's base URL, reachable over the internal Docker network")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup-manager-web: invalid --upstream %q: %v\n", *upstream, err)
		return 2
	}
	if upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		fmt.Fprintf(os.Stderr, "backup-manager-web: --upstream %q must be an absolute URL (e.g. http://rclone-manager:8080)\n", *upstream)
		return 2
	}

	staticFS, err := fs.Sub(webui.Assets, "dist")
	if err != nil {
		// webui.Assets is a compile-time go:embed of this module's own
		// webui/dist directory: this can only fail if that package was
		// edited to embed something else without updating this constant,
		// a programmer error to notice loudly, not a runtime condition.
		panic(fmt.Sprintf("backup-manager-web: webui.Assets has no \"dist\" subtree: %v", err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler := server.NewUI(server.UIConfig{Upstream: upstreamURL, StaticFS: staticFS})
	httpServer := newHTTPServer(*listenAddr, handler)

	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- httpServer.ListenAndServe() }()

	var exitErr error
	select {
	case <-ctx.Done():
	case err := <-serverErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			exitErr = fmt.Errorf("http server: %w", err)
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && exitErr == nil {
		exitErr = fmt.Errorf("http server shutdown: %w", err)
	}

	if exitErr != nil {
		return fail(exitErr)
	}
	return 0
}

// cmdHealthcheck is serve-ui's own HEALTHCHECK: since that container has
// no config, no state database, and no `backup-manager status` to run
// (that binary/subcommand belongs to the engine's own container, and
// checks REAL backup health, not "is a web server listening"), this asks
// the one question that actually applies here: does the UI host's own
// HTTP server answer at all. distroless has no shell and no curl/wget,
// so this exists specifically to give HEALTHCHECK's exec-form CMD
// something to invoke.
func cmdHealthcheck(args []string) int {
	fset := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	target := fset.String("url", localHealthcheckURL(envOrDefault("LISTEN_ADDR", defaultListenAddr)), "URL to GET")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web: healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "backup-manager-web: healthcheck: %s returned status %d\n", *target, resp.StatusCode)
		return 1
	}
	return 0
}

// localHealthcheckURL turns a --listen/LISTEN_ADDR value ("[HOST]:PORT")
// into a URL this same process can GET against itself: HOST is replaced
// with 127.0.0.1 whenever it is empty (the normal ":8080" form) or a
// wildcard bind address (0.0.0.0, ::), since a healthcheck run as a
// subprocess of this same container always reaches itself over loopback,
// never through whatever interface the server itself is bound to
// listen on.
func localHealthcheckURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// Not a valid "host:port" pair at all; fall back to treating the
		// whole value as a port-only suffix rather than producing a URL
		// guaranteed to fail to parse.
		return "http://127.0.0.1" + listenAddr + "/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "backup-manager-web:", err)
	return 1
}

// envOrDefault returns the environment variable key's value if set and
// non-empty, or def otherwise. Used for --listen/--upstream's defaults
// (container/compose.yaml sets LISTEN_ADDR/UPSTREAM_ADDR; a bare `go run`
// invocation outside a container falls back to the hardcoded defaults).
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBoolOrDefault is envOrDefault's boolean counterpart, used by
// --trust-forwarded-headers/$TRUST_FORWARDED_HEADERS: an unset or
// unparsable value falls back to def rather than failing this command's
// flag parsing outright, since a malformed environment variable
// shouldn't be able to silently flip a security-relevant default the
// wrong way.
func envBoolOrDefault(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return parsed
}

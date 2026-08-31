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
//
// Issue #129: this binary itself builds no HTTP routing or
// shutdown-orchestration logic anymore - that composition (an equivalent
// of the former apps/generic/server.NewEngine/NewUI, and the former
// cmdServe's own goroutine/shutdown-context dance) now lives in
// apps/common/webhost/serve, reusable by any other provider app. What is
// left here is genuinely generic-provider-specific: flag/env parsing, and
// constructing this provider's own apps/common/auth/local.Service and
// apps/generic/platform.Adapter to hand to that shared composition.
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
	"strings"
	"syscall"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/platform/notify"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
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
// container/compose.yaml's mount point. The mount is the DIRECTORY
// /etc/backup-manager/config (issue #196) and config.yaml lives inside
// it; --config also accepts that directory.
const defaultConfigPath = "/etc/backup-manager/config/config.yaml"

// defaultStateDatabase is where a FIRST-RUN configuration will point its
// SQLite journal (issue #176). It matches what
// scripts/deploy/deploy_generic.py's render_config_yaml has always
// written and what container/compose.yaml already mounts (STATE_DIR ->
// /data/state), so an instance an administrator sets up through the web
// UI lands in exactly the same place as one deployed by that script.
//
// It is a deployment fact, never something an API caller supplies: see
// core/service.FirstRunDefaults' own doc for why that boundary matters.
const defaultStateDatabase = "/data/state/state.db"

// defaultAuthStorePath lives inside the SAME already-writable state
// volume container/compose.yaml already mounts for the SQLite journal
// (STATE_DIR -> /data/state), so serving local authentication needs no
// additional volume of its own.
const defaultAuthStorePath = "/data/state/local-auth.json"

// defaultListenAddr is used only when neither --listen nor LISTEN_ADDR
// is set; container/compose.yaml always sets LISTEN_ADDR explicitly for
// both the `serve` and `serve-ui` containers.
const defaultListenAddr = ":8080"

// defaultProfile is the runtime profile a deployment gets when it does
// not select one (issue #167). Generic is the right default and not a
// guess: it is the profile with no host integration at all, so defaulting
// to it can only ever under-claim. Defaulting the other way — inferring a
// platform from the environment — is how a deployment ends up trusting an
// identity header nobody configured a gateway for.
const defaultProfile = string(profile.Generic)

// defaultUpstream matches container/compose.yaml's engine service name
// (`rclone-manager`) on its own internal default port - resolved through
// Docker's embedded DNS on the shared internal network, never a
// published host port (the engine has none).
const defaultUpstream = "http://rclone-manager:8080"

// shutdownGrace bounds how long `serve`/`serve-ui` wait for the HTTP
// server's graceful Shutdown (and, for `serve`, the scheduler loop's own
// exit) before giving up on a clean stop and returning anyway - the
// process is exiting either way once ctx is canceled (SIGTERM/SIGINT);
// this only decides how long it waits first. Matches
// serve.DefaultShutdownGrace; named separately here only so this file
// never has to import serve just to read a constant its own flags don't
// expose.
const shutdownGrace = serve.DefaultShutdownGrace

// healthcheckTimeout bounds `healthcheck`'s own HTTP GET - short, since
// this runs on the HEALTHCHECK interval and a slow answer is itself a
// sign of trouble, not something worth waiting out.
const healthcheckTimeout = 3 * time.Second

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
                               (default /etc/backup-manager/config/config.yaml).
                               If this file does not exist, serve starts
                               anyway and offers the first-run setup flow
                               instead of refusing to start (issue #176);
                               a file that exists and does not validate is
                               still a hard startup failure
  --state-database PATH        SQLite journal path written into a
                               first-run configuration (default
                               $STATE_DATABASE, or /data/state/state.db).
                               Ignored once a config file exists, which
                               names its own
  --listen ADDR                address to listen on
                               (default $LISTEN_ADDR, or :8080)
  --profile NAME               runtime profile: generic or ugos
                               (default $RUNTIME_PROFILE, or generic). A
                               profile changes the authentication
                               gateway, the notification bridge, the
                               launch bridge and reported capabilities,
                               and nothing else - it can never change
                               lifecycle, retention or validation
                               behaviour
  --trusted-upstream CIDR[,...]
                               the network ranges THIS container may
                               believe a provider-native identity header
                               from (default $TRUSTED_UPSTREAM_CIDRS).
                               Deliberately not the same variable
                               serve-ui reads: this container's only
                               possible peer is serve-ui itself, so this
                               range names the internal network and
                               serve-ui's range names the platform
                               GATEWAY, and the two are mutually
                               exclusive values. One variable feeding
                               both hops has exactly one value that lets
                               a gateway deployment authenticate, and
                               that value also makes serve-ui believe
                               anything on the internal network, which is
                               the LAN-forgery bug restated as
                               configuration (issue #87).
                               Required by a gateway profile: without it
                               there is no gateway, only an identity
                               header anyone on the LAN can set, so the
                               process refuses to start
  --auth-store PATH            path to the local-auth administrator record
                               (default /data/state/local-auth.json)
  --auth-mode MODE             authentication mode: "local" or "gateway".
                               Left unset it follows the profile, and it
                               is refused when it contradicts one
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
  --trusted-gateway CIDR[,...]
                   the network ranges this container may believe a
                   provider-native identity header from (default
                   $TRUSTED_GATEWAY_CIDRS). THIS is the hop the boundary
                   is actually on: this is the only container with a
                   LAN-facing published port, so it is the only place
                   where "did the platform gateway send this, or did
                   somebody on the LAN" is still a question the network
                   can answer. Left unset, every provider-native identity
                   header is stripped from every inbound request, which
                   is the right default and is why a gateway profile
                   refuses to start without it rather than serving a
                   console that can never sign anyone in. This range
                   names the GATEWAY and never the internal network; the
                   engine's own peer set is a separate variable
                   (serve --trusted-upstream) precisely because a value
                   correct for one hop is wrong for the other
  --upstream URL   the engine's base URL, reachable over the internal
                    Docker network (default $UPSTREAM_ADDR, or
                    http://rclone-manager:8080)
  --profile NAME   runtime profile (default $RUNTIME_PROFILE, or
                    generic). Selects which bundle under --ui-root is
                    served
  --ui-root PATH   a directory of per-profile UI bundles (default
                    $UI_ROOT). The bundle served is <PATH>/<profile>
  --ui-dir PATH    one explicit UI bundle directory (default $UI_DIR),
                    which wins over --ui-root. Both exist so a provider
                    package can ship its own bridge WITHOUT a
                    provider-specific binary: the bundle is chosen at run
                    time, so the binary's digest is identical whichever
                    bridge is served (issue #180, section 3.7's
                    one-binary rule)

  With neither --ui-dir nor --ui-root, the bundle compiled into this
  binary is served. A --ui-dir or --ui-root that turns out to be unusable
  is a hard start failure, never a silent fall back to that bundle: a UI
  that looks like it works while running the wrong provider bridge is the
  exact defect this mechanism exists to remove.

healthcheck flags:
  --url URL   URL to GET (default $LISTEN_ADDR turned into
               http://127.0.0.1:<port>/)
`)
}

func cmdServe(args []string) int {
	fset := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "path to the manager's YAML config file")
	listenAddr := fset.String("listen", envOrDefault("LISTEN_ADDR", defaultListenAddr), "address to listen on")
	profileName := fset.String("profile", envOrDefault("RUNTIME_PROFILE", defaultProfile), "runtime profile (generic or ugos)")
	trustedUpstream := fset.String("trusted-upstream", envOrDefault("TRUSTED_UPSTREAM_CIDRS", ""),
		"comma-separated CIDR ranges THIS container may believe a provider-native identity header from (the proxy in front of it); required by a gateway profile")
	// Accepted only to refuse it by name. serve used to read
	// TRUSTED_GATEWAY_CIDRS, which is serve-ui's variable and means
	// something else here (issue #87's review, M1), and an operator who
	// carries the old flag over would otherwise get "flag provided but
	// not defined" and no idea which of the two hops they are looking at.
	legacyTrustedGateway := fset.String("trusted-gateway", "",
		"rejected: this hop's peer set is --trusted-upstream; --trusted-gateway names the platform gateway and belongs to serve-ui")
	authStorePath := fset.String("auth-store", defaultAuthStorePath, "path to the local-auth administrator record")
	authMode := fset.String("auth-mode", "", "authentication mode (\"local\" or \"gateway\"); follows the profile when unset")
	trustForwardedHeaders := fset.Bool("trust-forwarded-headers", envBoolOrDefault("TRUST_FORWARDED_HEADERS", false),
		"trust X-Forwarded-For/X-Forwarded-Proto from the immediate caller - only safe behind serve-ui's own reverse proxy over an isolated network (see this command's own --help)")
	publicBaseURL := fset.String("public-base-url", envOrDefault("PUBLIC_BASE_URL", ""),
		"externally-reachable base URL for the one-time enrollment link (default: print just the raw token, since this process's own --listen address is never externally reachable)")
	stateDatabase := fset.String("state-database", envOrDefault("STATE_DATABASE", defaultStateDatabase),
		"SQLite journal path written into a first-run configuration; ignored once a config file exists, which names its own")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	// Profile resolution happens before anything opens a file or a
	// listener: an unrecognised profile is a configuration error, and
	// finding it out after the state database is open only makes the
	// message harder to read.
	runtimeProfile, err := profile.Lookup(*profileName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web:", err)
		return 2
	}
	if *legacyTrustedGateway != "" {
		fmt.Fprintln(os.Stderr, "backup-manager-web: serve does not take --trusted-gateway. The two hops trust different peers: --trusted-upstream (TRUSTED_UPSTREAM_CIDRS) is this container's own peer, the reverse proxy in front of it, while --trusted-gateway (TRUSTED_GATEWAY_CIDRS) names the platform gateway and belongs to serve-ui. A single value for both is the one configuration that cannot be correct.")
		return 2
	}
	if *trustedUpstream != "" {
		if runtimeProfile.Gateway == nil {
			fmt.Fprintf(os.Stderr, "backup-manager-web: --trusted-upstream was given but profile %q has no platform authentication gateway to trust\n", runtimeProfile.ID)
			return 2
		}
		runtimeProfile.Gateway.TrustedPeers = splitList(*trustedUpstream)
	}
	if err := checkAuthMode(*authMode, runtimeProfile); err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web:", err)
		return 2
	}
	// Fail closed before anything opens a file or a listener. A gateway
	// profile whose trust boundary does not parse, or is not there at
	// all, is not a deployment with a weaker boundary: it is one with
	// none, where every identity header on the LAN is believed.
	if runtimeProfile.Gateway != nil {
		if _, err := runtimeProfile.Gateway.Compile(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager-web: profile %q: %v\n", runtimeProfile.ID, err)
			return 2
		}
	}

	// Take the signal away from the embedded rclone before anything can
	// reach a remote, so its own handler is never installed at all, and
	// take on the matching obligation to run its exit handlers on the way
	// out. service.Open below wires a real rclone transport and this
	// process drives real backup cycles through it, so `serve` embeds
	// rclone exactly the way `daemon` does; rclone's lib/atexit ends the
	// process with 128+signal once a single transfer has armed it, which
	// made an ordinary `docker stop` of this container exit 143 despite
	// the handler below and despite compose.yaml's own
	// stop_grace_period (issue #212, the same defect issue #190 fixed for
	// the CLI daemon). Docker, Kubernetes and systemd all read a nonzero
	// exit on stop as a failure, so every routine restart of a container
	// documented as long-lived looked like a crash: it counted against
	// restart burst limits and it alerted. A stop an operator asked for,
	// and that this process performed, is a successful stop. A genuine
	// failure is untouched: an error out of RunEngine still goes through
	// fail and still exits 1.
	service.DisableSignalExit()
	defer service.RunExitHandlers()

	// The one signal handler in this binary, matching
	// core/cmd/backup-manager/daemon.go's own convention exactly: this is
	// what makes ctx the "process shutdown context" §9.3 requires the HTTP
	// server and the background scheduler to share - serve.RunEngine below
	// is what actually drives both off it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Local authentication is built BEFORE the configuration is loaded,
	// and that ordering is deliberate (issue #176). Enrollment needs no
	// configuration of its own, and on a fresh install it is what has to
	// happen first: an unconfigured instance reachable on a LAN must not
	// be configurable by whoever reaches the port first, so the operator
	// enrolls with the single-use token printed below and only then sees
	// a setup flow. Before this, the process exited at service.Open and
	// none of this was reachable at all.
	//
	// It is still opened only for a profile that actually uses it. A
	// gateway profile has no login surface of its own, so creating an
	// administrator record it would never consult, and printing an
	// enrolment token nobody can redeem, would be two misleading
	// artifacts rather than a harmless extra. Such a profile reaches the
	// setup flow already authenticated by its platform gateway, which is
	// the same boundary it will use afterwards.
	var (
		authSvc    *local.Service
		authRoutes http.Handler
		localAuth  capabilities.Authenticator
		trustFwd   = *trustForwardedHeaders
	)
	if runtimeProfile.Gateway == nil {
		authSvc, err = local.New(local.Config{
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
		authRoutes = authSvc.Handler()
		localAuth = authSvc.Authenticator()
		trustFwd = authSvc.TrustForwardedHeaders()
	}

	// One adapter, built from the selected profile's row of the table.
	// A future TrueNAS/Synology/UGOS deployment adds a row rather than a
	// binary: that is what makes this a runtime profile rather than a
	// build.
	platformAdapter, err := runtimeProfile.Adapter(profile.AdapterConfig{LocalAuth: localAuth})
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web:", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "backup-manager-web: runtime profile %q (%s), authentication: %s\n",
		runtimeProfile.ID, runtimeProfile.DisplayName, authModeOf(runtimeProfile))

	engineConfig := serve.EngineConfig{
		Platform:              platformAdapter,
		AuthRoutes:            authRoutes,
		TrustForwardedHeaders: trustFwd,
		BinaryVersion:         version,
		Commit:                commit,
	}

	var handler http.Handler
	var scheduler serve.Scheduler

	backend, cleanup, err := service.Open(ctx, *configPath)
	switch {
	case err == nil:
		// The shutdown counterpart of this command's startup lines, and
		// the other half of what #212 is about. The process used to leave
		// whenever rclone got there rather than when the shutdown it was
		// asked to perform had finished, and os.Exit runs no deferred
		// function, so the scheduler, the HTTP server and the state store
		// were all cut off at an arbitrary point with nothing saying so.
		// This prints from inside the deferred close, after the store is
		// really shut, so it is reachable only on the path that actually
		// completed: "the operator stopped it" and "it died" are
		// different things to a reader of these logs afterwards (FR-23).
		defer func() {
			if closeErr := cleanup(); closeErr != nil {
				fmt.Fprintln(os.Stderr, "backup-manager-web: closing the backup service:", closeErr)
			}
			fmt.Fprintln(os.Stderr, "backup-manager-web: shutdown complete, the backup service is closed")
		}()
		enableAlerts(backend, platformAdapter)
		engineConfig.Backend = backend
		handler = serve.NewEngine(engineConfig)
		scheduler = backend

	case errors.Is(err, service.ErrConfigAbsent):
		// Issue #176: no configuration on disk is a fresh install, not a
		// misconfiguration. Serve the setup flow rather than exiting, so
		// an administrator who installed this from an app store can
		// finish the job in the web UI they installed it to reach. A
		// config file that EXISTS and does not validate falls to the
		// default branch below and is still fatal.
		fmt.Fprintf(os.Stderr, "backup-manager-web: no configuration at %s yet; serving the first-run setup flow\n", *configPath)

		firstRun, frErr := service.NewFirstRun(service.FirstRunDefaults{
			ConfigPath:    *configPath,
			StateDatabase: *stateDatabase,
		})
		if frErr != nil {
			return fail(frErr)
		}
		engineConfig.FirstRun = firstRun
		engineConfig.Activate = func(ctx context.Context) (webhost.BackupServiceClient, func() error, error) {
			opened, closeFn, openErr := service.Open(ctx, *configPath)
			if openErr != nil {
				return nil, nil, openErr
			}
			// Alerting is decided from the configuration setup just
			// wrote, exactly as it is for a process that started with
			// one, so a first-run instance is not silently the one
			// deployment shape where the alerts block does nothing until
			// a restart.
			enableAlerts(opened, platformAdapter)
			return opened, closeFn, nil
		}

		engine, engErr := serve.NewFirstRunEngine(engineConfig)
		if engErr != nil {
			return fail(engErr)
		}
		// Same notice as the configured branch above, for the same
		// reason: a fresh install is stopped by the same `docker stop`,
		// and #212 would otherwise be fixed only for instances that had
		// already been configured.
		defer func() {
			if closeErr := engine.Close(); closeErr != nil {
				fmt.Fprintln(os.Stderr, "backup-manager-web: closing the first-run engine:", closeErr)
			}
			fmt.Fprintln(os.Stderr, "backup-manager-web: shutdown complete, the backup service is closed")
		}()
		// The same value is both the HTTP surface and the scheduler: it
		// serves setup now, the application after activation, and its
		// scheduler loop simply waits for a backend to exist rather than
		// this process ending up with no scheduler for its whole life.
		handler = engine
		scheduler = engine

	default:
		return fail(err)
	}

	httpServer := serve.NewHTTPServer(*listenAddr, handler)

	// serve.RunEngine owns the §9.3 orchestration (HTTP server + scheduler
	// share ctx) - see that function's own doc for exactly what each
	// branch does and why.
	if err := serve.RunEngine(ctx, httpServer, scheduler, shutdownGrace, os.Stderr); err != nil {
		return fail(err)
	}
	return 0
}

// enableAlerts is Work Package 3.5's proactive alerting wiring
// (docs/EPIC-B-multi-nas.md §71), factored out of cmdServe because it now
// runs from two places: a process that started with a configuration, and
// one that gained its first configuration through the setup flow.
//
// Both halves have to agree before a single notification goes out: the
// administrator opted in through the config file's alerts block, and this
// platform actually offers a local notification capability to deliver
// through. The generic Docker/Linux adapter declares none
// (apps/generic/platform reports every capability false, never emulated),
// so on this provider the sink refuses at wiring time and alerting stays
// visibly off, printed once here rather than discovered as silence later.
// A provider that DOES declare NativeNotifications gets its alerts
// through exactly this wiring with no further change.
func enableAlerts(backend *service.BackupService, platformAdapter capabilities.PlatformAdapter) {
	if sink, err := notify.NewPlatformSink(platformAdapter); err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web: proactive alerting is off:", err)
	} else if !backend.EnableAlerts(sink) {
		fmt.Fprintln(os.Stderr, "backup-manager-web: proactive alerting is off: the configuration has not set alerts.enabled")
	}
}

// cmdServeUI runs the UI-host container's whole job: serve the shared
// static UI and reverse-proxy /api/v1 and /health requests to the
// engine. Deliberately much simpler than cmdServe - no BackupService, no
// local-auth store, and no scheduler to coordinate a shutdown with, since
// none of those live in this container. serve.RunEngine's nil-Scheduler
// case covers exactly this shape.
func cmdServeUI(args []string) int {
	fset := flag.NewFlagSet("serve-ui", flag.ContinueOnError)
	listenAddr := fset.String("listen", envOrDefault("LISTEN_ADDR", defaultListenAddr), "address to listen on")
	upstream := fset.String("upstream", envOrDefault("UPSTREAM_ADDR", defaultUpstream), "the engine's base URL, reachable over the internal Docker network")
	profileName := fset.String("profile", envOrDefault("RUNTIME_PROFILE", defaultProfile), "runtime profile (generic or ugos)")
	trustedGateway := fset.String("trusted-gateway", envOrDefault("TRUSTED_GATEWAY_CIDRS", ""),
		"comma-separated CIDR ranges this LAN-facing container may believe a provider-native identity header from; required by a gateway profile")
	uiRoot := fset.String("ui-root", envOrDefault("UI_ROOT", ""), "a directory of per-profile UI bundles; the bundle served is <ui-root>/<profile>")
	uiDir := fset.String("ui-dir", envOrDefault("UI_DIR", ""), "one explicit UI bundle directory, which wins over --ui-root")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	runtimeProfile, err := profile.Lookup(*profileName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web:", err)
		return 2
	}

	// The trust boundary is resolved before anything opens a listener, and
	// it is resolved HERE rather than only in `serve` because this is the
	// hop that faces the LAN (issue #87). An engine behind this proxy
	// trusts this proxy by necessity — it is the engine's only possible
	// peer — so a provider-native identity header this container forwards
	// unexamined is a header the engine believes, whoever set it.
	//
	// A gateway profile with no range configured is refused rather than
	// silently stripped: stripping is the safe behaviour, but a UGOS
	// console nobody can sign in to, with no message saying why, is an
	// operator debugging the wrong thing for an afternoon.
	edgeGateway, err := compileEdgeGateway(runtimeProfile, *trustedGateway)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup-manager-web:", err)
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

	embedded, err := fs.Sub(webui.Assets, "dist")
	if err != nil {
		// webui.Assets is a compile-time go:embed of this module's own
		// webui/dist directory: this can only fail if that package was
		// edited to embed something else without updating this constant,
		// a programmer error to notice loudly, not a runtime condition.
		panic(fmt.Sprintf("backup-manager-web: webui.Assets has no \"dist\" subtree: %v", err))
	}

	// Issue #180, owned by #167. The bundle is chosen HERE, at run time,
	// rather than by whatever VITE_PLATFORM happened to be set to when
	// the binary was built. That is what lets a provider package ship its
	// own bridge while carrying the exact same core binary digest section
	// 3.7 requires, and apps/generic/tests/uibundle proves it against a
	// real built artifact rather than against this function.
	bundle, err := serve.ResolveUIBundle(serve.UIBundleSource{
		Dir:      *uiDir,
		Root:     *uiRoot,
		Profile:  bundleNameFor(runtimeProfile, *uiRoot),
		Embedded: embedded,
	})
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "backup-manager-web: runtime profile %q, UI bundle %s (%s)\n",
		runtimeProfile.ID, bundle.Origin, bundle.Detail)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler := serve.NewUI(serve.UIConfig{Upstream: upstreamURL, StaticFS: bundle.FS, Gateway: edgeGateway})
	httpServer := serve.NewHTTPServer(*listenAddr, handler)

	if err := serve.RunEngine(ctx, httpServer, nil, shutdownGrace, os.Stderr); err != nil {
		return fail(err)
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

// splitList turns a comma-separated flag value into a trimmed,
// empty-free list. Used by --trusted-gateway, where a stray space around
// a CIDR range would otherwise become a parse failure an operator has to
// squint at.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// authModeOf names how the selected profile authenticates, for the one
// startup line that says what this process is actually doing.
func authModeOf(p profile.Profile) string {
	if p.Gateway != nil {
		return "trusted platform gateway (" + p.Gateway.UsernameHeader + ")"
	}
	return "local account"
}

// checkAuthMode holds --auth-mode to the profile. Leaving it unset is the
// normal case and follows the profile; naming a mode the profile does not
// have is refused rather than silently ignored, because "--auth-mode
// local on a gateway profile" is somebody expecting a login form that
// will never appear.
func checkAuthMode(mode string, p profile.Profile) error {
	want := "local"
	if p.Gateway != nil {
		want = "gateway"
	}
	switch mode {
	case "":
		return nil
	case want:
		return nil
	default:
		return fmt.Errorf("--auth-mode %q contradicts profile %q, which authenticates through the %s mode", mode, p.ID, want)
	}
}

// compileEdgeGateway resolves the LAN-facing container's own trust
// boundary from the selected profile and --trusted-gateway.
//
// nil, nil is the answer for a profile with no gateway, and it means "no
// peer is trusted at this hop", not "no check": serve.NewUI reads a nil
// gateway as strip-everything. A gateway profile with no range is an
// error, matching `serve`'s own refusal, because that combination is
// somebody expecting a native session that will never arrive.
func compileEdgeGateway(p profile.Profile, trusted string) (*profile.CompiledGateway, error) {
	if p.Gateway == nil {
		if trusted != "" {
			return nil, fmt.Errorf("--trusted-gateway was given but profile %q has no platform authentication gateway to trust", p.ID)
		}
		return nil, nil
	}
	peers := splitList(trusted)
	if len(peers) == 0 {
		return nil, fmt.Errorf("profile %q: %w (serve-ui is the container with the LAN-facing port, so this is the hop the gateway actually connects to)", p.ID, profile.ErrNoTrustedPeer)
	}
	return (&profile.Gateway{TrustedPeers: peers, UsernameHeader: p.Gateway.UsernameHeader}).Compile()
}

// bundleNameFor is which subdirectory of --ui-root this profile serves.
// It returns "" when no root is configured, so ResolveUIBundle reports
// the honest "no --ui-root" case rather than a confusing "no bundle for
// profile generic" against a root nobody set.
func bundleNameFor(p profile.Profile, root string) string {
	if root == "" {
		return ""
	}
	if p.UIBundle != "" {
		return p.UIBundle
	}
	return string(p.ID)
}

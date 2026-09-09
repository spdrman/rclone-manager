// Command rbm-web is the generic Web host's own executable
// (issue #82/B4.1, docs/EPIC-B-multi-nas.md §9.2): it runs alongside the
// CLI (core/cmd/backup-manager, unchanged by this issue) inside the same
// canonical OCI image, and adds what that binary does not have: `serve`
// (the engine - core service/scheduler, local authentication, and the
// versioned /api/v1 API, sharing one process and one shutdown context per
// §9.3) and `serve-ui` (the shared static UI plus a reverse proxy to the
// engine).
//
// It is `rbm-web` to an operator and this directory is still
// cmd/backup-manager-web, for the same reason `rbm` lives in
// cmd/backup-manager: the image symlinks the old name beside the new one,
// and a Go package path is not something anybody types. Every name this
// binary prints for itself comes from core/cliecho.WebBinary, and the
// whole of that argument is in core/cliecho/cliname.go. selfname_test.go
// beside this file is what keeps it that way, and says why the paths
// under /etc and the image reference deliberately do not follow.
//
// These two run as SEPARATE CONTAINERS in production
// (container/compose.yaml), from the SAME image: `serve` has no
// published port and is reachable only from the `serve-ui` container
// over the internal Docker network, and `serve-ui` is the only container
// with a LAN-facing published port. Splitting them into two commands of
// one binary, rather than two separate binaries or images, is the same
// "one canonical image, vary command" principle already applied to
// `/rbm` vs. `/rbm-web` themselves.
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
	"io"
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
	"github.com/spdrman/rclone-manager/core/cliecho"
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

// Where a FIRST-RUN configuration points its SQLite journal (issue #176)
// is deliberately NOT a constant here. It is
// core/service.StateDatabaseDefault, which core/cmd/backup-manager's own
// --state-database also takes its default from, because #571 rests on
// this process and a `backup-set create` typed on the same host naming
// the same journal: this one announces about it before it serves the
// setup flow, and that one finds the announcement by asking about it.
// Two copies of one path, one of which read $STATE_DATABASE and one of
// which did not, is how that guarantee came apart.

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

// main is one line on purpose: everything worth testing lives in run,
// which returns an exit code instead of calling os.Exit, so the whole
// command surface can be driven from a test in-process. The only thing
// that genuinely cannot be observed that way is what the process exits
// with after a signal, and serve_signal_test.go re-executes the test
// binary to get at it.
func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches to one of the four commands and returns the process exit
// code, from the constants beside fail below: 2 for a usage error (no
// command, or one this binary does not have), 1 for a command that ran
// and failed, and 3 for `serve` refused because something else already
// serves this deployment (#551). That is the same split
// core/cmd/backup-manager publishes in its own usage block, so a caller
// scripting either binary reads them the same way. That last sentence was
// already here while it was untrue: #551 gave the CLI a third status and
// this binary kept returning 1 for the identical refusal, which is the
// half of that issue this file is.
func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	case "serve-ui":
		return cmdServeUI(args[1:])
	case "healthcheck":
		return cmdHealthcheck(args[1:])
	case "auth":
		return cmdAuth(args[1:])
	default:
		fmt.Fprintf(os.Stderr, cliecho.WebBinary+": unknown command %q\n\n", args[0])
		usage()
		return exitUsage
	}
}

// usage writes the command surface an operator reads. A reworded line
// here is a change to something somebody already has in a runbook, so it
// is a deliberate act with a recorded reason rather than an edit, and
// adding a command means adding a line. Two tests hold that:
// TestUsageDocumentsEveryModeTheBinaryCarries requires every mode this
// binary carries to appear here, and TestUsageIntroducesThisBinaryByName
// requires the first line to name it.
//
// It is NOT pinned byte for byte by the compatibility corpus, and this
// comment said it was until 0.3.3. FR-35 clause 4 is core/tests/compat,
// which builds ./cmd/backup-manager from the core module and runs THAT;
// it has never built this binary and cannot, because core may not reach
// into apps/. So the runbook promise above is held by the two tests named
// beside this file and by nothing else, which is worth knowing before
// relying on it: a wrong sentence about what guards a block is how a
// rename crossed five pull requests without touching the block it was
// about.
func usage() {
	// Fprintf over ONE raw literal, rather than the name concatenated into
	// it at each of the three places this block spells a command.
	//
	// The three are unavoidable: the first line is this binary's own name,
	// the healthcheck entry says which CLI command it stands in for, and
	// the auth entry shows the invocation an operator types. Splicing a
	// constant into any of them would close the literal there, and #648
	// already paid for what that costs: distribution/packaging reads the
	// CLI's command index straight out of its usage(), took the first
	// backticked string, and a splice on the FIRST line made it read a list
	// of zero commands. Nothing parses this block today, and the hazard is
	// not that it does; it is that a splice below the command index would
	// truncate a future reader silently rather than loudly. A literal with
	// verbs in it stays one chunk however anybody reads it.
	fmt.Fprintf(os.Stderr, `usage: %s <command> [flags]

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
              it has no state database to run %s status against the way
              the engine container does.
  auth create-admin --username U --password-stdin [--auth-store PATH]
              provision the first Web UI administrator directly in the
              local-auth store (apps/common/auth/local.CreateAdmin), with
              no HTTP request and no running server involved at all
              (issue #322). For the headless case the normal enrollment
              flow cannot help: no browser, and no server up yet to print
              a bootstrap token to. Refuses if a server is currently
              running against the same --auth-store (its lock is held for
              as long as it is up) - run this before first start, or
              while the server is stopped.

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

auth create-admin flags:
  --auth-store PATH   path to the local-auth administrator record
                       (default /data/state/local-auth.json, matching
                       serve's own --auth-store default)
  --username U         administrator username to create (required)
  --password-stdin     read the administrator password from stdin
                       (required; nothing else reads it, so it never
                       appears in this process's own argument list -
                       e.g. echo -n "$PASS" | %s auth create-admin
                       --username admin --password-stdin)
`, cliecho.WebBinary, cliecho.Binary, cliecho.WebBinary)
}

// cmdServe runs the engine: the API, the local-auth service, the backend
// and the scheduler, sharing one process and one shutdown context.
//
// Almost all of its length is flag and environment parsing followed by
// refusals, and that is where the value is. Every check below happens
// before anything binds a port or opens a journal, so a deployment that
// is misconfigured stops with a sentence about what is wrong instead of
// starting and being subtly wrong. The two peer-related flags are the ones
// worth reading carefully: --trusted-upstream names who may talk to THIS
// hop, and --trusted-gateway, which belongs to serve-ui and names the
// platform gateway, is accepted here only so it can be refused by name.
// An operator who carried the old spelling over gets told which of the two
// hops they are looking at rather than a bare unknown-flag error.
//
// The composition itself is not here. It moved to
// apps/common/webhost/serve so every other provider gets the same
// orchestration; what is left is the part that is genuinely specific to
// this provider.
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
	stateDatabase := fset.String("state-database", service.StateDatabaseDefault(),
		"SQLite journal path written into a first-run configuration; ignored once a config file exists, which names its own")
	if err := fset.Parse(args); err != nil {
		return exitUsage
	}

	// Profile resolution happens before anything opens a file or a
	// listener: an unrecognised profile is a configuration error, and
	// finding it out after the state database is open only makes the
	// message harder to read.
	runtimeProfile, err := profile.Lookup(*profileName)
	if err != nil {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+":", err)
		return exitUsage
	}
	if *legacyTrustedGateway != "" {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+": serve does not take --trusted-gateway. The two hops trust different peers: --trusted-upstream (TRUSTED_UPSTREAM_CIDRS) is this container's own peer, the reverse proxy in front of it, while --trusted-gateway (TRUSTED_GATEWAY_CIDRS) names the platform gateway and belongs to serve-ui. A single value for both is the one configuration that cannot be correct.")
		return exitUsage
	}
	if *trustedUpstream != "" {
		if runtimeProfile.Gateway == nil {
			fmt.Fprintf(os.Stderr, cliecho.WebBinary+": --trusted-upstream was given but profile %q has no platform authentication gateway to trust\n", runtimeProfile.ID)
			return exitUsage
		}
		runtimeProfile.Gateway.TrustedPeers = splitList(*trustedUpstream)
	}
	if err := checkAuthMode(*authMode, runtimeProfile); err != nil {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+":", err)
		return exitUsage
	}
	// Fail closed before anything opens a file or a listener. A gateway
	// profile whose trust boundary does not parse, or is not there at
	// all, is not a deployment with a weaker boundary: it is one with
	// none, where every identity header on the LAN is believed.
	if runtimeProfile.Gateway != nil {
		if _, err := runtimeProfile.Gateway.Compile(); err != nil {
			fmt.Fprintf(os.Stderr, cliecho.WebBinary+": profile %q: %v\n", runtimeProfile.ID, err)
			return exitUsage
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
			fmt.Fprintln(os.Stderr, cliecho.WebBinary+": printing bootstrap notice:", err)
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
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+":", err)
		return exitUsage
	}
	fmt.Fprintf(os.Stderr, cliecho.WebBinary+": runtime profile %q (%s), authentication: %s\n",
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

	// Issue #537: this process is about to serve the deployment, so it
	// says so before it reads a thing. A `backup-set create` typed into a
	// shell on the same host finds this announcement and refuses, rather
	// than rewriting a configuration this process has already read and
	// will never read again. Announcing before the open is what makes
	// that check an exclusion rather than a sample; core/service's
	// liveengine.go has the whole arrangement.
	//
	// On a first-run instance there is no configuration to name a journal
	// yet, and that used to mean no announcement at all until setup had
	// written one. Issue #571 is what that cost: an operator who installed
	// fresh, enrolled an administrator and then typed `backup-set create`
	// got a set written into a config.yaml the process below will never
	// read, exit 0, and a Web UI still answering 503. So the journal
	// --state-database names is handed over as well, because it is what
	// this process is going to serve either way: the configuration setup
	// writes names exactly that path, and the CLI's own --state-database
	// carries the same packaged default, which is what makes the
	// announcement something a `backup-set create` on this host can find.
	serving, err := service.AnnounceServingFirstRun(*configPath, *stateDatabase)
	if err != nil {
		return failServing(err)
	}
	defer func() { _ = serving.Release() }()
	// A first install that cannot announce itself yet still comes up, and
	// that is PR #581's review rather than a looseness. Announcing means
	// creating a lock file in the state directory, and a read-only bind
	// mount, a volume mounted after this service starts, or a uid that
	// cannot write /data/state made a fresh install exit here and be
	// restarted forever by its supervisor, diagnosable only through
	// `docker logs`. That is the one start where the operator has nothing
	// but a browser, so the setup flow is served and told to say why,
	// while setup itself stays refused (gateFirstRunOnServing below) until
	// the announcement can really be made. core/service's FirstRunServing
	// has the whole argument, including which failures still stop a start.
	if blocked := serving.Blocked(); blocked != nil {
		fmt.Fprintf(os.Stderr, cliecho.WebBinary+": this deployment's state directory cannot be used yet, so nothing has been announced and the setup flow will refuse to complete until it can be: %v\n", blocked)
	}

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
				fmt.Fprintln(os.Stderr, cliecho.WebBinary+": closing the backup service:", closeErr)
			}
			fmt.Fprintln(os.Stderr, cliecho.WebBinary+": shutdown complete, the backup service is closed")
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
		fmt.Fprintf(os.Stderr, cliecho.WebBinary+": no configuration at %s yet; serving the first-run setup flow\n", *configPath)

		firstRun, frErr := service.NewFirstRun(service.FirstRunDefaults{
			ConfigPath:    *configPath,
			StateDatabase: *stateDatabase,
		})
		if frErr != nil {
			return fail(frErr)
		}
		engineConfig.FirstRun = gateFirstRunOnServing(firstRun, serving)
		engineConfig.Activate = func(ctx context.Context) (webhost.BackupServiceClient, func() error, error) {
			// There is no announcement here any more, and its absence is
			// the fix for #571 rather than an omission. This used to be
			// the first moment this process could say what it served,
			// which left the whole of the setup flow invisible to a
			// `backup-set create` on the same host. The announcement is
			// now made before the setup flow is served at all, for the
			// journal --state-database names, and CreateInitialConfig
			// writes that same path into state.database (FirstRunDefaults
			// is where both read it from), so what this process serves
			// from here on is the deployment it already announced.
			//
			// Announcing a second time would not be harmless either: the
			// serving lock is taken EXCLUSIVELY and flock attaches to the
			// open file description, so a second acquire in this process
			// is a second description, and it would wait out the lock
			// timeout and then be refused as ErrAlreadyServing by the
			// announcement this process is already holding.
			//
			// A failure below is returned rather than fatal, and that is
			// not an oversight: this process is not exiting. It is serving
			// a setup flow, and a refusal here reaches the operator through
			// the API as restart_required (FirstRunEngine.activate), with
			// the configuration already durably written, so a restart
			// genuinely does finish the job.
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
				fmt.Fprintln(os.Stderr, cliecho.WebBinary+": closing the first-run engine:", closeErr)
			}
			fmt.Fprintln(os.Stderr, cliecho.WebBinary+": shutdown complete, the backup service is closed")
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
	return exitOK
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
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+": proactive alerting is off:", err)
	} else if !backend.EnableAlerts(sink) {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+": proactive alerting is off: the configuration has not set alerts.enabled")
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
		return exitUsage
	}

	runtimeProfile, err := profile.Lookup(*profileName)
	if err != nil {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+":", err)
		return exitUsage
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
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+":", err)
		return exitUsage
	}

	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, cliecho.WebBinary+": invalid --upstream %q: %v\n", *upstream, err)
		return exitUsage
	}
	if upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		fmt.Fprintf(os.Stderr, cliecho.WebBinary+": --upstream %q must be an absolute URL (e.g. http://rclone-manager:8080)\n", *upstream)
		return exitUsage
	}

	embedded, err := fs.Sub(webui.Assets, "dist")
	if err != nil {
		// webui.Assets is a compile-time go:embed of this module's own
		// webui/dist directory: this can only fail if that package was
		// edited to embed something else without updating this constant,
		// a programmer error to notice loudly, not a runtime condition.
		panic(fmt.Sprintf(cliecho.WebBinary+": webui.Assets has no \"dist\" subtree: %v", err))
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
	fmt.Fprintf(os.Stderr, cliecho.WebBinary+": runtime profile %q, UI bundle %s (%s)\n",
		runtimeProfile.ID, bundle.Origin, bundle.Detail)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler := serve.NewUI(serve.UIConfig{Upstream: upstreamURL, StaticFS: bundle.FS, Gateway: edgeGateway})
	httpServer := serve.NewHTTPServer(*listenAddr, handler)

	if err := serve.RunEngine(ctx, httpServer, nil, shutdownGrace, os.Stderr); err != nil {
		return fail(err)
	}
	return exitOK
}

// cmdAuth dispatches `auth`'s own subcommands, one level deeper than
// run's own top-level switch, the same shape core/cmd/backup-manager's
// `catalog rebuild` uses for its one subcommand (catalog.go). `auth`
// takes no flags of its own - only create-admin does - so there is no
// flags-around-operands ordering to resolve the way catalog.go's own
// parseFlagsAroundOperands does; today there is exactly one subcommand
// (create-admin, issue #322), but the shape leaves room for a later one
// (e.g. a headless password reset) without `auth` growing flags that
// would collide across subcommands.
func cmdAuth(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+": auth requires a subcommand (create-admin)")
		return exitUsage
	}
	switch args[0] {
	case "create-admin":
		return cmdAuthCreateAdmin(args[1:])
	default:
		fmt.Fprintf(os.Stderr, cliecho.WebBinary+": unknown auth subcommand %q (only create-admin exists)\n", args[0])
		return exitUsage
	}
}

// cmdAuthCreateAdmin is issue #322's headless provisioning path: it calls
// apps/common/auth/local.CreateAdmin directly, the store-level entry
// point that writes an AdminRecord straight to --auth-store's JSON file
// (store.go) with no HTTP request, no CSRF cookie, and no dependency on
// bootstrap.go's in-memory, network-reachable bootstrap token at all -
// see CreateAdmin's own doc for exactly why that is safe (it is a
// different trust boundary than "reaching the port", not a weaker one)
// and how it stays safe if `serve` happens to be running against the
// same store at the same time (ErrStoreLocked below).
//
// The password is read only from stdin, deliberately never from a flag:
// a flag's value sits in this process's own argument list (visible to
// anyone who can run `ps` on the same host for as long as it runs) and
// often ends up in a shell history file besides. Piping it in
// (--password-stdin, matching `docker login`'s own convention) keeps it
// out of both.
func cmdAuthCreateAdmin(args []string) int {
	fset := flag.NewFlagSet("auth create-admin", flag.ContinueOnError)
	authStorePath := fset.String("auth-store", defaultAuthStorePath, "path to the local-auth administrator record")
	username := fset.String("username", "", "administrator username to create (required)")
	passwordStdin := fset.Bool("password-stdin", false, "read the administrator password from stdin (required)")
	if err := fset.Parse(args); err != nil {
		return exitUsage
	}
	if *username == "" {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+": auth create-admin: --username is required")
		return exitUsage
	}
	if !*passwordStdin {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+": auth create-admin: --password-stdin is required (this command never accepts a password as a flag); pipe it in, e.g. echo -n \"$PASS\" | "+cliecho.WebBinary+" auth create-admin --username U --password-stdin")
		return exitUsage
	}

	password, err := readPasswordFromStdin(os.Stdin)
	if err != nil {
		return fail(fmt.Errorf("auth create-admin: %w", err))
	}

	admin, err := local.CreateAdmin(local.CreateAdminConfig{
		StorePath: *authStorePath,
		Username:  *username,
		Password:  password,
	})
	if err != nil {
		if errors.Is(err, local.ErrStoreLocked) {
			return fail(fmt.Errorf("auth create-admin: %w; stop the running server (or wait for the other create-admin invocation to finish) and try again", err))
		}
		return fail(fmt.Errorf("auth create-admin: %w", err))
	}

	// errcheck's default exclusions cover a diagnostic write to
	// os.Stderr (every other message in this file), not a write to
	// os.Stdout like this one, which is this command's actual
	// machine/operator-facing output rather than a log line - so its
	// error is checked explicitly rather than silently ignored.
	if _, err := fmt.Fprintf(os.Stdout, cliecho.WebBinary+": administrator %q created in %s. Start the server normally - it will see this account already exists and will not print or accept an enrollment bootstrap token.\n",
		admin.Username, *authStorePath); err != nil {
		return fail(fmt.Errorf("auth create-admin: writing confirmation: %w", err))
	}
	return exitOK
}

// readPasswordFromStdin reads all of r and returns it as a password,
// stripping exactly one trailing newline (and a preceding carriage
// return, for a CRLF source) the way `docker login --password-stdin`
// does, so `printf '%s' "$PASS" | ...` and `echo "$PASS" | ...` both
// hand this the password an operator actually meant, not that password
// plus a stray newline character. An empty result (a closed stdin, or a
// terminal an operator forgot to pipe into) is refused rather than
// silently treated as an empty password local.CreateAdmin would then
// refuse anyway for being too short - refusing here names the actual
// mistake instead of a symptom of it.
func readPasswordFromStdin(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("reading password from stdin: %w", err)
	}
	s := strings.TrimSuffix(string(b), "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" {
		return "", fmt.Errorf("stdin was empty; pipe the administrator password in rather than a terminal (--password-stdin)")
	}
	return s, nil
}

// cmdHealthcheck is serve-ui's own HEALTHCHECK: since that container has
// no config, no state database, and no `rbm status` to run (that
// binary/subcommand belongs to the engine's own container, and checks
// REAL backup health, not "is a web server listening"), this asks the
// one question that actually applies here: does the UI host's own
// HTTP server answer at all. distroless has no shell and no curl/wget,
// so this exists specifically to give HEALTHCHECK's exec-form CMD
// something to invoke.
func cmdHealthcheck(args []string) int {
	fset := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	target := fset.String("url", localHealthcheckURL(envOrDefault("LISTEN_ADDR", defaultListenAddr)), "URL to GET")
	if err := fset.Parse(args); err != nil {
		return exitUsage
	}

	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+": healthcheck:", err)
		return exitFailure
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, cliecho.WebBinary+": healthcheck: %s returned status %d\n", *target, resp.StatusCode)
		return exitFailure
	}
	return exitOK
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

// The exit statuses this binary promises, in one place, because they are
// a contract a supervisor branches on rather than an implementation
// detail. Three of them have always been here; the fourth is issue #551,
// and it is here because container/compose.yaml runs `/rbm-web serve`,
// so the deployment shape that code was justified by (a supervisor
// replacing a container while the outgoing process has not let go of the
// serving lock yet, where waiting and trying again is the right answer)
// is THIS binary's shape rather than `rbm daemon`'s. Leaving it out would
// have published a contract that holds for the binary an operator types
// by hand and not for the one their orchestrator restarts.
//
// # Why the numbers are written twice
//
// core/cmd/backup-manager carries the same four in its own setup.go, and
// nothing imports them from here or from there. The layer rules run one
// way, so a shared home would have to be a package under core/, and a
// CLI's exit-status contract is not something the engine packages should
// be growing to hold. Two copies of four integers, each next to the
// binary that returns them and each covered by that binary's own tests,
// is the cheaper arrangement. What keeps them honest is that they are
// PUBLISHED rather than internal: the CLI prints the table in its usage
// block, and a change on either side that is not made on the other is a
// change to a documented contract.
//
// # What deliberately does not return 3
//
// `auth create-admin` refused because a server is running against the
// same --auth-store (local.ErrStoreLocked) is the closest call on the
// list, and it stays on 1. It is a different fact: that server holds the
// auth STORE, which is not "another process is already serving this
// deployment, so nothing was done", and the answer to it is to stop the
// server rather than to wait, which is the opposite of what 3 invites.
const (
	// exitOK: the command did what it was asked.
	exitOK = 0

	// exitFailure: an ordinary failure. A configuration that will not
	// load, a store that will not open, a healthcheck that got no 2xx.
	exitFailure = 1

	// exitUsage: the command line was wrong. An unknown command, an
	// unknown flag, a missing or contradictory one.
	exitUsage = 2

	// exitEngineHoldsDeployment: another process is already serving this
	// deployment, so nothing was done. The one failure here worth
	// waiting on and starting again.
	exitEngineHoldsDeployment = 3
)

// fail prints one line to stderr and returns the exit code for a command
// that ran and did not work. The prefix matters more than it looks: this
// process shares a container log with the engine and the UI host, so a
// line without it is a line an operator cannot attribute.
func fail(err error) int {
	fmt.Fprintln(os.Stderr, cliecho.WebBinary+":", err)
	return exitFailure
}

// failServing is fail for the one failure in this binary that has an exit
// status of its own: being told, at the moment this process announces it
// is about to serve, that something else already serves this deployment
// (issue #551, core/service's ErrAlreadyServing).
//
// Asked with errors.Is at the one call site that can produce it, rather
// than folded into fail. fail then keeps a single rule for every other
// failure in the binary, and a sentinel that turns up wrapped inside some
// unrelated error later cannot quietly start changing the status a
// supervisor reads. The message is untouched: what a script is meant to
// branch on is the number, which is the whole reason this exists.
func failServing(err error) int {
	if errors.Is(err, service.ErrAlreadyServing) {
		fmt.Fprintln(os.Stderr, cliecho.WebBinary+":", err)
		return exitEngineHoldsDeployment
	}
	return fail(err)
}

// gateFirstRunOnServing puts one refusal in front of the one write a
// first-run instance exposes: this deployment has to be announced before
// its first configuration is written.
//
// Everything else about the setup surface is left alone. Importing a key,
// probing a host key and testing a connection all work on a deployment
// whose state volume is broken, and refusing them would take the wizard
// away from the operator instead of telling them what to fix.
//
// The refusal is re-decided per submission rather than fixed at startup,
// which is the half that makes this a fix rather than a nicer error.
// FirstRunServing.Blocked retries the announcement, so an operator who
// remounts the volume read-write, or corrects the ownership of
// /data/state, finishes setup in the wizard already on their screen. And
// because the announcement is made in that same call, "this deployment is
// announced" and "this deployment may be configured" become one instant:
// a `backup-set create` typed on the host either finds this process or
// gets here first, and never lands in the gap #571 was reported for.
func gateFirstRunOnServing(firstRun webhost.FirstRunClient, serving *service.FirstRunServing) webhost.FirstRunClient {
	return firstRunGate{FirstRunClient: firstRun, serving: serving}
}

// firstRunGate is that refusal. It embeds the surface it guards so a
// method added to webhost.FirstRunClient reaches the real one rather than
// silently going missing here.
type firstRunGate struct {
	webhost.FirstRunClient
	serving *service.FirstRunServing
}

func (g firstRunGate) CreateInitialConfig(ctx context.Context, req service.CreateBackupSetRequest) (service.BackupSet, error) {
	if err := g.serving.Blocked(); err != nil {
		return service.BackupSet{}, err
	}
	return g.FirstRunClient.CreateInitialConfig(ctx, req)
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

// The composition root: everything this package's doc comment lays out,
// assembled into the two things a provider host actually wires up.
//
// New does three things in an order that matters. It takes the store's
// exclusive lock first, before reading or writing anything, so that a
// second server or a racing create-admin is refused rather than
// interleaved. It reads the store to find out whether anybody has enrolled
// yet. And only if nobody has does it mint a bootstrap token, which is
// what makes a restart mid-enrollment invalidate the token the previous
// run printed.
//
// The lock is never released. There is no Close on Service and that is
// deliberate rather than an omission: the process that holds it is the
// server, it holds the store open for its whole life, and the kernel
// releases the flock when it exits by any means including a kill. An
// explicit release would only add a path where the lock is dropped while
// the store is still in use.
//
// Config.TrustForwardedHeaders is the one setting here with a real blast
// radius, and its own doc carries the topology argument rather than this
// opener, because that is where somebody about to set it will be looking.
package local

import (
	"fmt"
	"io"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

// Config is everything New needs to build a Service.
type Config struct {
	// StorePath is where the administrator record is persisted (store.go).
	// Its parent directory is created on first write if missing.
	StorePath string

	// Now is a seam over time.Now for tests; nil means time.Now.
	Now func() time.Time

	// LoginRateLimit/EnrollRateLimit bound login and enrollment attempts
	// per remote IP per minute. Zero means DefaultLoginRateLimit /
	// DefaultEnrollRateLimit.
	LoginRateLimit  int
	EnrollRateLimit int

	// PasswordRateLimit bounds POST /password attempts per remote IP per
	// minute, the same per-IP rate-limit treatment /login already gets
	// (issue #128). Zero means DefaultPasswordRateLimit.
	PasswordRateLimit int

	// TrustForwardedHeaders makes this Service trust X-Forwarded-For (for
	// rate limiting, ratelimit.go's remoteIP) and X-Forwarded-Proto (for
	// the session/CSRF cookies' Secure flag, forwarded.go's
	// requestIsSecure) instead of a request's own RemoteAddr/TLS.
	//
	// # Safe ONLY behind one specific, network-isolated reverse proxy
	//
	// Both headers are ordinary request headers: anyone who can reach
	// this Service's handler directly can set either to whatever they
	// like, which would let them pick their own rate-limit bucket (an
	// attacker rotating a fake X-Forwarded-For on every request to evade
	// the limiter entirely) or falsely claim a plaintext connection is
	// HTTPS. Enable this ONLY when this Service's handler is reachable
	// exclusively through one specific reverse proxy that (a) is the sole
	// possible direct TCP peer, by network topology, not merely by
	// convention, and (b) always sets both headers itself, derived from
	// its own real connection to the actual client, never copied from
	// whatever the client sent it.
	//
	// apps/generic's two-container split (container/compose.yaml) is
	// exactly that shape: the engine (this Service, wired by `serve`) has
	// no published port and joins no network but `internal`, which only
	// `web-ui` (`serve-ui`, apps/common/webhost/serve.NewUI's reverse proxy)
	// also joins - nothing else on the host, and nothing on the LAN, can
	// ever be this Service's direct peer. apps/generic/cmd/backup-manager-web's
	// `--trust-forwarded-headers` flag is what actually turns this on for
	// that deployment; container/compose.yaml sets it for the
	// `rclone-manager` (engine) service only, never for `web-ui` itself
	// (which correctly observes its own real TLS status directly and must
	// never trust a forwarded header from just anyone hitting its
	// published port).
	//
	// Defaults to false: a Service instantiated without this set (every
	// test in this package, and any future caller that doesn't know its
	// own topology guarantees this) trusts nothing but its own directly
	// observed connection, which is always safe regardless of topology.
	TrustForwardedHeaders bool
}

// Default rate limits: generous enough that an operator mistyping a
// password a few times in a row is never the thing that trips this, tight
// enough to make an automated guessing loop impractical.
const (
	DefaultLoginRateLimit    = 10
	DefaultEnrollRateLimit   = 5
	DefaultPasswordRateLimit = 10
	rateLimitWindow          = time.Minute
)

// Service composes everything this package's doc comment describes into
// the one thing a provider host actually wires up: an Authenticator for
// apps/common/webhost's authMiddleware, and an http.Handler for the
// login/enroll/logout/session routes themselves.
type Service struct {
	store                 *Store
	lock                  *storeLock
	sessions              *sessionManager
	bootstrap             *bootstrapIssuer
	loginLimiter          *RateLimiter
	enrollLimiter         *RateLimiter
	rotateLimiter         *RateLimiter
	now                   func() time.Time
	trustForwardedHeaders bool
}

// New builds a Service from cfg. If no administrator has enrolled yet
// (Store.Admin() is nil), it issues a fresh bootstrap token immediately -
// see PrintBootstrapNotice to actually surface it to the operator - so a
// process restart before enrollment completes always invalidates
// whatever token a previous run may have printed (§49.1: "single-use and
// SHALL expire").
func New(cfg Config) (*Service, error) {
	if cfg.StorePath == "" {
		return nil, fmt.Errorf("local: Config.StorePath is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	loginLimit := cfg.LoginRateLimit
	if loginLimit == 0 {
		loginLimit = DefaultLoginRateLimit
	}
	enrollLimit := cfg.EnrollRateLimit
	if enrollLimit == 0 {
		enrollLimit = DefaultEnrollRateLimit
	}
	passwordLimit := cfg.PasswordRateLimit
	if passwordLimit == 0 {
		passwordLimit = DefaultPasswordRateLimit
	}

	// Take this store's exclusive advisory lock BEFORE reading or writing
	// anything, and hold it for this Service's entire lifetime (there is
	// deliberately no Close/release - the real, long-lived `serve`
	// process holds it until it exits, at which point the kernel releases
	// it automatically, exactly like core/service/lock_unix.go's own
	// startup/journal locks). This is what makes store.go's own
	// documented assumption - "a single process owns path" - an enforced
	// invariant rather than a convention: a second `serve` against the
	// same store, or a `create-admin` invocation (provision.go) racing a
	// live one, is refused with ErrStoreLocked instead of racing this
	// Service's own Store.Enroll/Store.SetPassword read-modify-write
	// cycle (issue #322).
	lock, err := acquireStoreLock(cfg.StorePath)
	if err != nil {
		return nil, err
	}

	store := NewStore(cfg.StorePath)
	admin, err := store.Admin()
	if err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("local: read existing administrator: %w", err)
	}

	bootstrap := newBootstrapIssuer(now)
	if admin == nil {
		if _, err := bootstrap.issue(); err != nil {
			_ = lock.release()
			return nil, fmt.Errorf("local: issue bootstrap token: %w", err)
		}
	}

	loginLimiter := NewRateLimiter(loginLimit, rateLimitWindow)
	loginLimiter.now = now
	enrollLimiter := NewRateLimiter(enrollLimit, rateLimitWindow)
	enrollLimiter.now = now
	rotateLimiter := NewRateLimiter(passwordLimit, rateLimitWindow)
	rotateLimiter.now = now

	return &Service{
		store:                 store,
		lock:                  lock,
		sessions:              newSessionManager(now),
		bootstrap:             bootstrap,
		loginLimiter:          loginLimiter,
		enrollLimiter:         enrollLimiter,
		rotateLimiter:         rotateLimiter,
		now:                   now,
		trustForwardedHeaders: cfg.TrustForwardedHeaders,
	}, nil
}

// Authenticator returns the capabilities.Authenticator apps/common/webhost's
// authMiddleware should consult for /api/v1 requests.
func (s *Service) Authenticator() capabilities.Authenticator {
	return sessionAuthenticator{sessions: s.sessions}
}

// TrustForwardedHeaders reports whether this Service was configured to
// trust X-Forwarded-For/X-Forwarded-Proto from its immediate caller (see
// Config.TrustForwardedHeaders's own doc for exactly when that is safe).
// apps/generic/cmd/backup-manager-web calls this to fill
// apps/common/webhost/serve.EngineConfig.TrustForwardedHeaders, which
// decides the same thing for the CSRF cookie NewEngine issues
// (EnsureCSRFCookie) that this Service's own session cookie already
// decides for itself internally (handler.go), so both are governed by
// the one Config value a caller actually set, instead of two
// independently-configured, driftable settings.
func (s *Service) TrustForwardedHeaders() bool {
	return s.trustForwardedHeaders
}

// NeedsEnrollment reports whether no administrator has been created yet.
func (s *Service) NeedsEnrollment() (bool, error) {
	admin, err := s.store.Admin()
	if err != nil {
		return false, err
	}
	return admin == nil, nil
}

// PrintBootstrapNotice writes the current bootstrap token's enrollment
// instructions to w, if (and only if) enrollment is still open - it does
// nothing once an administrator exists, so calling it unconditionally at
// startup is always safe. baseURL (e.g. "http://localhost:8080") is used
// to print a direct link an operator can open; pass "" to print just the
// bare token instead.
//
// §49.1's "printed to the container log on first start" is what this
// method is for: a caller (the generic host's serve command) calls it
// once, right after constructing the Service, against the process's own
// stdout - the container's own log, exactly as the spec names it.
func (s *Service) PrintBootstrapNotice(w io.Writer, baseURL string) error {
	needsEnrollment, err := s.NeedsEnrollment()
	if err != nil {
		return err
	}
	if !needsEnrollment {
		return nil
	}

	s.bootstrap.mu.Lock()
	token := ""
	if s.bootstrap.token != nil && !s.bootstrap.token.used {
		token = s.bootstrap.token.value
	}
	s.bootstrap.mu.Unlock()
	if token == "" {
		return nil
	}

	if baseURL != "" {
		_, err = fmt.Fprintf(w,
			"backup-manager: no administrator account exists yet. Open %s/enroll?token=%s to create one (valid 30 minutes, single use).\n",
			baseURL, token)
	} else {
		_, err = fmt.Fprintf(w,
			"backup-manager: no administrator account exists yet. Enrollment bootstrap token: %s (valid 30 minutes, single use).\n",
			token)
	}
	return err
}

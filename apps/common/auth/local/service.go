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
}

// Default rate limits: generous enough that an operator mistyping a
// password a few times in a row is never the thing that trips this, tight
// enough to make an automated guessing loop impractical.
const (
	DefaultLoginRateLimit  = 10
	DefaultEnrollRateLimit = 5
	rateLimitWindow        = time.Minute
)

// Service composes everything this package's doc comment describes into
// the one thing a provider host actually wires up: an Authenticator for
// apps/common/webhost's authMiddleware, and an http.Handler for the
// login/enroll/logout/session routes themselves.
type Service struct {
	store         *Store
	sessions      *sessionManager
	bootstrap     *bootstrapIssuer
	loginLimiter  *RateLimiter
	enrollLimiter *RateLimiter
	now           func() time.Time
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

	store := NewStore(cfg.StorePath)
	admin, err := store.Admin()
	if err != nil {
		return nil, fmt.Errorf("local: read existing administrator: %w", err)
	}

	bootstrap := newBootstrapIssuer(now)
	if admin == nil {
		if _, err := bootstrap.issue(); err != nil {
			return nil, fmt.Errorf("local: issue bootstrap token: %w", err)
		}
	}

	loginLimiter := NewRateLimiter(loginLimit, rateLimitWindow)
	loginLimiter.now = now
	enrollLimiter := NewRateLimiter(enrollLimit, rateLimitWindow)
	enrollLimiter.now = now

	return &Service{
		store:         store,
		sessions:      newSessionManager(now),
		bootstrap:     bootstrap,
		loginLimiter:  loginLimiter,
		enrollLimiter: enrollLimiter,
		now:           now,
	}, nil
}

// Authenticator returns the capabilities.Authenticator apps/common/webhost's
// authMiddleware should consult for /api/v1 requests.
func (s *Service) Authenticator() capabilities.Authenticator {
	return sessionAuthenticator{sessions: s.sessions}
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

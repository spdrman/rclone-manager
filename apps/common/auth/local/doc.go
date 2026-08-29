// Package local is the reusable local-account authentication fallback
// described in docs/EPIC-B-multi-nas.md §3.6/§13A: Argon2id password
// hashing, secure HTTP-only session cookies, CSRF protection, rate
// limiting, a one-time bootstrap/enrollment flow, and no plaintext
// password persistence. It is the auth mode every provider except UGOS
// uses until (or unless) it gains a native provider-auth adapter (issue
// #92/B1.3); the generic Docker app (issue #82/B4.1) is its first real
// consumer.
//
// # Layering
//
//	Service (service.go)
//	     │
//	     ├── Store (store.go) — the one persisted fact this package owns:
//	     │   an administrator's username and Argon2id password hash
//	     │   (password.go), written to a single JSON file with an atomic
//	     │   write-temp-then-rename, never a plaintext password
//	     │
//	     ├── sessionManager (session.go) — in-memory, cookie-bearer
//	     │   sessions; a process restart signs everyone out, which is an
//	     │   accepted trade-off for a single-admin, single-process
//	     │   deployment, not an oversight
//	     │
//	     ├── bootstrapIssuer (bootstrap.go) — the single-use, expiring
//	     │   secret §49.1 requires before enrollment can claim the
//	     │   administrator account, printed to the process's own stdout
//	     │   (the "container log") rather than transmitted anywhere
//	     │
//	     ├── RateLimiter (ratelimit.go) — brute-force protection on
//	     │   /login and /enroll, keyed by remote IP
//	     │
//	     ├── requireCSRF / EnsureCSRFCookie (csrf.go) — double-submit
//	     │   cookie CSRF protection on every state-changing route this
//	     │   package serves
//	     │
//	     └── sessionAuthenticator (authenticator.go) — the
//	         capabilities.Authenticator apps/common/webhost's
//	         authMiddleware consults for every /api/v1 request; it only
//	         ever reads whether the caller's session cookie is live, it
//	         never re-checks a username/password itself
//
// Service.Handler serves POST /login, POST /enroll, POST /logout and
// GET /session; a caller (apps/generic) mounts it at whatever prefix
// matches ui/shared's own expectation (ui/shared/src/api/client.ts and
// apps/generic/frontend/platform.ts assume /api/v1/auth/*), separately
// from apps/common/webhost's own /api/v1 route group, since login and
// enrollment must be reachable without an existing session — the opposite
// of webhost's authMiddleware, which this package's Authenticator feeds.
package local

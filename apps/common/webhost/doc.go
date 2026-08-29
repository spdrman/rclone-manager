// Package webhost hosts the generic (non-native) HTTP API for provider
// apps with no native platform integration — Docker/Linux, TrueNAS,
// Unraid, OpenMediaVault, Proxmox VE, and Synology until/if it gains one
// (docs/EPIC-B-multi-nas.md §3.6, §11).
//
// Issue #94 (B1.5) is what actually populates this package: a versioned
// /api/v1 that does nothing but call into core/service.BackupService, but
// does it in a way a browser disconnecting cannot cancel, a retried
// idempotency key cannot duplicate, and a stale configuration revision
// cannot silently apply against. It is deliberately a skeleton — full
// backup-set CRUD, retention routes and artifact routes are later phases
// (docs/EPIC-B-multi-nas.md §15.2, §15.5, §15.6); see this package's
// introducing PR description for exactly what was left out and why.
//
// # Layering
//
//	NewRouter (router.go)
//	     │
//	     ├── authentication middleware (auth.go), built on
//	     │   apps/common/platform/capabilities' PlatformAdapter/
//	     │   Authenticator contract — this package never authenticates
//	     │   anyone itself, it only consults whatever Authenticator the
//	     │   caller's PlatformAdapter provides
//	     │
//	     ├── the destructive-operations gate (gate.go) — fails closed by
//	     │   construction until #92 (B1.3) proves a trusted-proxy identity
//	     │   check on real hardware
//	     │
//	     └── handlers (handlers_system.go, handlers_operations.go), which
//	         talk to core/'s only public boundary through the
//	         BackupServiceClient seam (backend.go), never core/internal
//	         directly — this module cannot even do that: Go's own
//	         "internal" import rule blocks it regardless of go.work.
//
// serving ui/shared's built assets via go:embed is a later work package,
// not this one.
//
// # Idempotency-Key scope
//
// The Idempotency-Key header POST /api/v1/operations reads (see
// submitOperation in handlers_operations.go) is a GLOBAL namespace across
// this entire deployment, not scoped per actor, per route, or per backup
// set: core/internal/state's operations table enforces a bare UNIQUE
// constraint on idempotency_key alone (migrations/0003_operations.sql),
// and core/service.SubmitRunCycle checks it the same way. That is not
// obvious from either of those files' own doc comments in isolation —
// this is the one place a future route author reaching for the same
// header on a different endpoint would think to look first, so it is
// stated here explicitly: pick a key unique across every caller of the
// whole system, not just your own, and use a UUID-strength value (this
// package's own tests and core/service.SubmitRunCycle's caller both do)
// rather than anything predictable or narrow enough that an unrelated
// caller could plausibly collide with it by chance.
package webhost

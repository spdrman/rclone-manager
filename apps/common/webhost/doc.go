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
package webhost

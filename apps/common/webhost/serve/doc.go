// Package serve composes apps/common/webhost's /api/v1 router (and, where
// a provider supplies one, its own local/native authentication routes)
// into the reusable "Web host" shape docs/EPIC-B-multi-nas.md §9.2
// requires: an engine handler, a UI-host handler, and the §9.3 "HTTP
// server + background scheduler share one process shutdown context"
// orchestration that runs the former alongside a BackupService.
//
// Issue #129: this package is what PR #119 should have put this
// composition in the first place, per §9.2's own words ("The reusable Web
// host under apps/common/webhost SHALL compose..."). PR #119 built it
// inside apps/generic instead (apps/generic/server.go's NewEngine/NewUI,
// and apps/generic/cmd/backup-manager-web's cmdServe orchestration) -
// functionally correct, but unusable by any other provider without
// duplicating it wholesale. This package is that composition, moved here
// and decoupled from any one provider: NewEngine takes a
// capabilities.PlatformAdapter and an optional auth-routes http.Handler
// rather than a concrete apps/common/auth/local.Service, the same way
// apps/common/webhost.NewRouter is already parameterized.
//
// apps/generic/cmd/backup-manager-web is this package's first caller: it
// builds a local.Service and a platform.Adapter (both still genuinely
// generic-provider-specific - every future provider builds its own) and
// hands them to NewEngine/NewUI/RunEngine here. A future TrueNAS/Synology
// app (#84/#85/#86) that also wants the local-auth fallback (§13A) does
// the same thing with its own PlatformAdapter.
//
// # Layering
//
//	RunEngine (run.go)
//	     │ drives the HTTP server built from NewEngine's handler and,
//	     │ optionally, a Scheduler, sharing one context (§9.3)
//	     ▼
//	NewEngine (engine.go)                    NewUI (ui.go)
//	     │                                        │
//	     ├── AuthRoutes, if set, mounted at        ├── reverse-proxies
//	     │   /api/v1/auth/                         │   /api/v1/* and
//	     │                                          │   /health/* to an
//	     └── apps/common/webhost.NewRouter          │   upstream engine
//	         for everything else under              │
//	         /api/v1 and /health                    └── serves a caller-
//	                                                     supplied static
//	                                                     UI bundle for
//	                                                     everything else
//
// Both NewEngine and NewUI wrap their mux in
// apps/common/auth/local.EnsureCSRFCookie - see that function's own doc
// for exactly why NewEngine may pass true (only when its own AuthRoutes
// backend was configured to trust its one verified reverse-proxy peer)
// and NewUI always passes false (it is the actual internet-facing edge
// and must never trust a forwarded header from just anyone hitting its
// published port).
package serve

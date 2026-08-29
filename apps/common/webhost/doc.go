// Package webhost will host the generic (non-native) web UI and HTTP API
// for provider apps with no native platform integration — Docker/Linux,
// TrueNAS, Unraid, OpenMediaVault, Proxmox VE, and Synology until/if it
// gains one (docs/EPIC-B-multi-nas.md §3.6, §11).
//
// This package intentionally contains no handlers yet. The real
// versioned /api/v1 surface, serving ui/shared's built assets via
// go:embed and routing through core's Service/RunCycle, is issue #94
// (B1.5) — a separate work package that depends on the core/ boundary
// this one (#106 / B1.1) establishes. This file exists only to reserve
// the location apps/common/webhost/ that §7's target repository
// structure calls for, so B1.5 has a home to land in rather than
// inventing its own layout.
package webhost

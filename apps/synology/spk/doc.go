// Package spk builds and conformance-checks the Synology DSM `.spk`
// package for this project (issue #85/B4.4, docs/EPIC-B-multi-nas.md §72
// Work Package 4.4, §4A's Synology entry, and D-5 in §5).
//
// # What this package is for
//
// Synology is the one provider in Phase 4 that cannot consume the
// canonical OCI image: DSM's Package Center installs a native `.spk`.
// §3.7's release-artifact hierarchy is explicit that the SPK is a sibling
// of the OCI image under the same root, carrying "the exact same core
// binary digest" — so this package's whole job is to wrap the ALREADY
// BUILT release binaries, and to make "those are the release binaries" a
// checkable claim rather than an assertion.
//
// It therefore builds nothing from source. Build takes a directory of
// binaries someone else produced (4.1's canonical image is where they
// come from) and Verify re-derives their SHA-256 out of the finished
// package and compares it against container/release-manifest.json.
//
// # Why the package is assembled here rather than by the Synology toolkit
//
// pkgscripts-ng exists to cross-compile source inside a DSM chroot and
// then tar the result. This project needs only the second half: the core
// binaries are CGO-free static Go builds cross-compiled by the canonical
// release, so there is nothing for a DSM toolchain to compile. What
// pkg_make_spk itself does is documented and small — `tar cf` of a
// staging directory whose payload is a compressed `package.tgz` — and
// reimplementing exactly that in Go buys three things the shell version
// cannot: it runs on any machine with a Go toolchain (no Debian 12 host,
// no chroot), it is byte-for-byte deterministic, and it is testable.
//
// Two deliberate differences from pkg_util.sh, both visible in the output:
//
//   - package.tgz is gzip-compressed, not xz. The toolkit's current
//     pkg_get_tar_option returns "cJf"; DSM extracts the payload with
//     compression auto-detection and the historical `.tgz` name is gzip,
//     which SynoCommunity packages have shipped for years. gzip keeps
//     both this builder and its verifier on the Go standard library, so
//     the conformance check has no dependency of its own to trust.
//   - INFO gets no `toolkit_version` or `create_time` line. Both are
//     stamped by pkg_make_spk and neither is a documented INFO field;
//     create_time in particular is a timestamp, which is precisely what
//     a reproducible artifact must not contain.
//
// # Scope boundary
//
// Nothing Synology-specific belongs in core/ or ui/shared/, and nothing
// here reaches into either: this module's go.mod has no dependency at all
// (see its own comment). Authentication is the reusable local-auth from
// the generic Web host, run by the same unmodified binaries the generic
// Docker app runs. Native DSM SSO and session integration are explicitly
// a follow-on behind their own security gate (§4A, §72 WP 4.4) and no
// code here anticipates them.
package spk

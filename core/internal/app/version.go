package app

import (
	"runtime"
	"runtime/debug"
)

// Answering "which rclone is this" without importing rclone.
//
// FR-3 draws a containment boundary around internal/transport/rclone, and
// `version` is the one command whose whole output is a fact about what is on
// the other side of it. The obvious implementations both cost something real:
// calling fs.Version puts an rclone import in the application layer, and
// exporting an accessor from the adapter puts a version-reporting API on a
// transport boundary that has no other reason to have one.
//
// Reading it back out of the binary's own build info costs neither, and it
// is not a trick: go.mod pins the version, the toolchain stamps the pin into
// every binary, and this reads back exactly what was compiled rather than
// what a constant somewhere claims. BuildVersionInfo's doc carries the
// verification against the production build flags, which is the part worth
// re-reading before anyone assumes -trimpath or -ldflags "-s -w" strips it.
//
// The consequence is that this file is the only one in the package that
// imports runtime/debug, and it should stay that way. A second reader of
// build info would be a second answer to a question that has one.

// rcloneModulePath is github.com/rclone/rclone's module path, exactly as
// it appears in go.mod and therefore exactly as it appears in this
// binary's embedded build info.
const rcloneModulePath = "github.com/rclone/rclone"

// VersionInfo is FR-26's `version` command payload: the backup-manager
// version, the embedded rclone version, the Go version and the build
// commit.
type VersionInfo struct {
	BinaryVersion string
	Commit        string
	GoVersion     string
	RcloneVersion string
}

// BuildVersionInfo assembles a VersionInfo. binaryVersion and commit are
// normally the values cmd/backup-manager's main.go sets via -ldflags
// (default "dev" / "none" in a non-release build, exactly like the
// `version` subcommand that already existed before this package).
//
// # Why this never imports rclone
//
// The EPIC's "Repository Layout" section is explicit: "Application
// packages outside internal/transport/rclone SHOULD NOT directly import
// rclone packages" (FR-3's containment boundary). Reporting "the embedded
// rclone version" without breaking that boundary, and without adding an
// exported accessor to internal/transport/rclone (out of this package's
// file scope; see this package's introducing PR description for the exact
// follow-up), means this package cannot call into rclone's fs.Version
// directly or through a wrapper.
//
// runtime/debug.ReadBuildInfo gives an equivalent answer without either
// problem: since go.mod pins github.com/rclone/rclone to an exact version
// (FR-2 requires this already), the Go toolchain embeds that pinned
// version into every binary's build info at compile time, and
// ReadBuildInfo reads it back at runtime without this package, or the
// binary as a whole, ever importing a single rclone package directly. This
// was verified against the exact build recipe container/Dockerfile uses
// (-trimpath -buildvcs=false -ldflags "-s -w ..."): none of those flags
// strip build info, only source paths, VCS stamping and the symbol
// table/DWARF data respectively, so this reports the correct version in
// the production container image as well as in `go run`/`go test`.
//
// A binary built with `go build` (not `go run`, which reports the main
// module's own version as "(devel)" but leaves every dependency's pinned
// version intact) or when build info is genuinely unavailable reports
// "unknown" rather than a blank string, so a log line or a `version`
// command's output always has a token to look at.
func BuildVersionInfo(binaryVersion, commit string) VersionInfo {
	return VersionInfo{
		BinaryVersion: binaryVersion,
		Commit:        commit,
		GoVersion:     runtime.Version(),
		RcloneVersion: embeddedRcloneVersion(),
	}
}

func embeddedRcloneVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path != rcloneModulePath {
			continue
		}
		// A `replace` directive (go.mod has none today, but a future one
		// might, e.g. during a local rclone upgrade spike) means the
		// actually-compiled code came from Replace, not Version; prefer it
		// when present so this never reports a version that is not what
		// was actually built.
		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version
		}
		if dep.Version != "" {
			return dep.Version
		}
	}
	return "unknown"
}

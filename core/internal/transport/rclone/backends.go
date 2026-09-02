package rclone

import (
	"sort"

	"github.com/rclone/rclone/fs"
)

// This file is the single source of truth for which rclone backends this
// binary is allowed to register (FR-4, docs/EPIC.md). backends_test.go
// checks it against the live fs.Registry, so a new blank import anywhere in
// this package that widens the registered set fails the build instead of
// shipping unnoticed. Widening this list on purpose is the "explicit
// feature/architecture decision" FR-4 asks for.

// RequiredBackends are the backends this product actually asks for. Each
// one has a direct blank import in adapter.go.
//
// s3 is EPIC E's addition (FR-28), and it is the FR-4 "explicit
// feature/architecture decision" that FR asks for, recorded here rather
// than left as an import line somebody has to notice. It is the whole S3
// implementation: no AWS SDK is imported by any file in this repository,
// in Go or in TypeScript, and the transitive one rclone's own backend
// carries is rclone's dependency, upgraded when rclone is.
//
// # What it cost
//
// Measured the way the crypt precedent was measured, on a linux/arm64,
// CGO_ENABLED=0 build of core/cmd/backup-manager, with this one blank
// import as the only difference between the two builds:
//
//	without backend/s3   29,646,334 bytes
//	with backend/s3      42,664,008 bytes
//	delta                13,017,674 bytes, 12.4 MiB, +43.9%
//
// That is a big number, twenty-seven times crypt's ~470KB, and it is worth
// saying plainly rather than burying: registering s3 pulls in the AWS SDK
// for Go v2 (its S3 service client, signer, and the smithy runtime under
// it), which is most of the delta. It buys the only non-local storage
// medium this product has.
//
// The alternative was not "a smaller build", it was an AWS SDK dependency
// of this repository's own, which costs the same binary space, adds a
// second HTTP, credential and retry stack beside the one already embedded,
// and puts a provider SDK inside the FR-3 boundary that exists to keep
// upstream churn in exactly one adapter. What would genuinely shrink it is
// not shipping S3 at all, and that is the decision FR-28 made the other
// way, out loud, with this measurement attached.
var RequiredBackends = []string{"local", "s3", "sftp"}

// AcceptedTransitiveBackends maps the name of a backend that registers
// itself even though nothing in this package imports it directly, to the
// reason it's accepted rather than eliminated. Every entry needs a real,
// non-empty reason: TestAcceptedTransitiveBackendsAreDocumented fails on an
// empty one, so a future addition can't slip in without someone writing
// down why.
var AcceptedTransitiveBackends = map[string]string{
	"crypt": `registered by fs/operations/lsjson.go, which imports
backend/crypt directly to decrypt file names for rclone's ListJSON
--show-encrypted option. That import carries no build tag, so anything
that imports fs/operations registers crypt, whether or not it ever asks
for a crypt remote.

fs/operations is a real, needed dependency here: adapter.go calls
operations.Copy from CopyToLocal. Tracing it: importing only
backend/local and backend/sftp (no fs/operations) registers exactly
"local" and "sftp" and nothing else, so local and sftp are not the
cause; fs/operations is, and only through that one file.

Eliminating it needs one of:
  - reimplementing the transfer in CopyToLocal over lower-level
    fs.Object/fs.Fs primitives instead of operations.Copy, which is a
    change to fsFor/CopyToLocal. That function is out of scope for this
    file set (issue #6 owns it).
  - vendoring a patched fs/operations with lsjson.go's crypt import
    removed. That is exactly the "permanent local patch to rclone" the
    rclone Upgrade Compatibility Contract (docs/EPIC.md) says needs an
    Architecture Decision Record, since it creates fork-like maintenance
    obligations the contract exists to avoid.

Neither is done here, so crypt is accepted as a measured residual cost
rather than a silent pass: it does add one nameable remote type
("crypt") to the configuration surface FR-4 wants to keep narrow. Measured
against a 21MB linux/arm64, CGO_ENABLED=0 build: crypt's own code
(backend/crypt, backend/crypt/pkcs7, and the two libraries only it pulls
in, Max-Sum/base32768 and rfjakob/eme) adds about 470KB, roughly 2% of
the binary... see the PR description for the full measurement and how
it was taken.`,
}

// ExpectedBackends is RequiredBackends plus the keys of
// AcceptedTransitiveBackends, sorted. It is the exact set
// backends_test.go checks fs.Registry against.
func ExpectedBackends() []string {
	all := make([]string, 0, len(RequiredBackends)+len(AcceptedTransitiveBackends))
	all = append(all, RequiredBackends...)
	for name := range AcceptedTransitiveBackends {
		all = append(all, name)
	}
	sort.Strings(all)
	return all
}

// RegisteredBackendNames returns the names of every rclone backend actually
// registered in this binary, sorted.
func RegisteredBackendNames() []string {
	names := make([]string, 0, len(fs.Registry))
	for _, r := range fs.Registry {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

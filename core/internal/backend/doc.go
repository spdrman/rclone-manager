// Package backend declares which storage backends this engine knows how to
// dial, as data, and reads that data at startup (issue #665, EPIC I / #664
// I1.2).
//
// A Manifest is one registered backend: its id, its label, the fields an
// operator fills in to configure an instance of it, which rclone backend
// carries it, and which of mediumcheck's eight probe steps apply to it.
// Registry.Bundled loads the three manifests this build ships
// (bundled/local_volume.json, bundled/s3.json, bundled/sftp.json) from
// files embedded at build time. Nothing reads a manifest from disk, from
// a request body or from a database: see "Bundled only" below.
//
// Nothing here changes observable product behaviour yet. It adds a
// package nothing calls except at startup, to refuse a malformed
// manifest loudly, the way config.StorageMedium itself landed ahead of
// anything that acted on it ("Nothing reads this yet",
// core/internal/config/config.go). #666 (a local-volume destination) and
// #667 (moving S3's own validation onto this registry) are the first
// real callers.
//
// # Why this package imports only the standard library
//
// #667 needs core/internal/config.Validate to ASK this registry whether
// an instance's fields are legal, so config will import backend. And
// core/internal/archive already imports config, and core/internal/
// mediumcheck imports archive, placement and transport. Any edge from
// backend to config, archive, mediumcheck or transport closes a cycle the
// moment #667 lands. So this package may import config, archive,
// mediumcheck, transport and rclone from NOWHERE, and TestThisPackageCannotOpenAFile
// enforces the sharper, structural half of that: it scans this package's
// own non-test source files and refuses an "os" import outright, which is
// the package any of those four would eventually have to go through to
// read a manifest off disk.
//
// The cost of the split is three small facts this package would
// otherwise ask config, transport/rclone and mediumcheck for, and cannot,
// copied here instead and pinned back against their authority by a test
// in each of those three packages' own external test package (the only
// package that may import both sides without creating the cycle this
// split exists to avoid) - the same arrangement
// config.archiveStorageClasses already uses and defends at length:
//
//   - ReservedInstanceID copies config.MediumLocal, pinned by
//     TestTheReservedInstanceIdMatchesConfigsOwn in core/internal/config's
//     external test package.
//   - SupportedRcloneBackends copies (a deliberately NARROWER subset of)
//     the rclone backends this binary registers, pinned by
//     TestEveryBundledManifestNamesABackendThisBinaryRegisters in
//     core/internal/transport/rclone's external test package.
//   - ProbeStepNames copies mediumcheck.Steps, pinned in both directions
//     by TestTheProbeStepVocabularyMatchesMediumcheckSteps in
//     core/internal/mediumcheck's external test package.
//
// # Bundled only, and there is no code that could become otherwise
//
// #665's scope line is explicit: "Bundled manifests only. The
// upload-and-register path is deliberately not here, so this issue does
// not need a signing or trust story. Getting there without one is the
// failure mode to avoid." So the guarantee is structural rather than
// promised. Bundled reads bundledFS, a //go:embed embed.FS, and nothing
// else. Load is exported ONLY so a test can plant a malformed manifest in
// an fstest.MapFS without shipping one in bundled/; its own docblock
// says a production caller passing anything but bundledFS is a review
// failure, and this package importing no filesystem package at all - no
// "os", no "net/http" - means there is no reachable code path, today,
// that produces a manifest this build did not ship. The future
// upload-and-register path #664 defers is where a signing and trust
// story is owed; #665's job is to not make that path accidentally
// reachable.
//
// # A malformed bundled manifest fails the whole registry
//
// Load walks every *.json in the given FS, in sorted name order,
// accumulates every problem across every file (the way config.validator
// accumulates), and on ANY problem returns (nil, err) rather than the
// well-formed manifests with a warning about the bad one. A partially
// loaded registry is a deployment that silently lost a backend, and the
// instances configured against it turn into "unknown backend" at the
// moment somebody's backup needs that destination - the
// protection-dies-quietly failure this product exists to prevent. This
// is scoped to BUNDLED manifests deliberately: the future upload path
// will need the other answer, because one bad uploaded manifest bricking
// an otherwise-working deployment is a different and worse failure, and
// Load's own docblock says so.
//
// # How the closed set stays closed
//
// MediumType's doctrine (core/internal/transport/medium.go) is that the
// set of backends "grows only by an FR-4 architecture decision, never by
// an import line". That is still true after #665. The cost of adding a
// backend is, in order: a JSON file under bundled/; its rclone_backend
// present in SupportedRcloneBackends; that name present in
// rclone.RequiredBackends with a blank import in adapter.go and a
// measured binary-size delta (FR-4's existing toll, unchanged); and a
// change to TestTheBundledSetIsExactly, which enumerates the shipped
// ids. All four are reviewed diffs.
//
// Three further guarantees carry the doctrine forward:
//
//   - A manifest can declare a new BACKEND. It cannot declare a new ROLE.
//     Role is a closed Go enum with three members (RoleObjectStore,
//     RoleLocalVolume, RoleRemoteFilesystem); adding a fourth is a Go
//     change with a compiler-enforced blast radius, exactly what
//     MediumType was. #731 added the third one and paid that toll: a
//     role internal/app's mediumType does not dispatch on is a refusal
//     at the moment something is about to be reached, which is where
//     sftp-as-a-destination stands until an adapter dials it.
//   - config.MediumLocal ("local") stays exactly one answer for where
//     local artifacts live. No instance of any backend may claim it
//     (core/internal/config/validate.go), and after #665 no *manifest*
//     may claim it either (TestAManifestCannotClaimTheReservedLocalId).
//     A local_volume instance is a DECLARED destination with its own id
//     and its own path, not a second spelling of "the drive backups
//     already land on".
//   - FR-32 survives because there is nowhere to put a violation.
//     FieldKind has no duration kind, no timestamp kind and no count
//     kind (TestFieldKindsAreExactly), so a manifest cannot declare a
//     field whose value could become an input to placement or pruning,
//     and TestRetentionDoesNotImportThisPackage asserts
//     core/internal/retention never imports this package and never
//     will.
//
// # What was genuinely weakened, on the record (issue #665, section 4.2; issue #731)
//
// sftp is already in rclone.RequiredBackends, because a backup SOURCE is
// reached over it. Once SupportedRcloneBackends exists, shipping SFTP as
// a DESTINATION costs a manifest file and an entry in a map, with no
// blank import and no binary-size measurement, because the import is
// already there for a different reason. FR-4's toll was never designed
// to distinguish "registered to read from" from "registered to write
// backups to", and after #665 those two are one decision for any backend
// already required as a source.
//
// #665 left SupportedRcloneBackends at {local, s3} and named that gap
// rather than closing it. #731 walked through it on purpose: the set is
// now {local, s3, sftp} and bundled/sftp.json is the third shipped
// manifest, so an operator can declare a destination on another machine
// reached over SSH. What the toll bought instead of a binary-size
// measurement is written down in
// docs/adr/0005-destination-backend-decision.md, which is the ADR this
// paragraph used to ask a future reviewer for.
//
// SupportedRcloneBackends is still NOT rclone.RequiredBackends, and the
// distinction is now about what it MEANS rather than about which names
// are in it: it is the list of backends a DESTINATION may be declared
// on, checked as a SUBSET of what the binary registers (not equal to it)
// by TestEveryBundledManifestNamesABackendThisBinaryRegisters, and
// pinned name for name by TestSupportedRcloneBackendsIsItsOwnReviewedList
// so a fourth one fails a named test. That is a review gate, not a
// structural one, and it is the honest limit of what this package
// achieves: the structural gate is the compiler for Role and fs.Find for
// an unregistered rclone backend name; between them sits one map literal
// whose only defence is that changing it shows up in a diff and fails a
// named test.
//
// # Credentials: a reference, never material, and nothing new is built
//
// A Field of KindCredential marks which field of a manifest collects a
// credential reference rather than a plain value; it is its own FieldKind
// rather than a boolean flag on a string field, deliberately: a boolean
// is a flag a code path can forget to read, and a credential field that
// is a KindString when nobody checks the flag is exactly the shape that
// leaks. As a kind, there is no code path in which a credential field is
// a string.
//
// The machinery that carries a credential reference into a working
// backend already exists and #665 does not touch it: collection through
// core/service.BackupService.ImportStorageCredentials, which writes a
// 0600 AWS shared-credentials file into "<config dir>/s3_credentials/"
// (dir 0700, named by a bare uuid) and has no read side at all; the
// opaque MediumCredentialRef that names it; resolveSpecCredentials
// (core/service/mediums.go) turning the reference into
// config.MediumCredentials{File: path}; and config.yaml holding that
// PATH, never material, because Load's KnownFields(true) makes an inline
// secret a parse error. "Under the deployment key" in #665's own prose
// names that same directory - the deployment's own private state
// directory, mounted and backed up beside config.yaml and never under
// the backup root (#298, the reason config.MediumCredentials.File's own
// doc gives). There is no encryption-at-rest layer in this tree and #665
// does not invent one; the custody model is filesystem modes plus "there
// is no read side", the same model ImportSSHKey already uses.
//
// The one rule #665 adds, and TestTheRegistryHasNowhereForCredentialMaterial
// enforces by a reflect walk: no type in this package can hold credential
// material, at any point, in any field. A KindCredential field's value in
// an instance's values map is a reference id, checked by ValidateInstance
// for shape (no path separator) and never for content. Manifest, Field,
// Probe and Registry have no []byte, no obs.Secret-shaped field, and no
// function here takes material as an argument. This is
// transport.Medium's "the struct has nowhere for a value to hide"
// (medium_test.go) applied one level up.
//
// # ValidateInstance ships with no production caller
//
// ValidateInstance checks one instance's collected values against the
// manifest that declares them - whatever a field can prove on its own,
// without a network call: a path is absolute and clean, a URL is http(s)
// with no userinfo, an enum value is one of the declared choices. It has
// a full test suite and no caller in #665, for the reason
// config.StorageMedium's own "Nothing reads this yet" states: #667 makes
// config.Validate the first caller, once it can delegate an s3 instance's
// field rules here without rewording the messages an operator already
// reads.
package backend

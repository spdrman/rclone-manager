package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// This file is issue #792's half of the EPIC #779 Phase 0 contract: what a
// backend can be asked to DO, declared as data beside the fields an
// operator fills in and the probe steps that would verify one instance.
//
// It is a third kind of statement about a backend and the three are not
// interchangeable. Fields are what a person types. Probe is what a
// connection test proves about one configured instance. Capabilities are
// what the BACKEND is - facts about the protocol and the code that speaks
// it, identical for every instance, and not discoverable from any
// configuration an operator could supply.
//
// # Why silence is a refusal and not a default
//
// Manifest.Configurable defaults to true, and its doc explains why: the
// ordinary manifest is one somebody is shipping, so the answer that costs
// nothing to say has to be the ordinary one. Capabilities are the exact
// opposite case, and the asymmetry is deliberate. There is no ordinary
// answer to "can a directory on this backend be read without holding all
// of it in memory" - it is true of an object store, false of sftp, and
// true of a local disk only because THIS process reads one in chunks. A
// default would therefore be a guess, and the optimistic guess is an
// out-of-memory kill in a daemon whose job is to still be running
// tomorrow, arriving on the first directory that got big and never in a
// test. So a manifest that says nothing about capabilities describes a
// backend this engine refuses to enumerate, by name, at the moment
// somebody asks it to (PlanEnumeration below), and the zero Capabilities
// value is the least capable backend describable rather than a blank one.
//
// # Why the block is all twelve keys or none
//
// The same argument one key at a time. A manifest that answers eleven
// keys is a manifest whose twelfth answer a consumer will infer, and
// validation
// refuses it naming the key that is missing (validateManifestCapabilities).
// The cost is that adding a key to the vocabulary is a diff that touches
// every bundled manifest, which is the intended toll: a capability
// nothing declares is a capability every consumer has to handle as
// unknown anyway.

// Capability is one key of the matrix. The set is closed in Go, the same
// way Role and FieldKind are, and it is a cross-issue contract inside
// EPIC #779: #793 classifies how far a source's metadata may be trusted
// from mtime_precision, hash_support, stable_size and metadata_support.
type Capability string

const (
	// CapBoundedListing is the key this issue exists for: can one
	// directory on this backend be enumerated in memory proportional to
	// a configured buffer rather than to the number of entries in it?
	//
	// It is a claim about the whole path from the protocol up to this
	// process, not about the protocol alone. local is true because
	// *os.File.ReadDir(n) takes a count and this repository has an
	// enumerator that uses it (core/internal/transport.LocalEnumerator).
	// sftp is false because rclone's sftp backend reads a directory
	// through github.com/pkg/sftp's ReadDir, which returns the whole
	// directory as one slice and hands back no cursor a caller could
	// resume from - so no code above it can bound the peak, and pretending
	// otherwise would put the refusal after the memory was already spent.
	CapBoundedListing Capability = "bounded_listing"

	// CapRecursiveListing is whether the backend has a NATIVE recursive
	// listing - one request that returns a subtree - which is exactly
	// rclone's fs.Features().ListR. It is false for local and sftp, where
	// a tree costs one directory read per directory, and true for s3,
	// where a prefix scan returns everything under it.
	//
	// It is not "can this engine recurse": it always can, by walking.
	// It is what makes the difference between one round trip and one per
	// directory, which is the cost issue #737 measured at 1.6k
	// directories over sftp.
	CapRecursiveListing Capability = "recursive_listing"

	// CapStreamingOpen is whether an object can be read as a stream
	// rather than fetched whole before anything can consume it.
	CapStreamingOpen Capability = "streaming_open"

	// CapRangeOpen is whether a byte range of an object can be requested,
	// which is what makes a resumed or partial read possible.
	CapRangeOpen Capability = "range_open"

	// CapMTimePrecision is the finest modification time this backend
	// PRESERVES. Note that transport.RemoteArtifact carries unix seconds
	// regardless (see transport/rclone's toArtifact), so a backend
	// declaring 1ns is currently truncated on the way in; the matrix
	// describes the backend, and the truncation is this engine's own and
	// is documented in docs/adr/0008.
	CapMTimePrecision Capability = "mtime_precision"

	// CapHashSupport names the checksums this backend can produce AT ALL.
	// It says nothing about what producing one costs, and the difference
	// matters enough to be stated here rather than discovered by a
	// consumer: local's three are computed by reading the whole file,
	// while s3's md5 is a validator the store already holds. So this key
	// answers "could a hash be obtained", not "is a hash cheaper than a
	// read" - #793's own narrower list (model.SourceSignals'
	// RemoteHashAlgorithms) is the second question, and ADR 0009 section
	// 3 records why it is deliberately narrower than this one rather
	// than a copy of it.
	//
	// Empty is a legitimate answer and is sftp's: rclone probes the far
	// host for an md5sum binary, so whether a hash is available is a
	// property of that host's PATH rather than of the protocol, and a
	// matrix may not claim what depends on somebody else's shell.
	CapHashSupport Capability = "hash_support"

	// CapStableSize is whether the size the backend reports for an object
	// describes the bytes a read of that object will return. It says
	// nothing about a file somebody is still writing to, which is a
	// source-consistency question and belongs to #793.
	CapStableSize Capability = "stable_size"

	// CapSymlinkSemantics is what a symbolic link IS on this backend to
	// this engine: stored as a link, followed to its target, skipped, or
	// not representable at all.
	CapSymlinkSemantics Capability = "symlink_semantics"

	// CapMetadataSupport is how much of an entry's metadata - ownership,
	// mode, times - survives a round trip through this backend.
	CapMetadataSupport Capability = "metadata_support"

	// CapCaseSensitivity is whether two names differing only in case are
	// two objects. "unknown" is the honest answer for a local volume and
	// for sftp, because the answer belongs to whichever filesystem is
	// mounted at the path an operator configured - APFS, exFAT on a USB
	// disk, ext4 - and this process cannot know which without probing
	// that specific path.
	//
	// It says nothing about what the backend STORES, which is
	// case_preservation below and a genuinely separate axis: a
	// case-insensitive filesystem usually keeps the name as it was
	// written, and one that does not (an 8.3 FAT volume) loses it.
	// Collapsing the two - as a "preserving" value inside this key would
	// - makes a restore's name fidelity unanswerable without knowing
	// which of the two meanings was meant.
	CapCaseSensitivity Capability = "case_sensitivity"

	// CapCasePreservation is whether a name survives a round trip with
	// the case it was written in. A restore is where this is noticed:
	// "Invoices" written back as "invoices" is a correct restore of the
	// wrong name, and nothing in a listing comparison would flag it.
	CapCasePreservation Capability = "case_preservation"

	// CapGenerationIdentity is whether this backend hands back an
	// identity for the CONTENT of an object, as opposed to the stable
	// slot the content sits in.
	//
	// A path is a slot id: it survives an overwrite, so two different
	// bytes have the same one and nothing about it says the object
	// A generation or an S3 versionId is a content
	// identity: it changes when the bytes change, which is what lets a
	// consumer decide "unchanged" without reading the object. EPIC
	// #779's trust classification (#793/#824) reads this key and nothing
	// else for that question, which is the point of putting it here:
	// one authority instead of a table per consumer.
	CapGenerationIdentity Capability = "generation_identity"
)

// capabilityKeys is the vocabulary, in declaration order.
//
// It is unexported and reached through CapabilityKeys() because a
// package-level exported slice is writable by anyone who imports the
// package: one line in a caller - or in a test of a caller - reorders or
// truncates the vocabulary for every consumer in the process, and the
// failure lands in validation, which is the code that is supposed to be
// the authority on it.
var capabilityKeys = []Capability{
	CapBoundedListing,
	CapRecursiveListing,
	CapStreamingOpen,
	CapRangeOpen,
	CapMTimePrecision,
	CapHashSupport,
	CapStableSize,
	CapSymlinkSemantics,
	CapMetadataSupport,
	CapCaseSensitivity,
	CapCasePreservation,
	CapGenerationIdentity,
}

// CapabilityKeys returns the vocabulary, in declaration order, as a copy
// the caller owns. Pinned name for name by TestCapabilityKeysAreExactly,
// because the key strings are an EPIC #779 Phase 0 contract other issues
// consume.
func CapabilityKeys() []Capability { return slices.Clone(capabilityKeys) }

// MTimePrecision is the finest modification time a backend preserves.
type MTimePrecision string

const (
	MTimeNanosecond  MTimePrecision = "1ns"
	MTimeMillisecond MTimePrecision = "1ms"
	MTimeSecond      MTimePrecision = "1s"
	MTimeTwoSeconds  MTimePrecision = "2s" // FAT, and it is not hypothetical on a USB disk
	MTimeUnknown     MTimePrecision = "unknown"
)

var validMTimePrecisions = map[MTimePrecision]bool{
	MTimeNanosecond: true, MTimeMillisecond: true, MTimeSecond: true,
	MTimeTwoSeconds: true, MTimeUnknown: true,
}

// SymlinkSemantics is what a symbolic link is on a backend.
type SymlinkSemantics string

const (
	SymlinksStored      SymlinkSemantics = "store"
	SymlinksFollowed    SymlinkSemantics = "follow"
	SymlinksSkipped     SymlinkSemantics = "skip"
	SymlinksUnsupported SymlinkSemantics = "unsupported"
)

var validSymlinkSemantics = map[SymlinkSemantics]bool{
	SymlinksStored: true, SymlinksFollowed: true,
	SymlinksSkipped: true, SymlinksUnsupported: true,
}

// MetadataSupport is how much of an entry's metadata survives.
type MetadataSupport string

const (
	MetadataFull    MetadataSupport = "full"
	MetadataPartial MetadataSupport = "partial"
	MetadataNone    MetadataSupport = "none"
)

var validMetadataSupport = map[MetadataSupport]bool{
	MetadataFull: true, MetadataPartial: true, MetadataNone: true,
}

// CaseSensitivity is whether two names differing only in case are two
// objects. It is one axis; CasePreservation is the other.
type CaseSensitivity string

const (
	CaseSensitive   CaseSensitivity = "sensitive"
	CaseInsensitive CaseSensitivity = "insensitive"
	CaseUnknown     CaseSensitivity = "unknown"
)

var validCaseSensitivity = map[CaseSensitivity]bool{
	CaseSensitive: true, CaseInsensitive: true, CaseUnknown: true,
}

// CasePreservation is whether a stored name keeps the case it was
// written in, which is a different question from whether two cases are
// two names.
type CasePreservation string

const (
	CasePreserved CasePreservation = "preserved"
	// CaseNormalized is a backend that rewrites the name - an 8.3 FAT
	// volume uppercasing it is the one that still exists on hardware
	// this product runs against.
	CaseNormalized          CasePreservation = "normalized"
	CasePreservationUnknown CasePreservation = "unknown"
)

var validCasePreservation = map[CasePreservation]bool{
	CasePreserved: true, CaseNormalized: true, CasePreservationUnknown: true,
}

// GenerationIdentity is what identity a backend gives an object's
// CONTENT, as opposed to its path.
type GenerationIdentity string

const (
	// GenerationVersioned is a per-write identifier the backend assigns
	// and returns beside the object: S3's versionId, GCS's generation.
	// It changes on every overwrite. Whether PRIOR versions are retained
	// is a bucket setting and therefore an instance's configuration, so
	// this key answers identity and not retention.
	GenerationVersioned GenerationIdentity = "versioned"

	// GenerationNone is a backend offering nothing but the path. An
	// overwrite that preserves size and mtime is invisible, which is
	// exactly why a stable slot id must not be reported here as an
	// identity.
	GenerationNone GenerationIdentity = "none"

	GenerationUnknown GenerationIdentity = "unknown"
)

var validGenerationIdentity = map[GenerationIdentity]bool{
	GenerationVersioned: true,
	GenerationNone:      true, GenerationUnknown: true,
}

// HashAlgorithm is one checksum a backend can be asked for, spelled the
// way rclone's own hash registry spells it.
//
// The set is closed for the same reason the key vocabulary is: a
// manifest declaring "sha-256" or "SHA256" would otherwise ship a claim
// that silently matches nothing a consumer looks for. It holds the
// general-purpose digests, not the provider-proprietary ones (quickxor,
// dropbox): a name this engine cannot ask any backend for is a claim no
// consumer can act on, and adding one is a reviewed diff here rather
// than a string in a JSON file.
type HashAlgorithm string

const (
	HashMD5    HashAlgorithm = "md5"
	HashSHA1   HashAlgorithm = "sha1"
	HashSHA256 HashAlgorithm = "sha256"
	HashSHA512 HashAlgorithm = "sha512"
	HashCRC32  HashAlgorithm = "crc32"
)

var validHashAlgorithms = map[HashAlgorithm]bool{
	HashMD5: true, HashSHA1: true, HashSHA256: true,
	HashSHA512: true, HashCRC32: true,
}

func hashAlgorithmList() string {
	return `"md5", "sha1", "sha256", "sha512", "crc32"`
}

// Capabilities is one backend's answers, and its zero value is the least
// capable backend this type can describe: nothing claimed, nothing
// declared, every gate below refusing. See the file header for why that
// is the direction the zero value points.
type Capabilities struct {
	BoundedListing     bool               `json:"bounded_listing"`
	RecursiveListing   bool               `json:"recursive_listing"`
	StreamingOpen      bool               `json:"streaming_open"`
	RangeOpen          bool               `json:"range_open"`
	MTimePrecision     MTimePrecision     `json:"mtime_precision"`
	HashSupport        []HashAlgorithm    `json:"hash_support"`
	StableSize         bool               `json:"stable_size"`
	SymlinkSemantics   SymlinkSemantics   `json:"symlink_semantics"`
	MetadataSupport    MetadataSupport    `json:"metadata_support"`
	CaseSensitivity    CaseSensitivity    `json:"case_sensitivity"`
	CasePreservation   CasePreservation   `json:"case_preservation"`
	GenerationIdentity GenerationIdentity `json:"generation_identity"`

	// declared is which keys the document actually carried, so validation
	// can refuse a partial block naming the key that is missing rather
	// than letting a forgotten boolean read as false. It is unexported
	// because it is provenance, not data: nothing outside this package
	// may set it, and a Capabilities built in Go (a test's fixture, a
	// future caller's literal) is correctly reported as declaring
	// nothing.
	declared map[Capability]bool
}

// Declares reports whether the document this value was decoded from
// carried the given key.
func (c Capabilities) Declares(key Capability) bool { return c.declared[key] }

// Undeclared returns the keys this value does not answer, in
// CapabilityKeys() order. Empty means the whole vocabulary was declared.
func (c Capabilities) Undeclared() []Capability {
	var missing []Capability
	for _, key := range capabilityKeys {
		if !c.declared[key] {
			missing = append(missing, key)
		}
	}
	return missing
}

// UnmarshalJSON decodes one capability block, recording which keys were
// present and refusing a key that is not in the vocabulary.
//
// It is hand-written for two reasons that the struct tags alone cannot
// cover. Presence: encoding/json leaves an absent boolean as false, which
// is indistinguishable from a declared false, and this type's whole
// argument is that those two are different things. Closure: Load decodes
// manifests with DisallowUnknownFields, and that setting does not reach a
// type with its own UnmarshalJSON, so a typo'd key ("bounded_list") would
// otherwise be silently dropped here - which is the failure mode the
// setting exists to prevent everywhere else in the format.
func (c *Capabilities) UnmarshalJSON(raw []byte) error {
	var fields map[Capability]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fields); err != nil {
		return fmt.Errorf("capabilities: %w", err)
	}

	known := make(map[Capability]bool, len(capabilityKeys))
	for _, key := range capabilityKeys {
		known[key] = true
	}
	var unknown []string
	for key := range fields {
		if !known[key] {
			unknown = append(unknown, string(key))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("capabilities declares %s, which is not in the vocabulary (%s)",
			strings.Join(quoted(unknown), ", "), capabilityKeyList())
	}

	c.declared = make(map[Capability]bool, len(fields))
	for key, value := range fields {
		var err error
		switch key {
		case CapBoundedListing:
			err = json.Unmarshal(value, &c.BoundedListing)
		case CapRecursiveListing:
			err = json.Unmarshal(value, &c.RecursiveListing)
		case CapStreamingOpen:
			err = json.Unmarshal(value, &c.StreamingOpen)
		case CapRangeOpen:
			err = json.Unmarshal(value, &c.RangeOpen)
		case CapMTimePrecision:
			err = json.Unmarshal(value, &c.MTimePrecision)
		case CapHashSupport:
			err = json.Unmarshal(value, &c.HashSupport)
		case CapStableSize:
			err = json.Unmarshal(value, &c.StableSize)
		case CapSymlinkSemantics:
			err = json.Unmarshal(value, &c.SymlinkSemantics)
		case CapMetadataSupport:
			err = json.Unmarshal(value, &c.MetadataSupport)
		case CapCaseSensitivity:
			err = json.Unmarshal(value, &c.CaseSensitivity)
		case CapCasePreservation:
			err = json.Unmarshal(value, &c.CasePreservation)
		case CapGenerationIdentity:
			err = json.Unmarshal(value, &c.GenerationIdentity)
		}
		if err != nil {
			return fmt.Errorf("capabilities.%s does not parse: %w", key, err)
		}
		c.declared[key] = true
	}
	// hash_support declared as [] is "this backend can be asked for no
	// checksum", which is sftp's honest answer and is a different
	// statement from not having answered at all. Normalising the empty
	// slice to nil keeps the two apart in exactly one place: declared.
	if len(c.HashSupport) == 0 {
		c.HashSupport = nil
	}
	return nil
}

func quoted(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

func capabilityKeyList() string {
	names := make([]string, len(capabilityKeys))
	for i, key := range capabilityKeys {
		names[i] = string(key)
	}
	return strings.Join(names, ", ")
}

// DeclaredCapabilities returns this backend's capability matrix and
// whether the manifest carried one at all.
//
// A caller that ignores ok gets the zero Capabilities, which claims
// nothing - so the unsafe reading of this function is also the
// conservative one.
func (m Manifest) DeclaredCapabilities() (Capabilities, bool) {
	if m.Capabilities == nil {
		return Capabilities{}, false
	}
	// The copy is not free and is not optional: Capabilities is a value,
	// but its hash list is a slice the registry keeps, and a caller that
	// sorted or appended to it in place would be editing what every
	// later caller reads.
	caps := *m.Capabilities
	caps.HashSupport = slices.Clone(caps.HashSupport)
	return caps, true
}

// ErrUnqualifiedBackend is a refusal to enumerate a backend that has not
// said whether it can be enumerated. It is distinguishable from
// ErrUnboundedListing on purpose: one is a manifest somebody has to
// finish writing, the other is a real property of a real backend and an
// operator has a configuration answer for it.
var ErrUnqualifiedBackend = errors.New("backend: this backend declares no capability matrix, so nothing may assume its directories can be listed safely")

// ErrUnboundedListing is a refusal to enumerate a backend whose listing
// cannot be bounded. There is no configuration that makes it enumerable,
// which is why it carries no advice about one.
var ErrUnboundedListing = errors.New("backend: this backend cannot list a directory in bounded memory")

// EnumerationPlan is how a directory on one backend may be enumerated.
// It is deliberately one field and no transport types: this package may
// import nothing from core/internal/transport (see doc.go), and a plan
// that named a transport option would be that import.
type EnumerationPlan struct {
	// Bounded is whether enumeration costs memory proportional to the
	// caller's buffer rather than to the directory.
	//
	// It is true in every plan PlanEnumeration returns today, and that is
	// the point of the type rather than a redundancy in it: the other
	// outcomes are refusals, and a caller that stores the plan branches
	// on a value rather than on having remembered what a nil error meant.
	// What Phase 1 adds here is in ADR 0008: a per-directory ordering
	// guarantee, which Kopia's uploader needs and which no bounded walk
	// gives away for free.
	Bounded bool
}

// PlanEnumeration decides how - or whether - a directory on this backend
// may be enumerated. One plan and two refusals, and the refusals are the
// point of the function:
//
//   - The manifest declares no capabilities: ErrUnqualifiedBackend. Not a
//     guess, not a conservative-looking default that is still a guess.
//   - bounded_listing: a plan. The peak is the caller's buffer.
//   - not bounded_listing: ErrUnboundedListing, before anything is
//     dialed, opened or allocated. Fail closed.
//
// # Why there is no ceiling parameter
//
// There was one, and it was wrong in a way worth recording so it is not
// reintroduced: an operator-configured maximum directory size, which
// turned the third answer into "walk it, and refuse a directory holding
// more than N entries". That number cannot be a memory bound. Counting
// entries requires having them, and having them is the allocation - on
// the unbounded path the whole directory is materialised by
// rclone/pkg-sftp before any code above can call len() on it, so the
// refusal arrives after the cost it was supposed to prevent, i.e. never,
// because the process is already dead on the directory that mattered.
// A ceiling is a fine thing to have for other reasons (a producer that
// wrote 20,000,000 files is a problem an operator wants told about), and
// it is not this decision, so it is not this function's parameter.
func (m Manifest) PlanEnumeration() (EnumerationPlan, error) {
	caps, ok := m.DeclaredCapabilities()
	if !ok {
		return EnumerationPlan{}, fmt.Errorf("%w: %q", ErrUnqualifiedBackend, m.ID)
	}
	if !caps.BoundedListing {
		return EnumerationPlan{}, fmt.Errorf(
			"%w: %q, so a directory of any size would be read into memory whole before anything could refuse it",
			ErrUnboundedListing, m.ID)
	}
	return EnumerationPlan{Bounded: true}, nil
}

package backend

import (
	"encoding/json"
	"errors"
	"fmt"
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
// # Why the block is all ten keys or none
//
// The same argument one key at a time. A manifest that answers nine keys
// is a manifest whose tenth answer a consumer will infer, and validation
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
	// while s3's md5 is an ETag the store already holds. So this key
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
	CapCaseSensitivity Capability = "case_sensitivity"
)

// CapabilityKeys is the vocabulary, in declaration order. Pinned name for
// name by TestCapabilityKeysAreExactly, because the key strings are an
// EPIC #779 Phase 0 contract other issues consume.
var CapabilityKeys = []Capability{
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
}

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
// objects.
type CaseSensitivity string

const (
	CaseSensitive   CaseSensitivity = "sensitive"
	CaseInsensitive CaseSensitivity = "insensitive"
	CasePreserving  CaseSensitivity = "preserving" // insensitive, but the case as written is kept
	CaseUnknown     CaseSensitivity = "unknown"
)

var validCaseSensitivity = map[CaseSensitivity]bool{
	CaseSensitive: true, CaseInsensitive: true,
	CasePreserving: true, CaseUnknown: true,
}

// Capabilities is one backend's answers, and its zero value is the least
// capable backend this type can describe: nothing claimed, nothing
// declared, every gate below refusing. See the file header for why that
// is the direction the zero value points.
type Capabilities struct {
	BoundedListing   bool             `json:"bounded_listing"`
	RecursiveListing bool             `json:"recursive_listing"`
	StreamingOpen    bool             `json:"streaming_open"`
	RangeOpen        bool             `json:"range_open"`
	MTimePrecision   MTimePrecision   `json:"mtime_precision"`
	HashSupport      []string         `json:"hash_support"`
	StableSize       bool             `json:"stable_size"`
	SymlinkSemantics SymlinkSemantics `json:"symlink_semantics"`
	MetadataSupport  MetadataSupport  `json:"metadata_support"`
	CaseSensitivity  CaseSensitivity  `json:"case_sensitivity"`

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
// CapabilityKeys order. Empty means the whole vocabulary was declared.
func (c Capabilities) Undeclared() []Capability {
	var missing []Capability
	for _, key := range CapabilityKeys {
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

	known := make(map[Capability]bool, len(CapabilityKeys))
	for _, key := range CapabilityKeys {
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
	names := make([]string, len(CapabilityKeys))
	for i, key := range CapabilityKeys {
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
	return *m.Capabilities, true
}

// ErrUnqualifiedBackend is a refusal to enumerate a backend that has not
// said whether it can be enumerated. It is distinguishable from
// ErrUnboundedListing on purpose: one is a manifest somebody has to
// finish writing, the other is a real property of a real backend and an
// operator has a configuration answer for it.
var ErrUnqualifiedBackend = errors.New("backend: this backend declares no capability matrix, so nothing may assume its directories can be listed safely")

// ErrUnboundedListing is a refusal to enumerate a backend whose listing
// cannot be bounded, with no ceiling configured to abort at.
var ErrUnboundedListing = errors.New("backend: this backend cannot list a directory in bounded memory")

// EnumerationPlan is how a directory on one backend may be enumerated.
// It is deliberately two fields and no transport types: this package may
// import nothing from core/internal/transport (see doc.go), and a plan
// that named a transport option would be that import.
type EnumerationPlan struct {
	// Bounded is whether enumeration costs memory proportional to the
	// caller's buffer rather than to the directory.
	Bounded bool

	// MaxDirectoryEntries is the entry count above which enumeration must
	// refuse, and it is zero exactly when Bounded is true, because a
	// streaming enumeration has no count to refuse: a ceiling there would
	// be an arbitrary rejection of a directory this engine can walk.
	MaxDirectoryEntries int
}

// PlanEnumeration decides how - or whether - a directory on this backend
// may be enumerated, given the ceiling an operator has configured for a
// backend that cannot stream (zero meaning none configured).
//
// The three answers, and why the middle one exists:
//
//   - The manifest declares no capabilities: ErrUnqualifiedBackend. Not a
//     guess, not a conservative-looking default that is still a guess.
//   - bounded_listing: a plan with no ceiling. The peak is the buffer.
//   - not bounded_listing, and a ceiling is configured: a plan carrying
//     it. The enumeration aborts when a directory turns out to be bigger,
//     which is a late refusal - the entries are already in memory by the
//     time anything can count them, because that is what "not bounded"
//     means - but it is bounded by a number an operator chose and it
//     fails as an error a person can act on instead of as a kill signal.
//   - not bounded_listing, no ceiling: ErrUnboundedListing. Fail closed.
func (m Manifest) PlanEnumeration(maxDirectoryEntries int) (EnumerationPlan, error) {
	caps, ok := m.DeclaredCapabilities()
	if !ok {
		return EnumerationPlan{}, fmt.Errorf("%w: %q", ErrUnqualifiedBackend, m.ID)
	}
	if caps.BoundedListing {
		return EnumerationPlan{Bounded: true}, nil
	}
	if maxDirectoryEntries <= 0 {
		return EnumerationPlan{}, fmt.Errorf(
			"%w: %q, and no maximum directory size is configured for it, so a directory of any size would be read whole",
			ErrUnboundedListing, m.ID)
	}
	return EnumerationPlan{MaxDirectoryEntries: maxDirectoryEntries}, nil
}

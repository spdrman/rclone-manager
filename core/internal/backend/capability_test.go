package backend

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The capability matrix is issue #792's half of the Phase 0 contract: ten
// keys, owned by this package, that say what a backend can actually be
// asked to DO, as opposed to what an operator can configure about it
// (Fields) or what a connection test would prove about one instance
// (Probe). Every test in this file is about one of two properties:
//
//   - the vocabulary is closed and every shipped manifest answers all of
//     it, so a consumer never has to guess what silence meant;
//   - silence, where it happens anyway, is UNQUALIFIED rather than
//     capable, and the engine refuses rather than finding out at 1,000,000
//     directory entries that it guessed wrong.

// TestCapabilityKeysAreExactly pins the vocabulary name for name. The key
// set is a cross-issue contract in EPIC #779 Phase 0 (#793 classifies
// metadata trust from mtime_precision, hash_support, stable_size and
// metadata_support), so a rename is a reviewed diff that breaks a named
// test rather than a field somebody quietly stopped populating.
func TestCapabilityKeysAreExactly(t *testing.T) {
	want := []Capability{
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
	if !reflect.DeepEqual(CapabilityKeys, want) {
		t.Fatalf("CapabilityKeys = %v, want %v", CapabilityKeys, want)
	}
	spelled := []string{
		"bounded_listing", "recursive_listing", "streaming_open", "range_open",
		"mtime_precision", "hash_support", "stable_size", "symlink_semantics",
		"metadata_support", "case_sensitivity",
	}
	for i, key := range CapabilityKeys {
		if string(key) != spelled[i] {
			t.Errorf("CapabilityKeys[%d] is %q, and the contract spells it %q", i, key, spelled[i])
		}
	}
}

// TestEveryBundledManifestDeclaresTheWholeCapabilityMatrix is the reason
// the block is all-or-nothing: a shipped backend that answers nine of ten
// keys is a backend whose tenth answer some consumer will infer, and the
// inference that costs nothing to make is the optimistic one.
func TestEveryBundledManifestDeclaresTheWholeCapabilityMatrix(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	for _, id := range reg.IDs() {
		m, err := reg.Backend(id)
		if err != nil {
			t.Fatalf("Backend(%q): %v", id, err)
		}
		caps, ok := m.DeclaredCapabilities()
		if !ok {
			t.Errorf("bundled backend %q declares no capabilities at all", id)
			continue
		}
		if missing := caps.Undeclared(); len(missing) > 0 {
			t.Errorf("bundled backend %q leaves %v undeclared", id, missing)
		}
	}
}

// TestTheBundledCapabilityValuesAreHonest pins what the three shipped
// backends claim. It is a fixture-shaped test on purpose: these values are
// the ADR's evidence table, and a diff that flips one has to say so here
// where the reason for the old value is written down.
//
// The two entries worth reading twice:
//
//   - sftp's bounded_listing is FALSE. rclone's sftp backend reads a
//     directory through github.com/pkg/sftp's ReadDir, which returns the
//     whole directory as one slice; there is no paging cursor to hand
//     back, so no caller above it can bound the peak. That is what makes
//     "refuse the case" the only honest third option (see
//     PlanEnumeration).
//   - local_volume's case_sensitivity is "unknown", not "sensitive". A
//     local volume on this product's target hardware is whatever
//     filesystem an operator plugged in - APFS, exFAT on a USB disk, ext4
//     - and the process cannot know which without probing the specific
//     path. A matrix that guessed "sensitive" here would be wrong on the
//     first Mac and on every FAT-formatted stick.
func TestTheBundledCapabilityValuesAreHonest(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	want := map[string]Capabilities{
		"local_volume": {
			BoundedListing:   true,
			RecursiveListing: false,
			StreamingOpen:    true,
			RangeOpen:        true,
			MTimePrecision:   MTimeNanosecond,
			HashSupport:      []string{"md5", "sha1", "sha256"},
			StableSize:       true,
			SymlinkSemantics: SymlinksSkipped,
			MetadataSupport:  MetadataFull,
			CaseSensitivity:  CaseUnknown,
		},
		"s3": {
			BoundedListing:   true,
			RecursiveListing: true,
			StreamingOpen:    true,
			RangeOpen:        true,
			MTimePrecision:   MTimeMillisecond,
			HashSupport:      []string{"md5"},
			StableSize:       true,
			SymlinkSemantics: SymlinksUnsupported,
			MetadataSupport:  MetadataPartial,
			CaseSensitivity:  CaseSensitive,
		},
		"sftp": {
			BoundedListing:   false,
			RecursiveListing: false,
			StreamingOpen:    true,
			RangeOpen:        true,
			MTimePrecision:   MTimeSecond,
			HashSupport:      nil,
			StableSize:       true,
			SymlinkSemantics: SymlinksSkipped,
			MetadataSupport:  MetadataPartial,
			CaseSensitivity:  CaseUnknown,
		},
	}
	for id, expect := range want {
		m, err := reg.Backend(id)
		if err != nil {
			t.Fatalf("Backend(%q): %v", id, err)
		}
		got, ok := m.DeclaredCapabilities()
		if !ok {
			t.Fatalf("bundled backend %q declares no capabilities", id)
		}
		if got.BoundedListing != expect.BoundedListing ||
			got.RecursiveListing != expect.RecursiveListing ||
			got.StreamingOpen != expect.StreamingOpen ||
			got.RangeOpen != expect.RangeOpen ||
			got.MTimePrecision != expect.MTimePrecision ||
			got.StableSize != expect.StableSize ||
			got.SymlinkSemantics != expect.SymlinkSemantics ||
			got.MetadataSupport != expect.MetadataSupport ||
			got.CaseSensitivity != expect.CaseSensitivity ||
			!reflect.DeepEqual(got.HashSupport, expect.HashSupport) {
			t.Errorf("%s capabilities:\n got %+v\nwant %+v", id, got, expect)
		}
	}
}

// capabilityBlock is the shipped local_volume block, as JSON text a test
// can splice into a fixture manifest and mutate one key of.
const capabilityBlock = `"capabilities": {
    "bounded_listing": true,
    "recursive_listing": false,
    "streaming_open": true,
    "range_open": true,
    "mtime_precision": "1ns",
    "hash_support": ["md5", "sha1", "sha256"],
    "stable_size": true,
    "symlink_semantics": "skip",
    "metadata_support": "full",
    "case_sensitivity": "unknown"
  },`

// withCapabilities splices a capabilities block into one of the fixture
// manifests in manifest_test.go.
func withCapabilities(manifest, block string) string {
	return strings.Replace(manifest, `"fields": [`, block+"\n  \"fields\": [", 1)
}

// TestAFullyDeclaredCapabilityBlockLoads is the control for the four
// refusals below: the block this file mutates is otherwise accepted, so a
// failure there is the RULE firing and not the fixture being malformed.
func TestAFullyDeclaredCapabilityBlockLoads(t *testing.T) {
	reg := mustLoadOne(t, "a.json", withCapabilities(validLocalVolumeManifest, capabilityBlock))
	m, err := reg.Backend("local_volume")
	if err != nil {
		t.Fatalf("Backend: %v", err)
	}
	caps, ok := m.DeclaredCapabilities()
	if !ok {
		t.Fatal("a manifest that declares the whole block reports no capabilities")
	}
	if !caps.BoundedListing || caps.RecursiveListing {
		t.Errorf("declared values did not survive the load: %+v", caps)
	}
	if missing := caps.Undeclared(); len(missing) > 0 {
		t.Errorf("a complete block still reports %v undeclared", missing)
	}
}

// TestAPartiallyDeclaredCapabilityBlockIsRefusedByName is the
// all-or-nothing rule, and the message has to name the key that is
// missing: "capabilities are incomplete" sends a reviewer back to the
// vocabulary to diff it by eye.
func TestAPartiallyDeclaredCapabilityBlockIsRefusedByName(t *testing.T) {
	partial := strings.Replace(capabilityBlock, `"stable_size": true,`, "", 1)
	_, err := Load(mapFS(map[string]string{"a.json": withCapabilities(validLocalVolumeManifest, partial)}))
	if err == nil {
		t.Fatal("a manifest declaring nine of ten capability keys loaded; silence on one key is a capability a consumer would have to guess")
	}
	if !errors.Is(err, ErrMalformedManifest) {
		t.Errorf("error is not ErrMalformedManifest: %v", err)
	}
	if !strings.Contains(err.Error(), "stable_size") {
		t.Errorf("the refusal does not name the missing key: %v", err)
	}
}

// TestAnUnknownCapabilityKeyIsRefused holds the other half of closure. A
// typo ("bounded_list") in a block that is otherwise complete is the
// failure mode that would otherwise load as "the vocabulary's
// bounded_listing was not declared" - a confusing message for a real
// mistake - or, worse, silently as a capability nobody set.
func TestAnUnknownCapabilityKeyIsRefused(t *testing.T) {
	typo := strings.Replace(capabilityBlock, `"bounded_listing": true,`, `"bounded_listing": true, "bounded_list": true,`, 1)
	_, err := Load(mapFS(map[string]string{"a.json": withCapabilities(validLocalVolumeManifest, typo)}))
	if err == nil {
		t.Fatal("a manifest declaring an unknown capability key loaded")
	}
	if !strings.Contains(err.Error(), "bounded_list") {
		t.Errorf("the refusal does not name the offending key: %v", err)
	}
}

// TestACapabilityEnumValueOutsideItsVocabularyIsRefused covers the four
// keys that are not booleans. "sometimes" is the shape of a real mistake:
// a value that reads like an answer and that no consumer could branch on.
func TestACapabilityEnumValueOutsideItsVocabularyIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"mtime_precision", `"mtime_precision": "1ns",`, `"mtime_precision": "sometimes",`},
		{"symlink_semantics", `"symlink_semantics": "skip",`, `"symlink_semantics": "maybe",`},
		{"metadata_support", `"metadata_support": "full",`, `"metadata_support": "some",`},
		{"case_sensitivity", `"case_sensitivity": "unknown"`, `"case_sensitivity": "mixed"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := strings.Replace(capabilityBlock, tc.from, tc.to, 1)
			if block == capabilityBlock {
				t.Fatalf("the fixture no longer contains %s, so this case mutates nothing", tc.from)
			}
			_, err := Load(mapFS(map[string]string{"a.json": withCapabilities(validLocalVolumeManifest, block)}))
			if err == nil {
				t.Fatalf("a manifest declaring %s outside its vocabulary loaded", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("the refusal does not name the key it is about: %v", err)
			}
		})
	}
}

// TestAnUnqualifiedBackendIsRefusedRatherThanEnumerated is #792's
// acceptance criterion, at this package's own boundary: a manifest that
// says nothing about bounded_listing is not enumerable, full stop. The
// alternative - treat silence as "probably fine" - is the OOM this issue
// exists to make impossible, and it arrives only in production, on the
// one directory that got big.
func TestAnUnqualifiedBackendIsRefusedRatherThanEnumerated(t *testing.T) {
	reg := mustLoadOne(t, "a.json", validLocalVolumeManifest) // no capabilities block at all
	m, err := reg.Backend("local_volume")
	if err != nil {
		t.Fatalf("Backend: %v", err)
	}
	if _, ok := m.DeclaredCapabilities(); ok {
		t.Fatal("a manifest with no capabilities block reports declared capabilities")
	}

	plan, err := m.PlanEnumeration(0)
	if err == nil {
		t.Fatalf("an unqualified backend planned an enumeration: %+v", plan)
	}
	if !errors.Is(err, ErrUnqualifiedBackend) {
		t.Errorf("error is not ErrUnqualifiedBackend: %v", err)
	}
	if !strings.Contains(err.Error(), "local_volume") {
		t.Errorf("the refusal does not name the backend it is about: %v", err)
	}

	// An operator-configured ceiling does not buy its way past this. A
	// ceiling says "directories here are smaller than N"; it says nothing
	// about whether the backend can be read without materialising one.
	if _, err := m.PlanEnumeration(1000); !errors.Is(err, ErrUnqualifiedBackend) {
		t.Errorf("a configured ceiling talked an unqualified backend into being enumerable: %v", err)
	}
}

// TestABackendThatCannotStreamIsRefusedUnlessACeilingIsConfigured is
// option C of the ADR, expressed as a function: sftp cannot be read in
// bounded memory, so enumerating it is a refusal until an operator states
// a ceiling this engine will abort at.
func TestABackendThatCannotStreamIsRefusedUnlessACeilingIsConfigured(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	sftp, err := reg.Backend("sftp")
	if err != nil {
		t.Fatalf("Backend(\"sftp\"): %v", err)
	}

	if _, err := sftp.PlanEnumeration(0); !errors.Is(err, ErrUnboundedListing) {
		t.Fatalf("sftp with no configured ceiling: error is %v, want ErrUnboundedListing", err)
	}

	plan, err := sftp.PlanEnumeration(50_000)
	if err != nil {
		t.Fatalf("sftp with a ceiling of 50k: %v", err)
	}
	if plan.Bounded {
		t.Error("the plan claims bounded enumeration for a backend whose listing is one slice")
	}
	if plan.MaxDirectoryEntries != 50_000 {
		t.Errorf("plan.MaxDirectoryEntries = %d, want the configured 50000", plan.MaxDirectoryEntries)
	}
}

// TestABackendThatStreamsNeedsNoCeiling is the other side: local_volume
// is read in chunks, so its peak is the chunk and there is nothing for a
// ceiling to protect. A plan that carried one anyway would be an
// arbitrary refusal of a directory this engine can in fact walk.
func TestABackendThatStreamsNeedsNoCeiling(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	local, err := reg.Backend("local_volume")
	if err != nil {
		t.Fatalf("Backend(\"local_volume\"): %v", err)
	}
	plan, err := local.PlanEnumeration(0)
	if err != nil {
		t.Fatalf("local_volume with no ceiling: %v", err)
	}
	if !plan.Bounded {
		t.Error("plan.Bounded is false for a backend declaring bounded_listing")
	}
	if plan.MaxDirectoryEntries != 0 {
		t.Errorf("plan.MaxDirectoryEntries = %d, want 0: a streaming enumeration has no entry count to refuse", plan.MaxDirectoryEntries)
	}
}

// TestTheZeroCapabilitiesValueIsTheLeastCapableOne is the property every
// consumer of this matrix leans on: whatever produced a Capabilities
// nobody filled in - a manifest with no block, a zero value in a struct
// literal, a future decoder that failed halfway - describes a backend
// this engine will not do anything clever with.
func TestTheZeroCapabilitiesValueIsTheLeastCapableOne(t *testing.T) {
	var zero Capabilities
	if zero.BoundedListing || zero.RecursiveListing || zero.StreamingOpen || zero.RangeOpen || zero.StableSize {
		t.Errorf("a zero Capabilities claims a boolean capability: %+v", zero)
	}
	if len(zero.HashSupport) != 0 {
		t.Errorf("a zero Capabilities claims hashes: %v", zero.HashSupport)
	}
	if len(zero.Undeclared()) != len(CapabilityKeys) {
		t.Errorf("a zero Capabilities reports %d undeclared keys, want all %d", len(zero.Undeclared()), len(CapabilityKeys))
	}
	for _, key := range CapabilityKeys {
		if zero.Declares(key) {
			t.Errorf("a zero Capabilities claims to declare %s", key)
		}
	}
}

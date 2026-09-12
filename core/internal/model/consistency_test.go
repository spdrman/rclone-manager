// The consistency and trust vocabulary, checked for the three properties
// every layer above it assumes.
//
// First: a trust class is never STRONGER than the evidence behind it. The
// whole point of this classification is that a backend which cannot prove
// content identity gets told so, and the failure mode of getting it wrong is
// not a wrong label in a panel, it is a changed file this manager decided
// not to read because a second-resolution timestamp agreed with itself.
//
// Second: every (class, preset) pair produces a policy that re-reads
// content at some bounded cadence. A policy that permits "skip forever" on
// weak evidence is the same bug wearing a configuration key.
//
// Third: the modes mean what they say. live_best_effort must not claim a
// point-in-time guarantee, and the two modes that DO rest on an external
// promise must treat a mutation they observe as that promise being broken,
// because an operator who quiesced the wrong service needs to hear about it.

package model

import (
	"testing"
	"time"
)

// The modes and classes are persisted and operator-visible, so their wire
// strings are pinned here rather than left to whatever a rename produces.
// A changed string is a changed API and a changed catalog column.
func TestConsistencyModeAndTrustClassWireStringsArePinned(t *testing.T) {
	for _, tc := range []struct {
		mode ConsistencyMode
		want string
	}{
		{ModeLiveBestEffort, "live_best_effort"},
		{ModeExternallyQuiesced, "externally_quiesced"},
		{ModeExternalSnapshot, "external_snapshot"},
	} {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("ConsistencyMode string = %q, want %q", got, tc.want)
		}
		back, err := ParseConsistencyMode(tc.want)
		if err != nil {
			t.Errorf("ParseConsistencyMode(%q): %v", tc.want, err)
			continue
		}
		if back != tc.mode {
			t.Errorf("ParseConsistencyMode(%q) = %q, want %q", tc.want, back, tc.mode)
		}
	}

	if len(ConsistencyModes()) != 3 {
		t.Fatalf("ConsistencyModes() has %d entries, want 3; a fourth mode is an operator-visible decision and an ADR change", len(ConsistencyModes()))
	}

	for _, tc := range []struct {
		class TrustClass
		want  string
	}{
		{TrustStrong, "strong"},
		{TrustWeak, "weak"},
		{TrustUnknown, "unknown"},
	} {
		if got := tc.class.String(); got != tc.want {
			t.Errorf("TrustClass string = %q, want %q", got, tc.want)
		}
	}
}

// The preset is an operator-facing configuration value and a trust class is
// a derived verdict, and no spelling may serve as both. They did once:
// `strong` was a preset AND a class, so a run report that said "strong"
// told nobody which of the two it meant and a source could be "weak under
// strong". The vocabularies are asserted to be disjoint rather than merely
// renamed, because the collision would come back the moment somebody added
// a third value to either side.
func TestPresetAndTrustClassVocabulariesDoNotCollide(t *testing.T) {
	presets := map[MetadataTrustPreset]string{
		PresetTrustMetadata: "trust_metadata",
		PresetConservative:  "conservative",
	}

	for preset, want := range presets {
		if got := preset.String(); got != want {
			t.Errorf("MetadataTrustPreset string = %q, want %q", got, want)
		}
	}

	for _, class := range []TrustClass{TrustStrong, TrustWeak, TrustUnknown} {
		for preset := range presets {
			if class.String() == preset.String() {
				t.Errorf("%q is both a trust class and a metadata-trust preset", class)
			}
		}
	}
}

// An unknown mode string must be refused rather than defaulted. The zero
// ConsistencyMode is the empty string, and a parser that quietly answered
// live_best_effort for a typo would give the weakest guarantee to an
// operator who asked for the strongest.
func TestParseConsistencyModeRefusesAnythingElse(t *testing.T) {
	for _, s := range []string{"", "live", "LIVE_BEST_EFFORT", "snapshot", " live_best_effort"} {
		if got, err := ParseConsistencyMode(s); err == nil {
			t.Errorf("ParseConsistencyMode(%q) = %q, want an error", s, got)
		}
	}
}

// Only external_snapshot may claim a point in time. This is the property
// that stops a surface from printing "consistent backup" over a live read,
// and it is asserted for all three modes rather than for the one that says
// yes, so a fourth mode cannot be added silently on the permissive side.
func TestOnlyExternalSnapshotGuaranteesAPointInTime(t *testing.T) {
	want := map[ConsistencyMode]bool{
		ModeLiveBestEffort:     false,
		ModeExternallyQuiesced: false,
		ModeExternalSnapshot:   true,
	}
	for _, m := range ConsistencyModes() {
		if got := m.GuaranteesPointInTime(); got != want[m] {
			t.Errorf("%s.GuaranteesPointInTime() = %v, want %v", m, got, want[m])
		}
	}
}

// A mutation observed under a mode that rests on an external promise is that
// promise being broken, and has to be reported as such. Under
// live_best_effort the same observation is ordinary and must NOT be reported
// as a violation, or the report becomes noise an operator learns to ignore.
func TestMutationIsAContractViolationOnlyWhereAPromiseWasMade(t *testing.T) {
	want := map[ConsistencyMode]bool{
		ModeLiveBestEffort:     false,
		ModeExternallyQuiesced: true,
		ModeExternalSnapshot:   true,
	}
	for _, m := range ConsistencyModes() {
		if got := m.MutationIsContractViolation(); got != want[m] {
			t.Errorf("%s.MutationIsContractViolation() = %v, want %v", m, got, want[m])
		}
	}
}

// Every mode has an operator-facing sentence, because the matrix in the ADR
// is rendered from these and a mode whose description was the empty string
// would render as a blank row.
func TestEveryModeDescribesItself(t *testing.T) {
	for _, m := range ConsistencyModes() {
		if m.Describe() == "" {
			t.Errorf("%s.Describe() is empty", m)
		}
	}
}

// The classification property, stated as the thing it defends: STRONG is
// reachable only through evidence that cannot silently lie about content (a
// hash the backend computes without this manager reading the object, or a
// generation identifier that changes on overwrite). No combination of
// timestamps, sizes and metadata richness may reach it, however fine the
// timestamp is.
func TestStrongTrustRequiresHashOrGenerationEvidence(t *testing.T) {
	// The most flattering possible metadata-only source: nanosecond
	// timestamps, stable sizes, full metadata. Still weak.
	best := SourceSignals{
		MTimePrecision:  MTimeNanosecond,
		StableSize:      true,
		MetadataSupport: MetadataFull,
	}
	if got := ClassifyMetadataTrust(best); got.Class != TrustWeak {
		t.Fatalf("nanosecond mtime + stable size + full metadata classified %q (%s), want weak: mtime is settable and an in-place rewrite can preserve it", got.Class, got.Reason)
	}

	withHash := best
	withHash.RemoteHashAlgorithms = []string{"sha256"}
	if got := ClassifyMetadataTrust(withHash); got.Class != TrustStrong {
		t.Errorf("a backend-computed content hash classified %q (%s), want strong", got.Class, got.Reason)
	}

	withGeneration := best
	withGeneration.ObjectGeneration = true
	if got := ClassifyMetadataTrust(withGeneration); got.Class != TrustStrong {
		t.Errorf("an object generation identifier classified %q (%s), want strong", got.Class, got.Reason)
	}
}

// Absent or unusable signals are UNKNOWN, never weak. Weak means "we know
// what this source reports and it is not enough"; unknown means "we have not
// established what it reports", and the two call for different policies
// (weak gets a sampled re-read, unknown gets an unconditional one).
func TestMissingSignalsClassifyUnknownRatherThanWeak(t *testing.T) {
	for name, sig := range map[string]SourceSignals{
		"zero value": {},
		"unstated mtime precision": {
			MTimePrecision:  MTimePrecisionUnknown,
			StableSize:      true,
			MetadataSupport: MetadataFull,
		},
		"no metadata at all": {
			MTimePrecision:  MTimeNanosecond,
			StableSize:      true,
			MetadataSupport: MetadataNone,
		},
		"size not stable": {
			MTimePrecision:  MTimeNanosecond,
			MetadataSupport: MetadataFull,
		},
		"precision string this package does not know": {
			MTimePrecision:  MTimePrecision("about a minute"),
			StableSize:      true,
			MetadataSupport: MetadataFull,
		},
	} {
		if got := ClassifyMetadataTrust(sig); got.Class != TrustUnknown {
			t.Errorf("%s classified %q (%s), want unknown", name, got.Class, got.Reason)
		}
	}
}

// Strong evidence survives poor metadata. An object store that reports no
// usable timestamp but does version its objects is still strong, because the
// evidence the class rests on is not the timestamp.
func TestStrongEvidenceOutranksPoorMetadata(t *testing.T) {
	got := ClassifyMetadataTrust(SourceSignals{
		MTimePrecision:   MTimePrecisionUnknown,
		MetadataSupport:  MetadataNone,
		ObjectGeneration: true,
	})
	if got.Class != TrustStrong {
		t.Fatalf("classified %q (%s), want strong", got.Class, got.Reason)
	}
}

// Every classification says why, because the reason is what reaches the
// operator when a source is downgraded and "weak" on its own is not
// actionable.
func TestEveryClassificationCarriesAReason(t *testing.T) {
	for _, sig := range []SourceSignals{
		{},
		{MTimePrecision: MTimeSecond, StableSize: true, MetadataSupport: MetadataFull},
		{RemoteHashAlgorithms: []string{"md5"}},
		{ObjectGeneration: true},
	} {
		if got := ClassifyMetadataTrust(sig); got.Reason == "" {
			t.Errorf("ClassifyMetadataTrust(%#v) returned class %q with no reason", sig, got.Class)
		}
	}
}

// mtime precision is a claim about resolution, and coarse resolutions have
// to be readable as durations so a policy (and a log line) can say how wide
// the blind window is.
func TestMTimePrecisionResolvesToADuration(t *testing.T) {
	for _, tc := range []struct {
		p    MTimePrecision
		want time.Duration
		ok   bool
	}{
		{MTimeNanosecond, time.Nanosecond, true},
		{MTimeMillisecond, time.Millisecond, true},
		{MTimeSecond, time.Second, true},
		{MTimeTwoSecond, 2 * time.Second, true},
		{MTimePrecisionUnknown, 0, false},
		{MTimePrecision("1h"), 0, false},
		{MTimePrecision("1us"), 0, false},
	} {
		got, ok := tc.p.Resolution()
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q.Resolution() = (%v, %v), want (%v, %v)", tc.p, got, ok, tc.want, tc.ok)
		}
	}
}

// The policy matrix, pinned. These four rows are the operator-visible
// contract the ADR publishes, so they are asserted by value rather than by
// property: a changed cadence here is a changed promise.
func TestVerificationPolicyMatrix(t *testing.T) {
	for _, tc := range []struct {
		class    TrustClass
		preset   MetadataTrustPreset
		wantMode VerificationMode
	}{
		{TrustStrong, PresetTrustMetadata, VerifyRemoteHash},
		{TrustStrong, PresetConservative, VerifyPeriodic},
		{TrustWeak, PresetTrustMetadata, VerifySampled},
		{TrustWeak, PresetConservative, VerifyAlways},
		{TrustUnknown, PresetTrustMetadata, VerifyAlways},
		{TrustUnknown, PresetConservative, VerifyAlways},
	} {
		got := VerificationPolicyFor(tc.class, tc.preset)
		if got.Mode != tc.wantMode {
			t.Errorf("VerificationPolicyFor(%q, %q).Mode = %q, want %q", tc.class, tc.preset, got.Mode, tc.wantMode)
		}
		if got.Reason == "" {
			t.Errorf("VerificationPolicyFor(%q, %q) has no reason", tc.class, tc.preset)
		}
	}
}

// The correctness property the presets exist to preserve: whatever the
// preset, a source whose metadata is not strong evidence is re-read at some
// bounded cadence. "never" must be unreachable through this function, and a
// policy that may skip content must name the interval after which it stops
// skipping.
func TestNoPolicyEverPermitsSkippingContentForever(t *testing.T) {
	classes := []TrustClass{TrustStrong, TrustWeak, TrustUnknown, TrustClass("something new")}
	presets := []MetadataTrustPreset{PresetTrustMetadata, PresetConservative, MetadataTrustPreset("typo")}

	for _, class := range classes {
		for _, preset := range presets {
			pol := VerificationPolicyFor(class, preset)
			if pol.Mode == VerifyNever {
				t.Errorf("VerificationPolicyFor(%q, %q) returned mode %q", class, preset, VerifyNever)
			}
			if pol.Mode == VerifyAlways {
				if pol.MetadataMaySkipContent() {
					t.Errorf("VerificationPolicyFor(%q, %q) is %q yet permits metadata to skip content", class, preset, pol.Mode)
				}
				continue
			}
			if !pol.MetadataMaySkipContent() {
				t.Errorf("VerificationPolicyFor(%q, %q) is %q yet forbids metadata skipping; one of the two is wrong", class, preset, pol.Mode)
			}
			if pol.ReverifyInterval <= 0 {
				t.Errorf("VerificationPolicyFor(%q, %q) may skip content with no re-verification interval", class, preset)
			}
		}
	}
}

// An unrecognised preset must fall to the cautious side. A typo in a
// configuration key must never buy a weaker policy than the one that was
// asked for.
func TestUnrecognisedPresetIsTreatedAsConservative(t *testing.T) {
	typo := VerificationPolicyFor(TrustWeak, MetadataTrustPreset("stronk"))
	conservative := VerificationPolicyFor(TrustWeak, PresetConservative)
	if typo.Mode != conservative.Mode {
		t.Fatalf("unrecognised preset gave mode %q, want %q (the conservative one)", typo.Mode, conservative.Mode)
	}
}

// A sampled policy has to name a fraction in (0,1]; a fraction of zero would
// be VerifyNever spelled differently and would pass the interval check above
// while sampling nothing.
func TestSampledPolicyNamesAUsableFraction(t *testing.T) {
	pol := VerificationPolicyFor(TrustWeak, PresetTrustMetadata)
	if pol.Mode != VerifySampled {
		t.Fatalf("mode = %q, want %q", pol.Mode, VerifySampled)
	}
	if pol.SampleFraction <= 0 || pol.SampleFraction > 1 {
		t.Fatalf("SampleFraction = %v, want a fraction in (0,1]", pol.SampleFraction)
	}
}

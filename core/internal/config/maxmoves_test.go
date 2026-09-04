package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// intp is the "the operator wrote this number" spelling. MaxMovesPerCycle
// is a pointer precisely so an explicit 0 and an absent key are different
// facts: one is a number this package refuses, the other is a key it
// defaults.
func intp(n int) *int { return &n }

// Tests for issue #239's config half: FR-30's max_moves_per_cycle, the
// per-cycle bound on how many artifacts one retention pass relocates.
//
// #238 landed the bound as a struct field on placement.Engine and said
// outright why it stopped there: "the place that reads it is the retention
// cycle that calls RunCycle, which is #239's", and adding a schema key
// nothing reads is what FR-35's round-trip rule exists to prevent. This is
// the key, and the cycle that reads it lands with it.

// TestMaxMovesPerCycle_DefaultsWhenAMediumIsDeclared pins the resolved
// value rather than the raw field, the same way EffectiveStorageClass and
// EffectiveUploadVerification are pinned: a default this package wrote
// back into the struct would be frozen into the operator's file by the
// next settings save.
func TestMaxMovesPerCycle_DefaultsWhenAMediumIsDeclared(t *testing.T) {
	c := mediumsConfig()
	mustValidate(t, &c)

	if got := c.EffectiveMaxMovesPerCycle(); got != DefaultMaxMovesPerCycle {
		t.Errorf("EffectiveMaxMovesPerCycle() = %d on a config that declares a medium and sets no bound, want the documented default %d", got, DefaultMaxMovesPerCycle)
	}
	if c.MaxMovesPerCycle != nil {
		t.Errorf("Validate wrote %d back into MaxMovesPerCycle; the default has to stay an accessor, or the next settings save freezes it into the operator's file", *c.MaxMovesPerCycle)
	}
}

// TestMaxMovesPerCycle_IsZeroWithNoMediumDeclared is the fail-safe
// direction. A deployment with no medium has nowhere to move anything to,
// and placement.Engine.RunCycle treats a non-positive bound as "do
// nothing at all", so this is what keeps a medium-free deployment's
// behaviour bit-identical (FR-35) without the cycle needing a second
// "are mediums configured" test of its own.
func TestMaxMovesPerCycle_IsZeroWithNoMediumDeclared(t *testing.T) {
	c := validConfig()
	mustValidate(t, &c)

	if got := c.EffectiveMaxMovesPerCycle(); got != 0 {
		t.Errorf("EffectiveMaxMovesPerCycle() = %d with no storage_mediums declared, want 0 so the move engine does nothing", got)
	}
}

// TestMaxMovesPerCycle_ValidationTable covers every refusal and its
// accepting counterpart, so no rule can pass by never firing.
func TestMaxMovesPerCycle_ValidationTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   func() Config
		wantErr []string // every substring the message must carry; nil means accepted
	}{
		{
			name:  "a positive bound beside a declared medium is accepted",
			build: func() Config { c := mediumsConfig(); c.MaxMovesPerCycle = intp(2); return c },
		},
		{
			name:    "zero written explicitly is refused rather than silently defaulted",
			build:   func() Config { c := mediumsConfig(); c.MaxMovesPerCycle = intp(0); return c },
			wantErr: []string{"max_moves_per_cycle", "positive"},
		},
		{
			name:    "a negative bound is refused",
			build:   func() Config { c := mediumsConfig(); c.MaxMovesPerCycle = intp(-1); return c },
			wantErr: []string{"max_moves_per_cycle", "positive"},
		},
		{
			name:    "a bound with no medium to move to is refused as unused",
			build:   func() Config { c := validConfig(); c.MaxMovesPerCycle = intp(4); return c },
			wantErr: []string{"max_moves_per_cycle", "storage_mediums"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.build()
			err := c.Validate()
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate said %q, which does not carry %q", err, want)
				}
			}
		})
	}
}

// TestMaxMovesPerCycle_NeverAppearsInAMediumFreeRoundTrip is FR-35's
// round-trip rule for this one key. core/service rewrites the whole Config
// on every settings save, and an older binary reading the result under
// Load's KnownFields(true) refuses an unknown field outright, so a key
// this deployment never wrote must not come back from a save.
func TestMaxMovesPerCycle_NeverAppearsInAMediumFreeRoundTrip(t *testing.T) {
	c := validConfig()
	mustValidate(t, &c)

	out, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "max_moves_per_cycle") {
		t.Errorf("re-marshalling a medium-free config injected max_moves_per_cycle into it:\n%s", out)
	}
}

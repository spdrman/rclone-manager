package capacity

import (
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// Thresholds.Validate
// ---------------------------------------------------------------------------

func TestThresholdsValidate(t *testing.T) {
	tests := []struct {
		name    string
		th      Thresholds
		wantErr bool
	}{
		{"warning above critical", Thresholds{WarningFreeBytes: 200, CriticalFreeBytes: 100}, false},
		{"warning equals critical", Thresholds{WarningFreeBytes: 100, CriticalFreeBytes: 100}, false},
		{"both zero", Thresholds{}, false},
		{"warning below critical", Thresholds{WarningFreeBytes: 50, CriticalFreeBytes: 100}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.th.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tt.wantErr {
				var invalid *ThresholdsInvalidError
				if !errors.As(err, &invalid) {
					t.Fatalf("Validate() error type = %T, want *ThresholdsInvalidError", err)
				}
				if msg := invalid.Error(); msg == "" {
					t.Error("Error() = empty string")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Assess: pure arithmetic, no filesystem involved
// ---------------------------------------------------------------------------

func TestAssessRejectsNegativeArtifactSize(t *testing.T) {
	_, err := Assess(Stat{AvailableBytes: 1000}, -1, Thresholds{})
	if err == nil {
		t.Fatal("Assess with a negative artifact size = nil error, want one")
	}
}

func TestAssessRejectsInvalidThresholds(t *testing.T) {
	_, err := Assess(Stat{AvailableBytes: 1000}, 10, Thresholds{WarningFreeBytes: 1, CriticalFreeBytes: 2})
	if err == nil {
		t.Fatal("Assess with inverted thresholds = nil error, want one")
	}
	var invalid *ThresholdsInvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("error type = %T, want *ThresholdsInvalidError", err)
	}
}

func TestAssessDetectsOverflow(t *testing.T) {
	// math.MaxInt64 (the largest artifact size Assess accepts) plus a
	// safety margin one short of math.MaxUint64 wraps a uint64 sum: the sum
	// mathematically exceeds 2^64-1, so the wrapped result comes back
	// smaller than either input alone, which is exactly the wraparound
	// Assess checks for.
	const maxInt64 = int64(1<<63 - 1)
	const marginNearMaxUint64 = ^uint64(0) - 1
	_, err := Assess(Stat{AvailableBytes: 1000}, maxInt64, Thresholds{SafetyMarginBytes: marginNearMaxUint64})
	if err == nil {
		t.Fatal("Assess with an overflowing requirement = nil error, want one")
	}
}

// TestAssessLevels walks every boundary of the OK/Warning/Critical
// classification against a fixed Thresholds, including the "does not even
// fit" case, which must classify as Critical regardless of the numeric
// thresholds (see Assessment.Level's doc).
func TestAssessLevels(t *testing.T) {
	th := Thresholds{
		WarningFreeBytes:  1000,
		CriticalFreeBytes: 100,
		SafetyMarginBytes: 50,
	}
	artifactSize := int64(500)
	required := uint64(500 + 50) // 550

	tests := []struct {
		name           string
		availableBytes uint64
		wantFits       bool
		wantLevel      Level
		wantProjected  uint64
		wantShortfall  uint64
	}{
		{
			name:           "comfortably OK",
			availableBytes: required + 2000, // projected = 2000 > warning(1000)
			wantFits:       true,
			wantLevel:      OK,
			wantProjected:  2000,
		},
		{
			name:           "exactly at the OK/Warning boundary reads as Warning",
			availableBytes: required + th.WarningFreeBytes, // projected == warning
			wantFits:       true,
			wantLevel:      Warning,
			wantProjected:  th.WarningFreeBytes,
		},
		{
			name:           "one byte above the warning boundary is still OK",
			availableBytes: required + th.WarningFreeBytes + 1,
			wantFits:       true,
			wantLevel:      OK,
			wantProjected:  th.WarningFreeBytes + 1,
		},
		{
			name:           "in the warning band",
			availableBytes: required + 500, // projected 500, between critical(100) and warning(1000)
			wantFits:       true,
			wantLevel:      Warning,
			wantProjected:  500,
		},
		{
			name:           "exactly at the Warning/Critical boundary reads as Critical",
			availableBytes: required + th.CriticalFreeBytes, // projected == critical
			wantFits:       true,
			wantLevel:      Critical,
			wantProjected:  th.CriticalFreeBytes,
		},
		{
			name:           "one byte above the critical boundary is Warning",
			availableBytes: required + th.CriticalFreeBytes + 1,
			wantFits:       true,
			wantLevel:      Warning,
			wantProjected:  th.CriticalFreeBytes + 1,
		},
		{
			name:           "fits exactly, projects to zero, still Critical",
			availableBytes: required,
			wantFits:       true,
			wantLevel:      Critical,
			wantProjected:  0,
		},
		{
			name:           "does not fit at all",
			availableBytes: required - 1,
			wantFits:       false,
			wantLevel:      Critical,
			wantShortfall:  1,
		},
		{
			name:           "wildly short",
			availableBytes: 0,
			wantFits:       false,
			wantLevel:      Critical,
			wantShortfall:  required,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stat := Stat{AvailableBytes: tt.availableBytes}
			a, err := Assess(stat, artifactSize, th)
			if err != nil {
				t.Fatalf("Assess() error = %v, want nil", err)
			}
			if a.Fits != tt.wantFits {
				t.Errorf("Fits = %v, want %v", a.Fits, tt.wantFits)
			}
			if a.Level != tt.wantLevel {
				t.Errorf("Level = %v, want %v", a.Level, tt.wantLevel)
			}
			if a.RequiredBytes != required {
				t.Errorf("RequiredBytes = %d, want %d", a.RequiredBytes, required)
			}
			if tt.wantFits {
				if a.ProjectedAvailableBytes != tt.wantProjected {
					t.Errorf("ProjectedAvailableBytes = %d, want %d", a.ProjectedAvailableBytes, tt.wantProjected)
				}
				if a.ShortfallBytes != 0 {
					t.Errorf("ShortfallBytes = %d, want 0 when Fits", a.ShortfallBytes)
				}
			} else {
				if a.ShortfallBytes != tt.wantShortfall {
					t.Errorf("ShortfallBytes = %d, want %d", a.ShortfallBytes, tt.wantShortfall)
				}
				if a.ProjectedAvailableBytes != 0 {
					t.Errorf("ProjectedAvailableBytes = %d, want 0 when !Fits", a.ProjectedAvailableBytes)
				}
			}
		})
	}
}

// TestAssessHeadroomArithmeticIsNotDoubled is the specific regression this
// package's headroom-arithmetic reasoning exists to protect: a caller must
// never be charged two artifacts' worth of space for the .partial/final
// hard-link overlap in internal/lifecycle/commit.go. This test pins the
// exact requirement formula (ArtifactSizeBytes + SafetyMarginBytes) rather
// than merely asserting an assessment came back.
func TestAssessHeadroomArithmeticIsNotDoubled(t *testing.T) {
	const artifactSize = int64(4 << 30) // 4 GiB, matching the scenario the issue calls out
	th := Thresholds{SafetyMarginBytes: 1 << 30}

	a, err := Assess(Stat{AvailableBytes: 5 << 30}, artifactSize, th)
	if err != nil {
		t.Fatalf("Assess() error = %v", err)
	}
	wantRequired := uint64(artifactSize) + th.SafetyMarginBytes
	if a.RequiredBytes != wantRequired {
		t.Fatalf("RequiredBytes = %d, want %d (artifact once, plus margin, never doubled)", a.RequiredBytes, wantRequired)
	}
	if doubled := uint64(artifactSize)*2 + th.SafetyMarginBytes; a.RequiredBytes == doubled {
		t.Fatalf("RequiredBytes accidentally matches the doubled-artifact formula (%d)", doubled)
	}
}

func TestAssessCurrentUsesZeroArtifactSize(t *testing.T) {
	th := Thresholds{WarningFreeBytes: 1000, CriticalFreeBytes: 100, SafetyMarginBytes: 200}
	a, err := AssessCurrent(Stat{AvailableBytes: 5000}, th)
	if err != nil {
		t.Fatalf("AssessCurrent() error = %v", err)
	}
	if a.ArtifactSizeBytes != 0 {
		t.Errorf("ArtifactSizeBytes = %d, want 0", a.ArtifactSizeBytes)
	}
	if a.RequiredBytes != th.SafetyMarginBytes {
		t.Errorf("RequiredBytes = %d, want %d (margin only)", a.RequiredBytes, th.SafetyMarginBytes)
	}
}

// ---------------------------------------------------------------------------
// Admit: the actual FR-21 go/no-go decision
// ---------------------------------------------------------------------------

func TestAdmitAllowsAnOKTransfer(t *testing.T) {
	th := Thresholds{WarningFreeBytes: 1000, CriticalFreeBytes: 100, SafetyMarginBytes: 50}
	a, err := Admit(Stat{AvailableBytes: 1_000_000}, 500, th)
	if err != nil {
		t.Fatalf("Admit() error = %v, want nil", err)
	}
	if a.Level != OK {
		t.Errorf("Level = %v, want OK", a.Level)
	}
}

func TestAdmitAllowsWithOnlyAWarning(t *testing.T) {
	th := Thresholds{WarningFreeBytes: 1000, CriticalFreeBytes: 100, SafetyMarginBytes: 50}
	required := uint64(500 + 50)
	a, err := Admit(Stat{AvailableBytes: required + 500}, 500, th) // projected 500: warning band
	if err != nil {
		t.Fatalf("Admit() error = %v, want nil (warning must not block)", err)
	}
	if a.Level != Warning {
		t.Errorf("Level = %v, want Warning", a.Level)
	}
}

func TestAdmitRefusesWhenItDoesNotFit(t *testing.T) {
	th := Thresholds{SafetyMarginBytes: 50}
	_, err := Admit(Stat{AvailableBytes: 100}, 500, th)
	if err == nil {
		t.Fatal("Admit() = nil error, want a refusal")
	}
	var insufficient *InsufficientCapacityError
	if !errors.As(err, &insufficient) {
		t.Fatalf("error type = %T, want *InsufficientCapacityError", err)
	}
	if insufficient.Assessment.Fits {
		t.Error("Assessment.Fits = true, want false")
	}
	if msg := err.Error(); msg == "" {
		t.Error("Error() = empty string")
	}
}

func TestAdmitRefusesWhenItWouldBreachCritical(t *testing.T) {
	th := Thresholds{WarningFreeBytes: 2000, CriticalFreeBytes: 1000, SafetyMarginBytes: 50}
	required := uint64(500 + 50)
	// Fits (barely), but leaves only 100 bytes projected, under the 1000-byte
	// critical floor: the "fits but not safely" refusal shape.
	_, err := Admit(Stat{AvailableBytes: required + 100}, 500, th)
	if err == nil {
		t.Fatal("Admit() = nil error, want a refusal for breaching the critical floor")
	}
	var insufficient *InsufficientCapacityError
	if !errors.As(err, &insufficient) {
		t.Fatalf("error type = %T, want *InsufficientCapacityError", err)
	}
	if !insufficient.Assessment.Fits {
		t.Error("Assessment.Fits = false, want true (it fits, it just breaches critical)")
	}
	if msg := err.Error(); msg == "" {
		t.Error("Error() = empty string")
	}
}

func TestAdmitPropagatesAssessErrors(t *testing.T) {
	_, err := Admit(Stat{}, -1, Thresholds{})
	if err == nil {
		t.Fatal("Admit() with a negative artifact size = nil error, want one")
	}
	var insufficient *InsufficientCapacityError
	if errors.As(err, &insufficient) {
		t.Fatal("Admit() returned *InsufficientCapacityError for an input error, want a plain validation error")
	}
}

// ---------------------------------------------------------------------------
// Level.String
// ---------------------------------------------------------------------------

func TestLevelString(t *testing.T) {
	tests := map[Level]string{
		OK:       "OK",
		Warning:  "WARNING",
		Critical: "CRITICAL",
		Level(9): "Level(9)",
	}
	for level, want := range tests {
		if got := level.String(); got != want {
			t.Errorf("Level(%d).String() = %q, want %q", int(level), got, want)
		}
	}
}

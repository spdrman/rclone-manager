package capacity

import (
	"errors"
	"strings"
	"testing"
)

// This file is issue #286's half of the FR-21 guard: the operator-set
// ceiling on how much space this manager may occupy, weighed against the
// filesystem's own free space, and the refusal that comes out of the two
// together.
//
// Every test here is written against the pair of questions the package
// doc calls out as genuinely different: "how much room is left on the
// disk" and "how much of my allowance have I spent". A test that only
// ever exercised one of them would pass on an implementation that never
// learned to tell them apart, which is the exact defect this file exists
// to prevent.

const gib = uint64(1) << 30

// ---------------------------------------------------------------------------
// The decisive pair: a cap refuses, and a larger cap admits the same transfer
// ---------------------------------------------------------------------------

// TestAdmitRefusesATransferThatWouldExceedTheCap is the whole point of the
// issue. The disk has room to spare, so nothing about free space explains
// the refusal: the only reason this transfer cannot begin is that
// finishing it would put this manager over the ceiling its operator set.
func TestAdmitRefusesATransferThatWouldExceedTheCap(t *testing.T) {
	stat := Stat{TotalBytes: 2000 * gib, FreeBytes: 1500 * gib, AvailableBytes: 1500 * gib}
	usage := Usage{Bytes: 95 * gib, Known: true}
	th := Thresholds{CapBytes: 100 * gib}

	a, err := Admit(stat, usage, int64(10*gib), th)

	var insufficient *InsufficientCapacityError
	if !errors.As(err, &insufficient) {
		t.Fatalf("Admit() error = %v (%T), want *InsufficientCapacityError: a 10 GiB artifact does not fit in the 5 GiB left of a 100 GiB cap", err, err)
	}
	if a.Fits {
		t.Error("Fits = true, want false: 10 GiB does not fit in 5 GiB of cap headroom")
	}
	if a.Binding != ConstraintCap {
		t.Errorf("Binding = %v, want %v: 1500 GiB of disk is free, so only the cap can explain this refusal", a.Binding, ConstraintCap)
	}
	if a.HeadroomBytes != 5*gib {
		t.Errorf("HeadroomBytes = %d, want %d (100 GiB cap minus 95 GiB already used)", a.HeadroomBytes, 5*gib)
	}
	if a.ShortfallBytes != 5*gib {
		t.Errorf("ShortfallBytes = %d, want %d", a.ShortfallBytes, 5*gib)
	}
	// "cap" alone would be satisfied by the word "capacity" this package
	// prefixes every message with, which is exactly the sort of assertion
	// that passes without the behaviour it names.
	if msg := err.Error(); !strings.Contains(msg, "configured cap") {
		t.Errorf("refusal message %q never mentions the configured cap, so an operator reading it would go looking at their disk", msg)
	}
}

// TestAdmitAllowsTheSameTransferUnderALargerCap is the positive control the
// test above needs, and it is not optional: a guard that refuses everything
// passes the refusal test perfectly. Identical filesystem, identical usage,
// identical artifact; the ONLY thing that changes is the cap, and the
// verdict has to change with it.
func TestAdmitAllowsTheSameTransferUnderALargerCap(t *testing.T) {
	stat := Stat{TotalBytes: 2000 * gib, FreeBytes: 1500 * gib, AvailableBytes: 1500 * gib}
	usage := Usage{Bytes: 95 * gib, Known: true}
	th := Thresholds{CapBytes: 200 * gib}

	a, err := Admit(stat, usage, int64(10*gib), th)
	if err != nil {
		t.Fatalf("Admit() error = %v, want nil: 10 GiB fits comfortably in the 105 GiB left of a 200 GiB cap", err)
	}
	if !a.Fits {
		t.Error("Fits = false, want true")
	}
	if a.Binding != ConstraintCap {
		t.Errorf("Binding = %v, want %v: 105 GiB of cap headroom is still less than 1500 GiB of disk", a.Binding, ConstraintCap)
	}
	if a.ProjectedHeadroomBytes != 95*gib {
		t.Errorf("ProjectedHeadroomBytes = %d, want %d", a.ProjectedHeadroomBytes, 95*gib)
	}
}

// ---------------------------------------------------------------------------
// Whichever is smaller
// ---------------------------------------------------------------------------

// TestTheSmallerOfDiskAndCapBinds walks the four interesting combinations.
// A cap does not help if the disk fills first, and a roomy disk does not
// license spending past the cap, so the guard has to take the minimum every
// time rather than picking one denominator and living with it.
func TestTheSmallerOfDiskAndCapBinds(t *testing.T) {
	tests := []struct {
		name         string
		available    uint64
		usage        Usage
		cap          uint64
		wantHeadroom uint64
		wantBinding  Constraint
	}{
		{
			name:         "no cap configured: the disk is the only answer",
			available:    40 * gib,
			usage:        Usage{Bytes: 10 * gib, Known: true},
			cap:          0,
			wantHeadroom: 40 * gib,
			wantBinding:  ConstraintDisk,
		},
		{
			name:         "cap headroom is smaller than disk free",
			available:    900 * gib,
			usage:        Usage{Bytes: 90 * gib, Known: true},
			cap:          100 * gib,
			wantHeadroom: 10 * gib,
			wantBinding:  ConstraintCap,
		},
		{
			name:         "disk free is smaller than cap headroom",
			available:    3 * gib,
			usage:        Usage{Bytes: 10 * gib, Known: true},
			cap:          1000 * gib,
			wantHeadroom: 3 * gib,
			wantBinding:  ConstraintDisk,
		},
		{
			name:         "already over the cap: no headroom at all, and no underflow",
			available:    900 * gib,
			usage:        Usage{Bytes: 150 * gib, Known: true},
			cap:          100 * gib,
			wantHeadroom: 0,
			wantBinding:  ConstraintCap,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := AssessCurrent(Stat{AvailableBytes: tt.available}, tt.usage, Thresholds{CapBytes: tt.cap})
			if err != nil {
				t.Fatalf("AssessCurrent() error = %v", err)
			}
			if a.HeadroomBytes != tt.wantHeadroom {
				t.Errorf("HeadroomBytes = %d, want %d", a.HeadroomBytes, tt.wantHeadroom)
			}
			if a.Binding != tt.wantBinding {
				t.Errorf("Binding = %v, want %v", a.Binding, tt.wantBinding)
			}
		})
	}
}

// TestADiskThatFillsFirstStillRefusesUnderAGenerousCap is the other half of
// "whichever is smaller", driven all the way to a verdict rather than only
// to a headroom number. A 1 TB cap with 10 GB spent says there is plenty of
// allowance left; the disk says there is not, and the disk wins.
func TestADiskThatFillsFirstStillRefusesUnderAGenerousCap(t *testing.T) {
	stat := Stat{TotalBytes: 500 * gib, FreeBytes: 2 * gib, AvailableBytes: 2 * gib}
	usage := Usage{Bytes: 10 * gib, Known: true}
	th := Thresholds{CapBytes: 1000 * gib}

	a, err := Admit(stat, usage, int64(8*gib), th)

	var insufficient *InsufficientCapacityError
	if !errors.As(err, &insufficient) {
		t.Fatalf("Admit() error = %v, want a refusal: the cap has 990 GiB left but the disk has 2 GiB", err)
	}
	if a.Binding != ConstraintDisk {
		t.Errorf("Binding = %v, want %v", a.Binding, ConstraintDisk)
	}
}

// ---------------------------------------------------------------------------
// Zero is a sentinel, and unknown usage is not zero usage
// ---------------------------------------------------------------------------

// TestACapOfZeroMeansNoCap pins the sentinel the operator-facing default
// rests on. A literal reading of "cap of zero bytes" would refuse every
// transfer on every deployment that never set one, which is all of them.
func TestACapOfZeroMeansNoCap(t *testing.T) {
	stat := Stat{TotalBytes: 500 * gib, FreeBytes: 400 * gib, AvailableBytes: 400 * gib}

	a, err := Admit(stat, Usage{Bytes: 300 * gib, Known: true}, int64(50*gib), Thresholds{CapBytes: 0})
	if err != nil {
		t.Fatalf("Admit() with cap 0 error = %v, want nil: zero means no cap, never a zero-byte ceiling", err)
	}
	if a.Binding != ConstraintDisk {
		t.Errorf("Binding = %v, want %v", a.Binding, ConstraintDisk)
	}
	if a.CapConfigured {
		t.Error("CapConfigured = true for a cap of 0, want false")
	}
}

// TestUsageIsIrrelevantWithoutACap is why the zero value of Usage is safe
// to pass: with no cap configured there is no question for it to answer,
// so a caller that has not measured anything is not thereby broken.
func TestUsageIsIrrelevantWithoutACap(t *testing.T) {
	stat := Stat{AvailableBytes: 100 * gib}

	a, err := AssessCurrent(stat, Usage{}, Thresholds{})
	if err != nil {
		t.Fatalf("AssessCurrent() with an unknown usage and no cap error = %v, want nil", err)
	}
	if a.HeadroomBytes != 100*gib {
		t.Errorf("HeadroomBytes = %d, want %d", a.HeadroomBytes, 100*gib)
	}
}

// TestACapWithUnknownUsageIsRefusedRatherThanGuessed is the counterpart. An
// unmeasured usage read as zero would report the whole cap as headroom,
// which is a confident wrong number in the one direction that lets a
// deployment blow straight through its ceiling. This package reports what
// it cannot assess (see the package doc); it never guesses at it.
func TestACapWithUnknownUsageIsRefusedRatherThanGuessed(t *testing.T) {
	stat := Stat{AvailableBytes: 900 * gib}

	_, err := AssessCurrent(stat, Usage{}, Thresholds{CapBytes: 100 * gib})
	if err == nil {
		t.Fatal("AssessCurrent() with a configured cap and an unmeasured usage = nil error, want a refusal to guess")
	}
	var unknown *UsageUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("error type = %T, want *UsageUnknownError", err)
	}
}

// ---------------------------------------------------------------------------
// Thresholds keep working against whichever constraint binds
// ---------------------------------------------------------------------------

// TestWarningAndCriticalMeasureTheBindingHeadroom is what stops the two
// threshold numbers from silently meaning "free disk" once a cap exists. An
// operator who sets a 100 GB cap and a 10 GB critical floor is asking to be
// refused with 10 GB of ALLOWANCE left, not 10 GB of disk left.
func TestWarningAndCriticalMeasureTheBindingHeadroom(t *testing.T) {
	stat := Stat{TotalBytes: 4000 * gib, FreeBytes: 3000 * gib, AvailableBytes: 3000 * gib}
	th := Thresholds{
		CapBytes:          100 * gib,
		WarningFreeBytes:  20 * gib,
		CriticalFreeBytes: 10 * gib,
	}

	// 60 GiB spent, so 40 GiB of allowance left. A 15 GiB artifact lands
	// with 25 GiB left: above the warning line.
	ok, err := Assess(stat, Usage{Bytes: 60 * gib, Known: true}, int64(15*gib), th)
	if err != nil {
		t.Fatalf("Assess() error = %v", err)
	}
	if ok.Level != OK {
		t.Errorf("Level = %v, want OK: 25 GiB of allowance would be left, above the 20 GiB warning line", ok.Level)
	}

	// A 25 GiB artifact lands with 15 GiB left: warning, but admitted.
	warn, err := Admit(stat, Usage{Bytes: 60 * gib, Known: true}, int64(25*gib), th)
	if err != nil {
		t.Fatalf("Admit() error = %v, want nil: a warning is never by itself a refusal", err)
	}
	if warn.Level != Warning {
		t.Errorf("Level = %v, want Warning", warn.Level)
	}

	// A 35 GiB artifact lands with 5 GiB left: below the critical floor,
	// refused even though it technically fits.
	_, err = Admit(stat, Usage{Bytes: 60 * gib, Known: true}, int64(35*gib), th)
	var insufficient *InsufficientCapacityError
	if !errors.As(err, &insufficient) {
		t.Fatalf("Admit() error = %v, want a refusal: finishing would leave 5 GiB of allowance, under the 10 GiB floor", err)
	}
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// TestAssessmentReportsBothDenominators is the display contract issue #286
// asks for in so many words: a bar at 80% of a 2 TB disk and a bar at 80%
// of a 100 GB cap are different facts, so the assessment has to carry
// enough for a caller to say which one it drew.
func TestAssessmentReportsBothDenominators(t *testing.T) {
	stat := Stat{TotalBytes: 2000 * gib, FreeBytes: 1200 * gib, AvailableBytes: 1200 * gib}
	usage := Usage{Bytes: 30 * gib, Known: true}
	th := Thresholds{CapBytes: 100 * gib}

	a, err := AssessCurrent(stat, usage, th)
	if err != nil {
		t.Fatalf("AssessCurrent() error = %v", err)
	}
	if a.LimitBytes != 100*gib {
		t.Errorf("LimitBytes = %d, want %d (the cap, which is the denominator in effect)", a.LimitBytes, 100*gib)
	}
	if a.UsedBytes != 30*gib {
		t.Errorf("UsedBytes = %d, want %d (this manager's own consumption, not the whole volume's)", a.UsedBytes, 30*gib)
	}
	if a.CapHeadroomBytes != 70*gib {
		t.Errorf("CapHeadroomBytes = %d, want %d", a.CapHeadroomBytes, 70*gib)
	}

	nocap, err := AssessCurrent(stat, usage, Thresholds{})
	if err != nil {
		t.Fatalf("AssessCurrent() error = %v", err)
	}
	if nocap.LimitBytes != 2000*gib {
		t.Errorf("LimitBytes = %d, want %d (the whole filesystem, which is the denominator with no cap set)", nocap.LimitBytes, 2000*gib)
	}
	if nocap.UsedBytes != 800*gib {
		t.Errorf("UsedBytes = %d, want %d (total minus free: with no cap the question is how full the volume is)", nocap.UsedBytes, 800*gib)
	}
}

// TestConstraintString keeps the two denominators nameable on a wire and in
// a log line without a caller re-deriving the words.
func TestConstraintString(t *testing.T) {
	if got := ConstraintDisk.String(); got != "disk" {
		t.Errorf("ConstraintDisk.String() = %q, want %q", got, "disk")
	}
	if got := ConstraintCap.String(); got != "cap" {
		t.Errorf("ConstraintCap.String() = %q, want %q", got, "cap")
	}
}

// TestCheckBeforeTransferHonoursTheCapAgainstARealFilesystem drives the one
// call a real caller makes, so the cap cannot be honoured by Assess and
// quietly dropped by the convenience wrapper the pipeline actually uses.
func TestCheckBeforeTransferHonoursTheCapAgainstARealFilesystem(t *testing.T) {
	dir := t.TempDir()

	// A cap of 1 KiB with 1 KiB already spent leaves nothing, on a
	// filesystem that certainly has room for a 512-byte file.
	_, err := CheckBeforeTransfer(dir, Usage{Bytes: 1024, Known: true}, 512, Thresholds{CapBytes: 1024})
	var insufficient *InsufficientCapacityError
	if !errors.As(err, &insufficient) {
		t.Fatalf("CheckBeforeTransfer() error = %v, want a refusal: the cap is spent", err)
	}

	// Positive control on the real filesystem too.
	if _, err := CheckBeforeTransfer(dir, Usage{Bytes: 1024, Known: true}, 512, Thresholds{CapBytes: 1 << 30}); err != nil {
		t.Fatalf("CheckBeforeTransfer() error = %v, want nil under a 1 GiB cap", err)
	}
}

package capacity

import (
	"path/filepath"
	"testing"
)

// TestStatPathReportsSaneValues exercises the real OS statfs call this
// package's admission logic ultimately depends on. It cannot pin exact
// byte counts (that would make the test flake with every other thing
// running on the machine), but it can and does assert the numbers are
// internally consistent, which is exactly what Assess/Admit trust to be
// true.
func TestStatPathReportsSaneValues(t *testing.T) {
	dir := t.TempDir()

	st, err := StatPath(dir)
	if err != nil {
		t.Fatalf("StatPath(%q) error = %v", dir, err)
	}

	if st.TotalBytes == 0 {
		t.Error("TotalBytes = 0, want > 0 for a real filesystem")
	}
	if st.FreeBytes > st.TotalBytes {
		t.Errorf("FreeBytes (%d) > TotalBytes (%d)", st.FreeBytes, st.TotalBytes)
	}
	if st.AvailableBytes > st.FreeBytes {
		t.Errorf("AvailableBytes (%d) > FreeBytes (%d): available-to-unprivileged should never exceed free-including-reserved", st.AvailableBytes, st.FreeBytes)
	}
}

// TestStatPathErrorsOnAMissingPath proves a bad path is a loud error, not a
// zero-valued Stat that Assess/Admit could mistake for "no space at all"
// or, worse, silently treat as fine.
func TestStatPathErrorsOnAMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	if _, err := StatPath(missing); err == nil {
		t.Fatalf("StatPath(%q) error = nil, want an error for a nonexistent path", missing)
	}
}

// TestCheckBeforeTransferAgainstARealFilesystem exercises the full
// StatPath+Admit path this package's real caller will actually use,
// against the local filesystem the test runs on, with thresholds set so
// low that any developer or CI machine's temp filesystem comfortably
// admits a tiny artifact.
func TestCheckBeforeTransferAgainstARealFilesystem(t *testing.T) {
	dir := t.TempDir()

	a, err := CheckBeforeTransfer(dir, 1024, Thresholds{
		WarningFreeBytes:  1,
		CriticalFreeBytes: 1,
		SafetyMarginBytes: 0,
	})
	if err != nil {
		t.Fatalf("CheckBeforeTransfer() error = %v, want nil for a 1KiB artifact against a real filesystem", err)
	}
	if !a.Fits {
		t.Error("Fits = false, want true")
	}
}

// TestCheckBeforeTransferRefusesAnImpossibleArtifact sets the incoming
// artifact size far beyond anything a real disk holds, proving the refusal
// path is reachable through the full StatPath+Admit call, not just through
// Admit in isolation with a synthetic Stat.
func TestCheckBeforeTransferRefusesAnImpossibleArtifact(t *testing.T) {
	dir := t.TempDir()

	const absurd = int64(1) << 62 // 4 exabytes; no test machine has this free
	_, err := CheckBeforeTransfer(dir, absurd, Thresholds{})
	if err == nil {
		t.Fatal("CheckBeforeTransfer() error = nil, want a refusal for an impossibly large artifact")
	}
}

func TestCheckBeforeTransferPropagatesAStatfsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	_, err := CheckBeforeTransfer(missing, 1, Thresholds{})
	if err == nil {
		t.Fatal("CheckBeforeTransfer() error = nil, want a statfs error for a nonexistent directory")
	}
}

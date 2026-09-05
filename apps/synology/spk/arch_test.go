package spk

import "testing"

// The architecture table, checked in both directions.
//
// Every family this package claims has to map to a target the canonical
// release actually builds, and a target the release does not build has to
// be refused rather than mapped to something plausible. That second
// direction is the one worth having: a table that quietly accepted an
// unbuildable GOARCH would produce a package referencing a binary nobody
// ever produced, and the failure would surface as a missing file at build
// time with no explanation of why that architecture was ever attempted.

// TestArchMapping pins this project's claimed architectures to Synology's
// own Appendix A platform/arch mapping table. §68's Provider Test Matrix
// requires "a representative DSM 7.x amd64 and/or arm64 model for each
// architecture claimed", so the size of this table is literally the number
// of NAS units an operator has to find.
func TestArchMapping(t *testing.T) {
	if len(Arches) != 2 {
		t.Fatalf("this project claims %d architectures; §68 needs one representative DSM 7.x model per claimed architecture, so widening this table widens the hardware requirement", len(Arches))
	}

	tests := []struct {
		goarch      string
		wantDSM     string
		wantMember  string
		wantMissing string
	}{
		{goarch: "amd64", wantDSM: "x86_64", wantMember: "apollolake", wantMissing: "rtd1296"},
		{goarch: "arm64", wantDSM: "armv8", wantMember: "rtd1296", wantMissing: "apollolake"},
	}

	for _, tc := range tests {
		t.Run(tc.goarch, func(t *testing.T) {
			a, err := ArchForGOARCH(tc.goarch)
			if err != nil {
				t.Fatalf("ArchForGOARCH(%q): %v", tc.goarch, err)
			}
			if a.DSM != tc.wantDSM {
				t.Fatalf("ArchForGOARCH(%q).DSM = %q, want %q", tc.goarch, a.DSM, tc.wantDSM)
			}
			if !a.Covers(tc.wantMember) {
				t.Fatalf("%s should cover DSM platform %q", a.DSM, tc.wantMember)
			}
			if a.Covers(tc.wantMissing) {
				t.Fatalf("%s must not cover DSM platform %q, which belongs to the other family", a.DSM, tc.wantMissing)
			}

			back, err := ArchForDSM(tc.wantDSM)
			if err != nil {
				t.Fatalf("ArchForDSM(%q): %v", tc.wantDSM, err)
			}
			if back.GOARCH != tc.goarch {
				t.Fatalf("ArchForDSM(%q).GOARCH = %q, want %q", tc.wantDSM, back.GOARCH, tc.goarch)
			}
		})
	}
}

// TestArchMapping_RejectsUnbuildableTargets is the control: the 32-bit DSM
// families are real Synology architectures, and the mapping must refuse
// them rather than quietly widen the support claim to hardware this
// project ships no binary for.
func TestArchMapping_RejectsUnbuildableTargets(t *testing.T) {
	for _, name := range []string{"noarch", "i686", "armv7", "armv5", "evansport", "alpine", "", "AMD64"} {
		t.Run(name, func(t *testing.T) {
			if _, err := ArchForDSM(name); err == nil {
				t.Fatalf("ArchForDSM(%q) succeeded; this project builds no binary for it", name)
			}
		})
	}
	for _, goarch := range []string{"386", "arm", "riscv64", ""} {
		t.Run("goarch/"+goarch, func(t *testing.T) {
			if _, err := ArchForGOARCH(goarch); err == nil {
				t.Fatalf("ArchForGOARCH(%q) succeeded; the canonical release builds only amd64 and arm64", goarch)
			}
		})
	}
}

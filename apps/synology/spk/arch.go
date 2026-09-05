// The architecture table: one row per DSM arch family, tying it to the Go
// build target the release produces and to the DSM platforms Synology's
// own mapping table says the family covers.
//
// The ELF machine field is the reason this is a table rather than a pair
// of strings. Hash parity proves the file in the package is the file the
// release manifest recorded, which is a strong claim about provenance and
// says nothing at all about architecture: a manifest that recorded the
// arm64 binary under the amd64 key produces a package that verifies
// perfectly and cannot run on the machine it claims to target. Reading the
// ELF header is the only check that catches that, and it is cheap.
//
// The platform list is here rather than in the README because the
// supported-model matrix and the acceptance procedure are generated from
// it. A list maintained in prose beside a list the code validates against
// is two lists, and the prose one is the one that goes stale.
package spk

import (
	"debug/elf"
	"fmt"
	"slices"
	"strings"
)

// Arch ties one INFO `arch` family to the Go build target the canonical
// release produces for it, and to the DSM platforms Synology's Appendix A
// mapping table says that family covers.
type Arch struct {
	// DSM is the value written into INFO's `arch` key.
	DSM string

	// GOARCH is the release binary's architecture, and the key
	// container/release-manifest.json records its SHA-256 under.
	GOARCH string

	// ELFMachine is what a correctly built binary's ELF header must say.
	// Hash parity alone cannot catch a manifest that recorded the wrong
	// file for an architecture; this can.
	ELFMachine elf.Machine

	// Platforms are the DSM platform names Appendix A lists under this
	// family. Recorded so the supported-model matrix in
	// apps/synology/README.md and the acceptance procedure are generated
	// from the same list the package is validated against.
	Platforms []string
}

// Covers reports whether platform is one of this family's members.
func (a Arch) Covers(platform string) bool { return slices.Contains(a.Platforms, platform) }

// Arches is every architecture this project claims for Synology.
//
// Deliberately two entries, not more. §68's Provider Test Matrix requires
// "a representative DSM 7.x amd64 and/or arm64 model for each architecture
// claimed", so each row here is a NAS an operator has to physically find
// before that architecture can stop being "build-supported but
// uncertified". The 32-bit families Appendix A also lists (i686/evansport,
// armv7/alpine, armv5/628x) are absent because the canonical release
// builds no 32-bit binary — there is nothing honest to package for them,
// and DSM refusing the install is the correct outcome.
var Arches = []Arch{
	{
		DSM:        "x86_64",
		GOARCH:     "amd64",
		ELFMachine: elf.EM_X86_64,
		Platforms: []string{
			"apollolake", "avoton", "braswell", "broadwell", "broadwellnk",
			"broadwellntb", "broadwellntbap", "bromolow", "cedarview",
			"coffeelake", "denverton", "geminilake", "grantley", "kvmx64",
			"purley", "skylaked", "v1000",
		},
	},
	{
		DSM:        "armv8",
		GOARCH:     "arm64",
		ELFMachine: elf.EM_AARCH64,
		Platforms:  []string{"rtd1296", "armada37xx", "rtd1619", "rtd1619b"},
	},
}

// ArchForGOARCH resolves a release binary's architecture to its DSM arch
// family.
func ArchForGOARCH(goarch string) (Arch, error) {
	for _, a := range Arches {
		if a.GOARCH == goarch {
			return a, nil
		}
	}
	return Arch{}, fmt.Errorf("no Synology package is built for GOARCH %q: the canonical release builds %s", goarch, claimedGOARCHes())
}

// ArchForDSM resolves an INFO `arch` value back to a release architecture.
// It refuses anything outside Arches, including `noarch`: a package that
// declares itself architecture-independent while carrying a native
// executable would install on hardware it cannot run on.
func ArchForDSM(name string) (Arch, error) {
	for _, a := range Arches {
		if a.DSM == name {
			return a, nil
		}
	}
	return Arch{}, fmt.Errorf("arch %q is not one this project claims: it builds only %s", name, claimedDSMArches())
}

func claimedGOARCHes() string {
	out := make([]string, 0, len(Arches))
	for _, a := range Arches {
		out = append(out, a.GOARCH)
	}
	return strings.Join(out, " and ")
}

func claimedDSMArches() string {
	out := make([]string, 0, len(Arches))
	for _, a := range Arches {
		out = append(out, a.DSM)
	}
	return strings.Join(out, " and ")
}

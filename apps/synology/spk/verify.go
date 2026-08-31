package spk

// Check names. Exported so tests, and the spkctl CLI's output, name the
// same checks rather than drifting apart through string literals.
const (
	CheckOuterArchive  = "outer-archive-is-an-uncompressed-tar"
	CheckLayout        = "documented-package-layout"
	CheckINFO          = "info-necessary-fields"
	CheckArch          = "declared-architecture-is-claimed"
	CheckBinaryParity  = "core-binary-hash-parity-with-the-release-manifest"
	CheckBinaryMachine = "core-binary-elf-machine-matches-the-declared-arch"
	CheckLauncher      = "dsm-desktop-launcher-is-present"
	CheckNoSecrets     = "no-bundled-secrets"
	CheckFileModes     = "no-setuid-setgid-or-world-writable-files"
	CheckLifecycle     = "lifecycle-scripts-delete-nothing-outside-the-package"
)

// Check is one conformance assertion's outcome.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Report is the whole result of verifying one `.spk`.
type Report struct {
	SPKPath string
	Arch    string
	Version string
	Checks  []Check
}

// OK reports whether every check passed.
func (r *Report) OK() bool { return len(r.Failures()) == 0 }

// Failures returns the checks that did not pass.
func (r *Report) Failures() []Check { return nil }

// CheckNames lists the checks that actually ran, for a test that needs to
// tell "failed" apart from "never executed".
func (r *Report) CheckNames() []string { return nil }

func (r *Report) String() string { return "" }

// Verify re-derives everything about a built `.spk` that can be checked
// without DSM hardware, and compares it against manifest.
func Verify(_ string, _ ReleaseManifest) (*Report, error) { return nil, errNotImplemented }

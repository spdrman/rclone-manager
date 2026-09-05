// The conformance check: open a finished `.spk` and re-derive every claim
// the build made about it.
//
// It reads the package rather than the build's own bookkeeping, and that
// separation is the entire value. A verifier that trusted anything Build
// recorded would be checking Build against itself, and the specific claim
// at stake here, that this package carries the exact release binaries, is
// one an operator has to be able to re-derive from the artifact alone.
//
// The checks are named as exported constants so the CLI's output, the
// tests and this file all use the same strings rather than three copies
// that drift. Each check appends its own result rather than returning
// early, so one failure never hides the others: an operator debugging a
// bad package wants the whole list, not the first thing that went wrong.
package spk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

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

	// ManifestPath, ManifestVersion and ManifestCommit name the input
	// the parity check was decided against.
	//
	// A green report that does not say what it compared the package to
	// cannot be audited: `spkctl verify --manifest` accepts any path, so
	// against a manifest generated from the very binaries being packaged
	// the parity check degrades to an internal consistency check with
	// the same name and the same output. Naming the manifest, its
	// version and the commit it records does not close that (refusing an
	// unreachable commit is issue #174's job), but it does put the
	// evidence's own input in the evidence.
	ManifestPath    string
	ManifestVersion string
	ManifestCommit  string

	Checks []Check
}

// OK reports whether every check passed.
func (r *Report) OK() bool { return len(r.Failures()) == 0 }

// Failures returns the checks that did not pass.
func (r *Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// CheckNames lists the checks that actually ran, for a test that needs to
// tell "failed" apart from "never executed".
func (r *Report) CheckNames() []string {
	out := make([]string, 0, len(r.Checks))
	for _, c := range r.Checks {
		out = append(out, c.Name)
	}
	return out
}

// String renders the whole report, failures and passes together.
//
// Printing the passes is not padding. An operator looking at a red line
// needs to know which of the other nine checks ran and were satisfied,
// because "one check failed" and "one check failed and eight never ran"
// call for different next steps, and a report that showed only failures
// cannot tell them apart.
//
// The manifest identity is printed even when there is nothing to print,
// spelled out as an explicit absence rather than an empty field, so a
// verdict is never attributed to an input nobody can name.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", r.SPKPath)
	if r.Arch != "" || r.Version != "" {
		fmt.Fprintf(&b, "  arch=%s version=%s\n", r.Arch, r.Version)
	}
	fmt.Fprintf(&b, "  manifest=%s manifest-version=%s manifest-commit=%s\n",
		orElse(r.ManifestPath, "(no file: built in memory)"),
		orElse(r.ManifestVersion, "(none recorded)"),
		orElse(r.ManifestCommit, "(none recorded)"))
	for _, c := range r.Checks {
		status := "ok  "
		if !c.OK {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %s\n", status, c.Name)
		if c.Detail != "" {
			for _, line := range strings.Split(strings.TrimRight(c.Detail, "\n"), "\n") {
				fmt.Fprintf(&b, "         %s\n", line)
			}
		}
	}
	return b.String()
}

func (r *Report) pass(name, detail string) { r.Checks = append(r.Checks, Check{name, true, detail}) }
func (r *Report) fail(name, detail string) { r.Checks = append(r.Checks, Check{name, false, detail}) }

// record turns a list of findings into one check result: no findings is a
// pass, and any number of them is a single failure carrying all of them.
//
// Collapsing them into one check rather than one per finding keeps the
// check list stable. A report whose number of lines depends on how broken
// the package is cannot be compared against a previous run, and the check
// names are what the CLI and the tests agree on.
func (r *Report) record(name string, findings []string) {
	if len(findings) == 0 {
		r.pass(name, "")
		return
	}
	r.fail(name, strings.Join(findings, "\n"))
}

// Verify re-derives everything about a built `.spk` that can be checked
// without DSM hardware, and compares it against manifest.
//
// It returns an error only when the file cannot be read at all. Every
// other problem is a failed Check, because a report that names which of
// ten properties broke is worth more than a single error that stops at
// the first one - and because the tests need each assertion to be
// individually observable, not collapsed into one boolean.
func Verify(spkPath string, manifest ReleaseManifest) (*Report, error) {
	raw, err := os.ReadFile(spkPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", spkPath, err)
	}
	rep := &Report{
		SPKPath:         spkPath,
		ManifestPath:    manifest.SourcePath,
		ManifestVersion: manifest.Version,
		ManifestCommit:  manifest.Commit,
	}

	// 1. The outer container. pkgscripts-ng's pkg_make_spk runs `tar cf`,
	// uncompressed, and DSM reads it that way.
	if compression := detectCompression(raw); compression != "" {
		rep.fail(CheckOuterArchive, fmt.Sprintf("the .spk is %s-compressed; pkg_make_spk produces a plain uncompressed tar (`tar cf`)", compression))
		return rep, nil
	}
	outer, err := readArchive(bytes.NewReader(raw))
	if err != nil {
		rep.fail(CheckOuterArchive, fmt.Sprintf("not readable as a tar archive: %v", err))
		return rep, nil
	}
	rep.pass(CheckOuterArchive, "")

	// 2. The documented layout.
	var missing []string
	for _, member := range RequiredSPKMembers {
		if _, ok := outer[member]; !ok {
			missing = append(missing, fmt.Sprintf("missing %s", member))
		}
	}
	rep.record(CheckLayout, missing)

	// 3. INFO.
	info := parseINFO(string(outer[INFOName].Body))
	rep.Arch, rep.Version = info["arch"], info["version"]
	var infoProblems []string
	for _, field := range NecessaryINFOFields {
		if strings.TrimSpace(info[field]) == "" {
			infoProblems = append(infoProblems, fmt.Sprintf("INFO has no %s (it has %v)", field, sortedKeys(info)))
		}
	}
	rep.record(CheckINFO, infoProblems)

	// 4. The declared architecture, and everything that depends on it.
	arch, archErr := ArchForDSM(info["arch"])
	if archErr != nil {
		rep.fail(CheckArch, archErr.Error())
	} else {
		rep.pass(CheckArch, fmt.Sprintf("%s covers %d DSM platforms and needs %s release binaries", arch.DSM, len(arch.Platforms), arch.GOARCH))
	}

	payload, payloadErr := readPayload(outer)
	verifyBinaries(rep, payload, payloadErr, arch, archErr, manifest)
	verifyLauncher(rep, info, payload, payloadErr)
	verifySecretsAndModes(rep, outer, payload)
	verifyLifecycleScripts(rep, outer, payload)

	return rep, nil
}

// verifyBinaries covers the two architecture-sensitive checks: hash
// parity with the release manifest, and the packaged binaries' own ELF
// machine.
func verifyBinaries(rep *Report, payload map[string]archiveEntry, payloadErr error, arch Arch, archErr error, manifest ReleaseManifest) {
	if payloadErr != nil {
		rep.fail(CheckBinaryParity, payloadErr.Error())
		rep.fail(CheckBinaryMachine, payloadErr.Error())
		return
	}
	if archErr != nil {
		detail := fmt.Sprintf("cannot check the packaged binaries: %v", archErr)
		rep.fail(CheckBinaryParity, detail)
		rep.fail(CheckBinaryMachine, detail)
		return
	}

	entry, err := manifest.Arch(arch.GOARCH)
	if err != nil {
		rep.fail(CheckBinaryParity, err.Error())
	}

	var parity, machine []string
	for _, name := range CoreBinaries {
		member, ok := payload[PayloadBinDir+"/"+name]
		if !ok {
			parity = append(parity, fmt.Sprintf("%s carries no %s", PayloadName, name))
			machine = append(machine, fmt.Sprintf("%s carries no %s", PayloadName, name))
			continue
		}

		if err == nil {
			got := sha256Bytes(member.Body)
			want := entry.BinarySHA256[name]
			if got != want {
				parity = append(parity, fmt.Sprintf(
					"%s: packaged SHA-256 %s, but the release manifest records %s for %s. This package was NOT built from the release binary (§3.7 requires the exact same core binary digest).",
					name, got, want, arch.GOARCH))
			}
		}

		f, elfErr := elf.NewFile(bytes.NewReader(member.Body))
		switch {
		case elfErr != nil:
			machine = append(machine, fmt.Sprintf("%s is not an ELF executable: %v", name, elfErr))
		case f.Machine != arch.ELFMachine:
			machine = append(machine, fmt.Sprintf(
				"%s is %s, but INFO declares arch=%s, which needs %s",
				name, f.Machine, arch.DSM, arch.ELFMachine))
		}
	}
	if err == nil {
		rep.record(CheckBinaryParity, parity)
	} else if len(parity) > 0 {
		rep.fail(CheckBinaryParity, strings.Join(parity, "\n"))
	}
	rep.record(CheckBinaryMachine, machine)
}

// verifyLauncher checks that INFO's dsmuidir points at a directory that
// exists in the payload and holds a parseable launcher config naming this
// package's app.
func verifyLauncher(rep *Report, info map[string]string, payload map[string]archiveEntry, payloadErr error) {
	if payloadErr != nil {
		rep.fail(CheckLauncher, payloadErr.Error())
		return
	}
	uidir := info["dsmuidir"]
	if uidir == "" {
		rep.fail(CheckLauncher, "INFO declares no dsmuidir, so DSM has nowhere to read a desktop launcher entry from")
		return
	}
	configPath := uidir + "/config"
	member, ok := payload[configPath]
	if !ok {
		rep.fail(CheckLauncher, fmt.Sprintf("INFO declares dsmuidir=%q but %s carries no %s", uidir, PayloadName, configPath))
		return
	}

	var parsed map[string]map[string]map[string]any
	if err := json.Unmarshal(member.Body, &parsed); err != nil {
		rep.fail(CheckLauncher, fmt.Sprintf("%s is not valid JSON: %v", configPath, err))
		return
	}
	entry, ok := parsed[".url"][info["dsmappname"]]
	if !ok {
		rep.fail(CheckLauncher, fmt.Sprintf("%s has no \".url\" entry for dsmappname %q", configPath, info["dsmappname"]))
		return
	}
	for _, required := range []string{"type", "title", "icon", "url"} {
		if entry[required] == nil {
			rep.fail(CheckLauncher, fmt.Sprintf("the launcher entry in %s has no %q", configPath, required))
			return
		}
	}
	rep.pass(CheckLauncher, fmt.Sprintf("%s opens %v", info["dsmappname"], entry["url"]))
}

// verifySecretsAndModes scans every file in both archives.
func verifySecretsAndModes(rep *Report, outer, payload map[string]archiveEntry) {
	var secrets, modes []string
	scan := func(where string, entries map[string]archiveEntry) {
		for _, name := range sortedKeys(entries) {
			e := entries[name]
			if e.Dir {
				continue
			}
			secrets = append(secrets, scanForSecrets(where+name, e.Body)...)
			if problem := unsafeMode(e.Mode); problem != "" {
				modes = append(modes, fmt.Sprintf("%s%s is %s (mode %04o)", where, name, problem, e.Mode))
			}
		}
	}
	scan("", outer)
	scan(PayloadName+":", payload)
	rep.record(CheckNoSecrets, secrets)
	rep.record(CheckFileModes, modes)
}

// verifyLifecycleScripts runs the unsafe-delete scan over every shell
// script the package actually ships, read out of the built archive
// rather than out of the embedded assets, so a build that packaged
// something else is caught.
//
// Both archives, not just scripts/ in the outer one: assetFiles will
// ship a script dropped into assets/share or assets/ui exactly as
// readily as one in assets/scripts, and postinst already reads a file
// out of share/ at install time.
//
// common.sh comes out of the archive too, for the same reason. It
// defines every path the other scripts delete, so resolving them against
// the pristine embedded copy would report a package whose common.sh
// redefined RUN_DIR to a share path as clean, which is precisely the
// substitution the sentence above claims to catch.
func verifyLifecycleScripts(rep *Report, outer, payload map[string]archiveEntry) {
	shared := ""
	if e, ok := outer[ScriptsDir+"/"+SharedScriptName]; ok {
		shared = string(e.Body)
	}
	var findings []string
	scan := func(where string, entries map[string]archiveEntry) {
		for _, name := range sortedKeys(entries) {
			e := entries[name]
			if e.Dir || !isShellScript(name, e.Body) {
				continue
			}
			findings = append(findings, ScanShippedScript(where+name, string(e.Body), shared)...)
		}
	}
	scan("", outer)
	scan(PayloadName+":", payload)
	rep.record(CheckLifecycle, findings)
}

// isShellScript reports whether an archive member is shell this scanner
// has to read: anything under scripts/, anything named *.sh, and any
// text file whose first line is a #! naming a shell.
func isShellScript(name string, body []byte) bool {
	if strings.HasPrefix(name, ScriptsDir+"/") || strings.HasSuffix(name, ".sh") {
		return true
	}
	if !looksLikeText(body) {
		return false
	}
	first, _, _ := strings.Cut(string(body), "\n")
	if !strings.HasPrefix(first, "#!") {
		return false
	}
	return strings.Contains(first, "sh") || strings.Contains(first, "bash")
}

// orElse is the report's stand-in for a value nobody supplied, so a
// report never prints an empty field a reader has to interpret.
func orElse(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// unsafeMode names the first dangerous bit set on a file mode, or the
// empty string.
//
// Only three are dangerous in a package that DSM unpacks as root: setuid
// and setgid because they create a privilege escalation out of an ordinary
// file, and world-writable because anybody on the NAS can then replace
// something that runs as root. It reports the first rather than all of
// them because the caller's job is to refuse the package, and one reason
// is enough to do that.
func unsafeMode(mode int64) string {
	switch {
	case mode&0o4000 != 0:
		return "setuid"
	case mode&0o2000 != 0:
		return "setgid"
	case mode&0o002 != 0:
		return "world-writable"
	}
	return ""
}

type archiveEntry struct {
	Body []byte
	Mode int64
	Dir  bool
}

// readArchive reads a whole tar into memory, keyed by member name with
// any trailing slash removed so a directory and a file are looked up the
// same way.
func readArchive(r io.Reader) (map[string]archiveEntry, error) {
	out := map[string]archiveEntry{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[strings.TrimSuffix(hdr.Name, "/")] = archiveEntry{
			Body: body,
			Mode: hdr.Mode,
			Dir:  hdr.Typeflag == tar.TypeDir,
		}
	}
}

// readPayload decompresses and reads package.tgz.
func readPayload(outer map[string]archiveEntry) (map[string]archiveEntry, error) {
	member, ok := outer[PayloadName]
	if !ok {
		return nil, fmt.Errorf("the package carries no %s", PayloadName)
	}
	if c := detectCompression(member.Body); c != "gzip" {
		if c == "" {
			c = "uncompressed"
		}
		// xz is what the toolkit's current pkg_get_tar_option produces, so
		// this is a plausible thing to meet rather than a corrupt file.
		// Saying so beats a decoder error nobody can act on.
		return nil, fmt.Errorf("%s is %s; this verifier reads the gzip payload this builder writes (see the package doc for why gzip)", PayloadName, c)
	}
	zr, err := gzip.NewReader(bytes.NewReader(member.Body))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", PayloadName, err)
	}
	defer func() { _ = zr.Close() }()
	entries, err := readArchive(zr)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", PayloadName, err)
	}
	return entries, nil
}

// detectCompression names the compression a byte stream carries, or "" if
// it carries none.
func detectCompression(b []byte) string {
	magics := []struct {
		name  string
		bytes []byte
	}{
		{"gzip", []byte{0x1f, 0x8b}},
		{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}},
		{"bzip2", []byte("BZh")},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
	}
	for _, m := range magics {
		if len(b) >= len(m.bytes) && slices.Equal(b[:len(m.bytes)], m.bytes) {
			return m.name
		}
	}
	return ""
}

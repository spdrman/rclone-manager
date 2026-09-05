package spk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Assembling the `.spk`, deterministically.
//
// Nothing here compiles anything, and that is the constraint everything
// else follows from. §3.7 requires the package to carry the exact release
// binary digest, so a rebuild would be the one thing this must never do:
// Build takes a directory of already-built binaries and a already-built UI
// bundle and wraps them.
//
// Determinism is the other constraint, and it is why every archive member
// is stamped with a fixed epoch rather than the current time, and why INFO
// carries no create_time line even though the Synology toolkit stamps one.
// Two builds of the same inputs have to produce the same bytes, or "this
// package carries the release digest" is a claim nobody downstream can
// independently re-derive, which makes it a claim rather than a fact.
//
// The UI bundle is required rather than optional, and that was a
// deliberate choice against convenience. An optional bundle produces a
// package that installs, runs, and quietly serves the generic bridge,
// which is exactly the defect this arrangement was introduced to fix, and
// nothing about the finished package would say so.

// BuildOptions is everything needed to wrap one architecture's release
// binaries in a `.spk`.
type BuildOptions struct {
	// GOARCH selects both the DSM arch family written into INFO and the
	// release-manifest entry the result must later verify against.
	GOARCH string

	// Version is INFO's `version`.
	Version string

	// BinariesDir holds the ALREADY BUILT release binaries, one file per
	// CoreBinaries entry. Nothing here compiles anything: §3.7 requires
	// the SPK to carry the exact release digest, so a rebuild would be
	// the one thing this package must never do.
	BinariesDir string

	// UIBundleDir holds the ALREADY BUILT shared UI bundle for this
	// provider: ui/shared/dist-bundles/synology, produced by `npm run
	// build:bundles synology`. Required, for the same reason BinariesDir
	// is: the package carries it, it never builds it.
	//
	// Required rather than optional on purpose. An optional bundle would
	// mean a .spk that installs and runs and shows the generic bridge,
	// which is precisely the defect issue #180 was filed about, and
	// nothing about the finished package would say so.
	UIBundleDir string

	// OutDir is where the `.spk` is written.
	OutDir string
}

// epoch is the modification time stamped on every archive member.
//
// A fixed timestamp, not time.Now(): two builds of the same release
// binaries must produce the same bytes, or "this SPK carries the release
// digest" is a claim nobody downstream can independently re-derive. This
// is also why INFO gets no create_time line even though pkg_make_spk
// stamps one.
var epoch = time.Unix(0, 0).UTC()

// Build assembles the package and returns the path it wrote.
func Build(opts BuildOptions) (string, error) {
	arch, err := ArchForGOARCH(opts.GOARCH)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(opts.Version) == "" {
		return "", fmt.Errorf("a package version is required (INFO's `version` key has no default)")
	}
	if opts.OutDir == "" {
		return "", fmt.Errorf("an output directory is required")
	}

	payload, err := stagePayload(opts.BinariesDir, opts.UIBundleDir, arch)
	if err != nil {
		return "", err
	}

	packageTGZ, uncompressed, err := packPayload(payload)
	if err != nil {
		return "", err
	}

	info := renderINFO(infoFields{
		Arch:        arch.DSM,
		Version:     opts.Version,
		ExtractSize: (uncompressed + 1023) / 1024,
		Checksum:    fmt.Sprintf("%x", md5.Sum(packageTGZ)),
	})

	members := []archiveMember{
		{Name: INFOName, Mode: 0o644, Body: []byte(info)},
		{Name: PayloadName, Mode: 0o644, Body: packageTGZ},
	}

	icon64, err := renderIcon(64)
	if err != nil {
		return "", err
	}
	icon256, err := renderIcon(256)
	if err != nil {
		return "", err
	}
	members = append(members,
		archiveMember{Name: IconName, Mode: 0o644, Body: icon64},
		archiveMember{Name: Icon256Name, Mode: 0o644, Body: icon256},
	)

	confFiles, err := assetFiles(ConfDir)
	if err != nil {
		return "", err
	}
	for _, f := range confFiles {
		members = append(members, archiveMember{Name: f.Name, Mode: 0o644, Body: f.Body})
	}

	scriptFiles, err := assetFiles(ScriptsDir)
	if err != nil {
		return "", err
	}
	for _, f := range scriptFiles {
		// 0755 for every scripts/ member including common.sh: DSM runs
		// the stages directly, and a sourced file being executable is
		// harmless where a stage that is not would be a failed install.
		members = append(members, archiveMember{Name: f.Name, Mode: 0o755, Body: f.Body})
	}

	spkBytes, err := writeArchive(members)
	if err != nil {
		return "", err
	}

	name := fmt.Sprintf("%s-%s-%s.spk", PackageName, arch.DSM, opts.Version)
	out := filepath.Join(opts.OutDir, name)
	if err := os.WriteFile(out, spkBytes, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", out, err)
	}
	return out, nil
}

// stagePayload reads the release binaries and the shipped UI/share assets
// into the members of package.tgz.
func stagePayload(binariesDir, uiBundleDir string, arch Arch) ([]archiveMember, error) {
	if binariesDir == "" {
		return nil, fmt.Errorf("a directory of release binaries is required: this package wraps them, it never builds them")
	}

	var members []archiveMember
	for _, name := range CoreBinaries {
		p := filepath.Join(binariesDir, name)
		body, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read release binary %s: %w", name, err)
		}
		if err := checkELF(name, body, arch); err != nil {
			return nil, err
		}
		members = append(members, archiveMember{
			Name: PayloadBinDir + "/" + name,
			Mode: 0o755,
			Body: body,
		})
	}

	for _, dir := range []string{DSMUIDir, PayloadShareDir} {
		files, err := assetFiles(dir)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			members = append(members, archiveMember{
				Name: f.Name,
				Mode: 0o644,
				Body: f.Body,
			})
		}
	}

	bundle, err := stageUIBundle(uiBundleDir)
	if err != nil {
		return nil, err
	}
	members = append(members, bundle...)

	icons, err := renderLauncherIcons()
	if err != nil {
		return nil, err
	}
	for _, icon := range icons {
		members = append(members, archiveMember{
			Name: DSMUIDir + "/images/" + icon.Name,
			Mode: 0o644,
			Body: icon.Body,
		})
	}

	return members, nil
}

// stageUIBundle reads the provider's shared UI bundle into package.tgz
// members, refusing anything that is not one.
//
// Three refusals, and each of them is a failure that would otherwise ship
// silently: no directory at all (a package serving the generic bridge on
// a Synology NAS, which is #180), no app shell (a bundle directory that
// is really an empty mount point, which answers every route with 404),
// and a marker naming another provider (the wrong bridge, which looks
// exactly like the right one until a capability is used).
func stageUIBundle(dir string) ([]archiveMember, error) {
	if dir == "" {
		return nil, fmt.Errorf("a built UI bundle directory is required (ui/shared/dist-bundles/%s, from `npm run build:bundles %s`): without it this package would serve the generic bridge on a Synology NAS, which is issue #180",
			UIBundlePlatform, UIBundlePlatform)
	}

	marker, err := os.ReadFile(filepath.Join(dir, UIBundleMarkerName))
	if err != nil {
		return nil, fmt.Errorf("read %s from the UI bundle at %s: %w", UIBundleMarkerName, dir, err)
	}
	var m struct {
		Platform string `json:"platform"`
	}
	if err := json.Unmarshal(marker, &m); err != nil {
		return nil, fmt.Errorf("parse %s from the UI bundle at %s: %w", UIBundleMarkerName, dir, err)
	}
	if m.Platform != UIBundlePlatform {
		return nil, fmt.Errorf("the UI bundle at %s was built for %q, and this package needs %q; a bundle for the wrong provider installs cleanly and shows the wrong bridge",
			dir, m.Platform, UIBundlePlatform)
	}

	var members []archiveMember
	shell := false
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "index.html" {
			shell = true
		}
		members = append(members, archiveMember{
			Name: PayloadUIBundleDir + "/" + rel,
			Mode: 0o644,
			Body: body,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the UI bundle at %s: %w", dir, err)
	}
	if !shell {
		return nil, fmt.Errorf("the UI bundle at %s has no index.html, so it is not an app shell; serving it would answer every route with 404", dir)
	}

	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members, nil
}

// checkELF refuses anything that is not a Linux executable for this
// architecture. Build is strict about this even though Verify checks it
// again independently: catching it here names the file on the build host,
// where it can still be fixed, rather than in a report about a finished
// package.
func checkELF(name string, body []byte, arch Arch) error {
	f, err := elf.NewFile(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("release binary %s is not an ELF executable: %w", name, err)
	}
	if f.Machine != arch.ELFMachine {
		return fmt.Errorf("release binary %s is %s, but a %s package needs %s",
			name, f.Machine, arch.DSM, arch.ELFMachine)
	}
	return nil
}

// packPayload writes package.tgz and returns it alongside the total
// uncompressed size INFO's extractsize reports.
func packPayload(members []archiveMember) ([]byte, int64, error) {
	raw, err := writeArchive(members)
	if err != nil {
		return nil, 0, err
	}

	var total int64
	for _, m := range members {
		total += int64(len(m.Body))
	}

	var buf bytes.Buffer
	// BestCompression rather than the default, and no gzip header name or
	// timestamp, so the output is both small and identical across builds.
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, 0, fmt.Errorf("open gzip writer: %w", err)
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, 0, fmt.Errorf("compress %s: %w", PayloadName, err)
	}
	if err := zw.Close(); err != nil {
		return nil, 0, fmt.Errorf("close %s: %w", PayloadName, err)
	}
	return buf.Bytes(), total, nil
}

type archiveMember struct {
	Name string
	Mode int64
	Body []byte
}

// writeArchive tars members, plus a directory entry for every directory
// they imply, in sorted order.
//
// Uncompressed, matching pkgscripts-ng's own `pkg_make_spk` (`tar cf`)
// for the outer `.spk`; packPayload is what adds gzip for the inner
// payload. USTAR format, fixed epoch, and uid/gid 0 with empty user and
// group names, all so the bytes depend only on the inputs.
func writeArchive(members []archiveMember) ([]byte, error) {
	dirs := map[string]bool{}
	for _, m := range members {
		for d := path.Dir(m.Name); d != "." && d != "/" && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}

	type entry struct {
		name  string
		isDir bool
		body  []byte
		mode  int64
	}
	var entries []entry
	for d := range dirs {
		entries = append(entries, entry{name: d, isDir: true, mode: 0o755})
	}
	for _, m := range members {
		entries = append(entries, entry{name: m.Name, body: m.Body, mode: m.Mode})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			ModTime:  epoch,
			Format:   tar.FormatUSTAR,
			Typeflag: tar.TypeReg,
		}
		if e.isDir {
			hdr.Name = e.name + "/"
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("write tar header %s: %w", e.name, err)
		}
		if !e.isDir {
			if _, err := tw.Write(e.body); err != nil {
				return nil, fmt.Errorf("write tar body %s: %w", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	return buf.Bytes(), nil
}

// sha256File hashes one file, hex-encoded, the same way
// scripts/release/record-release-hashes.sh does.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// infoFields are the values that vary per build; everything else in INFO
// is a constant from layout.go.
type infoFields struct {
	Arch        string
	Version     string
	ExtractSize int64
	Checksum    string
}

// renderINFO writes the INFO file.
//
// Field order is fixed rather than map-ordered so two builds produce
// identical bytes. Every key below is one Synology documents: the six
// necessary fields, then the optional ones this package actually needs.
// Nothing speculative is emitted - in particular no `thirdparty` (DSM
// 4.0-4.3 only), no `toolkit_version` and no `create_time`.
func renderINFO(f infoFields) string {
	lines := [][2]string{
		{"package", PackageName},
		{"version", f.Version},
		{"os_min_ver", OSMinVer},
		{"description", Description},
		{"arch", f.Arch},
		{"maintainer", Maintainer},
		{"displayname", DisplayName},
		// adminport/adminprotocol/adminurl are the documented way to point
		// Package Center's own Open action at a port the package serves
		// itself. dsmuidir/dsmappname are the documented way to put an
		// entry on the DSM desktop. Both are set because they are
		// different surfaces, and which of them a given DSM build
		// actually renders is one of the things the hardware acceptance
		// procedure records.
		{"adminport", fmt.Sprintf("%d", UIPort)},
		{"adminprotocol", "http"},
		{"adminurl", "/"},
		{"checkport", "yes"},
		{"dsmuidir", DSMUIDir},
		{"dsmappname", DSMAppName},
		{"ctl_stop", "yes"},
		{"ctl_uninstall", "yes"},
		{"extractsize", fmt.Sprintf("%d", f.ExtractSize)},
		{"checksum", f.Checksum},
	}
	var b strings.Builder
	for _, kv := range lines {
		fmt.Fprintf(&b, "%s=%q\n", kv[0], kv[1])
	}
	return b.String()
}

// parseINFO reads an INFO file back into key/value pairs.
func parseINFO(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = unquote(strings.TrimSpace(v))
	}
	return out
}

// sortedKeys is a small helper for deterministic error messages.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

package spk

import (
	"debug/elf"
	"strings"
	"testing"
)

// mustCheck returns the named check from a report, failing the test if
// the report does not contain it at all — a check that silently stopped
// running is indistinguishable from one that passed, and that is exactly
// the failure mode these controls exist to rule out.
func mustCheck(t *testing.T, rep *Report, name string) Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report has no check %q; it ran: %v", name, rep.CheckNames())
	return Check{}
}

func requirePass(t *testing.T, rep *Report, name string) {
	t.Helper()
	if c := mustCheck(t, rep, name); !c.OK {
		t.Fatalf("check %q failed unexpectedly: %s", name, c.Detail)
	}
}

func requireFail(t *testing.T, rep *Report, name, wantSubstring string) {
	t.Helper()
	c := mustCheck(t, rep, name)
	if c.OK {
		t.Fatalf("check %q passed, but this input was built to fail it", name)
	}
	if !strings.Contains(c.Detail, wantSubstring) {
		t.Fatalf("check %q failed with %q, which does not mention %q", name, c.Detail, wantSubstring)
	}
}

// TestVerify_AcceptsAFreshlyBuiltPackage is the baseline every control
// below is measured against: without it, a control that "fails" proves
// nothing, because the verifier might reject everything.
func TestVerify_AcceptsAFreshlyBuiltPackage(t *testing.T) {
	for _, goarch := range []string{"amd64", "arm64"} {
		t.Run(goarch, func(t *testing.T) {
			path, manifest := buildFixture(t, goarch)
			rep, err := Verify(path, manifest)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !rep.OK() {
				t.Fatalf("a freshly built package failed verification:\n%s", rep)
			}
		})
	}
}

// TestVerify_BinaryHashParity is issue #85's RED requirement and §3.7's
// "the exact same core binary digest" made checkable: the bytes inside
// package.tgz must hash to the release manifest's per-architecture
// SHA-256, and a package built from anything else must be rejected.
func TestVerify_BinaryHashParity(t *testing.T) {
	tests := []struct {
		name string
		// build produces the .spk to check and the manifest to check it
		// against. Returning both lets a case deliberately disagree.
		build         func(t *testing.T) (string, ReleaseManifest)
		wantOK        bool
		wantSubstring string
	}{
		{
			name: "packaged binaries match the release manifest",
			build: func(t *testing.T) (string, ReleaseManifest) {
				return buildFixture(t, "amd64")
			},
			wantOK: true,
		},
		{
			// The positive control. The package is assembled exactly the
			// same way, from binaries that differ from the released ones
			// by one byte of payload — the shape a rebuild-instead-of-
			// repackage mistake actually takes.
			name: "one packaged binary is a rebuild, not the release binary",
			build: func(t *testing.T) (string, ReleaseManifest) {
				release := stagedBinaries(t, "amd64", "release")
				manifest := manifestFor(t, "amd64", release)

				rebuild := stagedBinaries(t, "amd64", "rebuilt")
				path, err := Build(BuildOptions{
					GOARCH:      "amd64",
					Version:     manifest.Version,
					BinariesDir: rebuild,
					OutDir:      t.TempDir(),
				})
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				return path, manifest
			},
			wantOK:        false,
			wantSubstring: "backup-manager",
		},
		{
			// A subtler control: the package is right, but it is checked
			// against the OTHER architecture's recorded hashes. A verifier
			// that hashed the file and compared it to "any entry in the
			// manifest" would pass this, and would therefore never catch
			// an amd64 binary shipped inside an armv8 package.
			name: "manifest has no entry for the package's own architecture",
			build: func(t *testing.T) (string, ReleaseManifest) {
				path, manifest := buildFixture(t, "arm64")
				manifest.Architectures[0].Architecture = "amd64"
				return path, manifest
			},
			wantOK:        false,
			wantSubstring: "arm64",
		},
		{
			// The binary bytes are swapped inside an already-built package
			// rather than at build time, which is what tampering after the
			// fact looks like.
			name: "a packaged binary is replaced after the package was built",
			build: func(t *testing.T) (string, ReleaseManifest) {
				path, manifest := buildFixture(t, "amd64")
				tampered := mutateInnerPayload(t, path, func(inner []tarEntry) []tarEntry {
					return replaceBody(inner, PayloadBinDir+"/backup-manager-web",
						fakeELF(elf.EM_X86_64, []byte("tampered")))
				})
				return tampered, manifest
			},
			wantOK:        false,
			wantSubstring: "backup-manager-web",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, manifest := tc.build(t)
			rep, err := Verify(path, manifest)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if tc.wantOK {
				requirePass(t, rep, CheckBinaryParity)
				return
			}
			requireFail(t, rep, CheckBinaryParity, tc.wantSubstring)
		})
	}
}

// TestVerify_RejectsWrongELFMachine covers the other half of "the exact
// binary for the target architecture": hashes could agree with a manifest
// that itself recorded the wrong file, so the packaged binary's own ELF
// machine field is checked against the architecture INFO declares.
func TestVerify_RejectsWrongELFMachine(t *testing.T) {
	release := stagedBinaries(t, "arm64", "release")
	manifest := manifestFor(t, "arm64", release)

	// Same manifest, but the package is assembled for armv8 out of x86-64
	// binaries: hash parity is satisfied only if the manifest is rebuilt
	// from them, so this control deliberately rebuilds it that way and
	// leans entirely on the ELF check.
	wrong := stagedBinaries(t, "amd64", "release")
	manifest = manifestFor(t, "arm64", wrong)
	path, err := Build(BuildOptions{
		GOARCH:      "arm64",
		Version:     manifest.Version,
		BinariesDir: wrong,
		OutDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	rep, err := Verify(path, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requirePass(t, rep, CheckBinaryParity) // the hashes really do agree
	requireFail(t, rep, CheckBinaryMachine, "EM_X86_64")
}

// TestVerify_RequiresTheDocumentedLayout walks every member the Synology
// package structure documents as mandatory and removes exactly one, so
// each required-member assertion gets its own control rather than sharing
// one.
func TestVerify_RequiresTheDocumentedLayout(t *testing.T) {
	path, manifest := buildFixture(t, "amd64")

	for _, member := range RequiredSPKMembers {
		t.Run(member, func(t *testing.T) {
			broken := mutateSPK(t, path, func(entries []tarEntry) []tarEntry {
				return dropEntry(entries, member)
			})
			rep, err := Verify(broken, manifest)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			requireFail(t, rep, CheckLayout, member)
		})
	}
}

// TestVerify_RejectsCompressedOuterArchive pins the outer container's
// format: pkgscripts-ng's own pkg_make_spk packs the .spk with `tar cf`,
// uncompressed, and DSM reads it that way.
func TestVerify_RejectsCompressedOuterArchive(t *testing.T) {
	path, manifest := buildFixture(t, "amd64")

	rep, err := Verify(path, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requirePass(t, rep, CheckOuterArchive)

	gzipped := gzipFile(t, path)
	rep, err = Verify(gzipped, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requireFail(t, rep, CheckOuterArchive, "gzip")
}

// TestVerify_INFONecessaryFields checks the six fields Synology documents
// as necessary, one control each.
func TestVerify_INFONecessaryFields(t *testing.T) {
	path, manifest := buildFixture(t, "amd64")

	for _, field := range NecessaryINFOFields {
		t.Run(field, func(t *testing.T) {
			broken := mutateSPK(t, path, func(entries []tarEntry) []tarEntry {
				var kept []string
				for _, line := range strings.Split(string(infoBody(t, entries)), "\n") {
					if !strings.HasPrefix(line, field+"=") {
						kept = append(kept, line)
					}
				}
				return replaceBody(entries, INFOName, []byte(strings.Join(kept, "\n")))
			})
			rep, err := Verify(broken, manifest)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			requireFail(t, rep, CheckINFO, field)
		})
	}
}

// TestVerify_RejectsUnclaimedArchitecture keeps the package honest about
// §68's "a representative DSM 7.x model for each architecture claimed":
// an arch value outside the two families this project actually builds for
// would silently widen the claim.
func TestVerify_RejectsUnclaimedArchitecture(t *testing.T) {
	path, manifest := buildFixture(t, "amd64")

	broken := mutateSPK(t, path, func(entries []tarEntry) []tarEntry {
		body := strings.Replace(string(infoBody(t, entries)),
			`arch="x86_64"`, `arch="noarch"`, 1)
		return replaceBody(entries, INFOName, []byte(body))
	})
	rep, err := Verify(broken, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requireFail(t, rep, CheckArch, "noarch")
}

// TestVerify_RejectsBundledSecret is §72's "no bundled secrets" gate.
// Every string below is a detector fixture: a recognisable header with no
// key material behind it, never a real or usable credential.
func TestVerify_RejectsBundledSecret(t *testing.T) {
	tests := []struct {
		name   string
		inner  bool // inject into package.tgz rather than the outer .spk
		file   string
		body   string
		detail string
	}{
		{
			name:   "private key left in the package payload",
			inner:  true,
			file:   PayloadRoot + "/etc/id_ed25519",
			body:   "-----BEGIN OPENSSH PRIVATE KEY-----\nNOT-A-REAL-KEY\n-----END OPENSSH PRIVATE KEY-----\n",
			detail: "id_ed25519",
		},
		{
			name:   "RSA key header anywhere in the outer package",
			file:   "LICENSE",
			body:   "-----BEGIN RSA PRIVATE KEY-----\nNOT-A-REAL-KEY\n",
			detail: "LICENSE",
		},
		{
			name:   "an environment file carrying a filled-in password",
			inner:  true,
			file:   PayloadRoot + "/etc/app.env",
			body:   "LISTEN_ADDR=:8477\nADMIN_PASSWORD=hunter2\n",
			detail: "app.env",
		},
		{
			name:   "an API token baked into a lifecycle script",
			file:   ScriptsDir + "/postinst",
			body:   "#!/bin/sh\nexport API_TOKEN=ghp_000000000000000000000000000000000000\n",
			detail: "postinst",
		},
	}

	path, manifest := buildFixture(t, "amd64")

	// Baseline first: the package as built must pass, or every case below
	// is meaningless.
	rep, err := Verify(path, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requirePass(t, rep, CheckNoSecrets)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutate := mutateSPK
			if tc.inner {
				mutate = mutateInnerPayload
			}
			bad := mutate(t, path, func(entries []tarEntry) []tarEntry {
				return addEntry(dropEntry(entries, tc.file), tc.file, []byte(tc.body))
			})
			rep, err := Verify(bad, manifest)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			requireFail(t, rep, CheckNoSecrets, tc.detail)
		})
	}
}

// TestVerify_RejectsDangerousFileModes: nothing in a package that runs as
// the DSM package user has any business being setuid, setgid or
// world-writable.
func TestVerify_RejectsDangerousFileModes(t *testing.T) {
	path, manifest := buildFixture(t, "amd64")

	rep, err := Verify(path, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requirePass(t, rep, CheckFileModes)

	bad := mutateInnerPayload(t, path, func(inner []tarEntry) []tarEntry {
		for i := range inner {
			if inner[i].hdr.Name == PayloadBinDir+"/backup-manager" {
				inner[i].hdr.Mode = 0o4755
			}
		}
		return inner
	})
	rep, err = Verify(bad, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requireFail(t, rep, CheckFileModes, "backup-manager")
}

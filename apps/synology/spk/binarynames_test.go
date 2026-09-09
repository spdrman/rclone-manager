package spk

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The three halves of a binary name, and why they need holding together.
//
// CoreBinaries decides what stagePayload writes into bin/. conf/privilege
// names the same files again, by relative path, so DSM knows what to chmod
// and who to run them as. start-stop-status names them a third time, as
// the argv[0] it spawns, and common.sh names them a fourth, as the string
// pid_alive matches a running process against. Nothing compiles the last
// three: they are shipped bytes, embedded verbatim, and Go never reads
// them as code.
//
// So a rename that moves the Go constant and leaves a script behind
// produces a package that installs bin/rbm-web, grants privilege to a
// file that is not there, spawns a file that is not there, and then looks
// for a process that never started. Every existing test passes through
// that, and TestPidAlive_ChecksIdentityNotExistence passes hardest,
// because it creates its own fixture at the path it then looks for: it is
// self-consistent by construction and can never notice that the script
// which SPAWNS the process names something else.
//
// That has now happened three times in one rename. This file is the rule
// that makes the fourth loud: every bin/ path any shipped file names has
// to be a file CoreBinaries actually installs.

// binRef matches a reference to something inside the package's bin/
// directory, in any of the spellings the shipped scripts use for it.
// common.sh assigns PKG_BIN from SYNOPKG_PKGDEST, so both the short and
// the long form have to be recognised; a rename that reached only one
// spelling is exactly the failure mode here.
var binRef = regexp.MustCompile(`(?:\$\{PKG_BIN\}|\$PKG_BIN|\$\{SYNOPKG_PKGDEST\}/bin|\$SYNOPKG_PKGDEST/bin)/([A-Za-z0-9._+-]+)`)

// binaryReference is one shipped file's claim about a file in bin/.
type binaryReference struct {
	File string // the shipped path, e.g. "scripts/start-stop-status"
	Name string // the bin/ member it names, e.g. "rbm-web"
}

// binaryReferencesIn pulls every bin/ reference out of one shipped file.
// Scripts are read with binRef; conf/privilege is read as the JSON it is,
// because a relpath is a structured field rather than a shell word and a
// regex over it would be reading the wrong thing.
func binaryReferencesIn(name, body string) ([]binaryReference, error) {
	if path.Base(name) == "privilege" {
		var doc struct {
			Tool []struct {
				RelPath string `json:"relpath"`
			} `json:"tool"`
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w", name, err)
		}
		var out []binaryReference
		for _, t := range doc.Tool {
			dir, file := path.Split(t.RelPath)
			if strings.Trim(dir, "/") != PayloadBinDir {
				continue
			}
			out = append(out, binaryReference{File: name, Name: file})
		}
		return out, nil
	}

	var out []binaryReference
	for _, m := range binRef.FindAllStringSubmatch(body, -1) {
		out = append(out, binaryReference{File: name, Name: m[1]})
	}
	return out, nil
}

// checkBinaryNames is the rule itself, over a set of shipped files. It is
// a function of its argument rather than of the embedded assets so that
// the control below can hand it a deliberately wrong tree and watch it
// fail; a rule only ever run against a passing input is a comment.
func checkBinaryNames(files map[string]string) (refs []binaryReference, violations []string, err error) {
	installed := make(map[string]bool, len(CoreBinaries))
	for _, b := range CoreBinaries {
		installed[b] = true
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		found, err := binaryReferencesIn(name, files[name])
		if err != nil {
			return nil, nil, err
		}
		for _, ref := range found {
			refs = append(refs, ref)
			if !installed[ref.Name] {
				violations = append(violations, fmt.Sprintf(
					"%s names %s/%s, which stagePayload never writes: CoreBinaries installs %s",
					ref.File, PayloadBinDir, ref.Name, strings.Join(CoreBinaries, " and ")))
			}
		}
	}
	return refs, violations, nil
}

// shippedFilesNamingBinaries is every file that gets to make a claim about
// bin/: all of scripts/ (the lifecycle stages and the common.sh they
// source) and conf/, which is where privilege lives.
func shippedFilesNamingBinaries(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{"scripts", "conf"} {
		files, err := assetFiles(dir)
		if err != nil {
			t.Fatalf("read %s assets: %v", dir, err)
		}
		for _, f := range files {
			out[f.Name] = string(f.Body)
		}
	}
	if len(out) == 0 {
		t.Fatal("no shipped files were read at all, so every check below would pass vacuously")
	}
	return out
}

// TestEveryShippedFileNamesABinaryThePackageInstalls is the guard: the
// installer, the privilege declaration, the start script and the liveness
// check have to agree on what the two executables are called.
func TestEveryShippedFileNamesABinaryThePackageInstalls(t *testing.T) {
	files := shippedFilesNamingBinaries(t)

	refs, violations, err := checkBinaryNames(files)
	if err != nil {
		t.Fatalf("read the shipped files: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("shipped files name binaries this package does not install:\n  %s",
			strings.Join(violations, "\n  "))
	}

	// Vacuity controls. A clean result here is only worth something if
	// the walk actually reached both kinds of claim, and both have been
	// wrong in this repository within one release.
	var scriptRefs, privilegeRefs int
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r.File] = true
		if path.Base(r.File) == "privilege" {
			privilegeRefs++
		} else {
			scriptRefs++
		}
	}
	if scriptRefs == 0 {
		t.Error("no shipped script names anything under bin/, so the spawn half of this rule is not running")
	}
	if privilegeRefs == 0 {
		t.Error("conf/privilege grants nothing under bin/, so the privilege half of this rule is not running")
	}
	if len(seen) < 2 {
		t.Errorf("only %d shipped file made a claim about bin/; the three-way disagreement this test exists for needs at least two", len(seen))
	}
}

// TestTheBinaryNameGuardFailsOnAScriptThatMovedAlone is the control, and
// it reproduces the exact incident: one shipped script left naming the old
// binary while CoreBinaries moved on. Without it, the test above is a
// walk that has only ever seen agreement and could not tell agreement
// from not looking.
func TestTheBinaryNameGuardFailsOnAScriptThatMovedAlone(t *testing.T) {
	good := shippedFilesNamingBinaries(t)
	if _, violations, err := checkBinaryNames(good); err != nil || len(violations) > 0 {
		t.Fatalf("the shipped tree is not clean, so this control cannot tell a planted fault from a real one: err=%v violations=%v", err, violations)
	}

	for _, tc := range []struct {
		name    string
		file    string
		old     string
		planted string
	}{
		{
			name:    "the start script spawns a binary the payload does not carry",
			file:    "scripts/start-stop-status",
			old:     "${PKG_BIN}/" + CoreBinaries[1],
			planted: "${PKG_BIN}/rbm-web",
		},
		{
			name:    "the liveness check watches for a binary the payload does not carry",
			file:    "scripts/" + SharedScriptName,
			old:     "${PKG_BIN}/" + CoreBinaries[1],
			planted: "${PKG_BIN}/rbm-web",
		},
		{
			name:    "conf/privilege grants to a file the payload does not carry",
			file:    "conf/privilege",
			old:     `"` + PayloadBinDir + "/" + CoreBinaries[1] + `"`,
			planted: `"` + PayloadBinDir + `/rbm-web"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := make(map[string]string, len(good))
			for k, v := range good {
				mutated[k] = v
			}
			body, ok := mutated[tc.file]
			if !ok {
				t.Fatalf("%s is not a shipped file, so this control mutates nothing", tc.file)
			}
			if !strings.Contains(body, tc.old) {
				t.Fatalf("%s does not contain %q, so the substitution below is a no-op and this control proves nothing", tc.file, tc.old)
			}
			mutated[tc.file] = strings.Replace(body, tc.old, tc.planted, 1)

			_, violations, err := checkBinaryNames(mutated)
			if err != nil {
				t.Fatalf("check the mutated tree: %v", err)
			}
			if len(violations) == 0 {
				t.Fatalf("%s was left naming a binary CoreBinaries does not install and the rule said nothing", tc.file)
			}
			if !strings.Contains(violations[0], tc.file) {
				t.Errorf("the finding does not name the file that is wrong, so it cannot be acted on: %s", violations[0])
			}
		})
	}
}

// docsBinRef matches the extraction targets apps/synology/README.md's
// build procedure writes: `docker cp "${cid}:/rbm" release/amd64/rbm`.
// BinariesDir is read by CoreBinaries name, so a procedure that lands the
// files under any other name produces a directory the packer cannot use,
// and it says so only at build time.
var docsBinRef = regexp.MustCompile(`release/[A-Za-z0-9_]+/([A-Za-z0-9._+-]+)`)

// TestTheDocumentedBuildProcedureNamesTheBinariesThePackerReads closes the
// fourth copy of the same name. conf/privilege, start-stop-status and
// common.sh are the three the guard above holds together; the README is
// where an operator reads what to call the files in the first place, and
// it drifted too: it said `release/amd64/rclone-manager` while
// stagePayload had been reading BinariesDir by CoreBinaries name for a
// release already.
func TestTheDocumentedBuildProcedureNamesTheBinariesThePackerReads(t *testing.T) {
	readme := repoText(t, "apps/synology/README.md")

	installed := make(map[string]bool, len(CoreBinaries))
	for _, b := range CoreBinaries {
		installed[b] = true
	}

	checked := 0
	for _, m := range docsBinRef.FindAllStringSubmatch(readme, -1) {
		checked++
		if !installed[m[1]] {
			t.Errorf("the build procedure extracts a binary to release/.../%s, which BinariesDir never reads: stagePayload looks for %s",
				m[1], strings.Join(CoreBinaries, " and "))
		}
	}
	// The same rule for the installed path the README quotes back.
	for _, m := range binRef.FindAllStringSubmatch(readme, -1) {
		checked++
		if !installed[m[1]] {
			t.Errorf("the README says DSM starts %s, which this package never installs: stagePayload writes %s",
				m[1], strings.Join(CoreBinaries, " and "))
		}
	}
	// Positive control: a walk that reads nothing proves nothing, and
	// this one reads a document that could be reworded out from under it.
	if checked < 3 {
		t.Fatalf("only %d binary name was found in apps/synology/README.md, so this check is vacuous; the build procedure extracts two and the runtime section names one", checked)
	}
}

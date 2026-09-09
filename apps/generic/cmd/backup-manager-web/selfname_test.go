package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/cliecho"
)

// oldName is built from two halves deliberately: spelled whole, this
// file would contain the exact literal it exists to find and would
// report itself on every run.
const oldName = "backup-manager" + "-web"

// The web host used to spell its own name 37 times as a literal, while
// cliecho.WebBinary sat one import away holding the same value. That is
// the drift cliname.go exists to prevent, and it was live: every compose
// file, the Dockerfile, the installer and the docs said rbm-web, and the
// process an operator actually watches answered
// "backup-manager-web: runtime profile ...".
//
// Deriving the strings fixed it but cannot guard it, because a test that
// also derives from the constant moves with it and passes either way.
// What is checked here is the thing that would actually regress: a
// literal creeping back into the source, and the shipped binary printing
// a name that is not the constant.

func TestTheWebHostNamesItselfFromTheConstant(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rbm-web")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	run := exec.Command(bin, "no-such-command")
	out, _ := run.CombinedOutput()
	got := string(out)

	if !strings.Contains(got, cliecho.WebBinary+": unknown command") {
		t.Errorf("the diagnostic does not name cliecho.WebBinary (%q):\n%s", cliecho.WebBinary, got)
	}
	if !strings.Contains(got, "usage: "+cliecho.WebBinary+" <command>") {
		t.Errorf("the usage line does not name cliecho.WebBinary (%q):\n%s", cliecho.WebBinary, got)
	}
	// The old name is a symlink in the image and a Go package path here,
	// so it is legitimate in both of those places and in neither of the
	// two lines above.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, oldName+":") || strings.HasPrefix(line, "usage: "+oldName) {
			t.Errorf("the binary still spells the old name in an operator-facing line: %q", line)
		}
	}
}

func TestNoSourceFileSpellsTheWebHostsNameAsALiteral(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		for i, line := range strings.Split(string(b), "\n") {
			// A doc comment may name the command and the Go package
			// path; a format or argument string may not.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, `"`+oldName+": ") ||
				strings.Contains(line, "usage: "+oldName) {
				t.Errorf("%s:%d spells the web host's name as a literal, use cliecho.WebBinary: %s",
					e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
	// Positive control: a walk that reads nothing proves nothing.
	if checked < 2 {
		t.Fatalf("only read %d .go files in this package, so the check above is vacuous", checked)
	}
}

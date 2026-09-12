package backupengine_test

import (
	"os"
	"strings"
	"testing"
)

// The vendor name is assembled rather than written so this file can search
// for it without containing a match of its own, the same trick
// internal/placement's guard needs for the word it keeps out of production
// code.
var vendorName = "ko" + "pia"

// TestEngineFileNamesNoVendor is the enforcement half of the package doc's
// claim that no embedded-engine type appears in any signature in engine.go.
//
// A type cannot appear in a signature without its package qualifier or its
// import path appearing in the file, and both contain the vendor's name. So
// "engine.go does not contain that string" is a complete, cheap check of a
// claim that would otherwise be maintained by hope: the failure mode this
// catches is somebody adding a convenient parameter of an upstream type, in
// a hurry, because it was right there.
func TestEngineFileNamesNoVendor(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("reading engine.go: %v", err)
	}

	text := strings.ToLower(string(src))

	if strings.Contains(text, vendorName) {
		t.Errorf("engine.go mentions %q; the embedded engine must stay inside the adapter subpackage", vendorName)
	}

	// The boundary is only meaningful if the file it protects is the one
	// carrying the interface, so fail loudly if engine.go ever stops being
	// that file rather than passing vacuously.
	for _, want := range []string{"type Engine interface", "type Repository interface"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("engine.go no longer declares %q; this guard is checking the wrong file", want)
		}
	}
}

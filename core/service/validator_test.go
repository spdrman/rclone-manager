package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// TestResolveValidator_RejectsUnregisteredIdentifiers is WP3.2's RED proof
// for §26 Step 5's "Do not permit arbitrary shell commands from the
// browser": a raw executable path, a path-traversal attempt, an empty
// string and a plausible-looking-but-unregistered name must all be
// refused exactly the same way, with ErrUnregisteredValidator, never a
// resolved config.Command.
func TestResolveValidator_RejectsUnregisteredIdentifiers(t *testing.T) {
	for _, id := range []ValidatorID{
		"/bin/rm",
		"/bin/sh -c 'rm -rf /'",
		"../../etc/passwd",
		"",
		"not-a-real-validator",
		"trailer-marker.sh", // the script's own filename is not its ID
	} {
		t.Run(string(id), func(t *testing.T) {
			cmd, err := ResolveValidator(id)
			if !errors.Is(err, ErrUnregisteredValidator) {
				t.Fatalf("ResolveValidator(%q) error = %v, want ErrUnregisteredValidator", id, err)
			}
			if cmd != (config.Command{}) {
				t.Fatalf("ResolveValidator(%q) = %+v, want the zero value on refusal", id, cmd)
			}
		})
	}
}

// TestResolveValidator_RegisteredIdentifierResolvesToAnExecutable proves
// the other half: a registered identifier resolves to a real,
// independently-runnable config.Command this package built itself, never
// anything derived from what the caller passed in.
func TestResolveValidator_RegisteredIdentifierResolvesToAnExecutable(t *testing.T) {
	cmd, err := ResolveValidator(ValidatorTrailerMarker)
	if err != nil {
		t.Fatalf("ResolveValidator: %v", err)
	}
	if !filepath.IsAbs(cmd.Executable) {
		t.Fatalf("Executable = %q, want an absolute path", cmd.Executable)
	}
	info, err := os.Stat(cmd.Executable)
	if err != nil {
		t.Fatalf("materialized script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("materialized script %s is not executable: mode %v", cmd.Executable, info.Mode())
	}
	if cmd.Timeout.Duration() <= 0 {
		t.Fatalf("Timeout = %s, want a positive duration", cmd.Timeout.Duration())
	}
}

// TestResolveValidator_EveryRegisteredValidatorResolves guards against the
// catalog and RegisteredValidators drifting apart: every id
// RegisteredValidators advertises must actually resolve.
func TestResolveValidator_EveryRegisteredValidatorResolves(t *testing.T) {
	ids := RegisteredValidators()
	if len(ids) == 0 {
		t.Fatal("RegisteredValidators() is empty")
	}
	for _, id := range ids {
		if _, err := ResolveValidator(id); err != nil {
			t.Errorf("ResolveValidator(%q): %v", id, err)
		}
	}
}

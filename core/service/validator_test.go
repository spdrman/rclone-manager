package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// TestResolveValidator_RejectsUnregisteredIdentifiers is WP3.2's RED proof
// for §26 Step 5's "Do not permit arbitrary shell commands from the
// browser": a raw executable path, a path-traversal attempt, an empty
// string and a plausible-looking-but-unregistered name must all be
// refused exactly the same way, with errUnregisteredValidator, never a
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
			cmd, err := resolveValidator(id)
			if !errors.Is(err, errUnregisteredValidator) {
				t.Fatalf("resolveValidator(%q) error = %v, want errUnregisteredValidator", id, err)
			}
			if cmd != (config.Command{}) {
				t.Fatalf("resolveValidator(%q) = %+v, want the zero value on refusal", id, cmd)
			}
		})
	}
}

// TestResolveValidator_RegisteredIdentifierResolvesToAnExecutable proves
// the other half: a registered identifier resolves to a real,
// independently-runnable config.Command this package built itself, never
// anything derived from what the caller passed in.
func TestResolveValidator_RegisteredIdentifierResolvesToAnExecutable(t *testing.T) {
	cmd, err := resolveValidator(ValidatorTrailerMarker)
	if err != nil {
		t.Fatalf("resolveValidator: %v", err)
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
		if _, err := resolveValidator(id); err != nil {
			t.Errorf("resolveValidator(%q): %v", id, err)
		}
	}
}

// TestNoAPIRequestCanNameAnExecutable makes §26 Step 5's "do not permit
// arbitrary shell commands from the browser" a structural fact about this
// package rather than a property of nothing yet using it.
//
// The catalog above narrows validator selection to a fixed id, but it only
// narrows the door it owns. Nothing stopped the next author who needed
// validator selection from taking the shorter route and plumbing a string
// straight into config.Validation.Command, and neither the compiler nor
// this suite would have objected, because until this test there was
// nothing asserting the request types stay free of that shape. That is the
// same gap TestNoMutatingAPIRouteBypassesTheDestructiveGate closes for
// webhost's routes, in the same spirit.
//
// This walks every exported request type this package accepts from the API
// layer and refuses any field that could carry an executable: a
// config.Command in any form, or a name that reads like a path to one.
// Adding validator selection is meant to mean adding a ValidatorID field,
// which this happily allows.
func TestNoAPIRequestCanNameAnExecutable(t *testing.T) {
	commandType := reflect.TypeOf(config.Command{})
	banned := []string{"command", "executable", "argv", "script", "shell", "binary", "exec"}

	requests := []any{
		CreateBackupSetRequest{},
		ConnectionTestRequest{},
		RunCycleRequest{},
	}

	for _, req := range requests {
		rt := reflect.TypeOf(req)
		t.Run(rt.Name(), func(t *testing.T) {
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)

				ft := f.Type
				for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array {
					ft = ft.Elem()
				}
				if ft == commandType {
					t.Errorf("%s.%s is a config.Command: an API request must never carry one; select a validator by ValidatorID and resolve it inside this package", rt.Name(), f.Name)
				}

				lower := strings.ToLower(f.Name)
				for _, word := range banned {
					if strings.Contains(lower, word) {
						t.Errorf("%s.%s reads like it can name an executable (%q): §26 Step 5 forbids the API layer naming one, so select a validator by ValidatorID instead", rt.Name(), f.Name, word)
					}
				}
			}
		})
	}

	// The positive control. Every assertion above is a "must not", and a
	// test that walks zero fields, or matches nothing it should, passes
	// them all silently. This runs the identical check over a type that
	// does carry the banned shape and requires it to be caught.
	t.Run("the check catches what it is looking for", func(t *testing.T) {
		type bad struct {
			Name             string
			ValidatorCommand config.Command
		}
		rt := reflect.TypeOf(bad{})
		caught := 0
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Type == commandType {
				caught++
			}
			if strings.Contains(strings.ToLower(f.Name), "command") {
				caught++
			}
		}
		if caught != 2 {
			t.Fatalf("the control type tripped %d checks, want 2: the assertions above cannot be trusted if they do not fire on a field that obviously violates them", caught)
		}
	})
}

package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// validatorTestDir is the materialisation directory every test in this
// file resolves against: a fresh temp directory standing in for the
// state directory a real BackupService derives from cfg.State.Database
// (see validatorScriptDir). Nothing here uses TMPDIR the way
// materializedScriptDir used to -- the directory is passed in, which is
// the whole point of issue #162's first scope item.
func validatorTestDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "validators")
}

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
			cmd, err := resolveValidator(validatorTestDir(t), id)
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
	cmd, err := resolveValidator(validatorTestDir(t), ValidatorTrailerMarker)
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
	dir := validatorTestDir(t)
	catalog := RegisteredValidators()
	if len(catalog) == 0 {
		t.Fatal("RegisteredValidators() is empty")
	}
	for _, entry := range catalog {
		if _, err := resolveValidator(dir, entry.ID); err != nil {
			t.Errorf("resolveValidator(%q): %v", entry.ID, err)
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
// same gap webhost's own route walks close for its routes, in the same
// spirit.
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

// TestMaterializeValidators_RepairsAReapedScript is issue #162's
// stat-and-rewrite requirement, and the direct regression test for the
// defect the old sync.Once had: materializedScriptDir wrote the scripts
// exactly once per process and never looked at them again, so a
// systemd-tmpfiles sweep of an idle /tmp (or anything else that removed
// the file) left resolveValidator handing out a path to a script that no
// longer existed, for the rest of the process's life.
func TestMaterializeValidators_RepairsAReapedScript(t *testing.T) {
	dir := validatorTestDir(t)

	cmd, err := resolveValidator(dir, ValidatorTrailerMarker)
	if err != nil {
		t.Fatalf("resolveValidator: %v", err)
	}
	original, err := os.ReadFile(cmd.Executable)
	if err != nil {
		t.Fatalf("reading the materialized script: %v", err)
	}

	if err := os.Remove(cmd.Executable); err != nil {
		t.Fatalf("removing the materialized script: %v", err)
	}

	if _, err := resolveValidator(dir, ValidatorTrailerMarker); err != nil {
		t.Fatalf("resolveValidator after the script was reaped: %v", err)
	}
	repaired, err := os.ReadFile(cmd.Executable)
	if err != nil {
		t.Fatalf("the reaped script was not rewritten: %v", err)
	}
	if string(repaired) != string(original) {
		t.Fatalf("the rewritten script differs from the embedded one:\n%s", repaired)
	}
}

// TestMaterializeValidators_RepairsATamperedScript is the other half of
// stat-and-rewrite: a script whose content has drifted from the embedded
// copy (an operator editing it, or a partial write) is replaced, not
// trusted. Without this, the one thing a registered validator is FOR --
// running code this package, and only this package, chose -- would be
// decided by whatever happens to be on disk.
func TestMaterializeValidators_RepairsATamperedScript(t *testing.T) {
	dir := validatorTestDir(t)

	cmd, err := resolveValidator(dir, ValidatorTrailerMarker)
	if err != nil {
		t.Fatalf("resolveValidator: %v", err)
	}
	if err := os.WriteFile(cmd.Executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("tampering with the materialized script: %v", err)
	}

	if _, err := resolveValidator(dir, ValidatorTrailerMarker); err != nil {
		t.Fatalf("resolveValidator after tampering: %v", err)
	}
	got, err := os.ReadFile(cmd.Executable)
	if err != nil {
		t.Fatalf("reading the repaired script: %v", err)
	}
	if strings.Contains(string(got), "exit 0") {
		t.Fatalf("the tampered script was left in place:\n%s", got)
	}
	if !strings.Contains(string(got), "RCLONE-MANAGER-BACKUP-COMPLETE") {
		t.Fatalf("the repaired script is not the embedded trailer-marker script:\n%s", got)
	}
}

// TestMaterializeValidators_DoesNotLatchAFailure is the sync.Once defect
// stated directly: one transient failure (here, a state directory that is
// a plain file rather than a directory) must not disable every registered
// validator for the rest of the process's life. Once the condition is
// gone, the very next call has to succeed.
func TestMaterializeValidators_DoesNotLatchAFailure(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "validators")

	// A plain file where the directory needs to go: MkdirAll fails.
	if err := os.WriteFile(dir, []byte("in the way"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := resolveValidator(dir, ValidatorTrailerMarker); err == nil {
		t.Fatal("resolveValidator succeeded with a plain file where the validator directory belongs; want an error")
	}

	if err := os.Remove(dir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := resolveValidator(dir, ValidatorTrailerMarker); err != nil {
		t.Fatalf("resolveValidator after the obstruction was removed: %v (a transient failure must not latch)", err)
	}
}

// TestMaterializeValidators_WritesUnderTheGivenDirectoryNotTMPDIR is the
// "move it out of TMPDIR" scope item, asserted as a property rather than
// by reading the implementation: the resolved executable must live under
// the directory it was handed, and os.TempDir() must be nowhere in it.
//
// The positive control below proves this assertion can fail: it runs the
// identical check against a path deliberately built under os.TempDir()
// and requires it to be caught.
func TestMaterializeValidators_WritesUnderTheGivenDirectoryNotTMPDIR(t *testing.T) {
	dir := validatorTestDir(t)
	cmd, err := resolveValidator(dir, ValidatorTrailerMarker)
	if err != nil {
		t.Fatalf("resolveValidator: %v", err)
	}

	rel, err := filepath.Rel(dir, cmd.Executable)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("Executable = %q resolves outside the directory it was given (%q)", cmd.Executable, dir)
	}

	t.Run("the check catches a TMPDIR-rooted path", func(t *testing.T) {
		outside := filepath.Join(os.TempDir(), "rclone-manager-validators-control", "trailer-marker.sh")
		rel, err := filepath.Rel(dir, outside)
		escaped := err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
		if !escaped {
			t.Fatalf("the containment check did not fire on %q, so the assertion above proves nothing", outside)
		}
	})
}

// validatorIDPattern is the casing convention issue #162 asks this
// package to settle: lowercase kebab-case, the same vocabulary
// completion_strategy ("rename"/"marker"/"stable") and
// unavailable_reason ("not_created"/"unreadable"/"misconfigured")
// already use for a backup set's own configuration values -- NOT the
// UPPER_SNAKE_CASE reserved in this codebase for error codes and
// lifecycle states.
var validatorIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// TestRegisteredValidatorIDsAreLowercaseKebabCaseMatchingTheirScript is
// that decision made enforceable rather than merely written down. It
// also pins the second half of the reason for it: an id doubles as its
// own script's basename, so "trailer-marker" and "trailer-marker.sh" can
// never drift apart, and an UPPER_SNAKE id would either break that or
// require a translation table nothing else in this package needs.
func TestRegisteredValidatorIDsAreLowercaseKebabCaseMatchingTheirScript(t *testing.T) {
	catalog := RegisteredValidators()
	if len(catalog) == 0 {
		t.Fatal("RegisteredValidators() is empty; this test would pass vacuously")
	}
	for _, entry := range catalog {
		if !validatorIDPattern.MatchString(string(entry.ID)) {
			t.Errorf("ValidatorID %q is not lowercase kebab-case; see this test's own doc for the decision it enforces", entry.ID)
		}
		script, ok := catalogScripts[entry.ID]
		if !ok {
			t.Fatalf("RegisteredValidators advertises %q, which the catalog does not map to a script", entry.ID)
		}
		if want := string(entry.ID) + ".sh"; script != want {
			t.Errorf("ValidatorID %q maps to script %q, want %q: an id IS its script's basename", entry.ID, script, want)
		}
		if entry.Summary == "" {
			t.Errorf("ValidatorID %q has no Summary; the wizard's picklist has nothing to render but the raw id", entry.ID)
		}
	}

	t.Run("the pattern rejects the casing it was chosen over", func(t *testing.T) {
		for _, bad := range []string{"TRAILER_MARKER", "trailer_marker", "TrailerMarker", "trailer marker", "trailer-marker.sh"} {
			if validatorIDPattern.MatchString(bad) {
				t.Errorf("validatorIDPattern accepted %q; the assertions above prove nothing if it matches anything", bad)
			}
		}
	})
}

// TestRegisteredValidatorsNeverExposeAPath is the wire-safety half of
// §26 Step 5, from the response side: TestNoAPIRequestCanNameAnExecutable
// keeps a path OUT of what the API accepts, and this keeps one out of
// what the catalog hands BACK. The materialized script's own location is
// this process's business; a client that learned it would learn this
// deployment's filesystem layout, and the next author would be one field
// away from sending it back.
func TestRegisteredValidatorsNeverExposeAPath(t *testing.T) {
	entryType := reflect.TypeOf(Validator{})
	for i := 0; i < entryType.NumField(); i++ {
		f := entryType.Field(i)
		lower := strings.ToLower(f.Name)
		for _, word := range []string{"path", "executable", "command", "script", "dir", "file"} {
			if strings.Contains(lower, word) {
				t.Errorf("Validator.%s reads like it carries a filesystem path (%q); the catalog exposes an id and a human summary, nothing else", f.Name, word)
			}
		}
	}

	dir := validatorTestDir(t)
	for _, entry := range RegisteredValidators() {
		cmd, err := resolveValidator(dir, entry.ID)
		if err != nil {
			t.Fatalf("resolveValidator(%q): %v", entry.ID, err)
		}
		if strings.Contains(string(entry.ID)+entry.Summary, dir) || strings.Contains(entry.Summary, cmd.Executable) {
			t.Errorf("the catalog entry for %q leaks the materialized script path", entry.ID)
		}
	}

	t.Run("the field-name check catches what it is looking for", func(t *testing.T) {
		type bad struct {
			ID         ValidatorID
			ScriptPath string
		}
		rt := reflect.TypeOf(bad{})
		caught := 0
		for i := 0; i < rt.NumField(); i++ {
			lower := strings.ToLower(rt.Field(i).Name)
			for _, word := range []string{"path", "executable", "command", "script", "dir", "file"} {
				if strings.Contains(lower, word) {
					caught++
				}
			}
		}
		if caught != 2 {
			t.Fatalf("the control type tripped %d checks, want 2 (\"script\" and \"path\" on one field)", caught)
		}
	})
}

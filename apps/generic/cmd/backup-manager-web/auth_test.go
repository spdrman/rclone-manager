package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
)

// The create-admin subcommand, and mostly its stdin handling.
//
// Reading a password from stdin sounds trivial and has three cases that a
// naive read gets wrong, all of which end with an operator unable to log
// in with the password they thought they set: a trailing newline that must
// come off, a CRLF that must come off as a unit rather than leaving a
// stray carriage return in the password, and input with no trailing
// newline at all that must be left alone. None of those produce an error;
// they produce a password that is one byte different from the one that was
// typed.
//
// Empty input is refused rather than accepted as an empty password, which
// is the one case where doing nothing would be actively dangerous.
//
// withStdin swaps the package-level os.Stdin, which is safe only because
// nothing in this package runs in parallel. That is a real constraint on
// anybody adding a t.Parallel() here.

// withStdin temporarily redirects the package-level os.Stdin to a pipe
// fed with content, restoring the original *os.File on cleanup. Safe
// because this package's tests never run in parallel (none call
// t.Parallel()), so there is exactly one os.Stdin swap live at a time.
func withStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })

	go func() {
		_, _ = w.WriteString(content)
		_ = w.Close()
	}()
}

func TestReadPasswordFromStdin_StripsExactlyOneTrailingNewline(t *testing.T) {
	got, err := readPasswordFromStdin(strings.NewReader("correct-horse-battery\n"))
	if err != nil {
		t.Fatalf("readPasswordFromStdin: %v", err)
	}
	if got != "correct-horse-battery" {
		t.Errorf("readPasswordFromStdin(%q) = %q, want %q", "correct-horse-battery\n", got, "correct-horse-battery")
	}
}

func TestReadPasswordFromStdin_StripsATrailingCRLF(t *testing.T) {
	got, err := readPasswordFromStdin(strings.NewReader("correct-horse-battery\r\n"))
	if err != nil {
		t.Fatalf("readPasswordFromStdin: %v", err)
	}
	if got != "correct-horse-battery" {
		t.Errorf("readPasswordFromStdin(CRLF) = %q, want %q", got, "correct-horse-battery")
	}
}

func TestReadPasswordFromStdin_NoTrailingNewlineIsUnchanged(t *testing.T) {
	// printf '%s' (no trailing newline) must round-trip untouched -
	// there is no newline here for TrimSuffix to have anything to strip.
	got, err := readPasswordFromStdin(strings.NewReader("correct-horse-battery"))
	if err != nil {
		t.Fatalf("readPasswordFromStdin: %v", err)
	}
	if got != "correct-horse-battery" {
		t.Errorf("readPasswordFromStdin(no newline) = %q, want %q", got, "correct-horse-battery")
	}
}

func TestReadPasswordFromStdin_RefusesEmptyInput(t *testing.T) {
	if _, err := readPasswordFromStdin(strings.NewReader("")); err == nil {
		t.Fatal("readPasswordFromStdin(\"\") = nil error, want a refusal")
	}
	// A lone newline (an empty line piped in) is the same mistake as no
	// input at all once the trailing newline is stripped, and must be
	// refused the same way.
	if _, err := readPasswordFromStdin(strings.NewReader("\n")); err == nil {
		t.Fatal("readPasswordFromStdin(\"\\n\") = nil error, want a refusal")
	}
}

func TestCmdAuth_NoSubcommandFails(t *testing.T) {
	if got := run([]string{"auth"}); got != 2 {
		t.Errorf("run([\"auth\"]) = %d, want 2", got)
	}
}

func TestCmdAuth_UnknownSubcommandFails(t *testing.T) {
	if got := run([]string{"auth", "not-a-real-subcommand"}); got != 2 {
		t.Errorf("run([\"auth\", \"not-a-real-subcommand\"]) = %d, want 2", got)
	}
}

func TestCmdAuthCreateAdmin_RequiresUsername(t *testing.T) {
	if got := run([]string{"auth", "create-admin", "--password-stdin"}); got != 2 {
		t.Errorf("run(create-admin without --username) = %d, want 2", got)
	}
}

func TestCmdAuthCreateAdmin_RequiresThePasswordStdinFlag(t *testing.T) {
	if got := run([]string{"auth", "create-admin", "--username", "bm-admin"}); got != 2 {
		t.Errorf("run(create-admin without --password-stdin) = %d, want 2", got)
	}
}

func TestCmdAuthCreateAdmin_FailsOnEmptyStdin(t *testing.T) {
	withStdin(t, "")
	storePath := filepath.Join(t.TempDir(), "local-auth.json")

	got := run([]string{"auth", "create-admin", "--auth-store", storePath, "--username", "bm-admin", "--password-stdin"})
	if got == 0 {
		t.Fatal("run(create-admin) with empty stdin = 0, want non-zero")
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Errorf("store file exists after a refused create-admin: stat err = %v, want os.IsNotExist", err)
	}
}

func TestCmdAuthCreateAdmin_FailsOnAPasswordShorterThanTheMinimum(t *testing.T) {
	withStdin(t, "too-short\n")
	storePath := filepath.Join(t.TempDir(), "local-auth.json")

	if got := run([]string{"auth", "create-admin", "--auth-store", storePath, "--username", "bm-admin", "--password-stdin"}); got == 0 {
		t.Fatal("run(create-admin) with a too-short password = 0, want non-zero")
	}
}

// TestCmdAuthCreateAdmin_CreatesAnAdministratorTheStoreCanReadBack is
// this command's own end-to-end proof: given a real --auth-store path
// and a password piped through stdin exactly the way an operator's shell
// pipeline would, it must leave behind an AdminRecord
// apps/common/auth/local.NewStore can read straight back -
// provision_test.go (in that package) already proves that record then
// logs in through the real HTTP handler; this test proves this
// COMMAND is the thing that puts it there.
func TestCmdAuthCreateAdmin_CreatesAnAdministratorTheStoreCanReadBack(t *testing.T) {
	withStdin(t, "correct-horse-battery-staple\n")
	storePath := filepath.Join(t.TempDir(), "local-auth.json")

	if got := run([]string{"auth", "create-admin", "--auth-store", storePath, "--username", "bm-admin", "--password-stdin"}); got != 0 {
		t.Fatalf("run(create-admin) = %d, want 0", got)
	}

	admin, err := local.NewStore(storePath).Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin == nil || admin.Username != "bm-admin" {
		t.Fatalf("Admin() after create-admin = %+v, want the provisioned administrator", admin)
	}
}

func TestCmdAuthCreateAdmin_RefusesASecondRunOnceEnrolled(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "local-auth.json")

	withStdin(t, "correct-horse-battery-staple\n")
	if got := run([]string{"auth", "create-admin", "--auth-store", storePath, "--username", "bm-admin", "--password-stdin"}); got != 0 {
		t.Fatalf("first run(create-admin) = %d, want 0", got)
	}

	withStdin(t, "another-long-enough-password\n")
	got := run([]string{"auth", "create-admin", "--auth-store", storePath, "--username", "someone-else", "--password-stdin"})
	if got == 0 {
		t.Fatal("second run(create-admin) against an already-enrolled store = 0, want non-zero")
	}

	admin, err := local.NewStore(storePath).Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin.Username != "bm-admin" {
		t.Errorf("Admin().Username after a refused second create-admin = %q, want the original %q", admin.Username, "bm-admin")
	}
}

// TestCmdAuthCreateAdmin_RefusesWhileAServerIsRunningAgainstTheSameStore
// is issue #322's concurrency-safety requirement exercised at the
// command layer: a live apps/common/auth/local.Service (standing in for
// a running `serve`) holds the store's exclusive lock, and create-admin
// against the same --auth-store must refuse rather than race it.
func TestCmdAuthCreateAdmin_RefusesWhileAServerIsRunningAgainstTheSameStore(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "local-auth.json")

	svc, err := local.New(local.Config{StorePath: storePath})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	withStdin(t, "correct-horse-battery-staple\n")
	got := run([]string{"auth", "create-admin", "--auth-store", storePath, "--username", "bm-admin", "--password-stdin"})
	if got == 0 {
		t.Fatal("run(create-admin) while a Service holds the store = 0, want non-zero")
	}

	needsEnrollment, err := svc.NeedsEnrollment()
	if err != nil {
		t.Fatalf("NeedsEnrollment: %v", err)
	}
	if !needsEnrollment {
		t.Error("NeedsEnrollment() after a refused create-admin = false, want true (nothing should have been written)")
	}
}

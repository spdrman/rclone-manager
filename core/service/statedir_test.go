package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/testenv"
)

func TestValidateStateDir_CreatesMissingDirectory(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "does", "not", "exist", "yet", "state.db")

	if err := validateStateDir(dbPath); err != nil {
		t.Fatalf("validateStateDir: %v", err)
	}

	info, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("Stat parent dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("parent path exists but is not a directory")
	}
}

func TestValidateStateDir_AcceptsAnAlreadyExistingWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	if err := validateStateDir(dbPath); err != nil {
		t.Fatalf("validateStateDir: %v", err)
	}
}

func TestValidateStateDir_RefusesWhenParentIsAPlainFile(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "state-dir-is-actually-a-file")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dbPath := filepath.Join(notADir, "state.db")

	err := validateStateDir(dbPath)
	if !errors.Is(err, ErrStateDirInvalid) {
		t.Fatalf("validateStateDir error = %v, want ErrStateDirInvalid", err)
	}
}

func TestValidateStateDir_RefusesWhenDirectoryIsNotWritable(t *testing.T) {
	testenv.RequirePermissionBitsApply(t)

	root := t.TempDir()
	readOnlyDir := filepath.Join(root, "read-only-state")
	if err := os.Mkdir(readOnlyDir, 0o500); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnlyDir, 0o700) }) // let t.TempDir() clean up
	dbPath := filepath.Join(readOnlyDir, "state.db")

	err := validateStateDir(dbPath)
	if !errors.Is(err, ErrStateDirInvalid) {
		t.Fatalf("validateStateDir error = %v, want ErrStateDirInvalid", err)
	}
}

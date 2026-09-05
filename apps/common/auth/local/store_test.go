// The persistence rules, including the two the rest of this package leans
// on without re-checking.
//
// Enroll being single-shot is one: every caller assumes an existing
// administrator cannot be overwritten, and this is the only place that is
// actually proved. SetPassword preserving the username and creation time
// is the other, because it does a read-modify-write and the easy mistake is
// to write a fresh record with only the field that changed.
//
// The plaintext test reads the raw file rather than the parsed struct, on
// purpose. A struct-level assertion would pass even if the plaintext were
// sitting in a field nothing maps back, and what actually matters is what
// somebody who cats the file can see.
package local

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore_AdminIsNilBeforeAnyEnrollment(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	admin, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin != nil {
		t.Errorf("Admin() = %+v, want nil (no store file exists yet)", admin)
	}
}

func TestStore_EnrollPersistsAndAdminReturnsIt(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "nested", "auth.json"))
	record := AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$fake", CreatedAt: time.Now().UTC()}
	if err := store.Enroll(record); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	got, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if got == nil {
		t.Fatal("Admin() = nil after Enroll, want the enrolled record")
	}
	if got.Username != record.Username || got.PasswordHash != record.PasswordHash {
		t.Errorf("Admin() = %+v, want %+v", *got, record)
	}
}

func TestStore_EnrollTwiceFailsWithErrAlreadyEnrolled(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	first := AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$fake1"}
	if err := store.Enroll(first); err != nil {
		t.Fatalf("first Enroll: %v", err)
	}

	second := AdminRecord{Username: "someone-else", PasswordHash: "$argon2id$fake2"}
	err := store.Enroll(second)
	if !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("second Enroll error = %v, want errors.Is(err, ErrAlreadyEnrolled)", err)
	}

	// The original administrator must survive the rejected second attempt
	// untouched (§49.1: enrollment is single-shot and irreversible).
	got, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if got.Username != first.Username {
		t.Errorf("Admin().Username = %q after a rejected second Enroll, want the original %q", got.Username, first.Username)
	}
}

func TestStore_PersistsAcrossANewStoreInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	first := NewStore(path)
	if err := first.Enroll(AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$fake"}); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	// A brand new Store value pointed at the same path (simulating a
	// process restart) must see the same administrator - enrollment
	// closing must not depend on any in-memory state.
	second := NewStore(path)
	admin, err := second.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin == nil || admin.Username != "bm-admin" {
		t.Errorf("Admin() after reopening the store = %+v, want the persisted administrator", admin)
	}
}

func TestStore_SetPasswordUpdatesHashAndPreservesUsernameAndCreatedAt(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	created := time.Now().UTC().Truncate(time.Second)
	if err := store.Enroll(AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$old", CreatedAt: created}); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if err := store.SetPassword("$argon2id$new"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	got, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if got.PasswordHash != "$argon2id$new" {
		t.Errorf("Admin().PasswordHash = %q, want %q", got.PasswordHash, "$argon2id$new")
	}
	if got.Username != "bm-admin" {
		t.Errorf("Admin().Username = %q after SetPassword, want unchanged %q", got.Username, "bm-admin")
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("Admin().CreatedAt = %v after SetPassword, want unchanged %v", got.CreatedAt, created)
	}
}

// TestStore_SetPasswordFailsBeforeEnrollment guards against SetPassword
// ever silently creating a partial administrator record: rotation is
// meaningless before an administrator exists at all.
func TestStore_SetPasswordFailsBeforeEnrollment(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	err := store.SetPassword("$argon2id$new")
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("SetPassword before enrollment error = %v, want errors.Is(err, ErrNotEnrolled)", err)
	}
}

func TestStore_SetPasswordPersistsAcrossANewStoreInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	first := NewStore(path)
	if err := first.Enroll(AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$old"}); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := first.SetPassword("$argon2id$new"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// A brand new Store value pointed at the same path (simulating a
	// process restart) must see the rotated hash, not the pre-rotation one.
	second := NewStore(path)
	admin, err := second.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin == nil || admin.PasswordHash != "$argon2id$new" {
		t.Errorf("Admin() after reopening the store = %+v, want PasswordHash %q", admin, "$argon2id$new")
	}
}

func TestStore_NeverWritesAPlaintextPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := NewStore(path)
	encoded, err := hashPassword("super-secret-plaintext-value")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if err := store.Enroll(AdminRecord{Username: "bm-admin", PasswordHash: encoded}); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "super-secret-plaintext-value") {
		t.Errorf("store file at %s contains the plaintext password", path)
	}
}

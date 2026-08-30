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

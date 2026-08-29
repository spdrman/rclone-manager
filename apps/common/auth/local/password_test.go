package local

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPassword_NeverContainsThePlaintext(t *testing.T) {
	encoded, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if strings.Contains(encoded, "correct horse battery staple") {
		t.Fatalf("encoded hash contains the plaintext password: %q", encoded)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Errorf("encoded = %q, want a $argon2id$ prefix", encoded)
	}
}

func TestHashPassword_TwoHashesOfTheSamePasswordDiffer(t *testing.T) {
	a, err := hashPassword("same password")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	b, err := hashPassword("same password")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical; salts are not being randomized")
	}
}

func TestVerifyPassword_AcceptsTheCorrectPassword(t *testing.T) {
	encoded, err := hashPassword("s3cret-enough")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if err := verifyPassword(encoded, "s3cret-enough"); err != nil {
		t.Errorf("verifyPassword(correct password) = %v, want nil", err)
	}
}

func TestVerifyPassword_RejectsTheWrongPassword(t *testing.T) {
	encoded, err := hashPassword("s3cret-enough")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	err = verifyPassword(encoded, "wrong password")
	if !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("verifyPassword(wrong password) = %v, want errors.Is(err, ErrPasswordMismatch)", err)
	}
}

func TestVerifyPassword_RejectsGarbageEncoding(t *testing.T) {
	if err := verifyPassword("not-an-argon2-hash-at-all", "anything"); err == nil {
		t.Error("verifyPassword against a garbage encoded hash = nil error, want a non-nil error")
	}
}

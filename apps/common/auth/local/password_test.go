// What is asserted here is mostly what must NOT be true of a hash.
//
// The plaintext must not appear in it, and two hashes of the same password
// must differ, which together are how a missing or fixed salt shows up:
// both a salt-free implementation and one with a hardcoded salt would
// verify passwords correctly and pass any round-trip test, while making
// every stored hash comparable against a precomputed table.
//
// The garbage-encoding case is about the parser rather than the crypto. An
// encoded hash carries its own parameters, so verifyPassword reads
// attacker-adjacent structure out of a string, and it has to refuse a
// malformed one rather than fall through to a comparison against whatever
// it managed to decode.
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

// Argon2id hashing, and the format that makes a stored hash
// self-describing.
//
// The encoded string carries its own version and cost parameters rather
// than relying on the constants above, and verifyPassword reads them back
// out of the string instead of assuming today's values. That is what makes
// raising the cost parameters a one-line change: every hash already
// written stays verifiable at the cost it was created with, and only a
// later rotation moves it up. A verifier that used the current constants
// would silently reject every existing password the day somebody tuned
// them.
//
// The parameters themselves are sized for one administrator signing in a
// handful of times a day, not for a login surface under load, so they sit
// comfortably above OWASP's floor rather than at it.
package local

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for a single administrator account authenticating
// at most a handful of times a day, not a high-QPS multi-tenant login
// surface: comfortably above OWASP's minimum (m=19MiB, t=2, p=1) without
// making every login a noticeable pause.
const (
	argon2Time    uint32 = 3
	argon2Memory  uint32 = 64 * 1024 // 64 MiB
	argon2Threads uint8  = 2
	argon2KeyLen  uint32 = 32
	saltLen              = 16
)

// ErrPasswordMismatch is returned by verifyPassword when password does
// not match the encoded hash it was checked against.
var ErrPasswordMismatch = errors.New("local: password does not match")

// hashPassword returns a self-describing, PHC-string-shaped Argon2id
// hash of password. §3.6/§13A's "no plaintext password persistence" is
// this function's whole reason to exist: nothing in this package ever
// stores password itself, only what this returns.
func hashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("local: generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// verifyPassword reports (via error) whether password matches encoded, a
// string previously returned by hashPassword. The final comparison is
// constant-time (crypto/subtle), so a mismatch cannot be distinguished by
// timing based on how many bytes matched.
func verifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return fmt.Errorf("local: unrecognized password hash format")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return fmt.Errorf("local: parse argon2 version: %w", err)
	}

	var memory, timeCost uint32
	var threads uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return fmt.Errorf("local: parse argon2 params: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("local: decode salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return fmt.Errorf("local: decode hash: %w", err)
	}

	got := argon2.IDKey([]byte(password), salt, timeCost, memory, uint8(threads), uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// The single-use secret that stands between an unclaimed deployment and
// whoever reaches its port first.
//
// §49.1's requirement is easy to state and easy to get subtly wrong:
// reaching the port must not be enough to claim the administrator account.
// The token this file issues is what makes the difference, and its three
// properties all exist because of a specific way the naive version leaks.
// It is held in memory only, so a restart before enrollment completes
// invalidates whatever a previous run printed rather than leaving a
// standing credential in a file somebody might later read. There is only
// ever one live at a time, so a second start cannot leave two valid
// claims. And consume marks it used inside the same lock that checks it,
// so two requests arriving together cannot both win.
//
// It is printed to the process's own stdout and nowhere else. That is not
// a limitation to work around later: any channel that could deliver it
// elsewhere would be a channel an attacker could try to reach.
package local

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// bootstrapTokenTTL bounds how long a printed enrollment token remains
// valid (§49.1: "SHALL be single-use and SHALL expire"). 30 minutes is
// long enough for an operator to read the container's own startup log
// and paste the token into the enrollment page, short enough that a
// token nobody used stops being a standing credential.
const bootstrapTokenTTL = 30 * time.Minute

type bootstrapToken struct {
	value     string
	expiresAt time.Time
	used      bool
}

// bootstrapIssuer hands out the single-use secret §49.1 requires before
// enrollment can claim the administrator account: "reaching the port
// SHALL NOT be sufficient to claim the account." Only one token is ever
// live at a time; issuing a new one (which Service does once, at
// construction, only when no administrator exists yet - see service.go)
// silently invalidates whatever token came before it, which is exactly
// what should happen across a process restart before enrollment
// completes.
type bootstrapIssuer struct {
	mu    sync.Mutex
	token *bootstrapToken
	now   func() time.Time
}

func newBootstrapIssuer(now func() time.Time) *bootstrapIssuer {
	return &bootstrapIssuer{now: now}
}

// issue generates and remembers a fresh bootstrap token, replacing any
// previous one, and returns its value.
func (b *bootstrapIssuer) issue() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("local: generate bootstrap token: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)

	b.mu.Lock()
	b.token = &bootstrapToken{value: value, expiresAt: b.now().Add(bootstrapTokenTTL)}
	b.mu.Unlock()
	return value, nil
}

// consume reports whether candidate is the current, unexpired, unused
// bootstrap token, and if so marks it used atomically with that check -
// it can never succeed twice for the same token, and a comparison
// against the stored value runs in constant time so a mismatch cannot be
// distinguished by timing.
func (b *bootstrapIssuer) consume(candidate string) bool {
	if candidate == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.token == nil || b.token.used {
		return false
	}
	if b.now().After(b.token.expiresAt) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(b.token.value), []byte(candidate)) != 1 {
		return false
	}
	b.token.used = true
	return true
}

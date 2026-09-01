package local

import (
	"fmt"
	"time"
)

// CreateAdminConfig is the input to CreateAdmin.
type CreateAdminConfig struct {
	// StorePath is where the administrator record is persisted - the
	// same store.go file a Service constructed with Config.StorePath set
	// to the same value reads and writes.
	StorePath string

	// Username/Password are the credentials to provision. Password is
	// validated against the same minimum handleEnroll enforces
	// (minPasswordLength, handler.go) and is never persisted itself -
	// only hashPassword's output is (password.go).
	Username string
	Password string

	// Now is a seam over time.Now for tests; nil means time.Now, exactly
	// like Config.Now.
	Now func() time.Time
}

// CreateAdmin provisions this package's one administrator record
// (store.go's AdminRecord) directly, without ever going through
// Service.Handler's HTTP /enroll route, a CSRF cookie, or
// bootstrap.go's in-memory, single-use, network-reachable bootstrap
// token (issue #322).
//
// # Why this is safe (issue #322's own security question)
//
// docs/EPIC-B-multi-nas.md §49.1 requires that "reaching the port SHALL
// NOT be sufficient to claim the account" - the bootstrap token exists so
// that whoever happens to connect to an unclaimed instance first cannot
// become its administrator. CreateAdmin does not touch that property at
// all: it has no network listener, accepts no connection, and requires
// nothing but the ability to write to StorePath. Filesystem access to
// StorePath is a DIFFERENT trust boundary than "reaching the port" - one
// this project already grants complete trust to everywhere else
// (config.yaml, the state database, an imported SSH key) - so a caller
// able to invoke CreateAdmin already has, by this product's existing
// threat model, exactly the level of trust §49.1 is protecting the
// account from an untrusted network peer NOT having.
//
// # Concurrency safety
//
// CreateAdmin writes straight into the on-disk file a running Service's
// Store.Enroll/Store.SetPassword read-modify-write cycle also touches
// (store.go's own doc has always assumed "a single process owns path").
// It therefore takes the same exclusive advisory lock Service.New holds
// for its whole lifetime (lock_unix.go/lock_other.go) before writing
// anything, and refuses with ErrStoreLocked if a running server already
// holds it, rather than risking a lost write or a corrupted store. This
// command is meant to run before first start, or while the server is
// stopped: it deliberately does not wait for or coordinate with a live
// one, it fails fast and tells the operator to stop it first.
//
// # Compatibility
//
// The record CreateAdmin writes is produced by exactly the same
// hashPassword (password.go) and exactly the same Store.Enroll
// (store.go, including its own §49.1 single-shot guard) that
// handleEnroll uses - an administrator provisioned this way is
// byte-for-byte indistinguishable, to Store.Admin, handleLogin, or
// anything else that later reads the store, from one who enrolled
// through a browser.
func CreateAdmin(cfg CreateAdminConfig) (*AdminRecord, error) {
	if cfg.StorePath == "" {
		return nil, fmt.Errorf("local: CreateAdminConfig.StorePath is required")
	}
	if cfg.Username == "" {
		return nil, fmt.Errorf("local: CreateAdminConfig.Username is required")
	}
	if len(cfg.Password) < minPasswordLength {
		return nil, fmt.Errorf("local: password must be at least %d characters", minPasswordLength)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	lock, err := acquireStoreLock(cfg.StorePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.release() }()

	hash, err := hashPassword(cfg.Password)
	if err != nil {
		return nil, fmt.Errorf("local: hash password: %w", err)
	}

	admin := AdminRecord{
		Username:     cfg.Username,
		PasswordHash: hash,
		CreatedAt:    now().UTC(),
	}
	if err := NewStore(cfg.StorePath).Enroll(admin); err != nil {
		return nil, err
	}
	return &admin, nil
}

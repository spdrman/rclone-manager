package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The one thing this package persists: a username and a password hash.
//
// Everything else it holds (sessions, bootstrap tokens, rate-limit
// counters) is deliberately in memory, so this file is the whole of the
// on-disk surface, and it is a single small JSON file rather than a
// database because one record does not justify one.
//
// Two properties are load-bearing. Writes go through a temp file and a
// rename, so a crash mid-write leaves the previous file intact rather than
// a truncated one that fails to parse on the next start and locks the
// operator out. And Enroll refuses when a record already exists, which is
// §49.1's single-shot rule enforced at the lowest level rather than only
// in the handler: the handler checks too, but the handler's check and its
// write are not atomic, so this is what actually decides a race.
//
// Store takes no OS-level lock of its own. It is the path-only primitive,
// and both callers that own a store for longer than one call (Service.New,
// CreateAdmin) take the lock in lock_unix.go around it.

// AdminRecord is the one persisted local-auth identity this package
// supports today (docs/EPIC-B-multi-nas.md §13.4's admin-only initial
// release): a username and an Argon2id password hash (password.go).
// PasswordHash is never a plaintext password.
type AdminRecord struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

// storeFile is the on-disk shape Store persists. Enrollment is
// permanently closed the moment Admin is non-nil (§49.1: "single-shot and
// irreversible"); nothing in this package ever sets it back to nil.
type storeFile struct {
	Admin *AdminRecord `json:"admin"`
}

// ErrAlreadyEnrolled is returned by Store.Enroll when an administrator
// record already exists.
var ErrAlreadyEnrolled = errors.New("local: an administrator account already exists")

// Store persists exactly one AdminRecord to a JSON file at path, guarded
// by an in-process mutex (this package assumes a single process owns
// path; the generic host's serve command is exactly that) and made
// durable one write at a time via write-temp-then-rename, so a crash
// mid-write can never leave a half-written file for the next start to
// trip over.
//
// "A single process owns path" is no longer just an assumption: New and
// CreateAdmin (service.go/provision.go) both take path's own exclusive
// advisory lock (lock_unix.go/lock_other.go) before either constructs a
// Store or writes through one, so a second process reaching for the same
// path - a duplicate `serve`, or `create-admin` racing a live one -
// is refused with ErrStoreLocked rather than racing this Store's own
// read-modify-write cycle. Store itself still does not take that lock;
// it is deliberately the lower-level, path-only primitive both callers
// build on.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a Store backed by the JSON file at path. path's parent
// directory is created (mode 0700) on first write if it does not already
// exist; nothing is written or read until Admin or Enroll is called.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// load reads the file, treating "not there yet" as an empty store rather
// than an error: a deployment that has never enrolled has no file, and
// that is the normal first-start state, not a fault. A parse failure is
// different and is reported, because a file that exists and cannot be read
// means something wrote garbage over an administrator record and carrying
// on would quietly reopen enrollment.
func (s *Store) load() (storeFile, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return storeFile{}, nil
	}
	if err != nil {
		return storeFile{}, fmt.Errorf("local: read store %s: %w", s.path, err)
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return storeFile{}, fmt.Errorf("local: parse store %s: %w", s.path, err)
	}
	return f, nil
}

// save replaces the file's whole contents. Callers hold s.mu and have
// already merged whatever they wanted to change into f, so there is no
// partial-update path here to get wrong.
func (s *Store) save(f storeFile) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("local: create store directory: %w", err)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("local: encode store: %w", err)
	}
	// write-temp-then-rename: os.Rename is atomic on the same filesystem
	// (true of every mount this file is expected to live on: a bind-mounted
	// STATE_DIR volume, or a plain local directory in tests), so a reader
	// never observes a partially written file, and a crash between the
	// WriteFile and the Rename leaves the ORIGINAL file untouched rather
	// than a corrupted one.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("local: write store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("local: commit store: %w", err)
	}
	return nil
}

// Admin returns the persisted administrator record, or nil if enrollment
// has not happened yet.
func (s *Store) Admin() (*AdminRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Admin, nil
}

// ErrNotEnrolled is returned by Store.SetPassword when no administrator
// exists yet - rotating a password before enrollment is meaningless, and
// this is the one method in this package that could otherwise silently
// create a partial admin record.
var ErrNotEnrolled = errors.New("local: no administrator account exists yet")

// SetPassword replaces the persisted administrator's password hash with
// newHash, leaving Username and CreatedAt untouched. It fails with
// ErrNotEnrolled if no administrator has enrolled yet - password rotation
// is meaningless before enrollment, and this is the one method in this
// package that could otherwise silently create a partial admin record.
func (s *Store) SetPassword(newHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if f.Admin == nil {
		return ErrNotEnrolled
	}
	f.Admin.PasswordHash = newHash
	return s.save(f)
}

// Enroll persists admin as this store's one administrator record. It
// fails with ErrAlreadyEnrolled if a record already exists: enrollment is
// single-shot and irreversible (§49.1), and this is the one method in
// this package that could otherwise silently overwrite an existing
// administrator.
func (s *Store) Enroll(admin AdminRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if f.Admin != nil {
		return ErrAlreadyEnrolled
	}
	f.Admin = &admin
	return s.save(f)
}

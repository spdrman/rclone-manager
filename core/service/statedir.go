package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// This file is one step of §46.1's startup sequence (startup.go drives
// the rest): decide whether the directory that is about to hold the state
// database can actually be used, before anything takes a lock in it,
// snapshots it or migrates it.
//
// The step exists because of who reads the failure. state.Open would find
// the same problems a moment later, but it would find them as a SQLite
// driver error, and "unable to open database file" leaves an operator to
// infer that the volume mount is read-only, or that a file is sitting
// where a directory should be. Asking first costs a stat and a temp file,
// and turns a guess into a sentence.
//
// The check is deliberately asymmetric about what it will fix. A missing
// directory is created, because a fresh deployment's state directory not
// existing yet is the normal first container start rather than a mistake.
// A directory that exists and cannot be used is refused, because every
// way that happens is somebody's configuration being wrong and none of
// them is this process's to repair.

// ErrStateDirInvalid is returned by validateStateDir when the state
// database's parent directory cannot be used safely: it exists but is not
// a directory, or exists but this process cannot write to it. Section
// 46.1's startup sequence ("validate state directory") refuses to proceed
// past this check, rather than let state.Open discover the same problem
// later as a much less legible raw SQLite driver error.
var ErrStateDirInvalid = errors.New("service: state directory is not usable")

// validateStateDir is section 46.1's "validate state directory" step. It
// ensures dbPath's parent directory exists and is genuinely writable by
// this process, before anything else in the startup sequence (the startup
// lock, the pre-migration snapshot, migration itself) touches it.
//
// A missing directory is created, not refused: a brand-new deployment's
// state directory not existing yet is normal (an operator's first-ever
// container start against a fresh volume), the same "create it, don't
// insist it was pre-provisioned" posture pipeline.go's admitCapacity
// already takes for a backup set's LocalPath. What IS refused is a
// directory that exists but is unusable — a plain file where a directory
// should be, or a directory this process cannot write into (a misconfigured
// volume mount, a permissions mistake) — since state.Open's own PRAGMA
// setup and migrate() would otherwise fail deeper in, with an error that
// forces an operator to infer "the state directory itself is the problem"
// from SQLite driver prose instead of being told directly.
func validateStateDir(dbPath string) error {
	dir := filepath.Dir(dbPath)

	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return fmt.Errorf("%w: creating %s: %v", ErrStateDirInvalid, dir, mkErr)
		}
	case err != nil:
		return fmt.Errorf("%w: statting %s: %v", ErrStateDirInvalid, dir, err)
	case !info.IsDir():
		return fmt.Errorf("%w: %s exists and is not a directory", ErrStateDirInvalid, dir)
	}

	// Writability probe: create-and-remove a small temp file rather than
	// inspecting permission bits, since bits alone do not account for
	// ACLs, read-only bind mounts, or running as a uid the bits do not
	// describe — the only reliable way to know "can this process write
	// here" is to actually try.
	probe, err := os.CreateTemp(dir, ".state-dir-probe-*")
	if err != nil {
		return fmt.Errorf("%w: %s is not writable: %v", ErrStateDirInvalid, dir, err)
	}
	probePath := probe.Name()
	if cerr := probe.Close(); cerr != nil {
		_ = os.Remove(probePath)
		return fmt.Errorf("%w: %s is not writable: %v", ErrStateDirInvalid, dir, cerr)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("%w: removing probe file in %s: %v", ErrStateDirInvalid, dir, err)
	}
	return nil
}

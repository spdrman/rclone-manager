// Package model holds the identity types every other package agrees on.
//
// It deliberately depends on nothing. Config, state, lifecycle, discovery and
// retention all import it, so anything that lands here becomes shared
// vocabulary and should stay small.
package model

import (
	"fmt"
	"strings"
)

// BackupSetID names one stream of logically interchangeable restore points,
// for example production/postgres-primary or staging/uploads (FR-7).
//
// This type exists because retention, health, lifecycle and last-known-good all
// have to reason per set and never across sets. A bug that lets two sets share
// an identity is not a cosmetic bug: it lets one set's retention pass delete
// another set's last good restore point. So the identity is a struct with
// validated parts rather than a bare string that anyone can concatenate.
type BackupSetID struct {
	Source string
	Set    string
}

// separator is reserved. Neither half may contain it, otherwise
// {"a", "b/c"} and {"a/b", "c"} would render identically.
const separator = "/"

// NewBackupSetID validates both halves and returns the identity.
func NewBackupSetID(source, set string) (BackupSetID, error) {
	if err := validPart("source", source); err != nil {
		return BackupSetID{}, err
	}
	if err := validPart("backup set", set); err != nil {
		return BackupSetID{}, err
	}
	return BackupSetID{Source: source, Set: set}, nil
}

// ParseBackupSetID reads the "source/set" rendering back.
func ParseBackupSetID(s string) (BackupSetID, error) {
	source, set, found := strings.Cut(s, separator)
	if !found {
		return BackupSetID{}, fmt.Errorf("backup set id %q has no %q separator", s, separator)
	}
	return NewBackupSetID(source, set)
}

func (b BackupSetID) String() string { return b.Source + separator + b.Set }

// IsZero reports whether this is the unset value. Callers should treat a zero
// id as a programming error rather than as a wildcard, because a wildcard set
// id in a retention query would span every set at once.
func (b BackupSetID) IsZero() bool { return b.Source == "" && b.Set == "" }

func validPart(what, v string) error {
	switch {
	case v == "":
		return fmt.Errorf("%s must not be empty", what)
	case strings.Contains(v, separator):
		return fmt.Errorf("%s %q must not contain %q", what, v, separator)
	case strings.TrimSpace(v) != v:
		return fmt.Errorf("%s %q must not have leading or trailing whitespace", what, v)
	case strings.ContainsAny(v, "\x00\n\r"):
		return fmt.Errorf("%s %q must not contain control characters", what, v)
	}
	return nil
}

// ArtifactID identifies one artifact within one backup set.
//
// Name is the artifact's remote basename. It is untrusted input from a remote
// filesystem (FR-8), so it is validated here rather than wherever it happens to
// get used, and it is deliberately not a path: a path would let a remote name
// like ../../etc/passwd travel through the journal into a delete.
type ArtifactID struct {
	Set  BackupSetID
	Name string
}

// NewArtifactID validates the artifact name as a plain basename.
func NewArtifactID(set BackupSetID, name string) (ArtifactID, error) {
	if set.IsZero() {
		return ArtifactID{}, fmt.Errorf("artifact id needs a backup set")
	}
	switch {
	case name == "":
		return ArtifactID{}, fmt.Errorf("artifact name must not be empty")
	case name == "." || name == "..":
		return ArtifactID{}, fmt.Errorf("artifact name %q is a directory reference", name)
	case strings.ContainsAny(name, "/\\\x00\n\r"):
		return ArtifactID{}, fmt.Errorf("artifact name %q must be a basename, not a path", name)
	case strings.TrimSpace(name) != name:
		return ArtifactID{}, fmt.Errorf("artifact name %q must not have leading or trailing whitespace", name)
	}
	return ArtifactID{Set: set, Name: name}, nil
}

func (a ArtifactID) String() string { return a.Set.String() + separator + a.Name }

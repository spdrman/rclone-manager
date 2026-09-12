// How far a restore point has actually been checked, on a ladder rather
// than as a bool.
//
// EPIC K's finding, and the reason this type exists: structural
// verification is not restore verification. An engine can prove that every
// content identifier a snapshot references resolves and that every hash
// matches, and none of that proves a restore produces the tree an operator
// is expecting -- permissions, symlinks, sparse files, a path the restorer
// cannot create, or simply a repository that is fine and a restore path
// that is broken. A product that reports "verified" without saying which of
// those it did is making the promise it has not tested.
//
// # Why this is not VerificationMode
//
// consistency.go already has a VerificationMode, and the two are about
// opposite ends of a backup. VerificationMode is a BACKUP-time decision:
// how much of a source's content must be read rather than trusted to
// metadata, which is a question about whether a snapshot will be correct
// when it is taken. VerificationLevel is an AFTER-the-fact question about a
// snapshot that already exists: how hard did we look at what is stored.
//
// They are separate because a deployment can want either without the other.
// A conservative source policy that re-hashes every byte on the way in
// still says nothing about whether the repository has rotted since, and a
// nightly restore drill says nothing about whether the source was read
// honestly in the first place.

package model

import (
	"fmt"
	"strings"
)

// VerificationLevel names how thoroughly one restore point has been
// verified. The four levels are EPIC K's verification levels 1 to 4, and
// they are ordered: a level satisfies any requirement at or below it (see
// AtLeast).
type VerificationLevel string

const (
	// LevelStructural checks that the snapshot's own structures resolve:
	// every directory entry, every content identifier it references, every
	// index it needs. No file content is read, and nothing outside the
	// repository is touched.
	//
	// This is the level a repository can always afford, and the one whose
	// report is most easily over-read: it proves the snapshot is not
	// dangling, and it proves nothing about the bytes.
	LevelStructural VerificationLevel = "structural"

	// LevelContentSample reads and re-hashes a deterministic fraction of
	// the snapshot's content. It catches rot and corruption statistically:
	// a repository losing blobs is found by a sample long before it is
	// found by a restore, and the cost is a fraction of the data rather
	// than all of it.
	LevelContentSample VerificationLevel = "content_sample"

	// LevelContentFull reads and re-hashes every byte the snapshot
	// references. It is the strongest statement available about what is
	// STORED, and it still does not attempt a restore.
	LevelContentFull VerificationLevel = "full"

	// LevelRestoreDrill restores the snapshot (or a defined subset of it)
	// to a scratch location and compares the result. It is the only level
	// that proves the one thing a backup exists for, and the only one
	// whose cost includes writing the data out again.
	LevelRestoreDrill VerificationLevel = "restore_drill"
)

// VerificationLevels is the ladder, ascending. Its order is the order Rank
// reports, so the two cannot disagree.
//
// A fifth rung is a product decision -- a new promise, a new cost and a new
// row in every report -- which is what the count assertion in the tests is
// defending.
var VerificationLevels = []VerificationLevel{
	LevelStructural,
	LevelContentSample,
	LevelContentFull,
	LevelRestoreDrill,
}

// ParseVerificationLevel reads a level back from configuration or from the
// catalog, and refuses anything else rather than defaulting.
//
// Both defaults would be wrong. Reading an unrecognised value as the
// weakest rung silently downgrades what an operator asked for, and reading
// it as the strongest invents nightly restore drills -- real I/O, real
// scratch space -- that nobody asked for. An empty string is refused too:
// what silence means is the caller's decision, made where the caller knows
// whether it is reading a config file (where omission is legal and resolves
// to LevelStructural) or a catalog row (where a level is always written).
func ParseVerificationLevel(s string) (VerificationLevel, error) {
	for _, level := range VerificationLevels {
		if string(level) == s {
			return level, nil
		}
	}

	return "", fmt.Errorf("unknown verification level %q (expected one of %s)", s, joinLevels(VerificationLevels))
}

// DefaultVerificationLevel is what an incremental backup set gets when its
// configuration says nothing: the rung that always runs and costs nothing
// beyond the repository's own metadata.
//
// It is deliberately the weakest one. Every stronger rung reads or writes
// data on a cadence, which is a decision about a deployment's I/O budget
// and its backup window, and defaulting a deployment into it would be this
// product deciding how to spend somebody else's disks.
const DefaultVerificationLevel = LevelStructural

func (l VerificationLevel) String() string { return string(l) }

// Rank is this level's position on the ladder, 1 to 4.
//
// A level this package does not recognise ranks 0, which is below every
// real rung, so an unrecognised value can never satisfy a requirement (see
// AtLeast). That is the only safe direction: a typo in a policy must not
// buy a claim.
func (l VerificationLevel) Rank() int {
	for i, level := range VerificationLevels {
		if level == l {
			return i + 1
		}
	}

	return 0
}

// AtLeast reports whether this level satisfies a requirement of other.
//
// Both unknowns are refused rather than one being treated as permissive: an
// unrecognised level satisfies nothing, and nothing satisfies an
// unrecognised requirement, because a policy nobody can evaluate must not
// be reported as met.
func (l VerificationLevel) AtLeast(other VerificationLevel) bool {
	mine, theirs := l.Rank(), other.Rank()
	if mine == 0 || theirs == 0 {
		return false
	}

	return mine >= theirs
}

// ReadsContent reports whether this level reads stored file content at all.
// It is the bit a report must consult before using the word "verified"
// about bytes: LevelStructural does not, and an unrecognised level is
// treated as claiming nothing.
func (l VerificationLevel) ReadsContent() bool {
	return l.Rank() >= LevelContentSample.Rank()
}

// ProvesRestorable reports whether this level actually restored something.
// Only LevelRestoreDrill does, which is the whole distinction EPIC K asked
// for: everything below it is a statement about storage, not about restore.
func (l VerificationLevel) ProvesRestorable() bool {
	return l == LevelRestoreDrill
}

// Describe is the operator-facing sentence for this level. The wizard's
// picker and the verification report are renderings of these, so a level
// without one would render as a blank row.
func (l VerificationLevel) Describe() string {
	switch l {
	case LevelStructural:
		return "resolves the snapshot's own structures; reads no content and proves nothing about the bytes"
	case LevelContentSample:
		return "re-reads and re-hashes a deterministic fraction of the stored content"
	case LevelContentFull:
		return "re-reads and re-hashes every byte the snapshot references"
	case LevelRestoreDrill:
		return "restores the snapshot to scratch space and compares the result; the only level that proves a restore"
	default:
		return fmt.Sprintf("unknown verification level %q", string(l))
	}
}

// joinLevels renders the ladder for an error message, so a refusal lists
// the legal values instead of leaving an operator to find them.
func joinLevels(levels []VerificationLevel) string {
	var b strings.Builder

	for i, level := range levels {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(string(level))
	}

	return b.String()
}

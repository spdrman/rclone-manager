package source

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// ErrUnsafePath is the refusal for a source path this adapter will not
// turn into a repository path. Every rejection below wraps it, so a
// caller can branch on the class and read the sentence for the reason.
//
// It is one sentinel rather than one per rule because the caller's
// response is the same for all of them - refuse the entry, report it,
// carry on with the rest of the source - while the operator's response
// depends on the sentence, which is where the detail belongs.
var ErrUnsafePath = errors.New("source: unsafe source path")

// maxPathBytes bounds a single source path.
//
// It is not a filesystem limit and does not pretend to be one; it is a
// refusal to carry an unbounded string out of a directory listing into a
// snapshot identity, a log line and a report. 4096 is PATH_MAX on Linux
// and is longer than any path a real source has.
const maxPathBytes = 4096

// SafeRelPath turns one untrusted source path into the slash-separated,
// root-relative path this adapter will use, or refuses it by name.
//
// Untrusted is the operative word and it includes paths that came out of
// the source's own listing. A source is somebody else's filesystem: the
// names in it are chosen by whoever writes to it, and this process turns
// them into repository identities, log lines and, eventually, restore
// destinations. FR-8's rule that every name off a source is data, not
// instructions, is enforced here or nowhere.
//
// The rules, and what each one is actually stopping:
//
//   - Empty, or "." after cleaning: names nothing. A path that resolves
//     to the root itself is not an object.
//   - A NUL byte: a name that a C API - and therefore some layer under
//     some backend - truncates at the NUL, so "safe.txt\x00/../../etc"
//     is two different paths depending on who reads it.
//   - Invalid UTF-8: the same argument one level up. A name that is not
//     text cannot be compared, logged or restored reliably, and
//     normalising it would invent a name the source does not have.
//   - Absolute: "/etc/shadow" out of a listing is an instruction to
//     leave the root, and joining it to a root is the classic way that
//     instruction gets followed.
//   - A Windows-shaped absolute path ("C:\...", "\\server\share"): the
//     same instruction spelled for the other platform, which a
//     POSIX-only check reads as an ordinary relative name.
//   - A backslash anywhere: ambiguous, and ambiguity is the whole
//     problem. It is a path separator on one platform and an ordinary
//     character in a filename on another, so one name means two
//     different trees in two builds of this program, and a restore of
//     the wrong one is a silent, plausible-looking wrong answer. It is
//     REFUSED and reported rather than escaped or rewritten, so an
//     operator hears about the file instead of finding out at restore.
//   - A ".." element: traversal. Checked BEFORE cleaning, because
//     path.Clean happily resolves "a/../../b" to "../b" and a check
//     after cleaning has to reason about what the cleaner did.
//   - A trailing slash, a doubled slash, or a "." element: a
//     non-canonical spelling of a path this adapter would otherwise
//     store twice under two names. Cleaned, not refused - these are
//     spellings, not attacks - and the cleaned form is what comes back.
//
// Case folding is deliberately not one of the rules, and its absence is
// a decision rather than a gap. Two names differing only in case are one
// object on a case-insensitive source, so a run could store the same
// bytes twice under two identities; catching that would mean holding
// every path a run has seen in a set, which is memory linear in the
// source and exactly the allocation bounded enumeration exists to
// refuse. What matters for SAFETY is that no case variation reaches a
// different answer here than its lowercase spelling does, which is a
// property over arbitrary input and is fuzzed as one. Exclusion
// matching, where fold-blindness would let an operator's "do not back
// this up" be bypassed by spelling, folds case on backends that do not
// declare themselves case-sensitive; see Profile.ExcludeMatcher.
func SafeRelPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: the empty path names nothing", ErrUnsafePath)
	}

	if len(p) > maxPathBytes {
		return "", fmt.Errorf("%w: %d bytes long, over the %d-byte limit", ErrUnsafePath, len(p), maxPathBytes)
	}

	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: %q contains a NUL byte, which truncates the name in any layer that hands it to a C API", ErrUnsafePath, p)
	}

	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w: %q is not valid UTF-8, so it cannot be compared or restored as the name the source holds", ErrUnsafePath, p)
	}

	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: %q is absolute, and a source listing does not get to name a path outside the root", ErrUnsafePath, p)
	}

	if strings.Contains(p, `\`) {
		return "", fmt.Errorf(`%w: %q contains a backslash, which is a separator on one platform and a filename character on another, so the path names two different objects depending on who reads it`, ErrUnsafePath, p)
	}

	for _, element := range strings.Split(p, "/") {
		if element == ".." {
			return "", fmt.Errorf("%w: %q walks out of the source root through a %q element", ErrUnsafePath, p, "..")
		}
	}

	clean := path.Clean(p)
	if clean == "." || clean == "" {
		return "", fmt.Errorf("%w: %q resolves to the source root itself, which is not an object", ErrUnsafePath, p)
	}

	// The drive check runs on the CLEANED path and not on the input, and
	// the difference is not cosmetic: "./C:" is a relative spelling that
	// cleans to "C:", so a check on the input accepts a path that the
	// same function would refuse if it were handed its own output. The
	// fuzz target found exactly that, which is what fuzzing a sanitiser
	// for idempotence is for.
	if isWindowsDriveRelative(clean) {
		return "", fmt.Errorf("%w: %q names a Windows drive, which is an absolute path spelled so that a POSIX check reads it as a relative one", ErrUnsafePath, p)
	}

	// Belt and braces against a future edit to the rules above. Clean
	// cannot produce either of these from input that passed the element
	// scan, and the day it can, this is a refusal instead of an escape.
	if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q resolves to %q, which is outside the source root", ErrUnsafePath, p, clean)
	}

	return clean, nil
}

// isWindowsDriveRelative reports whether a path opens with a drive
// letter, which makes it absolute on Windows however it looks here.
//
// The backslash rule above already refuses `C:\x`; this catches `C:x`
// and `C:/x`, which contain no backslash and are still not relative
// paths on the platform that has drives.
func isWindowsDriveRelative(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}

	c := p[0]

	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Contains reports whether child is at or beneath root, both being
// already-safe slash paths.
//
// It is lexical, and lexical is the right answer here rather than a
// compromise: this adapter never resolves a symbolic link, so there is no
// filesystem question to ask. A link is described as a link and its
// target is checked with this function; nothing follows one.
func Contains(root, child string) bool {
	root = strings.Trim(path.Clean("/"+root), "/")
	child = strings.Trim(path.Clean("/"+child), "/")

	if root == "" {
		return true
	}

	return child == root || strings.HasPrefix(child, root+"/")
}

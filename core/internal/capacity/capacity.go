// Package capacity is FR-21's disk capacity guard: monitor a destination
// filesystem's free space, weigh it against a warning threshold, a critical
// threshold and a configurable safety margin, and refuse a transfer whose
// incoming artifact size means it cannot land safely.
//
// # The two rules this package exists to enforce
//
// FR-21 states two rules, and everything below is built around keeping both
// of them structurally true rather than merely documented:
//
//   - "Do not begin a transfer known not to fit safely." Admit (and its
//     convenience wrapper CheckBeforeTransfer) is the single place that
//     verdict is reached, and it is reached before a single byte moves: it
//     takes a Stat reading and an artifact size and returns either nil (safe
//     to proceed) or a typed *InsufficientCapacityError describing exactly
//     why not.
//   - "Do not silently violate retention to free space." This package
//     contains no deletion of any kind, of anything, anywhere: no file
//     removal, no call into another package's delete path, nothing that
//     could be mistaken for "free some room and retry". A full filesystem is
//     something this package reports, loudly, as a refusal; making room is
//     never this package's decision to make. If a caller ever wires this
//     guard's refusal into an automatic retention run, that wiring lives
//     outside this package and is a policy choice for whoever adds it, not
//     something capacity does on its own.
//
// # Why Thresholds is a plain struct instead of reading internal/config
//
// internal/config's BackupSet does not carry warning/critical/safety-margin
// fields yet (adding them is out of this package's scope; see this
// package's introducing PR for the exact fields proposed). Rather than
// import internal/config for a type it cannot get real values out of yet,
// this package defines Thresholds as the plain, caller-supplied struct it
// actually needs. Whoever wires FR-21 into config and the lifecycle engine
// later constructs a Thresholds from whatever config eventually holds; this
// package never has to change shape when that happens, because its inputs
// were never coupled to config's in the first place. The same reasoning is
// why the incoming artifact's size is taken as a plain int64 byte count
// rather than a transport.RemoteArtifact: this package has no need to know
// where that number came from, only that it is bytes.
//
// # Headroom arithmetic: why one artifact means one artifact's worth of space, not two
//
// It would be easy to assume the .partial file and its final copy briefly
// existing side by side (see internal/lifecycle/commit.go) means this
// package must reserve two artifacts' worth of space per transfer: one for
// the .partial file while it is being written, and a second for the final
// name once committed. That assumption is wrong, and it is wrong for a
// specific, load-bearing reason, not a hand-wave: commit.go's commitFile
// promotes a .partial file to its final name with os.Link followed by
// os.Remove of the old name (see linkWithoutClobbering), never with a copy.
// A hard link creates a second directory entry pointing at the *same
// inode*, so the same data blocks; it does not duplicate a single byte of
// content. For the brief window where both names resolve (after the link,
// before the old name is removed), the filesystem's block-usage accounting
// is identical to before the link and identical to after the remove: one
// artifact's blocks, referenced by a link count of two instead of one. The
// two-directory-entries state costs a few bytes of directory-inode
// metadata, nothing resembling a second copy of the artifact's content.
//
// So the peak local footprint of one artifact, across its whole
// DISCOVERED -> COMMITTED lifecycle, is its own size, once, from the moment
// TRANSFERRING starts writing the .partial file (which itself never exceeds
// the artifact's final size, since it is a straight copy, not something
// that grows past the source) through to COMMITTED. This package's
// Requirement is exactly:
//
//	RequiredBytes = ArtifactSizeBytes + Thresholds.SafetyMarginBytes
//
// not ArtifactSizeBytes*2. Doubling the requirement here would not make the
// guard safer, it would make it wrong in the conservative direction for the
// bytes that matter (rejecting transfers that fit comfortably) while doing
// nothing for the risks that are real and worth a margin for instead:
//
//   - The gap between when a remote listing captured an artifact's size and
//     when this guard is consulted, or between this guard's statfs reading
//     and the moment bytes actually start landing. FR-16-style drift is a
//     real, if rare, possibility this package cannot observe directly.
//   - Filesystem block-size rounding: a file's actual on-disk footprint is
//     its size rounded up to the underlying block size, which for a single
//     large backup artifact is negligible, but this package makes no
//     assumption about how large or small an artifact is.
//   - Anything else writing to the same filesystem that this package has no
//     visibility into: another backup set sharing the same physical volume,
//     an operator's unrelated process, the journal database's own file if
//     it happens to share a filesystem with the local destination. This
//     package checks one filesystem's capacity at one instant for one
//     artifact; it has no way to reserve space against concurrent writers,
//     and does not pretend to.
//
// Thresholds.SafetyMarginBytes exists precisely to be sized by the operator
// against those real risks, deliberately left as a plain byte count this
// package does not try to compute on its own.
//
// # Why Bavail (available), not Bfree (free)
//
// Stat's AvailableBytes field is populated from the statfs family's
// "available to an unprivileged process" count (f_bavail on Linux, f_bavail
// via unix.Statfs_t on Darwin), not the raw free-block count (f_bfree),
// wherever the two differ. Some filesystems (ext4 with a root-reserved
// percentage is the common case) hold back a slice of free blocks that only
// a privileged process can allocate into. Whether backup-manager itself
// runs as root is a deployment detail this package has no way to know, so
// it takes the conservative reading: AvailableBytes is what Admit's
// arithmetic uses, and it is never larger than FreeBytes.
package capacity

import "fmt"

// Stat is one filesystem capacity reading, as returned by StatPath.
//
// All three fields are byte counts, already multiplied out from whatever
// block size and block counts the underlying statfs call reported; nothing
// downstream of Stat needs to know block size exists.
type Stat struct {
	// TotalBytes is the filesystem's total capacity. It is carried for
	// observability (FR-23's "disk pressure" logging, FR-24's health
	// surface) only; nothing in this package's admission logic reads it.
	TotalBytes uint64

	// FreeBytes is every free block, including any a privileged process
	// could allocate into but an unprivileged one could not. Carried for
	// observability alongside TotalBytes; Admit does not use it either. See
	// the package doc's "Why Bavail, not Bfree" section for why
	// AvailableBytes, not this field, is what admission decisions are made
	// from.
	FreeBytes uint64

	// AvailableBytes is free space actually available to this process
	// (statfs's Bavail), the conservative number this package's admission
	// logic is built on.
	AvailableBytes uint64
}

// Thresholds is this package's caller-supplied configuration for FR-21.
// internal/config does not carry these fields yet; see the package doc for
// why this stays a plain struct rather than a config type, and see this
// package's introducing PR for the config fields proposed for a later
// change.
type Thresholds struct {
	// WarningFreeBytes is the free-space level, measured after a
	// hypothetical transfer would land, at or below which this package
	// reports Level Warning: worth logging or alerting on (FR-23's "disk
	// pressure", FR-24's health surface), but never by itself a reason to
	// refuse a transfer.
	WarningFreeBytes uint64

	// CriticalFreeBytes is the free-space floor, measured after a
	// hypothetical transfer would land, at or below which Admit refuses the
	// transfer outright. This is the hard rule FR-21 asks for: a transfer
	// that would itself be the thing that drops the filesystem to or below
	// this floor never begins.
	CriticalFreeBytes uint64

	// SafetyMarginBytes is added on top of the incoming artifact's own size
	// to compute how many bytes must be available right now for a transfer
	// to be admitted. See the package doc's headroom-arithmetic section for
	// what this margin is, and is not, meant to cover.
	SafetyMarginBytes uint64
}

// Validate reports whether t is internally consistent: WarningFreeBytes
// must be at or above CriticalFreeBytes, since free space is expected to
// cross the warning line first as it drops and the critical line only
// after. Thresholds with them reversed cannot be honored (Level would jump
// straight from OK to Critical, silently skipping Warning) and is refused
// as a caller/config mistake rather than acted on.
func (t Thresholds) Validate() error {
	if t.WarningFreeBytes < t.CriticalFreeBytes {
		return &ThresholdsInvalidError{Thresholds: t}
	}
	return nil
}

// Level classifies a filesystem's projected free space against Thresholds.
type Level int

const (
	// OK means projected free space is above WarningFreeBytes.
	OK Level = iota
	// Warning means projected free space is at or below WarningFreeBytes
	// but above CriticalFreeBytes. Worth surfacing; never a reason to
	// refuse a transfer by itself.
	Warning
	// Critical means projected free space is at or below CriticalFreeBytes,
	// including the degenerate case where the transfer does not even fit
	// today. Admit refuses whenever Level is Critical.
	Critical
)

func (l Level) String() string {
	switch l {
	case OK:
		return "OK"
	case Warning:
		return "WARNING"
	case Critical:
		return "CRITICAL"
	default:
		return fmt.Sprintf("Level(%d)", int(l))
	}
}

// Assessment is everything Assess or Admit computed on the way to a verdict,
// carried back whether or not the verdict was a refusal. A caller logs this
// for FR-23's disk-pressure line, or reads it for FR-24's health surface,
// regardless of whether the accompanying error was nil.
type Assessment struct {
	// Stat is the filesystem reading this assessment was computed from.
	Stat Stat

	// ArtifactSizeBytes is the incoming artifact's size, as given to Assess
	// or Admit. Zero is a legitimate input: it produces a pure "what is our
	// current standing" reading with no pending transfer, using only
	// Thresholds.SafetyMarginBytes as the reserve (see AssessCurrent).
	ArtifactSizeBytes int64

	// Thresholds is the configuration this assessment was computed under.
	Thresholds Thresholds

	// RequiredBytes is ArtifactSizeBytes plus Thresholds.SafetyMarginBytes:
	// how many bytes had to be available right now for Fits to be true.
	RequiredBytes uint64

	// Fits reports whether Stat.AvailableBytes covered RequiredBytes at
	// all, independent of the warning/critical thresholds. False here is
	// the "known not to fit" half of FR-21's rule.
	Fits bool

	// ProjectedAvailableBytes is Stat.AvailableBytes - RequiredBytes: the
	// free space this filesystem would have left immediately after this
	// transfer finished. Only meaningful when Fits is true; left at zero
	// when Fits is false, where ShortfallBytes is the meaningful field
	// instead.
	ProjectedAvailableBytes uint64

	// ShortfallBytes is RequiredBytes - Stat.AvailableBytes: how far short
	// of the requirement the filesystem currently is. Only meaningful
	// (nonzero) when Fits is false.
	ShortfallBytes uint64

	// Level classifies this assessment against Thresholds. It is Critical
	// whenever Fits is false, regardless of the numeric thresholds: not
	// fitting at all is always at least as severe as breaching the critical
	// floor.
	Level Level
}

// Assess computes an Assessment for one incoming artifact against one
// filesystem reading, without deciding anything: it never returns an error
// for "this does not fit" or "this would breach the critical threshold",
// only for genuinely invalid input (a negative size, inconsistent
// Thresholds, or a byte count that would overflow the arithmetic). Use
// Assess when the caller wants the numbers regardless of verdict (an FR-23
// disk-pressure log line, an FR-24 health reading); use Admit when the
// caller wants FR-21's actual go/no-go decision.
//
// Passing artifactSizeBytes as 0 produces a reading of the filesystem's
// current standing with no specific transfer pending, reserving only
// th.SafetyMarginBytes. AssessCurrent is a named convenience for exactly
// that call.
func Assess(stat Stat, artifactSizeBytes int64, th Thresholds) (Assessment, error) {
	if artifactSizeBytes < 0 {
		return Assessment{}, fmt.Errorf("capacity: artifact size must not be negative (got %d)", artifactSizeBytes)
	}
	if err := th.Validate(); err != nil {
		return Assessment{}, err
	}

	size := uint64(artifactSizeBytes)
	required := size + th.SafetyMarginBytes
	if required < size {
		return Assessment{}, fmt.Errorf(
			"capacity: artifact size (%d bytes) plus safety margin (%d bytes) overflows a 64-bit byte count",
			artifactSizeBytes, th.SafetyMarginBytes,
		)
	}

	a := Assessment{
		Stat:              stat,
		ArtifactSizeBytes: artifactSizeBytes,
		Thresholds:        th,
		RequiredBytes:     required,
	}

	if stat.AvailableBytes >= required {
		a.Fits = true
		a.ProjectedAvailableBytes = stat.AvailableBytes - required
		switch {
		case a.ProjectedAvailableBytes <= th.CriticalFreeBytes:
			a.Level = Critical
		case a.ProjectedAvailableBytes <= th.WarningFreeBytes:
			a.Level = Warning
		default:
			a.Level = OK
		}
		return a, nil
	}

	a.Fits = false
	a.ShortfallBytes = required - stat.AvailableBytes
	a.Level = Critical
	return a, nil
}

// AssessCurrent is Assess with no pending artifact: a pure "how is this
// filesystem doing right now" reading, reserving only th.SafetyMarginBytes.
// It is what FR-23's periodic disk-pressure logging and FR-24's health
// surface want when there is no specific transfer in flight to weigh.
func AssessCurrent(stat Stat, th Thresholds) (Assessment, error) {
	return Assess(stat, 0, th)
}

// Admit is FR-21's actual guard: "do not begin a transfer known not to fit
// safely," made mechanical. It returns a nil error only when a transfer of
// artifactSizeBytes may safely begin against stat under th; in every other
// case it returns the Assessment that produced the refusal alongside a
// typed *InsufficientCapacityError, so a caller gets both a decision and the
// numbers behind it in one call.
//
// "Safely begin" is Assess's Level being anything other than Critical:
// either the transfer does not fit today with the safety margin held back
// (Assessment.Fits is false), or it does fit but finishing it would itself
// drop the filesystem to or below th.CriticalFreeBytes. Both are refusals
// for the same reason, phrased for FR-21's two different failure shapes.
//
// Admit never deletes anything, never retries, and never lowers its own
// standards to let a transfer through: a refusal here is the caller's
// signal to report the condition (FR-23) and stop, never to go looking for
// something to delete to make room (see the package doc's second rule).
func Admit(stat Stat, artifactSizeBytes int64, th Thresholds) (Assessment, error) {
	a, err := Assess(stat, artifactSizeBytes, th)
	if err != nil {
		return Assessment{}, err
	}
	if a.Level == Critical {
		return a, &InsufficientCapacityError{Assessment: a}
	}
	return a, nil
}

// CheckBeforeTransfer is the one call a real caller integrating this
// package needs before starting a transfer for one artifact: it reads
// localDir's current filesystem capacity (StatPath) and applies Admit
// against that reading under th.
//
// localDir is expected to be a backup set's configured local destination
// directory (config.BackupSet.LocalPath in today's config, or wherever a
// future capacity-aware config surfaces it); this package does not import
// internal/config (see the package doc), so it takes the path as a plain
// string rather than a config type.
func CheckBeforeTransfer(localDir string, artifactSizeBytes int64, th Thresholds) (Assessment, error) {
	stat, err := StatPath(localDir)
	if err != nil {
		return Assessment{}, fmt.Errorf("capacity: reading filesystem capacity for %s: %w", localDir, err)
	}
	return Admit(stat, artifactSizeBytes, th)
}

// ThresholdsInvalidError reports a Thresholds value Validate refuses:
// WarningFreeBytes below CriticalFreeBytes, which this package cannot
// classify a Warning level out of (see Validate).
type ThresholdsInvalidError struct {
	Thresholds Thresholds
}

func (e *ThresholdsInvalidError) Error() string {
	return fmt.Sprintf(
		"capacity: invalid thresholds: warning_free_bytes (%d) must be >= critical_free_bytes (%d)",
		e.Thresholds.WarningFreeBytes, e.Thresholds.CriticalFreeBytes,
	)
}

// InsufficientCapacityError reports that Admit refused a transfer. It
// carries the full Assessment so a caller can log or report every number
// that went into the refusal, not just the fact of it.
type InsufficientCapacityError struct {
	Assessment Assessment
}

func (e *InsufficientCapacityError) Error() string {
	a := e.Assessment
	if !a.Fits {
		return fmt.Sprintf(
			"capacity: refusing to begin transfer: %d bytes available, %d required (artifact %d bytes + safety margin %d bytes), short by %d bytes",
			a.Stat.AvailableBytes, a.RequiredBytes, a.ArtifactSizeBytes, a.Thresholds.SafetyMarginBytes, a.ShortfallBytes,
		)
	}
	return fmt.Sprintf(
		"capacity: refusing to begin transfer: it would leave only %d bytes free, at or below the critical threshold of %d bytes (available %d, required %d)",
		a.ProjectedAvailableBytes, a.Thresholds.CriticalFreeBytes, a.Stat.AvailableBytes, a.RequiredBytes,
	)
}

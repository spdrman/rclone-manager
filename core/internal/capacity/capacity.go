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
// # Two different questions: disk free, and cap headroom
//
// Thresholds.CapBytes is the operator's ceiling on how much space this
// manager may occupy (issue #286). It answers a question the filesystem
// cannot: a volume can have a terabyte free while a 100 GB allowance has
// one gigabyte left, and a guard that only ever read statfs would admit a
// transfer that blows straight through the ceiling without noticing.
//
// So there are two headroom numbers, not one:
//
//	no cap  ->  headroom is the filesystem's AvailableBytes
//	cap set ->  headroom is CapBytes minus what this manager already uses
//
// and Assess takes whichever of the two is SMALLER, because a cap does not
// help if the disk fills first and a roomy disk is not permission to spend
// past the cap. Assessment.Binding names which of the two actually decided,
// so a caller can say out loud which denominator it drew: "80% of a 2 TB
// disk" and "80% of a 100 GB allowance" look identical on a bar and are not
// the same fact.
//
// The second number needs an input this package cannot take itself: how
// much this manager is currently using. That arrives as a Usage value the
// caller measures (core/service computes it from the state database's own
// record of artifact sizes, which measures THIS manager's consumption
// rather than everything sharing the mount). Usage carries a Known flag,
// and a configured cap with an unknown usage is a *UsageUnknownError rather
// than an assumed zero: reading "not measured" as "using nothing" would
// report the entire cap as free, which is a confident wrong number in the
// one direction that lets the ceiling be breached. With no cap configured
// there is no question for Usage to answer, so its zero value is safe
// exactly when it is meaningless.
//
// # Why Thresholds is a plain struct instead of reading internal/config
//
// This package defines Thresholds as the plain, caller-supplied struct it
// needs rather than importing internal/config: its inputs are byte counts,
// and coupling them to a config type would mean changing shape every time
// that type does. internal/config.Capacity is where an operator's values
// actually live today, and core/internal/app translates one into the other
// (app.New). The same reasoning is why the incoming artifact's size is
// taken as a plain int64 byte count rather than a transport.RemoteArtifact:
// this package has no need to know where that number came from, only that
// it is bytes.
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
	// CapBytes is the operator-configured ceiling on how many bytes this
	// manager may occupy on the filesystem it backs up to (issue #286).
	//
	// ZERO MEANS NO CAP, never a zero-byte ceiling. That sentinel is the
	// product default, so reading it literally would refuse every transfer
	// on every deployment that never set one, which is all of them until
	// an operator decides otherwise. internal/config.Capacity.CapBytes
	// carries the same sentinel with the same meaning and is where a
	// negative value is refused; by the time a number reaches this field
	// it is already a non-negative byte count.
	//
	// A configured cap changes what "headroom" means (see the package
	// doc's "Two different questions" section) and therefore requires a
	// Usage reading alongside it. It does not change what WarningFreeBytes
	// and CriticalFreeBytes mean: those two keep measuring whatever
	// headroom actually binds, so an operator who sets a 100 GB cap and a
	// 10 GB critical floor is refused with 10 GB of ALLOWANCE left, not
	// 10 GB of disk left.
	CapBytes uint64

	// WarningFreeBytes is the headroom level, measured after a
	// hypothetical transfer would land, at or below which this package
	// reports Level Warning: worth logging or alerting on (FR-23's "disk
	// pressure", FR-24's health surface), but never by itself a reason to
	// refuse a transfer.
	//
	// The name says "Free" because before the cap existed headroom and
	// free space were the same number. They are not any more: with a cap
	// configured this is measured against whichever of the two binds. The
	// field keeps its name so an existing config key and every caller that
	// sets it keep working, and this comment is where the difference is
	// stated.
	WarningFreeBytes uint64

	// CriticalFreeBytes is the headroom floor, measured after a
	// hypothetical transfer would land, at or below which Admit refuses the
	// transfer outright. This is the hard rule FR-21 asks for: a transfer
	// that would itself be the thing that drops the binding headroom to or
	// below this floor never begins. See WarningFreeBytes on the name.
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

// Usage is how much space this manager itself is currently occupying, as
// measured by the caller.
//
// It exists because Stat cannot answer that question. statfs reports what
// the whole filesystem has free, which on a shared volume is a number every
// other tenant moves too; enforcing a cap on THIS manager needs THIS
// manager's own consumption. core/service measures it by summing the sizes
// the state database already records for artifacts whose local copy the
// journal says exists, which is one aggregate query rather than a walk of
// the backup root, and which counts only files this manager put there.
//
// Known is not decoration. A caller that has not measured anything leaves
// it false, and Assess refuses (with *UsageUnknownError) rather than read
// the zero value as "using nothing" and hand back the entire cap as
// headroom. With no cap configured Usage is not consulted at all, so the
// zero value is safe exactly where it carries no meaning.
type Usage struct {
	// Bytes is this manager's own consumption, in bytes. Meaningful only
	// when Known is true.
	Bytes uint64

	// Known reports whether Bytes is a real measurement.
	Known bool
}

// Constraint names which of the two possible headroom questions actually
// decided an Assessment: the filesystem's free space, or the configured
// cap minus what this manager already uses.
//
// It is carried rather than derived at the display layer because the two
// are indistinguishable once they have been reduced to a percentage, and a
// caller that guessed would be asserting something it does not know, which
// is the defect issue #286 exists to stop repeating.
type Constraint int

const (
	// ConstraintDisk means the filesystem's own available space was the
	// smaller of the two, and is therefore the denominator in effect.
	// This is always the answer when no cap is configured.
	ConstraintDisk Constraint = iota

	// ConstraintCap means the configured cap's remaining allowance was the
	// smaller of the two.
	ConstraintCap
)

func (c Constraint) String() string {
	switch c {
	case ConstraintDisk:
		return "disk"
	case ConstraintCap:
		return "cap"
	default:
		return fmt.Sprintf("Constraint(%d)", int(c))
	}
}

// Level classifies the projected headroom against Thresholds.
//
// "Headroom", not "free space": since the cap landed (issue #286) the
// number these three are measured against is the smaller of the
// filesystem's available space and the configured cap's remaining
// allowance, so a deployment whose volume has a terabyte free can still
// read Critical. Assessment.Binding says which of the two decided.
type Level int

const (
	// OK means projected headroom is above WarningFreeBytes.
	OK Level = iota
	// Warning means projected headroom is at or below WarningFreeBytes
	// but above CriticalFreeBytes. Worth surfacing; never a reason to
	// refuse a transfer by itself.
	Warning
	// Critical means projected headroom is at or below CriticalFreeBytes,
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

	// Usage is this manager's own consumption, as given to Assess or Admit.
	// Consulted only when Thresholds.CapBytes is nonzero.
	Usage Usage

	// ArtifactSizeBytes is the incoming artifact's size, as given to Assess
	// or Admit. Zero is a legitimate input: it produces a pure "what is our
	// current standing" reading with no pending transfer, using only
	// Thresholds.SafetyMarginBytes as the reserve (see AssessCurrent).
	ArtifactSizeBytes int64

	// Thresholds is the configuration this assessment was computed under.
	Thresholds Thresholds

	// CapConfigured reports whether Thresholds.CapBytes named a real
	// ceiling (see that field: zero means no cap). Every field below whose
	// doc says "only when a cap is configured" is meaningless when this is
	// false.
	CapConfigured bool

	// CapHeadroomBytes is Thresholds.CapBytes - Usage.Bytes, floored at
	// zero: how much of the operator's allowance is left. Zero both when
	// the allowance is exactly spent and when it has already been
	// overrun, which are the same thing as far as admitting a transfer
	// goes. Meaningful only when CapConfigured is true.
	CapHeadroomBytes uint64

	// HeadroomBytes is the SMALLER of Stat.AvailableBytes and
	// CapHeadroomBytes, and is the number every verdict below was computed
	// against. With no cap configured it is simply Stat.AvailableBytes.
	HeadroomBytes uint64

	// Binding names which of the two produced HeadroomBytes. A caller
	// rendering a gauge has to say this out loud; see Constraint.
	Binding Constraint

	// LimitBytes and UsedBytes are the pair a gauge is drawn from, already
	// resolved against Binding so a caller never has to pick: with a cap
	// configured they are the cap and this manager's own consumption, and
	// with no cap they are the whole filesystem and how full it is
	// (TotalBytes - FreeBytes, the same "used" df prints).
	//
	// UsedBytes is deliberately NOT LimitBytes - HeadroomBytes. On a
	// filesystem with reserved blocks those two differ by the reserve, and
	// the honest answer to "how full is this volume" is the one that counts
	// the reserved blocks as used rather than as available headroom.
	LimitBytes uint64
	UsedBytes  uint64

	// RequiredBytes is ArtifactSizeBytes plus Thresholds.SafetyMarginBytes:
	// how many bytes of headroom had to exist right now for Fits to be
	// true.
	RequiredBytes uint64

	// Fits reports whether HeadroomBytes covered RequiredBytes at all,
	// independent of the warning/critical thresholds. False here is the
	// "known not to fit" half of FR-21's rule.
	Fits bool

	// ProjectedHeadroomBytes is HeadroomBytes - RequiredBytes: the headroom
	// that would be left immediately after this transfer finished, against
	// whichever constraint binds. Only meaningful when Fits is true; left
	// at zero when Fits is false, where ShortfallBytes is the meaningful
	// field instead.
	ProjectedHeadroomBytes uint64

	// ShortfallBytes is RequiredBytes - HeadroomBytes: how far short of the
	// requirement the binding constraint currently is. Only meaningful
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
func Assess(stat Stat, usage Usage, artifactSizeBytes int64, th Thresholds) (Assessment, error) {
	if artifactSizeBytes < 0 {
		return Assessment{}, fmt.Errorf("capacity: artifact size must not be negative (got %d)", artifactSizeBytes)
	}
	if err := th.Validate(); err != nil {
		return Assessment{}, err
	}

	capConfigured := th.CapBytes > 0
	if capConfigured && !usage.Known {
		return Assessment{}, &UsageUnknownError{CapBytes: th.CapBytes}
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
		Usage:             usage,
		ArtifactSizeBytes: artifactSizeBytes,
		Thresholds:        th,
		CapConfigured:     capConfigured,
		RequiredBytes:     required,
		HeadroomBytes:     stat.AvailableBytes,
		Binding:           ConstraintDisk,
		LimitBytes:        stat.TotalBytes,
		UsedBytes:         saturatingSub(stat.TotalBytes, stat.FreeBytes),
	}

	if capConfigured {
		// Floored at zero rather than allowed to wrap: a deployment that
		// is already past its ceiling has no allowance left, which is a
		// perfectly ordinary state to be in and must not read as an
		// enormous amount of headroom.
		a.CapHeadroomBytes = saturatingSub(th.CapBytes, usage.Bytes)
		a.LimitBytes = th.CapBytes
		a.UsedBytes = usage.Bytes
		// Whichever is smaller. A cap does not help if the disk fills
		// first, so the disk reading is never discarded, only compared
		// against.
		if a.CapHeadroomBytes < a.HeadroomBytes {
			a.HeadroomBytes = a.CapHeadroomBytes
			a.Binding = ConstraintCap
		}
	}

	if a.HeadroomBytes >= required {
		a.Fits = true
		a.ProjectedHeadroomBytes = a.HeadroomBytes - required
		switch {
		case a.ProjectedHeadroomBytes <= th.CriticalFreeBytes:
			a.Level = Critical
		case a.ProjectedHeadroomBytes <= th.WarningFreeBytes:
			a.Level = Warning
		default:
			a.Level = OK
		}
		return a, nil
	}

	a.Fits = false
	a.ShortfallBytes = required - a.HeadroomBytes
	a.Level = Critical
	return a, nil
}

// saturatingSub is a - b, floored at zero. Every subtraction in this
// package is between two uint64 byte counts that a wrap-around would turn
// into roughly eighteen exabytes of imaginary headroom, which is the exact
// shape of "a confident wrong number" this package refuses to produce.
func saturatingSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

// AssessCurrent is Assess with no pending artifact: a pure "how is this
// filesystem doing right now" reading, reserving only th.SafetyMarginBytes.
// It is what FR-23's periodic disk-pressure logging and FR-24's health
// surface want when there is no specific transfer in flight to weigh.
func AssessCurrent(stat Stat, usage Usage, th Thresholds) (Assessment, error) {
	return Assess(stat, usage, 0, th)
}

// Admit is FR-21's actual guard: "do not begin a transfer known not to fit
// safely," made mechanical. It returns a nil error only when a transfer of
// artifactSizeBytes may safely begin against stat under th; in every other
// case it returns the Assessment that produced the refusal alongside a
// typed *InsufficientCapacityError, so a caller gets both a decision and the
// numbers behind it in one call.
//
// "Safely begin" is Assess's Level being anything other than Critical:
// either the transfer does not fit in the binding headroom today with the
// safety margin held back (Assessment.Fits is false), or it does fit but
// finishing it would itself drop that headroom to or below
// th.CriticalFreeBytes. Both are refusals for the same reason, phrased for
// FR-21's two different failure shapes. "Binding headroom" is the disk's
// free space, or the configured cap's remaining allowance, whichever is
// smaller: see the package doc.
//
// Admit never deletes anything, never retries, and never lowers its own
// standards to let a transfer through: a refusal here is the caller's
// signal to report the condition (FR-23) and stop, never to go looking for
// something to delete to make room (see the package doc's second rule).
func Admit(stat Stat, usage Usage, artifactSizeBytes int64, th Thresholds) (Assessment, error) {
	a, err := Assess(stat, usage, artifactSizeBytes, th)
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
func CheckBeforeTransfer(localDir string, usage Usage, artifactSizeBytes int64, th Thresholds) (Assessment, error) {
	stat, err := StatPath(localDir)
	if err != nil {
		return Assessment{}, fmt.Errorf("capacity: reading filesystem capacity for %s: %w", localDir, err)
	}
	return Admit(stat, usage, artifactSizeBytes, th)
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
			"capacity: refusing to begin transfer: %d bytes of headroom %s, %d required (artifact %d bytes + safety margin %d bytes), short by %d bytes",
			a.HeadroomBytes, e.against(), a.RequiredBytes, a.ArtifactSizeBytes, a.Thresholds.SafetyMarginBytes, a.ShortfallBytes,
		)
	}
	return fmt.Sprintf(
		"capacity: refusing to begin transfer: it would leave only %d bytes of headroom %s, at or below the critical threshold of %d bytes (headroom %d, required %d)",
		a.ProjectedHeadroomBytes, e.against(), a.Thresholds.CriticalFreeBytes, a.HeadroomBytes, a.RequiredBytes,
	)
}

// against names the constraint that actually produced the refusal, so an
// operator is not sent to look at a disk that has a terabyte free when what
// ran out was the allowance they set themselves.
func (e *InsufficientCapacityError) against() string {
	a := e.Assessment
	if a.Binding == ConstraintCap {
		return fmt.Sprintf(
			"left under the configured cap of %d bytes (this manager is already using %d)",
			a.Thresholds.CapBytes, a.Usage.Bytes,
		)
	}
	return "available on the filesystem"
}

// UsageUnknownError reports that a cap is configured but the caller did not
// measure how much this manager is currently using, so no honest cap
// headroom can be computed. See Usage: the alternative is reading "not
// measured" as "using nothing", which reports the whole cap as free and
// makes the ceiling unenforceable exactly when it matters.
type UsageUnknownError struct {
	CapBytes uint64
}

func (e *UsageUnknownError) Error() string {
	return fmt.Sprintf(
		"capacity: a cap of %d bytes is configured but this manager's own current usage was not measured, so the remaining allowance cannot be computed",
		e.CapBytes,
	)
}

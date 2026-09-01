// This file is issue #286's manager-wide storage reading: one answer to
// "how much of my storage is this manager using, and out of what", taken
// from the one filesystem the backup root lives on.
//
// # Why this exists beside ListStorageStatus rather than inside it
//
// ListStorageStatus (storage.go) reports one entry per configured backup
// set, which is the right shape for a per-set detail view and the wrong
// shape for a dashboard gauge, for three separate reasons the live UGREEN
// install demonstrated all at once:
//
//   - A fresh instance has no backup sets, so the list is empty, and a
//     client summing an empty list gets zeros. Rendered as a fraction that
//     is 1 - 0/0, which is how "0 B of 0 B used · NaN%" reached a screen.
//   - Two backup sets on the same volume are two entries reporting the same
//     filesystem, and a client summing total_bytes across them reports
//     twice the disk. That is not a number that exists.
//   - The storage cap an operator sets is one ceiling for the whole
//     manager, not one per backup set, so there is no per-set entry it
//     could honestly be attached to.
//
// So this is a different question with a different answer, not a rollup of
// the other one. Both are served, and neither is derived from the other.
//
// # The honest unknown
//
// Known is false whenever no reading could be taken, and UnknownReason says
// which kind of "could not" it was. That is the whole point: an instance
// that does not know its capacity yet has to say so, in those words, rather
// than report zero bytes or a percentage of nothing. Nothing in this file
// ever fills a byte count in on an unknown reading, and Level stays empty,
// because an unread disk is not OK.
package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
)

// Denominator names what a storage gauge is a fraction OF.
//
// It is carried rather than inferred because the two readings are
// indistinguishable once reduced to a percentage: 80% of a 2 TB volume and
// 80% of a 100 GB allowance draw the same bar and mean entirely different
// things. A caller that guessed would be asserting something it does not
// know.
type Denominator string

const (
	// DenominatorDisk means limit_bytes is the whole filesystem the backup
	// root is on, which is what a deployment with no cap set is measured
	// against.
	DenominatorDisk Denominator = "disk"

	// DenominatorCap means limit_bytes is the operator's configured cap,
	// and used_bytes is this manager's own consumption rather than the
	// volume's.
	DenominatorCap Denominator = "cap"
)

// StorageUnknownReason classifies why a manager-wide reading could not be
// taken. Like StorageUnavailableReason beside it, it is a small closed set
// rather than an error string: the four situations below call for four
// different things from an operator, and none of them is served by a
// platform-specific errno.
type StorageUnknownReason string

const (
	// StorageUnknownNone is the value carried when Known is true.
	StorageUnknownNone StorageUnknownReason = ""

	// StorageUnknownNoBackupRoot means this configuration does not name,
	// and cannot derive, one directory whose filesystem to measure: either
	// there are no backup sets yet, or the sets there are share no ancestor
	// but "/" (see config.Capacity.BackupRoot for why "/" is refused rather
	// than measured). The fix is to finish setup, or to set
	// capacity.backup_root.
	StorageUnknownNoBackupRoot StorageUnknownReason = "no_backup_root"

	// StorageUnknownNotCreated means the backup root is known and does not
	// exist yet. This is the benign state between finishing setup and the
	// first cycle, which creates the destination on its way to writing into
	// it. Nothing here creates it: a read is a read.
	StorageUnknownNotCreated StorageUnknownReason = "not_created"

	// StorageUnknownUnreadable means the backup root exists as far as the
	// configuration goes but its filesystem could not be read: a vanished
	// mount, a permissions problem, failing hardware. This one needs a
	// person, and it is exactly the case that must never render as a benign
	// first run.
	StorageUnknownUnreadable StorageUnknownReason = "unreadable"

	// StorageUnknownMisconfigured means the reading itself was fine but the
	// configured capacity numbers cannot produce a verdict
	// (capacity.Thresholds.Validate), or a cap is configured and this
	// manager's own usage could not be measured, so no honest allowance
	// remains to report. Both are configuration or catalog faults rather
	// than disk faults.
	StorageUnknownMisconfigured StorageUnknownReason = "misconfigured"
)

// ManagerStorage is the one manager-wide storage reading: what the backup
// root's filesystem holds, what this manager itself accounts for, and which
// of the two the gauge is a fraction of.
type ManagerStorage struct {
	// Known reports whether every field below is meaningful. False leaves
	// every byte count at zero and Level empty, and UnknownReason says why.
	Known bool

	// UnknownReason classifies a Known of false, and is
	// StorageUnknownNone whenever it is true.
	UnknownReason StorageUnknownReason

	// MeasuredPath is the directory whose filesystem was actually statted,
	// reported whether or not the reading succeeded.
	//
	// It is here because of the container trap. The engine runs in a
	// container, and the filesystem that matters is the one the backup root
	// is on AS THE CONTAINER SEES IT, never the container's own rootfs and
	// never the host's "/". Measuring the wrong mount produces a confident
	// wrong number, which is worse than reporting nothing, because nobody
	// would notice. Naming the path is what makes it noticeable.
	MeasuredPath string

	// TotalBytes, FreeBytes and AvailableBytes are the filesystem's own
	// figures. AvailableBytes is what this process may actually use (df's
	// Avail) and is the number every verdict is decided from; FreeBytes
	// includes blocks only a privileged process could allocate into. See
	// internal/capacity.Stat.
	TotalBytes     uint64
	FreeBytes      uint64
	AvailableBytes uint64

	// CatalogBytes is how much space this manager is occupying, summed from
	// the sizes the state database already records for artifacts whose
	// local copy the journal says exists. It measures OUR consumption, not
	// the volume's: a `du` over the backup root would be slow on a large
	// tree and would count every file anything else put there.
	//
	// CatalogBytesKnown separates "we summed the catalog and it came to
	// zero", which is the correct answer on a deployment that has not
	// transferred anything yet, from "we could not read the catalog".
	CatalogBytes      uint64
	CatalogBytesKnown bool

	// OtherBytes is the reconciliation gap: how much of what the filesystem
	// reports as used this manager does NOT account for.
	//
	// It is surfaced rather than hidden on purpose. A large gap means
	// something other than this manager is writing into the same volume,
	// which is a thing an operator should know about a backup destination,
	// and it is also the only visible symptom of the one place the catalog
	// sum is known to over-count (internal/retention's prune removes a
	// local file without writing anything back to the journal, so a pruned
	// artifact keeps contributing its bytes; see
	// state.Journal.LocalBytesInUse). Floored at zero, since a catalog that
	// claims more than the volume holds is a discrepancy, not a negative
	// amount of other people's data.
	OtherBytes      uint64
	OtherBytesKnown bool

	// CapBytes is the operator's configured ceiling, zero when there is
	// none. See config.Capacity.CapBytes: zero is a sentinel meaning "no
	// cap", never a zero-byte ceiling.
	CapBytes uint64

	// Denominator, LimitBytes and UsedBytes are the gauge, already
	// resolved: with a cap they are the cap and this manager's own usage,
	// and without one they are the whole volume and how full it is.
	Denominator Denominator
	LimitBytes  uint64
	UsedBytes   uint64

	// HeadroomBytes is how much room is left before the binding constraint
	// refuses a transfer: the smaller of the filesystem's available space
	// and the cap's remaining allowance.
	HeadroomBytes uint64

	// BindingConstraint names which of the two actually produced
	// HeadroomBytes, and is a genuinely different fact from Denominator.
	//
	// Denominator answers "which question is this gauge drawn for", and is
	// settled by whether an operator configured a cap; it does not move
	// under a reader as numbers change. BindingConstraint answers "which
	// one would refuse the next transfer", and can be the disk even on a
	// capped deployment, which is worth saying: an operator watching their
	// allowance fill has no reason to expect the volume underneath to run
	// out first.
	BindingConstraint Denominator

	// WarningFreeBytes and CriticalFreeBytes are the configured thresholds
	// HeadroomBytes is weighed against. Both are zero when an operator has
	// set neither, which is the default and means there is no warning level
	// ahead of the hard refusal.
	WarningFreeBytes  uint64
	CriticalFreeBytes uint64

	// Level is internal/capacity.Level's own String() ("OK", "WARNING" or
	// "CRITICAL"), and is empty whenever Known is false.
	Level string
}

// ManagerStorage takes the manager-wide reading.
//
// Read-only in the strictest sense: it never creates the backup root, never
// deletes anything, and never triggers retention. A backup root that does
// not exist yet comes back as an honest unknown, and the first cycle is
// what creates it (internal/app's admitCapacity, on its way to writing into
// it).
func (b *BackupService) ManagerStorage(ctx context.Context) (ManagerStorage, error) {
	st := b.state.Load()
	cfg := st.inner.Config
	th := st.inner.Capacity

	out := ManagerStorage{
		CapBytes:          th.CapBytes,
		WarningFreeBytes:  th.WarningFreeBytes,
		CriticalFreeBytes: th.CriticalFreeBytes,
		Denominator:       DenominatorDisk,
	}
	if th.CapBytes > 0 {
		out.Denominator = DenominatorCap
	}

	root := cfg.EffectiveBackupRoot()
	if root == "" {
		out.UnknownReason = StorageUnknownNoBackupRoot
		return out, nil
	}
	out.MeasuredPath = root

	stat, statErr := statPath(root)
	if statErr != nil {
		out.UnknownReason = StorageUnknownNotCreated
		if !errors.Is(statErr, fs.ErrNotExist) {
			out.UnknownReason = StorageUnknownUnreadable
			// Logged for the same reason ListStorageStatus logs its own:
			// a backup destination that has stopped being readable is the
			// likeliest real cause of transfers being refused, and it has
			// to leave a trace even when nobody had the storage screen
			// open. The benign does-not-exist-yet case is deliberately not
			// logged, since it is true of every deployment before its
			// first cycle.
			b.logger.Error(ctx, "manager-storage",
				fmt.Errorf("reading the backup root %s: %w", root, statErr))
		}
		return out, nil
	}

	usage, usageErr := st.inner.LocalUsage(ctx)
	if usageErr != nil {
		b.logger.Error(ctx, "manager-storage", usageErr)
	}

	assessment, assessErr := capacity.AssessCurrent(stat, usage, th)
	if assessErr != nil {
		// Either the thresholds are inconsistent, or a cap is configured
		// and the catalog could not be summed. Both leave this reading
		// unknown rather than assessed from a guess, and both are already
		// on the log above or inside capacity's own error.
		out.UnknownReason = StorageUnknownMisconfigured
		b.logger.Error(ctx, "manager-storage",
			fmt.Errorf("assessing the backup root %s: %w", root, assessErr))
		return out, nil
	}

	out.Known = true
	out.UnknownReason = StorageUnknownNone
	out.TotalBytes = stat.TotalBytes
	out.FreeBytes = stat.FreeBytes
	out.AvailableBytes = stat.AvailableBytes
	out.CatalogBytes = usage.Bytes
	out.CatalogBytesKnown = usage.Known
	out.LimitBytes = assessment.LimitBytes
	out.UsedBytes = assessment.UsedBytes
	out.HeadroomBytes = assessment.HeadroomBytes
	out.Level = assessment.Level.String()
	out.BindingConstraint = DenominatorDisk
	if assessment.Binding == capacity.ConstraintCap {
		out.BindingConstraint = DenominatorCap
	}

	if usage.Known {
		out.OtherBytesKnown = true
		fsUsed := uint64(0)
		if stat.TotalBytes > stat.FreeBytes {
			fsUsed = stat.TotalBytes - stat.FreeBytes
		}
		if fsUsed > usage.Bytes {
			out.OtherBytes = fsUsed - usage.Bytes
		}
	}

	return out, nil
}

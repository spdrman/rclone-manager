// This file is issue #104 (B3.4)'s storage-pressure surfacing half:
// docs/EPIC-B-multi-nas.md §56 (Storage UX). internal/capacity (FR-21)
// already refuses a transfer that will not fit, and deliberately contains
// no deletion path of any kind (see that package's own doc) — this file
// does not add a second capacity check or a second policy. It only reads
// the SAME Assess/AssessCurrent machinery pipeline.go's admitCapacity
// already consults before every transfer, and hands the result to a
// caller outside core/ so the API/UI can display it honestly.
//
// Nothing in this file, or anywhere reachable from it, ever calls into
// internal/retention's apply path, and nothing here deletes anything.
// That is not merely a convention this file happens to follow: apps/
// common/webhost (the one caller outside core/ today) is a SEPARATE Go
// module and cannot import core/internal/retention at all — Go's own
// "internal" import rule blocks it regardless of what any go.work file
// says (see apps/common/webhost/doc.go) — so "storage pressure
// automatically triggers retention" is not just avoided here, it is
// structurally unreachable from the one place (the HTTP layer) that could
// otherwise be tempted to wire it that way.
package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
)

// StorageStatus is the plain, provider-agnostic shape of one backup set's
// FR-21 capacity assessment: docs/EPIC-B-multi-nas.md §56's exact display
// list (backup root, total/free capacity, the configured warning/critical
// thresholds) plus the resulting Level. It carries no deletion affordance
// of any kind — see this file's own package doc — and Available is false,
// with every numeric field left zero, when the local destination cannot
// be read yet (for example, a freshly created backup set whose LocalPath
// has not been created by a first cycle yet), so a caller can distinguish
// "we checked, storage is fine" from "we could not check" instead of the
// two looking identical.
type StorageStatus struct {
	BackupSetID string
	LocalPath   string

	// Available reports whether the byte counts and Level below are
	// meaningful. False means the reading could not be taken, and
	// UnavailableReason says which kind of "could not" it was.
	Available bool

	// UnavailableReason classifies why Available is false, and is
	// StorageUnavailableNone ("") whenever it is true. It exists because
	// "the directory does not exist yet, no cycle has run" and "the mount
	// this backup set writes to is gone" are the same shape on the wire
	// otherwise, and they could hardly be further apart in what an
	// operator should do about them.
	UnavailableReason StorageUnavailableReason

	TotalBytes uint64

	// FreeBytes is every free block on the filesystem, including any only a
	// privileged process could allocate into. It is here for a gauge to
	// display next to TotalBytes and for nothing else.
	//
	// It is NOT the number Level below was decided from, and it is not the
	// number FR-21's transfer refusal is decided from either. On ext4 with
	// the usual 5% root reserve it reads a few hundred GB higher than what
	// this process can actually use on a multi-TB volume. See
	// AvailableBytes.
	FreeBytes uint64

	// AvailableBytes is free space actually available to this process
	// (statfs's Bavail), which is what internal/capacity compares against
	// the thresholds below, and therefore the only one of the two free-space
	// numbers here that explains Level or a refused transfer. It matches
	// df's Avail column.
	//
	// It is carried ALONGSIDE FreeBytes rather than replacing it so that
	// nothing already reading free_bytes silently changes meaning under a
	// client: a gauge keeps its total/free pair, and the deciding number
	// arrives named for what it is.
	AvailableBytes uint64

	// WarningFreeBytes and CriticalFreeBytes are the configured thresholds
	// AvailableBytes is compared against.
	//
	// Both are zero in every deployment today. internal/config carries no
	// capacity fields yet (see internal/app/app.go's own note on that
	// deferral) and nothing outside tests assigns app.Service.Capacity, so
	// a running process reports 0 / 0 and, since Assess only reaches
	// Critical on a genuine shortfall, a Level of OK for anything short of
	// a full disk. FR-21's hard refusal is unaffected — it does not need a
	// configured floor to refuse a transfer that will not fit — but there
	// is no warning level to display until those config fields land, and
	// TestListStorageStatus_ProductionDefaults_ReportsZeroThresholdsAndOK
	// pins exactly that so the inert state is a stated fact rather than a
	// surprise.
	WarningFreeBytes  uint64
	CriticalFreeBytes uint64

	// Level is internal/capacity.Level's own String() ("OK", "WARNING" or
	// "CRITICAL"), empty when Available is false.
	Level string
}

// StorageUnavailableReason classifies a failed capacity reading. It is a
// small closed set rather than the underlying error's own text on purpose:
// an operator needs to know which of three genuinely different situations
// they are in, and a free-text errno string would both vary by platform and
// carry paths this type does not otherwise expose.
type StorageUnavailableReason string

const (
	// StorageUnavailableNone is the value carried when Available is true.
	StorageUnavailableNone StorageUnavailableReason = ""

	// StorageUnavailableNotCreated means LocalPath does not exist yet. This
	// is the benign first-run case: nothing before a backup set's first
	// successful cycle creates that directory (see pipeline.go's
	// admitCapacity for the same MkdirAll-on-first-use posture).
	StorageUnavailableNotCreated StorageUnavailableReason = "not_created"

	// StorageUnavailableUnreadable means LocalPath exists as far as the
	// configuration is concerned but its filesystem could not be read: a
	// vanished mount, a permissions problem, failing hardware. This one
	// needs an operator, and it is precisely the case that must never
	// render as a benign first run.
	StorageUnavailableUnreadable StorageUnavailableReason = "unreadable"

	// StorageUnavailableMisconfigured means the reading itself was fine but
	// the configured thresholds are not internally consistent, so no level
	// can honestly be computed from them (capacity.Thresholds.Validate).
	// It is a configuration fault, and it would apply to every backup set
	// at once.
	StorageUnavailableMisconfigured StorageUnavailableReason = "misconfigured"
)

// ListStorageStatus reports the current FR-21 capacity assessment for
// every configured backup set's local destination, computed with the
// exact same internal/capacity.AssessCurrent call pipeline.go's
// admitCapacity effectively performs (via CheckBeforeTransfer) before
// every transfer — this is a read of that same standing, not a second,
// possibly-disagreeing check. A backup set whose local destination cannot
// be statted yet is included with Available: false rather than causing
// the whole call to fail, so one not-yet-initialised set never hides the
// assessment for every other one.
func (b *BackupService) ListStorageStatus(ctx context.Context) ([]StorageStatus, error) {
	sets, err := b.ListBackupSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: listing backup sets for storage status: %w", err)
	}

	th := b.state.Load().inner.Capacity

	out := make([]StorageStatus, 0, len(sets))
	for _, bs := range sets {
		status := StorageStatus{
			BackupSetID:       bs.ID,
			LocalPath:         bs.LocalPath,
			WarningFreeBytes:  th.WarningFreeBytes,
			CriticalFreeBytes: th.CriticalFreeBytes,
		}

		stat, statErr := statPath(bs.LocalPath)
		if statErr != nil {
			status.UnavailableReason = StorageUnavailableNotCreated
			if !errors.Is(statErr, fs.ErrNotExist) {
				status.UnavailableReason = StorageUnavailableUnreadable
				// Logged, not just reported: a destination that has stopped
				// being readable is the likeliest real cause of transfers
				// being refused, and an operator reading logs after the fact
				// needs it to have left a trace even if nobody had the
				// storage screen open at the time. The benign
				// does-not-exist-yet case is deliberately not logged, since
				// it is true of every backup set before its first cycle.
				b.logger.Error(ctx, "storage-status",
					fmt.Errorf("backup set %s: reading %s: %w", bs.ID, bs.LocalPath, statErr))
			}
			out = append(out, status)
			continue
		}

		assessment, assessErr := capacity.AssessCurrent(stat, th)
		if assessErr != nil {
			status.UnavailableReason = StorageUnavailableMisconfigured
			b.logger.Error(ctx, "storage-status",
				fmt.Errorf("backup set %s: assessing %s: %w", bs.ID, bs.LocalPath, assessErr))
			out = append(out, status)
			continue
		}

		status.Available = true
		status.TotalBytes = stat.TotalBytes
		status.FreeBytes = stat.FreeBytes
		status.AvailableBytes = stat.AvailableBytes
		status.Level = assessment.Level.String()
		out = append(out, status)
	}

	return out, nil
}

// statPath is a seam over capacity.StatPath, in the same spirit as this
// package's other package-private test seams. A real filesystem cannot be
// made to report FreeBytes and AvailableBytes that differ on demand, and
// the difference between those two numbers is exactly what
// TestListStorageStatus_ReportsTheNumberTheLevelWasDecidedFrom is about.
var statPath = capacity.StatPath

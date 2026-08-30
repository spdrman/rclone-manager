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
	"fmt"

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

	// Available reports whether TotalBytes/FreeBytes/Level below are
	// meaningful. False means capacity.StatPath could not read LocalPath's
	// filesystem right now (most commonly: the directory does not exist
	// yet, since nothing before a backup set's first successful cycle
	// creates it — see pipeline.go's admitCapacity for the same
	// MkdirAll-on-first-use posture).
	Available bool

	TotalBytes uint64
	FreeBytes  uint64

	WarningFreeBytes  uint64
	CriticalFreeBytes uint64

	// Level is internal/capacity.Level's own String() ("OK", "WARNING" or
	// "CRITICAL"), empty when Available is false.
	Level string
}

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

		stat, statErr := capacity.StatPath(bs.LocalPath)
		if statErr != nil {
			out = append(out, status)
			continue
		}

		assessment, assessErr := capacity.AssessCurrent(stat, th)
		if assessErr != nil {
			out = append(out, status)
			continue
		}

		status.Available = true
		status.TotalBytes = stat.TotalBytes
		status.FreeBytes = stat.FreeBytes
		status.Level = assessment.Level.String()
		out = append(out, status)
	}

	return out, nil
}

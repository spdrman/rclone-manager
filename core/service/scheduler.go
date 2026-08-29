package service

import (
	"context"
	"time"
)

// RunOnSchedule is not yet implemented; see scheduler_test.go for its
// intended contract. This stub exists only so this package compiles while
// that contract is pinned down by a failing test first (issue #82/B4.1).
func (b *BackupService) RunOnSchedule(ctx context.Context, interval time.Duration) error {
	return nil
}

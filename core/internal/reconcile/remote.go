package reconcile

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// statRemote stats remotePath on source and reports whether the object
// exists. A transport.NotFound category is the only outcome I treat as a
// confirmed absence; every other error comes back unchanged, because FR-17
// must never guess an object is gone just because Stat failed for some
// other, ambiguous reason (a network error, a permission error, a
// cancelled context). See
// internal/lifecycle/remotedelete_test.go's
// TestDeleteRemote_RefusesWhenRemoteCannotBeStatted, whose own comment
// names this package as the one that owns deciding what an absent remote
// object means; a plain Stat failure is not that decision.
func statRemote(ctx context.Context, tp transport.Transport, source transport.Source, remotePath string) (transport.RemoteArtifact, bool, error) {
	art, err := tp.Stat(ctx, source, remotePath)
	if err == nil {
		return art, true, nil
	}
	if category, ok := transport.CategoryOf(err); ok && category == transport.NotFound {
		return transport.RemoteArtifact{}, false, nil
	}
	return transport.RemoteArtifact{}, false, err
}

// discoveredIdentity and currentIdentity mirror the same-named helpers in
// internal/lifecycle/remotedelete.go. FR-17's "changed identity" row is the
// reconciliation-time twin of FR-16's delete-time revalidation: both
// compare the same two capture points through the same
// model.CompareIdentity, so I deliberately reason about identity exactly
// the way DeleteRemote does. I reimplemented rather than exported
// remotedelete.go's versions because internal/lifecycle owns FR-15/16 and
// I am not allowed to modify it; this package owns FR-17 and builds the
// same shape from what it has on hand instead.
func discoveredIdentity(rec state.Record) (model.RemoteIdentity, error) {
	if rec.Remote.Size == nil {
		return model.RemoteIdentity{}, fmt.Errorf("no size was captured for this artifact at discovery; cannot compare remote identity now")
	}

	id := model.RemoteIdentity{
		Path:     rec.RemotePath,
		Size:     *rec.Remote.Size,
		Hash:     rec.Remote.Hash,
		HashAlg:  rec.Remote.HashAlg,
		StableID: rec.Remote.BackendID,
	}
	if rec.Remote.ModTime != nil {
		id.ModTime = rec.Remote.ModTime.Unix()
	}
	return id, nil
}

func currentIdentity(art transport.RemoteArtifact) model.RemoteIdentity {
	return model.RemoteIdentity{
		Path:     art.Path,
		Size:     art.Size,
		ModTime:  art.ModTime,
		Hash:     art.Hash,
		HashAlg:  string(art.HashAlg),
		StableID: art.ID,
	}
}

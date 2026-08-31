package app

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// haltReasonFor translates one backup set's cycle error into the durable
// refusal internal/state records, or reports that this error says nothing
// about whether the manager could connect.
//
// # Why only these two categories
//
// The fact this produces is "the manager could not connect to this backup
// set, and here is why", not the narrower "the host key changed". The
// wider one is worth having: an SSH login the server rejects stops the
// backups exactly as completely as a changed key does, and reporting it as
// a merely stale backup set is the same operator-facing hole one category
// along.
//
// It stops there, though, and the boundary is the point. HostVerification
// and Authentication are the only FR-22 categories that can only happen
// BEFORE a session exists: a missing directory, a permission failure on
// the remote path, a transient network error, an integrity failure are all
// reachable with a perfectly good connection, so none of them is evidence
// about the connection. Recording one of those as a refusal would put a
// sentence on an operator's screen that the manager cannot support.
//
// Cancellation and an unclassified error say nothing either, for the same
// reason from the other side: shutting a cycle down mid-flight is a
// decision this process made, not something a host did.
func haltReasonFor(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	category, ok := transport.CategoryOf(err)
	if !ok {
		return "", false
	}
	switch category {
	case transport.HostVerification:
		return state.HaltHostKeyChanged, true
	case transport.Authentication:
		return state.HaltAuthenticationFailed, true
	default:
		return "", false
	}
}

// recordConnectionOutcomes writes down what this cycle learned about
// whether each backup set can be connected to at all (issue #245).
//
// It is the durable counterpart of the transient computation
// evaluateAlerts already does, and it is deliberately driven off the same
// CycleReport so the two cannot disagree about which sets were refused.
// Three outcomes, not two, and the third is the one that keeps this
// honest:
//
//   - the set finished the cycle with no error at all: it connected, which
//     is the only evidence §77 invariant 5 accepts that a changed key has
//     been re-trusted, so any standing refusal is cleared;
//   - the set failed with a classified refusal (see haltReasonFor): the
//     refusal is recorded, replacing whatever was there before;
//   - anything else: this pass never got far enough to say either way, so
//     whatever is on record is left exactly as it is. Absence of the
//     refusal is not evidence the key verifies again, and inventing a
//     clear here is precisely how a real halt would quietly disappear on
//     the next unrelated failure.
//
// Nothing here returns an error, for the same reason evaluateAlerts does
// not: a journal write that failed is worth logging and is never worth
// failing a backup run over. Recording a refusal also never re-trusts a
// key, never retries the connection, and never suppresses the refusal it
// is describing; it only writes down what already happened.
//
// A backup set saved disabled is not cycled at all, so it appears in no
// CycleReport and nothing here touches it: whatever was last observed
// about it stands. That is deliberate rather than overlooked. Switching a
// set off is not evidence that its host key verifies again, and a refusal
// left standing on a set nobody is running says something true, which is
// that the last time this manager tried, it was refused, and nothing has
// happened since.
//
// A cancelled cycle writes nothing at all. Every write here would fail
// against a done context anyway, and the direction it leaves things in is
// the conservative one: a refusal that has since resolved stands for one
// more cycle rather than a real one being dropped on the way out.
func (s *Service) recordConnectionOutcomes(ctx context.Context, report CycleReport) {
	if ctx.Err() != nil {
		return
	}
	for _, set := range report.Sets {
		if set.Err == nil {
			if err := s.Journal.ClearBackupSetHalt(ctx, set.Set); err != nil {
				s.logger().Error(ctx, "halt-clear", err)
			}
			continue
		}
		reason, refused := haltReasonFor(set.Err)
		if !refused {
			continue
		}
		if err := s.Journal.RecordBackupSetHalt(ctx, set.Set, reason, s.now()); err != nil {
			s.logger().Error(ctx, "halt-record", err)
		}
	}
}

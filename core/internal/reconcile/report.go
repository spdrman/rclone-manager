package reconcile

import (
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// A pass produces two kinds of outcome, and this file's whole job is
// keeping them from sharing a channel.
//
// A Finding is something reconciliation concluded about an artifact,
// including the conclusion that nothing needed doing. An ArtifactError is
// reconciliation failing to conclude anything at all. Merged into one list,
// a run that could not stat half a backup set would be indistinguishable
// from a run that found half a backup set consistent, and only one of those
// two is reassuring.
//
// One error also never ends the pass. This runs at startup over every
// artifact in a set, and a single unreadable row must not stop the other
// ninety-nine from being brought back in line, so Report carries both lists
// and leaves the caller to decide what a partial pass is worth.

// Finding is what Reconcile decided about one artifact already in the
// journal. From is the lifecycle state I observed when I examined this
// artifact; To is the state it ended at, which equals From whenever I took
// no action.
type Finding struct {
	Artifact model.ArtifactID
	From     lifecycle.State
	To       lifecycle.State

	// NeedsInvestigation is set for FR-17's "changed identity" row: the
	// remote object at this artifact's path no longer matches, with strong
	// confidence, what I captured at discovery, while a delete is still
	// pending. I never act on this myself (From always equals To when this
	// is set): the row calls for the delete refused and the case
	// investigated, not resolved automatically one way or the other.
	NeedsInvestigation bool

	// Reason is a short, human-readable explanation of the finding,
	// suitable for a log line or an audit trail.
	Reason string
}

// Changed reports whether this finding actually moved the artifact's
// journal state, as opposed to confirming it already agreed with local
// files and remote state.
func (f Finding) Changed() bool { return f.From != f.To }

// ArtifactError is a per-artifact failure that did not stop the rest of a
// Reconcile call: one artifact I could not safely reach a verdict for,
// typically because its remote object could not be statted for a reason
// other than confirmed absence. This mirrors internal/discovery's
// CandidateError, for the same reason: one bad artifact must never hide
// every other artifact's result behind it.
type ArtifactError struct {
	Artifact model.ArtifactID
	Err      error
}

func (e ArtifactError) Error() string { return fmt.Sprintf("%s: %v", e.Artifact, e.Err) }
func (e ArtifactError) Unwrap() error { return e.Err }

// Report is everything one Reconcile call found and did: one Finding per
// artifact I examined, whether or not it needed any action, plus any
// per-artifact errors that did not abort the rest of the pass.
type Report struct {
	Findings []Finding
	Errors   []ArtifactError
}

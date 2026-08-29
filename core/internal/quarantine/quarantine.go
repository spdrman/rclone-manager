// Package quarantine turns already-loaded journal rows into the operator-
// facing picture Phase 4 (docs/EPIC.md) asks for: "[quarantine] needs to be
// visible, countable and actionable" (issue #28). It answers three
// questions an operator staring at a QUARANTINED or QUARANTINED_LOST
// artifact actually has:
//
//   - what is quarantined right now, and why, as far as the journal can
//     honestly say (see lifecycle.QuarantineReason for the limits of that
//     honesty);
//   - is this the recoverable kind (QUARANTINED, a fresh attempt can still
//     find the source) or the irrecoverable kind (QUARANTINED_LOST, the
//     source is already confirmed gone); and
//   - has this specific artifact been through quarantine before, so a
//     repeat failure reads as a repeat failure and not as a fresh one.
//
// Like health.ComputeBackupSetHealth and retention.GFSDecide, Summarize
// does no I/O of its own: it is a pure, deterministic function of
// already-loaded []state.Record (typically state.Journal.ListByBackupSet
// or ListByState) and the caller's clock reading. Actionability, the third
// leg of the issue's requirement, is lifecycle.ReleaseFromQuarantine: this
// package only reports, it never itself moves an artifact anywhere.
package quarantine

import (
	"sort"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Entry is one quarantined artifact's operator-facing summary.
type Entry struct {
	Artifact   model.ArtifactID
	RemotePath string
	LocalPath  string

	// State is always either lifecycle.Quarantined or
	// lifecycle.QuarantinedLost; Summarize never includes anything else.
	State lifecycle.State

	// Recoverable is false only for QuarantinedLost. It exists as its own
	// field, rather than asking a caller to compare State by hand, because
	// Phase 4 explicitly requires QUARANTINED_LOST to "surface differently
	// from ordinary quarantine": a caller building a dashboard or an alert
	// can branch on this without ever repeating the State comparison
	// itself.
	Recoverable bool

	// QuarantinedAt is when this artifact entered its current quarantine
	// state (state.Record.UpdatedAt at the moment it was loaded).
	QuarantinedAt time.Time

	// Age is how long ago that was, relative to the now Summarize was
	// given.
	Age time.Duration

	// TimesReturned is the record's RetryCount: how many times this
	// artifact has been sent back to DISCOVERED for a fresh attempt from
	// an exceptional state (today, exclusively via
	// lifecycle.ReleaseFromQuarantine's QUARANTINED -> DISCOVERED exit; see
	// that function's doc for why FR-22's future FAILED -> DISCOVERED exit
	// is expected to share the same counter rather than add a second one).
	TimesReturned int

	// Repeated is TimesReturned > 0: this artifact has been released from
	// quarantine before and is, once again, sitting in a quarantine state
	// now. This is Phase 4's "repeated quarantine of the same artifact
	// should be visible rather than looking like fresh failures each
	// time", answered as a single field a caller can filter or alert on
	// directly.
	Repeated bool

	// Reason is lifecycle.QuarantineReason's best-effort explanation of
	// why this artifact is quarantined. See that function's doc for
	// exactly what it can and cannot say.
	Reason string
}

// Report is everything Summarize found across the records it was given.
type Report struct {
	Entries []Entry

	// Total is len(Entries).
	Total int
	// Recoverable is how many entries are ordinary QUARANTINED (a fresh
	// attempt can still find the source).
	Recoverable int
	// Lost is how many entries are QUARANTINED_LOST (irrecoverable: no
	// copy exists anywhere).
	Lost int
	// RepeatOffenders is how many entries have Repeated == true.
	RepeatOffenders int
}

// quarantineStates are the two lifecycle states Summarize ever reports on.
// Every other state (including FAILED, which is a different, non-content
// exceptional state entirely, see machine.go) is silently excluded, not
// treated as an error: Summarize is meant to be handed a whole backup
// set's or a whole fleet's worth of records and pick out only the ones an
// operator needs this specific report for.
var quarantineStates = map[lifecycle.State]bool{
	lifecycle.Quarantined:     true,
	lifecycle.QuarantinedLost: true,
}

// Summarize builds a Report from records, the caller's already-loaded
// journal rows (state.Journal.ListByBackupSet or ListByState are the
// expected sources), as of now.
//
// A record whose State string is not one of the two quarantine states is
// skipped, not an error: unlike lifecycle.Validate, which must fail loudly
// on a state string it does not recognize at all (a schema drift or a
// hand-edited row is a bug worth catching), a plain non-quarantined record
// arriving here is the normal, expected case when a caller passes in a
// whole backup set's records rather than pre-filtering them itself.
//
// Entries are sorted deterministically: QUARANTINED_LOST first (the more
// severe, irrecoverable finding, so it can never be scrolled past), then
// by QuarantinedAt ascending within each group (the longest-neglected
// entries first), tie-broken by the artifact's own string form so two runs
// over the same input always render identically.
func Summarize(records []state.Record, now time.Time) Report {
	var report Report

	for _, rec := range records {
		st := lifecycle.State(rec.State)
		if !quarantineStates[st] {
			continue
		}

		entry := Entry{
			Artifact:      rec.Artifact,
			RemotePath:    rec.RemotePath,
			LocalPath:     rec.LocalPath,
			State:         st,
			Recoverable:   st == lifecycle.Quarantined,
			QuarantinedAt: rec.UpdatedAt,
			Age:           now.Sub(rec.UpdatedAt),
			TimesReturned: rec.RetryCount,
			Repeated:      rec.RetryCount > 0,
			Reason:        lifecycle.QuarantineReason(rec),
		}

		report.Entries = append(report.Entries, entry)
		report.Total++
		if entry.Recoverable {
			report.Recoverable++
		} else {
			report.Lost++
		}
		if entry.Repeated {
			report.RepeatOffenders++
		}
	}

	sort.SliceStable(report.Entries, func(i, j int) bool {
		a, b := report.Entries[i], report.Entries[j]
		if a.Recoverable != b.Recoverable {
			// false (QuarantinedLost, not Recoverable) sorts first.
			return !a.Recoverable
		}
		if !a.QuarantinedAt.Equal(b.QuarantinedAt) {
			return a.QuarantinedAt.Before(b.QuarantinedAt)
		}
		return a.Artifact.String() < b.Artifact.String()
	})

	return report
}

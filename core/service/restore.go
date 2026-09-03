package service

import (
	"github.com/spdrman/rclone-manager/core/internal/archive"
)

// ActionRestorePlacement is the durable operation an operator submits to
// make an archived copy readable again (EPIC E, FR-34).
//
// It is re-exported from internal/archive so that a caller outside core/,
// which cannot import an internal package, can still name the action it is
// looking at without spelling the string a second time.
const ActionRestorePlacement = archive.ActionRestore

// externallyExecutedActions are the operation actions whose work happens
// somewhere other than this process.
//
// The startup sweep exists because an operation left at queued or running
// by a dead process really was abandoned by it, and nothing else would
// ever move that row. That reasoning holds for every action executed by a
// goroutine here, and it is exactly backwards for a restore: the provider
// carries on restoring whether or not this process is alive, so the row is
// not stale, it is simply not finished, and its true state comes from
// asking the provider rather than from a sweep's assumption.
//
// A list rather than one string because the next action of this shape (a
// provider-side lifecycle transition, say) belongs here too, and the
// alternative is somebody discovering the sweep the hard way.
var externallyExecutedActions = []string{ActionRestorePlacement}

package service

import "github.com/spdrman/rclone-manager/core/internal/transport/rclone"

// This file is where a process decides who owns SIGINT and SIGTERM when
// rclone is embedded in it, and it is two forwarding calls because the
// decision is a pair rather than a switch: taking the signal back also
// takes on running what rclone registered to happen at exit.
//
// Splitting them across a seam like this looks like ceremony until you
// notice that nothing outside core/ can reach lib/atexit at all. FR-3
// keeps rclone inside one containment package, Go's internal rule keeps
// that package unreachable from apps/, and issue #212 is what the gap
// cost: a routine `docker stop` of the web container exited 143, which is
// the same defect issue #190 had already fixed for the CLI, reappearing
// in the one process that had no way to apply the fix.

// DisableSignalExit stops the embedded rclone from ending this process on
// SIGINT/SIGTERM, and RunExitHandlers is the obligation that comes with
// it. See internal/transport/rclone.DisableSignalExit for what rclone's
// lib/atexit actually does, when it arms itself, and why one transfer is
// enough to arm it for the life of the process.
//
// These two exist here for the same reason every other function in this
// package does (§7.2): a process outside core/ cannot import
// core/internal/transport/rclone at all, Go's own internal rule sees to
// that, and apps/generic's `serve` embeds rclone exactly the way
// cmd/backup-manager's `daemon` does, through Open below. Without a seam
// it would have had no way to take the signal back, and a routine
// `docker stop` of the web container exited 143 (issue #212, the same
// defect issue #190 fixed in the CLI). Forwarding, rather than importing
// lib/atexit here, is what keeps the rclone dependency inside the one
// containment package FR-3 puts it in.
//
// This is deliberately not called by Open itself. Who owns a signal is a
// decision about the whole process, and a library constructor is the
// wrong place to make it: a caller that has no signal handler of its own
// would be left with nothing handling SIGTERM at all. The command that
// installs the handler is the one that calls this.
func DisableSignalExit() { rclone.DisableSignalExit() }

// RunExitHandlers runs whatever the embedded rclone registered to happen
// at exit, and is the half of lib/atexit's contract a caller takes on by
// calling DisableSignalExit. See
// internal/transport/rclone.RunExitHandlers.
func RunExitHandlers() { rclone.RunExitHandlers() }

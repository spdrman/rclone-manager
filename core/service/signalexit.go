package service

import "github.com/spdrman/rclone-manager/core/internal/transport/rclone"

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

package rclone

import "github.com/rclone/rclone/lib/atexit"

// DisableSignalExit stops the embedded rclone from ending this process on
// SIGINT/SIGTERM, leaving this binary's own handler in charge of what a
// signal means (issue #190).
//
// rclone's lib/atexit installs a SIGINT/SIGTERM handler of its own the
// first time anything registers an at-exit function, and fs/operations
// registers one around every copy that is not in place, to remove the
// partial file if the process is killed. Unregistering that function when
// the copy finishes does not stop the handler, so one transfer arms it
// for the rest of the process's life. When a signal arrives it runs the
// registered functions and then calls os.Exit(128+signal), which is 143
// for SIGTERM: the status of a process that never handled the signal at
// all. A command that does handle it, cancels its context and shuts down
// cleanly was therefore still leaving with 143, and it was leaving
// whenever rclone won the race rather than when its own shutdown had
// finished, which is what could cost the shutdown log line as well.
//
// Calling this before any transfer stops that handler from ever being
// installed. Calling it afterwards stops the one already running. Both
// orders hold, and signalexit_test.go covers both, because lib/atexit
// arms itself lazily and takes a different route out in each case.
//
// This is deliberately not called by every command. `run` also cancels
// its context on a signal, but a one-shot cycle that was cut short did
// not complete, and 143 is the honest status for a process that was
// killed rather than asked to stop. The daemon is the case FR-1 and
// docs/deployment.md describe as a service an operator stops routinely.
//
// It unregisters nothing, so a caller that takes the signal away from
// rclone owes it RunExitHandlers on the way out.
func DisableSignalExit() { atexit.IgnoreSignals() }

// RunExitHandlers runs whatever the embedded rclone registered to happen
// at exit. It is the "you should also make sure you call Run in the
// normal exit path" half of lib/atexit's own contract, and the half a
// caller that has called DisableSignalExit now owns.
//
// In practice these are the partial-file cleanups fs/operations registers
// and unregisters around each copy, so by the time a command has finished
// its work there is usually nothing left registered, and a copy
// interrupted by context cancellation has already had its partial file
// removed by rclone's ordinary error path. Calling this is still not
// ceremony: it is what keeps "rclone no longer exits this process" from
// quietly dropping whatever a future rclone registers for longer.
func RunExitHandlers() { atexit.Run() }

// Command gotestwatch runs `go test` for one or more packages and bounds
// the whole invocation with a no-progress window derived from the run's
// own measured progress, instead of `go test`'s fixed -timeout (10 minutes
// by default).
//
// # Issue #256, and where it sits next to #247 and #248
//
// tests/crashmatrix's own harness invocations used to be bounded by one
// fixed constant, harnessTimeout = 45s. Issue #247 replaced it: the
// harness reports each real operation it starts and finishes, and a
// watchdog fails the run only when nothing has been reported for longer
// than a window derived from the slowest operation the run has itself
// already completed (see crash_matrix_test.go's progressTracker). A hang
// makes no progress; a slow machine makes slow progress; measuring
// progress is what tells them apart, where a fixed total cannot.
//
// That fix lives entirely inside the test code, one process down from
// `go test` itself. `go test`'s own -timeout is a second fixed deadline,
// applied to the whole test binary, and issue #256 is what noticed it:
// `core/tests/sftpintegration` run with -count=5 under real CPU
// oversubscription can still hit go test's default 10-minute timeout,
// killed as a package with no indication of which test was mid-flight,
// indistinguishable at the point of failure from a genuine hang.
//
// gotestwatch is #247's reasoning applied one layer further out. `go test
// -json` reports exactly the same kind of progress the harness does, as a
// structured stream of "a test started" / "a test finished" events instead
// of PROGRESS lines on stderr; gotestwatch watches that stream the way the
// crash-matrix watchdog watches the harness's, derives the same two
// bounds from it (a no-progress window and an overall backstop for a run
// that reports progress it never completes),
// and kills the whole `go test` process tree if either closes. `go test`'s
// own -timeout is switched off entirely (-timeout=0): there is no fixed
// deadline left to fall back on, so this is issue #256's first-preference
// fix, not its fallback.
//
// # Legibility, satisfied as a side effect
//
// The same event stream that decides when to stop also says what was
// running when it did: a trip names every test that had a "run" event
// with no matching pass/fail/skip yet. That is issue #256's third,
// minimum-bar fix ("say which test was running"), and it falls out of the
// mechanism rather than needing separate code.
//
// # A correctness property go test's own -timeout does not have
//
// go test's own -timeout fires as a panic inside the test binary itself;
// it does not kill anything the test binary went on to spawn (a
// tests/crashmatrix harness subprocess, a docker CLI invocation), so a
// process tree that was mid-hang when go test gave up on it can survive
// go test's own death and keep running. gotestwatch starts `go test` in
// its own process group (see run.go) and kills that whole group, so a
// tripped run cannot leave orphans behind the way relying on go test's own
// -timeout can.
//
// # What a trip does not say, and why (issue #533)
//
// The overall backstop used to name what it had caught: "That is a
// livelock, not a slow machine: the cap is derived from this run's own
// recent pace, so a genuinely, consistently slow run would have widened
// it." That reasoning is sound against a machine that is CONSISTENTLY slow
// and false against a bursty one, which is the run issue #533 was filed
// about: another project on the same machine was running a Playwright
// suite, the load was swinging between 11 and 72, a quiet stretch had set
// a fast pace, the cap tightened to match it, and one burst pushed the run
// past a cap that had been set while nothing was competing. The test the
// message named passed in 8.156s on its own, minutes later.
//
// Killing the run was right and that has not changed. The verdict was the
// defect, and it did not get a reworded verdict, because there is nothing
// to reword it from: a run that keeps reporting progress and never
// finishes produces the same arrivals gap for gap whether it is livelocked
// or being starved of the machine, so no rule over what the tracker can
// see separates the two (tracker_test.go's
// TestTracker_ALivelockAndALoadedHostProduceTheSameEvidence builds both
// from their own causes and finds the watchdog holding an identical trip).
// A guard that is merely wrong costs an hour; one that is wrong in a
// particular direction sends the next person hunting a deadlock that is
// not there.
//
// So a trip reports what it measured and stops: how long it ran against
// what bound, what was still running, the slowest recent gap the bound is
// made of, the slowest gap of the WHOLE run (which is how a reader sees a
// stall the rolling window has already forgotten, and is the one number
// that would have made the original incident obvious), and how late this
// host was running gotestwatch's own watchdog loop, which is the only
// reading this tool has on the machine rather than on the run.
//
// # Usage
//
//	gotestwatch <args to pass to `go test`, e.g. package paths, -count=N>
//	gotestwatch -step-floor=Xs -step-factor=Y -overall-floor=Xm -overall-factor=Y -- <go test args>
//
// The first form (no gotestwatch flags) is what scripts/ci-local.sh uses.
// The second is for tests and deliberate tuning: gotestwatch flags, if
// any, must precede a literal "--"; everything after it is passed to
// `go test` untouched except that -json and -timeout are always added by
// gotestwatch itself, so passing either explicitly is refused rather than
// silently overridden.
//
// Exit code 124 (the same convention coreutils' timeout(1) uses) means
// gotestwatch itself ended the run; any other non-zero code is `go test`'s
// own.
package main

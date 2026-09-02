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
// bounds from it (a no-progress window and an overall livelock backstop),
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

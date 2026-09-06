// Package service is core's public application-service boundary
// (docs/EPIC-B-multi-nas.md §3.3, §7.2): the ONLY core/ package apps/ or an
// HTTP/web host may import. See service.go's package doc for the full
// rationale; this file's own doc comments explain what each test proves.
package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"

	// Imported so this test binary's build info includes
	// github.com/rclone/rclone, exactly as app/version_test.go does, so
	// BuildVersion's EngineVersion has something real to report instead of
	// "unknown".
	_ "github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// resolveTestRetention fills in every backup set's resolved Retention, by
// running the real config.ResolveBackupSetRetention rather than a second
// copy of it (issue #333). These fixtures build a Config by hand rather
// than loading one, so nothing else fills that field in, and a set left at
// the zero Retention is not an unconfigured policy, it is a chain that
// keeps nothing and that DecideKeep refuses outright.
//
// It panics rather than returning: see internal/app's helper of the same
// name for why a fixture that quietly fails to resolve is worse than one
// that stops the test.
func resolveTestRetention(c *config.Config) *config.Config {
	if err := c.ResolveBackupSetRetention(); err != nil {
		panic("test fixture's retention does not resolve: " + err.Error())
	}
	return c
}

func testConfig(sources ...config.Source) *config.Config {
	protect := true
	return resolveTestRetention(&config.Config{
		Sources: sources,
		Retention: config.Retention{
			Timezone:             "UTC",
			WeekStartsOn:         "monday",
			DailyDays:            7,
			WeeklyMonths:         3,
			MonthlyMonths:        12,
			ProtectLastKnownGood: &protect,
		},
	})
}

func openTestJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func newTestService(t *testing.T, sources ...config.Source) *BackupService {
	t.Helper()
	return New(testConfig(sources...), openTestJournal(t), nil, nil)
}

// terminalStatusBudget is how long waitForTerminalStatus gives an
// asynchronous cycle to reach a terminal status.
//
// It is not a latency assertion, and no test that goes through this
// helper is about how fast a cycle is: they are about what the cycle did.
// The only failure a budget here can catch is an operation that will
// never reach a terminal status at all, and for that failure no budget is
// too generous. So the number comes from the two things that actually
// bound it, one on each side, rather than from how long a cycle happens
// to take on a quiet laptop (issues #384 and #385: it used to be a flat
// two seconds, and a correct cycle was measured at 2.65s on this machine
// while a cold build ran beside it).
//
// The floor comes from the product. internal/state.Open gives every
// journal PRAGMA busy_timeout = 5000 over a single connection
// (SetMaxOpenConns(1)), so one journal write inside a perfectly correct
// cycle may block five seconds on another connection's lock and still
// succeed, and a cycle makes several of them. Two services over one state
// directory is a supported case this package tests on purpose
// (TestClose_LeavesTheValidatorScriptsForAProcessStillUsingThem), so that
// is not hypothetical. Any budget under five seconds is below the
// product's own tolerated wait for one write. Thirty seconds is six of
// them, and it is also what backupsets_docker_test.go's second copy of
// this helper asked for to cover a real Docker+SSH round trip, which is
// why there is no longer a second copy.
//
// The ceiling comes from go test, which gives the whole package one
// deadline. A budget big enough to eat that deadline turns a wedged
// operation into a runtime panic and a dump of every goroutine in the
// binary instead of this test failing by name with the message below, so
// terminalStatusWait also caps any single wait at a share of what the
// binary actually has left. TestTerminalStatusBudget_StaysBetweenItsBounds
// (waitterminal_test.go) fails if either side of that is edited away.
//
// A var rather than a const so that test can shrink it and watch the
// timeout fire, instead of waiting out the real value; closeDrainTimeout
// (service.go) is a var for the same reason.
var terminalStatusBudget = 30 * time.Second

// terminalStatusDeadlineShare is the largest share of the test binary's
// own remaining time that one wait may spend. Four leaves three quarters
// of it for the binary to report the failure and finish the rest of the
// package, and it keeps shrinking if several waits time out in a row, so
// the sum of them can never reach the deadline. See terminalStatusBudget.
const terminalStatusDeadlineShare = 4

// terminalStatusWait returns how long a single wait may take, given the
// test binary's own deadline (t.Deadline(), which go test sets from
// -timeout and hasDeadline reports the presence of). It is split out from
// awaitTerminalStatus so the cap can be asserted directly rather than
// inferred from a passing test.
func terminalStatusWait(now time.Time, binaryDeadline time.Time, hasDeadline bool) time.Duration {
	if !hasDeadline {
		return terminalStatusBudget
	}
	if share := binaryDeadline.Sub(now) / terminalStatusDeadlineShare; share < terminalStatusBudget {
		return share
	}
	return terminalStatusBudget
}

// waitForTerminalStatus polls GetOperation until it reports a terminal
// status (completed or failed) or terminalStatusBudget expires. The
// background executor runs on its own goroutine (see SubmitRunCycle), so
// every test that cares about the outcome of that execution, rather than
// just the fact that a row was queued, has to observe it this way instead
// of assuming any particular timing.
func waitForTerminalStatus(t *testing.T, svc *BackupService, id string) Operation {
	t.Helper()
	op, err := awaitTerminalStatus(t, svc, id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// awaitTerminalStatus is waitForTerminalStatus's body with the failure
// returned rather than fataled, so the failure itself can be asserted on
// (waitterminal_test.go). A helper whose timeout nobody has ever watched
// fire is a helper nobody knows still works.
func awaitTerminalStatus(t *testing.T, svc *BackupService, id string) (Operation, error) {
	t.Helper()
	binaryDeadline, hasDeadline := t.Deadline()
	started := time.Now()
	budget := terminalStatusWait(started, binaryDeadline, hasDeadline)
	deadline := started.Add(budget)

	// The poll backs off instead of staying at its opening interval.
	// GetOperation reads the same journal the running cycle is writing to,
	// over the one connection internal/state.Open allows it, so every poll
	// is a poll that cycle cannot be using. A 2ms loop was affordable
	// against two seconds; held against thirty it would spend the whole
	// wait taking the connection away from the work it is waiting for.
	const (
		firstPoll = 2 * time.Millisecond
		maxPoll   = 50 * time.Millisecond
	)
	poll := firstPoll

	for {
		op, err := svc.GetOperation(context.Background(), id)
		if err != nil {
			return Operation{}, fmt.Errorf("GetOperation(%q): %v", id, err)
		}
		if op.Status == "completed" || op.Status == "failed" {
			return op, nil
		}
		if time.Now().After(deadline) {
			return Operation{}, fmt.Errorf(
				"operation %q did not reach a terminal status within %s (last status %q, %s). "+
					"That budget is not a latency bound: it is long enough that a correct cycle "+
					"cannot lose it on a loaded host, so reaching it means the operation is wedged, not slow",
				id, budget, op.Status, describeLiveProgress(op))
		}
		time.Sleep(poll)
		poll *= 2
		if poll > maxPoll {
			poll = maxPoll
		}
	}
}

// describeLiveProgress renders whatever the live progress registry still
// has for op, which is what tells a wedged cycle apart from a slow one at
// a glance: OperationProgress.Sequence exists precisely so a reader can
// see the service still producing readings even when the numbers in them
// have not moved (see that field's own doc).
func describeLiveProgress(op Operation) string {
	if op.Progress == nil {
		return "no live progress reading"
	}
	return fmt.Sprintf("live progress: stage %q, reading %d, taken %s ago",
		op.Progress.Stage, op.Progress.Sequence, time.Since(op.Progress.ObservedAt).Round(time.Millisecond))
}

// TestSubmitRunCycle_PersistsQueuedOperationBeforeReturning is the core of
// issue #94's durability claim, tested at the boundary layer apps/ actually
// calls: the operation the caller gets back already reports status "queued"
// and a non-empty ID, meaning the row exists before this function has done
// anything else (in particular before the asynchronous execution this same
// call kicks off has any chance to run).
func TestSubmitRunCycle_PersistsQueuedOperationBeforeReturning(t *testing.T) {
	svc := newTestService(t)

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-1",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	if op.ID == "" {
		t.Fatal("Operation.ID is empty")
	}
	if op.Status != "queued" {
		t.Errorf("Status = %q, want %q", op.Status, "queued")
	}
	if op.Actor != "alice" {
		t.Errorf("Actor = %q, want %q", op.Actor, "alice")
	}
	if op.Action != ActionRunCycle {
		t.Errorf("Action = %q, want %q", op.Action, ActionRunCycle)
	}
	if op.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// TestSubmitRunCycle_InvokesBackupServiceRunCycle is the acceptance
// criterion "API invokes BackupService" made concrete at this layer: given
// zero configured backup sets, the underlying core Service.RunCycle call
// this makes still actually runs (rather than, say, the boundary layer
// faking a response), which is observable as the operation reaching
// "completed" with a result summary describing a real cycle report.
func TestSubmitRunCycle_InvokesBackupServiceRunCycle(t *testing.T) {
	svc := newTestService(t)

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-1",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}

	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "completed" {
		t.Fatalf("Status = %q, want %q (Error = %q)", done.Status, "completed", done.Error)
	}
	if done.FinishedAt.IsZero() {
		t.Error("FinishedAt is zero on a completed operation")
	}
	if done.Result == "" {
		t.Error("Result is empty on a completed operation, want a cycle summary")
	}
}

// TestSubmitRunCycle_DuplicateIdempotencyKeyReturnsOriginalOperation is the
// acceptance criterion "duplicate idempotency key does not create duplicate
// work" at the boundary layer: resubmitting the same key returns the exact
// same operation identity and creation time, not a second one.
func TestSubmitRunCycle_DuplicateIdempotencyKeyReturnsOriginalOperation(t *testing.T) {
	svc := newTestService(t)
	req := RunCycleRequest{IdempotencyKey: "idem-1", Actor: "alice", ConfigRevision: svc.ConfigRevision()}

	first, err := svc.SubmitRunCycle(context.Background(), req)
	if err != nil {
		t.Fatalf("first SubmitRunCycle: %v", err)
	}
	second, err := svc.SubmitRunCycle(context.Background(), req)
	if err != nil {
		t.Fatalf("second SubmitRunCycle: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("second call returned ID %q, want the original %q", second.ID, first.ID)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("second call returned CreatedAt %v, want the original %v", second.CreatedAt, first.CreatedAt)
	}
}

// TestSubmitRunCycle_StaleConfigRevisionReturnsConflict proves the third
// acceptance criterion: submitting a configuration revision that is no
// longer current is rejected rather than silently applied against whatever
// configuration the service actually has loaded now. Two BackupService
// instances sharing one journal, built from deliberately different
// in-memory configs, stand in for "the config an operator's client last saw
// vs. the config the running process has now" without this package needing
// any live-reload machinery to prove the check works.
func TestSubmitRunCycle_StaleConfigRevisionReturnsConflict(t *testing.T) {
	journal := openTestJournal(t)

	oldSvc := New(testConfig(config.Source{Name: "alpha"}), journal, nil, nil)
	newSvc := New(testConfig(config.Source{Name: "alpha"}, config.Source{Name: "bravo"}), journal, nil, nil)

	if oldSvc.ConfigRevision() == newSvc.ConfigRevision() {
		t.Fatal("two services built from different configs report the same ConfigRevision; test fixture is not exercising anything")
	}

	_, err := newSvc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-stale",
		Actor:          "alice",
		ConfigRevision: oldSvc.ConfigRevision(), // stale: newSvc's config has since "changed"
	})
	if !errors.Is(err, ErrConfigRevisionStale) {
		t.Fatalf("SubmitRunCycle error = %v, want errors.Is(err, ErrConfigRevisionStale)", err)
	}

	// The conflict must be rejected before any operation row is created: a
	// client that fixes its request (supplies the current revision) and
	// resubmits under the exact same idempotency key must be treated as a
	// brand new request, not blocked by a half-created row the rejected
	// attempt left behind under that key.
	retry, err := newSvc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-stale",
		Actor:          "alice",
		ConfigRevision: newSvc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("retry with the current ConfigRevision under the same idempotency key: %v", err)
	}
	if retry.ID == "" {
		t.Error("retried SubmitRunCycle returned an empty Operation.ID")
	}
}

// TestSubmitRunCycle_CurrentConfigRevisionIsAccepted is the positive
// control for the test above: submitting the revision the service itself
// currently reports must succeed, so the conflict check is proven to
// distinguish stale from current rather than rejecting everything.
func TestSubmitRunCycle_CurrentConfigRevisionIsAccepted(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-1",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle with the current ConfigRevision: %v", err)
	}
}

func TestSubmitRunCycle_RequiresIdempotencyKeyAndConfigRevision(t *testing.T) {
	tests := []struct {
		name string
		req  func(svc *BackupService) RunCycleRequest
	}{
		{
			name: "missing idempotency key",
			req: func(svc *BackupService) RunCycleRequest {
				return RunCycleRequest{Actor: "alice", ConfigRevision: svc.ConfigRevision()}
			},
		},
		{
			name: "missing config revision",
			req: func(svc *BackupService) RunCycleRequest {
				return RunCycleRequest{IdempotencyKey: "idem-1", Actor: "alice"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(t)
			_, err := svc.SubmitRunCycle(context.Background(), tt.req(svc))
			if err == nil {
				t.Fatal("SubmitRunCycle: error = nil, want a validation error")
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("SubmitRunCycle error = %v, want errors.Is(err, ErrInvalidRequest)", err)
			}
			if errors.Is(err, ErrConfigRevisionStale) {
				t.Error("a missing config revision must be a validation error, not ErrConfigRevisionStale")
			}
		})
	}
}

func TestGetOperation_UnknownIDReturnsErrOperationNotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.GetOperation(context.Background(), "op_does_not_exist")
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("GetOperation error = %v, want errors.Is(err, ErrOperationNotFound)", err)
	}
}

func TestConfigRevision_StableForIdenticalConfig(t *testing.T) {
	a := New(testConfig(config.Source{Name: "alpha"}), openTestJournal(t), nil, nil)
	b := New(testConfig(config.Source{Name: "alpha"}), openTestJournal(t), nil, nil)
	if a.ConfigRevision() != b.ConfigRevision() {
		t.Errorf("ConfigRevision differs for two identically-configured services: %q vs %q", a.ConfigRevision(), b.ConfigRevision())
	}
	if a.ConfigRevision() == "" {
		t.Error("ConfigRevision is empty")
	}
}

func TestConfigRevision_ChangesWhenConfigContentChanges(t *testing.T) {
	a := New(testConfig(config.Source{Name: "alpha"}), openTestJournal(t), nil, nil)
	b := New(testConfig(config.Source{Name: "bravo"}), openTestJournal(t), nil, nil)
	if a.ConfigRevision() == b.ConfigRevision() {
		t.Error("ConfigRevision is identical for two differently-configured services")
	}
}

// TestBuildVersion_ReportsEverySection proves the wrapper actually maps
// every field app.BuildVersionInfo reports, and never invents "unknown"
// when the real value is discoverable (see the blank rclone import atop
// this file).
func TestBuildVersion_ReportsEverySection(t *testing.T) {
	v := BuildVersion("1.2.3", "abc123")
	if v.CoreVersion != "1.2.3" {
		t.Errorf("CoreVersion = %q, want %q", v.CoreVersion, "1.2.3")
	}
	if v.Commit != "abc123" {
		t.Errorf("Commit = %q, want %q", v.Commit, "abc123")
	}
	if v.GoVersion == "" {
		t.Error("GoVersion is empty")
	}
	if v.EngineVersion == "" || v.EngineVersion == "unknown" {
		t.Errorf("EngineVersion = %q, want a real pinned version", v.EngineVersion)
	}
}

// withStubbedRunCycle replaces the package-level runCycle seam (operations.go)
// with fn for the duration of the calling test, restoring the real one via
// t.Cleanup. Every test below that needs to control exactly when/how
// internal/app.Service.RunCycle "returns" (block it, panic it, time it)
// goes through this rather than exercising a real RunCycle pass, since none
// of them are testing RunCycle's own business logic — that belongs to
// internal/app's own test suite.
func withStubbedRunCycle(t *testing.T, fn func(inner *app.Service, ctx context.Context) app.CycleReport) {
	t.Helper()
	orig := runCycle
	t.Cleanup(func() { runCycle = orig })
	runCycle = fn
}

// TestSubmitRunCycle_SecondSubmissionWhileFirstInFlightIsRejected is issue
// #118 item 1's central regression test. SubmitRunCycle used to spawn a
// goroutine per newly-created operation with no synchronization at all, so
// two ordinary, unrelated requests (different idempotency keys, no
// attacker required — two browser tabs, or a double-click) could each
// start internal/app.Service.RunCycle concurrently against the very same
// BackupService, something internal/app/cycle.go's own doc says is meant
// to be impossible "by construction, not by a lock this package has to
// remember to take". This proves the fix: a second, genuinely new
// submission that arrives while the first is still actually inside
// RunCycle is rejected with ErrOperationAlreadyRunning rather than
// executing alongside it.
//
// # Rejecting, not queueing (the choice this test pins down)
//
// SubmitRunCycle's own doc documents this same decision; this comment
// explains why, next to the test that would break if it changed.
// Queueing the second submission to run once the first finishes was the
// other option on the table, and was rejected because it reintroduces,
// for the WAITER this time instead of the runner, exactly the
// request-lifetime-owns-operation-lifetime coupling §14 forbids: an HTTP
// handler would have to either block the request goroutine indefinitely
// on an unrelated operation's completion, or hand back a 202 for a queued
// slot this package cannot actually promise will ever come due (a
// deployment could stay mid-cycle indefinitely). Failing the just-created
// row fast and telling the caller so keeps every operation row's
// lifecycle bounded and every response honest about what actually
// happened, at the cost of the caller having to retry (with a fresh
// idempotency key) instead of this package silently waiting on its
// behalf.
func TestSubmitRunCycle_SecondSubmissionWhileFirstInFlightIsRejected(t *testing.T) {
	svc := newTestService(t)

	inFlight := make(chan struct{})
	release := make(chan struct{})
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		close(inFlight)
		<-release
		return app.CycleReport{}
	})

	first, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-first",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("first SubmitRunCycle: %v", err)
	}

	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("first operation never reached RunCycle")
	}

	_, err = svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-second",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if !errors.Is(err, ErrOperationAlreadyRunning) {
		t.Fatalf("second SubmitRunCycle (while the first is in flight) error = %v, want errors.Is(err, ErrOperationAlreadyRunning)", err)
	}

	close(release)

	firstDone := waitForTerminalStatus(t, svc, first.ID)
	if firstDone.Status != "completed" {
		t.Fatalf("first operation Status = %q, want %q (Error = %q)", firstDone.Status, "completed", firstDone.Error)
	}

	// The rejected submission's own row must have reached a terminal
	// status too, not be stuck at "queued" forever with nothing left to
	// ever move it: resubmitting the SAME idempotency key now (the first
	// operation, and with it the single-flight lock, has since been
	// released) must replay that row rather than start a third execution.
	rejectedReplay, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-second",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("resubmitting the rejected idempotency key: %v", err)
	}
	if rejectedReplay.Status != "failed" {
		t.Fatalf("rejected operation's row Status = %q, want %q", rejectedReplay.Status, "failed")
	}
	if rejectedReplay.Error == "" {
		t.Error("rejected operation's row Error is empty, want a reason")
	}
}

// TestExecuteRunCycle_RecoversFromPanicAndRecordsFailedOperation is issue
// #118 item 2's panic-recovery requirement: this PR is what first makes
// RunCycle reachable from a goroutine inside an always-on process (rather
// than a one-shot, supervised CLI run), so an unrecovered panic anywhere
// in its call graph would crash the entire persistent API server, not
// just fail one request. This test reaching its final assertion at all is
// itself part of what it proves: an unrecovered panic inside runCycle
// would crash this whole test binary, not just fail this one test.
func TestExecuteRunCycle_RecoversFromPanicAndRecordsFailedOperation(t *testing.T) {
	svc := newTestService(t)

	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		panic("boom: simulated panic inside RunCycle")
	})

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-panic",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}

	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "failed" {
		t.Fatalf("Status = %q, want %q after a panic inside RunCycle", done.Status, "failed")
	}
	if done.Error == "" {
		t.Error("Error is empty on an operation that failed via a recovered panic")
	}

	// The single-flight lock (item 1) must also have been released by the
	// panic-recovery path, not left held forever: a second operation must
	// still be able to run afterward.
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		return app.CycleReport{}
	})
	second, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-after-panic",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle after a recovered panic: %v", err)
	}
	secondDone := waitForTerminalStatus(t, svc, second.ID)
	if secondDone.Status != "completed" {
		t.Fatalf("second operation Status = %q, want %q (Error = %q)", secondDone.Status, "completed", secondDone.Error)
	}
}

// TestClose_WaitsForInFlightOperationToFinish is issue #118 item 2's
// WaitGroup-draining requirement: Close must not return (and must not
// close the journal a still-running executeRunCycle is about to write to)
// while an operation it started is still inside RunCycle.
func TestClose_WaitsForInFlightOperationToFinish(t *testing.T) {
	svc := newTestService(t)

	started := make(chan struct{})
	release := make(chan struct{})
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		close(started)
		<-release
		return app.CycleReport{}
	})

	_, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-close",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("operation never reached RunCycle")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- svc.Close() }()

	// Close must not return while executeRunCycle is still blocked on
	// release: give it a moment to (incorrectly) return early before
	// proving it hasn't.
	select {
	case <-closeDone:
		t.Fatal("Close returned before the in-flight operation finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the in-flight operation finished")
	}
}

// TestClose_TimesOutAndProceedsIfOperationDoesNotFinish is the other half
// of item 2's Close contract: an operation that never notices ctx was
// canceled (a genuinely stuck remote, in production) must not be able to
// hang a process shutdown forever. Close is documented to give up after
// closeDrainTimeout and close the journal anyway.
//
// block is deliberately never closed: the stubbed goroutine leaks for the
// rest of this test binary's life, standing in for an operation that never
// returns. That is safe to leave running because started (closed from
// inside the stub, before it blocks on block) proves the one-time read of
// the package-level runCycle var this goroutine needed has already
// happened by the time this test function returns; without that signal,
// restoring runCycle in withStubbedRunCycle's own t.Cleanup could race a
// still-in-flight first read of it from this leaked goroutine.
func TestClose_TimesOutAndProceedsIfOperationDoesNotFinish(t *testing.T) {
	svc := newTestService(t)

	origTimeout := closeDrainTimeout
	closeDrainTimeout = 20 * time.Millisecond
	t.Cleanup(func() { closeDrainTimeout = origTimeout })

	started := make(chan struct{})
	block := make(chan struct{})
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		close(started)
		<-block
		return app.CycleReport{}
	})

	_, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-timeout",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("operation never reached RunCycle")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- svc.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not time out and proceed while the operation was still hanging")
	}
}

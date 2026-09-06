package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file guards waitForTerminalStatus itself (service_test.go), which
// is the shared helper every operation-outcome test in this package goes
// through. Issues #384 and #385 are both the same defect in it: a flat
// two-second budget for a whole asynchronous RunCycle, which a correct
// cycle loses on a machine that is merely busy, in the one gate every
// merge in this repository rests on.
//
// A helper is easy to fix vacuously, so the fix is pinned from three
// sides here: the budget cannot be shrunk back under what the product
// itself tolerates, it cannot be widened into something that eats the
// test binary's own deadline, and its timeout is actually watched firing
// on an operation that genuinely never finishes.

// journalBusyTimeout is internal/state.Open's PRAGMA busy_timeout,
// copied here because it is a string literal in a package this one
// cannot read it from. It is the product's own answer to "how long may a
// single journal write legitimately block on another connection's lock",
// which is the floor terminalStatusBudget has to clear.
const journalBusyTimeout = 5 * time.Second

// terminalStatusBudgetCeiling is the other side. core/service already
// spends around thirty seconds of the package deadline go test hands it,
// and several waits timing out in a row must still leave room for the
// binary to report them by name rather than be killed mid-run.
const terminalStatusBudgetCeiling = 60 * time.Second

// TestTerminalStatusBudget_StaysBetweenItsBounds is the tripwire. It is
// deliberately a check on the constant rather than on behaviour: the
// point is that the next person to hit a slow machine cannot quietly put
// the two seconds back, and the person after that cannot answer a wedged
// operation by making the budget enormous.
func TestTerminalStatusBudget_StaysBetweenItsBounds(t *testing.T) {
	if terminalStatusBudget < journalBusyTimeout {
		t.Errorf("terminalStatusBudget = %s, which is under internal/state's own %s busy_timeout: "+
			"a single journal write in a correct cycle may block that long and still succeed, "+
			"so this budget can now fail an operation that is behaving exactly as designed",
			terminalStatusBudget, journalBusyTimeout)
	}
	if terminalStatusBudget < 30*time.Second {
		t.Errorf("terminalStatusBudget = %s, under the %s the Docker+SSH round trip in "+
			"backupsets_docker_test.go needs (it used to keep its own copy of this helper for exactly that)",
			terminalStatusBudget, 30*time.Second)
	}
	if terminalStatusBudget > terminalStatusBudgetCeiling {
		t.Errorf("terminalStatusBudget = %s, over the %s ceiling: a budget this large stops being "+
			"a readable failure and starts being a way to spend the package's whole go test deadline",
			terminalStatusBudget, terminalStatusBudgetCeiling)
	}
	if terminalStatusDeadlineShare < 2 {
		t.Errorf("terminalStatusDeadlineShare = %d: a share of one or less lets a single wait spend "+
			"everything the test binary has left, which is the panic this cap exists to avoid",
			terminalStatusDeadlineShare)
	}
}

// TestTerminalStatusWait_NeverSpendsMoreThanItsShareOfTheBinaryDeadline
// proves the ceiling is applied rather than merely described. Without
// this, "capped by t.Deadline()" would be a comment: the cap only ever
// bites on a binary already close to its deadline, which is precisely the
// run nobody is watching.
func TestTerminalStatusWait_NeverSpendsMoreThanItsShareOfTheBinaryDeadline(t *testing.T) {
	now := time.Now()

	if got := terminalStatusWait(now, time.Time{}, false); got != terminalStatusBudget {
		t.Errorf("with no binary deadline, wait = %s, want the full budget %s", got, terminalStatusBudget)
	}

	// Plenty of the binary's time left: the budget is what limits, not the share.
	roomy := now.Add(terminalStatusBudget * terminalStatusDeadlineShare * 10)
	if got := terminalStatusWait(now, roomy, true); got != terminalStatusBudget {
		t.Errorf("with %s of binary time left, wait = %s, want the full budget %s",
			roomy.Sub(now), got, terminalStatusBudget)
	}

	// Nearly out of it: the share is what limits, and it leaves the rest behind.
	tight := now.Add(4 * time.Second)
	got := terminalStatusWait(now, tight, true)
	if got != time.Second {
		t.Errorf("with 4s of binary time left, wait = %s, want 1s (a quarter of it)", got)
	}
	if got >= tight.Sub(now) {
		t.Errorf("wait = %s with only %s of binary time left: the whole point is to leave some",
			got, tight.Sub(now))
	}
}

// wedgeAnOperation writes an operation row and marks it running without
// anything anywhere executing it, which is exactly the shape
// waitForTerminalStatus's timeout exists to report: a process that died
// mid-operation leaves the row like this, and so does an executor
// goroutine that is stuck.
func wedgeAnOperation(t *testing.T, svc *BackupService, id string) {
	t.Helper()
	outcome, err := svc.journal.CreateOperation(context.Background(), state.OperationRequest{
		OperationID:    id,
		IdempotencyKey: "idem-" + id,
		Actor:          "test",
		ConfigRevision: svc.ConfigRevision(),
		Action:         ActionRunCycle,
		Parameters:     "{}",
		CreatedAt:      now(),
	})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if !outcome.Created {
		t.Fatalf("CreateOperation did not create %q", id)
	}
	if err := svc.journal.MarkOperationRunning(context.Background(), id, now()); err != nil {
		t.Fatalf("MarkOperationRunning: %v", err)
	}
}

// TestAwaitTerminalStatus_ReportsAnOperationThatNeverFinishes is the
// mutation the acceptance criteria of both #384 and #385 ask for, done
// as a test rather than as a hand edit so it stays done: with nothing
// executing the operation, the helper still gives up and still says what
// it means. The budget is shrunk for the duration (terminalStatusBudget
// is a var for this) rather than sitting through the real thirty
// seconds; what is being proven is that the timeout path works and
// reports the last status it saw, not how long it waits.
func TestAwaitTerminalStatus_ReportsAnOperationThatNeverFinishes(t *testing.T) {
	svc := newTestService(t)
	const id = "op_wedged"
	wedgeAnOperation(t, svc, id)

	restore := terminalStatusBudget
	terminalStatusBudget = 150 * time.Millisecond
	t.Cleanup(func() { terminalStatusBudget = restore })

	started := time.Now()
	op, err := awaitTerminalStatus(t, svc, id)
	if err == nil {
		t.Fatalf("awaitTerminalStatus returned no error for an operation nothing is executing (op = %+v)", op)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Errorf("gave up after %s, before the %s budget was even spent", elapsed, 150*time.Millisecond)
	}
	msg := err.Error()
	for _, want := range []string{id, "did not reach a terminal status", `"running"`, "wedged, not slow"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure message does not contain %q, so a reader cannot tell what happened:\n%s", want, msg)
		}
	}
}

// TestAwaitTerminalStatus_ReturnsAnOperationThatDidFinish is the positive
// control the test above needs. Without it a helper that had been broken
// into always returning an error would pass that one for entirely the
// wrong reason, which is the exact way a guard goes vacuous.
func TestAwaitTerminalStatus_ReturnsAnOperationThatDidFinish(t *testing.T) {
	svc := newTestService(t)
	const id = "op_finished"
	wedgeAnOperation(t, svc, id)
	if err := svc.journal.CompleteOperation(context.Background(), id, now(), "done"); err != nil {
		t.Fatalf("CompleteOperation: %v", err)
	}

	op, err := awaitTerminalStatus(t, svc, id)
	if err != nil {
		t.Fatalf("awaitTerminalStatus on a completed operation: %v", err)
	}
	if op.Status != "completed" {
		t.Errorf("Status = %q, want %q", op.Status, "completed")
	}
}

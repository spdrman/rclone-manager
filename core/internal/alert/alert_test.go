package alert_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/alert"
)

// The dispatcher's whole job is deciding when not to say anything, so every
// case below is a way of getting that wrong in one of two directions.
//
// Too quiet is the expensive one. A condition is silenced by having been
// delivered, never by having been seen, so a notifier that happened to be
// restarting when a backup set went stale must not mean nobody is ever
// told. The retry case walks that the whole way through, including the part
// afterwards where a delivered alert does stay quiet. And a pass that could
// not evaluate a condition reports a third answer rather than an absence,
// because reading "I did not look" as "it is fine now" resolves an alert
// that is still true.
//
// Too loud is the cheaper failure and still a real one. A condition that
// persists, a duplicate inside a single pass, and a delivery that keeps
// failing each get their own case, because a dispatcher that alerts once
// per poll is a dispatcher an operator switches off, and then it is quiet
// about everything.
//
// The last group is not about alerting at all. This pass is the tail of a
// backup cycle, so a sink that hangs must not take the daemon with it. The
// lock is not held across delivery and delivery is bounded anyway, and both
// are needed: releasing the lock on its own just moves the stall out of the
// dispatcher and into the cycle.

var epoch = time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)

// recordingSink is the one delivery mechanism a test installs. It is a
// stand-in for the platform's own local notification capability (the
// mechanism this work package selected), not a second mechanism: the
// dispatcher can only ever hold one, whatever it is.
type recordingSink struct {
	mu        sync.Mutex
	delivered []alert.Alert
	err       error
}

func (s *recordingSink) Deliver(_ context.Context, a alert.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, a)
	return s.err
}

func (s *recordingSink) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delivered)
}

func (s *recordingSink) at(i int) alert.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered[i]
}

func staleCondition(scope string) alert.Condition {
	return alert.Condition{Kind: alert.StaleBackup, Scope: scope, Detail: "no known-good backup within the stale threshold"}
}

// TestObserve_StaleTransitionFiresExactlyOneAlert is the Behavioral
// Contract's own example: crossing into STALE fires one alert, and the
// same unresolved condition observed again on the next pass fires
// nothing.
func TestObserve_StaleTransitionFiresExactlyOneAlert(t *testing.T) {
	sink := &recordingSink{}
	d := alert.NewDispatcher(sink, nil)

	fired := d.Observe(context.Background(), []alert.Condition{staleCondition("production/postgres-primary")}, nil, epoch)
	if len(fired) != 1 {
		t.Fatalf("first pass fired %d alerts, want exactly 1", len(fired))
	}
	if sink.count() != 1 {
		t.Fatalf("sink received %d alerts on the first pass, want exactly 1", sink.count())
	}
	got := sink.at(0)
	if got.Kind != alert.StaleBackup {
		t.Errorf("Kind = %q, want %q", got.Kind, alert.StaleBackup)
	}
	if got.Scope != "production/postgres-primary" {
		t.Errorf("Scope = %q, want the backup set the condition named", got.Scope)
	}
	if got.Title == "" || got.Message == "" {
		t.Errorf("Alert = %+v, want a non-empty Title and Message an operator can read", got)
	}
	if !got.ObservedAt.Equal(epoch) {
		t.Errorf("ObservedAt = %s, want the caller's clock reading %s", got.ObservedAt, epoch)
	}

	// Three more passes with the condition still unresolved.
	for pass := 2; pass <= 4; pass++ {
		if fired := d.Observe(context.Background(), []alert.Condition{staleCondition("production/postgres-primary")}, nil, epoch.Add(time.Duration(pass)*time.Hour)); len(fired) != 0 {
			t.Fatalf("pass %d fired %d alerts, want 0: an unresolved condition must not re-alert on every poll", pass, len(fired))
		}
	}
	if sink.count() != 1 {
		t.Fatalf("sink received %d alerts in total, want exactly 1", sink.count())
	}
}

// TestObserve_ResolvedConditionAlertsAgainWhenItRecurs proves the
// de-duplication is keyed on the condition rather than latched forever:
// once the condition stops being observed it is forgotten, so a genuine
// recurrence is a fresh alert.
func TestObserve_ResolvedConditionAlertsAgainWhenItRecurs(t *testing.T) {
	sink := &recordingSink{}
	d := alert.NewDispatcher(sink, nil)

	d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, nil, epoch)
	d.Observe(context.Background(), nil, nil, epoch.Add(time.Hour))
	d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, nil, epoch.Add(2*time.Hour))

	if sink.count() != 2 {
		t.Fatalf("sink received %d alerts, want 2 (fired, resolved, fired again)", sink.count())
	}
}

// TestObserve_ScopeIsPartOfTheDeduplicationKey proves two different
// backup sets in the same condition each get their own alert, rather than
// the first one suppressing the second.
func TestObserve_ScopeIsPartOfTheDeduplicationKey(t *testing.T) {
	sink := &recordingSink{}
	d := alert.NewDispatcher(sink, nil)

	fired := d.Observe(context.Background(), []alert.Condition{
		staleCondition("production/pg"),
		staleCondition("production/mysql"),
	}, nil, epoch)

	if len(fired) != 2 {
		t.Fatalf("fired %d alerts, want 2: two different backup sets are two different conditions", len(fired))
	}
}

// TestObserve_DuplicateConditionsInOnePassFireOnce proves the same
// condition offered twice in a single evaluation pass is still one alert.
func TestObserve_DuplicateConditionsInOnePassFireOnce(t *testing.T) {
	sink := &recordingSink{}
	d := alert.NewDispatcher(sink, nil)

	fired := d.Observe(context.Background(), []alert.Condition{
		staleCondition("production/pg"),
		staleCondition("production/pg"),
	}, nil, epoch)

	if len(fired) != 1 {
		t.Fatalf("fired %d alerts, want 1", len(fired))
	}
}

// TestObserve_DeliveryFailureDoesNotStormOnEveryPass proves a sink that
// fails (a notification daemon that is down, say) cannot turn one
// unresolved condition into one delivery attempt per poll: the retry in
// TestObserve_RetriesAConditionItCouldNotDeliver is rate-limited, not one
// attempt per pass. Five passes inside the first backoff window is one
// attempt.
func TestObserve_DeliveryFailureDoesNotStormOnEveryPass(t *testing.T) {
	sink := &recordingSink{err: errors.New("capability not supported by this platform adapter")}
	d := alert.NewDispatcher(sink, nil)

	for pass := 0; pass < 5; pass++ {
		d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, nil, epoch.Add(time.Duration(pass)*time.Second))
	}

	if sink.count() != 1 {
		t.Fatalf("sink saw %d delivery attempts, want exactly 1: retries are rate limited, not one per poll", sink.count())
	}
}

// TestObserve_RetriesAConditionItCouldNotDeliver is M2's contract: a
// condition is silenced by having been DELIVERED, never by having been
// seen. A notification daemon that happens to be restarting when a backup
// set goes stale must not mean nobody is ever told, for as long as the
// condition lasts.
func TestObserve_RetriesAConditionItCouldNotDeliver(t *testing.T) {
	sink := &recordingSink{err: errors.New("notification daemon is restarting")}
	d := alert.NewDispatcher(sink, nil)

	cond := []alert.Condition{staleCondition("production/pg")}

	if fired := d.Observe(context.Background(), cond, nil, epoch); len(fired) != 0 {
		t.Fatalf("Observe returned %d alerts, want 0: the return value is what was delivered, and delivery failed", len(fired))
	}
	if sink.count() != 1 {
		t.Fatalf("sink saw %d attempts on the first pass, want 1", sink.count())
	}

	// The condition is still unresolved, and now the channel is back.
	sink.setErr(nil)
	fired := d.Observe(context.Background(), cond, nil, epoch.Add(time.Hour))
	if len(fired) != 1 {
		t.Fatalf("Observe returned %d alerts on the retry, want 1: an observed but undelivered condition must be retried", len(fired))
	}
	if sink.count() != 2 {
		t.Fatalf("sink saw %d attempts, want 2", sink.count())
	}

	// Delivered is delivered: no further attempts while it stays true.
	d.Observe(context.Background(), cond, nil, epoch.Add(2*time.Hour))
	d.Observe(context.Background(), cond, nil, epoch.Add(3*time.Hour))
	if sink.count() != 2 {
		t.Fatalf("sink saw %d attempts, want still 2 once the alert was delivered", sink.count())
	}
}

// TestObserve_UnevaluatedConditionIsNeitherResolvedNorReAlerted is M3's
// contract at this package's own boundary: a pass that could not evaluate
// a condition says so, and that is a third answer, not a quiet "it is
// fine now".
func TestObserve_UnevaluatedConditionIsNeitherResolvedNorReAlerted(t *testing.T) {
	sink := &recordingSink{}
	d := alert.NewDispatcher(sink, nil)

	cond := staleCondition("production/pg")
	d.Observe(context.Background(), []alert.Condition{cond}, nil, epoch)
	if sink.count() != 1 {
		t.Fatalf("sink saw %d alerts on the first pass, want 1", sink.count())
	}

	// Two passes that could not look at this condition at all.
	unknown := []alert.Subject{cond.Subject()}
	d.Observe(context.Background(), nil, unknown, epoch.Add(time.Hour))
	d.Observe(context.Background(), nil, unknown, epoch.Add(2*time.Hour))

	// And then it can look again, and it is still true.
	d.Observe(context.Background(), []alert.Condition{cond}, nil, epoch.Add(3*time.Hour))
	if sink.count() != 1 {
		t.Fatalf("sink saw %d alerts, want still 1: a pass that could not look must not resolve a condition", sink.count())
	}

	// Positive control for that 1: the same dispatcher, sink and condition
	// DO produce a second alert when a pass that COULD look reports the
	// condition gone and it later comes back. Without this, "still 1"
	// would also be what a dispatcher that had stopped alerting entirely
	// would print.
	d.Observe(context.Background(), nil, nil, epoch.Add(4*time.Hour))
	d.Observe(context.Background(), []alert.Condition{cond}, nil, epoch.Add(5*time.Hour))
	if sink.count() != 2 {
		t.Fatalf("sink saw %d alerts, want 2: a genuinely resolved condition that recurs is a fresh alert", sink.count())
	}
}

// blockingSink hangs on the one scope it was told to hang on, and
// delivers everything else immediately, so a test can watch what the
// dispatcher does for OTHER conditions while one notification is stuck.
type blockingSink struct {
	blockScope string
	entered    chan struct{}
	release    chan struct{}
	deadline   chan time.Time
}

func newBlockingSink(blockScope string) *blockingSink {
	return &blockingSink{
		blockScope: blockScope,
		entered:    make(chan struct{}, 8),
		release:    make(chan struct{}),
		deadline:   make(chan time.Time, 8),
	}
}

func (s *blockingSink) Deliver(ctx context.Context, a alert.Alert) error {
	deadline, _ := ctx.Deadline()
	s.deadline <- deadline

	if a.Scope != s.blockScope {
		return nil
	}
	s.entered <- struct{}{}
	<-s.release
	return nil
}

// TestObserve_DoesNotHoldItsLockWhileASinkRuns is M4's contract. The
// alerting pass is the last step of a backup cycle, so a notifier that
// hangs under the dispatcher's own lock stops the daemon making backup
// progress. A second pass must be able to complete while the first one's
// delivery is still in flight.
func TestObserve_DoesNotHoldItsLockWhileASinkRuns(t *testing.T) {
	sink := newBlockingSink("production/pg")
	d := alert.NewDispatcher(sink, nil)

	go d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, nil, epoch)

	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was never called")
	}

	// The sink is now hung inside Deliver. A second, unrelated pass must
	// still complete.
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Observe(context.Background(), []alert.Condition{staleCondition("production/mysql")}, nil, epoch)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second Observe blocked behind a hung sink: the dispatcher is holding its lock across delivery")
	}

	close(sink.release)
}

// TestObserve_BoundsEveryDeliveryWithADeadline proves the dispatcher
// supplies the bound the Sink contract does not ask for. Without it,
// "does not hold the lock" only moves the stall from the dispatcher to
// the cycle: a sink with no timeout of its own hangs the pass either way.
func TestObserve_BoundsEveryDeliveryWithADeadline(t *testing.T) {
	sink := newBlockingSink("production/pg")
	d := alert.NewDispatcher(sink, nil)

	go d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, nil, epoch)

	var deadline time.Time
	select {
	case deadline = <-sink.deadline:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was never called")
	}
	close(sink.release)

	if deadline.IsZero() {
		t.Fatal("the context handed to the sink has no deadline: a hung notifier would stall the pass forever")
	}
	if remaining := time.Until(deadline); remaining > 2*time.Minute {
		t.Fatalf("the delivery deadline is %s away, want a bound short against a poll interval", remaining)
	}
}

// TestObserve_ConcurrentPassesOverTheSameConditionFireOnce asserts the
// mutual exclusion this type documents, which nothing exercised before:
// two passes racing over overlapping conditions must still produce
// exactly one alert per condition, not one per pass.
func TestObserve_ConcurrentPassesOverTheSameConditionFireOnce(t *testing.T) {
	sink := &recordingSink{}
	d := alert.NewDispatcher(sink, nil)

	conditions := []alert.Condition{
		staleCondition("production/pg"),
		staleCondition("production/mysql"),
	}

	var wg sync.WaitGroup
	for pass := 0; pass < 8; pass++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Observe(context.Background(), conditions, nil, epoch)
		}()
	}
	wg.Wait()

	if sink.count() != 2 {
		t.Fatalf("sink saw %d alerts across 8 concurrent passes, want exactly 2 (one per condition)", sink.count())
	}
}

// TestNewDispatcher_WithoutASinkIsInertRatherThanPanicking proves
// alerting with no mechanism configured is a silent no-op, matching this
// codebase's existing nil-*obs.Logger convention, instead of a nil
// dereference at the first observed condition.
func TestNewDispatcher_WithoutASinkIsInertRatherThanPanicking(t *testing.T) {
	d := alert.NewDispatcher(nil, nil)
	if d != nil {
		t.Fatalf("NewDispatcher(nil, nil) = %v, want nil: there is no mechanism to deliver through", d)
	}
	if fired := d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, nil, epoch); fired != nil {
		t.Fatalf("(*Dispatcher)(nil).Observe fired %v, want nothing", fired)
	}
}

package alert_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/alert"
)

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

	fired := d.Observe(context.Background(), []alert.Condition{staleCondition("production/postgres-primary")}, epoch)
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
		if fired := d.Observe(context.Background(), []alert.Condition{staleCondition("production/postgres-primary")}, epoch.Add(time.Duration(pass)*time.Hour)); len(fired) != 0 {
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

	d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, epoch)
	d.Observe(context.Background(), nil, epoch.Add(time.Hour))
	d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, epoch.Add(2*time.Hour))

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
	}, epoch)

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
	}, epoch)

	if len(fired) != 1 {
		t.Fatalf("fired %d alerts, want 1", len(fired))
	}
}

// TestObserve_DeliveryFailureDoesNotStormOnEveryPass proves a sink that
// fails (a platform with no notification capability, say) cannot turn one
// unresolved condition into one delivery attempt per poll.
func TestObserve_DeliveryFailureDoesNotStormOnEveryPass(t *testing.T) {
	sink := &recordingSink{err: errors.New("capability not supported by this platform adapter")}
	d := alert.NewDispatcher(sink, nil)

	for pass := 0; pass < 5; pass++ {
		d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, epoch.Add(time.Duration(pass)*time.Hour))
	}

	if sink.count() != 1 {
		t.Fatalf("sink saw %d delivery attempts, want exactly 1 even though delivery failed", sink.count())
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
	if fired := d.Observe(context.Background(), []alert.Condition{staleCondition("production/pg")}, epoch); fired != nil {
		t.Fatalf("(*Dispatcher)(nil).Observe fired %v, want nothing", fired)
	}
}

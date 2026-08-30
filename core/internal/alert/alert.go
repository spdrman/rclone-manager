// Package alert is Work Package 3.5's proactive notification path
// (docs/EPIC-B-multi-nas.md §71): it turns typed status signals other
// packages have already computed into at most one operator-facing
// notification per condition, and deliberately stops there.
//
// # This package detects nothing
//
// Every condition it can report is computed somewhere else, and this
// package is a consumer of that verdict, never a second opinion about it:
//
//   - staleness and repeated failure come from internal/health's own
//     BackupSetHealth (FR-24's four states, decided by that package's
//     decideState);
//   - critical storage pressure comes from internal/capacity's Assessment
//     Level (FR-21), with no threshold arithmetic repeated here;
//   - a changed SSH host key comes from internal/transport's
//     HostVerification category, the classification
//     internal/transport/rclone/ssh.go's refusal already produces (FR-6,
//     FR-22).
//
// conditions.go is the whole of that translation, and it is three small
// functions with no state. If a fifth signal ever needs alerting, the
// place to compute it is the package that owns the fact, not here.
//
// # An alert is a notification and nothing else
//
// §71 is explicit that a critical-storage or repeated-failure alert must
// never turn into an automatic call into B3.1's retention-apply path: an
// alert is a notification, never itself a trigger for deletion. That is
// structural here rather than a convention: this package imports nothing
// that can delete anything (no os, no internal/retention, no
// internal/lifecycle, no internal/state), so there is no reachable path
// from an observed condition to a removed byte. mechanism_test.go asserts
// exactly that import set, so adding one would fail the suite.
//
// The same goes for §77 invariant #5. A changed SSH host key still
// requires explicit administrator intervention: internal/transport/rclone
// refuses the connection, this package notifies somebody about it, and
// neither half ever re-trusts the new key.
//
// # One mechanism, not a framework
//
// §71 says "do not add a broad notification framework in v1", so a
// Dispatcher holds exactly one Sink: not a list of subscribers, not a
// topic registry, and with no way to attach a second consumer after
// construction. The chosen mechanism is the platform's own local
// notification capability, reached at the apps/ layer through
// apps/common/platform/capabilities.Notifier (core/ cannot import apps/,
// §7.1, so it always arrives here as a plain Sink). A platform that has
// not declared that capability gets a refusal at wiring time rather than
// an emulated one that silently drops alerts (§22).
//
// # De-duplication is keyed on the condition, not the poll
//
// The daemon evaluates on every cycle, and the same broken thing is
// observed on every one of them until somebody fixes it. Dispatcher
// therefore remembers which conditions are currently firing, keyed on
// (Kind, Scope): a condition newly observed fires once, the same
// condition observed again on later passes fires nothing, and a condition
// that stops being observed is forgotten, so a genuine recurrence is a
// fresh alert. A delivery failure still counts as fired, deliberately: an
// unreachable notification channel must not turn one unresolved condition
// into one delivery attempt per poll forever. The failure itself is
// logged at error level, so it is visible rather than swallowed.
package alert

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// Kind is one of the four conditions §71's Work Package 3.5 names. There
// are exactly four, and mechanism_test.go pins that: a fifth kind is how
// "one proactive mechanism for four conditions" quietly becomes the
// framework §71 rules out, so adding one is a deliberate edit with a test
// to update, not something that can drift in.
type Kind string

const (
	// StaleBackup is §71's "stale backup": internal/health reports this
	// backup set in its Stale state, meaning no known-good restore point
	// exists inside the configured stale_after window and nothing recent
	// suggests one is still coming.
	StaleBackup Kind = "STALE_BACKUP"

	// RepeatedFailure is §71's "repeated failure": this backup set has
	// accumulated at least the configured number of artifacts sitting in
	// FAILED, or internal/health has placed it in the Failing state,
	// which FR-24 defines as needing a human right now.
	RepeatedFailure Kind = "REPEATED_FAILURE"

	// HostKeyChanged is §71's "changed SSH host key": the transport layer
	// classified this backup set's failure as HostVerification and
	// refused the connection. The alert is added on top of that refusal,
	// never in place of it (§77 invariant #5).
	HostKeyChanged Kind = "HOST_KEY_CHANGED"

	// CriticalStoragePressure is §71's "critical storage pressure":
	// internal/capacity assessed this backup set's destination filesystem
	// at its Critical level. Warning is deliberately not an alert; see
	// internal/capacity's own Thresholds doc for why that level is worth
	// surfacing but is never a refusal.
	CriticalStoragePressure Kind = "CRITICAL_STORAGE_PRESSURE"
)

// Kinds is every Kind this package can produce, in the order §71 lists
// them. It exists so a test (and a reader) can see the whole vocabulary
// in one place.
var Kinds = []Kind{StaleBackup, RepeatedFailure, HostKeyChanged, CriticalStoragePressure}

func (k Kind) String() string { return string(k) }

// title is the short operator-facing headline for this Kind, the first
// half of what a platform notifier renders.
func (k Kind) title() string {
	switch k {
	case StaleBackup:
		return "Backup is stale"
	case RepeatedFailure:
		return "Backup set is failing"
	case HostKeyChanged:
		return "SSH host key changed"
	case CriticalStoragePressure:
		return "Storage is critically low"
	default:
		return "Backup manager alert"
	}
}

// Condition is one currently-true, alertable fact about one backup set.
// It is a plain value with no behaviour: conditions.go builds them from
// signals other packages computed, and Dispatcher decides which of them
// are new.
type Condition struct {
	// Kind is which of §71's four conditions this is.
	Kind Kind

	// Scope is what the condition is about: the backup set's
	// model.BackupSetID rendered as a string, in every case this package
	// produces today. It is half of the de-duplication key, so two backup
	// sets in the same condition are two separate alerts rather than one
	// suppressing the other.
	Scope string

	// Detail is the human-readable explanation delivered as the alert's
	// message. It is deliberately NOT part of the de-duplication key: a
	// condition whose wording changes slightly between passes (a failure
	// count ticking up, say) is still the same unresolved condition and
	// must not re-alert.
	Detail string
}

// Key is the de-duplication identity of this condition. The separator is
// a NUL byte because neither a Kind (a fixed set of upper-snake-case
// constants) nor a BackupSetID (internal/model rejects control characters
// in both halves) can contain one, so no two distinct conditions can ever
// collide on the same key by concatenation.
func (c Condition) Key() string { return string(c.Kind) + "\x00" + c.Scope }

// alert renders this condition as the notification an operator receives.
// now is the caller's own clock reading; this package never reads a clock
// of its own, so a frozen-clock test stays deterministic.
func (c Condition) alert(now time.Time) Alert {
	return Alert{
		Kind:       c.Kind,
		Scope:      c.Scope,
		Title:      c.Kind.title(),
		Message:    c.Detail,
		ObservedAt: now,
	}
}

// Alert is one operator-facing notification, ready for delivery. Title
// and Message map directly onto the two arguments a platform notifier
// takes; Kind and Scope are carried alongside so a sink can log or route
// on the typed values instead of parsing the rendered text back apart.
type Alert struct {
	Kind       Kind
	Scope      string
	Title      string
	Message    string
	ObservedAt time.Time
}

// Sink is the one delivery seam. There is exactly one per Dispatcher, by
// design (see the package doc): a provider app implements this over
// whatever local notification capability its platform actually offers,
// and a platform with none returns a typed refusal rather than pretending
// to deliver.
type Sink interface {
	Deliver(ctx context.Context, a Alert) error
}

// Dispatcher fires an alert the first time it observes a condition, and
// stays quiet about that same condition until it stops being observed.
// It is safe for concurrent use, though in practice one daemon cycle
// drives it at a time.
type Dispatcher struct {
	// sink is the single delivery mechanism. It is set once at
	// construction and never replaced: there is no method to attach a
	// second one.
	sink Sink

	logger *obs.Logger

	mu sync.Mutex
	// firing holds Condition.Key() for every condition currently
	// considered fired. An entry is added when an alert goes out and
	// removed the first pass that no longer observes it.
	firing map[string]struct{}
}

// NewDispatcher builds a Dispatcher delivering through sink. A nil sink
// returns a nil *Dispatcher rather than one that would panic on its first
// delivery: "alerting is not configured" is an ordinary state (it is
// opt-in), and every method here is safe on a nil receiver, matching the
// nil-*obs.Logger convention internal/obs already established.
//
// logger may be nil, with the same meaning it has everywhere else in
// core: a safe no-op.
func NewDispatcher(sink Sink, logger *obs.Logger) *Dispatcher {
	if sink == nil {
		return nil
	}
	return &Dispatcher{sink: sink, logger: logger, firing: map[string]struct{}{}}
}

// Observe is one evaluation pass. conditions is the complete set of
// conditions currently true, as computed by this pass; anything not in it
// is treated as resolved.
//
// It returns the alerts this pass actually fired, which is exactly the
// conditions that were not already firing. Delivery is attempted once per
// returned alert, and a delivery error is logged rather than returned:
// the caller is a daemon cycle that has no useful way to act on "the
// notification channel is down", and retrying on the next pass is exactly
// the alert storm this package's de-duplication exists to prevent.
func (d *Dispatcher) Observe(ctx context.Context, conditions []Condition, now time.Time) []Alert {
	if d == nil {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	active := make(map[string]struct{}, len(conditions))
	var fired []Alert

	for _, c := range conditions {
		if c.Kind == "" {
			continue
		}
		key := c.Key()
		if _, duplicate := active[key]; duplicate {
			continue
		}
		active[key] = struct{}{}

		if _, already := d.firing[key]; already {
			continue
		}
		d.firing[key] = struct{}{}

		a := c.alert(now)
		fired = append(fired, a)
		d.deliver(ctx, a)
	}

	// Forget every condition that is no longer observed, so the next
	// occurrence of it is a fresh alert rather than one suppressed
	// forever by a problem that has since been fixed.
	for key := range d.firing {
		if _, still := active[key]; !still {
			delete(d.firing, key)
		}
	}

	return fired
}

func (d *Dispatcher) deliver(ctx context.Context, a Alert) {
	if err := d.sink.Deliver(ctx, a); err != nil {
		d.logger.Error(ctx, "alert-delivery",
			fmt.Errorf("delivering the %s alert for %s: %w", a.Kind, a.Scope, err))
		return
	}
	d.logger.Alert(ctx, a.Kind.String(), a.Scope, a.Message)
}

package alert_test

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/alert"
	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/health"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// conditions.go turns verdicts other packages already reached into the four
// names the dispatcher de-duplicates on, and this file's job is to stop
// that translation from acquiring an opinion of its own.
//
// Every case hands over a finished verdict, a health.State, a
// capacity.Level, a transport category, and checks which conditions come
// back out. None of them builds a scenario and expects this package to work
// out whether it is bad, because working that out is precisely what
// conditions.go is not allowed to do.
//
// RepeatedFailure gets its two arms tested apart because they answer
// different questions. The count arm weighs an operator's configured number
// against a count health already produced. The Failing arm ignores the
// count entirely, since FR-24's "a human is needed right now" state gated
// behind a threshold would leave the single most severe state as the one
// that never alerts. The non-positive case then pins which way an
// unconfigured threshold fails, which is quiet on the count and still loud
// on Failing.
//
// The timestamp case is the smallest here and the most structural. A
// Condition holds no clock reading at all, so ObservedAt can only have come
// from the caller's own now, and that is what lets everything above run
// against a frozen clock and mean something.

func mustSetID(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

func kindsOf(conds []alert.Condition) []alert.Kind {
	out := make([]alert.Kind, 0, len(conds))
	for _, c := range conds {
		out = append(out, c.Kind)
	}
	return out
}

func hasKind(conds []alert.Condition, want alert.Kind) bool {
	for _, c := range conds {
		if c.Kind == want {
			return true
		}
	}
	return false
}

// repeatedFailureThreshold is this test's explicit threshold, per the
// issue's RED checklist: three artifacts sitting in FAILED is what counts
// as "repeated failure" for these cases.
const repeatedFailureThreshold = 3

// TestBackupSetConditions covers every FR-24 health state this work
// package alerts on, and every one it deliberately stays quiet about.
func TestBackupSetConditions(t *testing.T) {
	set := mustSetID(t, "production", "postgres-primary")

	tests := []struct {
		name  string
		in    health.BackupSetHealth
		want  []alert.Kind
		scope string
	}{
		{
			name:  "stale fires a stale-backup condition",
			in:    health.BackupSetHealth{Set: set, State: health.Stale, Reason: "no known-good backup within the stale threshold"},
			want:  []alert.Kind{alert.StaleBackup},
			scope: set.String(),
		},
		{
			name: "healthy fires nothing",
			in:   health.BackupSetHealth{Set: set, State: health.Healthy},
			want: nil,
		},
		{
			name: "degraded fires nothing",
			in:   health.BackupSetHealth{Set: set, State: health.Degraded},
			want: nil,
		},
		{
			name: "failures below the threshold fire nothing",
			in:   health.BackupSetHealth{Set: set, State: health.Degraded, Failures: repeatedFailureThreshold - 1},
			want: nil,
		},
		{
			name:  "failures at the threshold fire a repeated-failure condition",
			in:    health.BackupSetHealth{Set: set, State: health.Degraded, Failures: repeatedFailureThreshold},
			want:  []alert.Kind{alert.RepeatedFailure},
			scope: set.String(),
		},
		{
			name:  "failures above the threshold fire a repeated-failure condition",
			in:    health.BackupSetHealth{Set: set, State: health.Degraded, Failures: repeatedFailureThreshold + 5},
			want:  []alert.Kind{alert.RepeatedFailure},
			scope: set.String(),
		},
		{
			name:  "FAILING fires a repeated-failure condition regardless of the count",
			in:    health.BackupSetHealth{Set: set, State: health.Failing, Reason: "a FAILED artifact has no retry scheduled", Failures: 1},
			want:  []alert.Kind{alert.RepeatedFailure},
			scope: set.String(),
		},
		{
			name:  "stale and repeated failure are two independent conditions",
			in:    health.BackupSetHealth{Set: set, State: health.Stale, Failures: repeatedFailureThreshold},
			want:  []alert.Kind{alert.StaleBackup, alert.RepeatedFailure},
			scope: set.String(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := alert.BackupSetConditions(tc.in, repeatedFailureThreshold)
			gotKinds := kindsOf(got)
			if len(gotKinds) != len(tc.want) {
				t.Fatalf("BackupSetConditions kinds = %v, want %v", gotKinds, tc.want)
			}
			for i := range tc.want {
				if gotKinds[i] != tc.want[i] {
					t.Fatalf("BackupSetConditions kinds = %v, want %v", gotKinds, tc.want)
				}
			}
			for _, c := range got {
				if c.Scope != tc.scope {
					t.Errorf("Scope = %q, want %q", c.Scope, tc.scope)
				}
				if c.Detail == "" {
					t.Errorf("Condition %+v has no Detail for an operator to read", c)
				}
			}
		})
	}
}

// TestBackupSetConditions_NonPositiveThresholdDisablesRepeatedFailure
// pins the documented fail-safe: a threshold nobody configured must not
// mean "alert on the very first failure".
func TestBackupSetConditions_NonPositiveThresholdDisablesRepeatedFailure(t *testing.T) {
	set := mustSetID(t, "production", "postgres-primary")
	h := health.BackupSetHealth{Set: set, State: health.Degraded, Failures: 42}

	for _, threshold := range []int{0, -1} {
		if got := alert.BackupSetConditions(h, threshold); hasKind(got, alert.RepeatedFailure) {
			t.Errorf("threshold %d produced %v, want no repeated-failure condition", threshold, kindsOf(got))
		}
	}
}

// TestStorageConditions proves only internal/capacity's Critical level
// alerts. Warning is explicitly not an alert (see internal/capacity: it is
// worth surfacing, never a refusal), and this package adds no threshold
// arithmetic of its own.
func TestStorageConditions(t *testing.T) {
	tests := []struct {
		name  string
		level capacity.Level
		want  bool
	}{
		{"OK is quiet", capacity.OK, false},
		{"Warning is quiet", capacity.Warning, false},
		{"Critical alerts", capacity.Critical, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assessment := capacity.Assessment{
				Stat:       capacity.Stat{TotalBytes: 1 << 40, FreeBytes: 1 << 20, AvailableBytes: 1 << 20},
				Thresholds: capacity.Thresholds{WarningFreeBytes: 1 << 30, CriticalFreeBytes: 1 << 29},
				Level:      tc.level,
			}
			got := alert.StorageConditions("production/postgres-primary", assessment)
			if tc.want {
				if !hasKind(got, alert.CriticalStoragePressure) {
					t.Fatalf("StorageConditions(%s) = %v, want a critical-storage-pressure condition", tc.level, kindsOf(got))
				}
				if got[0].Scope != "production/postgres-primary" {
					t.Errorf("Scope = %q, want the backup set the caller named", got[0].Scope)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("StorageConditions(%s) = %v, want no condition", tc.level, kindsOf(got))
			}
		})
	}
}

// TestHostKeyConditions proves the host-key alert is driven by the
// transport layer's own FR-22 classification (internal/transport's
// HostVerification category), the signal
// internal/transport/rclone/ssh.go's refusal already produces, rather
// than by a second host-trust implementation living here.
func TestHostKeyConditions(t *testing.T) {
	all := []transport.Category{
		transport.Unclassified, transport.Transient, transport.Authentication,
		transport.HostVerification, transport.NotFound, transport.PermissionDenied,
		transport.IntegrityFailure, transport.Conflict, transport.UnsupportedCapability,
		transport.Permanent, transport.Cancelled,
	}

	for _, category := range all {
		got := alert.HostKeyConditions("production/postgres-primary", category)
		wantAlert := category == transport.HostVerification
		if gotAlert := hasKind(got, alert.HostKeyChanged); gotAlert != wantAlert {
			t.Errorf("HostKeyConditions(%s) produced an alert = %v, want %v", category, gotAlert, wantAlert)
		}
	}
}

// TestAlertsCarryNoTimestampOfTheirOwn proves Condition never invents a
// clock reading: the dispatcher stamps ObservedAt from the caller's own
// clock, so a frozen-clock test stays deterministic.
func TestAlertsCarryNoTimestampOfTheirOwn(t *testing.T) {
	sink := &recordingSink{}
	d := alert.NewDispatcher(sink, nil)
	at := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)

	d.Observe(context.Background(), alert.StorageConditions("production/pg", capacity.Assessment{Level: capacity.Critical}), nil, at)

	if sink.count() != 1 {
		t.Fatalf("sink received %d alerts, want 1", sink.count())
	}
	if !sink.at(0).ObservedAt.Equal(at) {
		t.Errorf("ObservedAt = %s, want %s", sink.at(0).ObservedAt, at)
	}
}

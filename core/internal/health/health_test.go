package health

import (
	"reflect"
	"testing"
	"time"
)

// These tests are about the types rather than about any calculation, and
// the first one is doing more work than it looks like.
//
// TestProcessAndBackupSetHealthShareNoFields walks both structs by
// reflection and fails on a single shared field name. That reads as an odd
// thing to assert until you take invariant 14 seriously: the two halves of
// FR-24 answer different questions, and the way that separation actually
// breaks is never a bad calculation. It is a field called Status or OK
// turning up on both types and a renderer reading whichever one it happens
// to be holding. Reflection catches that the moment it is written, and code
// review reliably does not.
//
// The rest pins what the four state names render as and that NewReport only
// bundles what it was handed. The names are compared against literals
// rather than against the constants because an operator greps for HEALTHY
// and a dashboard matches on STALE, and neither of those would fail here if
// a rename went through cleanly.

// TestProcessAndBackupSetHealthShareNoFields proves the separation the
// package doc claims is structural, not just a matter of discipline: the
// two types that answer FR-24's two questions cannot even accidentally
// alias a field name, so no future edit can make one type's data readable
// through the other's accessor by mistake.
func TestProcessAndBackupSetHealthShareNoFields(t *testing.T) {
	overlap := sharedFieldNames(reflect.TypeOf(ProcessHealth{}), reflect.TypeOf(BackupSetHealth{}))
	if len(overlap) != 0 {
		t.Fatalf("ProcessHealth and BackupSetHealth share field name(s) %v; process liveness and backup freshness must stay structurally separate (FR-24, invariant 14)", overlap)
	}
}

func sharedFieldNames(a, b reflect.Type) []string {
	names := map[string]bool{}
	for i := 0; i < a.NumField(); i++ {
		names[a.Field(i).Name] = true
	}
	var shared []string
	for i := 0; i < b.NumField(); i++ {
		n := b.Field(i).Name
		if names[n] {
			shared = append(shared, n)
		}
	}
	return shared
}

func TestNewProcessHealthCopiesInputsVerbatim(t *testing.T) {
	got := NewProcessHealth(ProcessInputs{BinaryVersion: "1.2.3", RcloneVersion: "v1.75.0"})
	if got.BinaryVersion != "1.2.3" || got.RcloneVersion != "v1.75.0" {
		t.Fatalf("got %+v", got)
	}
}

func TestStateOK(t *testing.T) {
	cases := []struct {
		state State
		want  bool
	}{
		{Healthy, true},
		{Degraded, false},
		{Stale, false},
		{Failing, false},
	}
	for _, tc := range cases {
		if got := tc.state.OK(); got != tc.want {
			t.Errorf("%s.OK() = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestStateStringIsExactlyTheFR24Name(t *testing.T) {
	cases := []struct {
		state State
		want  string
	}{
		{Healthy, "HEALTHY"},
		{Degraded, "DEGRADED"},
		{Stale, "STALE"},
		{Failing, "FAILING"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestNewReportBundlesWithoutRecomputing(t *testing.T) {
	now := time.Now().UTC()
	process := NewProcessHealth(ProcessInputs{BinaryVersion: "dev", RcloneVersion: "v1.75.0"})
	sets := []BackupSetHealth{
		ComputeBackupSetHealth(testSet, nil, nil, PlacementEvidence{}, day, BackupSetInputs{}, now),
	}

	report := NewReport(process, sets, now)

	if report.Process != process {
		t.Fatalf("Process = %+v, want %+v", report.Process, process)
	}
	if len(report.BackupSets) != 1 || report.BackupSets[0].State != Degraded {
		t.Fatalf("BackupSets = %+v", report.BackupSets)
	}
	if !report.GeneratedAt.Equal(now) {
		t.Fatalf("GeneratedAt = %v, want %v", report.GeneratedAt, now)
	}
}

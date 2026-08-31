package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/health"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Issue #245: a backup set the transport layer refuses to connect to backs
// up nothing on every cycle, and until this landed nothing wrote that down.
// The alert pass computed it transiently from the CycleReport and handed it
// to a notification sink, so an operator was told once, by a desktop
// notification, and every read surface afterwards said the set was merely
// stale.
//
// These tests drive the whole producer through a real RunCycle: a refusal
// recorded, a later connection clearing it, and a failure that says nothing
// about the connection leaving it exactly as it was.

func hostKeyRefusal() error {
	return transport.NewError(transport.HostVerification, "list",
		errors.New("knownhosts: key mismatch for prod-db-01.internal"))
}

func authRefusal() error {
	return transport.NewError(transport.Authentication, "list",
		errors.New("ssh: unable to authenticate"))
}

// haltReasonOf reads one backup set's halt reason back through
// BuildHealthReport, which is the read path GET /system/health and
// `backup-manager status` both go through. Reading it through the report
// rather than out of the journal directly is the point: a fact nothing
// reports is the defect this issue is about.
func haltReasonOf(t *testing.T, svc *Service, set model.BackupSetID) string {
	t.Helper()
	report, err := svc.BuildHealthReport(context.Background(), VersionInfo{})
	if err != nil {
		t.Fatalf("BuildHealthReport: %v", err)
	}
	for _, bs := range report.BackupSets {
		if bs.Set == set {
			return bs.HaltReason
		}
	}
	t.Fatalf("BuildHealthReport has no entry for %s (sets: %+v)", set, report.BackupSets)
	return ""
}

// TestRunCycle_AChangedHostKeyIsRecordedAndThenCleared is the decisive
// case. Setting the fact is only half of it: a "halted" banner still
// standing on a set that has since connected is confidently false, which
// is worse than saying nothing.
func TestRunCycle_AChangedHostKeyIsRecordedAndThenCleared(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	tr := newFakeTransport()
	tr.put("backup.dump", "halt payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	ctx := context.Background()

	// Before anything has failed, nothing is claimed. This is the control
	// for the clear below: an empty reason at the end has to mean "cleared"
	// rather than "this read never returned anything".
	svc.RunCycle(ctx)
	if got := haltReasonOf(t, svc, bs.ID); got != "" {
		t.Fatalf("halt reason after a clean cycle = %q, want empty", got)
	}

	// The key changes. FR-6 refuses the connection, nothing is backed up.
	tr.failForSourceID = bs.ID.String()
	tr.failErr = hostKeyRefusal()
	svc.RunCycle(ctx)

	if got := haltReasonOf(t, svc, bs.ID); got != state.HaltHostKeyChanged {
		t.Fatalf("halt reason after the refusal = %q, want %q", got, state.HaltHostKeyChanged)
	}

	// The administrator verifies the new key out of band and updates
	// known_hosts. The next cycle connects, which is the only evidence
	// §77 invariant 5 accepts, and the reason must go with it.
	tr.failForSourceID = ""
	tr.failErr = nil
	svc.RunCycle(ctx)

	if got := haltReasonOf(t, svc, bs.ID); got != "" {
		t.Fatalf("halt reason after a cycle that connected = %q, want it cleared", got)
	}
}

// TestRunCycle_AFailedAuthenticationIsARefusalToo pins the scope choice.
// The fact this records is "the manager could not connect to this set, and
// here is why", not "the host key changed": an SSH login the server
// rejects stops the backups exactly as completely, and reporting it as
// merely stale is the same hole one category along.
func TestRunCycle_AFailedAuthenticationIsARefusalToo(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	tr := newFakeTransport()
	tr.failForSourceID = bs.ID.String()
	tr.failErr = authRefusal()

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RunCycle(context.Background())

	if got := haltReasonOf(t, svc, bs.ID); got != state.HaltAuthenticationFailed {
		t.Fatalf("halt reason after an authentication refusal = %q, want %q", got, state.HaltAuthenticationFailed)
	}
}

// TestRunCycle_AFailureThatSaysNothingAboutTheConnectionNeitherSetsNorClears
// is the same rule internal/app/alerts.go already applies to the host-key
// alert: absence of the refusal is not evidence the key was re-trusted. A
// set that failed for an unrelated reason never got far enough to say
// either way, so the last real observation stands.
func TestRunCycle_AFailureThatSaysNothingAboutTheConnectionNeitherSetsNorClears(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	tr := newFakeTransport()
	tr.failForSourceID = bs.ID.String()
	tr.failErr = hostKeyRefusal()

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	ctx := context.Background()

	svc.RunCycle(ctx)
	if got := haltReasonOf(t, svc, bs.ID); got != state.HaltHostKeyChanged {
		t.Fatalf("halt reason after the refusal = %q, want %q", got, state.HaltHostKeyChanged)
	}

	// A permission failure on the remote directory is not a connection
	// refusal: the manager reached the host and was turned away by the
	// filesystem, so it says nothing about the key.
	tr.failErr = transport.NewError(transport.PermissionDenied, "list", errors.New("permission denied"))
	svc.RunCycle(ctx)

	if got := haltReasonOf(t, svc, bs.ID); got != state.HaltHostKeyChanged {
		t.Fatalf("halt reason after an unrelated failure = %q, want the earlier %q to stand", got, state.HaltHostKeyChanged)
	}

	// The other direction of the same rule: an unrelated failure on a set
	// with no halt does not invent one.
	clean := testBackupSet(t, t.TempDir())
	clean.Name = "auth-config"
	clean.ID = mustSetID(t, "production", "auth-config")
	cleanTr := newFakeTransport()
	cleanTr.failForSourceID = clean.ID.String()
	cleanTr.failErr = transport.NewError(transport.NotFound, "list", errors.New("no such directory"))

	cleanSvc := New(testConfig(t, testSource("production", clean)), openJournal(t), cleanTr, nil)
	cleanSvc.Now = fixedNow(epoch)
	cleanSvc.RunCycle(ctx)

	if got := haltReasonOf(t, cleanSvc, clean.ID); got != "" {
		t.Fatalf("halt reason after a NotFound failure = %q, want empty: that is not a connection refusal", got)
	}
}

// TestRunCycle_ARefusalIsScopedToTheSetItRefused proves one set's changed
// host key never speaks for another set's.
func TestRunCycle_ARefusalIsScopedToTheSetItRefused(t *testing.T) {
	refused := testBackupSet(t, t.TempDir())
	fine := testBackupSet(t, t.TempDir())
	fine.Name = "auth-config"
	fine.ID = mustSetID(t, "production", "auth-config")

	tr := newFakeTransport()
	tr.put("backup.dump", "scoped payload", epoch.Unix())
	tr.failForSourceID = refused.ID.String()
	tr.failErr = hostKeyRefusal()

	svc := New(testConfig(t, testSource("production", refused, fine)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RunCycle(context.Background())

	if got := haltReasonOf(t, svc, refused.ID); got != state.HaltHostKeyChanged {
		t.Fatalf("halt reason for the refused set = %q, want %q", got, state.HaltHostKeyChanged)
	}
	if got := haltReasonOf(t, svc, fine.ID); got != "" {
		t.Fatalf("halt reason for the set that connected = %q, want empty", got)
	}
}

// TestHaltReasonNeverDecidesTheHealthState keeps internal/health's own
// separation intact. A refusal is reported beside the verdict, never as
// part of it: decideState's evidence has no field a connection refusal
// could arrive through, and this proves the value travels past it rather
// than into it.
func TestHaltReasonNeverDecidesTheHealthState(t *testing.T) {
	set := mustSetID(t, "production", "postgres-primary")
	base := health.BackupSetInputs{}
	halted := health.BackupSetInputs{HaltReason: state.HaltHostKeyChanged}

	without := health.ComputeBackupSetHealth(set, nil, nil, 24*time.Hour, base, epoch)
	with := health.ComputeBackupSetHealth(set, nil, nil, 24*time.Hour, halted, epoch)

	if with.State != without.State || with.Reason != without.Reason {
		t.Fatalf("a halt reason changed the verdict: (%s, %q) with, (%s, %q) without",
			with.State, with.Reason, without.State, without.Reason)
	}
	// Positive control: the value really did travel, so the equality above
	// is the halt being kept out of the decision rather than dropped.
	if with.HaltReason != state.HaltHostKeyChanged {
		t.Fatalf("BackupSetHealth.HaltReason = %q, want %q", with.HaltReason, state.HaltHostKeyChanged)
	}
	if without.HaltReason != "" {
		t.Fatalf("BackupSetHealth.HaltReason = %q with no halt supplied, want empty", without.HaltReason)
	}
}

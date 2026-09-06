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

// The three refusals FR-22 says are evidence about the connection itself,
// each built as the classified transport.Error the adapter would really
// produce.
//
// They are three constructors rather than one with a category parameter
// because the category is the thing under test: haltReasonFor maps each to its
// own durable reason, and a single parameterised builder invites a test that
// passes the same category twice and proves half of what it says it does.
func hostKeyRefusal() error {
	return transport.NewError(transport.HostVerification, "list",
		errors.New("knownhosts: key mismatch for prod-db-01.internal"))
}

func authRefusal() error {
	return transport.NewError(transport.Authentication, "list",
		errors.New("ssh: unable to authenticate"))
}

func keyPermissionsRefusal() error {
	return transport.NewError(transport.KeyPermissions, "ssh_key_permissions",
		errors.New(`key_file has permissions 0777, expected exactly 0600`))
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

// TestRunCycle_ADriftedKeyPermissionIsARefusalDistinctFromAuthentication is
// issue #293's producer-level case. internal/transport/rclone/ssh.go now
// refuses to build a connection at all when a configured key_file's mode
// has drifted, classified as transport.KeyPermissions, and this proves
// that refusal reaches the same durable halt-reason mechanism #245 built,
// under its own name rather than folded into HaltAuthenticationFailed:
// the two call for different fixes, and an operator reading the halt
// reason needs to be able to tell which one this is.
func TestRunCycle_ADriftedKeyPermissionIsARefusalDistinctFromAuthentication(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	tr := newFakeTransport()
	tr.failForSourceID = bs.ID.String()
	tr.failErr = keyPermissionsRefusal()

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RunCycle(context.Background())

	if got := haltReasonOf(t, svc, bs.ID); got != state.HaltKeyPermissions {
		t.Fatalf("halt reason after a key-permission refusal = %q, want %q", got, state.HaltKeyPermissions)
	}
	if got := haltReasonOf(t, svc, bs.ID); got == state.HaltAuthenticationFailed {
		t.Fatalf("halt reason after a key-permission refusal read as %q, the credential reason: the two must stay distinct", got)
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

	without := health.ComputeBackupSetHealth(set, nil, nil, health.PlacementEvidence{}, 24*time.Hour, base, epoch)
	with := health.ComputeBackupSetHealth(set, nil, nil, health.PlacementEvidence{}, 24*time.Hour, halted, epoch)

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

// TestHaltReasonFor_OnlyTheThreeRefusalCategoriesEverProduceAReason is the
// blast-radius check for issue #388. That change moves a connect timeout
// rclone imposed on itself out of transport.Cancelled and into
// transport.Transient, and this is the place to be sure that move cannot
// change what an operator is told about the connection: neither category
// says anything about a host refusing us, and neither should.
//
// Reading haltReasonFor's switch says so; running it says so too, for every
// category the classifier can produce rather than the two this issue moved
// between.
func TestHaltReasonFor_OnlyTheThreeRefusalCategoriesEverProduceAReason(t *testing.T) {
	all := []transport.Category{
		transport.Unclassified, transport.Transient, transport.Authentication,
		transport.HostVerification, transport.KeyPermissions, transport.NotFound,
		transport.PermissionDenied, transport.IntegrityFailure, transport.Conflict,
		transport.UnsupportedCapability, transport.Permanent, transport.Cancelled,
	}
	refusals := map[transport.Category]string{
		transport.HostVerification: state.HaltHostKeyChanged,
		transport.Authentication:   state.HaltAuthenticationFailed,
		transport.KeyPermissions:   state.HaltKeyPermissions,
	}

	for _, category := range all {
		err := transport.NewError(category, "list", errors.New("something happened"))
		reason, ok := haltReasonFor(err)
		want, wantOK := refusals[category]
		if ok != wantOK || reason != want {
			t.Errorf("haltReasonFor(%s) = (%q, %v), want (%q, %v)", category, reason, ok, want, wantOK)
		}
	}

	// And the two categories #388 moves between, said out loud, since the
	// whole point is that an operator sees no difference here.
	timeout := transport.NewError(transport.Transient, "list", context.DeadlineExceeded)
	cancelled := transport.NewError(transport.Cancelled, "list", context.Canceled)
	if _, ok := haltReasonFor(timeout); ok {
		t.Error("a transient failure was recorded as a connection refusal")
	}
	if _, ok := haltReasonFor(cancelled); ok {
		t.Error("a cancellation was recorded as a connection refusal")
	}
}

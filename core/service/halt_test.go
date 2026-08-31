package service

import (
	"context"
	"errors"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Issue #245 end to end at this package's own boundary: a backup set the
// transport refuses to connect to has to reach a caller of Health with a
// reason attached, and a set that later connects has to stop carrying one.
//
// The run half goes through SubmitRunCycle, the same durable operation the
// HTTP layer submits, rather than reaching into internal/app directly, and
// the read half goes through Health, which is what GET /system/health
// serves. Neither half touches the journal by hand: a fact the read path
// cannot carry is the whole defect this issue is about.

// refusingTransport answers every call with one classified transport
// error, which is what a changed host key or a rejected login looks like
// from above: the connection never opens, so nothing is listed, copied or
// deleted. err is swapped between cycles to move the set between refused
// and reachable.
type refusingTransport struct {
	err error
}

func (f *refusingTransport) List(_ context.Context, _ transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, f.err
}

func (f *refusingTransport) Stat(_ context.Context, _ transport.Source, _ string) (transport.RemoteArtifact, error) {
	if f.err != nil {
		return transport.RemoteArtifact{}, f.err
	}
	return transport.RemoteArtifact{}, transport.NewError(transport.NotFound, "stat", errors.New("not found"))
}

func (f *refusingTransport) CopyToLocal(_ context.Context, _ transport.Source, _, _ string) (transport.TransferResult, error) {
	return transport.TransferResult{}, f.err
}

func (f *refusingTransport) RemoteHash(_ context.Context, _ transport.Source, _ string, _ transport.HashAlgorithm) (string, error) {
	return "", f.err
}

func (f *refusingTransport) DeleteRemote(_ context.Context, _ transport.Source, _ string) error {
	return f.err
}

var _ transport.Transport = (*refusingTransport)(nil)

func haltTestBackupSet(t *testing.T, localDir string) (config.Source, config.BackupSet) {
	t.Helper()
	id, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	bs := config.BackupSet{
		Name:       "postgres-primary",
		ID:         id,
		Include:    []string{"*.dump"},
		Completion: config.Completion{Strategy: "rename"},
		LocalPath:  localDir,
		RemotePath: "/backups",
	}
	return config.Source{Name: "production", BackupSets: []config.BackupSet{bs}}, bs
}

func haltReasonFromHealth(t *testing.T, svc *BackupService, id string) string {
	t.Helper()
	report, err := svc.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	for _, bs := range report.BackupSets {
		if bs.BackupSetID == id {
			return bs.HaltReason
		}
	}
	t.Fatalf("Health has no entry for %q (report: %+v)", id, report.BackupSets)
	return ""
}

func runCycleAndWait(t *testing.T, svc *BackupService, key string) {
	t.Helper()
	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: key,
		Actor:          "operator",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle(%q): %v", key, err)
	}
	waitForTerminalStatus(t, svc, op.ID)
}

// TestHealth_ReportsAChangedHostKeyAndThenStopsReportingIt is the decisive
// test for issue #245. Both halves matter and the second is the one that
// is easy to leave out: a set that has since connected must stop carrying
// a halt reason, because a stale "this set is halted" banner is worse than
// no banner at all.
func TestHealth_ReportsAChangedHostKeyAndThenStopsReportingIt(t *testing.T) {
	source, bs := haltTestBackupSet(t, t.TempDir())
	tr := &refusingTransport{}
	svc := New(testConfig(source), openTestJournal(t), tr, nil)

	// Control: with nothing refused, the read path reports no reason, so
	// the empty answer at the end cannot be an artefact of the read.
	runCycleAndWait(t, svc, "cycle-clean-1")
	if got := haltReasonFromHealth(t, svc, bs.ID.String()); got != "" {
		t.Fatalf("halt_reason after a cycle that connected = %q, want empty", got)
	}

	tr.err = transport.NewError(transport.HostVerification, "list",
		errors.New("knownhosts: key mismatch for prod-db-01.internal"))
	runCycleAndWait(t, svc, "cycle-refused")

	if got := haltReasonFromHealth(t, svc, bs.ID.String()); got != "HOST_KEY_CHANGED" {
		t.Fatalf("halt_reason after the refusal = %q, want %q", got, "HOST_KEY_CHANGED")
	}

	tr.err = nil
	runCycleAndWait(t, svc, "cycle-clean-2")

	if got := haltReasonFromHealth(t, svc, bs.ID.String()); got != "" {
		t.Fatalf("halt_reason after the set connected again = %q, want it cleared", got)
	}
}

// TestHealth_ReportsARejectedLogin pins the scope: this reports that the
// manager could not connect and why, not only that a host key changed. A
// login the server rejects stops the backups just as completely.
func TestHealth_ReportsARejectedLogin(t *testing.T) {
	source, bs := haltTestBackupSet(t, t.TempDir())
	tr := &refusingTransport{err: transport.NewError(transport.Authentication, "list",
		errors.New("ssh: unable to authenticate"))}
	svc := New(testConfig(source), openTestJournal(t), tr, nil)

	runCycleAndWait(t, svc, "cycle-auth")

	if got := haltReasonFromHealth(t, svc, bs.ID.String()); got != "AUTHENTICATION_FAILED" {
		t.Fatalf("halt_reason after a rejected login = %q, want %q", got, "AUTHENTICATION_FAILED")
	}
}

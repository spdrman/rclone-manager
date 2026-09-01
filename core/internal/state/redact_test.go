package state

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// realConnectionRefusedError reserves a TCP port, releases it immediately,
// and dials it, so err is a real *net.OpError carrying "connect:
// connection refused" for host:port — not a fabricated string. This
// mirrors internal/transport/rclone's own real-closed-port tests and
// internal/app/redaction_test.go's end-to-end proof, scaled down to what
// this package can produce with only the standard library: this package
// must not grow a dependency on internal/transport/rclone just to prove
// its own redaction seam works.
func realConnectionRefusedError(t *testing.T) (host string, port int, err error) {
	t.Helper()
	l, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatalf("allocating a free port: %v", lerr)
	}
	addr := l.Addr().(*net.TCPAddr)
	host, port = addr.IP.String(), addr.Port
	if cerr := l.Close(); cerr != nil {
		t.Fatalf("closing the listener: %v", cerr)
	}
	_, err = net.Dial("tcp", addr.String())
	if err == nil {
		t.Fatalf("net.Dial succeeded against a port nothing is listening on")
	}
	return host, port, err
}

// TestSetRedactor_FiltersStateTransitionsDetail is issue #295's proof for
// the journal's own seam: RecordTransition (journal.go) runs t.Detail
// through whatever Redactor SetRedactor last installed, before the
// state_transitions row is written, so a real connection-refused error's
// host:port never reaches that durable, append-only log once a Journal
// has been told which endpoint is sensitive.
func TestSetRedactor_FiltersStateTransitionsDetail(t *testing.T) {
	j, _ := openJournal(t)
	host, port, dialErr := realConnectionRefusedError(t)
	j.SetRedactor(obs.NewRedactor(obs.Endpoint{Host: host, Port: port, User: "backupuser"}))

	ctx := context.Background()
	artifact := testArtifact(t)
	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	detail := fmt.Sprintf("source %q: NewFs: couldn't connect SSH: %v", artifact.String(), dialErr)
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact:   artifact,
		Key:        "transfer-attempt-1",
		From:       "DISCOVERED",
		To:         "FAILED",
		OccurredAt: time.Now(),
		Detail:     detail,
	}); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}

	activity, err := j.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(activity) == 0 {
		t.Fatalf("RecentActivity returned no rows")
	}
	got := activity[0].Detail

	portStr := strconv.Itoa(port)
	if strings.Contains(got, portStr) {
		t.Errorf("state_transitions.detail still contains the port %q: %q", portStr, got)
	}
	if strings.Contains(got, host) {
		t.Errorf("state_transitions.detail still contains the host %q: %q", host, got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("state_transitions.detail was never redacted at all: %q", got)
	}
	if !strings.Contains(got, artifact.String()) {
		t.Errorf("state_transitions.detail lost the artifact id, want it preserved: %q", got)
	}
}

// TestSetRedactor_FiltersDeletionError proves the second column issue #295
// found: artifacts.remote_delete_error (Transition.Deletion.Error), a
// separate leak route from state_transitions.detail that
// internal/lifecycle/remotedelete.go's persistDeleteOutcome writes a raw
// transport failure into when the remote delete call itself fails. Left
// unfiltered, redacting state_transitions.detail alone would still leave
// the endpoint recoverable from this column.
func TestSetRedactor_FiltersDeletionError(t *testing.T) {
	j, _ := openJournal(t)
	host, port, dialErr := realConnectionRefusedError(t)
	j.SetRedactor(obs.NewRedactor(obs.Endpoint{Host: host, Port: port}))

	ctx := context.Background()
	artifact := testArtifact(t)
	for _, tr := range []Transition{
		{Artifact: artifact, Key: "k1", From: "", To: "DISCOVERED", OccurredAt: time.Now(), RemotePath: "/incoming/backup.dump"},
		{Artifact: artifact, Key: "k2", From: "DISCOVERED", To: "REMOTE_DELETE_PENDING", OccurredAt: time.Now()},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s: %v", tr.To, err)
		}
	}

	deleteErr := fmt.Sprintf("lifecycle: DeleteRemote: transport delete failed: %v", dialErr)
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact:   artifact,
		Key:        "k3",
		From:       "REMOTE_DELETE_PENDING",
		To:         "REMOTE_DELETE_PENDING",
		OccurredAt: time.Now(),
		Detail:     "FR-15: the remote delete call itself failed",
		Deletion:   &DeletionUpdate{Error: deleteErr},
	}); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	portStr := strconv.Itoa(port)
	if strings.Contains(rec.RemoteDeleteError, portStr) {
		t.Errorf("artifacts.remote_delete_error still contains the port %q: %q", portStr, rec.RemoteDeleteError)
	}
	if strings.Contains(rec.RemoteDeleteError, host) {
		t.Errorf("artifacts.remote_delete_error still contains the host %q: %q", host, rec.RemoteDeleteError)
	}
	if !strings.Contains(rec.RemoteDeleteError, "[REDACTED]") {
		t.Errorf("artifacts.remote_delete_error was never redacted at all: %q", rec.RemoteDeleteError)
	}
}

// TestNoRedactor_DetailIsUnchanged is the regression control: a Journal
// nobody ever called SetRedactor on (today's default for every existing
// deployment) must persist Detail exactly as given, byte for byte.
func TestNoRedactor_DetailIsUnchanged(t *testing.T) {
	j, _ := openJournal(t)
	_, _, dialErr := realConnectionRefusedError(t)

	ctx := context.Background()
	artifact := testArtifact(t)
	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	detail := fmt.Sprintf("source %q: NewFs: couldn't connect SSH: %v", artifact.String(), dialErr)
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact:   artifact,
		Key:        "transfer-attempt-1",
		From:       "DISCOVERED",
		To:         "FAILED",
		OccurredAt: time.Now(),
		Detail:     detail,
	}); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}

	activity, err := j.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(activity) == 0 {
		t.Fatalf("RecentActivity returned no rows")
	}
	if activity[0].Detail != detail {
		t.Fatalf("with no redactor installed, detail = %q, want it unchanged: %q", activity[0].Detail, detail)
	}
}

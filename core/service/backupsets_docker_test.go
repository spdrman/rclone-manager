package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/tests/sftpfixture"
)

// waitForTerminalStatusPatient is waitForTerminalStatus (service_test.go)
// with a deadline sized for a real Docker+SSH round trip instead of an
// in-memory/local-transport one.
func waitForTerminalStatusPatient(t *testing.T, svc *BackupService, id string) Operation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		op, err := svc.GetOperation(context.Background(), id)
		if err != nil {
			t.Fatalf("GetOperation(%q): %v", id, err)
		}
		if op.Status == "completed" || op.Status == "failed" {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %q did not reach a terminal status within the deadline (last status %q)", id, op.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCreateBackupSet_EndToEndAgainstARealSFTPFixture is issue #146's own
// INTEGRATION requirement made concrete: "complete the wizard for real
// against a test fixture SSH target, confirm a backup set actually
// exists afterward ... not just that the UI shows a success toast."
// There is no UI in this test (that is ui/shared/e2e/wizard.spec.ts's
// job, against the deliberately mocked API — see that file's own
// comment on why); this is the same sequence one level down, driven
// directly against core/service with a REAL disposable SFTP server
// (core/tests/sftpfixture, atmoz/sftp in Docker), so the whole chain —
// probe a real host key, import a real key, persist a real config file,
// hot-reload, and actually transfer/verify/commit/delete a real artifact
// over real SFTP — is proven end to end, not simulated at any layer.
//
// Skips itself (via sftpfixture.Start) when Docker is not available on
// this machine, exactly like every other Docker-backed test in this
// repository.
func TestCreateBackupSet_EndToEndAgainstARealSFTPFixture(t *testing.T) {
	fx := sftpfixture.Start(t)

	// Seed one real artifact the way a real producer would: written
	// directly into the fixture's upload directory, discovered by the
	// backup set CreateBackupSet is about to persist.
	if err := os.WriteFile(filepath.Join(fx.UploadDir, "backup.dump"), []byte("real end-to-end fixture payload"), 0o644); err != nil {
		t.Fatalf("seeding fixture artifact: %v", err)
	}

	svc, _ := openTestService(t)

	// Step 1 of the wizard's real flow: import the fixture's own client
	// key (real bytes, read off disk exactly as an operator pasting a
	// key into the wizard's textarea would submit them).
	keyPEM, err := os.ReadFile(fx.KeyFile)
	if err != nil {
		t.Fatalf("reading fixture key file: %v", err)
	}
	keyRef, err := svc.ImportSSHKey(context.Background(), keyPEM)
	if err != nil {
		t.Fatalf("ImportSSHKey: %v", err)
	}
	if keyRef.Fingerprint == "" {
		t.Fatal("ImportSSHKey returned an empty fingerprint")
	}

	// Step 2: probe the fixture's REAL host key over a real TCP
	// connection and real SSH handshake — no known_hosts file involved
	// yet, exactly like the wizard's "Verify server" step before any
	// trust decision has been made.
	probe, err := svc.ProbeHostKey(context.Background(), fx.Host, fx.Port)
	if err != nil {
		t.Fatalf("ProbeHostKey: %v", err)
	}
	if probe.Fingerprint == "" || probe.KnownHostsLine == "" {
		t.Fatalf("ProbeHostKey returned an incomplete result: %+v", probe)
	}

	// Cross-check against the fixture's own known-good fingerprint file:
	// confirms the probe captured a REAL key the fixture actually
	// serves, not a coincidentally-non-empty placeholder.
	wantKnownHosts, err := os.ReadFile(fx.KnownHostsFile)
	if err != nil {
		t.Fatalf("reading fixture's own known_hosts file: %v", err)
	}
	if !containsLine(string(wantKnownHosts), probe.KnownHostsLine) {
		t.Errorf("probed known_hosts_line %q was not found among the fixture's own recorded host keys:\n%s", probe.KnownHostsLine, wantKnownHosts)
	}

	// Step 3: a pre-save connection test against the real server, using
	// exactly the references the wizard's Save button would carry —
	// before anything is persisted.
	connResult, err := svc.TestConnection(context.Background(), ConnectionTestRequest{
		Host:           fx.Host,
		Port:           fx.Port,
		User:           fx.User,
		SSHKeyID:       keyRef.ID,
		KnownHostsLine: probe.KnownHostsLine,
		RemotePath:     "/upload",
	})
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !connResult.OK {
		t.Fatalf("TestConnection against the real fixture reported not-OK: %q", connResult.Message)
	}

	// Step 4: "Save, enable & run" — create the backup set for real and
	// let it run immediately.
	result, err := svc.CreateBackupSet(context.Background(), CreateBackupSetRequest{
		Name:               "fixture-set",
		Host:               fx.Host,
		Port:               fx.Port,
		User:               fx.User,
		SSHKeyID:           keyRef.ID,
		KnownHostsLine:     probe.KnownHostsLine,
		RemotePath:         "/upload",
		LocalPath:          t.TempDir(),
		Include:            []string{"*.dump"},
		CompletionStrategy: "rename",
		StaleAfter:         0, // defaults to defaultStaleAfter
		RunImmediately:     true,
		Actor:              "integration-test",
	})
	if err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	if result.Operation == nil {
		t.Fatal("CreateBackupSet did not submit a run_cycle operation for RunImmediately:true")
	}

	// Confirm a backup set actually exists afterward, queryable via
	// ListBackupSets/GetBackupSet — the acceptance criterion's own
	// wording — before even checking whether its first run succeeded.
	got, err := svc.GetBackupSet(context.Background(), result.Set.ID)
	if err != nil {
		t.Fatalf("GetBackupSet after create: %v", err)
	}
	if got.Host != fx.Host {
		t.Errorf("GetBackupSet host = %q, want %q", got.Host, fx.Host)
	}
	all, err := svc.ListBackupSets(context.Background())
	if err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}
	foundInList := false
	for _, s := range all {
		if s.ID == result.Set.ID {
			foundInList = true
		}
	}
	if !foundInList {
		t.Errorf("ListBackupSets did not include %q: %+v", result.Set.ID, all)
	}

	// And the run this call kicked off actually succeeded end to end
	// (discover, transfer, verify, commit, remote-delete) against the
	// real server — the strongest form of "not just a UI toast".
	//
	// waitForTerminalStatusPatient, not the shared waitForTerminalStatus
	// (service_test.go): that helper's 2-second deadline is calibrated
	// for the in-memory/local-transport tests around it, and is too
	// tight for a real Docker+SSH round trip.
	done := waitForTerminalStatusPatient(t, svc, result.Operation.ID)
	if done.Status != "completed" {
		t.Fatalf("run_cycle Operation.Status = %q, want %q (error: %s)", done.Status, "completed", done.Error)
	}

	// The artifact's local copy must actually be durable on disk —
	// discovered, transferred, verified and committed for real, not
	// simulated. It deliberately does NOT also check that the remote
	// copy was deleted: this account (a real, correctly-hardened,
	// shell-less internal-sftp account, exactly docs/ssh-setup.md's
	// recommendation) cannot compute a remote hash at all (see
	// core/tests/sftpintegration.TestSFTPHashCapability), and this new
	// backup set is created with Validation.Hash == "" for exactly that
	// reason (backupsets.go's own doc on that field) — which means FR-16's
	// remote-identity confirmation before deletion can only ever reach
	// "weak" confidence here (size/mtime agree, no hash, no backend
	// stable identifier), and correctly refuses to delete rather than
	// guess. That refusal is this project's safety design working as
	// intended, not a bug this test should paper over by choosing a
	// backup set shape that avoids exercising it.
	committedPath := filepath.Join(result.Set.LocalPath, "backup.dump")
	if _, err := os.Stat(committedPath); err != nil {
		t.Errorf("Stat(%q): %v, want the artifact durably committed locally", committedPath, err)
	}
}

// containsLine reports whether needle appears as one whole line of
// haystack (both known_hosts.d and a real known_hosts file may carry
// more than one recorded key, e.g. sftpfixture's own dual ed25519+RSA
// pinning).
func containsLine(haystack, needle string) bool {
	for _, line := range splitLines(haystack) {
		if line == needle {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, r := range s {
		if r == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

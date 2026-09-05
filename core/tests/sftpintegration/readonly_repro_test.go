package sftpintegration_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// readOnlyReproRetention mirrors app package's own testRetention helper:
// the defaults config.Validate would fill in for a config built by hand
// rather than through config.LoadAndValidate, since this file constructs a
// config.Config directly (like internal/app/cycle_test.go does) rather than
// parsing YAML.
func readOnlyReproRetention() config.Retention {
	protect := true
	return config.Retention{
		Timezone:             "UTC",
		WeekStartsOn:         "monday",
		DailyDays:            7,
		WeeklyMonths:         3,
		MonthlyMonths:        12,
		ProtectLastKnownGood: &protect,
	}
}

// TestReadOnlyBackupSet_RealSFTPFixture is issue #282's acceptance
// criterion, run for real: "the reproduction above, run against the local
// SFTP fixture with the flag set, leaves the remote directory intact."
//
// The scenario mirrors the issue's own repro as closely as this suite's
// existing fixtures allow: a real sshd on a source machine (machines.Start,
// which is the one way a test here gets a machine, #447),
// an artifact that transfers, verifies and commits, and then a second call
// to RunCycle standing in for the issue's "cycle 2" -- the point at which,
// on an unprotected backup set, the remote source would ordinarily be
// removed. Two backup sets share one fixture and one journal, isolated by
// backup-set id, so the control case (read-only unset) and the fix
// (read-only set) run against literally the same container in the same
// test.
//
// # Why the control case is a refusal, not a completed delete
//
// The source machine runs the project's own recommended hardened posture
// (atmoz/sftp, forced internal-sftp, no shell), which is exactly the
// deployment remotedelete.go's own package doc says routinely refuses a
// delete on weak remote-identity confidence -- the "usually refuses"
// behaviour the issue distinguishes from "never tries". The control case
// below proves that distinction directly: the delete IS attempted (a
// refusal is durably recorded in remote_delete_error, and the journal
// lands at REMOTE_DELETE_PENDING, still offered to DeleteRemote again next
// cycle) even though it does not succeed here. The read-only case is
// different in kind, not degree: no refusal is ever recorded, because
// DeleteRemote is never called at all, and the artifact reaches the
// permanent REMOTE_RETAINED terminal state instead of sitting at
// REMOTE_DELETE_PENDING forever being re-offered and re-refused.
func TestReadOnlyBackupSet_RealSFTPFixture(t *testing.T) {
	// The worked example for #447: one call names the tier, and everything
	// this test read off the fixture before (UploadDir, Host, Port, User,
	// KeyFile, KnownHostsFile, Context) is still there on the source
	// machine, unchanged.
	f := machines.Start(t).Source(t)
	adapter := rclone.New()
	ctx := f.Context()

	seed := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.UploadDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	newBackupSet := func(t *testing.T, sourceName, setName string, readOnly bool) (config.BackupSet, model.BackupSetID) {
		t.Helper()
		id, err := model.NewBackupSetID(sourceName, setName)
		if err != nil {
			t.Fatalf("NewBackupSetID: %v", err)
		}
		// RunCycle (via internal/app's sourceFor) builds its own
		// transport.Source out of bs.Remote/bs.RemotePath, unlike
		// discovery.Discover elsewhere in this file, which is handed an
		// already-built transport.Source directly. "upload" matches what
		// Source.TransportSource's own root-joining does (root == "" here, since
		// every backup set in this test shares the fixture's one upload
		// directory).
		return config.BackupSet{
			Name:       setName,
			ID:         id,
			Completion: config.Completion{Strategy: "rename"},
			LocalPath:  t.TempDir(),
			ReadOnly:   readOnly,
			RemotePath: "upload",
			Remote: config.Remote{
				Type: "sftp",
				Host: f.Host,
				Port: f.Port,
				User: f.User,
				// sourceFor (internal/app/app.go) reads Key.File, not the
				// deprecated KeyFile alias: this config is built by hand
				// rather than through config.Validate, which is the only
				// place that normally normalizes the alias into Key.File.
				Key:        config.Key{File: f.KeyFile},
				KnownHosts: f.KnownHostsFile,
			},
		}, id
	}

	runOneCycle := func(t *testing.T, bs config.BackupSet, journal app.Journal) app.CycleReport {
		t.Helper()
		cfg := &config.Config{
			Sources:   []config.Source{{Name: bs.ID.Source, BackupSets: []config.BackupSet{bs}}},
			Retention: readOnlyReproRetention(),
		}
		svc := app.New(cfg, journal, adapter, nil)
		return svc.RunCycle(ctx)
	}

	t.Run("without read_only, today's behaviour: a delete is attempted and refused, never withheld", func(t *testing.T) {
		seed("control.dump", "control payload, protected today only by weak identity confidence")
		bs, id := newBackupSet(t, "control-source", "control-set", false)
		journal := openJournal(t)

		report := runOneCycle(t, bs, journal)
		if len(report.Sets) != 1 || report.Sets[0].Err != nil {
			t.Fatalf("cycle 1: report = %+v", report)
		}

		if _, err := os.Stat(filepath.Join(f.UploadDir, "control.dump")); err != nil {
			t.Fatalf("control.dump should still exist (the delete is refused, not skipped): %v", err)
		}

		rec, err := journal.Get(ctx, mustArtifact(t, id, "control.dump"))
		if err != nil {
			t.Fatalf("journal.Get: %v", err)
		}
		if rec.State != string(lifecycle.RemoteDeletePending) {
			t.Fatalf("journal state = %q, want %q: intent is still recorded and this artifact is still offered to DeleteRemote again next cycle", rec.State, lifecycle.RemoteDeletePending)
		}
		if rec.RemoteDeleteError == "" {
			t.Fatal("no refusal was recorded in remote_delete_error: without read_only, DeleteRemote must actually be attempted, not silently skipped")
		}
	})

	t.Run("with read_only set, the reproduction leaves the remote directory intact", func(t *testing.T) {
		seed("gitea-db-20260901T033000Z.dump", "a production Gitea backup, produced by its own systemd timer")
		bs, id := newBackupSet(t, "readonly-source", "readonly-set", true)
		journal := openJournal(t)

		// Cycle 1: transfer, verify, commit, and reach REMOTE_RETAINED.
		report := runOneCycle(t, bs, journal)
		if len(report.Sets) != 1 || report.Sets[0].Err != nil {
			t.Fatalf("cycle 1: report = %+v", report)
		}

		artifact := mustArtifact(t, id, "gitea-db-20260901T033000Z.dump")
		rec, err := journal.Get(ctx, artifact)
		if err != nil {
			t.Fatalf("journal.Get: %v", err)
		}
		if rec.State != string(lifecycle.RemoteRetained) {
			t.Fatalf("after cycle 1: journal state = %q, want %q", rec.State, lifecycle.RemoteRetained)
		}
		if _, err := os.Stat(filepath.Join(f.UploadDir, "gitea-db-20260901T033000Z.dump")); err != nil {
			t.Fatalf("the remote object should still exist after cycle 1: %v", err)
		}

		// Cycle 2: standing in for the issue's own "cycle 2", the point at
		// which an unprotected set's remote source is removed. REMOTE_RETAINED
		// has nothing further processArtifact drives it through (only
		// COMMITTED/REMOTE_DELETE_PENDING reach the delete-or-retain
		// branch at all), so this cycle must be a pure no-op for this
		// artifact.
		report = runOneCycle(t, bs, journal)
		if len(report.Sets) != 1 || report.Sets[0].Err != nil {
			t.Fatalf("cycle 2: report = %+v", report)
		}

		rec, err = journal.Get(ctx, artifact)
		if err != nil {
			t.Fatalf("journal.Get: %v", err)
		}
		if rec.State != string(lifecycle.RemoteRetained) {
			t.Fatalf("after cycle 2: journal state = %q, want %q", rec.State, lifecycle.RemoteRetained)
		}

		got, err := os.ReadFile(filepath.Join(f.UploadDir, "gitea-db-20260901T033000Z.dump"))
		if err != nil {
			t.Fatalf("the remote directory must still hold this object after cycle 2: %v", err)
		}
		if string(got) != "a production Gitea backup, produced by its own systemd timer" {
			t.Fatalf("the remote object's content changed, which a read-only pipeline must never do")
		}
	})
}

func mustArtifact(t *testing.T, set model.BackupSetID, name string) model.ArtifactID {
	t.Helper()
	id, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return id
}

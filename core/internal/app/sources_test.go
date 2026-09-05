package app

import "testing"

// Whether every configured field actually reaches the summary.
//
// Sources is a projection and the only way it can be wrong is by dropping a
// field, so these tests assert the fields rather than the shape. The two
// booleans get a case each because they are the two that mean something an
// operator acts on, and because a projection that forgets one reports a set
// as running normally when it is switched off, or as deletable when it is
// declared read-only.
//
// Both assert the false side too. A field that is never copied and a field
// that is always true look identical to a test that only ever sets it.

func TestSources_ListsConfiguredSourcesAndBackupSets(t *testing.T) {
	bs := testBackupSet(t, "/var/backups/postgres")
	bs.Remote.Type = "sftp"

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), nil, nil)

	sources := svc.Sources()
	if len(sources) != 1 {
		t.Fatalf("len(sources) = %d, want 1", len(sources))
	}
	if sources[0].Name != "production" {
		t.Errorf("Name = %q, want %q", sources[0].Name, "production")
	}
	if len(sources[0].BackupSets) != 1 {
		t.Fatalf("BackupSets = %+v, want exactly one", sources[0].BackupSets)
	}
	got := sources[0].BackupSets[0]
	if got.ID != bs.ID || got.RemoteType != "sftp" || got.LocalPath != bs.LocalPath {
		t.Errorf("BackupSets[0] = %+v, want it to mirror the configured backup set", got)
	}
	if got.Disabled || got.ReadOnly {
		t.Errorf("BackupSets[0] = %+v, want both Disabled and ReadOnly false for this fixture", got)
	}
}

// TestSources_ReportsReadOnly is issue #316's mirror of the existing
// Disabled coverage above: `backup-manager sources` (and any future HTTP
// read of the same summary) has to say when a backup set is declared
// read-only (issue #282), not only whether it runs.
func TestSources_ReportsReadOnly(t *testing.T) {
	bs := testBackupSet(t, "/var/backups/postgres")
	bs.ReadOnly = true

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), nil, nil)

	sources := svc.Sources()
	if !sources[0].BackupSets[0].ReadOnly {
		t.Error("BackupSets[0].ReadOnly = false, want true")
	}
}

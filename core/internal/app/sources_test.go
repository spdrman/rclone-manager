package app

import "testing"

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
}

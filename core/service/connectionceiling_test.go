// This file covers the connection test running against the same remote a
// cycle would use, rather than against a lookalike assembled from a
// subset of its fields.
//
// The test button copies a configured remote into a transport.Source one
// field at a time, so every field is one that can be forgotten, and a
// forgotten one is not cosmetic: it makes the check succeed where a real
// cycle fails, or fail where a real cycle succeeds. Either way the button
// answers about something other than the thing it was pressed about.
//
// Everything here is therefore written against what was ASKED of the
// transport rather than against what came back. A fake that only reports
// success cannot tell a faithful request from a lossy one, so the fake
// records instead.
package service

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// sourceRecordingTransport keeps the transport.Source it was last asked to
// list. The question these tests ask is not "did the call succeed", it is
// "what did this service actually ask the transport to do", and a fake
// that only answers success cannot tell the two apart.
type sourceRecordingTransport struct {
	lastList transport.Source
	listed   bool
}

func (r *sourceRecordingTransport) List(_ context.Context, src transport.Source) ([]transport.RemoteArtifact, error) {
	r.lastList = src
	r.listed = true
	return nil, nil
}

func (r *sourceRecordingTransport) Stat(_ context.Context, _ transport.Source, _ string) (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{}, nil
}

func (r *sourceRecordingTransport) CopyToLocal(_ context.Context, _ transport.Source, _, _ string) (transport.TransferResult, error) {
	return transport.TransferResult{}, nil
}

func (r *sourceRecordingTransport) RemoteHash(_ context.Context, _ transport.Source, _ string, _ transport.HashAlgorithm) (string, error) {
	return "", nil
}

func (r *sourceRecordingTransport) DeleteRemote(_ context.Context, _ transport.Source, _ string) error {
	return nil
}

var _ transport.Transport = (*sourceRecordingTransport)(nil)

func cappedRemoteBackupSet(t *testing.T) config.Source {
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
		LocalPath:  t.TempDir(),
		RemotePath: "/backups",
		Remote: config.Remote{
			Type:       "sftp",
			Host:       "production.example.internal",
			Port:       2222,
			User:       "backupsvc",
			KnownHosts: "/etc/backup-manager/known_hosts",
			Key: config.Key{
				File:       "/etc/backup-manager/id_ed25519",
				Passphrase: config.Passphrase{Env: "BACKUP_SSH_KEY_PASSPHRASE"},
			},
			MaxConnections: 2,
		},
	}
	return config.Source{Name: "production", BackupSets: []config.BackupSet{bs}}
}

// #355 finding 5: this test path reads the operator's REAL configured
// remote and copies its fields one by one into a transport.Source. Missing
// one is not a cosmetic omission: the reachability check the UI offers
// then runs against exactly the host the operator capped, without the cap,
// so it can fail where a real cycle succeeds or pass where a real cycle
// fails. Either way the button lies about the thing it exists to answer.
func TestTestBackupSetConnectionCarriesTheRemotesOwnCeiling(t *testing.T) {
	tr := &sourceRecordingTransport{}
	svc := New(testConfig(cappedRemoteBackupSet(t)), openTestJournal(t), tr, nil)

	got, err := svc.TestBackupSetConnection(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("TestBackupSetConnection: %v", err)
	}
	if !got.OK {
		t.Fatalf("OK = false (%q) against a transport that answers every list", got.Message)
	}
	if !tr.listed {
		t.Fatal("the transport was never asked to list anything, so there is no Source to assert about")
	}

	// Control: the fields this literal does copy really did arrive, so a
	// missing one below is a missing field and not a Source built from
	// something else entirely.
	if tr.lastList.Host != "production.example.internal" || tr.lastList.Port != 2222 || tr.lastList.User != "backupsvc" {
		t.Fatalf("the Source under test is not the configured remote at all: %+v", tr.lastList)
	}

	if tr.lastList.MaxConnections != 2 {
		t.Errorf("MaxConnections = %d, want 2: the connection test runs uncapped against the one host the operator capped", tr.lastList.MaxConnections)
	}
	if tr.lastList.PassphraseEnv != "BACKUP_SSH_KEY_PASSPHRASE" {
		t.Errorf("PassphraseEnv = %q, want %q: a set whose key is passphrase-protected cannot be connection-tested at all without it", tr.lastList.PassphraseEnv, "BACKUP_SSH_KEY_PASSPHRASE")
	}
}

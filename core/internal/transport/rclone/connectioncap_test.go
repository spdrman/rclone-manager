package rclone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// A source that says nothing about connection limits must not constrain
// them, because that is what every configuration written before this field
// existed means and none of them should change behaviour.
func TestSftpConfigLeavesConnectionsUnsetWhenNoCeilingIsAsked(t *testing.T) {
	cfg, err := sftpConfig(baseSource(t))
	if err != nil {
		t.Fatalf("sftpConfig: %v", err)
	}
	if got, ok := cfg.Get("connections"); ok {
		t.Errorf("connections was set to %q for a source that asked for no ceiling; rclone's own default (0, unlimited) must stand", got)
	}
}

// #264: the two production hosts this manager exists to pull from reject a
// third concurrent SSH connection from one address with a TCP reset, so an
// operator has to be able to say "never open more than N" and have it reach
// rclone. Without this the sftp backend's `connections` option stays at its
// default of 0, which means unlimited.
func TestSftpConfigPassesAConnectionCeilingThrough(t *testing.T) {
	src := baseSource(t)
	src.MaxConnections = 2

	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("sftpConfig: %v", err)
	}
	got, ok := cfg.Get("connections")
	if !ok {
		t.Fatalf("connections was never set, so a configured ceiling of %d cannot reach rclone and the backend stays unlimited", src.MaxConnections)
	}
	if got != "2" {
		t.Errorf("connections = %q, want %q", got, "2")
	}
}

// concurrency and connections are different settings and confusing them is
// how this bug survived: concurrency is rclone's MaxConcurrentRequestsPerFile,
// the number of outstanding requests within one connection, and it says
// nothing at all about how many connections get opened. Setting a ceiling
// must not disturb it.
func TestAConnectionCeilingDoesNotDisturbPerFileConcurrency(t *testing.T) {
	src := baseSource(t)
	src.MaxConnections = 2

	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("sftpConfig: %v", err)
	}
	if got, _ := cfg.Get("concurrency"); got != "64" {
		t.Errorf("concurrency = %q, want %q: a connection ceiling must not change the per-file request window", got, "64")
	}
}

// A ceiling that rclone would reject outright is worse than no ceiling,
// because it fails NewFs for every operation rather than only for transfers.
func TestSftpConfigRefusesANegativeConnectionCeiling(t *testing.T) {
	src := baseSource(t)
	src.MaxConnections = -1

	_, err := sftpConfig(src)
	if err == nil {
		t.Fatal("a negative connection ceiling was accepted; rclone would take it and fail every backend operation")
	}
	if !strings.Contains(err.Error(), "max_connections") {
		t.Errorf("refusal %q does not name the field an operator has to fix", err)
	}
}

func baseSource(t *testing.T) transport.Source {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("unused: sftpConfig only stats this path\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte("example.internal ssh-ed25519 AAAA\n"), 0o644); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return source(key, kh)
}

func source(key, knownHosts string) transport.Source {
	return transport.Source{
		ID:         "s/b",
		Type:       "sftp",
		Host:       "example.internal",
		User:       "backup",
		KeyFile:    key,
		KnownHosts: knownHosts,
	}
}

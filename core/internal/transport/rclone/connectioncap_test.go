package rclone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"

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

// #355 finding 7: fsFor calls info.NewFs directly, so none of rclone's own
// option defaults apply and idle_timeout lands at its Go zero value. At
// zero, rclone never creates the pool drainer at all
// (backend/sftp/sftp.go: `if f.opt.IdleTimeout > 0 { f.drain = ... }`),
// which is why an Fs this adapter failed to release held its connections
// until the process exited rather than for a bounded 60 seconds.
func TestSftpConfigRestoresRclonesOwnPoolDrainer(t *testing.T) {
	cfg, err := sftpConfig(baseSource(t))
	if err != nil {
		t.Fatalf("sftpConfig: %v", err)
	}
	got, ok := cfg.Get("idle_timeout")
	if !ok {
		t.Fatal("idle_timeout was never set, so it reaches rclone as 0 and the sftp backend never creates its pool drainer; a pool nothing releases is then held forever rather than for a bounded time")
	}
	if got != "60s" {
		t.Errorf("idle_timeout = %q, want %q (rclone's own documented default for this option)", got, "60s")
	}
}

// #355 finding 1: one recursive List is not one connection unless
// something says so. rclone walks a tree it cannot list recursively with
// one goroutine per --checkers, and the sftp backend gives each goroutine
// its own connection, so discovery alone opens eight against a host that
// may reject the third.
//
// The observable proof is in connections_gate_test.go, against a real
// server. This is the same claim without a container, so a machine with no
// docker still fails when the bound is removed.
func TestOneConnectionAtATimeBoundsTheWalkAndTheTransfer(t *testing.T) {
	ctx := oneConnectionAtATime(context.Background())
	ci := fs.GetConfig(ctx)

	if ci.Checkers != 1 {
		t.Errorf("Checkers = %d, want 1: rclone walks a tree with one goroutine per checker and each one takes its own SFTP connection, so anything above 1 makes plain discovery open more connections than an operator was ever told about", ci.Checkers)
	}
	if ci.MultiThreadStreams != 1 {
		t.Errorf("MultiThreadStreams = %d, want 1: rclone splits a download above --multi-thread-cutoff across this many concurrent readers, and on sftp each reader is its own connection, so the multi-GB dumps this manager exists to fetch are exactly the transfers that fan out", ci.MultiThreadStreams)
	}
}

// The bound must be a bound and nothing else. rclone captures the ambient
// ConfigInfo into an Fs at construction, and the mid-transfer cancellation
// gate exists because a caller's own settings reaching the wrong operation
// is how a transfer stops being interruptible.
func TestOneConnectionAtATimeLeavesEverythingElseAlone(t *testing.T) {
	base, baseCI := fs.AddConfig(context.Background())
	if err := (&baseCI.BwLimit).Set("64k"); err != nil {
		t.Fatalf("set bwlimit: %v", err)
	}
	baseCI.Transfers = 7

	got := fs.GetConfig(oneConnectionAtATime(base))

	if got.BwLimit.String() != baseCI.BwLimit.String() {
		t.Errorf("BwLimit = %s, want %s: the caller's own bandwidth limit has to survive", got.BwLimit, baseCI.BwLimit)
	}
	if got.Transfers != 7 {
		t.Errorf("Transfers = %d, want 7", got.Transfers)
	}
}

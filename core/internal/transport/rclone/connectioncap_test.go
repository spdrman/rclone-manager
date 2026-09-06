package rclone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the in-process half of #264 and #355: how many connections
// one operation is allowed to open, how long it may spend dialling, and
// what happens to a caller's own settings on the way through.
//
// The observable half, what a real sshd actually sees, lives in
// core/tests/machinegate/connections_test.go and needs Docker. This half
// deliberately needs nothing: every case here reads an option map or a
// ConfigInfo, so a machine with no container runtime still fails when a
// bound is removed. That matters because the bounds below are invisible
// when they work and expensive when they do not; the production hosts this
// manager pulls from reject a third simultaneous SSH connection with a TCP
// reset, so an unbounded walk does not run slowly, it fails, as a bare
// "connection refused" naming nothing.
//
// Two shapes recur and both are load-bearing.
//
// Every bound is checked as a BOUND, not as an assignment: a caller that
// already asked for less keeps what it asked for, and a caller's unrelated
// settings survive. rclone captures the ambient ConfigInfo into an Fs at
// construction and never re-reads it, so a bound that overwrote things
// would make one caller's bandwidth limit permanent for that Fs's whole
// life.
//
// And the connect-timeout rows check rclone's own default first, and fail
// if it is already at or under the ceiling. A ceiling test against a
// default that is already lower proves nothing, and it would go quiet
// exactly when an rclone upgrade changed the number underneath it.

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

// baseSource is a valid sftp Source with the two files sftpConfig
// actually stats. Neither file's CONTENT is ever read on this path (the
// key is only ever handed to rclone as a path, and known_hosts likewise),
// so a placeholder is enough, and the key is written 0600 because #293's
// mode check refuses anything else before any of these cases get to run.
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

// source is the Source literal itself, separated from the file setup
// above so that what these cases hold constant (an sftp source with a key,
// a known_hosts and no connection ceiling) is readable without the
// t.TempDir plumbing around it. Every field sftpConfig branches on is set
// here, so a case can vary exactly one of them by name.
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
// The observable proof is in core/tests/machinegate/connections_test.go,
// against a real server. This is the same claim without a container, so a
// machine with no docker still fails when the bound is removed.
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

// The connect-timeout ceiling (#415). See ConnectTimeout's own doc for
// where fifteen seconds comes from; these three rows are what keeps it
// from quietly becoming something else.
//
// The observable proof that rclone honours a --contimeout at all is
// errors_test.go's TestClassify_ConnectTimeoutRcloneImposedIsTransient,
// which dials a blackholed address and watches the deadline fire. This is
// the other half: that the value rclone honours, on the path every
// operation in this adapter takes, is ours and not rclone's own 60s.
func TestOneConnectionAtATimeBoundsTheConnectTimeout(t *testing.T) {
	// rclone's own default is what a caller who never asked gets, and it
	// is the number #415 is about.
	base := fs.GetConfig(context.Background()).ConnectTimeout
	if base <= fs.Duration(ConnectTimeout) {
		t.Fatalf("rclone's default --contimeout is %s, already at or under the %s ceiling; "+
			"this row cannot show a ceiling being applied, so it proves nothing", time.Duration(base), ConnectTimeout)
	}

	got := fs.GetConfig(oneConnectionAtATime(context.Background())).ConnectTimeout
	if got != fs.Duration(ConnectTimeout) {
		t.Errorf("ConnectTimeout = %s, want %s: every operation in this adapter goes through oneConnectionAtATime, "+
			"and app.DefaultRetryPolicy spends six attempts on this number, so rclone's own %s default is what "+
			"turns a two-minute budget into six and a half minutes (#415)",
			time.Duration(got), ConnectTimeout, time.Duration(base))
	}
}

// A ceiling, not an assignment: a caller who already asked for less keeps
// what it asked for. errors_test.go's connect-timeout evidence depends on
// this directly (it runs six real dials into a blackhole at 500ms each,
// which at fifteen seconds would be ninety seconds of gate).
func TestOneConnectionAtATimeLeavesAShorterConnectTimeoutAlone(t *testing.T) {
	const asked = 500 * time.Millisecond
	base, baseCI := fs.AddConfig(context.Background())
	baseCI.ConnectTimeout = fs.Duration(asked)

	if got := fs.GetConfig(oneConnectionAtATime(base)).ConnectTimeout; got != fs.Duration(asked) {
		t.Errorf("ConnectTimeout = %s, want the %s the caller asked for: this is a ceiling on rclone's default, "+
			"not a policy that overrides a caller who wants to wait less", time.Duration(got), asked)
	}
}

// A ConnectTimeout of zero is rclone's "no connect timeout at all", which
// is the one value that would make the retry budget unbounded again, so it
// gets the ceiling like anything else above it.
func TestOneConnectionAtATimeBoundsAnUnsetConnectTimeout(t *testing.T) {
	base, baseCI := fs.AddConfig(context.Background())
	baseCI.ConnectTimeout = 0

	if got := fs.GetConfig(oneConnectionAtATime(base)).ConnectTimeout; got != fs.Duration(ConnectTimeout) {
		t.Errorf("ConnectTimeout = %s, want %s: zero means no deadline, which is the one setting that puts "+
			"app.DefaultRetryPolicy's budget back to unbounded", time.Duration(got), ConnectTimeout)
	}
}

// TestAStalledSourceIsBoundedByADifferentNumber is the honesty guard on
// app.DefaultRetryPolicy's budget (issue #415).
//
// That budget is "six attempts, at most ConnectTimeout each", and it is
// true of a source that never answers, which is what an operator means when
// they say a NAS is off. It is NOT true of every failure. ConnectTimeout
// bounds a dial; a source that answers, accepts the session and then goes
// quiet partway through a read is bounded by rclone's --timeout, an idle
// timeout on the transfer, which this adapter deliberately does not touch.
//
// So there are two numbers, they are far apart, and the way #415 comes back
// is somebody reading "six times ConnectTimeout" as the whole story. This
// row fails if they ever become one number, in either direction: by the
// idle timeout being pulled down to the connect timeout (which would start
// failing slow-but-live links), or by this adapter starting to override it
// at all (which would move a bound app's doc describes without app's doc
// knowing).
func TestAStalledSourceIsBoundedByADifferentNumber(t *testing.T) {
	rcloneDefault := fs.GetConfig(context.Background())
	ours := fs.GetConfig(oneConnectionAtATime(context.Background()))

	idle := time.Duration(ours.Timeout)
	connect := time.Duration(ours.ConnectTimeout)
	t.Logf("a dial is bounded by ConnectTimeout (%s, ours); a stalled transfer by --timeout (%s, rclone's own default, untouched)",
		connect, idle)

	// This adapter bounds one of them and not the other, on purpose.
	if idle != time.Duration(rcloneDefault.Timeout) {
		t.Errorf("this adapter now sets rclone's --timeout to %s, against the %s default it used to leave alone. "+
			"app.DefaultRetryPolicy's doc describes the stalled-source worst case in terms of that default and would "+
			"now be wrong; and --timeout is a bound on a transfer making no progress, so shortening it fails "+
			"slow-but-live links rather than unreachable ones",
			idle, time.Duration(rcloneDefault.Timeout))
	}

	// And they have to stay recognisably different, or "six attempts at
	// ConnectTimeout each" would quietly start reading as the whole budget.
	if idle <= connect {
		t.Errorf("--timeout is %s, at or under the %s connect timeout, so the two bounds have collapsed into one. "+
			"app.DefaultRetryPolicy's doc has a whole section explaining that they are different failures with "+
			"different costs; if that is genuinely no longer true, that section is what has to change",
			idle, connect)
	}
}

package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// This file is issue #295's end-to-end proof: a source marked
// config.Remote.Sensitive must not leak its host, port or account name
// into a log line (internal/obs) or a journal detail
// (internal/state.Journal.RecordTransition), even for an error this
// project never formats itself. Every failure below is real, not
// simulated: a genuine TCP dial against a real, closed port, driven
// through the actual rclone adapter (internal/transport/rclone), exactly
// the class of failure the issue's own reproduction shows (Go's net
// dialer's "connect: connection refused", surfaced through rclone's sftp
// backend's NewFs). See internal/transport/rclone/errors_test.go's
// TestClassify_Transient_RealConnectionRefused for the same technique.

// freeClosedTCPPort reserves an ephemeral TCP port and releases it
// immediately: nothing is listening on the returned port by the time this
// returns, so a real dial against it fails with a real
// "connect: connection refused" rather than anything this test fabricates.
func freeClosedTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// generateSFTPClientKey writes a throwaway ed25519 private key to disk and
// returns its path. rclone's sftp backend parses the configured key before
// it ever dials, so a garbage placeholder file (fine for the fakeTransport
// tests elsewhere in this package) would fail with a key-parsing error
// instead of the real network failure these tests need to provoke.
func generateSFTPClientKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "rclone-manager-295-test-client")
	if err != nil {
		t.Fatalf("ssh.MarshalPrivateKey: %v", err)
	}
	path := filepath.Join(t.TempDir(), "client_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing client key: %v", err)
	}
	return path
}

// unreachableRemote builds a config.Remote pointed at a real, closed TCP
// port on localhost: type "sftp", with everything internal/transport/
// rclone's sftpConfig requires to build a real SSH config present and
// valid, but nothing actually listening. Every List/CopyToLocal call
// against it fails with a real "dial tcp 127.0.0.1:<port>: connect:
// connection refused".
func unreachableRemote(t *testing.T, sensitive bool) (config.Remote, int) {
	t.Helper()
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
		t.Fatalf("writing known_hosts: %v", err)
	}
	port := freeClosedTCPPort(t)
	return config.Remote{
		Type:       "sftp",
		Host:       "127.0.0.1",
		Port:       port,
		User:       "backupuser",
		Key:        config.Key{File: generateSFTPClientKey(t)},
		KnownHosts: knownHosts,
		Sensitive:  sensitive,
	}, port
}

// fastRetryPolicy makes every retry.Do call in these tests attempt exactly
// once. The failures provoked here are permanent (nothing is ever going to
// start listening on the closed port), so the default multi-attempt,
// multi-second backoff schedule would only make every test slower without
// proving anything more.
func fastRetryPolicy() retry.Policy { return retry.Policy{MaxAttempts: 1} }

func unreachableBackupSet(t *testing.T, remote config.Remote) config.BackupSet {
	t.Helper()
	return config.BackupSet{
		Name:       "gitea-forge-dump",
		ID:         mustSetID(t, "cicd-pipeline", "gitea-forge-dump"),
		Remote:     remote,
		RemotePath: "/upload",
		LocalPath:  t.TempDir(),
		Completion: config.Completion{Strategy: "rename"},
	}
}

// TestSensitiveEndpoints_OnlyCollectsOptedInRemotes is a focused unit test
// for the small translation app.New relies on: across every configured
// Source and BackupSet, only a Remote with Sensitive true contributes an
// obs.Endpoint, and a config with none at all yields no endpoints (nil,
// not an empty non-nil slice, though callers should not care which).
func TestSensitiveEndpoints_OnlyCollectsOptedInRemotes(t *testing.T) {
	quiet := config.BackupSet{Name: "quiet", Remote: config.Remote{Host: "public.example.com", Port: 22, User: "svc"}}
	loud := config.BackupSet{Name: "loud", Remote: config.Remote{Host: "internal.example.com", Port: 2222, User: "backup", Sensitive: true}}

	cfg := &config.Config{Sources: []config.Source{
		{Name: "one", BackupSets: []config.BackupSet{quiet, loud}},
	}}

	got := sensitiveEndpoints(cfg)
	if len(got) != 1 {
		t.Fatalf("sensitiveEndpoints returned %d endpoints, want exactly 1: %+v", len(got), got)
	}
	want := obs.Endpoint{Host: "internal.example.com", Port: 2222, User: "backup"}
	if got[0] != want {
		t.Fatalf("sensitiveEndpoints()[0] = %+v, want %+v", got[0], want)
	}

	if got := sensitiveEndpoints(&config.Config{Sources: []config.Source{{Name: "one", BackupSets: []config.BackupSet{quiet}}}}); len(got) != 0 {
		t.Fatalf("sensitiveEndpoints with no opted-in remote returned %+v, want none", got)
	}

	if got := sensitiveEndpoints(nil); got != nil {
		t.Fatalf("sensitiveEndpoints(nil) = %+v, want nil", got)
	}
}

// TestSensitiveEndpointRedactsRealDialFailureFromLog is issue #295's
// behavioral contract for the log line: a source whose remote is marked
// Sensitive fails discovery against a real, closed TCP port, and the
// resulting log line must not contain the port, while still naming the
// source that failed.
func TestSensitiveEndpointRedactsRealDialFailureFromLog(t *testing.T) {
	remote, port := unreachableRemote(t, true)
	bs := unreachableBackupSet(t, remote)

	journal := openJournal(t)
	var buf bytes.Buffer
	logger := obs.New(&buf, obs.LevelInfo)

	svc := New(testConfig(t, testSource("cicd-pipeline", bs)), journal, rclone.New(), logger)
	svc.RetryPolicy = fastRetryPolicy()
	svc.Now = fixedNow(epoch)

	svc.RunCycle(context.Background())

	out := buf.String()
	portStr := strconv.Itoa(port)
	if strings.Contains(out, portStr) {
		t.Fatalf("log output contains the sensitive port %q, want it redacted:\n%s", portStr, out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("log output never mentions the redaction placeholder, want at least one occurrence:\n%s", out)
	}
	if !strings.Contains(out, "cicd-pipeline/gitea-forge-dump") {
		t.Fatalf("log output lost the source id entirely, want the line to still say what failed:\n%s", out)
	}
}

// TestSensitiveEndpointRedactsResolvedIPFromDNSHostname is the adversarial
// review's Critical finding on PR #304: TestSensitiveEndpointRedactsReal-
// DialFailureFromLog above configures Host as "127.0.0.1", a bare IP
// literal, the one case where the CONFIGURED string and the DNS-RESOLVED
// address Go's net stack actually puts into a dial failure coincide. That
// test cannot tell "redaction works" apart from "redaction only works
// because Host already happened to be an IP". This test configures Host as
// a DNS hostname instead ("localhost", resolved for real via
// net.LookupHost, not assumed), so the resolved address and the configured
// string differ exactly the way they would for a real deployment's
// dynamic-DNS or *.internal.example.com remote: the log line must still
// not contain whichever address(es) "localhost" actually resolved to.
func TestSensitiveEndpointRedactsResolvedIPFromDNSHostname(t *testing.T) {
	resolved, err := net.LookupHost("localhost")
	if err != nil || len(resolved) == 0 {
		t.Skipf("this host cannot resolve %q, cannot exercise the DNS-hostname path: %v", "localhost", err)
	}

	remote, port := unreachableRemote(t, true)
	remote.Host = "localhost"
	bs := unreachableBackupSet(t, remote)

	journal := openJournal(t)
	var buf bytes.Buffer
	logger := obs.New(&buf, obs.LevelInfo)

	svc := New(testConfig(t, testSource("cicd-pipeline", bs)), journal, rclone.New(), logger)
	svc.RetryPolicy = fastRetryPolicy()
	svc.Now = fixedNow(epoch)

	svc.RunCycle(context.Background())

	out := buf.String()
	for _, ip := range resolved {
		if strings.Contains(out, ip) {
			t.Fatalf("log output contains %q, the address DNS resolved %q to, want it redacted:\n%s", ip, "localhost", out)
		}
	}
	portStr := strconv.Itoa(port)
	if strings.Contains(out, portStr) {
		t.Fatalf("log output contains the sensitive port %q, want it redacted:\n%s", portStr, out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("log output never mentions the redaction placeholder, want at least one occurrence:\n%s", out)
	}
	if !strings.Contains(out, "cicd-pipeline/gitea-forge-dump") {
		t.Fatalf("log output lost the source id entirely, want the line to still say what failed:\n%s", out)
	}
}

// TestSensitiveEndpointRedactsRealDialFailureFromJournal is issue #295's
// behavioral contract for the durable journal: an artifact already at
// DISCOVERED, whose backup set's remote is marked Sensitive, fails its
// transfer copy against the same real, closed TCP port, and the FAILED
// transition's state_transitions.detail must not contain the port either.
// #284 is why this matters beyond the log: that detail is what gets copied
// into recovery manifests, so redacting the log and not this would only
// move the leak somewhere harder to find.
func TestSensitiveEndpointRedactsRealDialFailureFromJournal(t *testing.T) {
	remote, port := unreachableRemote(t, true)
	src := config.Source{Name: "cicd-pipeline"}
	bs := unreachableBackupSet(t, remote)

	journal := openJournal(t)
	ctx := context.Background()

	artifact, err := model.NewArtifactID(bs.ID, "no-hash")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := journal.Discover(ctx, artifact, "seed-key", "no-hash", state.RemoteIdentity{}, epoch); err != nil {
		t.Fatalf("seeding a DISCOVERED artifact: %v", err)
	}
	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	svc := New(testConfig(t, testSource("cicd-pipeline", bs)), journal, rclone.New(), nil)
	svc.RetryPolicy = fastRetryPolicy()
	svc.Now = fixedNow(epoch)

	svc.processArtifact(ctx, sourceFor(svc.Config, src, bs), bs, rec)

	activity, err := journal.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	var (
		failedDetail string
		found        bool
	)
	for _, a := range activity {
		if a.To == "FAILED" {
			failedDetail = a.Detail
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no FAILED transition recorded; activity=%+v", activity)
	}

	portStr := strconv.Itoa(port)
	if strings.Contains(failedDetail, portStr) {
		t.Fatalf("journal detail contains the sensitive port %q, want it redacted: %q", portStr, failedDetail)
	}
	if !strings.Contains(failedDetail, "[REDACTED]") {
		t.Fatalf("journal detail never mentions the redaction placeholder, want at least one occurrence: %q", failedDetail)
	}
}

// TestSensitiveEndpointDefaultBehaviourUnchanged is the regression control
// on the two tests above: a source that has NOT opted in
// (config.Remote.Sensitive left false, today's default) must keep seeing
// the real host and port in both the log line and the journal detail,
// exactly as every deployment does today. Redaction is opt-in; this is
// what proves it, rather than merely asserting the opt-in case works.
func TestSensitiveEndpointDefaultBehaviourUnchanged(t *testing.T) {
	remote, port := unreachableRemote(t, false)
	src := config.Source{Name: "cicd-pipeline"}
	bs := unreachableBackupSet(t, remote)

	journal := openJournal(t)
	ctx := context.Background()
	var buf bytes.Buffer
	logger := obs.New(&buf, obs.LevelInfo)

	artifact, err := model.NewArtifactID(bs.ID, "no-hash")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := journal.Discover(ctx, artifact, "seed-key", "no-hash", state.RemoteIdentity{}, epoch); err != nil {
		t.Fatalf("seeding a DISCOVERED artifact: %v", err)
	}
	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	svc := New(testConfig(t, testSource("cicd-pipeline", bs)), journal, rclone.New(), logger)
	svc.RetryPolicy = fastRetryPolicy()
	svc.Now = fixedNow(epoch)

	svc.processArtifact(ctx, sourceFor(svc.Config, src, bs), bs, rec)

	portStr := strconv.Itoa(port)
	out := buf.String()
	if !strings.Contains(out, portStr) {
		t.Fatalf("default (non-sensitive) log output no longer contains the port %q; default behaviour must stay unchanged:\n%s", portStr, out)
	}

	activity, err := journal.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	var (
		failedDetail string
		found        bool
	)
	for _, a := range activity {
		if a.To == "FAILED" {
			failedDetail = a.Detail
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no FAILED transition recorded; activity=%+v", activity)
	}
	if !strings.Contains(failedDetail, portStr) {
		t.Fatalf("default (non-sensitive) journal detail no longer contains the port %q; default behaviour must stay unchanged: %q", portStr, failedDetail)
	}
}

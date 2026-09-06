// Docker-backed evidence for what this adapter does to a server's
// connection table (#355, and the production failure behind #264).
//
// The rest of #355's tests are about the `max_connections` ceiling, which
// is the side dish. This file is about the main course: an Fs holds a pool
// of open SSH connections, and the two questions that actually decide
// whether a backup succeeds against a host that rejects a third
// simultaneous connection are "does an operation hand its connections back
// when it is done" and "how many does one operation open in the first
// place". Neither can be answered from inside the process, so both are
// answered here, by asking the server.
//
// Three measurements, deliberately different in kind:
//
//   - establishedConns is the CURRENT state of the server's TCP table. It
//     answers "is anything still open", which is the leak question.
//   - acceptedLogins is a MONOTONIC counter of successful SSH logins, read
//     out of sshd's own log. It answers "how many connections did this one
//     operation open", which a current-state sample cannot: a walk that
//     opens eight connections and closes them can be finished before any
//     sampler looks. A cumulative counter has no such window.
//   - sshClientGoroutines is the same leak question asked of THIS PROCESS
//     rather than of the server, and it is the one that can attribute a
//     survivor (#455). The fixture publishes -p 127.0.0.1::22, so the
//     socket sshd sees belongs to Docker's userland forwarder, not to us;
//     a row that outlives a drain is either our pool or the forwarding
//     chain swallowing the close, and only a reading taken from inside
//     tells those apart.
//
// Every assertion below has a positive control, because all three
// measurements could report zero for the boring reason that they see
// nothing at all.
package machinegate_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// dockerProbeBudget bounds every docker call this file makes. #161's whole
// lesson is that an unbounded `docker` is how a suite turns into a
// 25-minute hang, and a measurement helper is no more entitled to hang
// than a fixture is.
const dockerProbeBudget = 15 * time.Second

// connectionDrainBudget is how long an operation gets to have handed its
// connections back after it has returned.
//
// It is deliberately far below the 60s idle_timeout sftpConfig now sets.
// That timer is rclone's own drainer, and a leaked pool WILL eventually
// close through it; if this budget were anywhere near 60s a genuine leak
// could pass by self-healing rather than by being fixed. Five seconds is
// generous for "the server noticed a socket we closed synchronously" and
// nowhere near "the drainer got round to it".
const connectionDrainBudget = 5 * time.Second

// establishedConns and acceptedLogins used to live here, each with its own
// bounded `docker exec`. They are Source.EstablishedConnections and
// Source.AcceptedLogins now (#448): they are the probes the connection-cap
// case (#264) wants too, and a test under core/tests that reached them by
// exec'ing docker itself would be the bypass the testtier guard exists to
// stop.

// sshClientLoopFrame is the stack frame golang.org/x/crypto/ssh puts on the
// goroutine it starts for each client connection. newMux runs exactly one
// mux.loop per connection and that goroutine only returns once the
// connection's transport is closed, so counting these frames is a direct
// reading of how many SSH connections this process is still holding.
const sshClientLoopFrame = "ssh.(*mux).loop"

// sshClientGoroutines is the client-side half of establishedConns, and it
// is the half that can attribute a survivor.
//
// establishedConns reads the server's table from inside the container, and
// the fixture publishes -p 127.0.0.1::22, so the socket sshd sees is the
// container-side leg of Docker's userland forwarder rather than anything
// this process opened. A count that stays at 1 there is therefore two
// different stories wearing the same clothes: our pool never let go, or the
// forwarding chain never passed the close on. Reading our own goroutines
// tells them apart from inside, with no reproduction and no guessing.
//
// There is no t.Parallel in this package and the adapter never puts an Fs
// in rclone's cache, so once an operation has returned and its Fs has been
// shut down the correct reading here is zero.
func sshClientGoroutines() (int, string) {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var (
		count int
		found []string
	)
	for i, stanza := range strings.Split(string(buf), "\n\ngoroutine ") {
		if !strings.Contains(stanza, sshClientLoopFrame) {
			continue
		}
		count++
		if i > 0 {
			stanza = "goroutine " + stanza
		}
		found = append(found, strings.TrimSpace(stanza))
	}
	return count, strings.Join(found, "\n\n")
}

// softProbe runs a diagnostic command and returns whatever it managed to
// say. Nothing in here may fail the test: it only ever runs on a path that
// has already failed, and a diagnostic that replaces the failure it was
// printed to explain is worse than no diagnostic at all.
func softProbe(name string, args ...string) string {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Sprintf("(%s is not on PATH, so this one cannot be answered here: %v)\n", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerProbeBudget)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		text = "(no output)"
	}
	if err != nil {
		return fmt.Sprintf("%s\n(`%s %s` exited: %v)\n", text, name, strings.Join(args, " "), err)
	}
	return text + "\n"
}

// drainDiagnostics is what the next failed drain gets to be read with.
//
// A count cannot be argued with, only stared at. These three readings can:
// the server's own table with peers and states rather than a number, the
// host's view of what this test process actually still has open on the
// published port, and the stacks of any SSH client this process is still
// running. Between them they say which side the survivor is on, which is
// the whole question and is not answerable from a count.
func drainDiagnostics(t *testing.T, f *machines.Source, stacks string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("--- netstat -tn inside the container: peer and state, not a count ---\n")
	b.WriteString(f.ConnectionTable(t) + "\n")
	fmt.Fprintf(&b, "--- lsof -nP -iTCP:%d on the host, for this test process (pid %d) ---\n", f.Port, os.Getpid())
	if f.Port == 0 {
		b.WriteString("(the fixture published no host port, so there is nothing on the host to look at)\n")
	} else {
		b.WriteString(softProbe("lsof", "-nP", fmt.Sprintf("-iTCP:%d", f.Port), "-a", "-p", strconv.Itoa(os.Getpid())))
	}
	b.WriteString("--- SSH client goroutines in this test process ---\n")
	if stacks == "" {
		b.WriteString("(none: this process is not running a single " + sshClientLoopFrame + ")\n")
	} else {
		b.WriteString(stacks + "\n")
	}
	return b.String()
}

// drainVerdict names the side the survivor is on, so the reader does not
// have to work it out from two numbers at four in the morning.
func drainVerdict(conns, clients int) string {
	switch {
	case conns > 0 && clients == 0:
		return "This process holds no SSH client at all, so nothing here is still talking to the server. The fixture publishes -p 127.0.0.1::22, so the socket sshd can see is the container-side leg of Docker's userland forwarder and not a socket this process opened: an ESTABLISHED row with a clean client is the forwarding chain not passing the close on, not a pool this adapter leaked. Read the lsof below before touching the adapter."
	case conns == 0 && clients > 0:
		return "The server's table is empty but this process is still running SSH client goroutines, so the leak is on this side: the far end is gone and the client has not noticed."
	default:
		return "This process is still holding live SSH clients AND the server still sees connections, which is a genuine leak: an Fs that is never released holds its pool until the process exits, which is exactly the failure #355 is about."
	}
}

// requireNoConnections fails unless BOTH sides have let go within
// connectionDrainBudget: the server's connection table has emptied, and
// this process is no longer running an SSH client of its own.
//
// Both, because either one alone is ambiguous. The server's table is read
// through Docker's forwarder, so a row that outlives the drain may belong
// to the forwarder rather than to us. Our own goroutines are the reading
// that cannot be confused for anything else, and asserting the pair is what
// turns the next occurrence from a mystery into a one-line attribution.
func requireNoConnections(t *testing.T, f *machines.Source, after string) {
	t.Helper()
	deadline := time.Now().Add(connectionDrainBudget)
	for {
		conns := f.EstablishedConnections(t)
		clients, stacks := sshClientGoroutines()
		if conns == 0 && clients == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s left %d connection(s) open on the server and %d live SSH client goroutine(s) in this process %s later.\n%s\n%s",
				after, conns, clients, connectionDrainBudget, drainVerdict(conns, clients), drainDiagnostics(t, f, stacks))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSFTPConnectionsAreReleasedAndBounded(t *testing.T) {
	f := machines.Start(t).Source(t)
	a := rclone.New()
	ctx := context.Background()

	// Positive control for BOTH measurements, and it has to come first:
	// every assertion below reads "and then there were none", which is
	// also what a probe that can see nothing at all would say.
	t.Run("TheProbesCanSeeAConnection", func(t *testing.T) {
		before := f.AcceptedLogins(t)

		held := rawSFTPFs(t, ctx, f, "")
		if got := f.EstablishedConnections(t); got < 1 {
			shutdownFs(ctx, held)
			t.Fatalf("established connections = %d while an Fs is deliberately held open; this probe cannot see a connection, so nothing else in this file would prove anything", got)
		}
		if got := f.AcceptedLogins(t); got <= before {
			shutdownFs(ctx, held)
			t.Fatalf("accepted logins = %d after opening an Fs, was %d before; this probe cannot count a login, so the fan-out assertions below would pass on any behaviour", got, before)
		}
		// The third probe, and the one that reads this process rather than
		// the server. Everything below trusts a zero from it to mean "this
		// process holds no SSH client", and a probe looking for a frame
		// that no longer exists says exactly the same zero. So it has to be
		// caught seeing one first.
		if got, _ := sshClientGoroutines(); got < 1 {
			shutdownFs(ctx, held)
			t.Fatalf("SSH client goroutines = %d while an Fs is deliberately held open and the server can see its connection; x/crypto/ssh runs one %s per live client connection, so a probe that cannot find one here would read zero at every drain below and would blame the forwarder for a client this process really is still holding", got, sshClientLoopFrame)
		}

		shutdownFs(ctx, held)
		requireNoConnections(t, f, "shutdownFs on a deliberately held Fs")
	})

	// The subject of #355. Every operation builds its own Fs, and before
	// this each one abandoned it with its pool still open.
	t.Run("EveryOperationReleasesItsPool", func(t *testing.T) {
		writeUploadFile(t, f, "release-me.bin", []byte("release target"))
		src := f.TransportSource("release", "")

		if _, err := a.List(ctx, src); err != nil {
			t.Fatalf("List: %v", err)
		}
		requireNoConnections(t, f, "List")

		if _, err := a.Stat(ctx, src, "release-me.bin"); err != nil {
			t.Fatalf("Stat: %v", err)
		}
		requireNoConnections(t, f, "Stat")

		local := filepath.Join(t.TempDir(), "release-me.bin.partial")
		if _, err := a.CopyToLocal(ctx, src, "release-me.bin", local); err != nil {
			t.Fatalf("CopyToLocal: %v", err)
		}
		requireNoConnections(t, f, "CopyToLocal")

		// This one fails, on purpose: the fixture's account has no shell,
		// so it cannot compute a hash. An operation that returns an error
		// still has to hand its connections back, and a failing path is
		// exactly where a release is easiest to forget.
		if _, err := a.RemoteHash(ctx, src, "release-me.bin", transport.SHA256); err == nil {
			t.Log("note: the fixture reported SHA256 support; this case is still a release assertion either way")
		}
		requireNoConnections(t, f, "RemoteHash")

		if err := a.DeleteRemote(ctx, src, "release-me.bin"); err != nil {
			t.Fatalf("DeleteRemote: %v", err)
		}
		requireNoConnections(t, f, "DeleteRemote")

		// Repetition is the actual shape of the bug: one abandoned pool is
		// invisible, and a daemon that polls forever abandons one per
		// operation per cycle.
		for i := range 10 {
			if _, err := a.List(ctx, src); err != nil {
				t.Fatalf("List #%d: %v", i, err)
			}
		}
		requireNoConnections(t, f, "ten Lists in a row")
	})

	// The one leak path releasing on the way out does NOT close, because
	// there is no way out to release on: rclone's sftp backend returns a
	// live Fs alongside fs.ErrorIsFile, and an adapter that only looks at
	// err drops it with its pool already open.
	//
	// Reachable by ordinary misconfiguration: config.Validate asks only
	// that remote_path be a non-empty absolute path, so pointing it at a
	// file is accepted and the daemon then repeats it every poll interval.
	t.Run("ARemotePathThatNamesAFileDoesNotLeak", func(t *testing.T) {
		writeUploadFile(t, f, "not-a-directory.dump", []byte("this is a file, not a directory"))
		src := f.TransportSource("file-root", "not-a-directory.dump")

		for i := range 5 {
			if _, err := a.List(ctx, src); err == nil {
				t.Fatalf("List #%d against a root that names a file succeeded; this test's premise is gone", i)
			}
		}
		requireNoConnections(t, f, "five Lists against a root that names a file")

		if _, err := a.Stat(ctx, src, "not-a-directory.dump"); err == nil {
			t.Log("note: Stat against a file root succeeded")
		}
		requireNoConnections(t, f, "Stat against a root that names a file")
	})

	// #355 finding 1: List walks the whole tree, sftp has no native
	// recursive listing, so rclone walks it with one goroutine per
	// --checkers and each goroutine takes its own connection. Eight
	// connections for a plain discovery is not a ceiling an operator can
	// be expected to guess at, and against a host that rejects a third it
	// is a failed discovery.
	t.Run("ARecursiveListOpensOneConnection", func(t *testing.T) {
		const dirs = 24
		for i := range dirs {
			dir := filepath.Join(f.UploadDir, "tree", fmt.Sprintf("run-%02d", i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("seed %s: %v", dir, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "artifact.dump"), []byte("x"), 0o644); err != nil {
				t.Fatalf("seed artifact in %s: %v", dir, err)
			}
		}
		src := f.TransportSource("tree", "tree")

		// Control: the same walk with rclone's own default width really
		// does open more than one connection, so "exactly one" below is a
		// statement about this adapter and not about the fixture being
		// too small to fan out.
		control := rawSFTPFs(t, ctx, f, "tree")
		wideCtx, ci := fs.AddConfig(ctx)
		ci.Checkers = 8
		before := f.AcceptedLogins(t)
		objs, _, err := walk.GetAll(wideCtx, control, "", true, -1)
		if err != nil {
			shutdownFs(ctx, control)
			t.Fatalf("control walk: %v", err)
		}
		wide := f.AcceptedLogins(t) - before
		shutdownFs(ctx, control)
		// Partition the blame before going anywhere near List. This control
		// opened seven or eight connections and, until this line existed,
		// nothing asserted it had handed a single one of them back: the
		// requireNoConnections at the end of the subtest counts the server's
		// whole table, so a survivor from this walk was reported as "a
		// recursive List left a connection open" and sent the reader after
		// the wrong Fs.
		requireNoConnections(t, f, "the control walk at --checkers 8")
		if len(objs) != dirs {
			t.Fatalf("control walk found %d objects, want %d; it did not walk the tree this test seeded", len(objs), dirs)
		}
		if wide <= 1 {
			t.Fatalf("a walk at --checkers 8 over %d directories opened %d connection(s); this fixture does not fan out, so the assertion below would pass whatever List did", dirs, wide)
		}
		t.Logf("control: a walk at --checkers 8 over %d directories opened %d connections", dirs, wide)

		before = f.AcceptedLogins(t)
		got, err := a.List(ctx, src)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		opened := f.AcceptedLogins(t) - before
		if len(got) != dirs {
			t.Fatalf("List found %d objects, want %d", len(got), dirs)
		}
		if opened != 1 {
			t.Errorf("discovering a %d-directory tree opened %d connections, want exactly 1; a nested layout is normal here (gitea-runs/<RUN_ID>/*.dump), so discovery alone must not need a connection budget an operator was never told about", dirs, opened)
		}
		requireNoConnections(t, f, "a recursive List")
	})

	// The same question for the transfer. rclone splits a download above
	// --multi-thread-cutoff (256Mi by default) across --multi-thread-streams
	// (4 by default) concurrent readers, and on sftp each reader is its own
	// connection. A multi-GB database dump is precisely what this manager
	// pulls, so the default path for the artifacts it exists to fetch is
	// four connections, not one.
	t.Run("ACopyAboveTheMultiThreadCutoffOpensOneConnection", func(t *testing.T) {
		content := bytes.Repeat([]byte("rclone-manager"), (8<<20)/len("rclone-manager"))
		writeUploadFile(t, f, "big.dump", content)
		src := f.TransportSource("big", "")

		// Lower the cutoff and the chunk size rather than seeding a
		// 256MiB file: the behaviour under test is the split, and those
		// two numbers are the only thing standing between an 8MiB file
		// and the same split a production dump gets by default. Both have
		// to move, because rclone splits by chunk and the default chunk
		// (64Mi) would make any 8MiB file exactly one chunk however many
		// streams were allowed.
		multiThread := func(streams int) context.Context {
			c, ci := fs.AddConfig(ctx)
			ci.MultiThreadCutoff = fs.SizeSuffix(1 << 20)
			ci.MultiThreadChunkSize = fs.SizeSuffix(1 << 20)
			ci.MultiThreadStreams = streams
			return c
		}

		// Control, through rclone directly: at rclone's own stream count
		// this copy really does open more than one connection.
		controlFs := rawSFTPFs(t, ctx, f, "")
		obj, err := controlFs.NewObject(ctx, "big.dump")
		if err != nil {
			shutdownFs(ctx, controlFs)
			t.Fatalf("control NewObject: %v", err)
		}
		dstDir := t.TempDir()
		dstFs, err := fs.NewFs(ctx, dstDir)
		if err != nil {
			shutdownFs(ctx, controlFs)
			t.Fatalf("control destination: %v", err)
		}
		before := f.AcceptedLogins(t)
		if _, err := operations.Copy(multiThread(4), dstFs, nil, "control.dump", obj); err != nil {
			shutdownFs(ctx, controlFs)
			t.Fatalf("control copy: %v", err)
		}
		wide := f.AcceptedLogins(t) - before
		shutdownFs(ctx, controlFs)
		// Same partition as the walk control above, for the same reason:
		// this copy ran at rclone's own stream count and opened three
		// connections, and the assertion at the end of the subtest counts
		// the server's whole table, so a survivor from here would be
		// reported as "a copy above the multi-thread cutoff left a
		// connection open" and send the reader after CopyToLocal.
		requireNoConnections(t, f, "the control copy at --multi-thread-streams 4")
		if wide <= 1 {
			t.Fatalf("a copy at --multi-thread-streams 4 opened %d connection(s); the split this test is about did not happen, so the assertion below would prove nothing", wide)
		}
		t.Logf("control: a copy at --multi-thread-streams 4 opened %d connections", wide)

		before = f.AcceptedLogins(t)
		local := filepath.Join(t.TempDir(), "big.dump.partial")
		res, err := a.CopyToLocal(multiThread(4), src, "big.dump", local)
		if err != nil {
			t.Fatalf("CopyToLocal: %v", err)
		}
		opened := f.AcceptedLogins(t) - before
		if res.BytesTransferred != int64(len(content)) {
			t.Errorf("BytesTransferred = %d, want %d", res.BytesTransferred, len(content))
		}
		if opened != 1 {
			t.Errorf("copying a file above the multi-thread cutoff opened %d connections, want exactly 1; the artifacts this manager fetches are exactly the ones rclone splits by default", opened)
		}
		requireNoConnections(t, f, "a copy above the multi-thread cutoff")
	})

	// rclone's own help for the `connections` option says setting it "is
	// very likely to cause deadlocks" and asks for one more than the sum
	// of --transfers and --checkers, which for a ceiling of 1 is advice to
	// never set 1. That warning is about rclone's sync engine, and this
	// adapter is not that shape, but "probably fine" is not a thing to
	// leave in a comment for the next reader to trust. So it is measured:
	// every operation, at every small ceiling, under a deadline that turns
	// a deadlock into a failure instead of a hang.
	t.Run("EveryOperationCompletesAtEverySmallCeiling", func(t *testing.T) {
		for i := range 12 {
			dir := filepath.Join(f.UploadDir, "ceiling", fmt.Sprintf("run-%02d", i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("seed %s: %v", dir, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "artifact.dump"), []byte("x"), 0o644); err != nil {
				t.Fatalf("seed artifact in %s: %v", dir, err)
			}
		}
		payload := bytes.Repeat([]byte("rclone-manager"), (8<<20)/len("rclone-manager"))
		if err := os.WriteFile(filepath.Join(f.UploadDir, "ceiling", "big.dump"), payload, 0o644); err != nil {
			t.Fatalf("seed the payload: %v", err)
		}

		for _, ceiling := range []int{0, 1, 2, 3} {
			t.Run(fmt.Sprintf("max_connections=%d", ceiling), func(t *testing.T) {
				const budget = 40 * time.Second
				deadlined, cancel := context.WithTimeout(ctx, budget)
				defer cancel()

				src := f.TransportSource("ceiling", "ceiling")
				src.MaxConnections = ceiling

				start := time.Now()
				entries, err := a.List(deadlined, src)
				if err != nil {
					t.Fatalf("List at max_connections=%d: %v (elapsed %v of %v)", ceiling, err, time.Since(start), budget)
				}
				if len(entries) != 13 {
					t.Fatalf("List at max_connections=%d returned %d entries, want 13", ceiling, len(entries))
				}
				if _, err := a.Stat(deadlined, src, "big.dump"); err != nil {
					t.Fatalf("Stat at max_connections=%d: %v (elapsed %v of %v)", ceiling, err, time.Since(start), budget)
				}
				local := filepath.Join(t.TempDir(), "big.dump.partial")
				res, err := a.CopyToLocal(deadlined, src, "big.dump", local)
				if err != nil {
					t.Fatalf("CopyToLocal at max_connections=%d: %v (elapsed %v of %v)", ceiling, err, time.Since(start), budget)
				}
				if res.BytesTransferred != int64(len(payload)) {
					t.Errorf("BytesTransferred at max_connections=%d = %d, want %d", ceiling, res.BytesTransferred, len(payload))
				}
				// This one is expected to fail (the fixture account has
				// no shell), and it is here for the same reason as the
				// rest: what would show a deadlock is it not returning.
				_, _ = a.RemoteHash(deadlined, src, "big.dump", transport.SHA256)
				if deadlined.Err() != nil {
					t.Fatalf("at max_connections=%d the %v deadline expired mid-run, which is what a deadlock looks like from out here", ceiling, budget)
				}
				t.Logf("max_connections=%d: list+stat+copy(%d bytes)+hash in %v", ceiling, res.BytesTransferred, time.Since(start))
				requireNoConnections(t, f, fmt.Sprintf("a full round of operations at max_connections=%d", ceiling))
			})
		}
	})
}

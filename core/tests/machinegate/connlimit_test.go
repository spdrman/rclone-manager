// Package machinegate_test holds the tests that need a machine.
//
// It is where #448 moved the six container-backed tests that used to live
// in core/internal/transport/rclone and core/service, where `go test
// ./internal/...` ran them and where they needed a Docker daemon to say
// anything about a package that is otherwise pure. Everything here reaches
// its machine through core/tests/machines and nothing here execs docker.
//
// #414 and #415's three cancellation files came here for the same reason,
// one step later: they had already left the unit tier for
// tests/sftpintegration, which stopped being the right home when the
// fixture they reached their machine through was folded into the harness.
// They arrive as a set because adaptercancellation_test.go drives the slow
// link that slowlink_test.go defines, and because tests/sftpintegration has
// a measured budget these three would have doubled.
package machinegate_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/rclone/rclone/backend/sftp"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/walk"

	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// theCap is the number of simultaneous connections the source machine
// allows one address, and it is small on purpose: two is below anything
// rclone's own defaults will settle for on a tree of any width, so the
// difference between "the manager respects the cap" and "the manager gets
// away with it" is visible rather than theoretical.
const theCap = 2

// treeWidth is how many directories the control walk has to visit. It has
// to be wide enough that rclone's own fan-out really does exceed theCap;
// the control below measures that rather than assuming it.
const treeWidth = 24

// drainBudget is how long the control Fs gets to hand its connections back
// after it is shut down. It is far below the 60s idle_timeout the config
// above sets, so a pool that only closes through rclone's own drainer
// fails here rather than passing by self-healing.
const drainBudget = 10 * time.Second

// seededObjects is what a full walk of the seeded tree finds: one artifact
// per directory, plus one scratch file that exists only so the manager's
// DeleteRemote has something of its own to remove. Without the scratch
// file the delete in part 3 would change the count the final walk in part
// 4 checks, and the last control would fail for a reason that has nothing
// to do with connection caps.
const seededObjects = treeWidth + 1

// seedTree writes a directory tree under the source machine's upload
// directory. It is the shape this manager actually meets, a run id per
// directory with one artifact in it, not a synthetic fan-out.
func seedTree(t *testing.T, root string) {
	t.Helper()
	for i := range treeWidth {
		dir := filepath.Join(root, "tree", fmt.Sprintf("run-%02d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "artifact.dump"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed artifact in %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "tree", scratchArtifact), []byte("scratch"), 0o644); err != nil {
		t.Fatalf("seed the scratch artifact: %v", err)
	}
}

// scratchArtifact is the one file the manager is allowed to delete.
const scratchArtifact = "scratch.dump"

// shutdownAndDrain closes the control Fs's connection pool and waits for
// the server to agree that it has.
//
// It is not tidiness. The control walk holds up to --checkers connections,
// and against a source capped at two those ARE the cap: leaving the pool
// open makes the manager's own turn fail for want of a connection the
// control is still sitting on, which reads exactly like the manager
// violating the cap. That happened on the first run of this test, and this
// function is the fix.
func shutdownAndDrain(t *testing.T, src *machines.Source, f fs.Fs) {
	t.Helper()
	shutdownFs(context.Background(), f)
	deadline := time.Now().Add(drainBudget)
	for {
		open := src.EstablishedConnections(t)
		if open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the control Fs still had %d connection(s) open on the source %s after it was shut down. Against a cap of %d those connections ARE the cap, so anything measured after this would be measuring the control rather than the manager.", open, drainBudget, theCap)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// wideWalk is the identical workload the control runs capped and uncapped:
// a recursive listing at rclone's own --checkers, which on sftp means one
// connection per checker.
//
// LowLevelRetries is turned down to one for both runs. Left at rclone's
// default of ten, the capped run spends 93 seconds backing off before it
// admits it cannot connect, which is 93 seconds of gate wall clock spent
// re-proving something the first refusal already said. Turning it down
// changes how long the refusal takes, not whether it happens, and it is
// turned down on BOTH sides so the only difference between them is the
// rule on the machine.
func wideWalk(t *testing.T, ctx context.Context, src *machines.Source) (found int, logins int, err error) {
	t.Helper()
	// The config has to be on the context the Fs is BUILT with, not just
	// the one the walk is run with: rclone's sftp backend reads its retry
	// budget when it constructs its pacer, so a LowLevelRetries set only
	// on the walk's context is read by nothing and the capped run spends
	// ninety seconds backing off anyway.
	wide, ci := fs.AddConfig(ctx)
	ci.Checkers = 8
	ci.LowLevelRetries = 1

	f := rawSFTPFs(t, wide, src, "tree")
	before := src.AcceptedLogins(t)
	objs, _, err := walk.GetAll(wide, f, "", true, -1)
	found, logins = len(objs), src.AcceptedLogins(t)-before
	shutdownAndDrain(t, src, f)
	return found, logins, err
}

// TestTheSourceCapsConnectionsAndTheManagerStaysUnderIt is #264's rule,
// run for real on the two-container topology, and it is the caller
// machines.LimitConnections did not have (#463).
//
// The helper existed, worked and was called by nothing, so the shape it
// proves had never actually run in this gate. Giving it a caller is not
// enough on its own: a connection-cap assertion that passes because
// nothing ever opened enough connections is the exact defect, so the cap
// is watched changing the verdict of an identical workload before anything
// is claimed about the manager.
//
// Three parts, in this order, because each one is the premise of the next:
//
//  1. Uncapped, rclone at its own defaults walks the tree and opens more
//     than theCap connections doing it. That is the workload's appetite,
//     measured.
//  2. Capped at theCap, the identical walk FAILS. That is the cap biting,
//     watched rather than assumed, and it is what makes part 3 a claim
//     about the manager instead of a claim about a tree too small to fan
//     out.
//  3. Capped at theCap, every operation this manager performs succeeds,
//     because it opens one connection at a time (#355). That is the rule
//     #264 is about: a source that allows two simultaneous connections is
//     an ordinary hardened VPS, and the manager has to work against it.
func TestTheSourceCapsConnectionsAndTheManagerStaysUnderIt(t *testing.T) {
	m := machines.Start(t)
	src := m.Source(t)
	seedTree(t, src.UploadDir)
	ctx := src.Context()

	// 1. What the workload wants when nothing is stopping it.
	found, appetite, err := wideWalk(t, ctx, src)
	if err != nil {
		t.Fatalf("the control walk failed against an UNCAPPED source (%v), so nothing below can tell a cap from a broken machine", err)
	}
	if found != seededObjects {
		t.Fatalf("the control walk found %d objects, want %d; it did not walk the tree this test seeded", found, seededObjects)
	}
	if appetite <= theCap {
		t.Fatalf("a walk at --checkers 8 over %d directories opened %d connection(s), which is inside a cap of %d. The workload does not fan out past the cap on this machine, so capping it would change nothing and every assertion below would pass on any behaviour at all.", treeWidth, appetite, theCap)
	}
	t.Logf("uncapped: the control walk found %d objects and opened %d connections", found, appetite)

	// 2. The same walk, with the production rule installed. LimitConnections
	// proves the rule bites from a throwaway container before returning;
	// this proves it bites the workload the manager actually runs.
	src.LimitConnections(t, theCap)

	cappedFound, cappedLogins, cappedErr := wideWalk(t, ctx, src)
	if cappedErr == nil {
		t.Fatalf("with the source capped at %d simultaneous connections, a walk that wanted %d of them SUCCEEDED (%d objects, %d logins). Either the cap is not being enforced or this walk is no longer the workload the control measured, and either way the manager's own result below would prove nothing.", theCap, appetite, cappedFound, cappedLogins)
	}
	if !strings.Contains(cappedErr.Error(), "connect") && !strings.Contains(cappedErr.Error(), "connection") {
		t.Fatalf("the capped walk failed with %v, which does not read as a connection being refused. The cap is supposed to be what stopped it, and a failure for some other reason would make this a test of something else.", cappedErr)
	}
	t.Logf("capped at %d: the same walk failed with %v after %d logins", theCap, cappedErr, cappedLogins)

	// 3. And now the manager, against the same capped machine. This is the
	// assertion the issue asks for: every operation completes, because none
	// of them ever needs a second simultaneous connection.
	a := rclone.New()
	source := src.TransportSource("capped", "tree")
	source.MaxConnections = theCap

	before := src.AcceptedLogins(t)
	entries, err := a.List(ctx, source)
	if err != nil {
		t.Fatalf("List against a source capped at %d simultaneous connections: %v. That cap is an ordinary hardened VPS, and #264 is the requirement that this manager works against one.", theCap, err)
	}
	if len(entries) != seededObjects {
		t.Fatalf("List against the capped source returned %d entries, want %d", len(entries), seededObjects)
	}
	if opened := src.AcceptedLogins(t) - before; opened > theCap {
		t.Errorf("discovering the tree opened %d connections against a cap of %d; it completed, but only because they did not overlap, and an operator cannot be asked to bet on that", opened, theCap)
	}

	if _, err := a.Stat(ctx, source, "run-00/artifact.dump"); err != nil {
		t.Fatalf("Stat against the capped source: %v", err)
	}
	local := filepath.Join(t.TempDir(), "artifact.dump.partial")
	if _, err := a.CopyToLocal(ctx, source, "run-00/artifact.dump", local); err != nil {
		t.Fatalf("CopyToLocal against the capped source: %v", err)
	}
	if err := a.DeleteRemote(ctx, source, scratchArtifact); err != nil {
		t.Fatalf("DeleteRemote against the capped source: %v", err)
	}

	// 4. The last direction, and the one that makes part 2 evidence rather
	// than a coincidence: take the rule away and the identical walk works
	// again. Without this, "the walk failed while the cap was on" would be
	// satisfied by a machine that had simply stopped answering.
	src.RemoveConnectionLimit(t)
	againFound, againLogins, againErr := wideWalk(t, ctx, src)
	if againErr != nil {
		t.Fatalf("with the cap removed the control walk STILL failed (%v), so whatever stopped it while the cap was on was not the cap, and part 2 above proved nothing", againErr)
	}
	// One fewer than the first walk, because part 3's DeleteRemote really
	// did remove the scratch artifact against the capped source. That is
	// the difference being asserted, not tolerated.
	if againFound != seededObjects-1 {
		t.Fatalf("with the cap removed the control walk found %d objects, want %d (the %d seeded, less the one the manager deleted while the source was capped)", againFound, seededObjects-1, seededObjects)
	}
	t.Logf("uncapped again: the same walk found %d objects and opened %d connections", againFound, againLogins)
}

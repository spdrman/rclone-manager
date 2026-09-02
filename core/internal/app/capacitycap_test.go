package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the end-to-end proof for issue #286's actual requirement:
// "rclone-manager must prevent itself from using more space than the cap."
// Not a gauge, not a warning, a refusal, taken on the real pipeline path
// with the real journal and the real filesystem underneath it.
//
// The tests below are a matched set and none of them is worth anything
// alone. A guard that refused every transfer would pass the refusal test
// perfectly; what makes the refusal mean something is that the identical
// artifact, the identical journal, the identical disk and the identical
// code admit it the moment the cap is large enough, and admit it again
// when there is no cap at all.
//
// The numbers are in bytes rather than gigabytes on purpose. A cap of 1,005
// bytes against 1,000 bytes of recorded usage exercises exactly the same
// arithmetic as 100 GB against 90 GB, and it lets the positive control run
// a real transfer through a real verify step instead of asking a fake
// transport to claim a size it does not have.

// cappedService builds a Service whose FR-21 thresholds come from a real
// config.Capacity block, exactly as a running deployment's do (app.New
// translates one into the other), rather than from a hand-assigned
// Thresholds. That matters: a test that set svc.Capacity directly would
// still pass if the wiring from configuration to guard were missing, which
// is the state this repository was actually in until this change.
func cappedService(t *testing.T, journal Journal, tr transport.Transport, capBytes int64) *Service {
	t.Helper()
	return New(&config.Config{Capacity: config.Capacity{CapBytes: capBytes}}, journal, tr, nil)
}

// spendCap puts one COMMITTED artifact of the given size into the journal,
// so LocalBytesInUse reports a real, catalog-derived consumption for the
// cap to be weighed against.
//
// It walks the journal directly rather than running a cycle: the point is
// to establish "this manager already occupies N bytes", and how those bytes
// got there is not what any test in this file is about. The artifact
// belongs to a different backup set from the one under test, because the
// cap is manager-wide and space spent by one set is space the next set
// cannot have.
func spendCap(t *testing.T, journal *state.Journal, size int64) {
	t.Helper()
	ctx := context.Background()

	set, err := model.NewBackupSetID("production", "already-spent")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "earlier.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := journal.Discover(ctx, artifact, "spend:discover", "/backups/earlier.dump",
		state.RemoteIdentity{Size: &size}, epoch); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	chain := []lifecycle.State{
		lifecycle.Transferring, lifecycle.Transferred, lifecycle.Verifying,
		lifecycle.Verified, lifecycle.Committing, lifecycle.Committed,
	}
	from := string(lifecycle.Discovered)
	for _, to := range chain {
		if _, err := journal.RecordTransition(ctx, state.Transition{
			Artifact:   artifact,
			Key:        "spend:" + string(to),
			From:       from,
			To:         string(to),
			OccurredAt: epoch,
		}); err != nil {
			t.Fatalf("RecordTransition(%s -> %s): %v", from, to, err)
		}
		from = string(to)
	}
}

// capScenario is the shared fixture for the matched set below: one 13-byte
// artifact waiting to be transferred, and 1,000 bytes already spent.
// Everything is identical between the three tests except the cap.
type capScenario struct {
	journal *state.Journal
	tr      *fakeTransport
	bs      config.BackupSet
	source  transport.Source
	rec     state.Record
	local   string
}

const (
	capScenarioPayload   = "payload bytes" // 13 bytes
	capScenarioSpentSize = int64(1000)
)

func newCapScenario(t *testing.T) capScenario {
	t.Helper()
	local := t.TempDir()
	bs := testBackupSet(t, local)
	source := transport.Source{ID: "cap-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", capScenarioPayload, epoch.Unix())

	journal := openJournal(t)
	spendCap(t, journal, capScenarioSpentSize)

	return capScenario{
		journal: journal,
		tr:      tr,
		bs:      bs,
		source:  source,
		rec:     discoverOneRecord(t, context.Background(), journal, tr, source, bs),
		local:   local,
	}
}

// TestACappedManagerRefusesATransferThatWouldExceedTheCap is the decisive
// one. 1,000 of the 1,005-byte cap is already spent according to the
// catalog, the incoming artifact is 13 bytes, and the disk underneath is an
// ordinary developer or CI temp filesystem with gigabytes to spare. Nothing
// about free space explains this refusal.
func TestACappedManagerRefusesATransferThatWouldExceedTheCap(t *testing.T) {
	s := newCapScenario(t)
	ctx := context.Background()

	svc := cappedService(t, s.journal, s.tr, capScenarioSpentSize+5)
	svc.processArtifact(ctx, s.source, s.bs, s.rec)

	final, err := s.journal.Get(ctx, s.rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Discovered) {
		t.Errorf("journal state = %q, want %q: a cap refusal leaves the artifact exactly where it was, for a later cycle once space is freed", final.State, lifecycle.Discovered)
	}
	if got := s.tr.copyToLocalCalls(); got != 0 {
		t.Errorf("CopyToLocal was called %d time(s), want 0: a transfer known to exceed the cap must never begin", got)
	}
	if _, err := os.Stat(filepath.Join(s.local, "backup.dump")); !os.IsNotExist(err) {
		t.Errorf("a local file exists at the destination after a cap refusal (stat error = %v)", err)
	}
	if entries, err := os.ReadDir(s.local); err != nil {
		t.Fatalf("ReadDir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("the destination holds %d entries after a cap refusal, want none (not even a .partial)", len(entries))
	}
}

// TestTheSameTransferSucceedsUnderALargerCap is the positive control, and
// it is deliberately identical to the test above in every input but one.
// Same artifact, same 1,000 bytes of recorded usage, same disk, same code
// path; only the cap changes.
func TestTheSameTransferSucceedsUnderALargerCap(t *testing.T) {
	s := newCapScenario(t)
	ctx := context.Background()

	svc := cappedService(t, s.journal, s.tr, capScenarioSpentSize+100_000)
	svc.processArtifact(ctx, s.source, s.bs, s.rec)

	final, err := s.journal.Get(ctx, s.rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("journal state = %q, want %q: 13 bytes fit in the 100,000 bytes left of this cap, so nothing here should refuse it", final.State, lifecycle.Complete)
	}
	if got := s.tr.copyToLocalCalls(); got != 1 {
		t.Errorf("CopyToLocal was called %d time(s), want 1", got)
	}
	if _, err := os.Stat(filepath.Join(s.local, "backup.dump")); err != nil {
		t.Errorf("local final file: %v (the transfer was supposed to be admitted)", err)
	}
}

// TestAnUncappedManagerAdmitsTheSameTransfer is the second control, and it
// isolates a different thing from the one above: that the refusal comes
// from the CAP rather than from recorded usage merely existing. Same 1,000
// bytes spent, same 13-byte artifact, and no cap at all, which is this
// product's default.
func TestAnUncappedManagerAdmitsTheSameTransfer(t *testing.T) {
	s := newCapScenario(t)
	ctx := context.Background()

	svc := cappedService(t, s.journal, s.tr, 0)
	svc.processArtifact(ctx, s.source, s.bs, s.rec)

	final, err := s.journal.Get(ctx, s.rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Errorf("journal state = %q, want %q: with no cap the only question is whether the disk has room, and it does", final.State, lifecycle.Complete)
	}
}

// TestTheCapComesFromTheConfigurationRatherThanADefault pins the wiring
// itself. Before issue #286 nothing outside a test ever assigned
// Service.Capacity, so a config file could have carried any cap at all and
// the guard would never have seen it.
func TestTheCapComesFromTheConfigurationRatherThanADefault(t *testing.T) {
	const gb = int64(1) << 30
	svc := New(&config.Config{Capacity: config.Capacity{
		CapBytes:          100 * gb,
		WarningFreeBytes:  20 * gb,
		CriticalFreeBytes: 10 * gb,
		SafetyMarginBytes: 1 * gb,
	}}, openJournal(t), nil, nil)

	if svc.Capacity.CapBytes != uint64(100*gb) {
		t.Errorf("Capacity.CapBytes = %d, want %d: New must read the configured cap", svc.Capacity.CapBytes, 100*gb)
	}
	if svc.Capacity.WarningFreeBytes != uint64(20*gb) || svc.Capacity.CriticalFreeBytes != uint64(10*gb) {
		t.Errorf("thresholds = %d / %d, want %d / %d", svc.Capacity.WarningFreeBytes, svc.Capacity.CriticalFreeBytes, 20*gb, 10*gb)
	}
	if svc.Capacity.SafetyMarginBytes != uint64(1*gb) {
		t.Errorf("Capacity.SafetyMarginBytes = %d, want %d", svc.Capacity.SafetyMarginBytes, 1*gb)
	}
}

// TestLocalUsageCountsWhatTheCatalogRecords proves the number the cap is
// enforced from is the catalog's, taken through this package's own seam
// rather than from a walk of the backup root.
func TestLocalUsageCountsWhatTheCatalogRecords(t *testing.T) {
	journal := openJournal(t)
	svc := cappedService(t, journal, nil, 1<<30)

	before, err := svc.LocalUsage(context.Background())
	if err != nil {
		t.Fatalf("LocalUsage: %v", err)
	}
	if !before.Known || before.Bytes != 0 {
		t.Fatalf("LocalUsage on an empty journal = %+v, want a known zero", before)
	}

	spendCap(t, journal, 7777)

	after, err := svc.LocalUsage(context.Background())
	if err != nil {
		t.Fatalf("LocalUsage: %v", err)
	}
	if after.Bytes != 7777 {
		t.Errorf("LocalUsage = %d, want 7777", after.Bytes)
	}
}

// This file is the honesty check on restores: not that internal/archive
// refuses what it should, which its own suite covers, but that the thing
// this product actually ships can reach a provider at all.
//
// The distinction is worth the file. Every guarantee in the restore path
// is built on an interface, and if the only implementation of it in this
// repository were a test double, an operator would get "this deployment
// has no way to reach a storage medium" no matter what they configured,
// with every test still green. So the first case asserts the binding
// itself, and the second asserts the honest answer when the binding is
// genuinely absent.
//
// The rest is what a restore is at this boundary: a durable row that
// records what was asked, a status re-derived from the provider rather
// than trusted from the row, and a refusal that names the thing the
// caller has to change. Re-derivation is the one that cannot be faked,
// because it is what makes the answer survive a restart of the process
// that submitted it.
package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// TestTheShippedTransportCanActuallyRestore is the honesty test for this
// whole feature, and it is the one that would have failed before #241's
// adapter existed.
//
// internal/archive defines Store and builds every refusal and every
// durability guarantee on top of it. None of that is worth anything if the
// only thing in the repository that satisfies Store is a test double,
// because then an operator asking for a restore gets "this deployment has
// no way to reach a storage medium" no matter what they configure.
//
// So this asserts the binding that makes the feature reachable: the
// adapter this product actually ships with satisfies Store, and a
// BackupService constructed the ordinary way therefore HAS a restorer.
func TestTheShippedTransportCanActuallyRestore(t *testing.T) {
	var _ archive.Store = (*rclone.Adapter)(nil)

	b := New(testConfig(), openTestJournal(t), rclone.New(), nil)
	t.Cleanup(func() { _ = b.Close() })

	if b.restorer() == nil {
		t.Fatal("a service built with the transport this product ships cannot restore anything, so every restore an operator asks for is refused before it starts")
	}
}

// TestADeploymentThatCannotReachAMediumSaysSoRatherThanFailing is the
// other half. A service wired with a transport that is only a Transport
// has no medium boundary, and the honest answer is a named refusal rather
// than a nil dereference or a generic internal error.
func TestADeploymentThatCannotReachAMediumSaysSoRatherThanFailing(t *testing.T) {
	b := New(testConfig(), openTestJournal(t), &restoreOnlyTransport{}, nil)
	t.Cleanup(func() { _ = b.Close() })

	if b.restorer() != nil {
		t.Fatal("a transport with no medium boundary reported that it could restore")
	}

	_, err := b.SubmitRestorePlacement(context.Background(), RestorePlacementRequest{
		IdempotencyKey: "idem-1",
		ConfigRevision: b.ConfigRevision(),
		ArtifactID:     "production/postgres/dump.zst",
		Medium:         "cold-store",
		WindowDays:     3,
		Acknowledged:   true,
	})
	if !errors.Is(err, ErrRestoreUnavailable) {
		t.Fatalf("SubmitRestorePlacement = %v, want ErrRestoreUnavailable", err)
	}
}

// TestARestoreSubmittedThroughTheServiceIsDurableAndIsNotAProgressBar
// drives the whole path an operator's request takes: a configured medium,
// a real journal, a real placement row, the real Restorer, and a store
// that records what it was asked.
//
// It asserts three things that are separately easy to get wrong:
// the durable row exists with the restore action on it, the provider was
// asked for exactly one restore with the window that was requested, and
// the operation that comes back out of GetOperation carries no progress
// reading (FR-34: S3 reports running or finished and nothing else).
func TestARestoreSubmittedThroughTheServiceIsDurableAndIsNotAProgressBar(t *testing.T) {
	store := &recordingRestoreStore{}
	b, artifactID := serviceWithArchivedCopy(t, store)

	sub, err := b.SubmitRestorePlacement(context.Background(), RestorePlacementRequest{
		IdempotencyKey: "idem-restore-1",
		Actor:          "alice",
		ConfigRevision: b.ConfigRevision(),
		ArtifactID:     artifactID,
		Medium:         "cold-store",
		WindowDays:     3,
		Acknowledged:   true,
	})
	if err != nil {
		t.Fatalf("SubmitRestorePlacement: %v", err)
	}
	if !sub.Created {
		t.Error("the submission reported that nothing new was started")
	}
	if sub.Operation.Action != ActionRestorePlacement {
		t.Errorf("action = %q, want %q", sub.Operation.Action, ActionRestorePlacement)
	}
	if sub.WindowDays != 3 {
		t.Errorf("window = %d days, want 3", sub.WindowDays)
	}
	if sub.Wait == "" {
		t.Error("nothing was said about how long a DEEP_ARCHIVE restore takes")
	}
	if sub.Billing == "" {
		t.Error("nothing was said about this being billed")
	}
	for _, forbidden := range []string{"%", "ETA", "estimated"} {
		if strings.Contains(sub.Wait, forbidden) || strings.Contains(sub.Billing, forbidden) {
			t.Errorf("a restore surface said %q, which reads as a prediction this product cannot make", forbidden)
		}
	}

	if got := store.windows(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("the provider was asked for restores %v, want exactly one of 3 days", got)
	}

	// The row is durable: it is readable back out of the journal by a
	// caller that never saw the submission.
	op, err := b.GetOperation(context.Background(), sub.Operation.ID)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Action != ActionRestorePlacement {
		t.Errorf("the persisted row's action = %q, want %q", op.Action, ActionRestorePlacement)
	}
	if op.Progress != nil {
		t.Errorf("a restore came back carrying a progress reading (%+v), and there is no such thing", op.Progress)
	}
}

// TestARestoreStatusIsReDerivedFromTheProviderNotFromTheRow is the
// restart-safety requirement, driven through the boundary an operator's
// client actually polls.
//
// The row is left where a restart leaves it (running, because nothing in
// this process finishes it). The provider is then made to say the copy is
// restored and readable until a date. A read has to come back completed,
// with that date, WITHOUT anything having executed the operation here,
// because nothing here ever will.
func TestARestoreStatusIsReDerivedFromTheProviderNotFromTheRow(t *testing.T) {
	store := &recordingRestoreStore{}
	b, artifactID := serviceWithArchivedCopy(t, store)

	sub, err := b.SubmitRestorePlacement(context.Background(), RestorePlacementRequest{
		IdempotencyKey: "idem-restore-2",
		Actor:          "alice",
		ConfigRevision: b.ConfigRevision(),
		ArtifactID:     artifactID,
		Medium:         "cold-store",
		WindowDays:     3,
		Acknowledged:   true,
	})
	if err != nil {
		t.Fatalf("SubmitRestorePlacement: %v", err)
	}

	running, err := b.GetOperation(context.Background(), sub.Operation.ID)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if running.Status == state.OperationCompleted {
		t.Fatal("the restore reported itself finished while the provider still said it was running")
	}
	if running.Restore == nil {
		t.Fatal("a restore operation came back with no restore block at all")
	}
	if running.Restore.Access != string(archive.Restoring) {
		t.Errorf("access = %q, want %q", running.Restore.Access, archive.Restoring)
	}
	if running.Restore.Medium != "cold-store" || running.Restore.Class != config.StorageClassDeepArchive {
		t.Errorf("the restore block names %q/%q, want cold-store/%s", running.Restore.Medium, running.Restore.Class, config.StorageClassDeepArchive)
	}

	// The provider finishes it. Nothing in this process is told.
	expiry := time.Now().UTC().Add(72 * time.Hour)
	store.finish(expiry)

	done, err := b.GetOperation(context.Background(), sub.Operation.ID)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if done.Status != state.OperationCompleted {
		t.Fatalf("status = %q, want %q; nothing in this process executes a restore, so a read is the only thing that can ever finish the row",
			done.Status, state.OperationCompleted)
	}
	if done.Restore == nil {
		t.Fatal("a finished restore came back with no restore block at all")
	}
	if done.Restore.Access != string(archive.Immediate) {
		t.Errorf("access = %q, want %q", done.Restore.Access, archive.Immediate)
	}
	if done.Restore.RestoredUntil == nil {
		t.Fatal("the provider reported an expiry date and the surface dropped it")
	}
	if !done.Restore.RestoredUntil.Equal(expiry) {
		t.Errorf("restored until %v, want %v", done.Restore.RestoredUntil, expiry)
	}
	// The class's own figures survive a read that never saw the
	// submission, because they come from the class table rather than from
	// the request. That is what makes a restarted process able to answer.
	if done.Restore.Wait == "" {
		t.Error("a restore read back after the fact says nothing about how long that class takes")
	}
	if done.Restore.Billing == "" {
		t.Error("a restore read back after the fact says nothing about being billed")
	}
	if done.Progress != nil {
		t.Error("a finished restore carried a progress reading")
	}
}

// TestARestoreRefusalNamesTheThingTheCallerHasToChange covers the four
// request-shaped refusals as a caller outside core/ sees them.
//
// They all arrive as ErrRestoreRefused rather than as four sentinels
// because they are the same thing from a client's point of view: nothing
// was started, nothing was billed, and the fix is a different request. The
// assertion that matters beside that is the negative one, and it is the
// point of the whole guard: no refusal reaches the provider.
func TestARestoreRefusalNamesTheThingTheCallerHasToChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*RestorePlacementRequest)
		wantErr error
		says    string
	}{
		{
			name:    "nobody acknowledged the cost",
			mutate:  func(r *RestorePlacementRequest) { r.Acknowledged = false },
			wantErr: ErrRestoreRefused,
			says:    "asked for explicitly",
		},
		{
			name:    "the window is a month and a half",
			mutate:  func(r *RestorePlacementRequest) { r.WindowDays = 45 },
			wantErr: ErrRestoreRefused,
			says:    "out of range",
		},
		{
			name:    "there is no such backup",
			mutate:  func(r *RestorePlacementRequest) { r.ArtifactID = "production/postgres/no-such.zst" },
			wantErr: ErrArtifactNotFound,
		},
		{
			name:    "the backup is not on that medium",
			mutate:  func(r *RestorePlacementRequest) { r.Medium = "warm-store" },
			wantErr: ErrCopyNotFound,
		},
		{
			name:    "the caller is acting on a configuration that has moved",
			mutate:  func(r *RestorePlacementRequest) { r.ConfigRevision = "rev-from-yesterday" },
			wantErr: ErrConfigRevisionStale,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingRestoreStore{}
			b, artifactID := serviceWithArchivedCopy(t, store)

			req := RestorePlacementRequest{
				IdempotencyKey: "idem-refused",
				Actor:          "alice",
				ConfigRevision: b.ConfigRevision(),
				ArtifactID:     artifactID,
				Medium:         "cold-store",
				WindowDays:     3,
				Acknowledged:   true,
			}
			tc.mutate(&req)

			_, err := b.SubmitRestorePlacement(context.Background(), req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("SubmitRestorePlacement = %v, want %v", err, tc.wantErr)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal reads %q, and does not say %q", err, tc.says)
			}
			if got := store.windows(); len(got) != 0 {
				t.Fatalf("a refused request still asked the provider for %v; every refusal has to happen before anything is billed", got)
			}
			if ops := listAllOperations(t, b); len(ops) != 0 {
				t.Fatalf("a refused request left %d operation rows behind", len(ops))
			}
		})
	}
}

func listAllOperations(t *testing.T, b *BackupService) []Operation {
	t.Helper()
	ops, err := b.ListOperations(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	return ops
}

// serviceWithArchivedCopy builds a BackupService whose configuration
// declares a DEEP_ARCHIVE medium and whose journal holds one artifact with
// a real, ACTIVE, content-verified placement on it.
//
// The placement is written through RecordTransition rather than inserted,
// so it is the same row the product itself would have produced, and the
// service reads it through the same accessor every other caller uses.
func serviceWithArchivedCopy(t *testing.T, store *recordingRestoreStore) (*BackupService, string) {
	t.Helper()

	cfg := testConfig()
	// Two mediums, and the second one is not decoration. Without it, a
	// request naming a medium the artifact is not on would be refused by
	// the CONFIGURATION lookup rather than by the placement lookup, and
	// the test asserting that refusal would pass with the placement check
	// deleted. This was watched happening: the mutation that replaced
	// "no copy on that medium" with "take the first copy you find" stayed
	// green until warm-store was declared.
	cfg.StorageMediums = []config.StorageMedium{
		{
			ID:           "cold-store",
			Type:         config.StorageMediumTypeS3,
			Region:       "us-east-1",
			Bucket:       "backups",
			Prefix:       "prefix",
			StorageClass: config.StorageClassDeepArchive,
			Credentials:  config.MediumCredentials{File: "/var/lib/backup-manager/s3.creds"},
		},
		{
			ID:           "warm-store",
			Type:         config.StorageMediumTypeS3,
			Region:       "us-east-1",
			Bucket:       "warm-backups",
			StorageClass: config.StorageClassStandard,
			Credentials:  config.MediumCredentials{File: "/var/lib/backup-manager/s3.creds"},
		},
	}

	journal := openTestJournal(t)
	b := New(cfg, journal, &restoreCapableTransport{store: store}, nil)
	t.Cleanup(func() { _ = b.Close() })

	set, err := model.NewBackupSetID("production", "postgres")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "dump.zst")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	ctx := context.Background()
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "seed-discover",
		To:         string(lifecycle.Discovered),
		OccurredAt: time.Now().UTC().Add(-48 * time.Hour),
		RemotePath: "/backups/pg/dump.zst",
	}); err != nil {
		t.Fatalf("seeding the artifact: %v", err)
	}

	size := int64(4096)
	verifiedAt := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "seed-placement",
		From:       string(lifecycle.Discovered),
		To:         string(lifecycle.Transferring),
		OccurredAt: time.Now().UTC().Add(-24 * time.Hour),
		Placement: &state.PlacementUpdate{
			Medium:            "cold-store",
			Location:          "prefix/production/postgres/dump.zst",
			Size:              &size,
			Hash:              strings.Repeat("a", 64),
			HashAlg:           string(transport.SHA256),
			VerificationClass: state.VerificationContent,
			VerifiedAt:        &verifiedAt,
			Status:            state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("seeding the archived copy: %v", err)
	}

	return b, artifact.String()
}

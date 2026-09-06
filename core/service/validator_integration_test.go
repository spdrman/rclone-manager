package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is RED item 3 of WP3.2's TDD plan (docs/EPIC-B-multi-nas.md
// §71 Work Package 3.2): "a required validator's failure (or timeout)
// still prevents remote deletion end to end ... not just at
// internal/lifecycle.Verify directly." Both tests below resolve their
// backup set's config.Validation.Command through this package's own
// resolveValidator (validator.go) and drive the result through this
// package's real public entry point (SubmitRunCycle ->
// internal/app.Service.RunCycle -> the full FR-11 pipeline), never a
// hand-built lifecycle.VerifyParams.
//
// What they do NOT prove, and never did, is that anything an API caller
// can ask for produces that Command: they call resolveValidator
// themselves, which is a fixture, not the wiring. Issue #162 is what
// added the wiring, and validator_wiring_test.go is where the same
// refusal is proven through it -- a validator selected by id in
// config.yaml (or in a CreateBackupSetRequest), resolved by Open at load
// time. These two stay because a fake transport makes them fast and
// lets them assert DeleteRemote call counts directly, which the
// local-transport tests next door can only observe as a missing file.

// --- a small, fully-controlled transport.Transport, duplicated from
// internal/app's own test fixture (core/internal/app/helpers_test.go)
// rather than imported: this package is what internal/app's own callers
// reach through, not the other way around, and Go test files cannot share
// unexported helpers across a package boundary regardless -- the same
// convention internal/revalidate and internal/reconcile already follow
// for their own small duplicated test utilities.
type validatorFakeObject struct {
	data    []byte
	modTime int64
}

type validatorFakeTransport struct {
	objects     map[string]*validatorFakeObject
	deleteCalls int
}

func newValidatorFakeTransport() *validatorFakeTransport {
	return &validatorFakeTransport{objects: map[string]*validatorFakeObject{}}
}

func (f *validatorFakeTransport) put(path string, content []byte, modTime int64) {
	f.objects[path] = &validatorFakeObject{data: content, modTime: modTime}
}

func (f *validatorFakeTransport) hash(obj *validatorFakeObject) string {
	sum := sha256.Sum256(obj.data)
	return hex.EncodeToString(sum[:])
}

func (f *validatorFakeTransport) List(_ context.Context, _ transport.Source) ([]transport.RemoteArtifact, error) {
	out := make([]transport.RemoteArtifact, 0, len(f.objects))
	for path, obj := range f.objects {
		out = append(out, transport.RemoteArtifact{
			Path: path, Size: int64(len(obj.data)), ModTime: obj.modTime,
			Hash: f.hash(obj), HashAlg: transport.SHA256, ID: "fake:" + path,
		})
	}
	return out, nil
}

func (f *validatorFakeTransport) Stat(_ context.Context, _ transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	obj, ok := f.objects[remotePath]
	if !ok {
		return transport.RemoteArtifact{}, transport.NewError(transport.NotFound, "stat", errors.New("not found"))
	}
	return transport.RemoteArtifact{
		Path: remotePath, Size: int64(len(obj.data)), ModTime: obj.modTime,
		Hash: f.hash(obj), HashAlg: transport.SHA256, ID: "fake:" + remotePath,
	}, nil
}

func (f *validatorFakeTransport) CopyToLocal(_ context.Context, _ transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	obj, ok := f.objects[remotePath]
	if !ok {
		return transport.TransferResult{}, transport.NewError(transport.NotFound, "copy_to_local", errors.New("not found"))
	}
	if err := os.MkdirAll(filepath.Dir(localPartialPath), 0o755); err != nil {
		return transport.TransferResult{}, err
	}
	if err := os.WriteFile(localPartialPath, obj.data, 0o644); err != nil {
		return transport.TransferResult{}, err
	}
	return transport.TransferResult{BytesTransferred: int64(len(obj.data))}, nil
}

func (f *validatorFakeTransport) RemoteHash(_ context.Context, _ transport.Source, remotePath string, _ transport.HashAlgorithm) (string, error) {
	obj, ok := f.objects[remotePath]
	if !ok {
		return "", transport.NewError(transport.NotFound, "remote_hash", errors.New("not found"))
	}
	return f.hash(obj), nil
}

func (f *validatorFakeTransport) DeleteRemote(_ context.Context, _ transport.Source, remotePath string) error {
	f.deleteCalls++
	delete(f.objects, remotePath)
	return nil
}

var _ transport.Transport = (*validatorFakeTransport)(nil)

var wp32Epoch = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// trailerMarkerBackupSet builds a backup set whose FR-13 application
// validator is the "trailer-marker" registered validator, resolved
// through this package's own resolveValidator: nothing in this test file
// ever builds a config.Command by hand.
func trailerMarkerBackupSet(t *testing.T, localDir string) (config.Source, config.BackupSet) {
	t.Helper()
	cmd, err := resolveValidator(validatorTestDir(t), ValidatorTrailerMarker)
	if err != nil {
		t.Fatalf("resolveValidator: %v", err)
	}
	setID, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	bs := config.BackupSet{
		Name:       "postgres-primary",
		ID:         setID,
		Include:    []string{"*.dump"},
		Completion: config.Completion{Strategy: "rename"},
		LocalPath:  localDir,
		RemotePath: "/backups",
		Validation: config.Validation{Command: &cmd},
	}
	return config.Source{Name: "production", BackupSets: []config.BackupSet{bs}}, bs
}

// TestRunCycle_RegisteredValidatorFailureBlocksRemoteDeletionEndToEnd is
// RED item 3's negative case: an artifact whose content is missing the
// trailer this registered validator requires must be quarantined, and the
// remote source must never be deleted, all the way through
// SubmitRunCycle -- not merely at a direct internal/lifecycle.Verify call.
func TestRunCycle_RegisteredValidatorFailureBlocksRemoteDeletionEndToEnd(t *testing.T) {
	localDir := t.TempDir()
	source, bs := trailerMarkerBackupSet(t, localDir)

	tr := newValidatorFakeTransport()
	// Deliberately missing the trailer marker: the "incomplete/
	// unverifiable" artifact this RED item asks for.
	tr.put("backup.dump", []byte("payload bytes, no trailer"), wp32Epoch.Unix())

	journal := openTestJournal(t)
	svc := New(testConfig(source), journal, tr, nil)

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-validator-fail",
		Actor:          "test",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	final := waitForTerminalStatus(t, svc, op.ID)
	if final.Status != "completed" {
		t.Fatalf("operation status = %q, want completed (the cycle itself succeeds; the artifact's own quarantine is a business outcome, not an operation failure)", final.Status)
	}

	records, err := journal.ListByBackupSet(context.Background(), bs.ID)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("ListByBackupSet returned %d records, want 1", len(records))
	}
	if records[0].State != string(lifecycle.Quarantined) {
		t.Fatalf("artifact state = %q, want %s: a required registered validator's failure must prevent remote deletion end to end", records[0].State, lifecycle.Quarantined)
	}
	if tr.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tr.deleteCalls)
	}
	if _, stillThere := tr.objects["backup.dump"]; !stillThere {
		t.Fatal("the remote object was removed despite the registered validator rejecting the artifact")
	}
}

// TestRunCycle_RegisteredValidatorSuccessAllowsRemoteDeletionEndToEnd is
// the positive complement the issue's own Behavioral Contract asks for
// alongside the refusal case: a genuinely complete, verified backup (this
// time confirmed by the same registered validator) is allowed through to
// remote deletion, exactly like a backup set configured without any
// validator at all.
func TestRunCycle_RegisteredValidatorSuccessAllowsRemoteDeletionEndToEnd(t *testing.T) {
	localDir := t.TempDir()
	source, bs := trailerMarkerBackupSet(t, localDir)

	tr := newValidatorFakeTransport()
	tr.put("backup.dump", []byte("payload bytes\n--RCLONE-MANAGER-BACKUP-COMPLETE--\n"), wp32Epoch.Unix())

	journal := openTestJournal(t)
	svc := New(testConfig(source), journal, tr, nil)

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-validator-pass",
		Actor:          "test",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	final := waitForTerminalStatus(t, svc, op.ID)
	if final.Status != "completed" {
		t.Fatalf("operation status = %q, want completed", final.Status)
	}

	records, err := journal.ListByBackupSet(context.Background(), bs.ID)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("ListByBackupSet returned %d records, want 1", len(records))
	}
	if records[0].State != string(lifecycle.Complete) {
		t.Fatalf("artifact state = %q, want %s: a genuinely complete, verified backup must reach delete-eligible completion through the same registered-validator wiring", records[0].State, lifecycle.Complete)
	}
	if tr.deleteCalls != 1 {
		t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tr.deleteCalls)
	}
	if _, stillThere := tr.objects["backup.dump"]; stillThere {
		t.Fatal("the remote object was never deleted despite the registered validator accepting the artifact")
	}
}

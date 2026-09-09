package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/webhost"
	"github.com/spdrman/rclone-manager/core/service"
)

// PR #581's review, at the seam this binary owns.
//
// #571 made this process announce which deployment it is about to serve
// BEFORE it serves the setup flow, and announcing means creating a lock
// file in the state directory. The first version of that failed the start
// when the directory could not be used, which turned a read-only bind
// mount or a late-mounted volume into a container restart loop on the one
// start where the operator has nothing but a browser.
//
// So the start survives it now, and the refusal moves to the one place it
// still has to hold: setup itself. Writing a first configuration while
// nothing is announced is exactly the defect #571 reported, because a
// `backup-set create` on the same host would find nothing and write one
// too.

// recordingFirstRun is a first-run surface that only counts. The
// embedded interface is nil on purpose: anything this test has not
// thought about panics rather than quietly answering.
type recordingFirstRun struct {
	webhost.FirstRunClient
	created int
}

func (r *recordingFirstRun) CreateInitialConfig(context.Context, service.CreateBackupSetRequest) (service.BackupSet, error) {
	r.created++
	return service.BackupSet{ID: "api/nightly", SourceName: "api", Name: "nightly"}, nil
}

func TestFirstRunGate_RefusesSetupUntilTheDeploymentCanBeAnnounced(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	// A plain file where the state directory has to be: a stand-in for
	// the read-only mount and the wrong-uid volume, and one that refuses
	// for root as well, so this test says the same thing in a container.
	stateDir := filepath.Join(dir, "state")
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dbPath := filepath.Join(stateDir, "state.db")

	serving, err := service.AnnounceServingFirstRun(configPath, dbPath)
	if err != nil {
		t.Fatalf("AnnounceServingFirstRun: %v", err)
	}
	t.Cleanup(func() { _ = serving.Release() })

	inner := &recordingFirstRun{}
	surface := gateFirstRunOnServing(inner, serving)

	_, err = surface.CreateInitialConfig(context.Background(), service.CreateBackupSetRequest{Name: "nightly"})
	if !errors.Is(err, service.ErrNotAnnounced) {
		t.Fatalf("CreateInitialConfig while this deployment cannot be announced = %v, want an error matching ErrNotAnnounced", err)
	}
	if !errors.Is(err, service.ErrStateDirInvalid) {
		t.Errorf("CreateInitialConfig error = %v, want the state directory's own complaint underneath it; that sentence is the whole diagnosis the wizard shows", err)
	}
	if inner.created != 0 {
		t.Fatalf("the first configuration was written %d times although nothing could be announced; a `backup-set create` on this host would write one too, which is #571", inner.created)
	}

	// The operator fixes the volume, in the browser they still have open,
	// without restarting a container they may have no shell for.
	if err := os.Remove(stateDir); err != nil {
		t.Fatalf("removing the file in the way: %v", err)
	}

	if _, err := surface.CreateInitialConfig(context.Background(), service.CreateBackupSetRequest{Name: "nightly"}); err != nil {
		t.Fatalf("CreateInitialConfig once the state directory works = %v, want it to go through", err)
	}
	if inner.created != 1 {
		t.Fatalf("the first configuration was written %d times, want 1: the gate refuses forever rather than only while the volume is broken", inner.created)
	}
	engine, err := service.DetectRunningEngineForJournal(dbPath)
	if err != nil {
		t.Fatalf("DetectRunningEngineForJournal: %v", err)
	}
	if engine == nil {
		t.Fatal("setup was allowed through with nothing announced, so a `backup-set create` on this host would still write a configuration behind this process (issue #571)")
	}
}

package miniofixture_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/tests/miniofixture"
)

// TestFixtureStartsAServerThatWorks is the fixture's own smoke test. The
// integration suite exercises Start on every run, but it does so through
// the adapter, so a failure there could be either end. This checks the
// fixture's own promises directly.
func TestFixtureStartsAServerThatWorks(t *testing.T) {
	f := miniofixture.Start(t)

	if f.Endpoint == "" || f.Bucket == "" || f.AccessKeyID == "" || f.SecretAccessKey == "" {
		t.Fatalf("the fixture came back incomplete: %+v", f)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(f.Endpoint + "/minio/health/live")
	if err != nil {
		t.Fatalf("the endpoint the fixture reported does not answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health endpoint answered %s", resp.Status)
	}

	// The credentials file is a real file this fixture wrote, and the
	// adapter refuses one that anybody but its owner can read. A fixture
	// that wrote it wider would fail the file-source tests for a reason
	// that had nothing to do with the adapter.
	info, err := os.Stat(f.CredentialsFile)
	if err != nil {
		t.Fatalf("stat the credentials file: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the credentials file is mode %04o; the adapter refuses anything group- or world-accessible", mode)
	}
	if dir, derr := os.Stat(filepath.Dir(f.CredentialsFile)); derr != nil {
		t.Errorf("stat the credentials directory: %v", derr)
	} else if mode := dir.Mode().Perm(); mode&0o022 != 0 {
		t.Errorf("the credentials directory is mode %04o; a group- or world-writable one lets any local actor swap the file", mode)
	}
}

// TestFixtureContextIsCancelledWhenTheContainerDies is the #161 property,
// and it is the only reason the watchdog exists. Without it, a container
// that dies mid-test leaves the operation under test retrying against a
// corpse until `go test` kills the whole package, and the victim is
// whichever test happened to be talking to it rather than the one that
// broke.
//
// The container is killed deliberately here, which is why this test asserts
// the cancellation rather than reporting it as a failure.
func TestFixtureContextIsCancelledWhenTheContainerDies(t *testing.T) {
	f := miniofixture.Start(t)

	if err := f.Context().Err(); err != nil {
		t.Fatalf("the fixture's context was already cancelled before anything happened: %v", context.Cause(f.Context()))
	}

	kill := exec.Command("docker", "rm", "-f", f.ContainerID())
	if out, err := kill.CombinedOutput(); err != nil {
		t.Fatalf("killing the container: %v\n%s", err, out)
	}

	// probeInterval is 500ms and two consecutive misses are required, so
	// this should land in about a second. Ten is a wide margin that still
	// fails fast if the watchdog is not running at all.
	select {
	case <-f.Context().Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the fixture's context was not cancelled within 10s of its container being removed; the watchdog is not watching")
	}

	cause := context.Cause(f.Context())
	if cause == nil {
		t.Fatal("the context was cancelled with no cause; a cancellation nobody can explain is not much better than a hang")
	}
	if errors.Is(cause, context.Canceled) {
		t.Errorf("the cause is the bare context.Canceled rather than a statement about the container: %v", cause)
	}
}

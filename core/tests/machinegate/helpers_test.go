package machinegate_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"testing"

	_ "github.com/rclone/rclone/backend/sftp"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"

	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// The raw backend the controls in this package are argued against.
//
// Several cells here claim something about what the adapter does to a
// server, and a claim like that is only meaningful next to what happens when
// the adapter is NOT deciding. This builds an rclone sftp Fs with the
// backend's own defaults and none of the adapter's opinions, so a
// "connection table" or "connection count" assertion has a baseline that
// came from the workload rather than from the code under test.
//
// It deliberately does not reuse the adapter's own Fs builder, which is
// unexported and has to stay that way: a control constructed by the thing it
// is controlling for is not a control.

// rawSFTPFs builds an rclone sftp Fs against a source machine, with the
// options this adapter sets and nothing else: no `connections` ceiling, so
// the backend opens one connection per concurrent lister the way rclone
// would if nobody had thought about connection counts.
//
// It is deliberately NOT the adapter's own Fs, which is a good thing twice
// over. Every control in this package is an argument about what the
// WORKLOAD does when the code under test is not deciding, and an Fs built
// by the code under test would be answering a different question. And it is
// what #448 needed to move these tests out of package rclone at all: fsFor
// is unexported and has to stay that way.
func rawSFTPFs(t *testing.T, ctx context.Context, src *machines.Source, root string) fs.Fs {
	t.Helper()
	info, err := fs.Find("sftp")
	if err != nil {
		t.Fatalf("the sftp backend is not registered in this test binary, so there is nothing to build a control against: %v", err)
	}
	f, err := info.NewFs(ctx, "machinegate-control", path.Join("upload", root), configmap.Simple{
		"host":             src.Host,
		"port":             strconv.Itoa(src.Port),
		"user":             src.User,
		"key_file":         src.KeyFile,
		"known_hosts_file": src.KnownHostsFile,
		"subsystem":        "sftp",
		"chunk_size":       "32Ki",
		"concurrency":      "64",
		"idle_timeout":     "60s",
	})
	if err != nil {
		t.Fatalf("building a control Fs against %s: %v", src.Addr(), err)
	}
	return f
}

// shutdownFs closes an Fs's connection pool. It is package rclone's own
// shutdownFs, restated: the interface it goes through is public, and the
// four lines are not worth exporting a helper for.
func shutdownFs(ctx context.Context, f fs.Fs) {
	if sd, ok := f.(fs.Shutdowner); ok {
		_ = sd.Shutdown(ctx)
	}
}

// writeUploadFile seeds a file on the source machine by writing into the
// host directory bind-mounted onto its chroot.
func writeUploadFile(t *testing.T, f *machines.Source, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.UploadDir, name), content, 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

func alreadyCancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

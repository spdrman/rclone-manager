// One failure message, for the upgrade that breaks on two platforms and no
// others.
//
// TrueNAS and Unraid both carry an existing deployment's mappings forward,
// so an upgrade past #196 can end up bind-mounting the OLD config.yaml file
// at the new configuration DIRECTORY path. Everything downstream then reads
// <that file>/config.yaml, gets ENOTDIR, and the engine crash-loops saying
// "not a directory", which names neither the mount that is wrong nor the
// migration that changed it.
//
// The pair of tests is the whole design. One proves the hint appears where
// it applies, and the other proves it does not appear for a config that is
// merely missing, because a hint attached to every read failure is a hint
// nobody reads and would send an operator chasing a mount that is perfectly
// correct.

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// TestAConfigMountThatIsStillAFileSaysSo covers the upgrade path two
// platforms have and the others do not.
//
// TrueNAS carries an installed application's answered values forward, and
// Unraid keeps whatever mappings are already in the operator's own copy of
// the template. On both, an upgrade past #196 can bind-mount the OLD
// config.yaml file at the new configuration DIRECTORY path. --config then
// resolves to <that file>/config.yaml, the read fails with ENOTDIR, and
// the engine crash-loops on "not a directory", which names neither the
// mount nor the migration. The migration section in
// docs/runtime-contract.md claimed a fail-closed ${VAR:?} guarantee that
// covers the env-file platforms and simply does not exist on those two.
func TestAConfigMountThatIsStillAFileSaysSo(t *testing.T) {
	root := t.TempDir()

	// The shape an upgraded TrueNAS or Unraid deployment actually has:
	// the container's configuration DIRECTORY path is a file.
	mountedAsFile := filepath.Join(root, "config")
	if err := os.WriteFile(mountedAsFile, []byte("poll_interval: 15m\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := config.Load(filepath.Join(mountedAsFile, config.DefaultFileName))
	if err == nil {
		t.Fatal("loading a configuration file inside a path that is itself a file returned no error")
	}
	for _, want := range []string{mountedAsFile, "#196", "docs/runtime-contract.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q never mentions %q, so an operator is told a path is not a directory and nothing about which mount to change", err, want)
		}
	}
}

// TestAnOrdinaryMissingConfigIsNotBlamedOnTheMount is the control the
// assertion above needs. A hint appended to every read failure is a hint
// nobody reads, and it would send an operator whose configuration is
// merely absent chasing a mount that is perfectly correct.
func TestAnOrdinaryMissingConfigIsNotBlamedOnTheMount(t *testing.T) {
	dir := t.TempDir()

	_, err := config.Load(filepath.Join(dir, config.DefaultFileName))
	if err == nil {
		t.Fatal("loading a configuration file that does not exist returned no error")
	}
	if strings.Contains(err.Error(), "#196") {
		t.Errorf("a missing config file inside a real directory was reported as a mount-shape problem: %v", err)
	}

	// And the same for a directory that does not exist at all, which is
	// what a mount that failed entirely looks like.
	if got := config.ExplainConfigMountShape(filepath.Join(dir, "not-there", config.DefaultFileName)); got != "" {
		t.Errorf("a configuration directory that does not exist was explained as %q; nothing is known about its shape", got)
	}

	// The positive half, driven directly: the explanation fires on the
	// one shape it is for, so the two negatives above are about the
	// shape and not about a function that returns "" for everything.
	file := filepath.Join(dir, "config")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := config.ExplainConfigMountShape(filepath.Join(file, config.DefaultFileName)); got == "" {
		t.Error("a configuration directory that is a file was explained as nothing at all")
	}
}

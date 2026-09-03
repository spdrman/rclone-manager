package miniointegration_test

import (
	"os/exec"
	"testing"
)

// dirExistsInContainer asks the container itself, addressed by the exact id
// the fixture created, never by a `docker ps` scan: this machine runs many
// worktrees against one docker daemon.
func dirExistsInContainer(t *testing.T, containerID, path string) bool {
	t.Helper()
	err := exec.Command("docker", "exec", containerID, "test", "-d", path).Run()
	return err == nil
}

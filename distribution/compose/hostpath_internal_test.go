package compose

import (
	"testing"

	"github.com/spdrman/rclone-manager/distribution/packaging"
)

// TestTheTwoProhibitedHostPathEntryPointsGiveIdenticalVerdicts is the
// control that the two copies of this rule are gone.
//
// They were real, and they differed: packaging's trimmed a trailing slash
// and this package's ran filepath.Clean, so //var/run/docker.sock,
// /var/run/./docker.sock and /mnt/../var/run/docker.sock were refused here
// and allowed there. That mattered because packaging's copy exists solely
// to reach the Unraid template, which is XML and which this package cannot
// read at all, so on the one adapter where the weaker matcher was the only
// defence a host path spelled with a redundant slash reached production.
//
// hostPathMatches now delegates, so a divergence would have to be
// reintroduced deliberately. This is what would notice if it were.
func TestTheTwoProhibitedHostPathEntryPointsGiveIdenticalVerdicts(t *testing.T) {
	prohibited := []string{"/var/run/docker.sock", "/run/docker.sock", "/", "/etc", "/usr", "/var", "/boot", "/proc", "/sys", "/root", "/home"}
	hosts := []string{
		"/var/run/docker.sock",
		"//var/run/docker.sock",
		"/var/run/./docker.sock",
		"/var/run/../run/docker.sock",
		"/mnt/../var/run/docker.sock",
		"/var/run/docker.sock/",
		"/etc",
		"/etc/",
		"/etcetera",
		"/",
		"//",
		"",
		"/mnt/tank/backup-manager/state",
		"${STATE_DIR:?set STATE_DIR}",
	}

	agreed := 0
	for _, host := range hosts {
		for _, p := range prohibited {
			mine := hostPathMatches(host, p)
			theirs := packaging.HostPathIsAt(host, p)
			if mine != theirs {
				t.Errorf("hostPathMatches(%q, %q) = %v and packaging.HostPathIsAt says %v; the two rules have diverged again", host, p, mine, theirs)
			}
			agreed++
		}
	}
	if agreed == 0 {
		t.Fatal("no pair was compared, so this control proves nothing")
	}

	// Positive control: the comparison above is only meaningful while the
	// rule says yes to something and no to something else.
	if !hostPathMatches("//var/run/docker.sock", "/var/run/docker.sock") {
		t.Error("the Docker socket spelled with a redundant leading slash is not refused, which is the exact evasion this rule was fixed for")
	}
	if hostPathMatches("/mnt/tank/backup-manager/state", "/var") {
		t.Error("a real storage path is refused, which is how a prohibition gets switched off")
	}
}

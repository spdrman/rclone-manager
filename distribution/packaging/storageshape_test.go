package packaging

import (
	"strings"
	"testing"
)

// TestCanonicalDeclaresOneWriteModePerStorageRole is the source-of-truth
// half of issue #196: every canonical container path resolves to exactly
// one write mode, and the config role is a directory.
func TestCanonicalDeclaresOneWriteModePerStorageRole(t *testing.T) {
	if v := CheckCanonicalWriteModes(MustLoad()); len(v) > 0 {
		t.Errorf("canonical.json does not declare one write mode per storage role:\n%s", format(v))
	}
}

// TestEveryPlatformMountsEveryRoleWithItsDeclaredWriteMode is the adapter
// half. It is separate from TestEveryPlatformMapsEveryStorageRoleTheSameWay
// (which owns "which host path, at which container path") so that a
// failure here says the write MODE drifted rather than that a path did.
func TestEveryPlatformMountsEveryRoleWithItsDeclaredWriteMode(t *testing.T) {
	c := MustLoad()
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			if v := CheckStorageShapes(p.services(t), c); len(v) > 0 {
				t.Errorf("mounts disagree with canonical.json's declared write modes:\n%s", format(v))
			}
		})
	}
}

// TestTheReadOnlyConfigFileMountIsRefused is issue #196's acceptance
// criterion on this side: a positive control at the exact mount shape
// every platform shipped before #196, proving the check fails against it
// and says why.
//
// This is the piece that was missing, and its absence is the whole story
// of the issue. Every fixture in this repository used an ordinary
// writable file, so nothing ever exercised the shape an operator actually
// deploys, and three merged features could be inert inside every packaged
// container with the suite fully green.
func TestTheReadOnlyConfigFileMountIsRefused(t *testing.T) {
	c := MustLoad()

	// The pre-#196 declaration, verbatim in shape:
	//   <host>/config/config.yaml:/etc/backup-manager/config.yaml:ro
	legacy := []Service{{
		Name:   "backup-manager",
		Source: "positive control: the pre-#196 mount shape",
		Mounts: []Mount{{
			Role:          roleForContainerPath(c, c.ConfigFilePath()),
			HostPath:      "/mnt/tank/backup-manager/config/config.yaml",
			ContainerPath: c.ConfigFilePath(),
			ReadOnly:      true,
		}},
	}}

	v := CheckStorageShapes(legacy, c)
	if len(v) == 0 {
		t.Fatalf("the read-only single-file config mount at %s produced no violation, so nothing stops it being reintroduced", c.ConfigFilePath())
	}
	if !hasRule(v, RuleLegacyConfigFileMount) {
		t.Errorf("the shape was refused, but not as %q: %s. A failure that does not name the shape leaves the next reader guessing", RuleLegacyConfigFileMount, format(v))
	}
	joined := format(v)
	for _, want := range []string{c.ContainerPaths.Config, "#196"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the refusal never mentions %q, so it does not say what to do instead:\n%s", want, joined)
		}
	}

	// The second half of the same shape, on its own: the config
	// directory mounted read-only. A deployment can get this wrong
	// without going all the way back to a single file, and the
	// legacy-path rule above would not see it.
	readOnlyDir := []Service{{
		Name:   "backup-manager",
		Source: "positive control: the config directory mounted :ro",
		Mounts: []Mount{{
			Role:          "config",
			HostPath:      "/mnt/tank/backup-manager/config",
			ContainerPath: c.ContainerPaths.Config,
			ReadOnly:      true,
		}},
	}}
	v = CheckStorageShapes(readOnlyDir, c)
	if !hasRule(v, RuleWrongWriteMode) {
		t.Errorf("mounting the config directory read-only produced %v, want a %q violation", v, RuleWrongWriteMode)
	}

	// Negative control for the same checker: the shipped shape is
	// clean, so the two assertions above are about the mutation and not
	// about the checker rejecting everything it is handed.
	shipped := []Service{{
		Name:   "backup-manager",
		Source: "control: the #196 shape",
		Mounts: []Mount{{
			Role:          "config",
			HostPath:      "/mnt/tank/backup-manager/config",
			ContainerPath: c.ContainerPaths.Config,
		}},
	}}
	if v := CheckStorageShapes(shipped, c); len(v) > 0 {
		t.Errorf("the writable configuration directory was refused:\n%s", format(v))
	}
}

// TestKeyMaterialStaysAReadOnlySingleFile is the other direction, and it
// is not symmetry for its own sake. #196 changes ONE role. A change that
// relaxed the SSH key or known_hosts to writable at the same time would
// pass every write-mode assertion above while widening what a compromised
// engine process can rewrite.
func TestKeyMaterialStaysAReadOnlySingleFile(t *testing.T) {
	c := MustLoad()
	for _, role := range []string{"sshKey", "knownHosts"} {
		p, _ := c.ContainerPaths.ByRole(role)
		if got := c.WriteModeFor(p); got != WriteModeReadOnly {
			t.Errorf("the %q role (%s) is declared %s, want %s", role, p, got, WriteModeReadOnly)
		}
	}

	writableKey := []Service{{
		Name:   "backup-manager",
		Source: "positive control: key material mounted writable",
		Mounts: []Mount{{
			Role:          "sshKey",
			HostPath:      "/mnt/tank/backup-manager/secrets/id_ed25519",
			ContainerPath: c.ContainerPaths.SSHKey,
		}},
	}}
	if v := CheckStorageShapes(writableKey, c); !hasRule(v, RuleWrongWriteMode) {
		t.Errorf("mounting the SSH private key writable produced %v, want a %q violation", v, RuleWrongWriteMode)
	}
}

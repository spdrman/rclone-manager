package packaging

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// TestNoPlatformMountsAProhibitedHostPath is issue #169's privilege check
// in the one place that covers every format.
//
// distribution/compose already runs the whole prohibition list against
// the canonical definition and every compose artifact derived from it,
// and that is four of the five adapters. The fifth is an Unraid Docker
// template, which is XML, so the docker-socket and unbounded-filesystem
// rules never reached it: the one adapter that is not compose was the one
// adapter those rules did not cover.
func TestNoPlatformMountsAProhibitedHostPath(t *testing.T) {
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			if v := CheckMountedHostPaths(p.services(t)); len(v) > 0 {
				t.Errorf("mounts a host path EPIC B #81 prohibits:\n%s", format(v))
			}
		})
	}
}

// TestTheHostPathProhibitionFires is its positive control, and the
// negative half of the pair EPIC B #81 asks for by name: the rules prove
// the settings are absent, and apps/generic/tests/dockercli proves the
// deployment does real work without them.
func TestTheHostPathProhibitionFires(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		want bool
	}{
		{"the Docker socket", "/var/run/docker.sock", true},
		{"the Docker socket, the other spelling", "/run/docker.sock", true},
		{"the host root", "/", true},
		{"a host system directory", "/etc", true},
		{"something beneath a host system directory", "/etc/backup-manager", true},

		// The controls that stop this rule from refusing everything. A
		// prohibition that also fires on the real host paths would be
		// switched off within a week.
		{"a TrueNAS dataset", "/mnt/tank/backup-manager/state", false},
		{"an Unraid appdata path", "/mnt/user/appdata/backup-manager/state", false},
		{"an OMV data filesystem", "/srv/dev-disk-by-uuid/appdata/backup-manager/state", false},
		{"a Synology volume", "/volume1/docker/backup-manager/state", false},
		{"an unexpanded variable, which is the operator's to resolve", "${STATE_DIR:?set STATE_DIR}", false},

		// The near-misses. A prefix comparison that forgot the separator
		// would fire on all three of these, and a maintainer chasing
		// that noise would narrow the rule until it fired on nothing.
		{"a path that merely starts with a prohibited name", "/etcetera/backups", false},
		{"a path that merely starts with var", "/variable/backups", false},
		{"a path that merely starts with home", "/homelab/backups", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svcs := []Service{{
				Name:   "backup-manager",
				Source: "positive control",
				Mounts: []Mount{{Role: "state", HostPath: tc.host, ContainerPath: "/data/state"}},
			}}
			got := len(CheckMountedHostPaths(svcs)) > 0
			if got != tc.want {
				t.Errorf("mounting %s was refused=%v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestTheTwoProhibitedHostPathListsAgree pins this package's copy of the
// prohibited-path list to the contract's own. Two copies that can differ
// silently are worse than one copy with a hole in it, because the hole at
// least stays where it was put.
func TestTheTwoProhibitedHostPathListsAgree(t *testing.T) {
	raw, err := os.ReadFile(Path(filepath.Join("distribution", "compose", "runtime-contract.json")))
	if err != nil {
		t.Fatalf("read the runtime contract: %v", err)
	}
	var contract struct {
		Prohibited []struct {
			ID    string   `json:"id"`
			Kind  string   `json:"kind"`
			Paths []string `json:"paths"`
		} `json:"prohibited"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("parse the runtime contract: %v", err)
	}

	want := map[string]bool{}
	for _, rule := range contract.Prohibited {
		if rule.Kind != "mount-host-path" {
			continue
		}
		for _, p := range rule.Paths {
			want[p] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("the runtime contract declares no mount-host-path rule, so this comparison would pass having checked nothing")
	}

	got := map[string]bool{}
	for _, p := range prohibitedHostPaths {
		got[p.path] = true
	}
	for p := range want {
		if !got[p] {
			t.Errorf("the runtime contract prohibits mounting %s and this package's list does not, so the Unraid template is not held to it", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("this package prohibits mounting %s and the runtime contract does not; the contract is the source", p)
		}
	}
}

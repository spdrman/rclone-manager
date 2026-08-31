package packaging

import (
	"encoding/json"
	"os"
	"path"
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

// canonicalRuntimeServices reads container/compose.yaml, the definition
// every adapter derives from, with its own .env.example supplying the
// host paths.
//
// It exists because that file was in NEITHER completeness set.
// allPlatforms() enumerates adapter fixtures, so CheckStorageShapes never
// saw the canonical definition, and the runtime contract's own write-mode
// booleans covered two of its four service mounts. A `:ro` on /data/state
// or /data/backups in this one file therefore passed every gate in the
// repository, on the file that is the authority for the whole product,
// producing either a journal that cannot open or backups that cannot
// land.
func canonicalRuntimeServices(t *testing.T) []Service {
	t.Helper()
	env, err := ReadEnvFile(Path(filepath.Join("container", ".env.example")))
	if err != nil {
		t.Fatalf("read container/.env.example: %v", err)
	}
	svcs, err := ReadCompose(Path(filepath.Join("container", "compose.yaml")), env)
	if err != nil {
		t.Fatalf("read the canonical runtime definition: %v", err)
	}
	if len(svcs) == 0 {
		t.Fatal("container/compose.yaml declares no services, so every check over it would pass vacuously")
	}
	return svcs
}

// TestEveryPlatformMountsEveryRoleWithItsDeclaredWriteMode is the adapter
// half. It is separate from TestEveryPlatformMapsEveryStorageRoleTheSameWay
// (which owns "which host path, at which container path") so that a
// failure here says the write MODE drifted rather than that a path did.
//
// The canonical definition runs through the same checker as the adapters,
// and the overlap with distribution/compose's own write-mode rule is
// accepted deliberately: the cost is one duplicated failure message, and
// the cost of the arrangement it replaces is the class of defect this
// stack exists to close.
func TestEveryPlatformMountsEveryRoleWithItsDeclaredWriteMode(t *testing.T) {
	c := MustLoad()
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			if v := CheckStorageShapes(p.services(t), c); len(v) > 0 {
				t.Errorf("mounts disagree with canonical.json's declared write modes:\n%s", format(v))
			}
		})
	}
	t.Run("container/compose.yaml (canonical)", func(t *testing.T) {
		if v := CheckStorageShapes(canonicalRuntimeServices(t), c); len(v) > 0 {
			t.Errorf("the canonical runtime definition disagrees with canonical.json's declared write modes:\n%s", format(v))
		}
	})
}

// TestTheCanonicalDefinitionIsHeldToTheStorageShapeRule is the control on
// the fixture above: a rule pointed at a file nobody planted a fault in is
// a rule nobody has watched work. It flips the backup destination to `:ro`
// in the parsed canonical services and requires the refusal.
func TestTheCanonicalDefinitionIsHeldToTheStorageShapeRule(t *testing.T) {
	c := MustLoad()
	svcs := canonicalRuntimeServices(t)

	flipped := 0
	for i := range svcs {
		for j := range svcs[i].Mounts {
			if svcs[i].Mounts[j].ContainerPath == c.ContainerPaths.Backups {
				svcs[i].Mounts[j].ReadOnly = true
				flipped++
			}
		}
	}
	if flipped == 0 {
		t.Fatalf("the canonical definition mounts nothing at %s, so this control had nothing to break", c.ContainerPaths.Backups)
	}
	v := CheckStorageShapes(svcs, c)
	if !hasRule(v, RuleWrongWriteMode) {
		t.Fatalf("mounting the backup destination read-only in the canonical definition produced %v, want a %q violation. That is the mount every retained artifact lands on", v, RuleWrongWriteMode)
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

	// Both shapes of "the configuration is a file", as a table.
	//
	// The first row is the one this control used to get wrong. Its
	// comment said "the pre-#196 declaration, verbatim in shape:
	// <host>/config/config.yaml:/etc/backup-manager/config.yaml:ro" and
	// then planted ConfigFilePath(), which is the config DIRECTORY plus
	// config.yaml and therefore /etc/backup-manager/config/config.yaml,
	// a path no deployment has ever used. So the rule named for the
	// historical shape was proven against a value that is not it, and the
	// historical shape itself would have come back through the generic
	// role refusal with the unhelpful message this rule exists to replace.
	legacyPath := path.Join(path.Dir(c.ContainerPaths.Config), c.ConfigFileName)
	for _, tc := range []struct {
		name          string
		containerPath string
		hostPath      string
	}{
		{"the pre-#196 shape, literally", legacyPath, "/mnt/tank/backup-manager/config/config.yaml"},
		{"the same mistake made against the new directory", c.ConfigFilePath(), "/mnt/tank/backup-manager/config/config.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy := []Service{{
				Name:   "backup-manager",
				Source: "positive control: " + tc.name,
				Mounts: []Mount{{
					Role:          roleForContainerPath(c, tc.containerPath),
					HostPath:      tc.hostPath,
					ContainerPath: tc.containerPath,
					ReadOnly:      true,
				}},
			}}

			v := CheckStorageShapes(legacy, c)
			if len(v) == 0 {
				t.Fatalf("the read-only single-file config mount at %s produced no violation, so nothing stops it being reintroduced", tc.containerPath)
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
		})
	}

	// And the reason the first row could not have been caught by anything
	// else: the pre-#196 container path resolves to no canonical role, so
	// CheckStorageShapes's `if m.Role == "" { continue }` skips it and
	// only this rule stands between it and the generic refusal.
	if got := roleForContainerPath(c, legacyPath); got != "" {
		t.Errorf("roleForContainerPath(%s) = %q, want the empty role: if the legacy path ever resolves to a role, this rule and the write-mode rule both claim it and the message a reader gets depends on statement order", legacyPath, got)
	}
	if legacyPath == c.ConfigFilePath() {
		t.Fatalf("the legacy container path and the current one are both %s, so the table above ran the same case twice and the historical shape is still unproven", legacyPath)
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
	v := CheckStorageShapes(readOnlyDir, c)
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
	t.Run("container/compose.yaml (canonical)", func(t *testing.T) {
		if v := CheckMountedHostPaths(canonicalRuntimeServices(t)); len(v) > 0 {
			t.Errorf("the canonical runtime definition mounts a host path EPIC B #81 prohibits:\n%s", format(v))
		}
	})
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

// TestTheProhibitedHostPathSpellingsAllResolveToTheSameVerdict is the
// behavioural half, and it is the half that was missing.
//
// There used to be two implementations of this decision: this package's,
// which trimmed a trailing slash, and distribution/compose's, which ran
// filepath.Clean. The test below pinned the two path LISTS and said so in
// its own comment, so the spellings that only one of them caught were
// invisible to it. There is now one function, packaging.HostPathIsAt,
// which distribution/compose calls; this drives it over the spellings a
// host path can be written in, and distribution/compose's own
// hostpath_internal_test.go asserts the two entry points agree verdict for
// verdict over the same table.
func TestTheProhibitedHostPathSpellingsAllResolveToTheSameVerdict(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		// The evasions. Every one of these is the Docker socket, and
		// every one of them used to walk past this matcher.
		{"/var/run/docker.sock", true},
		{"//var/run/docker.sock", true},
		{"/var/run/./docker.sock", true},
		{"/var/run/../run/docker.sock", true},
		{"/mnt/../var/run/docker.sock", true},
		{"/var/run/docker.sock/", true},

		// A trailing slash on a prohibited directory, and the directory
		// itself.
		{"/etc/", true},
		{"/etc", true},
		{"/etc/./ssh", true},

		// The near-misses, kept here as well as in the table above so
		// that normalising the spelling cannot be "fixed" by widening
		// the comparison.
		{"/etcetera", false},
		{"/etcetera/backups", false},
		{"/mnt/tank/backup-manager/state", false},
		{"/mnt/tank/../tank/backup-manager/state", false},

		// An unexpanded reference is the operator's to resolve.
		{"${STATE_DIR:?set STATE_DIR}", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			got := false
			for _, p := range prohibitedHostPaths {
				if HostPathIsAt(tc.host, p.path) {
					got = true
					break
				}
			}
			if got != tc.want {
				t.Errorf("HostPathIsAt(%q, ...) refused=%v, want %v", tc.host, got, tc.want)
			}
		})
	}

	// The one case with no good answer, pinned so it stays deliberate: a
	// mount that declares no host path at all is malformed, and the
	// fail-closed reading of "no path" is the widest one.
	if !HostPathIsAt("", "/") {
		t.Error("an empty host path is not read as the host root; a malformed mount must fail closed, not pass")
	}
}

// TestTheTwoProhibitedHostPathListsAgree pins this package's copy of the
// prohibited-path list to the contract's own. Two copies that can differ
// silently are worse than one copy with a hole in it, because the hole at
// least stays where it was put.
//
// This pins the DATA. The rule itself is one function now, and the test
// above pins its behaviour.
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

package packaging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the automated half of Work Package 4.3's acceptance
// criteria (docs/EPIC-B-multi-nas.md §72), and the Phase 4 TDD Gate's
// checks that are decidable from the repository alone:
//
//	core version parity .............. TestEveryPlatformUsesTheExactCanonicalImage
//	                                   (one image reference, identical everywhere)
//	provider package metadata ........ TestEveryPlatformShipsItsRequiredMetadata
//	architecture ..................... TestArchitectureParityAndRecordedBinaryHashes
//	backup-root containment .......... TestBackupRootContainment
//	auth mode ........................ TestEveryPlatformUsesLocalAuthOnly
//	no bundled secrets ............... TestNoBundledSecrets
//	no provider-specific lifecycle ... TestNoLifecycleCodeInAnyContainerPackage
//
// NOT claimed here: core binary HASH parity. Nothing in this package
// derives a hash from anything. TestArchitectureParityAndRecordedBinaryHashes
// checks that container/release-manifest.json records a non-empty SHA-256
// for each binary on each architecture, which cannot detect a stale hash
// or a mismatch. It no longer covers for a manifest describing a build
// nobody can reach: issue #174 fixed that, and release-manifest-integrity
// now decides reachability on every run. Reachable is not compared,
// though. A gate item marked as covered by a check that cannot
// fail for the reason the item names is worse than an unclaimed gap,
// because it stops anyone looking. The only place binary hashes are
// verified against real bytes is `spkctl verify` against a built .spk in
// apps/synology.
//
// The other half (install, start, update, remove, and state surviving
// container replacement) cannot be reached from a laptop and lives in
// docs/acceptance/ as prewritten operator procedures, per §68.
//
// Every negative assertion here has a positive control in scan_test.go
// proving the checker it relies on can actually fail.

// platformFixture is one packaged target and how to read its metadata.
// Grouping the three behind one table is deliberate: WP4.3 groups them
// because they are the same shape, so a rule that holds for one has to
// hold for all three, and a new Tier B/C profile joins by adding a row.
type platformFixture struct {
	// name is the apps/<name>/ directory.
	name string
	// requiredFiles must all exist. This is what makes the suite fail
	// before the metadata is written rather than passing vacuously
	// against an empty directory.
	requiredFiles []string
	// services reads the platform's own metadata into the shared Service
	// shape.
	services func(t *testing.T) []Service
	// engineService and uiService name the two containers inside services.
	engineService string
	uiService     string
	// uiHealthcheck says how this platform stops the Web UI container
	// from running the image's baked-in `/backup-manager status`
	// healthcheck, which needs a config file and a state database that
	// container does not have (WP4.3 calls this out by name).
	uiHealthcheck uiHealthcheckStrategy
	// hardening says where this platform expresses read-only rootfs,
	// dropped capabilities and no-new-privileges.
	hardening hardeningStrategy
	// composeProfiles are the platform's compose files and the env file
	// each is read with, for the rules that need the file's own text
	// rather than the Services it reduces to.
	composeProfiles []composeProfile
	// acceptance is the procedure in docs/acceptance/ that stands in for
	// hardware, and docSubstitutions expands the placeholders that
	// procedure writes for machine-specific paths, so a rule about the
	// paths it touches is not defeated by "$DISK".
	acceptance       string
	docSubstitutions map[string]string
}

type composeProfile struct {
	// compose and env are relative to apps/<name>/. env is empty for a
	// profile that ships literal paths instead.
	compose string
	env     string
}

type uiHealthcheckStrategy int

const (
	// overrideHealthcheck: the profile replaces the test with
	// `/backup-manager-web healthcheck`.
	overrideHealthcheck uiHealthcheckStrategy = iota
	// disableHealthcheck: the profile turns it off. Unraid's only seam is
	// `docker run --health-cmd`, which is shell form, and the distroless
	// runtime image has no shell, so an override there would be a
	// permanently failing healthcheck. Off is honest; broken is not.
	disableHealthcheck
)

type hardeningStrategy int

const (
	composeHardening hardeningStrategy = iota
	extraParamsHardening
)

func allPlatforms() []platformFixture {
	return []platformFixture{
		{
			name: "truenas",
			requiredFiles: []string{
				"README.md",
				"compose/backup-manager.yaml",
				"catalog/app.yaml",
				"catalog/questions.yaml",
				"catalog/ix_values.yaml",
				"catalog/templates/docker-compose.yaml",
			},
			services: func(t *testing.T) []Service {
				t.Helper()
				svcs, err := ReadCompose(filepath.Join(PlatformDir("truenas"), "compose", "backup-manager.yaml"), nil)
				if err != nil {
					t.Fatalf("read TrueNAS custom-app compose: %v", err)
				}
				// The catalog entry is the app-store deliverable, and
				// what an install renders is the template driven by
				// ix_values.yaml, not the paste-in compose beside it.
				// Reading only the latter left the deliverable checked
				// for file existence and question/template agreement and
				// for nothing substantive: not its image, not its host
				// paths, not its ports, not its hardening. Rendering it
				// here puts it through every per-platform rule below.
				return append(svcs, renderedTrueNASCatalog(t)...)
			},
			engineService: "backup-manager",
			uiService:     "backup-manager-ui",
			uiHealthcheck: overrideHealthcheck,
			hardening:     composeHardening,
			composeProfiles: []composeProfile{
				{compose: "compose/backup-manager.yaml"},
			},
			acceptance:       "truenas-provider-acceptance.md",
			docSubstitutions: map[string]string{"/mnt/POOL": "/mnt/tank"},
		},
		{
			name: "unraid",
			requiredFiles: []string{
				"README.md",
				"template/backup-manager.xml",
				"template/backup-manager-ui.xml",
			},
			services: func(t *testing.T) []Service {
				t.Helper()
				var out []Service
				for _, f := range []string{"backup-manager.xml", "backup-manager-ui.xml"} {
					tpl, err := ReadUnraidTemplate(filepath.Join(PlatformDir("unraid"), "template", f))
					if err != nil {
						t.Fatalf("read Unraid template %s: %v", f, err)
					}
					out = append(out, tpl.AsService(f))
				}
				return out
			},
			engineService:    "backup-manager",
			uiService:        "backup-manager-ui",
			uiHealthcheck:    disableHealthcheck,
			hardening:        extraParamsHardening,
			acceptance:       "unraid-provider-acceptance.md",
			docSubstitutions: map[string]string{},
		},
		{
			name: "openmediavault",
			requiredFiles: []string{
				"README.md",
				"compose/backup-manager.yml",
				"compose/backup-manager.env",
			},
			services: func(t *testing.T) []Service {
				t.Helper()
				dir := filepath.Join(PlatformDir("openmediavault"), "compose")
				env, err := ReadEnvFile(filepath.Join(dir, "backup-manager.env"))
				if err != nil {
					t.Fatalf("read OMV env file: %v", err)
				}
				svcs, err := ReadCompose(filepath.Join(dir, "backup-manager.yml"), env)
				if err != nil {
					t.Fatalf("read OMV compose: %v", err)
				}
				return svcs
			},
			engineService: "backup-manager",
			uiService:     "backup-manager-ui",
			uiHealthcheck: overrideHealthcheck,
			hardening:     composeHardening,
			composeProfiles: []composeProfile{
				{compose: "compose/backup-manager.yml", env: "compose/backup-manager.env"},
			},
			acceptance:       "openmediavault-provider-acceptance.md",
			docSubstitutions: map[string]string{"$DISK": "/srv/dev-disk-by-uuid"},
		},
		{
			// WP4.5. Proxmox VE has no app store to package into, so
			// its Tier C profile is the same two-container Compose
			// shape as OpenMediaVault's, run inside a dedicated
			// container-host guest rather than on the PVE host. That
			// it joins this table by adding a row, with no new
			// checker, is the point: the deployment target changed
			// and none of the packaging rules did.
			name: "proxmox",
			requiredFiles: []string{
				"README.md",
				"compose/backup-manager.yml",
				"compose/backup-manager.env",
			},
			services: func(t *testing.T) []Service {
				t.Helper()
				dir := filepath.Join(PlatformDir("proxmox"), "compose")
				env, err := ReadEnvFile(filepath.Join(dir, "backup-manager.env"))
				if err != nil {
					t.Fatalf("read Proxmox env file: %v", err)
				}
				svcs, err := ReadCompose(filepath.Join(dir, "backup-manager.yml"), env)
				if err != nil {
					t.Fatalf("read Proxmox compose: %v", err)
				}
				return svcs
			},
			engineService: "backup-manager",
			uiService:     "backup-manager-ui",
			uiHealthcheck: overrideHealthcheck,
			hardening:     composeHardening,
			composeProfiles: []composeProfile{
				{compose: "compose/backup-manager.yml", env: "compose/backup-manager.env"},
			},
			acceptance: "proxmox-ve-deployment.md",
			// Every path the Proxmox procedure names is literal: the
			// share root is /mnt/backup-manager inside the guest, and
			// the profile derives the rest from it, so there is no
			// machine-specific placeholder to expand.
			docSubstitutions: map[string]string{},
		},
	}
}

// renderedTrueNASCatalog is what a TrueNAS Apps install actually deploys
// when the operator changes no answer: the catalog template with
// ix_values.yaml substituted in.
func renderedTrueNASCatalog(t *testing.T) []Service {
	t.Helper()
	catalog := filepath.Join(PlatformDir("truenas"), "catalog")
	rendered, err := RenderTrueNASCatalogTemplate(
		filepath.Join(catalog, "templates", "docker-compose.yaml"),
		filepath.Join(catalog, "ix_values.yaml"))
	if err != nil {
		t.Fatalf("render TrueNAS catalog template: %v", err)
	}
	svcs, err := ParseCompose([]byte(rendered), "catalog/templates/docker-compose.yaml (rendered from ix_values.yaml)", nil)
	if err != nil {
		t.Fatalf("parse rendered TrueNAS catalog: %v", err)
	}
	return svcs
}

// ---------------------------------------------------------------------
// The shared source of truth itself
// ---------------------------------------------------------------------

func TestCanonicalSourceIsInternallyConsistent(t *testing.T) {
	c := MustLoad()

	if got, want := c.Image.Reference, c.Image.Registry+"/"+c.Image.Repository+":"+c.Image.Tag; got != want {
		t.Errorf("image reference %q does not equal registry/repository:tag %q", got, want)
	}
	if c.Image.Tag == "" || c.Image.Tag == "latest" {
		t.Errorf("image tag is %q: §8's canonical-image rule forbids deploying `latest` and requires a pinned version", c.Image.Tag)
	}
	if c.AuthMode != "local-account" {
		t.Errorf("authMode = %q, want local-account: §13A gives every Tier B/C provider the reusable local auth, not one of its own", c.AuthMode)
	}
	if len(c.Architectures) == 0 {
		t.Error("no architectures declared")
	}
	for _, role := range Roles {
		p, ok := c.ContainerPaths.ByRole(role)
		if !ok || !strings.HasPrefix(p, "/") {
			t.Errorf("container path for role %q is %q, want an absolute path", role, p)
		}
	}
	if len(c.Platforms) == 0 {
		t.Fatal("no platforms declared")
	}
	for name, p := range c.Platforms {
		for _, role := range Roles {
			hp, ok := p.HostPaths.ByRole(role)
			if !ok || !strings.HasPrefix(hp, "/") {
				t.Errorf("%s: host path for role %q is %q, want an absolute path", name, role, hp)
			}
		}
	}
}

// TestArchitectureParityAndRecordedBinaryHashes is the gate's
// "architecture" line, and nothing more than that. The catalogs claim an
// architecture set; container/release-manifest.json records what was
// actually built, and a claim that outruns the build is the failure mode.
//
// It also checks that the manifest records a SHA-256 for every binary on
// every architecture, which is a presence check and NOT hash parity: no
// hash here is derived from any artifact, so a stale or wrong one passes.
// The test is named for what it measures for that reason. See #174.
func TestArchitectureParityAndRecordedBinaryHashes(t *testing.T) {
	c := MustLoad()

	data, err := os.ReadFile(filepath.Join(RepoRoot, "container", "release-manifest.json"))
	if err != nil {
		t.Fatalf("read release manifest: %v", err)
	}
	var manifest struct {
		Architectures []struct {
			Architecture string            `json:"architecture"`
			BinarySHA256 map[string]string `json:"binary_sha256"`
		} `json:"architectures"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse release manifest: %v", err)
	}

	var built []string
	for _, a := range manifest.Architectures {
		built = append(built, a.Architecture)
		for _, binary := range c.Binaries {
			name := strings.TrimPrefix(binary, "/")
			if a.BinarySHA256[name] == "" {
				t.Errorf("release manifest records no SHA-256 for %s on %s, but the packages ship an image claiming to contain it",
					name, a.Architecture)
			}
		}
	}

	claimed := append([]string(nil), c.Architectures...)
	sort.Strings(claimed)
	sort.Strings(built)
	if strings.Join(claimed, ",") != strings.Join(built, ",") {
		t.Errorf("canonical.json claims architectures %v but container/release-manifest.json records %v", claimed, built)
	}
}

// ---------------------------------------------------------------------
// Per-platform conformance
// ---------------------------------------------------------------------

func TestEveryPlatformShipsItsRequiredMetadata(t *testing.T) {
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, f := range p.requiredFiles {
				path := filepath.Join(PlatformDir(p.name), f)
				if _, err := os.Stat(path); err != nil {
					t.Errorf("missing required metadata %s: %v", f, err)
				}
			}
		})
	}
}

// TestNoLifecycleCodeInAnyContainerPackage is WP4.3's own RED requirement:
// "no apps/truenas/, apps/unraid/, or apps/openmediavault/ code implements
// lifecycle behaviour beyond metadata and templates". ScanLifecycle's
// positive controls in scan_test.go prove this can fail.
func TestNoLifecycleCodeInAnyContainerPackage(t *testing.T) {
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			violations, err := ScanLifecycle(PlatformDir(p.name))
			if err != nil {
				t.Fatalf("ScanLifecycle: %v", err)
			}
			if len(violations) > 0 {
				t.Errorf("apps/%s implements lifecycle behaviour beyond metadata and templates:\n%s", p.name, format(violations))
			}
		})
	}
}

// TestNoBundledSecrets is the gate's "no bundled secrets" line, and §13A's
// "provider packaging must not bake credentials into images".
func TestNoBundledSecrets(t *testing.T) {
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			violations, err := ScanSecrets(PlatformDir(p.name))
			if err != nil {
				t.Fatalf("ScanSecrets: %v", err)
			}
			if len(violations) > 0 {
				t.Errorf("apps/%s bundles credential material:\n%s", p.name, format(violations))
			}
		})
	}
}

// TestEveryPlatformUsesTheExactCanonicalImage is the first acceptance
// criterion for all three platforms, and the whole reason WP4.3 groups
// them: none of them is allowed a build of its own.
func TestEveryPlatformUsesTheExactCanonicalImage(t *testing.T) {
	c := MustLoad()

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, svc := range p.services(t) {
				if svc.Image != c.Image.Reference {
					t.Errorf("service %q (%s) uses image %q, want the canonical %q",
						svc.Name, svc.Source, svc.Image, c.Image.Reference)
				}
				if len(svc.UnresolvedVars) > 0 {
					t.Errorf("service %q (%s) leaves %v unresolved: a profile whose image or storage path only resolves when an operator happens to have set a variable installs wrong silently",
						svc.Name, svc.Source, svc.UnresolvedVars)
				}
			}
		})
	}
}

// TestEveryPlatformMapsEveryStorageRoleTheSameWay holds all three to one
// storage contract. The container side is fixed by the binaries
// themselves; the host side is whatever canonical.json declares for that
// platform. Both are checked, because getting either wrong produces a
// container that starts and then cannot find its own state.
func TestEveryPlatformMapsEveryStorageRoleTheSameWay(t *testing.T) {
	c := MustLoad()

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			platform, ok := c.Platforms[p.name]
			if !ok {
				t.Fatalf("canonical.json declares no platform %q", p.name)
			}

			seen := map[string]Mount{}
			for _, svc := range p.services(t) {
				for _, m := range svc.Mounts {
					if m.Role == "" {
						t.Errorf("service %q (%s) mounts %s at %s, which is not a container path the canonical image knows about",
							svc.Name, svc.Source, m.HostPath, m.ContainerPath)
						continue
					}
					if prev, dup := seen[m.Role]; dup && prev.HostPath != m.HostPath {
						t.Errorf("role %q is mounted from two different host paths (%s and %s)", m.Role, prev.HostPath, m.HostPath)
					}
					seen[m.Role] = m
				}
			}

			for _, role := range Roles {
				m, ok := seen[role]
				if !ok {
					t.Errorf("no service maps the %q storage role", role)
					continue
				}
				wantContainer, _ := c.ContainerPaths.ByRole(role)
				if m.ContainerPath != wantContainer {
					t.Errorf("role %q mounts at %s in the container, want %s", role, m.ContainerPath, wantContainer)
				}
				wantHost, _ := platform.HostPaths.ByRole(role)
				if m.HostPath != wantHost {
					t.Errorf("role %q defaults to host path %s, want %s (canonical.json is the source of truth; change it there, not here)",
						role, m.HostPath, wantHost)
				}
				wantRO := contains(c.ReadOnlyContainerPaths, wantContainer)
				if m.ReadOnly != wantRO {
					t.Errorf("role %q is mounted read-only=%v, want %v", role, m.ReadOnly, wantRO)
				}
			}
		})
	}
}

// TestBackupRootContainment is the gate's "backup-root containment" line
// and §19.2: private application state and the user backup root are
// separate security domains, and "the backup root MUST NOT contain SSH
// private keys or authentication state".
func TestBackupRootContainment(t *testing.T) {
	c := MustLoad()

	for name, platform := range c.Platforms {
		t.Run(name, func(t *testing.T) {
			backups := platform.HostPaths.Backups

			// One meaning for storageMount, pinned to the backup root.
			// Read as an app root it permitted the backup root to sit
			// anywhere beneath it, so on TrueNAS the containment check
			// was a check that a path contained itself; and because the
			// wizard seeds a backup destination from this string, an app
			// root proposed writing artifacts next to secrets/id_ed25519.
			if platform.StorageMount != backups {
				t.Errorf("storageMount is %s but the backup root is %s; storageMount IS the backup root (see canonical.go), which is what makes the containment rule below bite and what stops the backup-set wizard proposing a directory that also holds key material",
					platform.StorageMount, backups)
			}
			for _, role := range []string{"state", "config", "sshKey", "knownHosts"} {
				p, _ := platform.HostPaths.ByRole(role)
				if Contains(backups, p) {
					t.Errorf("%q (%s) sits inside the backup root %s; §19.2 keeps private state, config and key material out of it entirely",
						role, p, backups)
				}
				if Contains(p, backups) {
					t.Errorf("the backup root %s sits inside %q (%s), which collapses the two security domains the other way round",
						backups, role, p)
				}
			}
		})
	}
}

// TestEveryPlatformDeclaresTheSameStorageMountAsItsBridge pins
// canonical.json to apps/<platform>/frontend/platform.ts. Those two files
// belong to different work packages and are read by different audiences,
// which is exactly how they drift.
func TestEveryPlatformDeclaresTheSameStorageMountAsItsBridge(t *testing.T) {
	c := MustLoad()

	for name, platform := range c.Platforms {
		t.Run(name, func(t *testing.T) {
			got, err := BridgeStorageMount(filepath.Join(PlatformDir(name), "frontend", "platform.ts"))
			if err != nil {
				t.Fatalf("read bridge storage mount: %v", err)
			}
			if got != platform.StorageMount {
				t.Errorf("frontend bridge declares storageMount %q, canonical.json declares %q", got, platform.StorageMount)
			}
		})
	}
}

// TestOnlyTheWebUIContainerPublishesAPort proves the two-container split
// survived translation into each platform's own format. The engine holds
// the state database, the credentials and the API; it is reachable only
// from the Web UI container, and a profile that publishes its port has
// quietly undone that.
func TestOnlyTheWebUIContainerPublishesAPort(t *testing.T) {
	c := MustLoad()
	wantContainerPort := strconv.Itoa(c.ListenPort)

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, svc := range p.services(t) {
				switch svc.Name {
				case p.engineService:
					if len(svc.Ports) != 0 {
						t.Errorf("the engine service publishes %v; it must publish nothing and be reachable only from the Web UI container", svc.Ports)
					}
				case p.uiService:
					if len(svc.Ports) != 1 {
						t.Errorf("the Web UI service publishes %v, want exactly one port", svc.Ports)
						continue
					}
					if !strings.HasSuffix(svc.Ports[0], ":"+wantContainerPort) {
						t.Errorf("the Web UI service publishes %q, which does not map to the container's own listen port %s", svc.Ports[0], wantContainerPort)
					}
				default:
					t.Errorf("unexpected service %q; this profile should declare exactly %q and %q", svc.Name, p.engineService, p.uiService)
				}
			}
		})
	}
}

// TestTheWebUIContainerDoesNotRunTheImageHealthcheck is WP4.3's own
// warning made executable: the canonical image bakes in
// `HEALTHCHECK /backup-manager status`, which needs a config file and a
// state database the Web UI container does not have, so every profile has
// to override or disable it.
func TestTheWebUIContainerDoesNotRunTheImageHealthcheck(t *testing.T) {
	c := MustLoad()

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, svc := range p.services(t) {
				if svc.Name != p.uiService {
					continue
				}
				switch p.uiHealthcheck {
				case overrideHealthcheck:
					if svc.HealthcheckDisabled {
						t.Error("the Web UI service disables its healthcheck; this profile can express an override, so it should")
					}
					if !healthcheckMatches(svc.HealthcheckTest, c.Commands.Healthcheck) {
						t.Errorf("the Web UI service's healthcheck is %v, want one running %v", svc.HealthcheckTest, c.Commands.Healthcheck)
					}
				case disableHealthcheck:
					if !svc.HealthcheckDisabled {
						t.Errorf("the Web UI template does not disable the image healthcheck (ExtraParams = %q); left inherited, `/backup-manager status` fails forever in a container with no config and no state database",
							svc.ExtraParams)
					}
				}
			}
		})
	}
}

// TestEveryPlatformAppliesTheSameHardening keeps the three profiles from
// drifting on the settings container/compose.yaml already established for
// the generic app: non-root, read-only rootfs, all capabilities dropped,
// no new privileges.
func TestEveryPlatformAppliesTheSameHardening(t *testing.T) {
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, svc := range p.services(t) {
				switch p.hardening {
				case composeHardening:
					if !svc.ReadOnlyRootFS {
						t.Errorf("service %q does not set read_only: true", svc.Name)
					}
					if !contains(svc.CapDrop, "ALL") {
						t.Errorf("service %q does not drop ALL capabilities (cap_drop = %v)", svc.Name, svc.CapDrop)
					}
					if !contains(svc.SecurityOpt, "no-new-privileges:true") {
						t.Errorf("service %q does not set no-new-privileges (security_opt = %v)", svc.Name, svc.SecurityOpt)
					}
					if svc.User == "" {
						t.Errorf("service %q does not pin a non-root user", svc.Name)
					}
					if len(svc.Tmpfs) == 0 {
						t.Errorf("service %q has a read-only rootfs and no tmpfs for /tmp; Go's temp directory would be unwritable", svc.Name)
					}
				case extraParamsHardening:
					// Parsed into flags, not searched for substrings.
					// Presence-only matching made `--user` satisfiable by
					// `--userns=host` and could not see a `--privileged`,
					// a `--cap-add` or a `seccomp=unconfined` appended to
					// the same line, which is the one format where the
					// template scanner's own <Privileged> rule cannot
					// reach either. CheckExtraParamsHardening's positive
					// controls in scan_test.go prove each of those fails.
					if v := CheckExtraParamsHardening(svc.Source, svc.ExtraParams); len(v) > 0 {
						t.Errorf("template %q does not apply the same hardening as the compose profiles (ExtraParams = %q):\n%s",
							svc.Name, svc.ExtraParams, format(v))
					}
				}
			}
		})
	}
}

// TestEachContainerRunsItsCanonicalCommand pins which of the canonical
// image's two commands each container runs. Two containers from one image
// differ only by argv, so getting this wrong produces a deployment that
// starts cleanly and serves nothing: two engines and no UI, or two UIs and
// no API.
func TestEachContainerRunsItsCanonicalCommand(t *testing.T) {
	c := MustLoad()

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, svc := range p.services(t) {
				var want []string
				switch svc.Name {
				case p.engineService:
					want = c.Commands.Engine
				case p.uiService:
					want = c.Commands.WebUI
				default:
					continue
				}
				// runsCanonicalCommand, not string equality: since #167
				// the canonical commands may carry a `--profile=` flag.
				// The binary and every positional argument still have to
				// match exactly; see that helper for what it does and
				// does not allow.
				if !runsCanonicalCommand(svc.Command, want) {
					t.Errorf("service %q (%s) runs %v, want %v", svc.Name, svc.Source, svc.Command, want)
				}
			}
		})
	}
}

// TestEveryPlatformUsesLocalAuthOnly is the gate's "auth mode" line and
// WP4.3's "all three use local-auth from the generic host; no platform
// gets its own auth mechanism".
func TestEveryPlatformUsesLocalAuthOnly(t *testing.T) {
	c := MustLoad()

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			bridge, err := os.ReadFile(filepath.Join(PlatformDir(p.name), "frontend", "platform.ts"))
			if err != nil {
				t.Fatalf("read bridge: %v", err)
			}
			if !strings.Contains(string(bridge), `mode: "`+c.AuthMode+`"`) {
				t.Errorf("the frontend bridge does not report auth mode %q", c.AuthMode)
			}
			if strings.Contains(string(bridge), "nativeAuth: true") {
				t.Error("the frontend bridge claims native auth; only UGOS has a native session adapter")
			}

			findings, err := ScanForBespokeAuth(PlatformDir(p.name))
			if err != nil {
				t.Fatalf("ScanForBespokeAuth: %v", err)
			}
			if len(findings) > 0 {
				t.Errorf("apps/%s wires an authentication mechanism of its own:\n%s", p.name, format(findings))
			}
		})
	}
}

// TestEveryVariableAProfileDeclaresIsActuallyRead is the guard for a knob
// that is documented as the one thing an operator must change and is read
// by nothing. The OpenMediaVault env file shipped exactly that: DISK, under
// the heading "THE ONE SUBSTITUTION THAT MATTERS", with the five host paths
// beneath it spelling the placeholder out literally instead of deriving
// from it. Following the instruction exactly changed one line and deployed
// five wrong bind mounts, and because Docker creates a missing bind-mount
// source, the backup root would have landed on the OS disk. The old suite
// could not see it: it compared the env file's literal placeholder against
// canonical.json's identical literal placeholder, and both agreed on the
// wrong thing.
func TestEveryVariableAProfileDeclaresIsActuallyRead(t *testing.T) {
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, profile := range p.composeProfiles {
				if profile.env == "" {
					continue
				}
				env, err := ReadEnvFile(filepath.Join(PlatformDir(p.name), profile.env))
				if err != nil {
					t.Fatalf("read env file: %v", err)
				}
				if len(env) == 0 {
					t.Fatalf("%s declares no variables; without this guard the comparison below would pass vacuously", profile.env)
				}
				compose, err := os.ReadFile(filepath.Join(PlatformDir(p.name), profile.compose))
				if err != nil {
					t.Fatalf("read compose: %v", err)
				}
				referenced := map[string]bool{}
				for _, ref := range VarRefs(string(compose)) {
					referenced[ref.Name] = true
				}
				if len(referenced) == 0 {
					t.Fatalf("%s references no variables at all; without this guard the comparison below would pass vacuously", profile.compose)
				}
				for name := range env {
					if !referenced[name] {
						t.Errorf("%s sets %s but %s reads it nowhere: a knob an operator is told to change and that changes nothing is worse than no knob at all",
							profile.env, name, profile.compose)
					}
				}
			}
		})
	}
}

// TestEveryStoragePathFailsClosed is §19.2's other half. A host path
// written ${STATE_DIR:-/some/path} lands somewhere plausible when the
// variable is unset, and Docker creates a missing bind-mount source, so
// the deployment starts with its backup root on the wrong filesystem
// rather than refusing to start. container/compose.yaml already uses the
// fail-closed form for exactly this reason, and its own comment says so;
// the provider profiles have to make the same choice. Literal paths are
// fine, since there is nothing to be unset.
func TestEveryStoragePathFailsClosed(t *testing.T) {
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, svc := range p.services(t) {
				for _, m := range svc.Mounts {
					for _, ref := range VarRefs(m.HostPathRaw) {
						if ref.FailClosed {
							continue
						}
						t.Errorf("service %q (%s) mounts the %q role from %s, which falls back to %q when %s is unset; write it ${%s:?message} so an unset variable stops the deployment instead of putting the backup root on the OS disk",
							svc.Name, svc.Source, m.Role, m.HostPathRaw, ref.Default, ref.Name, ref.Name)
					}
				}
			}
		})
	}
}

// TestForwardedHeaderTrustMatchesTheDeclaredTopology is the rule that was
// missing entirely: grepping this package for TRUST_FORWARDED_HEADERS
// returned nothing, while docs/deployment.md makes it the one environment
// variable with an explicit never-set-it-here rule. Setting it on the Web
// UI container, the internet-facing edge, collapses per-IP rate limiting
// on /api/v1/auth/login and /api/v1/auth/enroll and lets a client dictate
// the Secure-cookie decision. One assertion over Service.Environment
// covers all three metadata formats, in both directions.
func TestForwardedHeaderTrustMatchesTheDeclaredTopology(t *testing.T) {
	c := MustLoad()

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			platform := c.Platforms[p.name]
			if platform.TrustForwardedHeadersNote == "" {
				t.Error("canonical.json records no reason for this platform's forwarded-header decision; the decision is a security guarantee and belongs in the file a reviewer reads")
			}
			checked := 0
			for _, svc := range p.services(t) {
				edge := svc.Name == p.uiService
				if !edge && svc.Name != p.engineService {
					continue
				}
				checked++
				if v := CheckForwardedHeaderTrust(svc, edge, platform.TrustForwardedHeaders); len(v) > 0 {
					t.Errorf("service %q (%s):\n%s", svc.Name, svc.Source, format(v))
				}
			}
			if checked == 0 {
				t.Fatal("no service was checked; the rule would pass vacuously")
			}
		})
	}
}

// TestAcceptanceProceduresAreSafeAndVerifiable holds the operator
// procedures to the two rules that are decidable from their text: they may
// not recursively chown a tree they did not create, and they may not ask
// the operator to confirm the backup root is untouched byte for byte
// without ever recording anything to compare against.
// CheckAcceptanceProcedure's positive controls are in scan_test.go.
func TestAcceptanceProceduresAreSafeAndVerifiable(t *testing.T) {
	c := MustLoad()

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			if p.acceptance == "" {
				t.Fatal("no acceptance procedure declared for this platform")
			}
			path := filepath.Join(RepoRoot, "docs", "acceptance", p.acceptance)
			violations, err := ReadAcceptanceProcedure(path, c.Platforms[p.name].HostPaths.Backups, p.docSubstitutions)
			if err != nil {
				t.Fatalf("read acceptance procedure: %v", err)
			}
			if len(violations) > 0 {
				t.Errorf("docs/acceptance/%s:\n%s", p.acceptance, format(violations))
			}
		})
	}
}

// TestTheGateTableClaimsOnlyWhatIsChecked keeps docs/acceptance/README.md
// honest about which Phase 4 gate lines this package actually decides. It
// claimed hash parity, which nothing here measures.
func TestTheGateTableClaimsOnlyWhatIsChecked(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(RepoRoot, "docs", "acceptance", "README.md"))
	if err != nil {
		t.Fatalf("read acceptance README: %v", err)
	}
	text := string(data)

	if strings.Contains(text, "core version/hash parity") {
		t.Error("the gate table still folds hash parity into the version-parity row; nothing in distribution/packaging derives a hash from any artifact, so that row has to name what it measures")
	}
	if !strings.Contains(text, "#174") {
		t.Error("the gate table does not point at #174, which is where the unverified release manifest is tracked")
	}
	for _, want := range []string{"core version parity", "core binary hash parity"} {
		if !strings.Contains(text, want) {
			t.Errorf("the gate table has no %q row", want)
		}
	}
}

// ---------------------------------------------------------------------
// Platform-specific rules
// ---------------------------------------------------------------------

// TestOpenMediaVaultShipsNoNativePlugin enforces §4A's deferral and
// WP4.3's "Do NOT implement a native OMV plugin in v1", so scope creep
// shows up as a red test rather than as a review comment nobody makes.
func TestOpenMediaVaultShipsNoNativePlugin(t *testing.T) {
	findings, err := ScanForOMVPlugin(PlatformDir("openmediavault"))
	if err != nil {
		t.Fatalf("ScanForOMVPlugin: %v", err)
	}
	if len(findings) > 0 {
		t.Errorf("apps/openmediavault contains native-plugin material, which §4A defers:\n%s", format(findings))
	}
}

// TestUnraidWebUIJSONAgreesWithTheTemplate reconciles the two files that
// both describe Unraid's WebUI and storage layout:
// apps/unraid/frontend/webui.json (written by an earlier work package,
// imported by nothing) and the Docker template this work package adds.
// Two files stating the same fact is how a wrong fact survives, so they
// are pinned together.
func TestUnraidWebUIJSONAgreesWithTheTemplate(t *testing.T) {
	c := MustLoad()
	platform := c.Platforms["unraid"]

	data, err := os.ReadFile(filepath.Join(PlatformDir("unraid"), "frontend", "webui.json"))
	if err != nil {
		t.Fatalf("read webui.json: %v", err)
	}
	var meta struct {
		WebUI      string `json:"webui"`
		Appdata    string `json:"appdata"`
		BackupRoot string `json:"backupRoot"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse webui.json: %v", err)
	}

	tpl, err := ReadUnraidTemplate(filepath.Join(PlatformDir("unraid"), "template", "backup-manager-ui.xml"))
	if err != nil {
		t.Fatalf("read Unraid UI template: %v", err)
	}
	if tpl.WebUI != meta.WebUI {
		t.Errorf("the UI template's <WebUI> is %q but webui.json says %q", tpl.WebUI, meta.WebUI)
	}
	if meta.BackupRoot != platform.HostPaths.Backups {
		t.Errorf("webui.json's backupRoot is %q but canonical.json declares %q", meta.BackupRoot, platform.HostPaths.Backups)
	}
	if !Contains(meta.Appdata, platform.HostPaths.State) {
		t.Errorf("webui.json's appdata %q does not contain the declared state path %q", meta.Appdata, platform.HostPaths.State)
	}
}

// TestTrueNASCatalogQuestionsAndTemplateAgree checks the one thing about a
// TrueNAS catalog entry that is decidable here: every question the install
// wizard asks is consumed by the rendered template, and every value the
// template reads is asked for. TrueNAS's own catalog validator is the rest
// of the story, and it runs in
// docs/acceptance/truenas-provider-acceptance.md step 8.
func TestTrueNASCatalogQuestionsAndTemplateAgree(t *testing.T) {
	catalog := filepath.Join(PlatformDir("truenas"), "catalog")

	questions, err := TrueNASQuestionVariables(filepath.Join(catalog, "questions.yaml"))
	if err != nil {
		t.Fatalf("read questions.yaml: %v", err)
	}
	if len(questions) == 0 {
		t.Fatal("questions.yaml declares no variables; without this guard the comparisons below would pass vacuously")
	}

	template, err := os.ReadFile(filepath.Join(catalog, "templates", "docker-compose.yaml"))
	if err != nil {
		t.Fatalf("read catalog template: %v", err)
	}
	referenced := TrueNASTemplateVariables(string(template))
	if len(referenced) == 0 {
		t.Fatal("the catalog template reads no .Values at all; without this guard the comparisons below would pass vacuously")
	}

	ixValues := filepath.Join(catalog, "ix_values.yaml")

	for _, q := range questions {
		if !containsString(referenced, q) {
			t.Errorf("questions.yaml asks for %q but the catalog template never reads .Values.%s", q, q)
		}
		ok, err := LookupYAMLPath(ixValues, q)
		if err != nil {
			t.Fatalf("read ix_values.yaml: %v", err)
		}
		if !ok {
			t.Errorf("questions.yaml asks for %q but ix_values.yaml gives it no default", q)
		}
	}

	for _, ref := range referenced {
		if !containsString(questions, ref) {
			t.Errorf("the catalog template reads .Values.%s but no question in questions.yaml supplies it", ref)
		}
	}
}

// TestTrueNASCatalogDefaultsArePinnedToCanonical checks the values a
// catalog install renders with, not merely that a key exists for each
// question. LookupYAMLPath answers "there is a default here", which is
// what let ix_values.yaml hold any image reference, any host path and any
// port while the suite stayed green.
func TestTrueNASCatalogDefaultsArePinnedToCanonical(t *testing.T) {
	c := MustLoad()
	platform := c.Platforms["truenas"]
	ixValues := filepath.Join(PlatformDir("truenas"), "catalog", "ix_values.yaml")

	want := map[string]string{
		"image.reference":             c.Image.Reference,
		"storage.state.hostPath":      platform.HostPaths.State,
		"storage.backups.hostPath":    platform.HostPaths.Backups,
		"storage.config.hostPath":     platform.HostPaths.Config,
		"storage.sshKey.hostPath":     platform.HostPaths.SSHKey,
		"storage.knownHosts.hostPath": platform.HostPaths.KnownHosts,
		"network.webPort":             strconv.Itoa(c.ListenPort),
	}
	for dotted, expected := range want {
		got, ok, err := YAMLValue(ixValues, dotted)
		if err != nil {
			t.Fatalf("read ix_values.yaml: %v", err)
		}
		if !ok {
			t.Errorf("ix_values.yaml has no %s", dotted)
			continue
		}
		if got != expected {
			t.Errorf("ix_values.yaml sets %s to %q, want the canonical %q", dotted, got, expected)
		}
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func healthcheckMatches(test, want []string) bool {
	if len(test) == 0 {
		return false
	}
	joined := strings.Join(test, " ")
	for _, arg := range want {
		if !strings.Contains(joined, arg) {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/testenv"
)

// This file is issue #196's missing test, and the reason it was missing is
// the reason it matters.
//
// Every other fixture in this package writes config.yaml into a plain
// writable t.TempDir(). Packaging did not: TrueNAS, Unraid,
// OpenMediaVault and the canonical Compose deployment all bind-mounted
// config.yaml as a single READ-ONLY FILE into a container whose rootfs is
// itself read-only. Three merged features write that file —
// CreateBackupSet (#146), the settings write path (#140) and #176's
// first-run setup — and all three replace it through one temp-file-plus-
// rename, which has to create a sibling temp file in the file's own
// directory. On that mount shape the directory is the image's read-only
// rootfs, so every one of them failed at the write however correct the
// code above it was, and the whole suite stayed green because no fixture
// ever had that shape.
//
// The two write paths beside them were inert for the same reason:
// ImportSSHKey creates ssh_keys/ next to config.yaml, and the known-hosts
// store creates known_hosts.d/ there.
//
// So the shape is the fixture here, not an afterthought. Both shapes run
// through the same table: the read-only single-file mount packaging used
// to declare, and the writable directory it declares now.

// configMountShape is one way packaging can present the configuration to
// the container.
type configMountShape struct {
	name string
	// readOnly makes the configuration directory unwritable, which is
	// what a read-only single-file bind mount on a read-only rootfs
	// actually looks like from inside the container: the file cannot be
	// replaced and no sibling can be created next to it.
	readOnly bool
	// wantWritesToFail is what this shape means for every write path
	// that lands in the configuration directory.
	wantWritesToFail bool
}

var configMountShapes = []configMountShape{
	{
		// What canonical.json declared before #196:
		//   <host>/config/config.yaml:/etc/backup-manager/config.yaml:ro
		name:             "read-only single-file mount (pre-#196 packaging)",
		readOnly:         true,
		wantWritesToFail: true,
	},
	{
		// What canonical.json declares now:
		//   <host>/config:/etc/backup-manager/config
		name:             "writable directory mount (#196)",
		readOnly:         false,
		wantWritesToFail: false,
	},
}

// configMountFixture is a config directory laid out the way packaging
// mounts one, with everything the engine writes elsewhere kept OUTSIDE
// it: the state database and the backup destinations live on their own
// mounts in every packaged profile, so putting them in the same directory
// would make the read-only shape fail for the wrong reason.
type configMountFixture struct {
	configDir  string
	configPath string
	// keyID is an SSH key already present in ssh_keys/ before the
	// directory was sealed, so CreateBackupSet can be driven all the way
	// to its config write on the read-only shape instead of stopping at
	// ImportSSHKey.
	keyID string
}

func newConfigMountFixture(t *testing.T, shape configMountShape) configMountFixture {
	t.Helper()

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	stateDir := filepath.Join(root, "state")
	remoteDir := filepath.Join(root, "remote")
	localDir := filepath.Join(root, "local")
	for _, d := range []string{configDir, stateDir, remoteDir, localDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("config-mount fixture payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(configDir, config.DefaultFileName)
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(stateDir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}

	// Seed one key inside the directory before sealing it. On the
	// read-only shape ImportSSHKey cannot run at all, and without a key
	// already present CreateBackupSet would be refused for a missing
	// ssh_key_id rather than reaching the write this test is about.
	keysDir := filepath.Join(configDir, "ssh_keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatalf("MkdirAll ssh_keys: %v", err)
	}
	keyID := "seeded-fixture-key"
	if err := os.WriteFile(filepath.Join(keysDir, keyID), []byte(testFixtureEd25519Key), 0o600); err != nil {
		t.Fatalf("WriteFile seeded key: %v", err)
	}
	knownHostsDir := filepath.Join(configDir, "known_hosts.d")
	if err := os.MkdirAll(knownHostsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll known_hosts.d: %v", err)
	}

	if shape.readOnly {
		// Settle the environment question BEFORE sealing anything. Under
		// euid 0 the chmods below are accepted and then ignored, so the
		// shape this fixture exists to reproduce cannot be produced at
		// all, and the eight tests in this repository that used to answer
		// that with a t.Skip answered it invisibly: `go test` prints
		// nothing for a skip without -v, and scripts/ci-local.sh passes
		// no -v anywhere.
		testenv.RequirePermissionBitsApply(t)

		// The file itself AND every directory around it, because that is
		// what the mount produced: :ro on the bind, and the image's
		// read-only rootfs underneath it. ssh_keys/ and known_hosts.d/
		// are sealed for the same reason they are seeded at all: on the
		// real shape they could not exist, and leaving them writable here
		// would make this fixture more permissive than the deployment it
		// claims to reproduce.
		if err := os.Chmod(configPath, 0o444); err != nil {
			t.Fatalf("Chmod config file: %v", err)
		}
		sealed := []string{keysDir, knownHostsDir, configDir}
		for _, d := range sealed {
			if err := os.Chmod(d, 0o555); err != nil {
				t.Fatalf("Chmod %s: %v", d, err)
			}
		}
		// t.TempDir's own cleanup cannot remove a file out of a directory
		// it may not write to.
		t.Cleanup(func() {
			for _, d := range []string{configDir, keysDir, knownHostsDir} {
				_ = os.Chmod(d, 0o755)
			}
			_ = os.Chmod(configPath, 0o644)
		})
		for _, d := range sealed {
			requireDirectoryIsActuallySealed(t, d)
		}
	}

	return configMountFixture{configDir: configDir, configPath: configPath, keyID: keyID}
}

// requireDirectoryIsActuallySealed is the environment control. Permission
// bits mean nothing to a process running as root, and a suite running as
// root would otherwise report the read-only shape as "writes succeed",
// which reads as this whole issue being imaginary. So the shape is
// verified against the filesystem before anything is concluded from it.
//
// A successful probe is a FAILURE here and not a skip. The environment
// question is already settled by then: newConfigMountFixture calls
// testenv.RequirePermissionBitsApply before it seals anything, so by the
// time this runs the process is one whose writes permission bits do stop.
// A probe that still succeeds therefore means the fixture is not the shape
// it says it is, which is the one conclusion that must never be reported
// as "nothing to see here" — this is the single test carrying the claim
// that #196 made #140, #146 and #195 inert in every packaged container.
func requireDirectoryIsActuallySealed(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, ".write-probe")
	f, err := os.Create(probe)
	if err == nil {
		_ = f.Close()
		_ = os.Remove(probe)
		t.Fatalf("creating a file in %s succeeded at euid %d after it was sealed at 0555, so the read-only single-file mount shape was never reproduced and every assertion drawn from it is meaningless", dir, os.Geteuid())
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("sealing %s produced %v, want a permission error; the fixture is not the mount shape this test claims to exercise", dir, err)
	}
}

// openServiceOnFixture opens a real BackupService against the fixture.
// Open succeeds on both shapes on purpose: the engine READS its
// configuration perfectly well from a read-only mount, which is exactly
// why the defect survived. The failure is at the first write.
func openServiceOnFixture(t *testing.T, f configMountFixture) *BackupService {
	t.Helper()
	svc, cleanup, err := Open(context.Background(), f.configDir)
	if err != nil {
		t.Fatalf("Open against %s: %v", f.configDir, err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if svc.configPath != f.configPath {
		t.Fatalf("Open(%q) resolved configPath to %q, want %q: --config naming the packaged configuration directory has to resolve to the file inside it",
			f.configDir, svc.configPath, f.configPath)
	}
	return svc
}

// assertWriteOutcome is the one place the two shapes' expectations are
// spelled out, so a failing write is asserted for WHY it failed rather
// than merely that it did. "Permission denied somewhere" is not the
// claim; "permission denied inside the configuration directory" is.
func assertWriteOutcome(t *testing.T, shape configMountShape, f configMountFixture, what string, err error) {
	t.Helper()
	if !shape.wantWritesToFail {
		if err != nil {
			t.Errorf("%s failed on the writable directory mount: %v", what, err)
		}
		return
	}
	if err == nil {
		t.Errorf("%s succeeded against a read-only configuration mount; that is the shape three merged features were silently inert on, so a pass here means the fixture is not sealed", what)
		return
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("%s failed with %v, want a permission error: any other failure means this test is measuring something other than the mount shape", what, err)
	}
	if !strings.Contains(err.Error(), f.configDir) {
		t.Errorf("%s failed with %v, which never names the configuration directory %s; an operator reading this has no way to know which mount to change", what, err, f.configDir)
	}
}

// configWritePath is one of the three merged features #196 made inert,
// reduced to the call that lands in the configuration directory.
//
// They are a list rather than three inline subtests so the mutation
// control below drives EXACTLY the same calls as the table above it. A
// control that drives its own copy of the write paths is a control over a
// copy, and the copy is the thing that stays correct.
type configWritePath struct {
	name string
	run  func(t *testing.T, svc *BackupService, f configMountFixture) error
}

func configWritePaths() []configWritePath {
	return []configWritePath{
		{"settings write (#140)", func(t *testing.T, svc *BackupService, f configMountFixture) error {
			_, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
				Retention: &RetentionUpdate{Tiers: []RetentionTier{
					{Name: "daily", Granularity: GranularityDay, Keep: 7},
					{Name: "annual", Granularity: GranularityYear, Keep: 5},
				}},
			})
			return err
		}},
		{"create backup set (#146)", func(t *testing.T, svc *BackupService, f configMountFixture) error {
			_, err := svc.CreateBackupSet(context.Background(), CreateBackupSetRequest{
				Name:               "mount-shape-set",
				Host:               "example.internal",
				Port:               22,
				User:               "backup-agent",
				SSHKeyID:           f.keyID,
				KnownHostsLine:     "example.internal ssh-ed25519 AAAAtestfixtureline",
				RemotePath:         "/backups/mount-shape-set",
				LocalPath:          filepath.Join(t.TempDir(), "mount-shape-set"),
				Include:            []string{"*.dump"},
				CompletionStrategy: "marker",
			})
			return err
		}},
		{"import ssh key", func(t *testing.T, svc *BackupService, f configMountFixture) error {
			_, err := svc.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key))
			return err
		}},
	}
}

// TestTheConfigMountShapeDecidesWhetherEveryConfigWritePathWorks is #196's
// acceptance criterion: a test at the read-only-file mount shape that
// shows the failure it produces, with the writable-directory shape as its
// positive control in the same table.
func TestTheConfigMountShapeDecidesWhetherEveryConfigWritePathWorks(t *testing.T) {
	for _, shape := range configMountShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newConfigMountFixture(t, shape)
			svc := openServiceOnFixture(t, f)

			for _, w := range configWritePaths() {
				t.Run(w.name, func(t *testing.T) {
					assertWriteOutcome(t, shape, f, strings.SplitN(w.name, " (", 2)[0], w.run(t, svc, f))
				})
			}
		})
	}
}

// TestUnsealingOnlyTheConfigurationMountMakesEveryWritePathWorkAgain is the
// mutation control the read-only arm above never had, and the reason it is
// needed is worth stating plainly: this file's RED commit did not compile.
// It referenced config.DefaultFileName and config.ResolvePath, which the
// GREEN commit adds, so the recorded RED is a build failure and not one
// assertion in this file has been observed failing for its own reason.
//
// Reordering the stack would fix the record and nothing else. This fixes
// the evidence, which is the part that has to keep working: it takes the
// sealed fixture, changes ONE thing (the permission bits on the
// configuration mount, and nothing else anywhere), and requires all three
// write paths to go from failing to succeeding.
//
// Without it, "every write failed" on the sealed shape is compatible with
// the writes failing for a reason that has nothing to do with the mount —
// a fixture whose state database is unreachable, a validation refusal
// before the write is reached, a service that never opened properly. The
// assertion would still be green and #196's claim would still be unproven.
// With it, the configuration mount is the only variable in the experiment.
func TestUnsealingOnlyTheConfigurationMountMakesEveryWritePathWorkAgain(t *testing.T) {
	sealed := configMountShape{name: "read-only single-file mount (pre-#196 packaging)", readOnly: true, wantWritesToFail: true}
	f := newConfigMountFixture(t, sealed)
	svc := openServiceOnFixture(t, f)

	// First half: the failure, on exactly the calls the table drives.
	for _, w := range configWritePaths() {
		if err := w.run(t, svc, f); err == nil {
			t.Fatalf("%s succeeded against the sealed configuration mount, so there is no failure for this control to remove", w.name)
		} else if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("%s failed with %v, not a permission error; this control can only attribute a permission failure to the mount", w.name, err)
		}
	}

	// The mutation, and it is the only one: the configuration mount stops
	// being read-only. Nothing else about the fixture, the service or the
	// requests changes.
	for _, d := range []string{f.configDir, filepath.Join(f.configDir, "ssh_keys"), filepath.Join(f.configDir, "known_hosts.d")} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatalf("Chmod %s: %v", d, err)
		}
	}
	if err := os.Chmod(f.configPath, 0o644); err != nil {
		t.Fatalf("Chmod %s: %v", f.configPath, err)
	}

	// Second half: every one of them now works, against the same service,
	// with the same arguments.
	for _, w := range configWritePaths() {
		if err := w.run(t, svc, f); err != nil {
			t.Errorf("%s still failed after the configuration mount was made writable: %v. Either the failure above was not caused by the mount, or making the mount writable is not enough to fix it, and #196's fix rests on it being both", w.name, err)
		}
	}
}

// TestAnEmptyConfigDirectoryIsNotAConfigFile pins the other half of the
// shape change. A bind mount cannot express "not there yet" for a file —
// Docker creates a DIRECTORY at a source path that does not exist — so
// the state a fresh app-store install actually starts in was not even
// representable before #196. It is now: the mount is a directory, and an
// empty one is the honest "not configured yet".
//
// What the engine does with that is #176's (serve a first-run flow rather
// than refusing to start). What this test pins is that the packaging half
// reaches it: an empty configuration directory resolves to the config
// FILE inside it, missing, rather than to the directory itself, which is
// what would surface as an unreadable-file error naming a path no
// operator can act on.
func TestAnEmptyConfigDirectoryIsNotAConfigFile(t *testing.T) {
	dir := t.TempDir()

	if got, want := config.ResolvePath(dir), filepath.Join(dir, config.DefaultFileName); got != want {
		t.Errorf("ResolvePath(%q) = %q, want %q", dir, got, want)
	}

	_, err := config.Load(dir)
	if err == nil {
		t.Fatal("config.Load against an empty configuration directory returned no error")
	}
	if !strings.Contains(err.Error(), config.DefaultFileName) {
		t.Errorf("loading an empty configuration directory reported %v, which never names %s; the caller cannot tell an unconfigured install from a broken mount", err, config.DefaultFileName)
	}

	// Positive control: the same call against a path that is neither a
	// directory nor a file is returned unchanged, so ResolvePath is not
	// simply appending the file name to everything it is handed.
	missing := filepath.Join(dir, "not-there.yaml")
	if got := config.ResolvePath(missing); got != missing {
		t.Errorf("ResolvePath(%q) = %q, want it unchanged: a path the caller is about to create must not be turned into a directory path", missing, got)
	}
}

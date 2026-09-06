package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// Issue #555's two properties, driven rather than asserted in prose.
//
// The whole argument for a deployment identity is that a config_revision
// cannot do this job: it is a hash of configuration content, so it moves
// when a deployment is edited and it matches between two deployments built
// from one template. Both halves of that are what these tests are about,
// and they are worth driving rather than reasoning about, because a
// revision comparison is what the first draft of #555 was going to be.

// TestADeploymentKeepsItsIdentityWhileItsConfigurationMoves is the half a
// revision gets wrong in one direction: the same deployment, edited, is
// still the same deployment.
//
// Open the deployment, edit its configuration, open it again. The revision
// has to move, which is what proves the edit was real, and the identity has
// to stay exactly where it was. A guard built on the revision would refuse
// every routed write on a deployment somebody had changed since the engine
// started, which is most of them.
func TestADeploymentKeepsItsIdentityWhileItsConfigurationMoves(t *testing.T) {
	configPath := writeTestConfigFile(t)
	serveDeployment(t, configPath)

	svc, closeFn, err := Open(t.Context(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	identityBefore := svc.DeploymentID()
	revisionBefore := svc.ConfigRevision()
	if err := closeFn(); err != nil {
		t.Fatalf("closing the first open: %v", err)
	}
	if identityBefore == "" {
		t.Fatal("a deployment that has been opened reports no identity of its own, so nothing below is comparing anything")
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	edited := strings.Replace(string(raw), "poll_interval: 15m", "poll_interval: 30m", 1)
	if edited == string(raw) {
		t.Fatalf("the fixture's shape changed and this edit no longer changes anything:\n%s", raw)
	}
	if err := os.WriteFile(configPath, []byte(edited), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	svc, closeFn, err = Open(t.Context(), configPath)
	if err != nil {
		t.Fatalf("re-opening after the edit: %v", err)
	}
	defer func() {
		if err := closeFn(); err != nil {
			t.Errorf("closing the second open: %v", err)
		}
	}()

	if svc.ConfigRevision() == revisionBefore {
		t.Fatalf("the configuration revision is still %s after a real edit, so this test is not driving the case it claims to", revisionBefore)
	}
	if got := svc.DeploymentID(); got != identityBefore {
		t.Errorf("the deployment renamed itself when its configuration changed: was %s, now %s. An identity that moves with the configuration is a revision with extra steps, and a routed write would refuse on every deployment somebody had edited", identityBefore, got)
	}
}

// TestTwoDeploymentsBuiltFromOneTemplateDoNotShareAnIdentity is the half a
// revision gets wrong in the other direction, and it is the reason #555 is
// not closed by comparing revisions.
//
// The two configurations below are the same template with one path
// substituted, which is what a staging and a production instance on one
// host are. Their identities have to differ. On this host their revisions
// also differ, because a state database path is part of the configuration
// and two journals on one filesystem cannot share a path; in the shape
// this actually protects (two containers, each with its own volume mounted
// at the same place) the two configurations are byte-identical and the
// revisions are equal, which is why the identity is minted per journal and
// not derived from anything the configuration says.
func TestTwoDeploymentsBuiltFromOneTemplateDoNotShareAnIdentity(t *testing.T) {
	first := openFromTemplate(t)
	second := openFromTemplate(t)

	if first == "" || second == "" {
		t.Fatalf("a deployment reported no identity (%q, %q), so this comparison certifies nothing", first, second)
	}
	if first == second {
		t.Errorf("two deployments built from one template both call themselves %s, so nothing could tell a write aimed at one from a write aimed at the other", first)
	}
}

// openFromTemplate stands up one deployment from the shared fixture and
// reports the identity it minted for itself.
func openFromTemplate(t *testing.T) string {
	t.Helper()
	configPath := writeTestConfigFile(t)
	serveDeployment(t, configPath)
	svc, closeFn, err := Open(t.Context(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := closeFn(); err != nil {
			t.Errorf("closing: %v", err)
		}
	})
	return svc.DeploymentID()
}

// serveDeployment announces that this process serves the deployment
// configPath names, which is the act that mints its identity.
//
// It is a helper rather than a line in each test because it is the whole
// of #559's third finding: minting belongs to a process about to serve,
// so a test that wants a deployment WITH a name has to say that something
// serves it. A test that opens without announcing is modelling a CLI, and
// a CLI reads.
func serveDeployment(t *testing.T, configPath string) {
	t.Helper()
	release, err := AnnounceServing(configPath)
	if err != nil {
		t.Fatalf("announcing this process as serving %s: %v", configPath, err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("releasing the serving announcement: %v", err)
		}
	})
}

// TestAnIdentityIsMintedOnceAndNeverAgain is what makes the comparison a
// comparison at all.
//
// An identity re-minted on every start would be a fresh random number in
// the engine and a different fresh random number in the CLI, and every
// routed write would refuse. Two starts, one name.
func TestAnIdentityIsMintedOnceAndNeverAgain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	first, err := ensureDeploymentIdentity(dbPath)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	second, err := ensureDeploymentIdentity(dbPath)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if first != second {
		t.Errorf("this deployment called itself %s and then %s, so nothing that compared the two ends would ever agree", first, second)
	}

	read, err := DeploymentIdentity(dbPath)
	if err != nil {
		t.Fatalf("DeploymentIdentity: %v", err)
	}
	if read != first {
		t.Errorf("the reading half answered %s and the minting half answered %s; they are the two ends of one comparison and cannot differ", read, first)
	}
}

// TestReadingAnIdentityNeverMintsOne is the property the CLI's side of the
// check rests on.
//
// The command that reads this is about to hand a change to somebody else's
// process, and a process that is not serving a deployment has no business
// naming it. An identity invented on the reading side would be compared
// against the engine's and would refuse for a reason the reader created.
func TestReadingAnIdentityNeverMintsOne(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	got, err := DeploymentIdentity(dbPath)
	if err != nil {
		t.Fatalf("DeploymentIdentity on a journal nothing has opened: %v", err)
	}
	if got != "" {
		t.Errorf("a deployment nothing has ever opened answered %q; empty is the answer that means \"this cannot be confirmed\", and a name invented here would be compared against an engine's", got)
	}
	if _, err := os.Stat(dbPath + deploymentIDSuffix); !os.IsNotExist(err) {
		t.Errorf("reading an identity created %s (stat err = %v); the reading half writes nothing", dbPath+deploymentIDSuffix, err)
	}
}

// TestAnIdentityThatCannotBeUnderstoodIsRemintedRatherThanRefused covers
// the two ways a file can be there and say nothing: a crash between create
// and write, and somebody editing it by hand.
//
// Re-minted rather than refused, because refusing means a deployment that
// will not start, and what is lost by re-minting is a name nothing has
// stored a copy of.
func TestAnIdentityThatCannotBeUnderstoodIsRemintedRatherThanRefused(t *testing.T) {
	for _, junk := range []string{"", "  \n", "not-hex-at-all", "abc"} {
		t.Run("["+junk+"]", func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			if err := os.WriteFile(dbPath+deploymentIDSuffix, []byte(junk), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if got, err := DeploymentIdentity(dbPath); err != nil || got != "" {
				t.Fatalf("DeploymentIdentity over %q = (%q, %v), want (\"\", nil): an identity this build cannot read is absent, not an error", junk, got, err)
			}
			got, err := ensureDeploymentIdentity(dbPath)
			if err != nil {
				t.Fatalf("minting over %q: %v", junk, err)
			}
			if got == "" {
				t.Fatalf("minting over %q produced nothing", junk)
			}
			back, err := DeploymentIdentity(dbPath)
			if err != nil || back != got {
				t.Errorf("after re-minting over %q the file reads back as (%q, %v), want %q", junk, back, err, got)
			}
		})
	}
}

// TestAnIdentityIsNotDerivedFromTheConfigurationAtAll is the negative
// control for the two tests above, and it is the one that would catch a
// later refactor that quietly went back to hashing content.
//
// Two deployments whose configurations differ in every field this product
// has, and one deployment's identity compared against its own revision.
// The identity may never equal any revision: they are different facts and
// a build where one was computed from the other would pass every other
// test here.
func TestAnIdentityIsNotDerivedFromTheConfigurationAtAll(t *testing.T) {
	configPath := writeTestConfigFile(t)
	serveDeployment(t, configPath)
	svc, closeFn, err := Open(t.Context(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := closeFn(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()

	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	if svc.DeploymentID() == ConfigRevisionOf(cfg) {
		t.Errorf("this deployment's identity is its configuration revision (%s), so it is a content hash wearing a different name and two instances from one template would share it", svc.DeploymentID())
	}
}

// TestAnIdentityThatCannotBeReadDoesNotStopTheDeploymentStarting is
// #559's fourth finding, and it is a rule this package stated and did not
// follow.
//
// Open's own comment argues that a read failure here must not be fatal:
// "a process that refused to start because it could not name itself would
// trade a routed write that gets refused for a deployment that does not
// come up at all". The code above it did the opposite, because
// runStartupSequence minted inside the same Open and returned the error,
// so a `chmod 000` on one small file stopped the deployment, and a
// directory left at that path stopped it permanently. The three sibling
// lock files are all EACCES-tolerant at read time; this is now the same
// rule, and it is the code rather than the comment that carries it.
//
// Both shapes are driven, because they fail at different points: the file
// cannot be read, and the file is not a file at all.
func TestAnIdentityThatCannotBeReadDoesNotStopTheDeploymentStarting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, path string)
	}{
		{
			name: "unreadable file",
			arrange: func(t *testing.T, path string) {
				if os.Geteuid() == 0 {
					t.Skip("running as root, which reads a 0000 file anyway, so this arrangement is not the one under test")
				}
				if err := os.WriteFile(path, []byte("ab34cd78ef90ab34cd78ef90ab34cd78\n"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				if err := os.Chmod(path, 0); err != nil {
					t.Fatalf("Chmod: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
				if _, err := os.ReadFile(path); err == nil {
					t.Skip("this filesystem does not enforce permission bits, so the file is readable and the arrangement is not the one under test")
				}
			},
		},
		{
			name: "a directory sitting at that path",
			arrange: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatalf("Mkdir: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeTestConfigFile(t)
			cfg, err := config.LoadAndValidate(configPath)
			if err != nil {
				t.Fatalf("LoadAndValidate: %v", err)
			}
			tc.arrange(t, cfg.State.Database+deploymentIDSuffix)

			// The announcement is where a serving process mints, so it is
			// the first thing that meets this and the first thing that
			// could refuse to come up over it.
			release, err := AnnounceServing(configPath)
			if err != nil {
				t.Fatalf("announcing this process as serving a deployment whose identity cannot be read: %v, want nil. A deployment that will not start is a worse outcome than one that cannot name itself", err)
			}
			t.Cleanup(func() { _ = release() })

			svc, closeFn, err := Open(t.Context(), configPath)
			if err != nil {
				t.Fatalf("Open over a deployment whose identity cannot be read: %v, want nil. This is the case Open's own comment says must not be fatal", err)
			}
			t.Cleanup(func() {
				if err := closeFn(); err != nil {
					t.Errorf("closing: %v", err)
				}
			})
			if got := svc.DeploymentID(); got != "" {
				t.Errorf("this deployment reports the identity %q off a file it cannot read; empty is the honest answer and it is the one a client reads as \"cannot be confirmed\"", got)
			}
		})
	}
}

// TestOpeningADeploymentWithoutServingItNeverNamesIt is #559's third
// finding at this level: the mint belongs to a process about to serve,
// and to nothing else.
//
// Open is the constructor the CLI comes through as well as the web host
// (openBackupService and openConfigWriteRoute both call it), so a mint
// inside it renames a deployment on a `backup-manager status`. That is
// not a hypothetical: with the engine up and holding a cached identity
// and the file absent, which is what restoring only the .db leaves, one
// CLI invocation renamed the deployment and every routed write afterwards
// refused against its own engine.
func TestOpeningADeploymentWithoutServingItNeverNamesIt(t *testing.T) {
	configPath := writeTestConfigFile(t)
	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}

	svc, closeFn, err := Open(t.Context(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := closeFn(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()

	if got := svc.DeploymentID(); got != "" {
		t.Errorf("opening a deployment nothing is serving named it %s; the near side of the routed-write check is read and never minted, and a name invented here is compared against the engine's", got)
	}
	if _, err := os.Stat(cfg.State.Database + deploymentIDSuffix); !os.IsNotExist(err) {
		t.Errorf("opening a deployment nothing is serving created %s (stat err = %v); one CLI command doing this renames a deployment out from under the engine still serving it", cfg.State.Database+deploymentIDSuffix, err)
	}
}

// TestAnInterruptedMintLeavesNothingBehind covers the file a process
// killed between creating the temporary and renaming it into place leaves
// in the state directory.
//
// Nothing swept them, so they accumulated one per hard kill, forever, in
// the directory an operator is meant to be able to look at and understand.
func TestAnInterruptedMintLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Two, spelled the two ways this build and the one before it name a
	// temporary, because a sweep that only knew the current spelling would
	// leave every file the previous build abandoned.
	leftovers := []string{
		dbPath + deploymentIDSuffix + ".restore-2417483096",
		dbPath + deploymentIDSuffix + ".1907622381",
	}
	for _, path := range leftovers {
		if err := os.WriteFile(path, []byte("half a mint\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	// A neighbour that is not one of ours, so the sweep is shown to be
	// about this journal's leftovers rather than about the directory.
	neighbour := filepath.Join(dir, "other.db"+deploymentIDSuffix+".restore-11")
	if err := os.WriteFile(neighbour, []byte("not mine\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ensureDeploymentIdentity(dbPath); err != nil {
		t.Fatalf("minting: %v", err)
	}
	for _, path := range leftovers {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is still there after a mint (stat err = %v); nothing else ever removes one, so it stays for the life of the deployment", path, err)
		}
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Errorf("the sweep removed %s, which belongs to a different journal in the same directory: %v", neighbour, err)
	}
}

// TestAnIdentityPathThatIsNotAFileReadsAsAbsent covers the two things
// that can sit at that path and are not an identity.
//
// A symlink is not followed. Nothing this product ships puts one there,
// the reader may be running as a different user from the one that owns
// the state directory, and following one is how "read the identity beside
// the journal" becomes "read whatever somebody pointed this at".
//
// A file longer than any identity this build writes is not read past its
// bound and does not count, so a path pointed at something enormous costs
// a short read and answers honestly.
func TestAnIdentityPathThatIsNotAFileReadsAsAbsent(t *testing.T) {
	valid := "ab34cd78ef90ab34cd78ef90ab34cd78"

	t.Run("a symlink is not followed", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "state.db")
		elsewhere := filepath.Join(dir, "somebody-elses-identity")
		if err := os.WriteFile(elsewhere, []byte(valid+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Symlink(elsewhere, dbPath+deploymentIDSuffix); err != nil {
			t.Skipf("this filesystem does not do symlinks (%v), so the arrangement is not the one under test", err)
		}
		got, err := DeploymentIdentity(dbPath)
		if err != nil {
			t.Fatalf("DeploymentIdentity over a symlink: %v", err)
		}
		if got != "" {
			t.Errorf("DeploymentIdentity followed a symlink and answered %q; the identity is the file beside the journal, not whatever a link points at", got)
		}
	})

	t.Run("a file longer than an identity", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "state.db")
		padded := valid + "\n" + strings.Repeat("x", maxDeploymentIDFileBytes*4)
		if err := os.WriteFile(dbPath+deploymentIDSuffix, []byte(padded), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := DeploymentIdentity(dbPath)
		if err != nil {
			t.Fatalf("DeploymentIdentity over an over-long file: %v", err)
		}
		if got != "" {
			t.Errorf("DeploymentIdentity answered %q off a file that is not one this build wrote; reading the first 32 characters of an arbitrary file as a deployment's name is how two deployments end up sharing one", got)
		}
	})
}

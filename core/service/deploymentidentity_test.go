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
	svc, closeFn, err := Open(t.Context(), writeTestConfigFile(t))
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

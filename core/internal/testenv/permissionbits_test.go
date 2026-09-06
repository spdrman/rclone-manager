package testenv_test

import (
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/testenv"
)

// This suite has to prove a policy about an environment it is not running
// in.
//
// The whole point of the package is what happens at euid 0, and no
// developer machine and no correctly configured CI job runs the suite
// there, so the branch that matters is the branch that never executes.
// That is why Decide is a pure function of its two inputs rather than
// something that reads os.Geteuid itself: the table below drives every
// branch, root included, on any machine, with no privileges and no
// container.
//
// A table over a pure function can also drift away from the process it is
// meant to describe, so the last test goes the other way and seals a real
// directory in a real temp dir. It is the only assertion here about the
// machine actually running the suite, and without it the two above would
// keep passing on a host where the real helper refuses everything.

// TestRootIsARefusalRatherThanASkip is the positive control for a policy
// that is otherwise only observable by re-running the whole suite as root.
//
// Decide is a pure function of euid and the opt-out precisely so this can
// exist: every branch of the policy is driven here, including the one no
// developer machine ever reaches, and the refusal branch is asserted to be
// a refusal rather than a skip.
func TestRootIsARefusalRatherThanASkip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		euid   int
		optOut string
		want   testenv.Decision
	}{
		{"an ordinary user", 501, "", testenv.PermissionBitsApply},
		{"an ordinary user who set the opt-out anyway", 501, "1", testenv.PermissionBitsApply},
		{"root, the case eight tests used to skip silently", 0, "", testenv.RefuseAsRoot},
		{"root with the opt-out typed on purpose", 0, "1", testenv.SkipByExplicitOptOut},
		{"root with the opt-out set to something else", 0, "true", testenv.RefuseAsRoot},
		{"root with the opt-out set to nothing", 0, "0", testenv.RefuseAsRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := testenv.Decide(tc.euid, tc.optOut); got != tc.want {
				t.Errorf("Decide(%d, %q) = %q, want %q", tc.euid, tc.optOut, got, tc.want)
			}
		})
	}
}

// TestTheRefusalSaysHowToRunTheSuiteProperly stops the refusal from being a
// dead end. Somebody meets this message inside a rootful container, and the
// only useful failure is one that names both ways out of it.
func TestTheRefusalSaysHowToRunTheSuiteProperly(t *testing.T) {
	msg := testenv.RefusalMessage(0)
	for _, want := range []string{"euid 0", "--user", testenv.AllowRootPermissionTestsEnv} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal never mentions %q, so a reader is told what went wrong and not what to do:\n%s", want, msg)
		}
	}
}

// TestThisSuiteItselfIsRunningWherePermissionBitsApply is the control on the
// control. Everything above drives Decide with a fabricated euid, so it
// would keep passing on a machine where the real helper refuses everything.
// This is the one assertion about the process actually running the suite.
func TestThisSuiteItselfIsRunningWherePermissionBitsApply(t *testing.T) {
	testenv.RequirePermissionBitsApply(t)

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if f, err := os.Create(dir + "/probe"); err == nil {
		_ = f.Close()
		t.Fatalf("a directory sealed at 0555 accepted a new file at euid %d, so this environment does not enforce permission bits and RequirePermissionBitsApply did not notice", os.Geteuid())
	}
}

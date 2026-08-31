// Package testenv holds the environment controls a test has to satisfy
// before it is allowed to conclude anything from the filesystem.
//
// It exists because of one recurring shape. A test that seals a directory
// and then asserts a write fails is only a test while permission bits are
// enforced against the process running it. Under euid 0 they are not: the
// seal is ignored, the write succeeds, and the assertion the whole test
// exists for either never runs or runs backwards. Eight tests across three
// packages each answered that on their own with a t.Skip, and a Go skip
// prints nothing without -v, which scripts/ci-local.sh does not pass. So a
// root run (a rootful container, a CI image with no --user, a rootful build
// agent, plain `docker run`) silently retired eight controls at once and
// still reported ok.
//
// The default here is the other way round: root is a refusal, everywhere,
// and the opt-out is explicit and has to be typed. That is deliberately
// louder than the alternative of keying the refusal on a CI marker, which
// only fails where the marker happens to be set and is silent exactly where
// nobody is watching.
package testenv

import (
	"fmt"
	"os"
	"testing"
)

// AllowRootPermissionTestsEnv is the one opt-out. Setting it to "1" says
// "I know permission bits do not apply to this process and I want the
// affected controls skipped anyway", which is a statement someone has to
// make on purpose rather than a property of the machine.
const AllowRootPermissionTestsEnv = "ALLOW_ROOT_PERMISSION_TESTS"

// Decision is what a permission-bit-dependent test may do in a given
// environment. It is a value rather than a branch inside the helper so the
// policy can be driven directly by a test: a control that can only be
// exercised by re-running the suite as root is a control nobody runs.
type Decision string

const (
	// PermissionBitsApply: an ordinary unprivileged process. Seal the
	// fixture and assert.
	PermissionBitsApply Decision = "permission-bits-apply"
	// RefuseAsRoot: euid 0 with no opt-out. The test cannot produce the
	// shape it claims to test, and saying so quietly is what this package
	// exists to stop.
	RefuseAsRoot Decision = "refuse-as-root"
	// SkipByExplicitOptOut: euid 0, and somebody typed the opt-out.
	SkipByExplicitOptOut Decision = "skip-by-explicit-opt-out"
)

// Decide is the whole policy, as a function of the two inputs it has.
func Decide(euid int, optOut string) Decision {
	if euid != 0 {
		return PermissionBitsApply
	}
	if optOut == "1" {
		return SkipByExplicitOptOut
	}
	return RefuseAsRoot
}

// RefusalMessage is what a refusing run prints. Exported so the test of
// this package can assert the message says what to do, rather than only
// that some failure happened.
func RefusalMessage(euid int) string {
	return fmt.Sprintf(
		"this test asserts that a filesystem permission is ENFORCED, and it is running as euid %d, where permission bits are not enforced at all. "+
			"The seal it plants would be ignored, the write it expects to fail would succeed, and the control would report a pass it never earned. "+
			"Run the suite as an unprivileged user (in a container: --user $(id -u):$(id -g)), or set %s=1 to skip these controls deliberately and accept that this run proves nothing about them.",
		euid, AllowRootPermissionTestsEnv)
}

// RequirePermissionBitsApply is the one call every permission-bit-dependent
// test makes before it seals anything. It fails the test under root rather
// than skipping it, so a run that cannot enforce permissions is loud once
// per test instead of silent everywhere.
func RequirePermissionBitsApply(t *testing.T) {
	t.Helper()
	euid := os.Geteuid()
	switch Decide(euid, os.Getenv(AllowRootPermissionTestsEnv)) {
	case PermissionBitsApply:
		return
	case SkipByExplicitOptOut:
		t.Skipf("running as euid %d with %s=1: permission bits are not enforced, so this control is skipped by request and proves nothing", euid, AllowRootPermissionTestsEnv)
	default:
		t.Fatal(RefusalMessage(euid))
	}
}

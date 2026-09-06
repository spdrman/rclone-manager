package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #542, Phase 2 of #536: the mode an invocation ran in is decided
// once, carried, and said out loud, and a mode that cannot be carried out
// is a refusal rather than a quiet downgrade to a direct write.
//
// # Why every case here runs twice
//
// The same shape liveengine_test.go uses, for the same reason. A test
// that only proves the engine-attached announcement cannot tell a
// detector that works from one that answers "engine" whatever the world
// looks like, and a CLI that decided it was engine-attached on a bare
// host would have no way to do anything at all. So every announcement is
// asserted in both directions: with a real engine process holding the
// journal, and with nothing running, against the identical argv.
//
// Both arms also assert the announcement the other mode would have
// printed is ABSENT. That is the half that catches the failure #542 names
// -- a command that quietly falls back to direct and reports a success
// the engine will never see -- because a fallback that also announced
// itself as engine-attached would satisfy a one-sided assertion.
//
// # And the fixture is two processes, not two function calls
//
// startEngineHolding (liveengine_test.go) re-executes this test binary as
// `daemon`, so the journal is held by a real process running the same
// dispatch the shipped binary runs. Nothing below simulates an engine.

// directLine and engineAttachedLine are the two announcements an operator
// greps for, built from the product's own constants so a reworded mode
// cannot leave these assertions passing against a line nobody prints.
var (
	directLine         = modeLinePrefix + string(directMode)
	engineAttachedLine = modeLinePrefix + string(engineAttachedMode)
)

// runCapturingBothStreams runs one invocation and gives back its exit
// status and everything it printed, stdout and stderr folded into one
// string.
//
// Folded deliberately: the claim being checked is what an operator sees
// in a terminal, and a terminal does not separate the two. It also keeps
// the assertions honest about the one asymmetry this change introduces on
// purpose -- a direct run announces itself on stdout beside the command's
// own report, an engine-attached refusal announces itself on stderr
// beside the refusal -- without either arm having to know which.
func runCapturingBothStreams(t *testing.T, args []string) (int, string) {
	t.Helper()
	var (
		code int
		out  string
	)
	stderr := captureStderr(t, func() {
		out = captureStdout(t, func() { code = run(args) })
	})
	return code, out + stderr
}

// TestEveryConfigurationWriteSaysWhichModeItRan is #542's acceptance:
// both modes announce themselves, on every command that rewrites
// config.yaml, and neither is ever announced in the other's place.
func TestEveryConfigurationWriteSaysWhichModeItRan(t *testing.T) {
	t.Run("with an engine holding the deployment", func(t *testing.T) {
		for _, m := range configMutations {
			t.Run(m.name, func(t *testing.T) {
				configPath := writeTestConfigWithDeploymentPolicy(t)
				keyPath := writeTestPrivateKey(t)
				before := readFile(t, configPath)

				stop := startEngineHolding(t, configPath)
				defer stop()

				code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

				if !strings.Contains(out, engineAttachedLine) {
					t.Errorf("%s never said which mode it ran in; an operator cannot tell whether this reached the engine\nwant a line containing %q, got:\n%s", m.name, engineAttachedLine, out)
				}
				if strings.Contains(out, directLine) {
					t.Errorf("%s reported %q while another process was holding this deployment; that is the #535 defect with a label on it\n%s", m.name, directLine, out)
				}
				if code == 0 {
					t.Errorf("%s exited 0 against a running engine, so a change the engine never sees was reported as a success\n%s", m.name, out)
				}
				if after := readFile(t, configPath); after != before {
					t.Errorf("%s changed config.yaml behind a running engine\nbefore:\n%s\nafter:\n%s", m.name, before, after)
				}
			})
		}
	})

	t.Run("with nothing running", func(t *testing.T) {
		for _, m := range configMutations {
			t.Run(m.name, func(t *testing.T) {
				configPath := writeTestConfigWithDeploymentPolicy(t)
				keyPath := writeTestPrivateKey(t)
				before := readFile(t, configPath)

				code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

				if code != 0 {
					t.Fatalf("%s exited %d with nothing running, want 0\n%s", m.name, code, out)
				}
				if !strings.Contains(out, directLine) {
					t.Errorf("%s wrote the configuration itself and did not say so; want a line containing %q, got:\n%s", m.name, directLine, out)
				}
				if strings.Contains(out, engineAttachedLine) {
					t.Errorf("%s claimed %q on a host with no engine on it\n%s", m.name, engineAttachedLine, out)
				}
				if after := readFile(t, configPath); after == before {
					t.Errorf("%s exited 0 with nothing running but left config.yaml unchanged, so this arm proves nothing about the write it announced", m.name)
				}
			})
		}
	})
}

// TestTheFirstConfigurationWriteSaysItWroteDirectly covers the one
// configuration write that does not go through openBackupService.
//
// `backup-set create` against an install with no config.yaml takes
// createFirstConfig (issue #176: the installer leaves the directory
// empty), which builds a service.FirstRun rather than opening a
// BackupService. It is still this binary writing a deployment's
// configuration, so it still has a mode, and leaving it out would put a
// configuration write into the tree that announces nothing.
func TestTheFirstConfigurationWriteSaysItWroteDirectly(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	keyPath := writeTestPrivateKey(t)

	args := createArgs(configPath, keyPath, "api/postgres", "--state-database", filepath.Join(dir, "state.db"))
	code, out := runCapturingBothStreams(t, args)

	if code != 0 {
		t.Fatalf("run(%v) = %d, want 0\n%s", args, code, out)
	}
	if !strings.Contains(out, directLine) {
		t.Errorf("the first configuration was written without saying which mode wrote it; want a line containing %q, got:\n%s", directLine, out)
	}
}

// TestAModeThatCannotBeDecidedIsRefusedRatherThanAssumedDirect is the
// "briefly unreachable" half of #542, in the only form this build can
// actually produce: a probe that fails.
//
// The detection reads the journal's own advisory lock file, so a lock
// file this process cannot open is a real, unsimulated probe failure,
// which is why the fixture makes one rather than swapping the detector
// out. What must not happen is the thing #537's own doc warns about:
// "I could not tell" quietly becoming "nothing is running", which is the
// same write, reported the same way, as the one #535 recorded.
//
// Root can open a mode-000 file, so the case is skipped there rather than
// left to pass without having tested anything.
func TestAModeThatCannotBeDecidedIsRefusedRatherThanAssumedDirect(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can open a mode-000 lock file, so this fixture cannot produce a failed probe")
	}
	for _, m := range configMutations {
		t.Run(m.name, func(t *testing.T) {
			configPath := writeTestConfigWithDeploymentPolicy(t)
			keyPath := writeTestPrivateKey(t)
			before := readFile(t, configPath)

			// The journal's lock file, spelled the way core/service
			// spells it (startup.go's journalLockSuffix, beside the
			// state.db writeTestConfig names). Unexported over there, so
			// this is the same literal coupling liveengine_test.go
			// already has to "state.db": a rename on that side lands here
			// as a fixture that stops producing a failed probe, which is
			// a red test rather than a quiet one, because the assertions
			// below need the refusal.
			lockPath := filepath.Join(filepath.Dir(configPath), "state.db.journal-lock")
			if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
				t.Fatalf("WriteFile %s: %v", lockPath, err)
			}
			if err := os.Chmod(lockPath, 0o000); err != nil {
				t.Fatalf("Chmod %s: %v", lockPath, err)
			}
			// Put back before the framework removes the directory, so a
			// deliberately unreadable fixture cannot become a cleanup
			// failure on some other platform's rules.
			t.Cleanup(func() { _ = os.Chmod(lockPath, 0o600) })

			code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

			if code == 0 {
				t.Errorf("%s exited 0 while the detection could not be performed at all\n%s", m.name, out)
			}
			if strings.Contains(out, directLine) {
				t.Errorf("%s reported %q after a probe that could not answer; a mode that cannot be decided is a refusal, not a default\n%s", m.name, directLine, out)
			}
			if after := readFile(t, configPath); after != before {
				t.Errorf("%s wrote config.yaml after a probe that could not answer\nbefore:\n%s\nafter:\n%s", m.name, before, after)
			}
		})
	}
}

// TestTheModeIsDecidedOncePerInvocation is #542's first clause held to
// the kernel rather than to a reading of the code: however many places in
// one invocation want to know which mode this is, the detection runs
// exactly once and everything downstream is handed that one answer.
//
// It matters beyond tidiness. A second probe is a second question asked
// of a world that can have changed between the two, so an engine that
// exits in the gap turns a refusal into a direct write, which is the
// downgrade this issue exists to make impossible. Counting the probes is
// the only way to tell a decision that is carried from one that happens
// to agree with itself today.
func TestTheModeIsDecidedOncePerInvocation(t *testing.T) {
	countProbes := func(t *testing.T) *int {
		t.Helper()
		calls := 0
		real := detectRunningEngine
		detectRunningEngine = func(configPath string) (*service.RunningEngine, error) {
			calls++
			return real(configPath)
		}
		t.Cleanup(func() { detectRunningEngine = real })
		return &calls
	}

	for _, m := range configMutations {
		t.Run(m.name, func(t *testing.T) {
			configPath := writeTestConfigWithDeploymentPolicy(t)
			keyPath := writeTestPrivateKey(t)

			calls := countProbes(t)
			code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

			if code != 0 {
				t.Fatalf("%s exited %d with nothing running, want 0\n%s", m.name, code, out)
			}
			if *calls != 1 {
				t.Errorf("%s asked whether an engine is running %d times; the mode is one decision per invocation, and a second ask is a second answer that can disagree with the first", m.name, *calls)
			}
		})
	}

	t.Run("backup-set create, writing the first configuration", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.yaml")
		keyPath := writeTestPrivateKey(t)

		calls := countProbes(t)
		args := createArgs(configPath, keyPath, "api/postgres", "--state-database", filepath.Join(dir, "state.db"))
		code, out := runCapturingBothStreams(t, args)

		if code != 0 {
			t.Fatalf("run(%v) = %d, want 0\n%s", args, code, out)
		}
		if *calls != 1 {
			t.Errorf("writing the first configuration asked whether an engine is running %d times, want 1", *calls)
		}
	})
}

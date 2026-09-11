package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/service"
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
// asserted in both directions: with a real engine process serving the
// deployment, and with nothing serving it, against the identical argv.
//
// Both arms also assert the announcement the other mode would have
// printed is ABSENT. That is the half that catches the failure #542 names
// (a command that quietly falls back to direct and reports a success the
// engine will never see) because a fallback that also announced itself as
// engine-attached would satisfy a one-sided assertion.
//
// # And the fixture is two processes, not two function calls
//
// startEngineHolding (liveengine_test.go) re-executes this test binary as
// `daemon`, which announces itself as serving before it opens anything,
// so the thing these announcements are about is a real process running
// the same dispatch the shipped binary runs. Nothing below simulates an
// engine.
//
// # What the announcement may claim
//
// "Serving", not "holding the journal open", and that distinction is the
// whole of what changed underneath this file. The detector this was first
// written against read the shared journal lock, which every `status`,
// every `sources` and every cron `run` takes, so `mode: engine-attached`
// fired for a plain reader. It now reads a lock only a serving process
// takes, and TestAPlainJournalReaderIsNeverAnnouncedAsAnEngine is what
// holds the announcement to that.

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
// purpose (a direct write announces itself on stdout beside the command's
// own report, an engine-attached refusal announces itself on stderr
// beside the refusal) without either arm having to know which.
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
	t.Run("with an engine serving the deployment", func(t *testing.T) {
		for _, m := range configMutations {
			t.Run(m.name, func(t *testing.T) {
				configPath := writeTestConfigWithDeploymentPolicy(t)
				keyPath := writeTestPrivateKey(t)
				if m.prepare != nil {
					m.prepare(t, configPath)
				}
				before := readFile(t, configPath)

				stop := startEngineHolding(t, configPath)
				defer stop()

				code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

				if !strings.Contains(out, engineAttachedLine) {
					t.Errorf("%s never said which mode it ran in; an operator cannot tell whether this reached the engine\nwant a line containing %q, got:\n%s", m.name, engineAttachedLine, out)
				}
				if strings.Contains(out, directLine) {
					t.Errorf("%s reported %q while another process was serving this deployment; that is the #535 defect with a label on it\n%s", m.name, directLine, out)
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

	t.Run("with nothing serving it", func(t *testing.T) {
		for _, m := range configMutations {
			t.Run(m.name, func(t *testing.T) {
				configPath := writeTestConfigWithDeploymentPolicy(t)
				keyPath := writeTestPrivateKey(t)
				if m.prepare != nil {
					m.prepare(t, configPath)
				}
				before := readFile(t, configPath)

				code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

				if code != 0 {
					t.Fatalf("%s exited %d with nothing serving this deployment, want 0\n%s", m.name, code, out)
				}
				if !strings.Contains(out, directLine) {
					t.Errorf("%s wrote the configuration itself and did not say so; want a line containing %q, got:\n%s", m.name, directLine, out)
				}
				if strings.Contains(out, engineAttachedLine) {
					t.Errorf("%s claimed %q on a host with no engine on it\n%s", m.name, engineAttachedLine, out)
				}
				if after := readFile(t, configPath); after == before {
					t.Errorf("%s exited 0 with nothing serving this deployment but left config.yaml unchanged, so this arm proves nothing about the write it announced", m.name)
				}
			})
		}
	})
}

// TestTheFirstConfigurationWriteSaysWhichModeItRan covers the one
// configuration write that does not go through openBackupService.
//
// `backup-set create` against an install with no config.yaml takes
// createFirstConfig (issue #176: the installer leaves the directory
// empty), which builds a service.FirstRun rather than opening a
// BackupService. It is still this binary writing a deployment's
// configuration, so it still has a mode, and leaving it out would put a
// configuration write into the tree that announces nothing.
//
// Both arms again, and here the second one is not a formality: what this
// path decides from is the journal --state-database names rather than a
// configuration file that is not there, so the engine-attached arm is a
// mistyped --config against a live deployment and the direct arm is a
// genuine bare host.
func TestTheFirstConfigurationWriteSaysWhichModeItRan(t *testing.T) {
	t.Run("with an engine serving that journal", func(t *testing.T) {
		configPath := writeTestConfigWithDeploymentPolicy(t)
		keyPath := writeTestPrivateKey(t)
		dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

		stop := startEngineHolding(t, configPath)
		defer stop()

		// The path the operator meant to type, one letter out.
		mistyped := filepath.Join(filepath.Dir(configPath), "confg.yaml")
		args := createArgs(mistyped, keyPath, "api/postgres", "--state-database", dbPath)
		code, out := runCapturingBothStreams(t, args)

		if !strings.Contains(out, engineAttachedLine) {
			t.Errorf("writing a first configuration beside a live engine never said which mode it ran in; want a line containing %q, got:\n%s", engineAttachedLine, out)
		}
		if strings.Contains(out, directLine) {
			t.Errorf("writing a first configuration reported %q while an engine serves %s\n%s", directLine, dbPath, out)
		}
		if code == 0 {
			t.Errorf("backup-set create exited 0 writing a first configuration at %s while an engine serves %s\n%s", mistyped, dbPath, out)
		}
		if _, err := os.Stat(mistyped); !os.IsNotExist(err) {
			t.Errorf("a first configuration was written at %s while an engine serves %s (stat err = %v)", mistyped, dbPath, err)
		}
	})

	t.Run("with nothing serving that journal", func(t *testing.T) {
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
		if strings.Contains(out, engineAttachedLine) {
			t.Errorf("writing a first configuration on a bare host claimed %q\n%s", engineAttachedLine, out)
		}
	})
}

// TestAPlainJournalReaderIsNeverAnnouncedAsAnEngine is the review finding
// that changed what this announcement means, kept from coming back.
//
// The detection this was first built on read the SHARED journal lock,
// which is taken by every process that opens the journal at all: a
// `backupd status` an operator left in another terminal, a
// `sources`, a cron `run` for the length of a whole backup cycle.
// lock_unix.go's own doc calls that ordinary use of this CLI. So every
// configuration write on such a host printed `mode: engine-attached` and
// refused, and the announcement made that wrong answer the
// operator-visible face of the tool: not a bad message, a bad contract.
//
// The holder below is exactly that shape, and nothing more: the journal
// is opened and held, and nothing announces itself as serving anything.
// The mode has to come out direct, and the write has to land, or the CLI
// is stranded on a host with no engine on it.
func TestAPlainJournalReaderIsNeverAnnouncedAsAnEngine(t *testing.T) {
	for _, m := range configMutations {
		t.Run(m.name, func(t *testing.T) {
			configPath := writeTestConfigWithDeploymentPolicy(t)
			keyPath := writeTestPrivateKey(t)
			if m.prepare != nil {
				m.prepare(t, configPath)
			}
			before := readFile(t, configPath)

			// A `status` or a cron `run`, in as few lines as the thing
			// can be built from: open the journal, hold it, do nothing
			// else. This is core/service's own door, so what it takes is
			// what every reading command takes.
			_, journal, releaseJournal, err := service.OpenConfigAndJournal(context.Background(), configPath)
			if err != nil {
				t.Fatalf("opening the journal as a plain reader would: %v", err)
			}
			defer func() {
				_ = journal.Close()
				_ = releaseJournal()
			}()

			code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

			if strings.Contains(out, engineAttachedLine) {
				t.Errorf("%s announced %q because another process merely had the journal open; a reader is not an engine, and this strands the CLI on a host with nothing serving it\n%s", m.name, engineAttachedLine, out)
			}
			if !strings.Contains(out, directLine) {
				t.Errorf("%s did not announce %q beside a plain journal reader; want it, got:\n%s", m.name, directLine, out)
			}
			if code != 0 {
				t.Errorf("%s exited %d beside a plain journal reader, want 0\n%s", m.name, code, out)
			}
			if after := readFile(t, configPath); after == before {
				t.Errorf("%s left config.yaml unchanged beside a plain journal reader, so the direct mode it announced did nothing", m.name)
			}
		})
	}
}

// TestAModeThatCannotBeDecidedIsRefusedRatherThanAssumedDirect is the
// "briefly unreachable" half of #542, in the only form this build can
// actually produce: a probe that fails.
//
// What must not happen is the thing core/service's own doc warns about:
// "I could not tell" quietly becoming "nothing is running", which is the
// same write, reported the same way, as the one #535 recorded. #538's
// table already asserts that such a command does not exit 0 and does not
// change the file. What is asserted here is the half that is #542's: it
// must not tell the operator it wrote directly, because a command that
// says "direct" when it could not tell has lied whether or not it also
// wrote.
//
// The failure is a real one rather than an injected seam: a serving-lock
// path that is a symlink to itself, so opening it is ELOOP for any uid,
// on any filesystem, with no privileges needed to arrange it.
func TestAModeThatCannotBeDecidedIsRefusedRatherThanAssumedDirect(t *testing.T) {
	for _, m := range configMutations {
		t.Run(m.name, func(t *testing.T) {
			configPath := writeTestConfigWithDeploymentPolicy(t)
			keyPath := writeTestPrivateKey(t)
			if m.prepare != nil {
				m.prepare(t, configPath)
			}
			before := readFile(t, configPath)

			// The lock the decision reads, spelled the way core/service
			// spells it (startup.go's servingLockSuffix, beside the
			// state.db writeTestConfig names). Unexported over there, so
			// this is the same literal coupling liveengine_test.go
			// already has: a rename on that side lands here as a fixture
			// that stops producing a failed probe, which is a red test
			// rather than a quiet one, because the assertions below need
			// the refusal.
			loop := filepath.Join(filepath.Dir(configPath), "state.db.serving-lock")
			if err := os.Symlink(loop, loop); err != nil {
				t.Fatalf("Symlink: %v", err)
			}

			code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

			if code == 0 {
				t.Errorf("%s exited 0 while the mode could not be decided at all\n%s", m.name, out)
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

// TestTheModeIsDecidedInsideTheClaimThatKeepsItTrue is the other half of
// "decided once", and the half that only became sayable when the
// detection was rewritten.
//
// Asking once is worth nothing if the answer can go stale before it is
// acted on. #538's review demonstrated exactly that: the check ran at the
// top of openBackupService, and the startup sequence, an SSH host-key
// probe over the network, a key import and a blocking read of standard
// input all happened afterwards with nothing held, so an engine that
// started in the gap was written straight over. A mode announced from a
// sample would be a label on the same defect.
//
// So the decision is made from inside service.BeginConfigWrite's claim,
// which is the same `.startup-lock` every engine start has to take, and
// the claim is held until the command is done. What that buys is
// asserted here rather than described: while a configuration write is in
// flight, an engine cannot finish starting.
//
// Both directions again. The second half is what stops this passing
// against a build where nothing can ever start: the identical call has to
// succeed the moment the write is finished with its claim.
func TestTheModeIsDecidedInsideTheClaimThatKeepsItTrue(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	ctx := context.Background()

	var (
		cleanup func()
		openErr error
	)
	// Captured because entering the mode announces it, and this test is
	// about the claim rather than about the line.
	captureStdout(t, func() {
		_, cleanup, openErr = openBackupService(ctx, configPath, writesConfig)
	})
	if openErr != nil {
		t.Fatalf("openBackupService(writesConfig) with nothing serving this deployment: %v", openErr)
	}

	_, closeFn, err := service.Open(ctx, configPath)
	if err == nil {
		_ = closeFn()
		cleanup()
		t.Fatalf("an engine finished starting while a configuration write held its claim, so the mode that write announced was a sample rather than a decision: an engine that arrives in that gap is written straight over, which is #535 through the guard meant to stop it")
	}
	if !errors.Is(err, service.ErrStartupLocked) {
		cleanup()
		t.Fatalf("a start attempted underneath a configuration write failed with %v, not %v; the refusal has to come from the write's claim rather than from something else being wrong", err, service.ErrStartupLocked)
	}

	cleanup()

	// And the claim is given back, or the assertion above would be
	// satisfied by a deployment nothing can ever start.
	_, closeFn, err = service.Open(ctx, configPath)
	if err != nil {
		t.Fatalf("an engine could not start after the configuration write was done with its claim: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Errorf("closing the journal: %v", err)
	}
}

// TestTheModeIsDecidedOncePerInvocation is #542's first clause held to
// the kernel rather than to a reading of the code: however many places in
// one invocation want to know which mode this is, the detection runs
// exactly once and everything downstream is handed that one answer.
//
// It matters beyond tidiness. A second probe is a second question asked
// of a world that can have changed between the two, so an engine that
// starts in the gap turns a refusal into a direct write, which is the
// downgrade this issue exists to make impossible. Counting the probes is
// the only way to tell a decision that is carried from one that happens
// to agree with itself today.
//
// Both entry points are counted into one number on purpose. They are the
// same question, asked by callers that differ only in whether there is a
// configuration file to read the journal out of, and a counter that
// watched one of them would report "asked once" for an invocation that
// asked the other one twice.
func TestTheModeIsDecidedOncePerInvocation(t *testing.T) {
	countProbes := func(t *testing.T) *int {
		t.Helper()
		calls := 0
		realByConfig := detectRunningEngine
		realByJournal := detectRunningEngineForJournal
		detectRunningEngine = func(configPath string) (*service.RunningEngine, error) {
			calls++
			return realByConfig(configPath)
		}
		detectRunningEngineForJournal = func(dbPath string) (*service.RunningEngine, error) {
			calls++
			return realByJournal(dbPath)
		}
		t.Cleanup(func() {
			detectRunningEngine = realByConfig
			detectRunningEngineForJournal = realByJournal
		})
		return &calls
	}

	for _, m := range configMutations {
		t.Run(m.name, func(t *testing.T) {
			configPath := writeTestConfigWithDeploymentPolicy(t)
			keyPath := writeTestPrivateKey(t)
			if m.prepare != nil {
				m.prepare(t, configPath)
			}

			calls := countProbes(t)
			code, out := runCapturingBothStreams(t, m.args(configPath, keyPath))

			if code != 0 {
				t.Fatalf("%s exited %d with nothing serving this deployment, want 0\n%s", m.name, code, out)
			}
			if *calls != 1 {
				t.Errorf("%s asked whether an engine is serving this deployment %d times; the mode is one decision per invocation, and a second ask is a second answer that can disagree with the first", m.name, *calls)
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
			t.Errorf("writing the first configuration asked whether an engine is serving this deployment %d times, want 1", *calls)
		}
	})
}

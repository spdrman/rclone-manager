package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// The three announcements an operator (or a test) greps for, built from
// the product's own constants so a reworded mode cannot leave these
// assertions passing against a line nobody prints.
var (
	directReadLine         = modeLinePrefix + string(directMode)
	engineAttachedReadLine = modeLinePrefix + string(engineAttachedMode)
	unconfirmedReadLine    = modeLinePrefix + string(unconfirmedMode)
)

// Issue #544, Phase 2 of #536: a read command beside a live engine answers
// about the world that engine holds, or refuses; it never quietly answers
// about a different one.
//
// # Why the tests here are comparisons rather than assertions
//
// #535's operator symptom was `sources` listing two backup sets while the
// Web UI showed none. Neither surface was broken on its own terms: each
// answered correctly about the configuration it had. What was missing was
// anything comparing the two. A suite that asserted the CLI's answer
// against its own fixture and the API's against its own would have been
// green throughout that bug, which is why the cases below drive the same
// question down both routes against ONE deployment and compare the
// answers, rather than checking each route separately.
//
// # And why the disagreement is arranged deliberately
//
// A comparison that has never been watched fail is not a comparison. The
// agreement cases would pass against a build that ignores the engine
// entirely, because the two routes do agree when nothing has made them
// disagree. So every agreement case has a twin in which the engine is
// given a DIFFERENT configuration for the same journal, which is exactly
// the state #535 left a deployment in, and the twin requires a refusal.

// readCommand is one read surface #544 names, plus how to invoke it.
//
// `settings` is deliberately absent: it is a read of the configuration
// too, and it belongs to the same family, but issue #543 owns settings.go
// in this wave and two lanes editing one command is how a mode gets
// announced twice. `restore` is absent for a different reason, written
// down here rather than left to be noticed: it is declared readsConfig and
// it writes an operation row into the journal, so it is not a read of this
// kind at all.
var readCommands = []struct {
	name string
	args func(configPath string) []string

	// volatile is the lines this surface prints that two runs a moment
	// apart legitimately disagree about whichever route answered them.
	// core/tests/compat leaves `status` out of its pinned CLI table for
	// exactly this reason, in as many words: it prints live free space,
	// and two runs a second apart on the same machine disagree.
	//
	// Only `status` has one, and steadyStatus below fails the test if the
	// pattern stops matching, so a reworded line lands here as a red
	// assertion rather than as a comparison that quietly stopped
	// comparing.
	volatile []*regexp.Regexp

	// asks is the contract path this surface has to put its own question
	// to the engine on, beyond the /system/version every read asks.
	//
	// It is asserted rather than assumed. Without it, a build whose
	// comparisons all returned nil early would pass every agreement case
	// below, because two routes that never speak to each other do agree.
	asks string
}{
	{"sources", func(p string) []string { return []string{"sources", "--config", p} }, nil, "/backup-sets"},
	{"status", func(p string) []string { return []string{"status", "--config", p} },
		[]*regexp.Regexp{
			// The age of the newest known-good backup, which is measured
			// from now on each run.
			regexp.MustCompile(`(?m)^  newest known-good backup: .*$`),
			// A live capacity reading of the machine the suite runs on.
			regexp.MustCompile(`(?m)^  free space: .*$`),
		}, "/system/health"},
	{"artifacts", func(p string) []string { return []string{"artifacts", "--config", p} }, nil, "/backups"},
	{"artifacts, one artifact's detail", func(p string) []string {
		return []string{"artifacts", "--config", p, "production/postgres-primary/backup.dump"}
	}, nil, "/backups/production/postgres-primary/backup.dump"},
	{"artifacts, filtered to a backup set", func(p string) []string {
		return []string{"artifacts", "--config", p, "--backup-set", "postgres-primary"}
	}, nil, "/backups"},
	{"artifacts, filtered to a source and a backup set", func(p string) []string {
		return []string{"artifacts", "--config", p, "--source", "production", "--backup-set", "postgres-primary"}
	}, nil, "/backups"},
	{"retention", func(p string) []string { return []string{"retention", "--config", p, "--dry-run"} }, nil, "/backup-sets/production/postgres-primary/retention/preview"},
	{"retention, under a policy this command line supplied", func(p string) []string {
		// Under a policy this command line supplied, so the engine is
		// deliberately NOT asked to agree with the verdicts: it is asked
		// only which configuration is in force. That exception is
		// asserted, not assumed, by naming no question of its own.
		return []string{"retention", "--config", p, "--dry-run", "--daily-days", "5"}
	}, nil, ""},
}

// seededDeployment is a configuration with one real backup already
// ingested, so every surface below has something to say. An empty journal
// would make several of these print the same two lines whatever route
// answered them, which is agreement that proves nothing.
func seededDeployment(t *testing.T) string {
	t.Helper()
	configPath := writeTestConfig(t)
	if code := captureStdoutCode(t, func() int { return run([]string{"run", "--config", configPath}) }); code != 0 {
		t.Fatalf("seeding the deployment with one backup: run exited %d", code)
	}
	return configPath
}

// divergedConfig is configPath's own configuration with one more backup
// set in it, naming the same journal.
//
// One more, rather than one fewer, so the engine holds a set the file does
// not: that is #535's exact shape with the roles as they actually were. It
// is also the shape a comparison is most likely to get wrong, because a
// CLI that only checks that everything IT knows about is known to the
// engine passes while the engine holds twice as much.
func divergedConfig(t *testing.T, configPath string) string {
	t.Helper()
	raw := readFile(t, configPath)
	dir := filepath.Dir(configPath)
	extra := "      - id: postgres-secondary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + filepath.Join(dir, "remote") + "\n" +
		"        local_path: " + filepath.Join(dir, "local-secondary") + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n"
	marker := "retention:\n"
	if !strings.Contains(raw, marker) {
		t.Fatalf("the fixture has no retention block to anchor on; this helper is out of date:\n%s", raw)
	}
	updated := strings.Replace(raw, marker, extra+marker, 1)
	out := filepath.Join(dir, "config-engine.yaml")
	if err := os.WriteFile(out, []byte(updated), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return out
}

// TestBothRoutesAnswerTheSameQuestionTheSameWay is #544's acceptance: with
// an engine serving the very configuration this command loaded, every read
// prints exactly what it prints with no engine at all.
//
// The point is not that the engine-attached run works. It is that the two
// runs cannot differ: an operator on a terminal and an operator in a
// browser are looking at one deployment, and a CLI whose answer moved when
// an engine came up would be showing them two.
func TestBothRoutesAnswerTheSameQuestionTheSameWay(t *testing.T) {
	for _, rc := range readCommands {
		t.Run(rc.name, func(t *testing.T) {
			configPath := seededDeployment(t)

			var direct string
			directCode := 0
			directStderr := captureStderr(t, func() {
				direct = captureStdout(t, func() { directCode = run(rc.args(configPath)) })
			})
			if directCode != 0 {
				t.Fatalf("%s exited %d with nothing serving this deployment\n%s\n%s", rc.name, directCode, direct, directStderr)
			}

			engine := startReadEngine(t, configPath, configPath)
			engine.use(t)

			var attached string
			attachedCode := 0
			attachedStderr := captureStderr(t, func() {
				attached = captureStdout(t, func() { attachedCode = run(rc.args(configPath)) })
			})
			if attachedCode != 0 {
				t.Fatalf("%s exited %d beside an engine serving the configuration it just read\n%s\n%s", rc.name, attachedCode, attached, attachedStderr)
			}

			if !strings.Contains(attachedStderr, engineAttachedReadLine) {
				t.Fatalf("%s did not report %q beside a live engine, so this comparison was between two runs that both answered from the file\nstderr:\n%s", rc.name, engineAttachedReadLine, attachedStderr)
			}
			asked := engine.asked()
			if !slices.Contains(asked, "/system/version") {
				t.Errorf("%s never asked the engine which configuration it holds; it asked for %v", rc.name, asked)
			}
			if rc.asks != "" && !slices.Contains(asked, rc.asks) {
				t.Errorf("%s never put its own question to the engine (%s); it asked for %v", rc.name, rc.asks, asked)
			}

			direct, attached = steadyStatus(t, rc.name, rc.volatile, direct, attached)
			if attached != direct {
				t.Errorf("%s answered differently depending on whether an engine was up, which is two surfaces showing two worlds\nwith nothing serving:\n%s\nbeside the engine:\n%s", rc.name, direct, attached)
			}
		})
	}
}

// TestAReadRefusesWhenTheEngineHoldsADifferentConfiguration is the twin
// above's negative control, and it is #535 from the read side.
//
// The engine here serves a configuration with a backup set the file does
// not have, against the same journal: precisely what a deployment looks
// like after a `docker exec backup-set create` that the serving process
// never saw, with the roles the other way round. What the CLI must not do
// is print its own world and exit 0, because that answer looks
// authoritative and is not.
func TestAReadRefusesWhenTheEngineHoldsADifferentConfiguration(t *testing.T) {
	for _, rc := range readCommands {
		t.Run(rc.name, func(t *testing.T) {
			configPath := seededDeployment(t)
			engine := startReadEngine(t, configPath, divergedConfig(t, configPath))
			engine.use(t)

			var out string
			code := 0
			stderr := captureStderr(t, func() {
				out = captureStdout(t, func() { code = run(rc.args(configPath)) })
			})

			if code == 0 {
				t.Errorf("%s exited 0 while the process serving this deployment held a different configuration, so an operator was shown a world its Web UI does not have\nstdout:\n%s\nstderr:\n%s", rc.name, out, stderr)
			}
			if strings.Contains(out, "postgres-primary") {
				t.Errorf("%s printed its own answer anyway; a read that cannot be reconciled with the serving process must not present one\nstdout:\n%s", rc.name, out)
			}
		})
	}
}

// steadyStatus replaces the lines a surface is allowed to disagree with
// itself about, in both answers, and fails if a pattern matched neither.
//
// The failure case is the point. A normalizer that silently stops matching
// turns a comparison into a comparison of two normalized-away strings, and
// that is how a guard becomes decoration: this one has to keep earning its
// place on every run.
func steadyStatus(t *testing.T, name string, volatile []*regexp.Regexp, direct, attached string) (string, string) {
	t.Helper()
	for _, re := range volatile {
		if !re.MatchString(direct) || !re.MatchString(attached) {
			t.Fatalf("%s: the pattern %s matched nothing, so this comparison would have been normalizing away a line that is no longer printed\nwith nothing serving:\n%s\nbeside the engine:\n%s", name, re, direct, attached)
		}
		direct = re.ReplaceAllString(direct, "<varies between runs>")
		attached = re.ReplaceAllString(attached, "<varies between runs>")
	}
	return direct, attached
}

// TestAReadRefusesWhenTheEngineAnswersDifferently is the control the
// revision comparison cannot be: an engine reporting the very
// configuration revision this command loaded, and then answering the
// command's own question with one thing missing or one verdict flipped.
//
// That is the shape a caching bug takes, and it is the only way to watch
// the five comparisons in readagreement.go actually fail. Without it they
// would be five functions that have only ever returned nil.
func TestAReadRefusesWhenTheEngineAnswersDifferently(t *testing.T) {
	for _, rc := range readCommands {
		if rc.asks == "" {
			// This surface deliberately asks the engine nothing of its
			// own, so there is no answer here to make disagree.
			continue
		}
		t.Run(rc.name, func(t *testing.T) {
			configPath := seededDeployment(t)
			engine := startReadEngine(t, configPath, configPath)
			engine.stale = true
			engine.use(t)

			var out string
			code := 0
			stderr := captureStderr(t, func() {
				out = captureStdout(t, func() { code = run(rc.args(configPath)) })
			})

			if !strings.Contains(stderr, engineAttachedReadLine) {
				t.Fatalf("%s did not reach engine-attached mode, so this case never exercised the comparison it is about\nstderr:\n%s", rc.name, stderr)
			}
			if code == 0 {
				t.Errorf("%s exited 0 while the serving process answered its own question differently, so an operator was shown one surface's answer as though both agreed\nstdout:\n%s\nstderr:\n%s", rc.name, out, stderr)
			}
			if strings.Contains(out, "postgres-primary") {
				t.Errorf("%s printed its answer anyway\nstdout:\n%s", rc.name, out)
			}
		})
	}
}

// TestEveryReadSaysWhichWorldItIsAbout is #544's other half: the mode, on
// every read surface, in all three states, and never one announced in
// another's place.
//
// Three arms rather than two, because reads have a third mode writes do
// not. A read that cannot reach the engine still answers, which is the
// right trade for a command an operator runs when something is already
// wrong, and the whole of what stops that being #535 again is that it says
// so. So the third arm is the one that would be missed, and it asserts
// both that the sentence is there and that neither of the other two is.
func TestEveryReadSaysWhichWorldItIsAbout(t *testing.T) {
	t.Run("with nothing serving this deployment", func(t *testing.T) {
		for _, rc := range readCommands {
			t.Run(rc.name, func(t *testing.T) {
				configPath := seededDeployment(t)
				code, out := runReadCapturingBothStreams(t, rc.args(configPath))
				if code != 0 {
					t.Fatalf("%s exited %d on a host with no engine on it\n%s", rc.name, code, out)
				}
				requireOnly(t, rc.name, out, directReadLine, engineAttachedReadLine, unconfirmedReadLine)
			})
		}
	})

	t.Run("with an engine serving it", func(t *testing.T) {
		for _, rc := range readCommands {
			t.Run(rc.name, func(t *testing.T) {
				configPath := seededDeployment(t)
				engine := startReadEngine(t, configPath, configPath)
				engine.use(t)
				code, out := runReadCapturingBothStreams(t, rc.args(configPath))
				if code != 0 {
					t.Fatalf("%s exited %d beside an engine serving what it read\n%s", rc.name, code, out)
				}
				requireOnly(t, rc.name, out, engineAttachedReadLine, directReadLine, unconfirmedReadLine)
			})
		}
	})

	t.Run("with an engine serving it and no route to it", func(t *testing.T) {
		for _, rc := range readCommands {
			t.Run(rc.name, func(t *testing.T) {
				configPath := seededDeployment(t)
				startReadEngine(t, configPath, configPath)
				// Deliberately not engine.use: an operator who has never
				// told this host where its own engine is, which is every
				// deployment until somebody does.
				t.Setenv(apiURLEnv, "")

				code, out := runReadCapturingBothStreams(t, rc.args(configPath))
				if code != 0 {
					t.Fatalf("%s exited %d because it could not reach the engine; a read that cannot ask still has to answer\n%s", rc.name, code, out)
				}
				requireOnly(t, rc.name, out, unconfirmedReadLine, directReadLine, engineAttachedReadLine)
				if !strings.Contains(out, apiURLEnv) {
					t.Errorf("%s said its answer was unconfirmed without saying what would make it confirmable; want %s named\n%s", rc.name, apiURLEnv, out)
				}
			})
		}
	})
}

// requireOnly asserts that want appears and that neither of the other two
// mode lines does.
//
// Both halves matter. A build that announced every mode at once would
// satisfy a one-sided assertion while telling an operator nothing, and a
// build that quietly answered from the file beside a live engine would
// satisfy it too as long as it printed the engine-attached sentence.
func requireOnly(t *testing.T, name, out, want string, absent ...string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("%s never said which world its answer was about; want a line containing %q, got:\n%s", name, want, out)
	}
	for _, other := range absent {
		if strings.Contains(out, other) {
			t.Errorf("%s announced %q as well, so an operator cannot tell which world this answer is about\n%s", name, other, out)
		}
	}
}

// runReadCapturingBothStreams runs one invocation and folds its two
// streams, for mode_test.go's reason: what is promised is that an operator
// reading one terminal can see which mode ran.
func runReadCapturingBothStreams(t *testing.T, args []string) (int, string) {
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

// TestTheReadModeIsDecidedOncePerInvocation is mode_test.go's first clause
// applied to the surfaces that had no mode at all until this issue.
//
// It matters here for the same reason it matters for a write, one step
// removed: a second probe is a second question put to a world that can
// have changed between the two, and a read that decided "direct" on the
// first and then answered on the second would announce a mode it did not
// act in.
func TestTheReadModeIsDecidedOncePerInvocation(t *testing.T) {
	for _, rc := range readCommands {
		t.Run(rc.name, func(t *testing.T) {
			configPath := seededDeployment(t)

			probes := 0
			realDetect := detectRunningEngine
			detectRunningEngine = func(path string) (*service.RunningEngine, error) {
				probes++
				return realDetect(path)
			}
			t.Cleanup(func() { detectRunningEngine = realDetect })

			code, out := runReadCapturingBothStreams(t, rc.args(configPath))
			if code != 0 {
				t.Fatalf("%s exited %d\n%s", rc.name, code, out)
			}
			if probes != 1 {
				t.Errorf("%s asked whether an engine is serving this deployment %d times; a decision asked twice is two answers about a world that can change in between", rc.name, probes)
			}
		})
	}
}

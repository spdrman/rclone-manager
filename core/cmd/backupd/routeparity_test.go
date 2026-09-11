package main

import (
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/cliecho"
)

// Issue #543's acceptance criterion, driven rather than described: "a
// create that the API would refuse must be refused identically when
// attached, including the exit code. Prove that with the same input
// against both routes."
//
// # What "identically" is worth here
//
// The exit code half used to be cheap, and this test said so: every
// failure in this binary exited 1 and every usage mistake 2, so comparing
// two refusals' exit codes was satisfied by construction and proved that
// the two routes agreed about nothing in particular.
//
// #551 gave that comparison something to compare. There is now a third
// failure code, 3, and it means one thing: another process is serving this
// deployment, so nothing was done. So a route that answered one of these
// inputs with "the engine holds this" rather than with the refusal the
// input actually earns is now visible here, which it was not before, and
// the code each route exits with is asserted exactly rather than only
// against the other. Both have to be the ordinary failure: these are
// refusals about the REQUEST, and an engine-attached one is refused while
// attached to an engine, which is exactly the confusion a script branching
// on 3 would make if either route reached for it.
//
// The half that is worth something today is the REASON. The same input is
// driven at both routes and the refusal the direct route printed has to
// appear, verbatim, in what the engine-attached route printed. That is a
// real assertion because the fake engine's refusals are produced by the
// real core/service (fakeengine_test.go): the sentence being compared was
// generated twice by the same validator, once in this process and once
// behind an HTTP boundary, and a mapping that dropped or mangled a field
// on the way out would produce a different sentence on the far side.
//
// # Why the two configurations are identical
//
// Deliberately. "The same input" has to mean the same input all the way
// down, and a refusal that depends on what is already configured (creating
// over an id that exists) is only the same question on both routes if both
// deployments hold the same thing. They are two separate files, so a write
// that landed in the wrong one is still visible; they just start out
// saying the same thing.

// refusingInput is one invocation both routes have to refuse.
type refusingInput struct {
	name string
	args func(configPath, keyPath string) []string
}

// refusingInputs are chosen for what they exercise on the way to the
// refusal, not only for the refusal itself.
//
// The duplicate id reaches the engine's own configuration, so it is
// refused by what the SERVING process holds rather than by anything the
// CLI could decide on its own. The stable-without-a-window case is refused
// by request validation before any configuration is consulted. The patch
// and remove cases name a set that is not there, which is the refusal a
// script most often meets. Between them every routed verb is covered.
var refusingInputs = []refusingInput{
	{
		name: "create over an id the deployment already has",
		args: func(configPath, keyPath string) []string {
			return createArgs(configPath, keyPath, cliSet)
		},
	},
	{
		name: "create with the stable strategy and no window",
		args: func(configPath, keyPath string) []string {
			return createArgs(configPath, keyPath, "api/postgres", "--completion-strategy", "stable")
		},
	},
	{
		name: "patch a set this deployment does not have",
		args: func(configPath, _ string) []string {
			return []string{"backup-set", "--config", configPath, "patch", "nosuch/set", "--stale-after", "48h"}
		},
	},
	{
		name: "remove a set this deployment does not have",
		args: func(configPath, _ string) []string {
			return []string{"backup-set", "--config", configPath, "remove", "nosuch/set"}
		},
	},
	{
		name: "patch the settings with a timezone that is not one",
		args: func(configPath, _ string) []string {
			return []string{"settings", "--config", configPath, "patch", "--timezone", "Definitely/NotAZone"}
		},
	},
}

func TestARefusalIsTheSameOnBothRoutes(t *testing.T) {
	for _, in := range refusingInputs {
		t.Run(in.name, func(t *testing.T) {
			directCode, directErr := refuseDirectly(t, in)
			engineCode, engineErr := refuseThroughAnEngine(t, in)

			if directCode == exitOK || engineCode == exitOK {
				t.Fatalf("this input was not refused on both routes, so there is no parity to compare: direct exited %d, engine-attached exited %d\ndirect: %s\nengine: %s",
					directCode, engineCode, directErr, engineErr)
			}
			if directCode != engineCode {
				t.Errorf("the same input exited %d directly and %d through the engine; a script cannot tell one refusal from the other\ndirect: %s\nengine: %s",
					directCode, engineCode, directErr, engineErr)
			}
			// And they agree on the RIGHT code, which is the half that
			// only became checkable with #551. Equal-to-each-other was
			// satisfied by construction while there was one failure code;
			// equal-to-1 says these refusals are about what was asked for,
			// and that neither route has started reporting them as the
			// deployment being busy, which is the one thing a script is
			// meant to wait on.
			for _, got := range []struct {
				route string
				code  int
				out   string
			}{{"directly", directCode, directErr}, {"through the engine", engineCode, engineErr}} {
				if got.code != exitFailure {
					t.Errorf("refused %s with exit %d, want %d; %d is reserved for another process holding this deployment, and a script that waited for an engine to stop over a request it will never accept would wait forever\n%s",
						got.route, got.code, exitFailure, exitEngineHoldsDeployment, got.out)
				}
			}

			reason := refusalReason(directErr)
			if reason == "" {
				t.Fatalf("the direct route printed no refusal to compare against:\n%s", directErr)
			}
			if !strings.Contains(engineErr, reason) {
				t.Errorf("the engine-attached route refused for a different reason than the direct one.\ndirect said:  %s\nengine said:  %s", reason, engineErr)
			}
		})
	}
}

// refuseDirectly runs one invocation with nothing serving the deployment.
func refuseDirectly(t *testing.T, in refusingInput) (int, string) {
	t.Helper()
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)
	before := readFile(t, configPath)

	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(in.args(configPath, keyPath)) })
	})
	if after := readFile(t, configPath); after != before {
		t.Errorf("a refused %s changed config.yaml on the direct route", in.name)
	}
	return code, stderr
}

// refuseThroughAnEngine runs the identical invocation with an engine
// attached and a route to it.
func refuseThroughAnEngine(t *testing.T, in refusingInput) (int, string) {
	t.Helper()
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)
	engine := startFakeEngineFor(t, configPath)
	engine.attach(t)
	before := readFile(t, configPath)

	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(in.args(configPath, keyPath)) })
	})
	if after := readFile(t, configPath); after != before {
		t.Errorf("a refused %s changed config.yaml on the engine-attached route", in.name)
	}
	return code, stderr
}

// refusalReason strips this binary's own prefix off the last complaint it
// printed, leaving the sentence the two routes have to agree on.
//
// The LAST line rather than the whole capture: the engine-attached route
// announces its mode on stderr first, and comparing that in would be
// comparing the two routes' announcements, which are supposed to differ.
func refusalReason(stderr string) string {
	const prefix = cliecho.Binary + ": "
	for _, line := range reverse(strings.Split(strings.TrimRight(stderr, "\n"), "\n")) {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return after
		}
	}
	return ""
}

func reverse(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		out = append(out, lines[i])
	}
	return out
}

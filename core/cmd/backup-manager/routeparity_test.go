package main

import (
	"strings"
	"testing"
)

// Issue #543's acceptance criterion, driven rather than described: "a
// create that the API would refuse must be refused identically when
// attached, including the exit code. Prove that with the same input
// against both routes."
//
// # What "identically" is worth here, and what it is not
//
// The exit code half is cheap and this test says so out loud rather than
// claiming more than it has. Every failure in this binary exits 1 (fail(),
// setup.go) and every usage mistake exits 2, so an exit-code comparison
// between two refusals is satisfied by construction today; issue #551 is
// where that becomes a real signal. What the comparison is worth is that
// it will start to mean something the moment #551 lands, and that a route
// which turned a refusal into a usage error, or into a success, fails here
// now.
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

			if directCode == 0 || engineCode == 0 {
				t.Fatalf("this input was not refused on both routes, so there is no parity to compare: direct exited %d, engine-attached exited %d\ndirect: %s\nengine: %s",
					directCode, engineCode, directErr, engineErr)
			}
			if directCode != engineCode {
				t.Errorf("the same input exited %d directly and %d through the engine; a script cannot tell one refusal from the other\ndirect: %s\nengine: %s",
					directCode, engineCode, directErr, engineErr)
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
	engine := startFakeEngine(t, writeTestConfig(t))
	engine.attach(t)
	attachEngineTo(t, configPath)
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
	const prefix = "backup-manager: "
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

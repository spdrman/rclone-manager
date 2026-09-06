package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #559's review of #555, driven. Three findings land in this file, and
// what they have in common is that the write guard was argued for and
// then not watched: the sub-rule it rests on had no test, the invariant it
// promises was broken two calls away by setup.go, and the mode line above
// the refusal said something else.
//
// # Two nothings are not an agreement
//
// deploymentcheck.go calls it load-bearing in as many words: "two
// processes that both name nothing would compare EQUAL, which is the one
// way this check could pass while proving nothing." Nothing drove it. A
// reviewer deleted both empty refusals so two empties compared equal and
// the whole suite here, and the two-routes suite in apps/generic, stayed
// green.
//
// Every path into the empty case is reachable on a real deployment.
// served.DeploymentID is "" on every engine older than this build, on one
// that could not read its own identity file, and on one holding no
// deployment at all. The near side is "" on every deployment nothing has
// served on this build yet, and on one restored from its .db alone. Those
// two states overlap for the whole of this change's rollout, which is
// exactly when the guard has to hold.
//
// # And the near side really is never minted here
//
// That claim used to be false. openConfigWriteRoute calls service.Open
// before it enters config-write mode, service.Open ran runStartupSequence,
// and that minted. So on a deployment whose identity file was missing
// beside an engine still holding the old one, a single `backup-manager
// status` renamed the deployment, and every routed write afterwards
// refused against its own engine while telling the operator to go and
// check $BACKUP_MANAGER_API_URL. Minting now belongs to core/service's
// AnnounceServing, so only a process about to serve can name a
// deployment, and TestNoCommandOnThisSideMintsThisDeploymentsIdentity
// watches that from the outside.

// deploymentIDFileSuffix is the name core/service gives this deployment's
// identity file, spelled out here rather than reached for.
//
// It is an operator-visible file in a state directory people look at, and
// the tests below arrange states an operator's deployment gets into (a
// journal restored on its own, a deployment nothing has served yet) by
// moving it out of the way. Spelling it means a rename is something this
// suite notices rather than follows.
const deploymentIDFileSuffix = ".deployment-id"

// hideDeploymentIdentity moves this deployment's identity aside, which is
// the state a journal restored without the rest of its state directory
// comes back in.
//
// Moved rather than deleted, so a test that wants to put it back can, and
// so a failure leaves the evidence in the temporary directory.
func hideDeploymentIdentity(t *testing.T, configPath string) string {
	t.Helper()
	path := journalNamedByTestConfig(t, configPath) + deploymentIDFileSuffix
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("this deployment has no identity to hide (%v), so the state under test was never arranged", err)
	}
	if err := os.Rename(path, path+".moved-aside"); err != nil {
		t.Fatalf("moving %s aside: %v", path, err)
	}
	return path
}

// TestARoutedWriteRefusesWhenTheEngineNamesNoDeployment is the far half of
// the sub-rule.
//
// An engine from before this check answers with no deployment_id at all,
// and so does one that could not read its own identity file. Neither is a
// match with anything, and a comparison that let two empties through would
// send the change wherever the address happened to point.
func TestARoutedWriteRefusesWhenTheEngineNamesNoDeployment(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.anonymous = true
	engine.attach(t)
	before := readFile(t, cliConfig)

	args := []string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "48h"}
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})

	if code == 0 {
		t.Errorf("backup-set patch exited 0 against an engine that named no deployment, so a change went to a process nothing established was this deployment's\nstderr: %s", stderr)
	}
	if after := readFile(t, cliConfig); after != before {
		t.Error("backup-set patch wrote config.yaml as well; a write that cannot be confirmed is refused, never downgraded")
	}
	if containsRequest(engine.requests(), "PATCH /backup-sets/"+cliSet) {
		t.Errorf("the patch was sent anyway: %v", engine.requests())
	}
	if !strings.Contains(stderr, "did not say which deployment it serves") && !strings.Contains(stderr, "answered without naming a deployment") {
		t.Errorf("the refusal does not say what was observed:\n%s", stderr)
	}
}

// TestARoutedWriteRefusesWhenThisDeploymentHasNoIdentity is the near half,
// and it is the refusal that could not fire at all until minting moved.
//
// A deployment restored from its .db alone has no identity file. Beside an
// engine that is still serving and still holding the one it read at
// startup, that is two processes where exactly one can name itself.
func TestARoutedWriteRefusesWhenThisDeploymentHasNoIdentity(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.attach(t)
	if engine.svc.DeploymentID() == "" {
		t.Fatal("the engine could not name its own deployment either, so this test is driving the far half rather than the near one")
	}
	hidden := hideDeploymentIdentity(t, cliConfig)
	before := readFile(t, cliConfig)

	args := []string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "48h"}
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})

	if code == 0 {
		t.Errorf("backup-set patch exited 0 from a deployment that cannot name itself\nstderr: %s", stderr)
	}
	if after := readFile(t, cliConfig); after != before {
		t.Error("backup-set patch wrote config.yaml as well")
	}
	if !strings.Contains(stderr, "has no identity of its own") {
		t.Errorf("the refusal is not the one about this deployment's own name, so either the check ran on something else or this command minted an identity and then compared it:\n%s", stderr)
	}
	if _, err := os.Stat(hidden); !os.IsNotExist(err) {
		t.Errorf("the command minted %s on its way past (stat err = %v); it is about to hand a change to somebody else's process and has no business naming the deployment it is standing in", hidden, err)
	}
}

// TestARoutedWriteRefusesWhenNeitherSideCanNameItsDeployment is the case
// the sub-rule is actually about: both answers empty, and equal.
//
// This is the state a whole fleet is in for the length of this change's
// rollout, so a comparison that reads two nothings as agreement does not
// fail rarely, it fails everywhere at once and then stops.
func TestARoutedWriteRefusesWhenNeitherSideCanNameItsDeployment(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.anonymous = true
	engine.attach(t)
	hideDeploymentIdentity(t, cliConfig)
	before := readFile(t, cliConfig)

	args := []string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "48h"}
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})

	if code == 0 {
		t.Errorf("backup-set patch exited 0 with neither end able to say which deployment it is, which is two nothings compared as a match\nstderr: %s", stderr)
	}
	if after := readFile(t, cliConfig); after != before {
		t.Error("backup-set patch wrote config.yaml as well")
	}
	if containsRequest(engine.requests(), "PATCH /backup-sets/"+cliSet) {
		t.Errorf("the patch was sent to an engine nothing established was this deployment's: %v", engine.requests())
	}
}

// TestNoCommandOnThisSideMintsThisDeploymentsIdentity is #559's third
// finding, watched from where an operator would meet it.
//
// deploymentcheck.go promises the near side is read and never minted, and
// setup.go broke it two calls later: openConfigWriteRoute calls
// service.Open, which ran the startup sequence, which minted. `backup-set
// create` is enough, and so is `backup-manager status`, which writes
// nothing and is what somebody runs first when a deployment looks wrong.
//
// The engine here is holding the identity it read when it started, so a
// mint on this side does not merely invent a name: it renames the
// deployment away from the one the engine is serving, and every routed
// write afterwards refuses against that deployment's own engine.
func TestNoCommandOnThisSideMintsThisDeploymentsIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(configPath string) []string
	}{
		{
			name: "status",
			args: func(configPath string) []string {
				return []string{"status", "--config", configPath}
			},
		},
		{
			name: "sources",
			args: func(configPath string) []string {
				return []string{"sources", "--config", configPath}
			},
		},
		{
			name: "backup-set patch",
			args: func(configPath string) []string {
				return []string{"backup-set", "--config", configPath, "patch", cliSet, "--stale-after", "48h"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cliConfig := writeTestConfig(t)
			engine := startFakeEngineFor(t, cliConfig)
			engine.attach(t)
			served := engine.svc.DeploymentID()
			if served == "" {
				t.Fatal("the engine names no deployment, so there is nothing here for a mint to rename this deployment away from")
			}
			hidden := hideDeploymentIdentity(t, cliConfig)

			captureStderr(t, func() {
				captureStdoutCode(t, func() int { return run(tc.args(cliConfig)) })
			})

			if _, err := os.Stat(hidden); !os.IsNotExist(err) {
				minted, readErr := os.ReadFile(hidden)
				t.Errorf("%s minted %s (%q, read err %v) on a deployment an engine is serving as %s; one command that writes nothing has renamed the deployment, and every routed write afterwards refuses against its own engine", tc.name, hidden, strings.TrimSpace(string(minted)), readErr, served)
			}
		})
	}
}

// TestTheModeLineSaysWhyARouteThatWasNamedIsRefused is #559's fifth
// finding.
//
// mode.go argues the distinction itself: "no route to the engine serving
// this deployment" sends an operator to set $BACKUP_MANAGER_API_URL, and
// "the route goes somewhere this command will not write" sends them to
// correct one they have already set. The announcement filled its reason in
// from one error type, so four of the five ways a named route is refused
// printed "this build has no route to it" one line above a refusal saying
// something else entirely, which is a regression against main as well as
// two contradicting sentences.
func TestTheModeLineSaysWhyARouteThatWasNamedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, cliConfig string)
		wantSay string
	}{
		{
			name: "the address cannot be made into a route",
			arrange: func(t *testing.T, cliConfig string) {
				attachEngineTo(t, cliConfig)
				t.Setenv(apiURLEnv, "not-an-address-at-all")
				t.Setenv(apiUsernameEnv, "operator")
				t.Setenv(apiPasswordEnv, "not-a-real-password")
			},
			wantSay: "does not name an engine this command can reach",
		},
		{
			name: "the engine does not answer",
			arrange: func(t *testing.T, cliConfig string) {
				attachEngineTo(t, cliConfig)
				t.Setenv(apiURLEnv, "http://127.0.0.1:1")
				t.Setenv(apiUsernameEnv, "operator")
				t.Setenv(apiPasswordEnv, "not-a-real-password")
			},
			wantSay: "did not answer when this command asked which deployment it serves",
		},
		{
			name: "the engine names no deployment",
			arrange: func(t *testing.T, cliConfig string) {
				engine := startFakeEngineFor(t, cliConfig)
				engine.anonymous = true
				engine.attach(t)
			},
			wantSay: "did not say which deployment it serves",
		},
		{
			name: "this deployment has no identity",
			arrange: func(t *testing.T, cliConfig string) {
				engine := startFakeEngineFor(t, cliConfig)
				engine.attach(t)
				hideDeploymentIdentity(t, cliConfig)
			},
			wantSay: "this deployment has no identity of its own",
		},
		{
			name: "the engine serves a different deployment",
			arrange: func(t *testing.T, cliConfig string) {
				engine := startFakeEngine(t, writeTestConfig(t))
				engine.attach(t)
				attachEngineTo(t, cliConfig)
			},
			wantSay: "serves a different deployment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cliConfig := writeTestConfig(t)
			tc.arrange(t, cliConfig)

			args := []string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "48h"}
			var out string
			var code int
			stderr := captureStderr(t, func() {
				out = captureStdout(t, func() { code = run(args) })
			})
			if code == 0 {
				t.Fatalf("backup-set patch exited 0, so there is no refusal for the mode line to agree or disagree with\nstdout:\n%s\nstderr:\n%s", out, stderr)
			}

			line := modeLineIn(t, out+stderr)
			if strings.Contains(line, "this build has no route to it") {
				t.Errorf("the mode line says this build has no route to the engine, and an address was given and used; that sends an operator to set a variable they have already set\nmode line: %s\nrefusal:   %s", line, stderr)
			}
			if !strings.Contains(line, tc.wantSay) {
				t.Errorf("the mode line does not say why the route was refused, so it and the sentence under it describe two different failures\nwant it to say: %s\nmode line:      %s", tc.wantSay, line)
			}
		})
	}
}

// modeLineIn returns the one announcement line out of a capture, or fails.
func modeLineIn(t *testing.T, captured string) string {
	t.Helper()
	for _, line := range strings.Split(captured, "\n") {
		if strings.HasPrefix(line, modeLinePrefix) {
			return line
		}
	}
	t.Fatalf("nothing in this capture announced a mode, so an operator could not tell which world the command ran in:\n%s", captured)
	return ""
}

// TestARoutedCreatePrintsTheStaleAfterTheOperatorTyped is #559's sixth
// finding.
//
// #555 put stale_after_seconds on the wire and deleted the "not reported"
// branch's reason for existing, and the fixture in this package went on
// omitting the field, so every routed test here exercised the legacy
// engine and none exercised the new one. A routed create in this package
// still printed "stale_after: not reported" for a value on its own command
// line, under a green suite.
func TestARoutedCreatePrintsTheStaleAfterTheOperatorTyped(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.attach(t)

	args := createArgs(cliConfig, writeTestPrivateKey(t), "api/postgres", "--stale-after", "48h")
	var out string
	var code int
	stderr := captureStderr(t, func() {
		out = captureStdout(t, func() { code = run(args) })
	})
	if code != 0 {
		t.Fatalf("a routed create exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	if strings.Contains(out, "stale_after: not reported") {
		t.Errorf("the routed create printed stale_after as not reported for a value typed on its own command line; that sentence is about an engine that cannot carry the field, and this one can\nstdout:\n%s", out)
	}
	if !strings.Contains(out, "stale_after: 48h0m0s") {
		t.Errorf("the routed create did not print the stale_after the operator typed\nstdout:\n%s", out)
	}
}

// TestARoutedCreateAndADirectCreateReportTheSameStaleAfter is the same
// property from the side that matters: the two routes agree.
func TestARoutedCreateAndADirectCreateReportTheSameStaleAfter(t *testing.T) {
	directOut := captureStdout(t, func() {
		configPath := writeTestConfig(t)
		if code := run(createArgs(configPath, writeTestPrivateKey(t), "api/postgres", "--stale-after", "48h")); code != 0 {
			t.Fatalf("a direct create exited %d", code)
		}
	})

	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.attach(t)
	var routedOut string
	stderr := captureStderr(t, func() {
		routedOut = captureStdout(t, func() {
			if code := run(createArgs(cliConfig, writeTestPrivateKey(t), "api/postgres", "--stale-after", "48h")); code != 0 {
				t.Fatalf("a routed create exited %d", code)
			}
		})
	})

	if directLine, routedLine := staleAfterLine(directOut), staleAfterLine(routedOut); directLine != routedLine {
		t.Errorf("the two routes report a different stale_after for one command line, which is the divergence the routed write refuses a sub-second window to avoid\ndirect: %s\nrouted: %s\nstderr: %s", directLine, routedLine, stderr)
	}
}

// staleAfterLine picks the one line both routes have to agree on.
func staleAfterLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "stale_after:") {
			return strings.TrimSpace(line)
		}
	}
	return "(no stale_after line at all)"
}

// TestNoTestHereLeavesAnIdentityLyingAround is a small guard on the
// arrangement the file above depends on: hideDeploymentIdentity moves the
// file rather than removing it, so the evidence survives a failure.
func TestNoTestHereLeavesAnIdentityLyingAround(t *testing.T) {
	configPath := writeTestConfig(t)
	startFakeEngineFor(t, configPath)
	hidden := hideDeploymentIdentity(t, configPath)
	if _, err := os.Stat(hidden + ".moved-aside"); err != nil {
		t.Fatalf("the identity was not moved aside but disposed of (%v), so a failing test above leaves nothing to look at", err)
	}
	if filepath.Dir(hidden) == "" {
		t.Fatal("the identity file has no directory, which means the fixture's journal path is not what this file assumes")
	}
}

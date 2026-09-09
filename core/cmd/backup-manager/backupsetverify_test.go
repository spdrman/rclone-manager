package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Issue #624 (H2.3): a backup set's SSH connection is proven before the
// set is written, or the operator is told in so many words that it was
// not.
//
// The asymmetry this file closes is the reason it exists rather than any
// one assertion in it. `medium add` has verified by default since #443,
// `--no-verify` is an explicit opt-out whose output says nothing was
// proven, and the S3 wizard keeps Save disabled until the candidate check
// comes back ok. `backup-set create` had none of it: nothing on that path
// ran a check, and there was no flag to skip because there was nothing to
// skip. The six-step check from #596 existed and was reachable only from a
// set that ALREADY existed, so the order was backwards.
//
// Every case below drives the real check against a real address, because
// the whole point is that the check happens. A fake route that answered
// "ok" from a struct would pass against the bug: the bug was that nothing
// was asked at all.
//
// The address is a loopback port with nothing on it rather than a name
// that does not resolve. Both fail, and the closed port fails on a TCP
// refusal in microseconds where a bad name spends the resolver's timeout,
// which on a machine with a wildcard-answering DNS server is not even a
// failure. A test whose red depends on the developer's resolver is a test
// that goes green for the wrong reason.

// aClosedLoopbackPort returns a port on 127.0.0.1 that nothing is
// listening on: it binds one, reads the number, and closes it again.
//
// Racy in principle and not in practice, and the alternative is worse. A
// hardcoded number is a port some other test in this package (or the
// developer's own tooling) may genuinely be serving, which would turn a
// "connection refused" case into an unrelated protocol failure and read
// as a flake. What matters here is only that the dial fails, and every
// way this can be wrong still fails the dial.
func aClosedLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the probe listener: %v", err)
	}
	return port
}

// unreachableCreateArgs is createArgs pointed at an address that answers
// nothing, so the connection test in front of the write really does fail.
//
// It overrides --host and --port rather than taking a different base,
// because the assertion is about the verb every other test in this
// package drives and not about a second, gentler spelling of it. A flag
// passed twice takes its last value in package flag, which is what makes
// the override a two-line helper rather than a copy of createArgs.
func unreachableCreateArgs(t *testing.T, configPath, keyPath, id string, extra ...string) []string {
	t.Helper()
	args := createArgs(configPath, keyPath, id)
	// --no-verify=false, spelled out, because createArgs carries a bare
	// --no-verify for every case in this package that is about something
	// other than the check. A bare boolean flag cannot be turned off by
	// repeating it, so the cases here that need the check to actually run
	// say so with a value.
	args = append(args, "--no-verify=false", "--host", "127.0.0.1", "--port", strconv.Itoa(aClosedLoopbackPort(t)))
	return append(args, extra...)
}

// TestRun_BackupSetCreateProvesTheConnectionBeforeItWrites is the
// acceptance criterion, and it is written so it cannot pass against
// 0.3.3: before #624 this exact invocation exited 0 and wrote the set,
// because nothing on the create path ran a check at all.
//
// Two assertions, and the second is the one that matters. A non-zero exit
// with the set written anyway would be a command that reported a failure
// and did the thing, which is worse than either half on its own.
func TestRun_BackupSetCreateProvesTheConnectionBeforeItWrites(t *testing.T) {
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)
	before := readFile(t, configPath)

	args := unreachableCreateArgs(t, configPath, keyPath, "api/unreachable")
	out := captureStdout(t, func() {
		if got := run(args); got != 1 {
			t.Errorf("run(%v) = %d, want 1: a source that cannot be reached is not a source this manager will back up from, and creating the set anyway is what #624 is about", args, got)
		}
	})

	if after := readFile(t, configPath); after != before {
		t.Errorf("a refused create still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// The report, not only the verdict. Six named steps are the whole of
	// what #596 built, and a create that printed "failed" would throw the
	// diagnosis away exactly as the button did before that issue.
	for _, step := range []string{"credentials", "resolve", "connect", "host_key", "authenticate", "list"} {
		if !strings.Contains(out, step) {
			t.Errorf("the refusal does not report the %s step; a single verdict is what #596 replaced:\n%s", step, out)
		}
	}
	if !strings.Contains(out, "nothing was written") {
		t.Errorf("the refusal does not say the set was not created:\n%s", out)
	}
}

// TestRun_BackupSetCreateNoVerifySaysNothingWasProven is the escape
// hatch, spelled exactly as the destination side spells it.
//
// The output assertion is the point of the flag rather than decoration
// around it. `medium add --no-verify` prints a line saying no credential
// was obtained and no endpoint was contacted, precisely so an operator
// cannot mistake it for a check that passed, and a source-side opt-out
// that printed what a verified create printed would be the same green
// line standing behind a check nobody ran.
func TestRun_BackupSetCreateNoVerifySaysNothingWasProven(t *testing.T) {
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)

	args := unreachableCreateArgs(t, configPath, keyPath, "api/offline", "--no-verify=true")
	out := captureStdout(t, func() {
		if got := run(args); got != 0 {
			t.Errorf("run(%v) = %d, want 0: --no-verify is how configuration gets built offline against a host this machine cannot currently reach", args, got)
		}
	})

	if !strings.Contains(out, "not verified") {
		t.Errorf("--no-verify did not say that nothing was proven:\n%s", out)
	}
	if !strings.Contains(readFile(t, configPath), "api/offline") && !strings.Contains(readFile(t, configPath), "offline") {
		t.Errorf("--no-verify refused to write, which is the one thing it exists to do:\n%s", readFile(t, configPath))
	}
}

// TestRun_BackupSetCreateNoVerifyMarksTheSet is the half that keeps
// --no-verify honest after the terminal has scrolled away.
//
// The sentence printed at creation is read once, by whoever typed the
// command. The mark is what is still there tomorrow, for the operator who
// did not, and it is the difference between an escape hatch and a hole:
// without it a set nobody ever proved is indistinguishable from one that
// was checked against a real server.
func TestRun_BackupSetCreateNoVerifyMarksTheSet(t *testing.T) {
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)

	args := unreachableCreateArgs(t, configPath, keyPath, "api/offline", "--no-verify=true")
	out := captureStdout(t, func() {
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})

	if !strings.Contains(out, "connection: not verified") {
		t.Errorf("the set this command printed back does not report itself as unverified:\n%s", out)
	}
	if raw := readFile(t, configPath); !strings.Contains(raw, "connection_unverified: true") {
		t.Errorf("the mark is not in the configuration, so it does not survive this process:\n%s", raw)
	}
}

// anOfflineSFTPSet creates one sftp-backed set pointed at an address that
// answers nothing, through the --no-verify path, and returns its id.
//
// The patch cases need a set with a HOST, and the package fixture's own
// set reads a local path: `--host` on a local remote is a configuration
// core refuses outright, so patching that set would drive a validation
// refusal rather than a connection check. Building the subject through
// the create verb also means these cases run against a set shaped exactly
// as this command writes them.
func anOfflineSFTPSet(t *testing.T, configPath, id string) {
	t.Helper()
	keyPath := writeTestPrivateKey(t)
	args := unreachableCreateArgs(t, configPath, keyPath, id, "--no-verify=true")
	captureStdout(t, func() {
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0: the fixture for these cases is itself a --no-verify create", args, got)
		}
	})
}

// TestRun_BackupSetPatchProvesTheConnectionWhenItChangesOne is the edit
// half. A patch that repoints a set at a different remote folder is
// making the same claim a create makes, and it is worth MORE scrutiny
// rather than less: a create that cannot connect has produced nothing,
// and a patch that cannot connect has broken a set that was working.
//
// The control below is the half that stops this being a patch verb that
// refuses everything: a --local-path edit changes nothing a connection
// test could have an opinion about, so it runs no check and is not
// refused even though this set's host answers nothing.
func TestRun_BackupSetPatchProvesTheConnectionWhenItChangesOne(t *testing.T) {
	t.Run("a changed remote path is proven first", func(t *testing.T) {
		configPath := writeTestConfig(t)
		anOfflineSFTPSet(t, configPath, "api/offline")
		before := readFile(t, configPath)
		args := []string{"backup-set", "--config", configPath, "patch", "api/offline",
			"--remote-path", "/srv/somewhere-else"}

		if got := run(args); got != 1 {
			t.Errorf("run(%v) = %d, want 1: whether this account can read that folder is exactly what a connection test answers, and nothing else on this path asks", args, got)
		}
		if after := readFile(t, configPath); after != before {
			t.Errorf("a refused patch still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("an edit that changes no connection field runs no check", func(t *testing.T) {
		configPath := writeTestConfig(t)
		anOfflineSFTPSet(t, configPath, "api/offline")
		newLocal := filepath.Join(t.TempDir(), "moved")
		args := []string{"backup-set", "--config", configPath, "patch", "api/offline",
			"--local-path", newLocal}

		out := captureStdout(t, func() {
			if got := run(args); got != 0 {
				t.Errorf("run(%v) = %d, want 0: where artifacts land on THIS machine is not something a connection test can prove or disprove", args, got)
			}
		})
		// And it does not announce a skip either. A "not verified" line
		// on an edit where no check would have run is a sentence an
		// operator learns to read as noise, which is how the line stops
		// working on the edits where it means something.
		if strings.Contains(out, "not verified: --no-verify") {
			t.Errorf("an edit that names no connection field announced a skip that never happened:\n%s", out)
		}
	})

	t.Run("--no-verify says so when the edit really did skip one", func(t *testing.T) {
		configPath := writeTestConfig(t)
		anOfflineSFTPSet(t, configPath, "api/offline")
		args := []string{"backup-set", "--config", configPath, "patch", "api/offline",
			"--remote-path", "/srv/somewhere-else", "--no-verify"}

		out := captureStdout(t, func() {
			if got := run(args); got != 0 {
				t.Errorf("run(%v) = %d, want 0: --no-verify is the way past the refusal, not a second refusal", args, got)
			}
		})
		if !strings.Contains(out, "not verified") {
			t.Errorf("--no-verify did not say that nothing was proven:\n%s", out)
		}
		if raw := readFile(t, configPath); !strings.Contains(raw, "connection_unverified: true") {
			t.Errorf("the edit left no mark, so it is indistinguishable from one that was checked:\n%s", raw)
		}
	})
}

// TestRun_BackupSetCreateProvesTheConnectionOnAFreshInstallToo is the
// case #624 asks about by name: a create against a deployment with
// nothing running.
//
// It is the one path with no engine to route to by construction, because
// it was taken precisely because there is no config.yaml at all, and it is
// therefore the path where "silently skip the check and report success"
// would have been easiest to write and hardest to notice. It uses the
// durable path instead: the check runs in this process, through the same
// FirstRun the browser's setup flow calls, and nothing is written when it
// fails.
//
// The assertion is that no configuration exists afterwards. A first
// configuration written and then reported as a failure would be the worst
// of the shapes available here: an operator retries, the retry folds into
// the file the failed attempt left behind, and the deployment is one
// nobody meant to create.
func TestRun_BackupSetCreateProvesTheConnectionOnAFreshInstallToo(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	keyPath := writeTestPrivateKey(t)

	args := unreachableCreateArgs(t, configPath, keyPath, "api/postgres",
		"--state-database", filepath.Join(dir, "state.db"))
	out := captureStdout(t, func() {
		if got := run(args); got != 1 {
			t.Errorf("run(%v) = %d, want 1: a first configuration is still a configuration, and this one names a source that answers nothing", args, got)
		}
	})

	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("a refused first create left a configuration at %s (stat err = %v), so a retry would fold into a file nobody meant to write", configPath, err)
	}
	if !strings.Contains(out, "nothing was written") {
		t.Errorf("the refusal does not say the configuration was not written:\n%s", out)
	}
}

// TestRun_BackupSetTestConnectionIsAVerb closes core/cliecho's own
// standing gap: the check has been the only thing behind POST
// /backup-sets/test-connection since #596 and no verb reached it, so the
// one action an operator most wants from a terminal when a backup stops
// working could only be taken from a browser.
//
// `preflight` answers to the same verb, and that is deliberate rather
// than generous. The destination noun has spelled this `preflight` since
// #443 and is being renamed to `test-connection` under #622 with
// `preflight` kept working; both nouns answering to both spellings means
// an operator who learned either word is right on either noun, and
// nothing anybody scripted stops working.
func TestRun_BackupSetTestConnectionIsAVerb(t *testing.T) {
	for _, verb := range []string{"test-connection", "preflight"} {
		t.Run(verb, func(t *testing.T) {
			configPath := writeTestConfig(t)
			args := []string{"backup-set", "--config", configPath, verb, "production/postgres-primary"}
			out := captureStdout(t, func() {
				if got := run(args); got != 0 {
					t.Errorf("run(%v) = %d, want 0: the fixture's source is a local path this process can read", args, got)
				}
			})
			for _, step := range []string{"credentials", "resolve", "connect", "host_key", "authenticate", "list"} {
				if !strings.Contains(out, step) {
					t.Errorf("`backup-set %s` does not report the %s step:\n%s", verb, step, out)
				}
			}
		})
	}
}

// TestRun_BackupSetTestConnectionClearsTheUnverifiedMark is the last
// clause of #624's acceptance, and the one that makes the mark a state
// rather than a scar: an unverified set that is later proven stops being
// reported as unverified.
//
// Driven end to end through the CLI rather than against the service,
// because the claim is about what an operator can actually do: create
// offline, come back when the host is reachable, run the check, and have
// the deployment stop saying the set was never proven.
func TestRun_BackupSetTestConnectionClearsTheUnverifiedMark(t *testing.T) {
	configPath := writeTestConfig(t)

	// Marked by hand rather than by a --no-verify create, so this test
	// fails for its own reason. A fixture built by the create path would
	// go green on a build where clearing never happened and creation
	// never marked.
	raw := readFile(t, configPath)
	marked := strings.Replace(raw,
		"        completion:\n",
		"        connection_unverified: true\n        completion:\n", 1)
	if marked == raw {
		t.Fatalf("the fixture changed shape and this test no longer marks anything:\n%s", raw)
	}
	if err := os.WriteFile(configPath, []byte(marked), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	args := []string{"backup-set", "--config", configPath, "test-connection", "production/postgres-primary"}
	if got := run(args); got != 0 {
		t.Fatalf("run(%v) = %d, want 0", args, got)
	}
	if after := readFile(t, configPath); strings.Contains(after, "connection_unverified") {
		t.Errorf("a passing check left the set still marked unverified:\n%s", after)
	}
}

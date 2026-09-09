package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The exit statuses `artifacts` and `fetch` answer with, driven against the
// contract the usage block publishes rather than read off what they happen
// to do.
//
// # The rule these cases hold them to
//
// The table in usage() has four rows and two of them are close enough
// together to be got wrong: 2 is "nothing ran, the command line was wrong"
// and 1 is "an ordinary failure". The line between them is whether this
// deployment had to be consulted to know. A string that is not shaped like
// a backup set id is wrong on every deployment there will ever be, so it
// is a 2 and it is answered before the configuration is loaded. A set name
// the configuration does not have, or one two sources share, needs that
// configuration to know, so it is a 1. An answer that is true and empty is
// a 0 and always was.
//
// `retention` already drew the line there for its own operand (issue
// #568), and so do `backup-set create/patch/remove`, `backup-set
// retention` and `unconfigured clear` for theirs. `artifacts` and `fetch`
// did not: a malformed --backup-set went all the way through to the
// service and came back a 1, and `fetch` ignored a surplus argument
// entirely and exited 0 having run a real cycle the operator's command
// line did not describe.
//
// # The two places the rule is applied narrowly, and why
//
// A malformed ARTIFACT id stays a 1. `validate`, `retry`, `quarantine`,
// `restore` and this command's own detail form have all answered that way
// since long before the table existed, and moving five commands at once is
// a bigger change than making these three agree. A flag value that parses
// and then fails validation stays a 1 for the same reason: `settings
// patch`, `backup-set retention` and `retention`'s own override flags all
// send it through the identical config validation the YAML file goes
// through, and answer with what that says.
//
// Both are the reading that matches the majority of existing commands,
// which is the tie-breaker when the table itself does not settle it.

// TestArtifacts_AMalformedBackupSetIdIsAUsageMistake: --backup-set is
// spelled either "source/set" or a bare set name (issue #569), and
// everything else is neither.
//
// The three shapes here are the ones a person actually types: a copied
// artifact id with the file name still on the end, and either half of a
// composite id left empty by a shell that expanded a variable to nothing.
// All three used to reach internal/app, get parsed there, and come back
// through fail() as a 1 with "app: backup set ..." in front of them, which
// is a sentence about this deployment for a command line that is wrong on
// every deployment.
//
// The stderr assertion is the half that makes this more than a number: the
// mode line (#544) is printed by enterReadMode, which runs only after the
// configuration is loaded and the journal is open. Its absence is the
// proof that nothing was opened.
func TestArtifacts_AMalformedBackupSetIdIsAUsageMistake(t *testing.T) {
	configPath := twoSourceDeployment(t)

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"an artifact id pasted whole, file name and all", apiVarArtifact},
		{"a composite id whose set half is empty", "api-server/"},
		{"a composite id whose source half is empty", "/var-backups"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() {
				captureStdout(t, func() {
					code = run([]string{"artifacts", "--config", configPath, "--backup-set", tc.id})
				})
			})
			if code != exitUsage {
				t.Errorf("artifacts --backup-set %q exited %d, want %d; %q is not a backup set id on any deployment, so nothing about this one had to be read to know the command line is wrong\nstderr: %s",
					tc.id, code, exitUsage, tc.id, stderr)
			}
			if strings.Contains(stderr, "mode: ") {
				t.Errorf("artifacts --backup-set %q announced a read mode, so it opened the configuration and the journal before refusing a command line that is wrong whatever they hold\nstderr: %s",
					tc.id, stderr)
			}
			if !strings.Contains(stderr, "is not a backup set id") {
				t.Errorf("artifacts --backup-set %q did not say what shape a backup set id has, which is the one thing the operator needs to fix it\nstderr: %s",
					tc.id, stderr)
			}
		})
	}
}

// TestArtifacts_AnIdNamingNothingIsStillAnOrdinaryFailure is the partner
// the test above needs to mean anything.
//
// A command that answered 2 for every --backup-set it did not like would
// pass every case up there and would have moved the refusal this flag is
// actually for onto the wrong row. "api-server/nope" is a perfectly
// well-formed backup set id; the only reason it is wrong is that this
// deployment does not configure it, and that is the row the table gives to
// a set that is not there.
func TestArtifacts_AnIdNamingNothingIsStillAnOrdinaryFailure(t *testing.T) {
	configPath := twoSourceDeployment(t)

	var code int
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			code = run([]string{"artifacts", "--config", configPath, "--backup-set", "api-server/nope"})
		})
	})
	if code != exitFailure {
		t.Errorf("artifacts --backup-set api-server/nope exited %d, want %d; the id is well formed and it takes this deployment's configuration to know it names nothing\nstderr: %s",
			code, exitFailure, stderr)
	}
}

// TestFetch_AMalformedBackupSetIdIsAUsageMistake is the same rule on the
// other command that grew this flag (issue #569).
//
// `fetch` was worse than `artifacts` here, in two different ways.
// "api-server/var-backups/alternatives.tar" was cut at the FIRST separator
// and the remainder handed to the service as a set name, so the refusal
// that came back named a source ("no configured source named api-server"
// on a deployment that configures exactly that source, or worse, a source
// that is configured and a set that is not). "api-server/" cut to an empty
// set name and fell through to "fetch requires --source and --backup-set",
// told to an operator who had just passed --backup-set.
func TestFetch_AMalformedBackupSetIdIsAUsageMistake(t *testing.T) {
	configPath := twoSourceDeployment(t)

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"an artifact id pasted whole, file name and all", apiVarArtifact},
		{"a composite id whose set half is empty", "api-server/"},
		{"a composite id whose source half is empty", "/var-backups"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() {
				captureStdout(t, func() {
					code = run([]string{"fetch", "--config", configPath, "--backup-set", tc.id})
				})
			})
			if code != exitUsage {
				t.Errorf("fetch --backup-set %q exited %d, want %d; %q is not a backup set id on any deployment\nstderr: %s",
					tc.id, code, exitUsage, tc.id, stderr)
			}
			if !strings.Contains(stderr, "is not a backup set id") {
				t.Errorf("fetch --backup-set %q did not say what shape a backup set id has; the operator who pasted an artifact id needs to be told which part to drop\nstderr: %s",
					tc.id, stderr)
			}
		})
	}
}

// TestFetch_ASurplusArgumentIsRefused is the one case here where the old
// behaviour was not a wrong number but a wrong RUN.
//
// `fetch` takes no operand at all: both of its subjects arrive in flags.
// It parsed with a bare fs.Parse, which stops at the first argument that
// is not a flag and leaves it in fs.Args() for somebody to read, and
// nobody read it. So `fetch --source S --backup-set B prod/db` performed a
// real cycle against S/B, transferred real bytes, and exited 0 without a
// word about the third of the command line it ignored. That is the same
// defect issue #568 fixed on `retention`, one command over, and it is
// sharper here because this command writes.
//
// The table calls a surplus argument a 2, and `artifacts`, `retention`,
// `validate`, `version` and `unconfigured` all already answer that way.
func TestFetch_ASurplusArgumentIsRefused(t *testing.T) {
	configPath := writeTwoSourceTestConfig(t)

	var code int
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			code = run([]string{"fetch", "--config", configPath, "--source", "api-server", "--backup-set", "var-backups", "api-server/var-backups"})
		})
	})
	if code != exitUsage {
		t.Errorf("fetch with a surplus argument exited %d, want %d; it ran a real cycle and said nothing about the argument it dropped\nstderr: %s",
			code, exitUsage, stderr)
	}
}

// TestArtifactsAndFetch_AnEmptyResultIsAnAnswer pins the third row of the
// rule, which is the one nothing was holding.
//
// A read that matched nothing is a true answer and exits 0. Collapsing it
// into a failure is the mistake that reads to an operator as "your backups
// are gone" when what happened is "there are none yet", and it is easy to
// make by accident: every one of these commands has a plausible-looking
// `if len(x) == 0 { return fail(...) }` waiting to be written into it.
//
// `fetch --dry-run` is here rather than a plain `fetch` because the two
// mean different things by empty and only one of them is a read. A cycle
// that had work in front of it and landed none of it is the table's own
// "a cycle that backed nothing up" and exits 1 by way of cycleExit; a
// cycle with nothing waiting on the remote at all is a quiet night and
// exits 0 (internal/app.CycleProgress.NothingGotThrough draws that line,
// and issue #361 is why). --dry-run never runs a cycle, so it is squarely
// a read like the two above it.
func TestArtifactsAndFetch_AnEmptyResultIsAnAnswer(t *testing.T) {
	configPath := writeEmptyRemoteConfig(t)

	for _, tc := range []struct {
		name string
		args []string
		says string
	}{
		{"artifacts, on a journal with nothing in it", []string{"artifacts", "--config", configPath}, "0 artifact(s)"},
		{"artifacts, filtered to a configured set with no rows", []string{"artifacts", "--config", configPath, "--backup-set", "production/postgres-primary"}, "0 artifact(s)"},
		{"fetch --dry-run, against a remote holding nothing", []string{"fetch", "--config", configPath, "--dry-run", "--source", "production", "--backup-set", "postgres-primary"}, "0 object(s) on the remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			var out string
			stderr := captureStderr(t, func() {
				out = captureStdout(t, func() { code = run(tc.args) })
			})
			if code != exitOK {
				t.Errorf("%v exited %d, want %d; finding nothing is an answer, not a failure\nstdout: %s\nstderr: %s", tc.args, code, exitOK, out, stderr)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("%v printed nothing that says the result was empty; a silent zero and a command that fell over look the same to a person reading a terminal\nstdout: %s", tc.args, out)
			}
		})
	}
}

// writeEmptyRemoteConfig is writeTestConfigIn's fixture with the one file
// taken out of the remote, so a read over it has nothing to find and no
// reason to fail.
func writeEmptyRemoteConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + filepath.Join(dir, "local") + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

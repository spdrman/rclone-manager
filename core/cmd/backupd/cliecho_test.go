package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/cliecho"
)

// Every command the Web UI's terminal can print, fed through the real
// dispatcher (issue #599).
//
// The point of echoing a `backupd` command into a panel an
// operator is reading is that they can paste it. A renamed flag, a removed
// one or an argument in the wrong position has to fail here rather than
// being printed at somebody who then pastes it and gets exit 2, so this
// drives the actual `run` in main.go, the actual `commands` map, and the
// actual flag sets each verb declares. Nothing is reimplemented, which is
// the whole reason this test lives in this package rather than beside the
// builders.
//
// # What "parses" means, and why exit 2 is the assertion
//
// usage() pins the exit codes and 2 is exactly "nothing ran: the command
// line was wrong (an unknown command, an unknown flag, a missing or
// surplus argument)". So a correct command line reaches the work and fails
// at the work; a wrong one is refused before anything opens. Pointing
// --config at a file that is not valid YAML is what makes the second half
// of that cheap and side-effect-free: every one of these verbs loads the
// configuration before it does anything, so the whole set exits 1 in
// milliseconds, having written nothing and opened no journal.
//
// The positive control at the bottom is what stops this passing
// vacuously.

// unparseableConfig writes a config file that exists and will not load,
// so every command below gets as far as opening it and no further.
func unparseableConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("this: is: not: valid: yaml\n"), 0o600); err != nil {
		t.Fatalf("writing the config fixture: %v", err)
	}
	return path
}

// dispatch runs one echoed command with --config appended, and reports the
// exit code. Appending is safe for every verb here: they all parse flags
// around their operands (parseFlagsAroundOperands, setup.go), which is
// what lets `validate <id> --config <path>` work at all.
//
// `version` is the one exception and usage() states it: "every command
// except version accepts --config". Appending it there would make this
// test fail on the one command that has no configuration to load, which
// would say nothing about the command and everything about the fixture.
func dispatch(t *testing.T, argv []string, configPath string) int {
	t.Helper()
	if len(argv) < 2 || argv[0] != cliecho.Binary {
		t.Fatalf("an echoed command is %v; every one of them starts with the binary's own name and a verb", argv)
	}
	args := append([]string(nil), argv[1:]...)
	if args[0] != "version" {
		args = append(args, "--config", configPath)
	}
	return run(args)
}

func TestEveryEchoedCommandParses(t *testing.T) {
	// The route is only consulted when it is present, and it is absent
	// here on purpose: a command echoed by a browser that happens to have
	// these set must be identical to one echoed by a browser that does
	// not.
	t.Setenv("BACKUP_MANAGER_API_URL", "")
	t.Setenv("BACKUP_MANAGER_API_USERNAME", "")
	t.Setenv("BACKUP_MANAGER_API_PASSWORD", "")
	configPath := unparseableConfig(t)

	examples := cliecho.Examples()
	if len(examples) == 0 {
		t.Fatal("cliecho reports no examples, so this test would pass having checked nothing")
	}

	checked := 0
	for _, action := range examples {
		line := cliecho.Echo(action)
		if len(line.Command) == 0 {
			// A builder that declined for this request shape. The gap it
			// printed instead is checked by the parity test; there is no
			// command line here to parse.
			continue
		}
		checked++
		t.Run(action.Method+" "+action.Route+" "+strings.Join(line.Command[1:], " "), func(t *testing.T) {
			if code := dispatch(t, line.Command, configPath); code == exitUsage {
				t.Errorf("`%s` is refused as a bad command line (exit %d).\nThe terminal prints this at an operator to paste, so a flag renamed or removed in this package has to break here rather than in their shell.",
					strings.Join(line.Command, " "), code)
			}
		})
	}
	if checked == 0 {
		t.Fatal("every example declined to build a command, so nothing was parsed")
	}
}

// TestEveryEchoedCommandParses_CatchesARenamedFlag is the control the case
// above needs. Every assertion up there is that something did NOT happen,
// and an assertion like that is worth exactly as much as the proof it can
// fire at all.
func TestEveryEchoedCommandParses_CatchesARenamedFlag(t *testing.T) {
	configPath := unparseableConfig(t)

	// A real echoed command with one flag spelled the way a rename would
	// leave it. This is the failure the test above exists to catch, and
	// the exit code has to be 2 rather than 1: nothing ran.
	renamed := []string{cliecho.Binary, "backup-set", "patch", "api-server/var-backups", "--stale-after-seconds", "172800"}
	if code := dispatch(t, renamed, configPath); code != exitUsage {
		t.Fatalf("a command carrying a flag this binary does not declare exited %d, and the test above only ever asserts that a command is NOT refused; if a bad flag is not refused, that test is checking nothing",
			code)
	}

	// And the same command with the flag as it is actually spelled gets
	// past the parse, which is what makes the failure above about the
	// flag rather than about the fixture.
	correct := []string{cliecho.Binary, "backup-set", "patch", "api-server/var-backups", "--stale-after", "48h"}
	if code := dispatch(t, correct, configPath); code == exitUsage {
		t.Fatalf("the same command with the real flag name is also refused (exit %d), so the case above proves nothing about the flag", code)
	}
}

// What each command exits with, and what it says on the way out.
//
// The exit status is half a contract on its own. A refusal that exits 2
// without explaining itself is as bad for the operator as one that exits 0,
// so every row here asserts both, and a row that expects nothing on stderr
// means exactly that: a successful command may not print a warning nobody
// reads.
//
// The tables are built around agreement rather than around coverage. The
// interesting failures in this binary have all been two commands answering
// the same question differently: a filter naming a set that does not exist
// has to be refused in the same words wherever it is typed, and a flag has
// to work on either side of an operand for every command that takes one.
// Each table therefore pairs its refusals with a positive control, because
// a command that refused everything would satisfy the refusals alone.
package main

import (
	"strings"
	"testing"
)

// checkExitAndStderr is the shared assertion for the three tables below:
// it runs one argv through run() and asserts both the exit code and what
// the binary said about it.
//
// It reuses retention_flags_test.go's captureStderr, which swaps
// os.Stderr for a pipe. Both writers these tables care about resolve
// os.Stderr at call time (this package's own fail/usageError write to it
// directly, and a flag.FlagSet with no explicit output falls back to it
// inside Parse), so one swap catches both, which is what lets these
// tests assert *why* a command refused rather than only that it did.
//
// wantStderr is a substring, and an empty one means "this command must
// not complain at all", which keeps each table's success rows from
// passing while quietly printing a warning nobody reads.
func checkExitAndStderr(t *testing.T, args []string, wantExit int, wantStderr string) {
	t.Helper()

	code := -1
	stderr := captureStderr(t, func() { code = run(args) })

	if code != wantExit {
		t.Errorf("run(%v) = %d, want %d\nstderr: %s", args, code, wantExit, stderr)
	}
	switch {
	case wantStderr == "" && strings.TrimSpace(stderr) != "":
		t.Errorf("run(%v) wrote to stderr, want nothing:\n%s", args, stderr)
	case wantStderr != "" && !strings.Contains(stderr, wantStderr):
		t.Errorf("run(%v) stderr = %q, want it to contain %q", args, stderr, wantStderr)
	}
}

// TestRun_ArtifactsRefusesAnUnconfiguredFilter is issue #187 at the CLI
// boundary an operator actually types at.
//
// The refusal has to name the set, and it has to be the same refusal
// `fetch` already gives for the identical name: the last row runs fetch
// with that name and asserts the identical message, so this is a proof
// that the two commands agree rather than a second, separately-worded
// answer to the same question. The first two rows are the positive
// controls that a filter naming something real still lists, and still
// exits 0, because a refusal that fired for everything would satisfy the
// refusal rows on its own.
func TestRun_ArtifactsRefusesAnUnconfiguredFilter(t *testing.T) {
	cfg := writeTestConfig(t)

	const (
		missingSet    = "backup-manager: app: no configured backup set named production/no-such-set"
		missingSource = "backup-manager: app: no configured source named no-such-source"
	)

	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantStderr string
	}{
		{
			name:     "no filter at all",
			args:     []string{"artifacts", "--config", cfg},
			wantExit: 0,
		},
		{
			name:     "a configured source and backup set",
			args:     []string{"artifacts", "--config", cfg, "--source", "production", "--backup-set", "postgres-primary"},
			wantExit: 0,
		},
		{
			name:       "an unconfigured backup set",
			args:       []string{"artifacts", "--config", cfg, "--source", "production", "--backup-set", "no-such-set"},
			wantExit:   1,
			wantStderr: missingSet,
		},
		{
			name:       "an unconfigured source",
			args:       []string{"artifacts", "--config", cfg, "--source", "no-such-source"},
			wantExit:   1,
			wantStderr: missingSource,
		},
		{
			name:       "fetch refuses the identical name, identically",
			args:       []string{"fetch", "--config", cfg, "--source", "production", "--backup-set", "no-such-set"},
			wantExit:   1,
			wantStderr: missingSet,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkExitAndStderr(t, tc.args, tc.wantExit, tc.wantStderr)
		})
	}
}

// TestRun_ValidateAcceptsFlagsOnEitherSideOfItsOperand is issue #188.
//
// The usage banner documents `validate <source/backup-set/artifact>` and,
// separately, that every command except version accepts --config. Neither
// line puts them in an order, so both orders have to work. The genuine
// arity errors keep their own message: the rows with no operand and with
// two operands are what stops this fix from being "stop counting
// arguments", and the unknown-flag row is what stops it from being "treat
// anything after the operand as another operand".
func TestRun_ValidateAcceptsFlagsOnEitherSideOfItsOperand(t *testing.T) {
	cfg := writeTestConfig(t)
	if got := run([]string{"run", "--config", cfg}); got != 0 {
		t.Fatalf("run([\"run\", \"--config\", %q]) = %d, want 0 (this test needs one committed artifact)", cfg, got)
	}
	const id = "production/postgres-primary/backup.dump"
	const arity = "backup-manager: validate takes exactly one argument: <source/backup-set/artifact>"

	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantStderr string
	}{
		{
			name:     "the flag before the operand",
			args:     []string{"validate", "--config", cfg, id},
			wantExit: 0,
		},
		{
			name:     "the flag after the operand",
			args:     []string{"validate", id, "--config", cfg},
			wantExit: 0,
		},
		{
			// The one ordering rule that survives: "--" still ends flag
			// parsing, so an operand that looks like a flag can be written
			// after it.
			name:     "an operand written after the -- terminator",
			args:     []string{"validate", "--config", cfg, "--", id},
			wantExit: 0,
		},
		{
			name:       "no operand at all is still an arity error",
			args:       []string{"validate", "--config", cfg},
			wantExit:   2,
			wantStderr: arity,
		},
		{
			name:       "two operands are still an arity error",
			args:       []string{"validate", id, id, "--config", cfg},
			wantExit:   2,
			wantStderr: arity,
		},
		{
			name:       "an unknown flag after the operand is still an unknown flag",
			args:       []string{"validate", id, "--bogus", "--config", cfg},
			wantExit:   2,
			wantStderr: "flag provided but not defined: -bogus",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkExitAndStderr(t, tc.args, tc.wantExit, tc.wantStderr)
		})
	}
}

// TestRun_VersionRefusesWhatItDoesNotUnderstand is issue #189.
//
// Every other command in this binary answers a flag it does not know with
// exit 2, and the last row runs one of them to prove that convention is
// really there rather than assumed. version was the one command that
// swallowed the flag instead, which turns `version --json` (the obvious
// thing for a script to guess at) into exit 0 plus human-readable text,
// and moves the failure to whatever tries to parse it.
func TestRun_VersionRefusesWhatItDoesNotUnderstand(t *testing.T) {
	cfg := writeTestConfig(t)

	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantStderr string
	}{
		{
			name:     "no arguments at all",
			args:     []string{"version"},
			wantExit: 0,
		},
		{
			name:       "the one flag the usage banner says it does not take",
			args:       []string{"version", "--config", "/nowhere/config.yaml"},
			wantExit:   2,
			wantStderr: "flag provided but not defined: -config",
		},
		{
			name:       "a flag a script would reasonably guess at",
			args:       []string{"version", "--json"},
			wantExit:   2,
			wantStderr: "flag provided but not defined: -json",
		},
		{
			name:       "an operand it documents no use for",
			args:       []string{"version", "frobnicate"},
			wantExit:   2,
			wantStderr: "backup-manager: version takes no arguments",
		},
		{
			name:       "the convention version was missing, shown on another command",
			args:       []string{"check", "--config", cfg, "--bogus"},
			wantExit:   2,
			wantStderr: "flag provided but not defined: -bogus",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkExitAndStderr(t, tc.args, tc.wantExit, tc.wantStderr)
		})
	}
}

// TestRun_CatalogAcceptsFlagsOnEitherSideOfItsSubcommand is issue #213.
//
// `catalog` used to read its subcommand out of args[0] before it parsed
// anything, so `catalog --config X rebuild` was refused with `unknown
// subcommand "--config"`: a message that names the wrong thing entirely,
// since the subcommand is fine and the parser had simply not reached it.
// #188 fixed the same shape for `validate` and left the helper behind,
// and no CLI documentation orders a command's flags against its
// subcommand, so both orders have to work.
//
// The rows that keep this from being "stop checking the subcommand" are
// the two genuine errors, which must keep their exact wording and their
// exit code: no subcommand at all, and a subcommand that really is
// unknown. The last two rows keep "--" meaning what it has always meant,
// in both directions: it ends flag parsing without hiding the subcommand
// behind it, and a flag written after it is an operand, so it is an
// unknown subcommand rather than a flag.
func TestRun_CatalogAcceptsFlagsOnEitherSideOfItsSubcommand(t *testing.T) {
	cfg := writeTestConfig(t)
	if got := run([]string{"run", "--config", cfg}); got != 0 {
		t.Fatalf("run([\"run\", \"--config\", %q]) = %d, want 0 (this test needs one committed artifact)", cfg, got)
	}

	const (
		missing = `backup-manager: catalog: missing subcommand (expected "rebuild")`
		unknown = `backup-manager: catalog: unknown subcommand "frobnicate" (expected "rebuild")`
	)

	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantStderr string
	}{
		{
			name:     "the subcommand before the flag, the order that already worked",
			args:     []string{"catalog", "rebuild", "--config", cfg},
			wantExit: 0,
		},
		{
			name:     "the flag before the subcommand, issue #213's own reproduction",
			args:     []string{"catalog", "--config", cfg, "rebuild"},
			wantExit: 0,
		},
		{
			name:     "the subcommand's own flag still works where it always did",
			args:     []string{"catalog", "rebuild", "--dry-run", "--config", cfg},
			wantExit: 0,
		},
		{
			name:     "every flag before the subcommand",
			args:     []string{"catalog", "--config", cfg, "--dry-run", "rebuild"},
			wantExit: 0,
		},
		{
			name:       "no subcommand at all keeps its own message",
			args:       []string{"catalog", "--config", cfg},
			wantExit:   2,
			wantStderr: missing,
		},
		{
			name:       "a genuinely unknown subcommand keeps its own message",
			args:       []string{"catalog", "--config", cfg, "frobnicate"},
			wantExit:   2,
			wantStderr: unknown,
		},
		{
			name:       "and keeps it with the flag written after it too",
			args:       []string{"catalog", "frobnicate", "--config", cfg},
			wantExit:   2,
			wantStderr: unknown,
		},
		{
			name:     "a subcommand written after the -- terminator is still found",
			args:     []string{"catalog", "--config", cfg, "--", "rebuild"},
			wantExit: 0,
		},
		{
			// "--" really does end flag parsing: --dry-run after it is an
			// operand, so it is an unknown subcommand, not a flag.
			name:       "a flag written after -- is an operand, not a flag",
			args:       []string{"catalog", "--config", cfg, "--", "--dry-run"},
			wantExit:   2,
			wantStderr: `backup-manager: catalog: unknown subcommand "--dry-run" (expected "rebuild")`,
		},
		{
			name:       "an unknown flag is still an unknown flag",
			args:       []string{"catalog", "rebuild", "--bogus", "--config", cfg},
			wantExit:   2,
			wantStderr: "flag provided but not defined: -bogus",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkExitAndStderr(t, tc.args, tc.wantExit, tc.wantStderr)
		})
	}
}

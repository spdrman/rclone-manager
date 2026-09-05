// Issue #435 at the command, where two things can go wrong that no test
// inside internal/app can see.
//
// The first is the one this file was written for. Service.MediumStore is
// filled in from the transport adapter (app.New), and `validate` opened
// its service with withTransport false, because it was a purely local
// command when it was written. So the command could not have reached a
// medium even after internal/app learned how: every moved artifact, in
// every deployment, would have come back with #434's "this deployment has
// no way to reach a storage medium" refusal, forever, and the operator it
// told that to could have done nothing about it. That is the shape of
// wiring bug a unit test cannot reach, because the unit test sets the
// field itself.
//
// The second is the flag. `--content` has to actually be a flag on this
// subcommand's flag set, and it has to work on either side of the operand
// the way every other flag on this binary does.
package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TestRun_ValidateReachesTheMediumRatherThanRefusingForWantOfOne is the
// wiring assertion.
//
// The fixture's medium reads its credentials from an environment variable
// nothing sets, so the check cannot succeed. That is the point: what is
// under test is WHICH refusal comes back. "I could not reach that bucket"
// means the command has a medium store and used it. "This deployment has
// no way to reach a storage medium" means it does not have one at all,
// which is what withTransport false produces and what an operator can do
// nothing whatever about.
func TestRun_ValidateReachesTheMediumRatherThanRefusingForWantOfOne(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	artifact := stageMovedArtifact(t, configPath, "moved.dump")

	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if got := run([]string{"validate", "--config", configPath, artifact.String()}); got != 1 {
				t.Errorf("validate = %d, want 1: the medium could not be reached, which is a refusal", got)
			}
		})
	})

	if strings.Contains(stderr, "no way to reach a storage medium") {
		t.Errorf("validate refused for want of a medium store, so this command still cannot reach a medium at all:\n%s", stderr)
	}
	if !strings.Contains(stderr, "cold_offsite") {
		t.Errorf("the refusal does not name the medium it could not reach, so an operator cannot tell which bucket to look at:\n%s", stderr)
	}
	mustNotHaveQuarantined(t, configPath, artifact.String())
}

// TestRun_ValidateWithContentStillRefusesAnUnreachableMedium is the same
// assertion on the expensive path, and it is also where the flag is proved
// to exist: a --content this flag set did not declare is a parse error
// (exit 2), not the refusal below.
func TestRun_ValidateWithContentStillRefusesAnUnreachableMedium(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	artifact := stageMovedArtifact(t, configPath, "moved.dump")

	for _, args := range [][]string{
		{"validate", "--config", configPath, "--content", artifact.String()},
		{"validate", "--config", configPath, artifact.String(), "--content"},
	} {
		args := args
		t.Run(strings.Join(args[3:], " "), func(t *testing.T) {
			var stderr string
			captureStdout(t, func() {
				stderr = captureStderr(t, func() {
					if got := run(args); got != 1 {
						t.Errorf("validate --content = %d, want 1; 2 means the flag is not declared on this subcommand", got)
					}
				})
			})
			if strings.Contains(stderr, "flag provided but not defined") {
				t.Errorf("--content is not a flag on validate:\n%s", stderr)
			}
			mustNotHaveQuarantined(t, configPath, artifact.String())
		})
	}
}

// TestUsage_NamesValidatesContentFlag keeps the flag discoverable. The
// usage block is what --help prints and what the black-box guard in the
// tests repo reads, and a flag that costs money is exactly the one an
// operator must not have to find by reading source.
func TestUsage_NamesValidatesContentFlag(t *testing.T) {
	out := captureStderr(t, usage)
	if !strings.Contains(out, "--content") {
		t.Errorf("usage() does not mention validate's --content flag:\n%s", out)
	}
	if !strings.Contains(out, "egress") {
		t.Errorf("usage() mentions --content without saying it costs egress, which is the only reason it is a flag:\n%s", out)
	}
}

// mustNotHaveQuarantined re-reads the journal and refuses any quarantine
// edge. It is the half of every case above that matters most: a refusal
// that quarantined the artifact on its way out would be #434 all over
// again, reached by a different route.
func mustNotHaveQuarantined(t *testing.T, configPath, id string) {
	t.Helper()
	ctx := context.Background()
	j, err := state.Open(ctx, filepath.Join(filepath.Dir(configPath), "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	artifact, err := app.ParseArtifactID(id)
	if err != nil {
		t.Fatalf("ParseArtifactID(%q): %v", id, err)
	}
	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Complete) {
		t.Fatalf("journal state = %q after a refused validate, want %q: an unreachable medium is not evidence that a backup is gone",
			rec.State, lifecycle.Complete)
	}
}

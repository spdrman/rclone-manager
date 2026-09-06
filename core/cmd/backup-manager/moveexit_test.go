package main

import (
	"strings"
	"testing"
)

// `run` is what a cron job invokes, and its exit status is the only thing
// that cron job reads. Until this file existed, a cycle in which every
// single move was refused exited 0 with nothing on stderr, so a
// deployment whose artifacts were supposed to be offsite and were not
// looked exactly like one that was working.
//
// The fixture is not contrived. writeOffsiteTestConfig declares a medium
// whose credentials come from an environment variable, that variable is
// not set, and the daily tier names the medium, so the artifact `run`
// ingests a moment earlier is immediately due to move somewhere it cannot
// reach. That is a real deployment one typo away.

// TestRun_ExitsNonZeroWhenEveryMoveWasRefused is the finding at the exit
// status.
func TestRun_ExitsNonZeroWhenEveryMoveWasRefused(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if got := run([]string{"run", "--config", configPath}); got != 1 {
				t.Errorf("run = %d, want 1: every move this cycle planned was refused, and this exit status is the only thing a cron job reads", got)
			}
		})
	})

	if !strings.Contains(stderr, "moved nothing") {
		t.Errorf("stderr does not say the cycle moved nothing:\n%s", stderr)
	}
	if !strings.Contains(stderr, "BACKUP_S3_COLD") {
		t.Errorf("stderr does not carry the engine's own reason, so an operator still has to go and find it:\n%s", stderr)
	}
}

// TestRun_SaysNothingOnStdoutAboutMoves is the rule setup.go's cycleExit
// already follows, restated where the new writer is added.
//
// This binary's stdout is FR-23's newline-delimited JSON event stream. A
// sentence in the middle of it breaks every consumer that parses the
// stream a line at a time, so diagnostics go to stderr, always.
func TestRun_SaysNothingOnStdoutAboutMoves(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	stdout := captureStdout(t, func() {
		captureStderr(t, func() { run([]string{"run", "--config", configPath}) })
	})

	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			t.Errorf("stdout carries a line that is not a JSON event: %q", line)
		}
	}
}

// TestRun_StillExitsZeroInADeploymentWithNoMedium is FR-35's
// compatibility promise at the one place it is load bearing outside this
// repository.
//
// `run`'s exit status is pinned by the black-box contract suite in
// spdrman/rclone-manager-tests, and every case there is a medium-free
// deployment. A medium-free deployment attempts no moves, so the new
// verdict's denominator is zero and it can never be true, which is what
// keeps those cases exiting 0 by arithmetic rather than by a guard
// somebody has to remember.
func TestRun_StillExitsZeroInADeploymentWithNoMedium(t *testing.T) {
	configPath := writeTestConfig(t)

	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if got := run([]string{"run", "--config", configPath}); got != 0 {
				t.Errorf("run = %d, want 0 for a deployment that declares no storage medium", got)
			}
		})
	})
	if strings.Contains(stderr, "moved nothing") {
		t.Errorf("a medium-free deployment was told it moved nothing, which is true and is not news:\n%s", stderr)
	}
}

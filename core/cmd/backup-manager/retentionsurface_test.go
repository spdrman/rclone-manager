package main

import (
	"strings"
	"testing"
)

// Two things about `retention <source/backup-set>` that the operand itself
// (issue #568) did not settle: what the one-set form says about the sets
// it deliberately leaves out, and where its refusals land on the exit-code
// table the usage block publishes.

// TestRun_RetentionOneSetPointsAtTheFormThatShowsUngovernedSets is the
// gap the operand opened.
//
// The issue #418 appendix, "retained under no policy at all", belongs to
// the whole-deployment form and stays there: an operator who named one set
// asked about that set, and a list of other sets is an answer to a
// question they did not ask. But the operator who types `retention
// prod/db` every day and never types it bare then never meets that list at
// all, and what it is about is backups nothing retains, reconciles or
// expires. Those are the ones most worth knowing about and the least
// likely to announce themselves.
//
// So the one-set form says the list exists and says where to see it. One
// line, and only when there is something behind it, which is the same rule
// the appendix itself follows: a deployment that has never removed a
// backup set prints exactly what it printed before.
func TestRun_RetentionOneSetPointsAtTheFormThatShowsUngovernedSets(t *testing.T) {
	configPath := writeOperandTestConfig(t)
	if code := run([]string{"run", "--config", configPath}); code != 0 {
		t.Fatalf("run = %d, want 0; this test needs a finished backup under each set", code)
	}
	captureStdout(t, func() {
		if code := run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-replica"}); code != 0 {
			t.Fatalf("backup-set remove = %d, want 0; this test needs one set whose configuration is gone and whose backups are not", code)
		}
	})

	out := captureStdout(t, func() {
		captureStderr(t, func() {
			if code := run([]string{"retention", "--config", configPath, "production/postgres-primary"}); code != 0 {
				t.Errorf("retention production/postgres-primary = %d, want 0", code)
			}
		})
	})

	if !strings.Contains(out, "retention` with no argument") {
		t.Errorf("the one-set preview does not tell the operator that the bare form lists the backup sets nothing governs, so somebody who only ever names a set never learns those exist:\n%s", out)
	}
	if !strings.Contains(out, "issue #418") {
		t.Errorf("the pointer does not cite the issue the appendix it points at is filed under, which is how a reader gets from this line to the whole argument:\n%s", out)
	}
	if strings.Contains(out, "production/postgres-replica") {
		t.Errorf("the one-set preview named the removed set. It is a pointer at the other form, not the appendix itself; naming the set here is the answer to the question the operator did not ask:\n%s", out)
	}
}

// TestRun_RetentionOneSetSaysNothingExtraWhenEverySetIsConfigured is the
// control the case above needs.
//
// A pointer printed unconditionally would satisfy that test and would put
// a line about removed backup sets on every one-set preview of every
// deployment that has never removed one, which is nearly all of them. The
// appendix has followed that rule since #418 for a reason that has not
// changed: absence of the line is what a deployment with nothing to say
// prints.
func TestRun_RetentionOneSetSaysNothingExtraWhenEverySetIsConfigured(t *testing.T) {
	configPath := writeOperandTestConfig(t)
	if code := run([]string{"run", "--config", configPath}); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}

	out := captureStdout(t, func() {
		captureStderr(t, func() {
			if code := run([]string{"retention", "--config", configPath, "production/postgres-primary"}); code != 0 {
				t.Errorf("retention production/postgres-primary = %d, want 0", code)
			}
		})
	})

	if strings.Contains(out, "with no argument") {
		t.Errorf("the one-set preview points at a list that would be empty on this deployment:\n%s", out)
	}
}

// TestRun_RetentionRefusesAMalformedIdBeforeItOpensAnything is the exit
// code rule stated as behaviour rather than as a comment.
//
// 2 and 1 are close enough together to be got wrong, and the line between
// them is whether this deployment had to be read to know. "postgres-primary"
// with no source half is not a backup set id anywhere, so it is a 2 and
// this command answers it before it loads a configuration or opens a
// journal. The mode line (#544) is what proves the second half: enterReadMode
// prints it, and enterReadMode runs only after both are open.
//
// `artifacts` and `fetch` draw the line in the same place now, and so do
// `backup-set create/patch/remove/retention` and `unconfigured clear`. This
// command is where it was drawn first (#568).
func TestRun_RetentionRefusesAMalformedIdBeforeItOpensAnything(t *testing.T) {
	configPath := writeOperandTestConfig(t)

	var code int
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			code = run([]string{"retention", "--config", configPath, "postgres-primary"})
		})
	})

	if code != exitUsage {
		t.Errorf("retention postgres-primary exited %d, want %d\nstderr: %s", code, exitUsage, stderr)
	}
	if strings.Contains(stderr, "mode: ") {
		t.Errorf("retention announced a read mode, so it opened the configuration and the journal before refusing a command line no deployment would have accepted\nstderr: %s", stderr)
	}
}

// TestRun_RetentionAnEmptyPreviewIsAnAnswer pins the third row of the same
// rule, which nothing was holding.
//
// A configured backup set with no finished backups under it yet has a
// perfectly good answer and it is empty. Reporting that as a failure is
// the mistake that reads to an operator as "your backups are gone" when
// what happened is "there are none yet", and on this command in
// particular, which is the one somebody reads before a deletion, it is
// the difference between a quiet answer and an alarm.
//
// Both forms, because they compute the report differently: the bare one
// through RetentionPreviewAll, the named one through RetentionPreview.
func TestRun_RetentionAnEmptyPreviewIsAnAnswer(t *testing.T) {
	configPath := writeOperandTestConfig(t)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"the whole deployment", []string{"retention", "--config", configPath}},
		{"one named backup set", []string{"retention", "--config", configPath, "production/postgres-primary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			var out string
			stderr := captureStderr(t, func() {
				out = captureStdout(t, func() { code = run(tc.args) })
			})
			if code != exitOK {
				t.Errorf("%v exited %d, want %d; a backup set with nothing in it yet is an answer, not a failure\nstdout: %s\nstderr: %s", tc.args, code, exitOK, out, stderr)
			}
			if !strings.Contains(out, "(no managed, completed backups yet)") {
				t.Errorf("%v printed nothing saying the preview was empty; a silent zero and a command that fell over look the same in a terminal\nstdout: %s", tc.args, out)
			}
		})
	}
}

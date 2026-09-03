package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/service"
)

// setNamesIn reads configPath the way a fresh boot would and lists the
// backup set ids it declares.
func setNamesIn(t *testing.T, configPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	var out []string
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			out = append(out, src.Name+"/"+bs.Name)
		}
	}
	return out
}

// TestRun_BackupSetRemove_TakesTheSetOutOfTheFileAndSaysWhatStayed is the
// CLI's half of issue #391.
//
// The verb exists so the operator standing at a real NAS with no browser
// can do what the Web UI does, which is the same reason `create` and
// `patch` are here. What it prints matters as much as what it writes: a
// removal that only said "removed" would leave the operator with no way
// to see the half of the promise that is not visible in the file
// afterwards, which is that the backups are still there.
func TestRun_BackupSetRemove_TakesTheSetOutOfTheFileAndSaysWhatStayed(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := setNamesIn(t, configPath); len(got) != 1 {
		t.Fatalf("the fixture declares %v, want exactly one set; without one this test cannot tell removal from a no-op", got)
	}

	var out string
	code := 1
	out = captureStdout(t, func() {
		code = run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"})
	})
	if code != 0 {
		t.Fatalf("run(backup-set remove) = %d, want 0. Output:\n%s", code, out)
	}

	if got := setNamesIn(t, configPath); len(got) != 0 {
		t.Errorf("the configuration still declares %v after the removal", got)
	}
	if !strings.Contains(out, "production/postgres-primary") {
		t.Errorf("the output does not name what it removed:\n%s", out)
	}
	if !strings.Contains(out, "stay on storage") {
		t.Errorf("the output does not say the backups stay on storage, which is the half of this an operator cannot see in the file:\n%s", out)
	}
}

// TestRun_BackupSetRemove_UnknownSetFailsAndWritesNothing. The refusal
// matters more than the exit code on its own: a removal that answered
// "fine" to a mistyped name, and left the real set running, is the exact
// shape of the defect this issue is about.
func TestRun_BackupSetRemove_UnknownSetFailsAndWritesNothing(t *testing.T) {
	configPath := writeTestConfig(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	out := captureStderr(t, func() {
		if code := run([]string{"backup-set", "--config", configPath, "remove", "production/typo"}); code == 0 {
			t.Error("run(backup-set remove) on a set that does not exist = 0, want a failure")
		}
	})
	if !strings.Contains(out, "production/typo") {
		t.Errorf("the refusal does not name what it could not find:\n%s", out)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a refused removal rewrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRun_BackupSetRemove_RefusesEveryOtherVerbsFlags. remove names a set
// and removes it, so every flag this command declares belongs to another
// verb. Accepting one silently is the failure
// TestRun_BackupSetVerbsRefuseEachOthersFlags already describes, and it
// is worse here: `backup-set remove a/b --read-only` exiting 0 having
// removed the set is a command that did something other than what it was
// asked.
func TestRun_BackupSetRemove_RefusesEveryOtherVerbsFlags(t *testing.T) {
	for _, wrong := range [][]string{
		{"--read-only"},
		{"--disabled"},
		{"--run"},
		{"--acknowledge-repoint"},
		{"--host", "elsewhere.internal"},
		{"--local-path", "/tmp/elsewhere"},
		{"--include", "*.sql"},
	} {
		configPath := writeTestConfig(t)
		args := append([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"}, wrong...)

		out := captureStderr(t, func() {
			if code := run(args); code != 2 {
				t.Errorf("run(%v) = %d, want 2 (a usage error)", args, code)
			}
		})
		if !strings.Contains(out, strings.TrimPrefix(wrong[0], "--")) {
			t.Errorf("the refusal for %s does not name the flag it refused: %q", wrong[0], out)
		}
		if got := setNamesIn(t, configPath); len(got) != 1 {
			t.Errorf("the refused removal still changed the configuration, which now declares %v", got)
		}
	}

	// The control. Without it every case above would also pass against a
	// remove verb that refused every invocation it was ever given.
	configPath := writeTestConfig(t)
	if code := run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"}); code != 0 {
		t.Fatalf("the same removal with no extra flag = %d, want 0; the refusals above prove nothing without this", code)
	}
}

// TestRun_BackupSetRemove_ArtifactsStillListsWhatStayed pins the sentence
// the verb prints: the backups "stay listed by `backup-manager
// artifacts`". The unfiltered list has to ASK for a removed set's rows
// (internal/app widens only when asked, because the quarantine read is
// the same call and must not), so a terminal that forgot to ask would
// print that sentence and contradict it on the very next command.
func TestRun_BackupSetRemove_ArtifactsStillListsWhatStayed(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run(run) = %d, want 0; without a cycle there is nothing on record to survive the removal", got)
	}
	before := captureStdout(t, func() { _ = run([]string{"artifacts", "--config", configPath}) })
	if !strings.Contains(before, "production/postgres-primary/backup.dump") {
		t.Fatalf("the cycle left no artifact on record, so nothing below would be evidence:\n%s", before)
	}

	_ = captureStdout(t, func() {
		if code := run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"}); code != 0 {
			t.Fatalf("run(backup-set remove) = %d, want 0", code)
		}
	})

	after := captureStdout(t, func() {
		if code := run([]string{"artifacts", "--config", configPath}); code != 0 {
			t.Errorf("run(artifacts) after the removal = %d, want 0", code)
		}
	})
	if !strings.Contains(after, "production/postgres-primary/backup.dump") {
		t.Errorf("`artifacts` no longer lists the removed set's backup, which the removal just promised it would:\n%s", after)
	}
}

// removeFake is the seam backupSetRemoveWith drives, so the one failure
// mode the real service cannot be made to produce on demand (a journal
// read failing while the configuration write works) can be produced.
type removeFake struct {
	listErr error
	listed  int
	removed []string
}

func (f *removeFake) ListArtifacts(_ context.Context, _ service.ArtifactFilter) ([]service.Artifact, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return make([]service.Artifact, f.listed), nil
}

func (f *removeFake) RemoveBackupSet(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}

// TestBackupSetRemove_ACountThatCannotBeTakenDoesNotStopTheRemoval. The
// verb counts what stays before it removes, because afterwards that
// filter names a set the configuration no longer has. That count is a
// courtesy. A journal read failing must not turn into "the removal
// failed": the operator asked for a set to stop collecting, the
// configuration can be written, and refusing to write it because a
// number could not be looked up leaves the set running for the sake of
// a sentence.
func TestBackupSetRemove_ACountThatCannotBeTakenDoesNotStopTheRemoval(t *testing.T) {
	fake := &removeFake{listErr: errors.New("journal: database is locked")}
	var out bytes.Buffer

	code := backupSetRemoveWith(context.Background(), fake, "production/postgres-primary", &out)

	if code != 0 {
		t.Errorf("exit code = %d, want 0; the removal itself succeeded", code)
	}
	if len(fake.removed) != 1 || fake.removed[0] != "production/postgres-primary" {
		t.Errorf("RemoveBackupSet was called with %v, want exactly [production/postgres-primary]; a failed count aborted the removal", fake.removed)
	}
	text := out.String()
	if !strings.Contains(text, "removed the configuration for production/postgres-primary") {
		t.Errorf("the output does not say what it removed:\n%s", text)
	}
	if !strings.Contains(text, "could not count") || !strings.Contains(text, "stay on storage") {
		t.Errorf("the output neither admits the count could not be taken nor says the backups are still there:\n%s", text)
	}
	if strings.Contains(text, "0 backup(s)") {
		t.Errorf("the output claims 0 backups stayed, which is a specific and reassuring number for a count that was never taken:\n%s", text)
	}
}

// The control for the case above: with the count available, the verb
// reports it, so the test above is not passing against a verb that never
// counts anything.
func TestBackupSetRemove_ReportsTheCountWhenItCanBeTaken(t *testing.T) {
	fake := &removeFake{listed: 3}
	var out bytes.Buffer

	if code := backupSetRemoveWith(context.Background(), fake, "production/postgres-primary", &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "3 backup(s) stay on storage") {
		t.Errorf("the output does not report the 3 backups that stayed:\n%s", out.String())
	}
}

// TestRun_BackupSetRemove_RefusesEveryDeclaredFlagButConfig walks the
// flag set itself rather than a hand-picked seven, so a flag declared
// tomorrow is checked today. With the refusal expressed as "everything
// but --config" this cannot fail by omission; the test is here so that
// property is stated somewhere a widening of the allow-list would trip.
func TestRun_BackupSetRemove_RefusesEveryDeclaredFlagButConfig(t *testing.T) {
	var names []string
	declareBackupSetFlags().fs.VisitAll(func(fl *flag.Flag) {
		if fl.Name != "config" {
			names = append(names, fl.Name)
		}
	})
	if len(names) < 15 {
		t.Fatalf("declareBackupSetFlags declares %d flags besides --config (%v); that is fewer than the verbs are known to take, so the walk is not seeing them", len(names), names)
	}

	for _, name := range names {
		configPath := writeTestConfig(t)
		args := []string{"backup-set", "--config", configPath, "remove", "production/postgres-primary", "--" + name}
		if fl := declareBackupSetFlags().fs.Lookup(name); fl != nil && !isBoolFlag(fl) {
			args = append(args, "value")
		}
		out := captureStderr(t, func() {
			if code := run(args); code != 2 {
				t.Errorf("run(%v) = %d, want 2 (a usage error); remove accepted --%s", args, code, name)
			}
		})
		if !strings.Contains(out, name) {
			t.Errorf("the refusal for --%s does not name it: %q", name, out)
		}
		if got := setNamesIn(t, configPath); len(got) != 1 {
			t.Errorf("--%s: the refused removal still changed the configuration, which now declares %v", name, got)
		}
	}
}

// isBoolFlag reports whether fl takes no value, the way the flag package
// itself decides it.
func isBoolFlag(fl *flag.Flag) bool {
	b, ok := fl.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

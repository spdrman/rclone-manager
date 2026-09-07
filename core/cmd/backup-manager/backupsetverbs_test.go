package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hazard that only exists because the verbs share one flag set.
//
// Separately, each verb declared its own flags, so passing another verb's
// was an unknown flag and the parser refused it. Merged, every flag parses
// for every verb, and the failure mode changes from a refusal into silence:
// a verb accepting a flag it does not act on exits 0 having changed nothing
// the operator asked for.
//
// So this is the composition's own test rather than any one verb's. It is
// paired with a control that the same invocation without the wrong flag
// really does succeed, because a command that refused everything would pass
// the refusal half on its own.

// TestRun_BackupSetVerbsRefuseEachOthersFlags is the composition's own
// test, and it exists because merging #350's `patch` and #356's `create`
// into one command created a hazard neither branch had on its own.
//
// Separately, each verb declared only its own flags, so passing the
// other's was an unknown flag and `flag` refused it. Together they share
// one FlagSet, so every flag parses for every verb, and the failure
// becomes silent: `backup-set patch --read-only` would have exited 0
// having changed nothing about the posture the operator just asked for.
//
// It runs in both directions again. Between #411 and #572 there was
// nothing to check in the second one: --acknowledge-repoint, the only flag
// patch had ever had to itself, came to mean something on create too, and
// the deny-list was left empty. Issue #572 gave patch a flag of its own
// again, --acknowledge-host-key-change, which answers a question about a
// host key already on record and therefore cannot mean anything for a set
// that does not exist yet. The middle subtest is the direction that stayed
// true through both, that create ACCEPTS --acknowledge-repoint, which is
// also the assertion that fails if it is ever quietly parsed and dropped.
//
// The four SSH-facing flags left create's deny-list in #572, so patch now
// takes them; backupsetkeytrust_test.go is where that half is checked,
// because "patch accepts it" is only worth anything if the edit it makes
// actually lands.
//
// The refusals are checked against a control that the same invocation
// without the wrong flag really does succeed. Without the control this
// would also pass against a command that refused everything.
func TestRun_BackupSetVerbsRefuseEachOthersFlags(t *testing.T) {
	t.Run("patch refuses create's flags", func(t *testing.T) {
		configPath := writeTestConfig(t)
		before, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		newLocal := filepath.Join(t.TempDir(), "moved")
		base := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary",
			"--local-path", newLocal}

		for _, wrong := range [][]string{
			{"--read-only"},
			{"--disabled"},
			{"--run"},
			{"--state-database", "/tmp/nope.db"},
		} {
			args := append(append([]string{}, base...), wrong...)
			out := captureStderr(t, func() {
				if got := run(args); got != 2 {
					t.Errorf("run(%v) = %d, want 2 (a usage error): %s is a create flag, and accepting it here changes nothing while exiting 0", args, got, wrong[0])
				}
			})
			if !strings.Contains(out, strings.TrimPrefix(wrong[0], "--")) {
				t.Errorf("the refusal for %s does not name the flag it refused: %q", wrong[0], out)
			}
		}

		after, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("a refused patch still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
		}

		// The control. Without it every assertion above would also pass
		// against a patch verb that refused this invocation outright.
		if got := run(base); got != 0 {
			t.Fatalf("run(%v) = %d, want 0: the same patch without the create flag has to work, or the cases above prove nothing", base, got)
		}
	})

	t.Run("create refuses patch's flags", func(t *testing.T) {
		configPath := writeTestConfig(t)
		keyPath := writeTestPrivateKey(t)

		// There is no host key on record for a set that does not exist,
		// so this flag answers nothing on create. Refusing it is what
		// stops `backup-set create ... --acknowledge-host-key-change`
		// exiting 0 having granted an acknowledgement to nothing.
		args := createArgs(configPath, keyPath, "api/nope", "--acknowledge-host-key-change")
		out := captureStderr(t, func() {
			if got := run(args); got != 2 {
				t.Errorf("run(%v) = %d, want 2 (a usage error)", args, got)
			}
		})
		if !strings.Contains(out, "acknowledge-host-key-change") {
			t.Errorf("the refusal does not name the flag it refused: %q", out)
		}

		// The control: the same create without it works, so the refusal
		// above is about the flag and not about the invocation.
		if got := run(createArgs(configPath, keyPath, "api/nope")); got != 0 {
			t.Fatal("the same create without the patch flag has to work, or the case above proves nothing")
		}
	})

	t.Run("create takes the acknowledgement", func(t *testing.T) {
		configPath := writeTestConfig(t)
		keyPath := writeTestPrivateKey(t)
		args := createArgs(configPath, keyPath, "api/acknowledged", "--acknowledge-repoint")
		if got := run(args); got != 0 {
			t.Errorf("run(%v) = %d, want 0: --acknowledge-repoint is a create flag since issue #411", args, got)
		}

		// And it really reached the request rather than being parsed and
		// dropped, which is what the shared FlagSet makes easy to do by
		// accident. The set really is there afterwards.
		if got := run([]string{"backup-set", "--config", configPath, "remove", "api/acknowledged"}); got != 0 {
			t.Errorf("removing the set the create above made = %d, want 0: it was never created", got)
		}
	})
}

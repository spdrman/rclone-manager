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
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
// It used to run in both directions. Since issue #411 there is nothing to
// check in the other one, because --acknowledge-repoint, the only flag
// patch ever had to itself, means something on create too: removing a set
// frees its id up, so a create over an id that already has artifacts on
// record is the same repoint an edit makes. So the second subtest checks
// the direction that is now true, that create ACCEPTS it, which is also
// the assertion that fails if it is ever quietly parsed and dropped.
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
			{"--trust-host-key"},
			{"--ssh-key-id", "some-key"},
			{"--known-hosts-line", "example.com ssh-ed25519 AAAA"},
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

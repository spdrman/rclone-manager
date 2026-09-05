package main

import (
	"strings"
	"testing"
)

// TestRetryIsDiscoverableFromTheCommandLine. A verb absent from the
// command table cannot be run and a verb absent from the usage block
// cannot be found; the second is how `backup-set remove` shipped
// invisibly (#391). This one is the only route out of a state an operator
// reaches when something has already gone wrong, so it being findable
// matters more than usual.
func TestRetryIsDiscoverableFromTheCommandLine(t *testing.T) {
	if _, ok := commands["retry"]; !ok {
		t.Fatal("there is no retry command, so nothing an operator types reaches FAILED's own declared exit")
	}
	out := captureStderr(t, usage)
	if !strings.Contains(out, "retry <source/backup-set/artifact>") {
		t.Error("usage() does not list the retry verb, so an operator cannot discover it")
	}
	// The usage line has to say that nothing does this on its own, because
	// an operator who assumes a stuck backup will sort itself out is an
	// operator who never comes back to it.
	if !strings.Contains(out, "Nothing does this automatically") {
		t.Error("usage() does not say the retry is operator-triggered, so somebody may wait for a cycle that is never coming")
	}
}

// TestARetryIsRefusedBeforeAnythingIsOpened. Every case runs with no
// resolvable config path, so a refusal that leaked past the argument
// checks would fail opening a state database rather than returning 2.
// Exit code 2 is this CLI's usage-error code and 1 is its failure code, so
// a 2 proves the request was rejected on its own terms.
func TestARetryIsRefusedBeforeAnythingIsOpened(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		says string
	}{
		{name: "no artifact at all", args: nil, says: "expected <source/backup-set/artifact>"},
		{name: "two artifacts", args: []string{"a/b/c", "d/e/f"}, says: "expected <source/backup-set/artifact>"},
		{name: "an id that is not three parts", args: []string{"production/postgres"}, says: "artifact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStderr(t, func() {
				code = cmdRetry(append(tc.args, "--config", "/nonexistent/no-such-config.yaml"))
			})
			if code == 0 {
				t.Fatalf("exit code = 0 for %s; stderr=%s", tc.name, out)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("stderr = %q, want it to contain %q", out, tc.says)
			}
		})
	}
}
